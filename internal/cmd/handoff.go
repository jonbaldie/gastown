package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/cli"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/ui"
	"github.com/jonbaldie/gastown/internal/workspace"
)

var handoffCmd = &cobra.Command{
	Use:         "handoff [bead-or-role]",
	GroupID:     GroupWork,
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Short:       "Hand off to a fresh session, work continues from hook",
	Long: `End watch. Hand off to a fresh agent session.

This is the canonical way to end any agent session. It handles all roles:

  - Mayor, Crew, Witness, Refinery, Deacon: Respawns with fresh Claude instance
  - Polecats: Calls 'gt done --status DEFERRED' (Witness handles lifecycle)

When run without arguments, hands off the current session.
When given a bead ID (gt-xxx, hq-xxx), hooks that work first, then restarts.
When given a role name, hands off that role's session (and switches to it).

Examples:
  gt handoff                          # Hand off current session
  gt handoff gt-abc                   # Hook bead, then restart
  gt handoff gt-abc -s "Fix it"       # Hook with context, then restart
  gt handoff -s "Context" -m "Notes"  # Hand off with custom message
  gt handoff -c                       # Collect state into handoff message
  gt handoff crew                     # Hand off crew session
  gt handoff mayor                    # Hand off mayor session

The --collect (-c) flag gathers current state (hooked work, inbox, ready beads,
in-progress items) and includes it in the handoff mail. This provides context
for the next session without manual summarization.

The --cycle flag triggers automatic session cycling (used by PreCompact hooks).
Unlike --auto (state only) or normal handoff (polecat→gt-done redirect), --cycle
always does a full respawn regardless of role. This enables crew workers and
polecats to get a fresh context window when the current one fills up.

Any molecule on the hook will be auto-continued by the new session.
The SessionStart hook runs 'gt prime' to restore context.`,
	RunE: runHandoff,
}

var (
	handoffWatch      bool
	handoffDryRun     bool
	handoffSubject    string
	handoffMessage    string
	handoffCollect    bool
	handoffStdin      bool
	handoffAuto       bool
	handoffCycle      bool
	handoffReason     string
	handoffNoGitCheck bool
	handoffYes        bool
)

func init() {
	handoffCmd.Flags().BoolVarP(&handoffWatch, "watch", "w", true, "Switch to new session (for remote handoff)")
	handoffCmd.Flags().BoolVarP(&handoffDryRun, "dry-run", "n", false, "Show what would be done without executing")
	handoffCmd.Flags().StringVarP(&handoffSubject, "subject", "s", "", "Subject for handoff mail (optional)")
	handoffCmd.Flags().StringVarP(&handoffMessage, "message", "m", "", "Message body for handoff mail (optional)")
	handoffCmd.Flags().BoolVarP(&handoffCollect, "collect", "c", false, "Auto-collect state (status, inbox, beads) into handoff message")
	handoffCmd.Flags().BoolVar(&handoffStdin, "stdin", false, "Read message body from stdin (avoids shell quoting issues)")
	handoffCmd.Flags().BoolVar(&handoffAuto, "auto", false, "Save state only, no session cycling (for PreCompact hooks)")
	handoffCmd.Flags().BoolVar(&handoffCycle, "cycle", false, "Auto-cycle session (for PreCompact hooks that want full session replacement)")
	handoffCmd.Flags().StringVar(&handoffReason, "reason", "", "Reason for handoff (e.g., 'compaction', 'idle')")
	handoffCmd.Flags().BoolVar(&handoffNoGitCheck, "no-git-check", false, "Skip git workspace cleanliness check")
	handoffCmd.Flags().BoolVarP(&handoffYes, "yes", "y", false, "Skip confirmation prompt (for automation and scripting)")
	rootCmd.AddCommand(handoffCmd)
}

func runHandoff(_ *cobra.Command, args []string) error {
	if err := readHandoffStdin(); err != nil {
		return err
	}

	// --auto mode: save state only, no session cycling.
	// Used by PreCompact hook to preserve state before compaction.
	// Note: auto-mode exits here, before the git-status warning check below.
	// This is intentional — auto-handoffs are triggered by hooks and should not
	// spam warnings. The --no-git-check flag has no effect in auto mode.
	if handoffAuto {
		return runHandoffAuto()
	}

	// --cycle mode: full session cycling, triggered by PreCompact hook.
	// Unlike --auto (state only), this replaces the current session with a fresh one.
	// Unlike normal handoff, this skips the polecat→gt-done redirect because
	// cycling preserves work state (the hook stays attached).
	//
	// Flow: collect state → send handoff mail → respawn pane (fresh Claude instance)
	// The successor session picks up hooked work via SessionStart hook (gt prime --hook).
	if handoffCycle {
		return runHandoffCycle()
	}

	if handled, err := redirectPolecatHandoff(); handled {
		return err
	}

	// Prompt for confirmation unless --yes/-y was passed or stdin is not a TTY.
	// Only interactive (human) sessions get prompted; agent automation proceeds
	// without blocking on stdin (gas-6z0).
	if !confirmHandoff() {
		return nil
	}

	// Enforce minimum handoff cooldown to prevent tight restart loops (gt-058d).
	// When a patrol agent (e.g., witness) completes quickly on idle rigs,
	// it can hand off immediately and the daemon respawns, creating a crash loop.
	enforceHandoffCooldown()

	collectHandoffContext()
	return runHandoffSession(args)
}

func readHandoffStdin() error {
	if !handoffStdin {
		return nil
	}
	if handoffMessage != "" {
		return fmt.Errorf("cannot use --stdin with --message/-m")
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	handoffMessage = strings.TrimRight(string(data), "\n")
	return nil
}

func redirectPolecatHandoff() (bool, error) {
	role := os.Getenv("GT_ROLE")
	polecatName := ""
	if role != "" {
		parsedRole, _, name := parseRoleString(role)
		if parsedRole != RolePolecat {
			return false, nil
		}
		polecatName = name
		if polecatName == "" {
			polecatName = os.Getenv("GT_POLECAT")
		}
	} else {
		polecatName = os.Getenv("GT_POLECAT")
		if polecatName == "" {
			return false, nil
		}
	}
	fmt.Printf("%s Polecat detected (%s) - using gt done for handoff\n",
		style.Bold.Render("🐾"), polecatName)
	doneCmd := exec.Command("gt", "done", "--status", "DEFERRED")
	doneCmd.Stdout = os.Stdout
	doneCmd.Stderr = os.Stderr
	return true, doneCmd.Run()
}

func confirmHandoff() bool {
	if handoffYes || handoffDryRun || !term.IsTerminal(int(os.Stdin.Fd())) {
		return true
	}
	if promptYesNo("Ready to hand off? This will restart the session.") {
		return true
	}
	fmt.Println("Handoff canceled.")
	return false
}

func collectHandoffContext() {
	if !handoffCollect {
		return
	}
	collected := collectHandoffState()
	if handoffMessage == "" {
		handoffMessage = collected
	} else {
		handoffMessage += "\n\n---\n" + collected
	}
	if handoffSubject == "" {
		handoffSubject = "Session handoff with context"
	}
}

func runHandoffSession(args []string) error {
	t, townTmux, pane, currentSession, err := prepareHandoffTmux()
	if err != nil {
		return err
	}

	// Warn if workspace has uncommitted or unpushed work (wa-7967c).
	// Note: this checks the caller's cwd, not the target session's workdir.
	// For remote handoff (gt handoff <role>), the warning reflects the caller's
	// workspace state. Checking the target session's workdir would require tmux
	// pane introspection and is deferred to a future enhancement.
	if !handoffNoGitCheck {
		warnHandoffGitStatus()
	}

	targetSession, restartCmd, err := resolveHandoffTarget(args, currentSession)
	if err != nil {
		return err
	}
	if targetSession != currentSession {
		// Update tmux session env before respawn (not during dry-run — see below)
		updateSessionEnvForHandoff(townTmux, targetSession, "")
		return handoffRemoteSession(townTmux, targetSession, restartCmd)
	}

	return runSelfHandoff(t, pane, currentSession, restartCmd)
}

func prepareHandoffTmux() (*tmux.Tmux, *tmux.Tmux, string, string, error) {
	// Use a socket-aware Tmux for pane operations. The calling process may be
	// on a different tmux server than the town socket (e.g., default socket).
	// For self-handoff, pane operations (clear-history, respawn-pane) must target
	// the caller's own server. SocketFromEnv() reads $TMUX to find the right one.
	t := tmux.NewTmuxWithSocket(tmux.SocketFromEnv())
	// Town-socket Tmux for session-level queries (getSessionPane, etc.)
	townTmux := tmux.NewTmux()

	if !tmux.IsInsideTmux() {
		return nil, nil, "", "", fmt.Errorf("not running in tmux - cannot hand off")
	}

	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return nil, nil, "", "", fmt.Errorf("TMUX_PANE not set - cannot hand off")
	}

	currentSession, err := getCurrentTmuxSession()
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("getting session name: %w", err)
	}
	return t, townTmux, pane, currentSession, nil
}

