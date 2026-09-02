package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

func newHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "hook [bead-id] [target]",
		Aliases:     []string{"work"},
		GroupID:     GroupWork,
		Annotations: map[string]string{AnnotationPolecatSafe: "true"},
		Short:       "Show or attach work on a hook",
		Long: `Show what's on your hook, or attach new work.

With no arguments, shows your current hook status (alias for 'gt mol status').
With a bead ID, attaches that work to your hook.
With a bead ID and target, attaches work to another agent's hook.

The hook is the "durability primitive" - work on your hook survives session
restarts, context compaction, and handoffs. When you restart (via gt handoff),
your SessionStart hook finds the attached work and you continue from where
you left off.

Examples:
  gt hook                                    # Show what's on my hook
  gt hook status                             # Same as above
  gt hook gt-abc                             # Attach issue gt-abc to your hook
  gt hook gt-abc -s "Fix the bug"            # With subject for handoff mail
  gt hook gt-abc gastown/crew/max            # Attach gt-abc to max's hook

Related commands:
  gt sling <bead>    # Hook + start now (keep context)
  gt handoff <bead>  # Hook + restart (fresh context)
  gt unsling         # Remove work from hook`,
		Args: cobra.MaximumNArgs(2),
		RunE: runHookOrStatus,
	}
}

// hookStatusCmd shows hook status (alias for mol status)
var hookStatusCmd = &cobra.Command{
	Use:   "status [target]",
	Short: "Show what's on your hook",
	Long: `Show what's slung on your hook.

This is an alias for 'gt mol status'. Shows what work is currently
attached to your hook, along with progress information.

Examples:
  gt hook status                    # Show my hook
  gt hook status greenplace/nux     # Show nux's hook`,
	Args: cobra.MaximumNArgs(1),
	RunE: runMoleculeStatus,
}

// hookShowCmd shows hook status in compact one-line format
var hookShowCmd = &cobra.Command{
	Use:   "show [agent]",
	Short: "Show what's on an agent's hook (compact)",
	Long: `Show what's on any agent's hook in compact one-line format.

With no argument, shows your own hook status (auto-detected from context).

Use cases:
- Mayor checking what polecats are working on
- Witness checking polecat status
- Debugging coordination issues
- Quick status overview

Examples:
  gt hook show                         # What's on MY hook? (auto-detect)
  gt hook show gastown/polecats/nux    # What's nux working on?
  gt hook show gastown/witness         # What's the witness hooked to?
  gt hook show mayor                   # What's the mayor working on?

Output format (one line):
  gastown/polecats/nux: gt-abc123 'Fix the widget bug' [in_progress]`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHookShow,
}

// hookAttachCmd attaches a bead to a hook (alias for 'gt hook <bead-id>')
var hookAttachCmd = &cobra.Command{
	Use:   "attach <bead-id> [target]",
	Short: "Attach work to a hook",
	Long: `Attach a bead to your hook or another agent's hook.

With just a bead ID, attaches to your own hook (same as 'gt hook <bead-id>').
With a target, attaches to another agent's hook (for remote dispatch).

Examples:
  gt hook attach gt-abc                    # Attach to my hook
  gt hook attach gt-abc gastown/crew/max   # Attach to max's hook`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runHook(cmd, args)
	},
}

// hookDetachCmd detaches a bead from a hook (alias for 'gt hook clear')
var hookDetachCmd = &cobra.Command{
	Use:   "detach <bead-id> [target]",
	Short: "Detach work from a hook",
	Long: `Remove a specific bead from a hook (same as 'gt hook clear <bead-id>').

Examples:
  gt hook detach gt-abc               # Detach gt-abc from my hook
  gt hook detach gt-abc gastown/nux   # Detach gt-abc from nux's hook`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUnslingWith(cmd, args, commandBoolFlag(cmd, "dry-run"), commandBoolFlag(cmd, "force"))
	},
}

// hookClearCmd clears the hook (alias for 'gt unhook')
var hookClearCmd = &cobra.Command{
	Use:   "clear [bead-id] [target]",
	Short: "Clear your hook (alias for 'gt unhook')",
	Long: `Remove work from your hook (alias for 'gt unhook').

With no arguments, clears your own hook. With a bead ID, only clears
if that specific bead is currently hooked. With a target, operates on
another agent's hook.

Examples:
  gt hook clear                       # Clear my hook (whatever's there)
  gt hook clear gt-abc                # Only clear if gt-abc is hooked
  gt hook clear greenplace/joe        # Clear joe's hook

Related commands:
  gt unhook           # Same as 'gt hook clear'
  gt unsling          # Same as 'gt hook clear'`,
	Args: cobra.MaximumNArgs(2),
	RunE: runHookClear,
}

