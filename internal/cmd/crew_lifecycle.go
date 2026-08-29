package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/crew"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/townlog"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

func runCrewRemove(_ *cobra.Command, args []string) error {
	forceRemove := crewForce || crewPurge
	var lastErr error
	for _, arg := range args {
		if err := removeCrewMember(arg, forceRemove); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func removeCrewMember(arg string, forceRemove bool) error {
	name, rigOverride := crewRemoveTarget(arg)
	crewMgr, r, err := getCrewManagerForMember(rigOverride, name)
	if err != nil {
		fmt.Printf("Error removing %s: %v\n", arg, err)
		return err
	}
	if err := stopCrewRemovalSession(r.Name, name, arg, forceRemove); err != nil {
		return err
	}
	crewPath := filepath.Join(r.Path, "crew", name)
	if err := removeCrewWorkspace(crewMgr, r.Path, r.Name, name, arg, crewPath, forceRemove); err != nil {
		return err
	}
	handleCrewRemoveAgentBead(r.Path, r.Name, name)
	return nil
}

func crewRemoveTarget(arg string) (string, string) {
	rigOverride := crewRig
	if rig, crewName, ok := parseRigSlashName(arg); ok {
		if rigOverride == "" {
			rigOverride = rig
		}
		return crewName, rigOverride
	}
	return arg, rigOverride
}

func stopCrewRemovalSession(rigName, name, arg string, forceRemove bool) error {
	sessionID := crewSessionName(rigName, name)
	if !forceRemove {
		t := tmux.NewTmux()
		if hasSession, _ := t.HasSession(sessionID); hasSession {
			fmt.Printf("Error removing %s: session '%s' is running (use --force to kill and remove)\n", arg, sessionID)
			return fmt.Errorf("session running")
		}
	}
	t := tmux.NewTmux()
	if hasSession, _ := t.HasSession(sessionID); !hasSession {
		return nil
	} else if err := t.KillSessionWithProcesses(sessionID); err != nil {
		fmt.Printf("Error killing session for %s: %v\n", arg, err)
		return err
	}
	fmt.Printf("Killed session %s\n", sessionID)
	return nil
}

func removeCrewWorkspace(crewMgr *crew.Manager, rigPath, rigName, name, arg, crewPath string, forceRemove bool) error {
	if isCrewWorktree(crewPath) {
		return removeCrewWorktree(rigPath, rigName, name, arg, crewPath, forceRemove)
	}
	if err := crewMgr.Remove(name, forceRemove); err != nil {
		printCrewRemoveError(arg, err)
		return err
	}
	fmt.Printf("%s Removed crew workspace: %s/%s\n", style.Bold.Render("✓"), rigName, name)
	return nil
}

func isCrewWorktree(crewPath string) bool {
	info, err := os.Stat(filepath.Join(crewPath, ".git"))
	return err == nil && !info.IsDir()
}

func removeCrewWorktree(rigPath, rigName, name, arg, crewPath string, forceRemove bool) error {
	removeArgs := []string{"worktree", "remove", crewPath}
	if forceRemove {
		removeArgs = []string{"worktree", "remove", "--force", crewPath}
	}
	removeCmd := exec.Command("git", removeArgs...)
	removeCmd.Dir = constants.RigMayorPath(rigPath)
	output, err := removeCmd.CombinedOutput()
	if err != nil {
		fmt.Printf("Error removing worktree %s: %v\n%s", arg, err, string(output))
		return err
	}
	fmt.Printf("%s Removed crew worktree: %s/%s\n", style.Bold.Render("✓"), rigName, name)
	return nil
}

func printCrewRemoveError(arg string, err error) {
	if err == crew.ErrCrewNotFound {
		fmt.Printf("Error removing %s: crew workspace not found\n", arg)
	} else if err == crew.ErrHasChanges {
		fmt.Printf("Error removing %s: uncommitted changes (use --force)\n", arg)
	} else {
		fmt.Printf("Error removing %s: %v\n", arg, err)
	}
}

func handleCrewRemoveAgentBead(rigPath, rigName, name string) {
	townRoot, _ := workspace.Find(rigPath)
	if townRoot == "" {
		townRoot = rigPath
	}
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentBeadID := beads.CrewBeadIDWithPrefix(prefix, rigName, name)
	if crewPurge {
		purgeCrewAgentBead(rigPath, rigName, name, agentBeadID)
		return
	}
	closeCrewAgentBead(rigPath, agentBeadID)
}

func purgeCrewAgentBead(rigPath, rigName, name, agentBeadID string) {
	deleteCmd := beads.Spawn("delete", agentBeadID, "--force")
	deleteCmd.Dir = rigPath
	if output, err := deleteCmd.CombinedOutput(); err != nil {
		if !strings.Contains(string(output), "no issue found") && !strings.Contains(string(output), "not found") {
			style.PrintWarning("could not delete agent bead %s: %v", agentBeadID, err)
		}
	} else {
		fmt.Printf("Deleted agent bead: %s\n", agentBeadID)
	}
	agentAddr := fmt.Sprintf("%s/crew/%s", rigName, name)
	unassignCmd := beads.Spawn("list", "--assignee="+agentAddr, "--format=id")
	unassignCmd.Dir = rigPath
	if output, err := unassignCmd.CombinedOutput(); err == nil {
		unassignCrewAgentBeads(rigPath, strings.Fields(strings.TrimSpace(string(output))))
	}
}

func unassignCrewAgentBeads(rigPath string, ids []string) {
	for _, id := range ids {
		if id == "" {
			continue
		}
		updateCmd := beads.Spawn("update", id, "--unassign")
		updateCmd.Dir = rigPath
		if _, err := updateCmd.CombinedOutput(); err == nil {
			fmt.Printf("Unassigned: %s\n", id)
		}
	}
}

func closeCrewAgentBead(rigPath, agentBeadID string) {
	closeArgs := []string{"close", agentBeadID, "--reason=Crew workspace removed"}
	if sessionID := runtime.SessionIDFromEnv(); sessionID != "" {
		closeArgs = append(closeArgs, "--session="+sessionID)
	}
	closeCmd := beads.Spawn(closeArgs...)
	closeCmd.Dir = rigPath
	if output, err := closeCmd.CombinedOutput(); err != nil {
		if !strings.Contains(string(output), "no issue found") && !strings.Contains(string(output), "already closed") {
			style.PrintWarning("could not close agent bead %s: %v", agentBeadID, err)
		}
	} else {
		fmt.Printf("Closed agent bead: %s\n", agentBeadID)
	}
}

func runCrewRefresh(_ *cobra.Command, args []string) error {
	name := resolveCrewRefreshName(args[0])

	crewMgr, r, err := getCrewManagerForMember(crewRig, name)
	if err != nil {
		return err
	}

	worker, err := getCrewRefreshWorker(crewMgr, name)
	if err != nil {
		return err
	}

	handoffMsg := crewRefreshMessage(name)
	if err := sendCrewRefreshMail(worker.ClonePath, r.Name, name, handoffMsg); err != nil {
		return err
	}

	fmt.Printf("Sent handoff mail to %s/%s\n", r.Name, name)

	if err := startCrewRefresh(crewMgr, name); err != nil {
		return err
	}

	fmt.Printf("%s Refreshed crew workspace: %s/%s\n",
		style.Bold.Render("✓"), r.Name, name)
	fmt.Printf("Attach with: %s\n", style.Dim.Render(fmt.Sprintf("gt crew at %s", name)))

	return nil
}

func resolveCrewRefreshName(name string) string {
	// Parse rig/name format (e.g., "beads/emma" -> rig=beads, name=emma)
	if rig, crewName, ok := parseRigSlashName(name); ok {
		if crewRig == "" {
			crewRig = rig
		}
		return crewName
	}
	return name
}

func getCrewRefreshWorker(crewMgr *crew.Manager, name string) (*crew.CrewWorker, error) {
	worker, err := crewMgr.Get(name)
	if err != nil {
		if err == crew.ErrCrewNotFound {
			return nil, fmt.Errorf("crew workspace '%s' not found", name)
		}
		return nil, fmt.Errorf("getting crew worker: %w", err)
	}
	return worker, nil
}

func crewRefreshMessage(name string) string {
	if crewMessage != "" {
		return crewMessage
	}
	return fmt.Sprintf("Context refresh for %s. Check mail and beads for current work state.", name)
}

func sendCrewRefreshMail(clonePath, rigName, name, handoffMsg string) error {
	mailDir := filepath.Join(clonePath, "mail")
	if _, err := os.Stat(mailDir); os.IsNotExist(err) {
		if err := os.MkdirAll(mailDir, 0755); err != nil {
			return fmt.Errorf("creating mail dir: %w", err)
		}
	}
	mailbox := mail.NewMailbox(mailDir)
	address := fmt.Sprintf("%s/%s", rigName, name)
	msg := &mail.Message{
		From:    address,
		To:      address,
		Subject: "🤝 HANDOFF: Context Refresh",
		Body:    handoffMsg,
	}
	if err := mailbox.Append(msg); err != nil {
		return fmt.Errorf("sending handoff mail: %w", err)
	}
	return nil
}

func startCrewRefresh(crewMgr *crew.Manager, name string) error {
	if err := crewMgr.Start(name, crew.StartOptions{
		KillExisting:  true,      // Kill old session if running
		Topic:         "refresh", // Startup nudge topic
		Interactive:   true,      // No --dangerously-skip-permissions
		AgentOverride: crewAgentOverride,
	}); err != nil {
		return fmt.Errorf("starting crew session: %w", err)
	}
	return nil
}

// runCrewStart starts crew workers in a rig.
// If first arg is a valid rig name, it's used as the rig; otherwise rig is inferred from cwd.
// Remaining args (or all args if rig is inferred) are crew member names.
// Defaults to all crew members if no names specified.
func runCrewStart(cmd *cobra.Command, args []string) error {
	rigName, crewNames := resolveCrewStartTargets(args)

	// Get the rig manager and rig (infers from cwd if rigName is empty)
	crewMgr, r, err := getCrewManager(rigName)
	if err != nil {
		return err
	}
	// Update rigName in case it was inferred
	rigName = r.Name

	crewNames, err = loadCrewStartNames(crewMgr, rigName, crewNames)
	if err != nil {
		return err
	}
	if len(crewNames) == 0 {
		return nil
	}

	opts, err := crewStartOptions(cmd, crewMgr, r.Path, crewNames)
	if err != nil {
		return err
	}

	fmt.Printf("Starting %d crew member(s) in %s...\n", len(crewNames), rigName)
	results := startCrewMembersInParallel(crewMgr, crewNames, opts)
	lastErr, startedCount, skippedCount := collectCrewStartResults(results, rigName)
	printCrewStartSummary(startedCount, skippedCount, r.Name)
	return lastErr
}

func resolveCrewStartTargets(args []string) (string, []string) {
	if crewRig != "" {
		return crewRig, args
	}
	if len(args) == 0 {
		return "", nil
	}
	if _, _, err := getRig(args[0]); err == nil {
		return args[0], args[1:]
	}
	return "", args
}

func loadCrewStartNames(crewMgr *crew.Manager, rigName string, crewNames []string) ([]string, error) {
	if !crewAll && len(crewNames) > 0 {
		return crewNames, nil
	}
	workers, err := crewMgr.List()
	if err != nil {
		return nil, fmt.Errorf("listing crew: %w", err)
	}
	if len(workers) == 0 {
		fmt.Printf("No crew members in rig %s\n", rigName)
		return nil, nil
	}
	for _, worker := range workers {
		crewNames = append(crewNames, worker.Name)
	}
	return crewNames, nil
}

func crewStartOptions(cmd *cobra.Command, crewMgr *crew.Manager, rigPath string, crewNames []string) (crew.StartOptions, error) {
	townRoot, _ := workspace.Find(rigPath)
	if townRoot == "" {
		townRoot = filepath.Dir(rigPath)
	}
	accountsPath := constants.MayorAccountsPath(townRoot)
	claudeConfigDir, _, _ := config.ResolveAccountConfigDir(accountsPath, crewAccount)
	if err := validateCrewStartResume(crewMgr, crewNames); err != nil {
		return crew.StartOptions{}, err
	}
	if crewResume == "last" && len(crewNames) > 1 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: --resume will auto-resume the most recent session for all %d crew members\n", len(crewNames))
	}
	return crew.StartOptions{
		Account:         crewAccount,
		ClaudeConfigDir: claudeConfigDir,
		AgentOverride:   crewAgentOverride,
		ResumeSessionID: crewResume,
		KillExisting:    crewResume != "", // Resume needs to kill existing session first
	}, nil
}

func validateCrewStartResume(crewMgr *crew.Manager, crewNames []string) error {
	if crewResume == "" || crewResume == "last" {
		return nil
	}
	if len(crewNames) > 1 {
		return fmt.Errorf("--resume with a specific session ID can only target a single crew member, got %d", len(crewNames))
	}
	workers, err := crewMgr.List()
	if err != nil {
		return nil
	}
	for _, worker := range workers {
		if worker.Name == crewResume {
			return fmt.Errorf("%q looks like a crew member name, not a session ID; use --resume=%s if you meant a session ID, or use --resume (no value) to auto-resume the most recent session", crewResume, crewResume)
		}
	}
	return nil
}

type crewStartResult struct {
	name    string
	err     error
	skipped bool
}

func startCrewMembersInParallel(crewMgr *crew.Manager, crewNames []string, opts crew.StartOptions) <-chan crewStartResult {
	results := make(chan crewStartResult, len(crewNames))
	var wg sync.WaitGroup
	for _, name := range crewNames {
		wg.Add(1)
		go func(crewName string) {
			defer wg.Done()
			err := crewMgr.Start(crewName, opts)
			skipped := errors.Is(err, crew.ErrSessionRunning)
			if skipped {
				err = nil
			}
			results <- crewStartResult{name: crewName, err: err, skipped: skipped}
		}(name)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	return results
}

func collectCrewStartResults(results <-chan crewStartResult, rigName string) (error, int, int) {
	var lastErr error
	startedCount := 0
	skippedCount := 0
	for result := range results {
		if result.err != nil {
			fmt.Printf("  %s %s/%s: %v\n", style.ErrorPrefix, rigName, result.name, result.err)
			lastErr = result.err
		} else if result.skipped {
			fmt.Printf("  %s %s/%s: already running\n", style.Dim.Render("○"), rigName, result.name)
			skippedCount++
		} else {
			fmt.Printf("  %s %s/%s: started\n", style.SuccessPrefix, rigName, result.name)
			startedCount++
		}
	}
	return lastErr, startedCount, skippedCount
}

func printCrewStartSummary(startedCount, skippedCount int, rigName string) {
	fmt.Println()
	if startedCount > 0 || skippedCount > 0 {
		fmt.Printf("%s Started %d, skipped %d (already running) in %s\n",
			style.Bold.Render("✓"), startedCount, skippedCount, rigName)
	}
}

func runCrewRestart(_ *cobra.Command, args []string) error {
	// Handle --all flag
	if crewAll {
		return runCrewRestartAll()
	}

	var lastErr error

	for _, arg := range args {
		name := arg
		rigOverride := crewRig

		// Parse rig/name format (e.g., "beads/emma" -> rig=beads, name=emma)
		if rig, crewName, ok := parseRigSlashName(name); ok {
			if rigOverride == "" {
				rigOverride = rig
			}
			name = crewName
		}

		crewMgr, r, err := getCrewManagerForMember(rigOverride, name)
		if err != nil {
			fmt.Printf("Error restarting %s: %v\n", arg, err)
			lastErr = err
			continue
		}

		// Use manager's Start() with restart options
		// Start() will create workspace if needed (idempotent)
		err = crewMgr.Start(name, crew.StartOptions{
			KillExisting:  true,      // Kill old session if running
			Topic:         "restart", // Startup nudge topic
			AgentOverride: crewAgentOverride,
		})
		if err != nil {
			fmt.Printf("Error restarting %s: %v\n", arg, err)
			lastErr = err
			continue
		}

		fmt.Printf("%s Restarted crew workspace: %s/%s\n",
			style.Bold.Render("✓"), r.Name, name)
		fmt.Printf("Attach with: %s\n", style.Dim.Render(fmt.Sprintf("gt crew at %s", name)))
	}

	return lastErr
}

// runCrewRestartAll restarts all running crew sessions.
// If crewRig is set, only restarts crew in that rig.
func runCrewRestartAll() error {
	agents, err := getAgentSessions(true)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}
	targets := crewSessionsForStop(agents)

	if len(targets) == 0 {
		printNoCrewSessionsToRestart()
		return nil
	}

	if crewDryRun {
		printCrewRestartAllDryRun(targets)
		return nil
	}

	fmt.Printf("Restarting %d crew session(s)...\n\n", len(targets))

	var succeeded, failed int
	var failures []string

	for _, agent := range targets {
		agentName := fmt.Sprintf("%s/crew/%s", agent.Rig, agent.AgentName)
		if err := restartCrewSession(agent, agentName); err != nil {
			failed++
			failures = append(failures, fmt.Sprintf("%s: %v", agentName, err))
			continue
		}
		succeeded++
	}

	return summarizeCrewRestartAll(succeeded, failed, failures)
}

func printNoCrewSessionsToRestart() {
	fmt.Println("No running crew sessions to restart.")
	if crewRig != "" {
		fmt.Printf("  (filtered by rig: %s)\n", crewRig)
	}
}

func printCrewRestartAllDryRun(targets []*AgentSession) {
	fmt.Printf("Would restart %d crew session(s):\n\n", len(targets))
	for _, agent := range targets {
		fmt.Printf("  %s %s/crew/%s\n", AgentTypeIcons[AgentCrew], agent.Rig, agent.AgentName)
	}
}

func restartCrewSession(agent *AgentSession, agentName string) error {
	savedRig := crewRig
	crewRig = agent.Rig
	crewMgr, _, err := getCrewManager(crewRig)
	if err != nil {
		crewRig = savedRig
		fmt.Printf("  %s %s\n", style.ErrorPrefix, agentName)
		return err
	}
	err = crewMgr.Start(agent.AgentName, crew.StartOptions{
		KillExisting:  true,      // Kill old session if running
		Topic:         "restart", // Startup nudge topic
		AgentOverride: crewAgentOverride,
	})
	crewRig = savedRig
	if err != nil {
		fmt.Printf("  %s %s\n", style.ErrorPrefix, agentName)
		time.Sleep(constants.ShutdownNotifyDelay)
		return err
	}
	fmt.Printf("  %s %s\n", style.SuccessPrefix, agentName)
	time.Sleep(constants.ShutdownNotifyDelay)
	return nil
}

func summarizeCrewRestartAll(succeeded, failed int, failures []string) error {
	fmt.Println()
	if failed > 0 {
		fmt.Printf("%s Restart complete: %d succeeded, %d failed\n",
			style.WarningPrefix, succeeded, failed)
		for _, failure := range failures {
			fmt.Printf("  %s\n", style.Dim.Render(failure))
		}
		return fmt.Errorf("%d restart(s) failed", failed)
	}
	fmt.Printf("%s Restart complete: %d crew session(s) restarted\n", style.SuccessPrefix, succeeded)
	return nil
}

// runCrewStop stops one or more crew workers.
// Supports: "name", "rig/name" formats, "rig" (to stop all in rig), or --all.
func runCrewStop(_ *cobra.Command, args []string) error {
	if crewAll || len(args) == 0 {
		return runCrewStopAll()
	}
	if rig, ok := crewStopRig(args); ok {
		crewRig = rig
		return runCrewStopAll()
	}
	return stopCrewMembers(args)
}

func crewStopRig(args []string) (string, bool) {
	if len(args) != 1 || strings.Contains(args[0], "/") {
		return "", false
	}
	if _, _, err := getRig(args[0]); err != nil {
		return "", false
	}
	return args[0], true
}

func stopCrewMembers(args []string) error {
	var lastErr error
	t := tmux.NewTmux()

	for _, arg := range args {
		if err := stopCrewMember(t, arg); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func stopCrewMember(t *tmux.Tmux, arg string) error {
	name, rigOverride := crewStopTarget(arg)
	_, r, err := getCrewManagerForMember(rigOverride, name)
	if err != nil {
		fmt.Printf("Error stopping %s: %v\n", arg, err)
		return err
	}
	sessionID := crewSessionName(r.Name, name)
	hasSession, err := t.HasSession(sessionID)
	if err != nil {
		fmt.Printf("Error checking session %s: %v\n", sessionID, err)
		return err
	}
	if !hasSession {
		fmt.Printf("No session found for %s/%s\n", r.Name, name)
		return nil
	}
	if crewDryRun {
		fmt.Printf("Would stop %s/%s (session: %s)\n", r.Name, name, sessionID)
		return nil
	}
	return killCrewSession(t, r.Path, r.Name, name, sessionID)
}

func crewStopTarget(arg string) (string, string) {
	rigOverride := crewRig
	if rig, crewName, ok := parseRigSlashName(arg); ok {
		if rigOverride == "" {
			rigOverride = rig
		}
		return crewName, rigOverride
	}
	return arg, rigOverride
}

func killCrewSession(t *tmux.Tmux, rigPath, rigName, crewName, sessionID string) error {
	var output string
	if !crewForce {
		output, _ = t.CapturePane(sessionID, 50)
	}
	if err := t.KillSessionWithProcesses(sessionID); err != nil {
		fmt.Printf("  %s [%s] %s: %s\n",
			style.ErrorPrefix,
			rigName, crewName,
			style.Dim.Render(err.Error()))
		return err
	}
	fmt.Printf("  %s [%s] %s: stopped\n",
		style.SuccessPrefix,
		rigName, crewName)
	logCrewStop(rigPath, rigName, crewName)
	printCapturedCrewOutput(output)
	return nil
}

func logCrewStop(rigPath, rigName, crewName string) {
	townRoot, _ := workspace.Find(rigPath)
	if townRoot == "" {
		return
	}
	agent := fmt.Sprintf("%s/crew/%s", rigName, crewName)
	logger := townlog.NewLogger(townRoot)
	_ = logger.Log(townlog.EventKill, agent, "gt crew stop")
}

func printCapturedCrewOutput(output string) {
	if len(output) > 200 {
		output = output[len(output)-200:]
	}
	if output != "" {
		fmt.Printf("      %s\n", style.Dim.Render("(output captured)"))
	}
}

// runCrewStopAll stops all running crew sessions.
// If crewRig is set, only stops crew in that rig.
func runCrewStopAll() error {
	agents, err := getAgentSessions(true)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}
	targets := crewSessionsForStop(agents)

	if len(targets) == 0 {
		printNoCrewSessionsToStop()
		return nil
	}

	if crewDryRun {
		printCrewStopAllDryRun(targets)
		return nil
	}

	fmt.Printf("%s Stopping %d crew session(s)...\n\n",
		style.Bold.Render("🛑"), len(targets))

	t := tmux.NewTmux()
	var succeeded, failed int
	var failures []string

	for _, agent := range targets {
		agentName := fmt.Sprintf("%s/crew/%s", agent.Rig, agent.AgentName)
		if err := stopCrewSessionAll(t, agent, agentName); err != nil {
			failed++
			failures = append(failures, fmt.Sprintf("%s: %v", agentName, err))
			continue
		}
		succeeded++
	}

	return summarizeCrewStopAll(succeeded, failed, failures)
}

func crewSessionsForStop(agents []*AgentSession) []*AgentSession {
	var targets []*AgentSession
	for _, agent := range agents {
		if agent.Type != AgentCrew {
			continue
		}
		if crewRig != "" && agent.Rig != crewRig {
			continue
		}
		targets = append(targets, agent)
	}
	return targets
}

func printNoCrewSessionsToStop() {
	fmt.Println("No running crew sessions to stop.")
	if crewRig != "" {
		fmt.Printf("  (filtered by rig: %s)\n", crewRig)
	}
}

func printCrewStopAllDryRun(targets []*AgentSession) {
	fmt.Printf("Would stop %d crew session(s):\n\n", len(targets))
	for _, agent := range targets {
		fmt.Printf("  %s %s/crew/%s\n", AgentTypeIcons[AgentCrew], agent.Rig, agent.AgentName)
	}
}

func stopCrewSessionAll(t *tmux.Tmux, agent *AgentSession, agentName string) error {
	sessionID := agent.Name
	var output string
	if !crewForce {
		output, _ = t.CapturePane(sessionID, 50)
	}
	if err := t.KillSessionWithProcesses(sessionID); err != nil {
		fmt.Printf("  %s %s\n", style.ErrorPrefix, agentName)
		return err
	}
	fmt.Printf("  %s %s\n", style.SuccessPrefix, agentName)
	logCrewStopAll(agentName)
	printCapturedCrewOutput(output)
	return nil
}

func logCrewStopAll(agentName string) {
	townRoot, _ := workspace.FindFromCwd()
	if townRoot == "" {
		return
	}
	logger := townlog.NewLogger(townRoot)
	_ = logger.Log(townlog.EventKill, agentName, "gt crew stop --all")
}

func summarizeCrewStopAll(succeeded, failed int, failures []string) error {
	fmt.Println()
	if failed > 0 {
		fmt.Printf("%s Stop complete: %d succeeded, %d failed\n",
			style.WarningPrefix, succeeded, failed)
		for _, failure := range failures {
			fmt.Printf("  %s\n", style.Dim.Render(failure))
		}
		return fmt.Errorf("%d stop(s) failed", failed)
	}
	fmt.Printf("%s Stop complete: %d crew session(s) stopped\n", style.SuccessPrefix, succeeded)
	return nil
}