func resolveHandoffTarget(args []string, currentSession string) (string, string, error) {
	targetSession := currentSession
	if len(args) > 0 {
		arg := args[0]
		if looksLikeBeadID(arg) {
			if err := hookBeadForHandoff(arg); err != nil {
				return "", "", fmt.Errorf("hooking bead: %w", err)
			}
			if handoffSubject == "" {
				handoffSubject = fmt.Sprintf("🪝 HOOKED: %s", arg)
			}
		} else {
			var err error
			targetSession, err = resolveRoleToSession(arg)
			if err != nil {
				return "", "", fmt.Errorf("resolving role: %w", err)
			}
		}
	}

	restartCmd, err := buildRestartCommand(targetSession)
	if err != nil {
		return "", "", err
	}
	return targetSession, restartCmd, nil
}

func runSelfHandoff(t *tmux.Tmux, pane, currentSession, restartCmd string) error {
	// Close any in-progress molecule steps before cycling (gt-e26g).
	// Without this, patrol agents that handoff mid-cycle leak orphaned wisps.
	cleanupMoleculeOnHandoff()

	fmt.Printf("%s Handing off %s...\n", style.Bold.Render("🤝"), currentSession)
	agent := sessionToGTRole(currentSession)
	if agent == "" {
		agent = currentSession
	}

	// Dry run mode - show what would happen (BEFORE any side effects)
	if handoffDryRun {
		if handoffSubject != "" || handoffMessage != "" {
			fmt.Printf("Would send handoff mail: subject=%q (auto-hooked)\n", handoffSubject)
		}
		fmt.Printf("Would execute: tmux clear-history -t %s\n", pane)
		fmt.Printf("Would execute: tmux respawn-pane -k -t %s %s\n", pane, restartCmd)
		return nil
	}

	// Update tmux session environment for liveness detection.
	// IsAgentAlive reads GT_PROCESS_NAMES via tmux show-environment (session env),
	// not from shell exports. The restart command sets shell exports for the child
	// process, but we must also update the session env so liveness checks work.
	// Placed after the dry-run guard to avoid mutating session state during dry-run.
	updateSessionEnvForHandoff(t, currentSession, "")

	if err := persistSelfHandoff(agent); err != nil {
		return err
	}
	logSelfHandoff(agent)
	clearSelfHandoffHistory(t, pane)
	writeSelfHandoffMarker(currentSession)
	recordHandoffTime()
	return respawnSelfHandoff(t, pane, currentSession, restartCmd)
}

func persistSelfHandoff(agent string) error {
	// Send handoff mail to self (defaults applied inside sendHandoffMail).
	// The mail is auto-hooked so the next session picks it up.
	// CRITICAL: Mail must persist to Dolt BEFORE logging to town.log.
	// If Dolt is down, we must NOT log a false handoff to town.log.
	beadID, err := sendHandoffMail(handoffSubject, handoffMessage)
	if err != nil {
		// Handoff persistence failure is fatal — do not silently continue.
		// A silent failure causes the next session to find an empty hook,
		// losing all handoff context.
		if townRoot, trErr := workspace.FindFromCwd(); trErr == nil && townRoot != "" {
			_ = LogHandoffNoPersist(townRoot, agent, handoffSubject, err)
		}
		fmt.Fprintf(os.Stderr, "The session was NOT respawned. Fix the issue and retry 'gt handoff'.\n")
		return fmt.Errorf("handoff mail failed to persist (Dolt may be down): %w", err)
	}
	fmt.Printf("%s Sent handoff mail %s (auto-hooked)\n", style.Bold.Render("📬"), beadID)
	return nil
}

func logSelfHandoff(agent string) {
	// Log handoff event AFTER Dolt persistence succeeds.
	// Previously this logged BEFORE sendHandoffMail, causing false entries
	// in town.log when Dolt was down.
	if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
		_ = LogHandoff(townRoot, agent, handoffSubject)
		_ = events.LogFeed(events.TypeHandoff, agent, events.HandoffPayload(handoffSubject, true))
	}
}

func clearSelfHandoffHistory(t *tmux.Tmux, pane string) {
	// Clear scrollback history before respawn (resets copy-mode from [0/N] to [0/0])
	if err := t.ClearHistory(pane); err != nil {
		// Non-fatal - continue with respawn even if clear fails
		style.PrintWarning("could not clear history: %v", err)
	}
}

func writeSelfHandoffMarker(currentSession string) {
	// Write handoff marker for successor detection (prevents handoff loop bug).
	// The marker is cleared by gt prime after it outputs the warning.
	// This tells the new session "you're post-handoff, don't re-run /handoff"
	if cwd, err := os.Getwd(); err == nil {
		runtimeDir := filepath.Join(cwd, constants.DirRuntime)
		_ = os.MkdirAll(runtimeDir, 0755)
		markerPath := filepath.Join(runtimeDir, constants.FileHandoffMarker)
		_ = os.WriteFile(markerPath, []byte(currentSession), 0644)
	}
}

func respawnSelfHandoff(t *tmux.Tmux, pane, currentSession, restartCmd string) error {
	// Set remain-on-exit so the pane survives process death during handoff.
	// Without this, killing processes causes tmux to destroy the pane before
	// we can respawn it. This is essential for tmux session reuse.
	if err := t.SetRemainOnExit(pane, true); err != nil {
		style.PrintWarning("could not set remain-on-exit: %v", err)
	}

	// NOTE: For self-handoff, we do NOT call KillPaneProcesses here.
	// That would kill the gt handoff process itself before it can call RespawnPane,
	// leaving the pane dead with no respawn. RespawnPane's -k flag handles killing
	// the old process and spawns the new one together.
	// See: https://github.com/steveyegge/gastown/issues/859 (pane is dead bug)
	//
	// For orphan prevention, we rely on respawn-pane -k which sends SIGHUP/SIGTERM.
	// If orphans still occur, the solution is to adjust the restart command to
	// kill orphans at startup, not to kill ourselves before respawning.

	// Check if pane's working directory exists (may have been deleted)
	paneWorkDir, _ := t.GetPaneWorkDir(currentSession)
	if paneWorkDir != "" {
		if _, err := os.Stat(paneWorkDir); err != nil {
			if townRoot := detectTownRootFromCwd(); townRoot != "" {
				style.PrintWarning("pane working directory deleted, using town root")
				return t.RespawnPaneWithWorkDir(pane, townRoot, restartCmd)
			}
		}
	}

	// Use respawn-pane -k to atomically kill current process and start new one
	// Note: respawn-pane automatically resets remain-on-exit to off
	return t.RespawnPane(pane, restartCmd)
}

// runHandoffAuto saves state without cycling the session.
// Used by the PreCompact hook to preserve context before compaction.
// No tmux required — just collects state, sends handoff mail, and writes marker.
func runHandoffAuto() error {
	subject := autoHandoffSubject()
	message := autoHandoffMessage()

	if handoffDryRun {
		fmt.Printf("[auto-handoff] Would send mail: subject=%q\n", subject)
		fmt.Printf("[auto-handoff] Would write handoff marker\n")
		return nil
	}

	// Close any in-progress molecule steps before state save (gt-e26g).
	cleanupMoleculeOnHandoff()

	beadID, err := sendHandoffMail(subject, message)
	reportAutoHandoffMail(beadID, err)

	writeAutoHandoffMarker()

	logAutoHandoff(subject)

	return nil
}

func autoHandoffSubject() string {
	if handoffSubject != "" {
		return handoffSubject
	}
	reason := handoffReason
	if reason == "" {
		reason = "auto"
	}
	return fmt.Sprintf("🤝 HANDOFF: %s", reason)
}

func autoHandoffMessage() string {
	if handoffMessage != "" {
		return handoffMessage
	}
	return collectHandoffState()
}

func reportAutoHandoffMail(beadID string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "auto-handoff: could not send mail: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "auto-handoff: saved state to %s\n", beadID)
}

func writeAutoHandoffMarker() {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	runtimeDir := filepath.Join(cwd, constants.DirRuntime)
	_ = os.MkdirAll(runtimeDir, 0755)
	markerPath := filepath.Join(runtimeDir, constants.FileHandoffMarker)
	_ = os.WriteFile(markerPath, []byte(autoHandoffSessionName()), 0644)
}