func init() {
	hookCmd := newHookCmd()
	state := moleculeState()
	// Flags for attaching work (gt hook <bead-id>)
	hookCmd.Flags().StringP("subject", "s", "", "Subject for handoff mail (optional)")
	hookCmd.Flags().StringP("message", "m", "", "Message for handoff mail (optional)")
	hookCmd.Flags().BoolP("dry-run", "n", false, "Show what would be done")
	hookCmd.Flags().BoolP("force", "f", false, "Replace existing incomplete hooked bead")
	hookCmd.Flags().Bool("clear", false, "Clear your hook (alias for 'gt unhook')")

	// --json flag for status output (used when no args, i.e., gt hook --json)
	hookCmd.Flags().BoolVar(&state.json, "json", false, "Output as JSON (for status)")
	hookStatusCmd.Flags().BoolVar(&state.json, "json", false, "Output as JSON")
	hookShowCmd.Flags().BoolVar(&state.json, "json", false, "Output as JSON")

	// Flags for attach subcommand
	hookAttachCmd.Flags().BoolP("force", "f", false, "Replace existing incomplete hooked bead")

	// Flags for detach subcommand (mirror unsling flags)
	hookDetachCmd.Flags().BoolP("force", "f", false, "Detach even if work is incomplete")

	// Flags for clear subcommand (mirror unsling flags)
	hookClearCmd.Flags().BoolP("dry-run", "n", false, "Show what would be done")
	hookClearCmd.Flags().BoolP("force", "f", false, "Clear even if work is incomplete")

	hookCmd.AddCommand(hookStatusCmd)
	hookCmd.AddCommand(hookShowCmd)
	hookCmd.AddCommand(hookAttachCmd)
	hookCmd.AddCommand(hookDetachCmd)
	hookCmd.AddCommand(hookClearCmd)

	rootCmd.AddCommand(hookCmd)
}

// runHookOrStatus dispatches to status, clear, or hook based on args/flags
func runHookOrStatus(cmd *cobra.Command, args []string) error {
	// --clear flag is alias for 'gt unhook'
	if commandBoolFlag(cmd, "clear") {
		return runUnslingWith(cmd, args, commandBoolFlag(cmd, "dry-run"), commandBoolFlag(cmd, "force"))
	}
	if len(args) == 0 {
		// No args - show status
		return runMoleculeStatus(cmd, args)
	}
	// Has arg - attach work
	return runHook(cmd, args)
}

// runHookClear handles 'gt hook clear' - delegates to runUnsling
func runHookClear(cmd *cobra.Command, args []string) error {
	return runUnslingWith(cmd, args, commandBoolFlag(cmd, "dry-run"), commandBoolFlag(cmd, "force"))
}

func closeCompletedHookedMolecule(workDir, beadID string) error {
	closeArgs := []string{"close", beadID, "--force", "--reason=Auto-replaced by gt hook (molecule complete)"}
	if sessionID := runtime.SessionIDFromEnv(); sessionID != "" {
		closeArgs = append(closeArgs, "--session="+sessionID)
	}
	return BdCmd(closeArgs...).Dir(workDir).WithAutoCommit().Run()
}

// checkPinnedBeadComplete checks if a pinned bead's attached molecule is 100% complete.
// Returns (isComplete, hasAttachment):
// - isComplete=true if no molecule attached OR all molecule steps are closed
// - hasAttachment=true if there's an attached molecule
func checkPinnedBeadComplete(b *beads.Beads, issue *beads.Issue) (isComplete bool, hasAttachment bool) {
	// Check for attached molecule
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil || attachment.AttachedMolecule == "" {
		// No molecule attached - consider complete (naked bead)
		return true, false
	}

	// Get progress of attached molecule
	progress, err := getMoleculeProgressInfo(b, attachment.AttachedMolecule)
	if err != nil {
		// Can't determine progress - be conservative, treat as incomplete
		return false, true
	}

	if progress == nil {
		// No steps found - might be a simple issue, treat as complete
		return true, true
	}

	return progress.Complete, true
}

// runHookShow displays another agent's hook in compact one-line format.
func runHookShow(_ *cobra.Command, args []string) error {
	if err := ensureCurrentHookWorktreeIntegrity(); err != nil {
		return err
	}
	target, err := resolveHookShowTarget(args)
	if err != nil {
		return err
	}
	workDir, err := resolveHookShowWorkDir(args, target)
	if err != nil {
		return err
	}
	hookedBeads, err := listHookShowBeads(workDir, target)
	if err != nil {
		return err
	}
	return printHookShow(target, hookedBeads)
}

func resolveHookShowTarget(args []string) (string, error) {
	if len(args) > 0 {
		return normalizeHookShowTarget(args[0]), nil
	}
	resolved, err := resolveSelfTarget()
	if err != nil {
		return "", fmt.Errorf("auto-detecting agent (use explicit argument): %w", err)
	}
	return resolved.agentID, nil
}

func resolveHookShowWorkDir(args []string, target string) (string, error) {
	workDir, err := findLocalBeadsDir()
	if err != nil {
		return "", fmt.Errorf("not in a beads workspace: %w", err)
	}
	if len(args) == 0 {
		return workDir, nil
	}
	townRoot, townErr := workspace.FindFromCwd()
	if townErr == nil && townRoot != "" {
		workDir = resolveHookLookupWorkDir(workDir, target, townRoot)
	}
	return workDir, nil
}