func autoHandoffSessionName() string {
	if tmux.IsInsideTmux() {
		if name, err := getCurrentTmuxSession(); err == nil {
			return name
		}
	}
	return "auto-handoff"
}

func logAutoHandoff(subject string) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return
	}
	agent := detectSender()
	if agent == "" || agent == "overseer" {
		agent = "unknown"
	}
	_ = events.LogFeed(events.TypeHandoff, agent, events.HandoffPayload(subject, false))
}

// runHandoffCycle performs a full session cycle — save state AND respawn.
// This is the PreCompact-triggered session succession mechanism (gt-op78).
//
// Unlike --auto (state only) or normal handoff (polecat→gt-done redirect),
// --cycle always does a full respawn regardless of role. This enables
// crew workers (and polecats) to get a fresh context window when the
// current one fills up.
//
// The flow:
//  1. Auto-collect state (inbox, ready beads, hooked work)
//  2. Send handoff mail to self (auto-hooked for successor)
//  3. Write handoff marker (prevents handoff loop)
//  4. Respawn the tmux pane with a fresh Claude instance
//
// The successor session starts via SessionStart hook (gt prime --hook),
// finds the hooked work, and continues from where we left off.
func runHandoffCycle() error {
	subject, message := cycleHandoffInputs()
	t, pane, currentSession, fallback := prepareCycleSession(subject, message)
	if fallback {
		return runHandoffAuto()
	}

	if handoffDryRun {
		printCycleHandoffDryRun(subject, pane)
		return nil
	}

	// Close any in-progress molecule steps before cycling (gt-e26g).
	cleanupMoleculeOnHandoff()

	if err := persistCycleHandoff(subject, message, currentSession); err != nil {
		return err
	}

	writeCycleHandoffMarker(currentSession)

	// Record handoff time for cooldown enforcement (gt-058d).
	recordHandoffTime()

	logCycleHandoff(currentSession, subject)

	// Build restart command with --continue so the new session resumes
	// the previous conversation (preserves context across compaction cycles).
	restartCmd, err := buildRestartCommandWithOpts(currentSession, buildRestartCommandOpts{
		ContinueSession: true,
		ContinuePrompt:  "Context compacted. Continue your previous task.",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "handoff --cycle: could not build restart command: %v\n", err)
		return err
	}

	fmt.Fprintf(os.Stderr, "handoff --cycle: cycling session %s\n", currentSession)

	return respawnCyclePane(t, pane, currentSession, restartCmd)
}

func cycleHandoffInputs() (string, string) {
	subject := handoffSubject
	if subject == "" {
		reason := handoffReason
		if reason == "" {
			reason = "context-cycle"
		}
		subject = fmt.Sprintf("🤝 HANDOFF: %s", reason)
	}
	message := handoffMessage
	if message == "" {
		message = collectHandoffState()
	}
	return subject, message
}

func prepareCycleSession(subject, message string) (*tmux.Tmux, string, string, bool) {
	if !tmux.IsInsideTmux() {
		return cycleFallback(subject, message, "not in tmux, falling back to state-save only")
	}
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return cycleFallback(subject, message, "TMUX_PANE not set, falling back to state-save only")
	}
	currentSession, err := getCurrentTmuxSession()
	if err != nil {
		return cycleFallback(subject, message, fmt.Sprintf("could not get session: %v, falling back to state-save only", err))
	}
	return tmux.NewTmuxWithSocket(tmux.SocketFromEnv()), pane, currentSession, false
}

func cycleFallback(subject, message, reason string) (*tmux.Tmux, string, string, bool) {
	fmt.Fprintf(os.Stderr, "handoff --cycle: %s\n", reason)
	handoffMessage = message
	handoffSubject = subject
	return nil, "", "", true
}

func printCycleHandoffDryRun(subject, pane string) {
	fmt.Printf("[cycle] Would send handoff mail: subject=%q\n", subject)
	fmt.Printf("[cycle] Would write handoff marker\n")
	fmt.Printf("[cycle] Would execute: tmux clear-history -t %s\n", pane)
	fmt.Printf("[cycle] Would execute: tmux respawn-pane -k -t %s <restart-cmd>\n", pane)
}

func persistCycleHandoff(subject, message, currentSession string) error {
	beadID, err := sendHandoffMail(subject, message)
	if err == nil {
		fmt.Fprintf(os.Stderr, "handoff --cycle: saved state to %s\n", beadID)
		return nil
	}
	agent := sessionToGTRole(currentSession)
	if agent == "" {
		agent = currentSession
	}
	if townRoot, trErr := workspace.FindFromCwd(); trErr == nil && townRoot != "" {
		_ = LogHandoffNoPersist(townRoot, agent, subject, err)
	}
	fmt.Fprintf(os.Stderr, "The session was NOT respawned. Fix the issue and retry.\n")
	return fmt.Errorf("handoff --cycle: mail failed to persist: %w", err)
}

func writeCycleHandoffMarker(currentSession string) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	runtimeDir := filepath.Join(cwd, constants.DirRuntime)
	_ = os.MkdirAll(runtimeDir, 0755)
	markerContent := currentSession
	if handoffReason != "" {
		markerContent += "\n" + handoffReason
	}
	_ = os.WriteFile(filepath.Join(runtimeDir, constants.FileHandoffMarker), []byte(markerContent), 0644)
}

func logCycleHandoff(currentSession, subject string) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return
	}
	agent := sessionToGTRole(currentSession)
	if agent == "" {
		agent = currentSession
	}
	_ = LogHandoff(townRoot, agent, subject)
	_ = events.LogFeed(events.TypeHandoff, agent, events.HandoffPayload(subject, true))
}

func respawnCyclePane(t *tmux.Tmux, pane, currentSession, restartCmd string) error {
	if err := t.SetRemainOnExit(pane, true); err != nil {
		style.PrintWarning("could not set remain-on-exit: %v", err)
	}
	if err := t.ClearHistory(pane); err != nil {
		style.PrintWarning("could not clear history: %v", err)
	}
	paneWorkDir, _ := t.GetPaneWorkDir(currentSession)
	if paneWorkDir != "" && !handoffPathExists(paneWorkDir) {
		if townRoot := detectTownRootFromCwd(); townRoot != "" {
			return t.RespawnPaneWithWorkDir(pane, townRoot, restartCmd)
		}
	}
	return t.RespawnPane(pane, restartCmd)
}

// getCurrentTmuxSession returns the current tmux session name.
func getCurrentTmuxSession() (string, error) {
	// Prefer GT_ROLE for session resolution. BuildCommand uses -L <town-socket>,
	// but the calling process may live on the default socket (e.g., Claude Code
	// spawned by tmux on the default server). In that case, display-message on
	// the town socket returns an arbitrary session (often hq-boot) instead of
	// the caller's actual session.
	if role := os.Getenv("GT_ROLE"); role != "" {
		resolved, err := resolveRoleToSession(role)
		if err == nil && resolved != "" {
			return resolved, nil
		}
		// Fall through to tmux detection if role resolution fails
	}

	// Use TMUX_PANE for targeted display-message to avoid returning an
	// arbitrary session when multiple sessions share the town socket.
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return "", fmt.Errorf("TMUX_PANE not set")
	}
	out, err := tmux.BuildCommand("display-message", "-t", pane, "-p", "#{session_name}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveRoleToSession converts a role name or path to a tmux session name.
// Accepts:
//   - Role shortcuts: "crew", "witness", "refinery", "mayor", "deacon"
//   - Full paths: "<rig>/crew/<name>", "<rig>/witness", "<rig>/refinery"
//   - Direct session names (passed through)
//
// For role shortcuts that need context (crew, witness, refinery), it auto-detects from environment.
func resolveRoleToSession(role string) (string, error) {
	// First, check if it's a path format (contains /)
	if strings.Contains(role, "/") {
		return resolvePathToSession(role)
	}

	return resolveRoleShortcut(strings.ToLower(role))
}

func resolveRoleShortcut(role string) (string, error) {
	switch role {
	case constants.RoleMayor, "may":
		return getMayorSessionName(), nil

	case constants.RoleDeacon, "dea":
		return getDeaconSessionName(), nil

	case constants.RoleCrew:
		return resolveCrewRoleShortcut()

	case constants.RoleWitness, "wit":
		return resolveRigRoleShortcut(session.RoleWitness)

	case constants.RoleRefinery, "ref":
		return resolveRigRoleShortcut(session.RoleRefinery)

	default:
		// Assume it's a direct session name (e.g., gt-gastown-crew-max)
		return role, nil
	}
}

func resolveCrewRoleShortcut() (string, error) {
	// Try to get rig and crew name from environment or cwd.
	rig := os.Getenv("GT_RIG")
	crewName := os.Getenv("GT_CREW")
	if rig == "" || crewName == "" {
		// Try to detect from cwd.
		detected, err := detectCrewFromCwd()
		if err == nil {
			rig = detected.rigName
			crewName = detected.crewName
		}
	}
	if rig == "" || crewName == "" {
		return "", fmt.Errorf("cannot determine crew identity - run from crew directory or specify GT_RIG/GT_CREW")
	}
	return session.CrewSessionName(session.PrefixFor(rig), crewName), nil
}

func resolveRigRoleShortcut(role session.Role) (string, error) {
	rig := os.Getenv("GT_RIG")
	if rig == "" {
		return "", fmt.Errorf("cannot determine rig - set GT_RIG or run from rig context")
	}
	switch role {
	case session.RoleWitness:
		return session.WitnessSessionName(session.PrefixFor(rig)), nil
	case session.RoleRefinery:
		return session.RefinerySessionName(session.PrefixFor(rig)), nil
	default:
		return "", fmt.Errorf("unknown rig role: %s", role)
	}
}

// resolvePathToSession converts a path like "<rig>/crew/<name>" to a session name.
// Supported formats:
//   - <rig>/crew/<name> -> gt-<rig>-crew-<name>
//   - <rig>/witness -> gt-<rig>-witness
//   - <rig>/refinery -> gt-<rig>-refinery
//   - <rig>/polecats/<name> -> gt-<rig>-<name> (explicit polecat)
//   - <rig>/<name> -> gt-<rig>-<name> (polecat shorthand, if name isn't a known role)
func resolvePathToSession(path string) (string, error) {
	parts := strings.Split(path, "/")
	if err := validateAgentPath(path, parts); err != nil {
		return "", err
	}

	if len(parts) == 3 {
		if sessionName, ok := resolveThreePartPath(parts); ok {
			return sessionName, nil
		}
	}

	if len(parts) == 2 {
		if sessionName, err, handled := resolveTwoPartPath(parts); handled {
			return sessionName, err
		}
	}

	return "", fmt.Errorf("cannot parse path '%s' - expected <rig>/<polecat>, <rig>/crew/<name>, <rig>/witness, or <rig>/refinery", path)
}

func validateAgentPath(path string, parts []string) error {
	for _, part := range parts {
		if !safeAgentPathSegment(part) {
			return fmt.Errorf("invalid target path segment in %q", path)
		}
	}
	return nil
}

func resolveThreePartPath(parts []string) (string, bool) {
	rig := parts[0]
	switch parts[1] {
	case constants.RoleCrew:
		return session.CrewSessionName(session.PrefixFor(rig), parts[2]), true
	case "polecats":
		return session.PolecatSessionName(session.PrefixFor(rig), strings.ToLower(parts[2])), true
	default:
		return "", false
	}
}

func resolveTwoPartPath(parts []string) (string, error, bool) {
	rig := parts[0]
	second := parts[1]
	switch secondLower := strings.ToLower(second); secondLower {
	case constants.RoleWitness:
		return session.WitnessSessionName(session.PrefixFor(rig)), nil, true
	case constants.RoleRefinery:
		return session.RefinerySessionName(session.PrefixFor(rig)), nil, true
	case constants.RoleCrew:
		return "", fmt.Errorf("crew path requires name: %s/crew/<name>", rig), true
	case "polecats":
		return "", fmt.Errorf("polecats path requires name: %s/polecats/<name>", rig), true
	default:
		return resolveCrewOrPolecatPath(rig, second, secondLower)
	}
}

func resolveCrewOrPolecatPath(rig, second, secondLower string) (string, error, bool) {
	// Not a known role - check if it's a crew member before assuming polecat.
	// Crew members exist at <townRoot>/<rig>/crew/<name>.
	townRoot := detectTownRootFromCwd()
	if townRoot != "" {
		crewPath := filepath.Join(townRoot, rig, "crew", second)
		if info, err := os.Stat(crewPath); err == nil && info.IsDir() {
			return session.CrewSessionName(session.PrefixFor(rig), second), nil, true
		}
	}
	// Not a crew member - treat as polecat name (e.g., gastown/nux).
	return session.PolecatSessionName(session.PrefixFor(rig), secondLower), nil, true
}

// claudeEnvVars lists the Claude-related environment variables to propagate
// during handoff. These vars aren't inherited by tmux respawn-pane's fresh shell.
var claudeEnvVars = []string{
	// Claude API and config
	"ANTHROPIC_API_KEY",
	"CLAUDE_CODE_USE_BEDROCK",
	// AWS vars for Bedrock
	"AWS_PROFILE",
	"AWS_REGION",
	// OTEL telemetry — propagate so Claude keeps sending metrics after handoff
	// (tmux respawn-pane starts a fresh shell that doesn't inherit these)
	"CLAUDE_CODE_ENABLE_TELEMETRY",
	"OTEL_METRICS_EXPORTER",
	"OTEL_METRIC_EXPORT_INTERVAL",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
	"OTEL_LOGS_EXPORTER",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
	"OTEL_LOG_TOOL_DETAILS",
	"OTEL_LOG_TOOL_CONTENT",
	"OTEL_LOG_USER_PROMPTS",
	"OTEL_RESOURCE_ATTRIBUTES",
	// bd telemetry — so `bd` calls inside Claude emit to VictoriaMetrics/Logs
	"BD_OTEL_METRICS_URL",
	"BD_OTEL_LOGS_URL",
	// GT telemetry source vars — needed to recompute derived vars after handoff
	"GT_OTEL_METRICS_URL",
	"GT_OTEL_LOGS_URL",
}

// buildRestartCommand creates the command to run when respawning a session's pane.
// This needs to be the actual command to execute (e.g., claude), not a session attach command.
// The command includes a cd to the correct working directory for the role.
//
// buildRestartCommandOpts controls restart command generation.
type buildRestartCommandOpts struct {
	// ContinueSession adds --continue and omits the beacon prompt,
	// so the agent resumes its previous conversation silently.
	ContinueSession bool
	// ContinuePrompt overrides the default continuation prompt when
	// ContinueSession is true. If empty, falls back to a generic
	// continuation message.
	ContinuePrompt string
}

func buildRestartCommand(sessionName string) (string, error) {
	return buildRestartCommandWithOpts(sessionName, buildRestartCommandOpts{})
}

func buildRestartCommandWithOpts(sessionName string, opts buildRestartCommandOpts) (string, error) {
	ctx, err := buildRestartContext(sessionName, opts)
	if err != nil {
		return "", err
	}
	return renderRestartCommand(ctx.workDir, ctx.runtimeCmd, ctx.envMap), nil
}

type restartCommandContext struct {
	townRoot     string
	workDir      string
	rigPath      string
	gtRole       string
	simpleRole   string
	beacon       string
	currentAgent string
	runtimeCmd   string
	envMap       map[string]string
}

func buildRestartContext(sessionName string, opts buildRestartCommandOpts) (restartCommandContext, error) {
	townRoot := detectTownRootFromCwd()
	if townRoot == "" {
		return restartCommandContext{}, fmt.Errorf("cannot detect town root - run from within a Gas Town workspace")
	}
	workDir, err := sessionWorkDir(sessionName, townRoot)
	if err != nil {
		return restartCommandContext{}, err
	}
	identity, err := session.ParseSessionName(sessionName)
	if err != nil {
		return restartCommandContext{}, fmt.Errorf("cannot parse session name %q: %w", sessionName, err)
	}
	gtRole := identity.GTRole()
	simpleRole := config.ExtractSimpleRole(gtRole)
	rigPath := restartRigPath(townRoot, identity)
	beacon := restartBeacon(identity, simpleRole, opts)
	currentAgent := restartAgentName(sessionName)
	runtimeCmd, err := restartRuntimeCommand(townRoot, rigPath, simpleRole, beacon, currentAgent)
	if err != nil {
		return restartCommandContext{}, err
	}
	runtimeCmd = addContinueFlag(runtimeCmd, opts.ContinueSession)
	envMap := restartEnvironment(townRoot, rigPath, gtRole, simpleRole, currentAgent)
	return restartCommandContext{
		townRoot: townRoot, workDir: workDir, rigPath: rigPath,
		gtRole: gtRole, simpleRole: simpleRole, beacon: beacon,
		currentAgent: currentAgent, runtimeCmd: runtimeCmd, envMap: envMap,
	}, nil
}

func restartRigPath(townRoot string, identity *session.AgentIdentity) string {
	if identity.Rig == "" {
		return ""
	}
	return filepath.Join(townRoot, identity.Rig)
}

func restartBeacon(identity *session.AgentIdentity, simpleRole string, opts buildRestartCommandOpts) string {
	if opts.ContinueSession {
		if opts.ContinuePrompt != "" {
			return opts.ContinuePrompt
		}
		return "Your account was rotated to avoid a rate limit. Continue your previous task."
	}
	if isPatrolRole(simpleRole) {
		return session.BuildStartupPrompt(session.BeaconConfig{
			Recipient: identity.BeaconAddress(), Sender: "self", Topic: "patrol",
		}, "Run `"+cli.Name()+" prime --hook` and begin patrol.")
	}
	return session.FormatStartupBeacon(session.BeaconConfig{
		Recipient: identity.BeaconAddress(), Sender: "self", Topic: "handoff",
	})
}

func restartAgentName(sessionName string) string {
	currentAgent, agentInEnv := os.LookupEnv("GT_AGENT")
	if agentInEnv {
		return currentAgent
	}
	if val, err := tmux.NewTmux().GetEnvironment(sessionName, "GT_AGENT"); err == nil {
		return val
	}
	return ""
}

func restartRuntimeCommand(townRoot, rigPath, simpleRole, beacon, currentAgent string) (string, error) {
	if currentAgent != "" {
		runtimeCmd, err := config.GetRuntimeCommandWithPromptAndAgentOverride(rigPath, beacon, currentAgent)
		if err != nil {
			return "", fmt.Errorf("resolving agent config: %w", err)
		}
		return runtimeCmd, nil
	}
	if simpleRole != "" {
		return config.ResolveRoleAgentConfig(simpleRole, townRoot, rigPath).BuildCommandWithPrompt(beacon), nil
	}
	return config.GetRuntimeCommandWithPrompt(rigPath, beacon), nil
}

func addContinueFlag(runtimeCmd string, continueSession bool) string {
	if !continueSession {
		return runtimeCmd
	}
	if updated := strings.Replace(runtimeCmd, "claude.exe ", "claude.exe --continue ", 1); updated != runtimeCmd {
		return updated
	}
	return strings.Replace(runtimeCmd, "claude ", "claude --continue ", 1)
}

func restartEnvironment(townRoot, rigPath, gtRole, simpleRole, currentAgent string) map[string]string {
	envMap := make(map[string]string)
	var agentEnv map[string]string
	if gtRole != "" {
		runtimeConfig := restartRuntimeConfig(townRoot, rigPath, simpleRole, currentAgent)
		agentEnv = runtimeConfig.Env
		addRoleEnvironment(envMap, gtRole, runtimeConfig)
	}
	envMap["GT_ROOT"] = townRoot
	if currentAgent != "" {
		envMap["GT_AGENT"] = currentAgent
	}
	addProcessNameEnvironment(envMap, currentAgent)
	addClaudeEnvironment(envMap)
	mergeAgentEnvironment(envMap, agentEnv)
	if _, hasNodeOpts := agentEnv["NODE_OPTIONS"]; !hasNodeOpts {
		envMap["NODE_OPTIONS"] = ""
	}
	config.SanitizeAgentEnv(envMap, agentEnv)
	return envMap
}

func restartRuntimeConfig(townRoot, rigPath, simpleRole, currentAgent string) *config.RuntimeConfig {
	if currentAgent != "" {
		if runtimeConfig, _, err := config.ResolveAgentConfigWithOverride(townRoot, rigPath, currentAgent); err == nil {
			return runtimeConfig
		}
		return config.ResolveRoleAgentConfig(simpleRole, townRoot, rigPath)
	}
	if simpleRole != "" {
		return config.ResolveRoleAgentConfig(simpleRole, townRoot, rigPath)
	}
	return config.ResolveAgentConfig(townRoot, rigPath)
}

func addRoleEnvironment(envMap map[string]string, gtRole string, runtimeConfig *config.RuntimeConfig) {
	envMap["GT_ROLE"] = gtRole
	envMap["BD_ACTOR"] = gtRole
	envMap["GIT_AUTHOR_NAME"] = gtRole
	if runtimeConfig.Session != nil && runtimeConfig.Session.SessionIDEnv != "" {
		envMap["GT_SESSION_ID_ENV"] = runtimeConfig.Session.SessionIDEnv
	}
}

func addProcessNameEnvironment(envMap map[string]string, currentAgent string) {
	if processNames := os.Getenv("GT_PROCESS_NAMES"); processNames != "" {
		envMap["GT_PROCESS_NAMES"] = processNames
		return
	}
	if currentAgent != "" {
		resolved := config.ResolveProcessNames(currentAgent, "")
		envMap["GT_PROCESS_NAMES"] = strings.Join(resolved, ",")
	}
}

func addClaudeEnvironment(envMap map[string]string) {
	for _, name := range claudeEnvVars {
		if val := os.Getenv(name); val != "" {
			envMap[name] = val
		}
	}
}

func mergeAgentEnvironment(envMap, agentEnv map[string]string) {
	for key, value := range agentEnv {
		if _, exists := envMap[key]; !exists {
			envMap[key] = value
		}
	}
}

func renderRestartCommand(workDir, runtimeCmd string, envMap map[string]string) string {
	cdPrefix := fmt.Sprintf("cd %s && ", workDir)
	if runtime.GOOS == "windows" {
		cdPrefix = fmt.Sprintf("cd %s; ", workDir)
	}
	execPrefix := ""
	if runtime.GOOS != "windows" {
		execPrefix = "exec "
	}
	envCmd := config.PrependEnv(execPrefix+runtimeCmd, envMap)
	return cdPrefix + envCmd
}

// updateSessionEnvForHandoff updates the tmux session environment with the
// agent name and process names for liveness detection. IsAgentAlive reads
// GT_PROCESS_NAMES from the tmux session env (via tmux show-environment), not
// from shell exports in the pane. Without this, post-handoff liveness checks
// would use stale values from the previous agent.
func updateSessionEnvForHandoff(t *tmux.Tmux, sessionName, agentOverride string) {
	currentAgent := handoffAgentName(t, sessionName, agentOverride)

	if currentAgent == "" {
		return
	}

	// Update GT_AGENT in session env
	_ = t.SetEnvironment(sessionName, "GT_AGENT", currentAgent)

	// Resolve and update GT_PROCESS_NAMES in session env
	// When switching agents, recompute from config. When preserving, use env value.
	processNames := handoffProcessNames(sessionName, currentAgent, agentOverride)

	_ = t.SetEnvironment(sessionName, "GT_PROCESS_NAMES", processNames)
}

func handoffAgentName(t *tmux.Tmux, sessionName, agentOverride string) string {
	if agentOverride != "" {
		return agentOverride
	}
	if currentAgent := os.Getenv("GT_AGENT"); currentAgent != "" {
		return currentAgent
	}
	if currentAgent, err := t.GetEnvironment(sessionName, "GT_AGENT"); err == nil {
		return currentAgent
	}
	return ""
}

func handoffProcessNames(sessionName, currentAgent, agentOverride string) string {
	if agentOverride != "" {
		if processNames := overriddenHandoffProcessNames(sessionName, currentAgent); processNames != "" {
			return processNames
		}
	}
	if processNames := os.Getenv("GT_PROCESS_NAMES"); processNames != "" {
		return processNames
	}
	resolved := config.ResolveProcessNames(currentAgent, "")
	return strings.Join(resolved, ",")
}

func overriddenHandoffProcessNames(sessionName, currentAgent string) string {
	townRoot := detectTownRootFromCwd()
	if townRoot == "" {
		return ""
	}
	rigPath := handoffRigPath(townRoot, sessionName)
	rc, _, err := config.ResolveAgentConfigWithOverride(townRoot, rigPath, currentAgent)
	if err != nil {
		return ""
	}
	resolved := config.ResolveProcessNames(currentAgent, rc.Command, rc.Args...)
	return strings.Join(resolved, ",")
}

func handoffRigPath(townRoot, sessionName string) string {
	identity, err := session.ParseSessionName(sessionName)
	if err != nil || identity.Rig == "" {
		return ""
	}
	return filepath.Join(townRoot, identity.Rig)
}

// sessionWorkDir returns the correct working directory for a session.
// This is the canonical home for each role type.
func sessionWorkDir(sessionName, townRoot string) (string, error) {
	// Get session names for comparison
	mayorSession := getMayorSessionName()
	deaconSession := getDeaconSessionName()

	bootSession := session.BootSessionName()

	switch {
	case sessionName == mayorSession:
		// Mayor runs from ~/gt/mayor/, not town root.
		// Tools use workspace.FindFromCwd() which walks UP to find town root.
		return townRoot + "/mayor", nil

	case sessionName == bootSession:
		// Boot watchdog runs from ~/gt/deacon/dogs/boot/, not ~/gt/deacon/.
		// Boot is ephemeral (fresh each daemon tick) with its own CLAUDE.md.
		return townRoot + "/deacon/dogs/boot", nil

	case sessionName == deaconSession:
		return townRoot + "/deacon", nil

	case strings.Contains(sessionName, "-crew-"):
		return crewSessionWorkDir(sessionName, townRoot)

	default:
		return parsedSessionWorkDir(sessionName, townRoot)
	}
}

func crewSessionWorkDir(sessionName, townRoot string) (string, error) {
	// gt-<rig>-crew-<name> -> <townRoot>/<rig>/crew/<name>
	rig, name, _, ok := parseCrewSessionName(sessionName)
	if !ok {
		return "", fmt.Errorf("cannot parse crew session name: %s", sessionName)
	}
	return fmt.Sprintf("%s/%s/crew/%s", townRoot, rig, name), nil
}

func parsedSessionWorkDir(sessionName, townRoot string) (string, error) {
	identity, err := session.ParseSessionName(sessionName)
	if err != nil {
		return "", fmt.Errorf("unknown session type: %s (%w)", sessionName, err)
	}
	switch identity.Role {
	case session.RoleMayor:
		return townRoot + "/mayor", nil
	case session.RoleDeacon, session.RoleOverseer:
		return townRoot + "/deacon", nil
	case session.RoleWitness:
		return fmt.Sprintf("%s/%s/witness", townRoot, identity.Rig), nil
	case session.RoleRefinery:
		return fmt.Sprintf("%s/%s/refinery/rig", townRoot, identity.Rig), nil
	case session.RolePolecat:
		return fmt.Sprintf("%s/%s/polecats/%s", townRoot, identity.Rig, identity.Name), nil
	case session.RoleDog:
		return fmt.Sprintf("%s/deacon/dogs/%s", townRoot, identity.Name), nil
	default:
		return "", fmt.Errorf("unknown session type: %s (role %s, try specifying role explicitly)", sessionName, identity.Role)
	}
}

// sessionToGTRole converts a session name to a GT_ROLE value.
// Uses session.ParseSessionName for consistent parsing across the codebase.
func sessionToGTRole(sessionName string) string {
	identity, err := session.ParseSessionName(sessionName)
	if err != nil {
		return ""
	}
	return identity.GTRole()
}

// detectTownRootFromCwd walks up from the current directory to find the town root.
// Falls back to GT_TOWN_ROOT or GT_ROOT env vars if cwd detection fails (broken state recovery).
func detectTownRootFromCwd() string {
	// Use workspace.FindFromCwd which handles both primary (mayor/town.json)
	// and secondary (mayor/ directory) markers
	townRoot, err := workspace.FindFromCwd()
	if err == nil && townRoot != "" {
		return townRoot
	}

	// Fallback: try environment variables for town root
	// GT_TOWN_ROOT is set by shell integration, GT_ROOT is set by session manager
	// This enables handoff to work even when cwd detection fails due to
	// detached HEAD, wrong branch, deleted worktree, etc.
	if townRoot := townRootFromEnvironment(); townRoot != "" {
		return townRoot
	}

	// Final fallback: read GT_TOWN_ROOT from tmux global environment.
	// This handles the run-shell case where CWD is $HOME and process env
	// vars aren't set — the daemon sets GT_TOWN_ROOT in tmux global env.
	return townRootFromTmux()
}

func townRootFromEnvironment() string {
	for _, envName := range []string{"GT_TOWN_ROOT", "GT_ROOT"} {
		if envRoot := os.Getenv(envName); envRoot != "" {
			if isTownRoot(envRoot) {
				return envRoot
			}
		}
	}
	return ""
}

func townRootFromTmux() string {
	if socket := tmux.SocketFromEnv(); socket != "" {
		t := tmux.NewTmuxWithSocket(socket)
		if envRoot, err := t.GetGlobalEnvironment("GT_TOWN_ROOT"); err == nil && envRoot != "" {
			if isTownRoot(envRoot) {
				return envRoot
			}
		}
	}
	return ""
}

func isTownRoot(root string) bool {
	if _, err := os.Stat(filepath.Join(root, workspace.PrimaryMarker)); err == nil {
		return true
	}
	info, err := os.Stat(filepath.Join(root, workspace.SecondaryMarker))
	return err == nil && info.IsDir()
}

// handoffRemoteSession respawns a different session and optionally switches to it.
func handoffRemoteSession(t *tmux.Tmux, targetSession, restartCmd string) error {
	targetPane, err := remoteHandoffPane(t, targetSession)
	if err != nil {
		return err
	}

	fmt.Printf("%s Handing off %s...\n", style.Bold.Render("🤝"), targetSession)

	if handoffDryRun {
		printRemoteHandoffDryRun(targetPane, targetSession, restartCmd)
		return nil
	}

	prepareRemoteHandoffPane(t, targetPane)
	if err := respawnRemoteHandoffPane(t, targetPane, targetSession, restartCmd); err != nil {
		return fmt.Errorf("respawning pane: %w", err)
	}

	return switchToHandoffSession(targetSession)
}

func remoteHandoffPane(t *tmux.Tmux, targetSession string) (string, error) {
	exists, err := t.HasSession(targetSession)
	if err != nil {
		return "", fmt.Errorf("checking session: %w", err)
	}
	if !exists {
		return "", fmt.Errorf("session '%s' not found - is the agent running?", targetSession)
	}
	targetPane, err := getSessionPane(targetSession)
	if err != nil {
		return "", fmt.Errorf("getting target pane: %w", err)
	}
	return targetPane, nil
}

func printRemoteHandoffDryRun(targetPane, targetSession, restartCmd string) {
	fmt.Printf("Would execute: tmux clear-history -t %s\n", targetPane)
	fmt.Printf("Would execute: tmux respawn-pane -k -t %s %s\n", targetPane, restartCmd)
	if handoffWatch {
		fmt.Printf("Would execute: tmux switch-client -t %s\n", targetSession)
	}
}

func prepareRemoteHandoffPane(t *tmux.Tmux, targetPane string) {
	// Set remain-on-exit so the pane survives process death during handoff.
	if err := t.SetRemainOnExit(targetPane, true); err != nil {
		style.PrintWarning("could not set remain-on-exit: %v", err)
	}
	// Kill all processes in the pane before respawning to prevent orphan leaks.
	if err := t.KillPaneProcesses(targetPane); err != nil {
		style.PrintWarning("could not kill pane processes: %v", err)
	}
	// Clear scrollback history before respawn.
	if err := t.ClearHistory(targetPane); err != nil {
		style.PrintWarning("could not clear history: %v", err)
	}
}

func respawnRemoteHandoffPane(t *tmux.Tmux, targetPane, targetSession, restartCmd string) error {
	paneWorkDir, _ := t.GetPaneWorkDir(targetSession)
	if paneWorkDir != "" && !handoffPathExists(paneWorkDir) {
		if townRoot := detectTownRootFromCwd(); townRoot != "" {
			style.PrintWarning("pane working directory deleted, using town root")
			return t.RespawnPaneWithWorkDir(targetPane, townRoot, restartCmd)
		}
	}
	return t.RespawnPane(targetPane, restartCmd)
}

func handoffPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func switchToHandoffSession(targetSession string) error {
	if !handoffWatch {
		return nil
	}
	fmt.Printf("Switching to %s...\n", targetSession)
	if err := tmux.BuildCommand("switch-client", "-t", targetSession).Run(); err != nil {
		fmt.Printf("Note: Could not auto-switch (use: tmux switch-client -t %s)\n", targetSession)
	}
	return nil
}

// getSessionPane returns the pane identifier for a session's main pane.
func getSessionPane(sessionName string) (string, error) {
	// Get the pane ID for the first pane in the session
	out, err := tmux.BuildCommand("list-panes", "-t", sessionName, "-F", "#{pane_id}").Output()
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", fmt.Errorf("no panes found in session")
	}
	return lines[0], nil
}