func listHookShowBeads(workDir, target string) ([]*beads.Issue, error) {
	hookedBeads, err := listAssignedActiveWork(beads.New(workDir), target)
	if err != nil {
		return nil, fmt.Errorf("listing active hook work: %w", err)
	}
	if len(hookedBeads) > 0 {
		return hookedBeads, nil
	}
	return listTownHookShowBeads(target), nil
}

func listTownHookShowBeads(target string) []*beads.Issue {
	townRoot, err := findTownRoot()
	if err != nil || townRoot == "" {
		return nil
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if _, err := os.Stat(townBeadsDir); err == nil {
		if townWork, err := listAssignedActiveWork(beads.New(townBeadsDir), target); err == nil && len(townWork) > 0 {
			return townWork
		}
	}
	if isTownLevelRole(target) {
		return scanAllRigsForHookedBeads(townRoot, target)
	}
	return nil
}

func printHookShow(target string, hookedBeads []*beads.Issue) error {
	if moleculeState().json {
		return printHookShowJSON(target, hookedBeads)
	}
	if len(hookedBeads) == 0 {
		fmt.Printf("%s: (empty)\n", target)
		return nil
	}
	bead := hookedBeads[0]
	fmt.Printf("%s: %s '%s' [%s]\n", target, bead.ID, bead.Title, bead.Status)
	return nil
}

func printHookShowJSON(target string, hookedBeads []*beads.Issue) error {
	type compactInfo struct {
		Agent  string `json:"agent"`
		BeadID string `json:"bead_id,omitempty"`
		Title  string `json:"title,omitempty"`
		Status string `json:"status"`
	}
	info := compactInfo{Agent: target, Status: "empty"}
	if len(hookedBeads) > 0 {
		info.BeadID = hookedBeads[0].ID
		info.Title = hookedBeads[0].Title
		info.Status = hookedBeads[0].Status
	}
	return json.NewEncoder(os.Stdout).Encode(info)
}

func ensureCurrentHookWorktreeIntegrity() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return nil
	}
	roleCtx := detectRole(cwd, townRoot)
	return ensureRoleWorktreeIntegrity(cwd, townRoot, roleCtx.Role)
}

// normalizeHookShowTarget resolves target aliases/shorthand to canonical agent IDs.
// Examples:
//   - "rig/polecat" -> "rig/polecats/polecat"
//   - "mayor" -> "mayor"
//
// If resolution fails, it returns the original target unchanged.
func normalizeHookShowTarget(target string) string {
	target = strings.TrimSpace(target)
	if skipNormalizeHookShowTarget(target) {
		return target
	}
	if sessionName, err := resolveRoleToSession(target); err == nil && sessionName != "" {
		if addr, ok := sessionNameToCanonicalAddress(sessionName, target); ok {
			return addr
		}
	}
	if identity, err := session.ParseAddress(target); err == nil {
		return identity.Address()
	}
	if expanded, ok := expandHookShowShorthand(target); ok {
		return expanded
	}
	return target
}

func skipNormalizeHookShowTarget(target string) bool {
	if target == "" || target == "." || target == ".." {
		return true
	}
	return strings.ContainsAny(target, `/\\`) && !safeAgentTargetPath(target)
}

func expandHookShowShorthand(target string) (string, bool) {
	parts := strings.Split(target, "/")
	if len(parts) != 2 || !safeAgentPathSegment(parts[0]) || !safeAgentPathSegment(parts[1]) {
		return "", false
	}
	name := parts[1]
	switch strings.ToLower(name) {
	case "witness", "refinery", "mayor", "deacon":
		return "", false
	}
	townRoot := detectTownRootFromCwd()
	if townRoot != "" {
		crewPath := filepath.Join(townRoot, parts[0], "crew", name)
		if info, statErr := os.Stat(crewPath); statErr == nil && info.IsDir() {
			return parts[0] + "/crew/" + name, true
		}
	}
	return parts[0] + "/polecats/" + strings.ToLower(name), true
}

// sessionNameToCanonicalAddress maps a tmux session name to a canonical agent
// assignee address (e.g., "gastown/polecats/toast").
//
// targetHint is the original user input and is used to seed a temporary
// prefix→rig mapping for deterministic parsing in tests or minimal
// environments where the global session registry is not initialized.
func sessionNameToCanonicalAddress(sessionName, targetHint string) (string, bool) {
	if identity, err := session.ParseSessionName(sessionName); err == nil {
		return canonicalAssigneeAddress(identity), true
	}

	registry := session.NewPrefixRegistry()
	for rig, prefix := range session.DefaultRegistry().AllRigs() {
		registry.Register(prefix, rig)
	}
	parts := strings.Split(strings.TrimSpace(targetHint), "/")
	if len(parts) >= 2 && parts[0] != "" {
		rig := parts[0]
		registry.Register(session.PrefixFor(rig), rig)
	}

	identity, err := session.ParseSessionNameWithRegistry(sessionName, registry)
	if err != nil {
		return "", false
	}
	return canonicalAssigneeAddress(identity), true
}

// findTownRoot finds the Gas Town root directory.
func findTownRoot() (string, error) {
	return workspace.FindFromCwd()
}