// sendHandoffMail sends a handoff mail to self and auto-hooks it.
// Returns the created bead ID and any error.
func sendHandoffMail(subject, message string) (string, error) {
	subject = normalizeHandoffSubject(subject)
	message = normalizeHandoffMessage(message)

	// Detect agent identity for self-mail
	agentID, _, _, err := resolveSelfTarget()
	if err != nil {
		return "", fmt.Errorf("detecting agent identity: %w", err)
	}

	// Normalize identity to match mailbox query format
	agentID = mail.AddressToIdentity(agentID)

	// Detect town root for beads location
	townRoot := detectTownRootFromCwd()
	if townRoot == "" {
		return "", fmt.Errorf("cannot detect town root")
	}

	// Build labels for mail metadata (matches mail router format)
	labels := fmt.Sprintf("from:%s", agentID)

	// Close stale hooked mail beads from previous sessions before creating a new one.
	// Without this, each handoff cycle accumulates beads in status=hooked. (GH#3859)
	closeStaleHandoffMail(townRoot, agentID)
	beadID, err := createHandoffMail(townRoot, agentID, subject, message, labels)
	if err != nil {
		return "", err
	}
	return hookHandoffMail(townRoot, agentID, beadID)
}

func normalizeHandoffSubject(subject string) string {
	if subject == "" {
		return "🤝 HANDOFF: Session cycling"
	}
	if strings.Contains(subject, "HANDOFF") {
		return subject
	}
	return "🤝 HANDOFF: " + subject
}

func normalizeHandoffMessage(message string) string {
	if message == "" {
		return "Context cycling. Check bd ready for pending work."
	}
	return message
}

func closeStaleHandoffMail(townRoot, agentID string) {
	townB := beads.New(filepath.Join(townRoot, ".beads"))
	n, err := townB.CloseStaleHookedMailBeads(agentID)
	if err != nil {
		style.PrintWarning("couldn't close previous hooked mail bead(s): %v", err)
	} else if n > 0 {
		fmt.Printf("%s Closed %d stale hooked mail bead(s)\n", style.Dim.Render("🧹"), n)
	}
}

func createHandoffMail(townRoot, agentID, subject, message, labels string) (string, error) {
	// Create mail bead directly using bd create with --silent to get the ID.
	// Mail goes to town-level beads (hq- prefix). Flags go first, then -- to
	// end flag parsing, then the positional subject.
	args := []string{
		"create",
		"--assignee", agentID,
		"-d", message,
		"--priority", "1", // high — handoffs should float above normal mail
		"--labels", labels + ",gt:message",
		"--actor", agentID,
		// NOT ephemeral: handoff mail must be in issues table so gt hook can find it.
		"--silent", // Output only the bead ID
		"--", subject,
	}

	cmd := BdCmd(args...).WithAutoCommit().Dir(townRoot).Build()
	cmd.Env = append(cmd.Env, "BEADS_DIR="+filepath.Join(townRoot, ".beads"))

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", handoffMailCreateError(stderr.String(), err)
	}

	beadID := strings.TrimSpace(stdout.String())
	if beadID == "" {
		return "", fmt.Errorf("bd create did not return bead ID")
	}
	return beadID, nil
}

func handoffMailCreateError(stderr string, cause error) error {
	if errMsg := strings.TrimSpace(stderr); errMsg != "" {
		return fmt.Errorf("creating handoff mail: %s", errMsg)
	}
	return fmt.Errorf("creating handoff mail: %w", cause)
}

func hookHandoffMail(townRoot, agentID, beadID string) (string, error) {
	hookCmd := BdCmd("update", beadID, "--status=hooked", "--assignee="+agentID).
		WithAutoCommit().
		Dir(townRoot).
		Build()
	hookCmd.Env = append(hookCmd.Env, "BEADS_DIR="+filepath.Join(townRoot, ".beads"))
	hookCmd.Stderr = os.Stderr
	if err := hookCmd.Run(); err != nil {
		// Non-fatal: mail was created, just couldn't hook.
		style.PrintWarning("created mail %s but failed to auto-hook: %v", beadID, err)
	}
	return beadID, nil
}

// warnHandoffGitStatus checks the current workspace for uncommitted or unpushed
// work and prints a warning if found. Non-blocking — handoff continues regardless.
// Skips .beads/ changes since those are managed by Dolt and not a concern.
func warnHandoffGitStatus() {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	g := git.NewGit(cwd)
	if !g.IsRepo() {
		return
	}
	status, err := g.CheckUncommittedWork()
	if err != nil || status.CleanExcludingBeads() {
		return
	}
	fmt.Fprintf(os.Stderr, "%s workspace has uncommitted work: %s\n", ui.IconWarn, status.String())
	if len(status.ModifiedFiles) > 0 {
		fmt.Fprintf(os.Stderr, "%s   modified: %s\n", ui.IconWarn, strings.Join(status.ModifiedFiles, ", "))
	}
	if len(status.UntrackedFiles) > 0 {
		fmt.Fprintf(os.Stderr, "%s   untracked: %s\n", ui.IconWarn, strings.Join(status.UntrackedFiles, ", "))
	}
	if status.UnpushedCommits > 0 {
		fmt.Fprintf(os.Stderr, "%s   %d unpushed commit(s) — run 'git push' before handoff\n", ui.IconWarn, status.UnpushedCommits)
	}
	fmt.Fprintln(os.Stderr, "  (use --no-git-check to suppress this warning)")
}

// looksLikeBeadID checks if a string looks like a bead ID.
// Bead IDs have format: prefix-xxxx where prefix is 1-5 lowercase letters and xxxx is alphanumeric.
// Examples: "gt-abc123", "bd-ka761", "hq-cv-abc", "beads-xyz", "ap-qtsup.16"
func looksLikeBeadID(s string) bool {
	// Find the first hyphen
	idx := strings.Index(s, "-")
	if idx < 1 || idx > 5 {
		// No hyphen, or prefix is empty/too long
		return false
	}

	return validHandoffBeadPrefix(s[:idx]) && validHandoffBeadSuffix(s[idx+1:])
}

func validHandoffBeadPrefix(prefix string) bool {
	for _, c := range prefix {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

func validHandoffBeadSuffix(suffix string) bool {
	if suffix == "" {
		return false
	}
	for i, c := range suffix {
		if i == 0 {
			if !isHandoffLowerAlphaOrDigit(c) {
				return false
			}
			continue
		}
		if !isHandoffLowerAlphaOrDigit(c) && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

func isHandoffLowerAlphaOrDigit(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// hookBeadForHandoff attaches a bead to the current agent's hook.
func hookBeadForHandoff(beadID string) error {
	// Verify the bead exists first
	verifyCmd := beads.Spawn("show", beadID, "--json")
	if err := verifyCmd.Run(); err != nil {
		return fmt.Errorf("bead '%s' not found", beadID)
	}

	// Determine agent identity
	agentID, _, _, err := resolveSelfTarget()
	if err != nil {
		return fmt.Errorf("detecting agent identity: %w", err)
	}

	fmt.Printf("%s Hooking %s...\n", style.Bold.Render("🪝"), beadID)

	if handoffDryRun {
		fmt.Printf("Would run: bd update %s --status=pinned --assignee=%s\n", beadID, agentID)
		return nil
	}

	// Pin the bead using bd update (discovery-based approach)
	pinCmd := beads.Spawn("update", beadID, "--status=pinned", "--assignee="+agentID)
	pinCmd.Stderr = os.Stderr
	if err := pinCmd.Run(); err != nil {
		return fmt.Errorf("pinning bead: %w", err)
	}

	fmt.Printf("%s Work attached to hook (pinned bead)\n", style.Bold.Render("✓"))
	return nil
}

// collectHandoffState gathers current state for handoff context.
// Collects: git workspace state (deterministic), inbox summary, ready beads, hooked work.
// Git state is always collected first via Go library calls (no shelling out) to ensure
// the handoff always contains useful context even when external commands fail. (GH#1996)
func collectHandoffState() string {
	var parts []string

	// Deterministic git state — always collected via Go library, never empty. (GH#1996)
	if gitState := collectGitState(); gitState != "" {
		parts = append(parts, gitState)
	}

	parts = appendHandoffSection(parts, collectHookedWork())
	parts = appendHandoffSection(parts, collectHandoffInbox())
	parts = appendHandoffSection(parts, collectReadyWork())
	parts = appendHandoffSection(parts, collectInProgressWork())

	if len(parts) == 0 {
		return "No active state to report."
	}

	return strings.Join(parts, "\n\n")
}

func appendHandoffSection(parts []string, section string) []string {
	if section != "" {
		return append(parts, section)
	}
	return parts
}

func collectHookedWork() string {
	output, err := exec.Command("gt", "hook").Output()
	if err != nil {
		return ""
	}
	return formatHandoffSection(output, "## Hooked Work", "Nothing on hook", 0, "")
}

func collectHandoffInbox() string {
	output, err := exec.Command("gt", "mail", "inbox").Output()
	if err != nil {
		return ""
	}
	return formatHandoffSection(output, "## Inbox", "Inbox empty", 10, "... (more messages)")
}

func collectReadyWork() string {
	output, err := beads.Spawn("ready").Output()
	if err != nil {
		return ""
	}
	return formatHandoffSection(output, "## Ready Work", "No issues ready", 10, "... (more issues)")
}

func collectInProgressWork() string {
	output, err := beads.Spawn("list", "--status=in_progress").Output()
	if err != nil {
		return ""
	}
	return formatHandoffSection(output, "## In Progress", "No issues", 5, "... (more)")
}

func formatHandoffSection(output []byte, heading, emptyMarker string, limit int, overflow string) string {
	content := strings.TrimSpace(string(output))
	if content == "" || strings.Contains(content, emptyMarker) {
		return ""
	}
	lines := strings.Split(content, "\n")
	if limit > 0 && len(lines) > limit {
		lines = append(lines[:limit], overflow)
	}
	return heading + "\n" + strings.Join(lines, "\n")
}

// collectGitState captures deterministic workspace state using the Go git library.
// This uses only the git.Git wrapper (no shelling out to gt/bd), so it works
// reliably even when PATH is broken or external commands are unavailable.
// Returns empty string if git state cannot be read (e.g., not in a git repo). (GH#1996)
func collectGitState() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	g := git.NewGit(cwd)
	if !g.IsRepo() {
		return ""
	}

	lines := gitBranchState(g)
	lines = append(lines, gitWorkState(g)...)
	lines = append(lines, gitRecentState(g)...)

	if len(lines) == 0 {
		return ""
	}

	return "## Workspace State\n" + strings.Join(lines, "\n")
}

func gitBranchState(g *git.Git) []string {
	branch, err := g.CurrentBranch()
	if err != nil || branch == "" {
		return nil
	}
	return []string{"Branch: " + branch}
}

func gitWorkState(g *git.Git) []string {
	work, err := g.CheckUncommittedWork()
	if err != nil {
		return nil
	}
	var lines []string
	if work.HasUncommittedChanges {
		lines = append(lines, formatGitFiles("Modified", work.ModifiedFiles, 10))
		lines = append(lines, formatGitFiles("Untracked", work.UntrackedFiles, 5))
	}
	if work.StashCount > 0 {
		lines = append(lines, fmt.Sprintf("Stashes: %d", work.StashCount))
	}
	if work.UnpushedCommits > 0 {
		lines = append(lines, fmt.Sprintf("Unpushed commits: %d", work.UnpushedCommits))
	}
	return compactNonEmptyLines(lines)
}

func formatGitFiles(label string, files []string, limit int) string {
	if len(files) == 0 {
		return ""
	}
	if len(files) > limit {
		files = append(files[:limit], fmt.Sprintf("... (+%d more)", len(files)-limit))
	}
	return label + ": " + strings.Join(files, ", ")
}

func compactNonEmptyLines(lines []string) []string {
	compact := lines[:0]
	for _, line := range lines {
		if line != "" {
			compact = append(compact, line)
		}
	}
	return compact
}

func gitRecentState(g *git.Git) []string {
	logStr, err := g.RecentCommits(5)
	if err != nil || logStr == "" {
		return nil
	}
	return []string{"Recent commits:\n" + logStr}
}

// cleanupMoleculeOnHandoff closes any in-progress molecule steps before session
// handoff, preventing orphaned wisps from accumulating. (gt-e26g)
//
// Without this, patrol agents (witness, refinery, deacon) that handoff mid-cycle
// leave unfinished molecule steps open forever. The next session pours a new
// molecule, so the old steps are never completed.
//
// All errors are non-fatal — handoff must succeed even if cleanup fails.
func cleanupMoleculeOnHandoff() {
	b, handoffBead, molID, ok := handoffMoleculeContext()
	if !ok {
		return
	}

	// Close descendant steps (the leaked wisps)
	if n := closeDescendants(b, molID); n > 0 {
		fmt.Fprintf(os.Stderr, "handoff: closed %d molecule step(s) for %s\n", n, molID)
	}

	// Detach molecule with audit trail
	if _, err := b.DetachMoleculeWithAudit(handoffBead.ID, beads.DetachOptions{
		Operation: "squash",
		Reason:    "handoff: session cycling",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "handoff: warning: detach molecule audit failed: %v\n", err)
	}

	// Close all descendant wisps first, then the molecule root.
	// Without this, handoff leaks orphan wisps into the DB.
	// Best-effort in handoff path — log but proceed.
	if _, err := forceCloseDescendants(b, molID); err != nil {
		style.PrintWarning("handoff: could not close descendants of %s: %v", molID, err)
	}

	// Force-close the molecule root wisp
	if err := b.ForceCloseWithReason("handoff", molID); err != nil {
		fmt.Fprintf(os.Stderr, "handoff: warning: couldn't close molecule %s: %v\n", molID, err)
	}
}

func handoffMoleculeContext() (*beads.Beads, *beads.Issue, string, bool) {
	cwd, townRoot, ok := handoffWorkspace()
	if !ok {
		return nil, nil, "", false
	}
	agentID := handoffAgentIdentity(cwd, townRoot)
	if agentID == "" {
		return nil, nil, "", false
	}
	b, ok := handoffBeads()
	if !ok {
		return nil, nil, "", false
	}
	parts := strings.Split(agentID, "/")
	role := parts[len(parts)-1]
	handoffBead, err := b.FindHandoffBead(role)
	if err != nil || handoffBead == nil {
		return nil, nil, "", false
	}
	attachment := beads.ParseAttachmentFields(handoffBead)
	if attachment == nil || attachment.AttachedMolecule == "" {
		return nil, nil, "", false
	}
	return b, handoffBead, attachment.AttachedMolecule, true
}

func handoffWorkspace() (string, string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", false
	}
	townRoot, err := workspace.FindFromCwd()
	return cwd, townRoot, err == nil && townRoot != ""
}

func handoffAgentIdentity(cwd, townRoot string) string {
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return ""
	}
	return buildAgentIdentity(RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	})
}

func handoffBeads() (*beads.Beads, bool) {
	workDir, err := findLocalBeadsDir()
	if err != nil {
		return nil, false
	}
	return beads.New(workDir), true
}

// enforceHandoffCooldown sleeps if the last handoff was too recent.
// This prevents tight restart loops when patrol agents (e.g., witness)
// complete quickly on idle rigs and immediately hand off. (gt-058d)
//
// The cooldown is based on the modification time of the last_handoff_ts
// file in the .runtime directory. If the file exists and was written
// less than MinHandoffCooldown ago, the function sleeps for the remaining
// time. This ensures at least MinHandoffCooldown passes between handoffs.
//
// Crew and mayor roles are exempt — they hand off on human request,
// not on patrol loops, so the cooldown just gets in the way.
func enforceHandoffCooldown() {
	if role := os.Getenv("GT_ROLE"); role != "" {
		parsed, _, _ := parseRoleString(role)
		switch parsed {
		case RoleMayor, RoleCrew:
			return
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return
	}

	tsPath := filepath.Join(cwd, constants.DirRuntime, constants.FileLastHandoffTS)
	info, err := os.Stat(tsPath)
	if err != nil {
		return // No previous handoff recorded — first handoff, no cooldown
	}

	age := time.Since(info.ModTime())
	if age >= constants.MinHandoffCooldown {
		return // Enough time has passed
	}

	remaining := constants.MinHandoffCooldown - age
	fmt.Printf("%s Handoff cooldown: waiting %v (last handoff %v ago, min %v)\n",
		style.Dim.Render("⏳"), remaining.Round(time.Second),
		age.Round(time.Second), constants.MinHandoffCooldown)
	time.Sleep(remaining)
}

// recordHandoffTime writes the current timestamp to the handoff cooldown file.
// Called before respawning to establish the baseline for the next cooldown check.
func recordHandoffTime() {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}

	runtimeDir := filepath.Join(cwd, constants.DirRuntime)
	_ = os.MkdirAll(runtimeDir, 0755)
	tsPath := filepath.Join(runtimeDir, constants.FileLastHandoffTS)
	_ = os.WriteFile(tsPath, []byte(fmt.Sprintf("%d", time.Now().Unix())), 0644)
}

// isPatrolRole returns true if the role runs a patrol loop (refinery, witness, deacon).
// Patrol roles must re-enter their patrol molecule on handoff rather than
// "waiting for instructions," which leads to idle CPU burn.
func isPatrolRole(role string) bool {
	switch role {
	case "refinery", "witness", "deacon":
		return true
	}
	return false
}
