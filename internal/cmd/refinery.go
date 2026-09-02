package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/refinery"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var refineryCmd = &cobra.Command{
	Use:     "refinery",
	Aliases: []string{"ref"},
	GroupID: GroupAgents,
	Short:   "Manage the Refinery (merge queue processor)",
	RunE:    requireSubcommand,
	Long: `Manage the Refinery - the per-rig merge queue processor.

The Refinery serializes all merges to main for a rig:
  - Receives MRs submitted by polecats (via gt done)
  - Rebases work branches onto latest main
  - Runs validation (tests, builds, checks)
  - Merges to main when clear
  - If conflict: spawns FRESH polecat to re-implement (original is gone)

Work flows: Polecat completes → gt done → MR in queue → Refinery merges.
The polecat is already nuked by the time the Refinery processes.

One Refinery per rig. Persistent agent that processes work as it arrives.

Role shortcuts: "refinery" in mail/nudge addresses resolves to this rig's Refinery.`,
}

var refineryStartCmd = &cobra.Command{
	Use:     "start [rig]",
	Aliases: []string{"spawn"},
	Short:   "Start the refinery",
	Long: `Start the Refinery for a rig.

Launches the merge queue processor which monitors for polecat work branches
and merges them to the appropriate target branches.

If rig is not specified, infers it from the current directory.

Examples:
  gt refinery start greenplace
  gt refinery start              # infer rig from cwd`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRefineryStart,
}

var refineryStopCmd = &cobra.Command{
	Use:   "stop [rig]",
	Short: "Stop the refinery",
	Long: `Stop a running Refinery.

Gracefully stops the refinery, completing any in-progress merge first.
If rig is not specified, infers it from the current directory.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRefineryStop,
}

var refineryStatusCmd = &cobra.Command{
	Use:   "status [rig]",
	Short: "Show refinery status",
	Long: `Show the status of a rig's Refinery.

Displays running state, current work, queue length, and statistics.
If rig is not specified, infers it from the current directory.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRefineryStatus,
}

var refineryQueueCmd = &cobra.Command{
	Use:   "queue [rig]",
	Short: "Show merge queue",
	Long: `Show the merge queue for a rig.

Lists all pending merge requests waiting to be processed.
If rig is not specified, infers it from the current directory.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRefineryQueue,
}

var refineryAttachCmd = &cobra.Command{
	Use:   "attach [rig]",
	Short: "Attach to refinery session",
	Long: `Attach to a running Refinery's Claude session.

Allows interactive access to the Refinery agent for debugging
or manual intervention.

If rig is not specified, infers it from the current directory.

Examples:
  gt refinery attach greenplace
  gt refinery attach          # infer rig from cwd`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRefineryAttach,
}

var refineryRestartCmd = &cobra.Command{
	Use:   "restart [rig]",
	Short: "Restart the refinery",
	Long: `Restart the Refinery for a rig.

Stops the current session (if running) and starts a fresh one.
If rig is not specified, infers it from the current directory.

Examples:
  gt refinery restart greenplace
  gt refinery restart          # infer rig from cwd`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRefineryRestart,
}

var refineryClaimCmd = &cobra.Command{
	Use:   "claim <mr-id>",
	Short: "Claim an MR for processing",
	Long: `Claim a merge request for processing by this refinery worker.

When running multiple refinery workers in parallel, each worker must claim
an MR before processing to prevent double-processing. Claims expire after
10 minutes if not processed (for crash recovery).

The worker ID is automatically determined from the GT_REFINERY_WORKER
environment variable, or defaults to "refinery-1".

Examples:
  gt refinery claim gt-abc123
  GT_REFINERY_WORKER=refinery-2 gt refinery claim gt-abc123`,
	Args: cobra.ExactArgs(1),
	RunE: runRefineryClaim,
}

var refineryReleaseCmd = &cobra.Command{
	Use:   "release <mr-id>",
	Short: "Release a claimed MR back to the queue",
	Long: `Release a claimed merge request back to the queue.

Called when processing fails and the MR should be retried by another worker.
This clears the claim so other workers can pick up the MR.

Examples:
  gt refinery release gt-abc123`,
	Args: cobra.ExactArgs(1),
	RunE: runRefineryRelease,
}

var refineryUnclaimedCmd = &cobra.Command{
	Use:   "unclaimed [rig]",
	Short: "List unclaimed MRs available for processing",
	Long: `List merge requests that are available for claiming.

Shows MRs that are not currently claimed by any worker, or have stale
claims (worker may have crashed). Useful for parallel refinery workers
to find work.

Examples:
  gt refinery unclaimed
  gt refinery unclaimed --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRefineryUnclaimed,
}

var refineryReadyCmd = &cobra.Command{
	Use:   "ready [rig]",
	Short: "List MRs ready for processing (unclaimed and unblocked)",
	Long: `List merge requests ready for processing.

Shows MRs that are:
- Not currently claimed by any worker (or claim is stale)
- Not blocked by an open task (e.g., conflict resolution in progress)

This is the preferred command for finding work to process.

Use --all to see ALL open MRs (claimed, blocked, etc.) with raw data
including timestamps, assignees, and branch existence. Designed for
agent-side queue health analysis.

Examples:
  gt refinery ready
  gt refinery ready --json
  gt refinery ready --all --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRefineryReady,
}

var refineryBlockedCmd = &cobra.Command{
	Use:   "blocked [rig]",
	Short: "List MRs blocked by open tasks",
	Long: `List merge requests blocked by open tasks.

Shows MRs waiting for conflict resolution or other blocking tasks to complete.
When the blocking task closes, the MR will appear in 'ready'.

Examples:
  gt refinery blocked
  gt refinery blocked --json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRefineryBlocked,
}

func init() {
	// Start flags
	refineryStartCmd.Flags().Bool("foreground", false, "Run in foreground (default: background)")
	_ = refineryStartCmd.Flags().MarkHidden("foreground")
	refineryStartCmd.Flags().String("agent", "", "Agent alias to run the Refinery with (overrides town default)")
	refineryStartCmd.Flags().Bool("force", false, "Start even when rig has upstream_url (manual override for fork-backed rigs)")

	// Attach flags
	refineryAttachCmd.Flags().String("agent", "", "Agent alias to run the Refinery with (overrides town default)")

	// Restart flags
	refineryRestartCmd.Flags().String("agent", "", "Agent alias to run the Refinery with (overrides town default)")
	refineryRestartCmd.Flags().Bool("force", false, "Restart even when rig has upstream_url (manual override for fork-backed rigs)")

	// Status flags
	refineryStatusCmd.Flags().Bool("json", false, "Output as JSON")

	// Queue flags
	refineryQueueCmd.Flags().Bool("json", false, "Output as JSON")

	// Unclaimed flags
	refineryUnclaimedCmd.Flags().Bool("json", false, "Output as JSON")

	// Ready flags
	refineryReadyCmd.Flags().Bool("json", false, "Output as JSON")
	refineryReadyCmd.Flags().Bool("all", false, "Show all open MRs (claimed, blocked, etc.) with raw data for queue health analysis")

	// Blocked flags
	refineryBlockedCmd.Flags().Bool("json", false, "Output as JSON")

	// Add subcommands
	refineryCmd.AddCommand(refineryStartCmd)
	refineryCmd.AddCommand(refineryStopCmd)
	refineryCmd.AddCommand(refineryRestartCmd)
	refineryCmd.AddCommand(refineryStatusCmd)
	refineryCmd.AddCommand(refineryQueueCmd)
	refineryCmd.AddCommand(refineryAttachCmd)
	refineryCmd.AddCommand(refineryClaimCmd)
	refineryCmd.AddCommand(refineryReleaseCmd)
	refineryCmd.AddCommand(refineryUnclaimedCmd)
	refineryCmd.AddCommand(refineryReadyCmd)
	refineryCmd.AddCommand(refineryBlockedCmd)

	rootCmd.AddCommand(refineryCmd)
}

type refineryManagerContext struct {
	mgr     *refinery.Manager
	r       *rig.Rig
	rigName string
}

// getRefineryManager creates a refinery manager for a rig.
// If rigName is empty, infers the rig from cwd.
func getRefineryManager(rigName string) (refineryManagerContext, error) {
	// Infer rig from cwd if not provided
	if rigName == "" {
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return refineryManagerContext{}, fmt.Errorf("not in a Gas Town workspace: %w", err)
		}
		rigName, err = inferRigFromCwd(townRoot)
		if err != nil {
			return refineryManagerContext{}, fmt.Errorf("could not determine rig: %w\nUsage: gt refinery <command> <rig>", err)
		}
	}

	_, r, err := getRig(rigName)
	if err != nil {
		return refineryManagerContext{}, err
	}

	return refineryManagerContext{mgr: refinery.NewManager(r), r: r, rigName: rigName}, nil
}

func runRefineryStart(cmd *cobra.Command, args []string) error {
	foreground := commandBoolFlag(cmd, "foreground")
	force := commandBoolFlag(cmd, "force")
	agentOverride := commandStringFlag(cmd, "agent")
	rigName := ""
	if len(args) > 0 {
		rigName = args[0]
	}

	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	mgr := ctx.mgr
	rigName = ctx.rigName

	if err := checkRigNotParkedOrDocked(rigName); err != nil {
		return err
	}
	if foreground {
		return fmt.Errorf("foreground mode is deprecated; use background mode (remove --foreground flag)")
	}

	fmt.Printf("Starting refinery for %s...\n", rigName)

	start := mgr.Start
	if force {
		start = mgr.StartAllowingForkRig
	}
	if err := start(foreground, agentOverride); err != nil {
		if errors.Is(err, refinery.ErrAlreadyRunning) {
			fmt.Printf("%s Refinery is already running\n", style.Dim.Render("⚠"))
			return nil
		}
		return fmt.Errorf("starting refinery: %w", err)
	}

	fmt.Printf("%s Refinery started for %s\n", style.Bold.Render("✓"), rigName)
	fmt.Printf("  %s\n", style.Dim.Render("Use 'gt refinery status' to check progress"))
	return nil
}

func runRefineryStop(_ *cobra.Command, args []string) error {
	rigName := ""
	if len(args) > 0 {
		rigName = args[0]
	}

	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	mgr := ctx.mgr
	rigName = ctx.rigName

	if err := mgr.Stop(); err != nil {
		if err == refinery.ErrNotRunning {
			fmt.Printf("%s Refinery is not running\n", style.Dim.Render("⚠"))
			return nil
		}
		return fmt.Errorf("stopping refinery: %w", err)
	}

	fmt.Printf("%s Refinery stopped for %s\n", style.Bold.Render("✓"), rigName)
	return nil
}

// RefineryStatusOutput is the JSON output format for refinery status.
type RefineryStatusOutput struct {
	Running     bool   `json:"running"`
	RigName     string `json:"rig_name"`
	Session     string `json:"session,omitempty"`
	QueueLength int    `json:"queue_length"`
}

func runRefineryStatus(cmd *cobra.Command, args []string) error {
	statusJSON := commandBoolFlag(cmd, "json")
	rigName := ""
	if len(args) > 0 {
		rigName = args[0]
	}

	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	mgr := ctx.mgr
	rigName = ctx.rigName

	// ZFC: tmux is source of truth for running state
	running, _ := mgr.IsRunning()
	sessionInfo, _ := mgr.Status() // may be nil if not running

	// Get queue from beads
	queue, _ := mgr.Queue()
	queueLen := len(queue)

	// JSON output
	if statusJSON {
		output := RefineryStatusOutput{
			Running:     running,
			RigName:     rigName,
			QueueLength: queueLen,
		}
		if sessionInfo != nil {
			output.Session = sessionInfo.Name
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	// Human-readable output
	fmt.Printf("%s Refinery: %s\n\n", style.Bold.Render("⚙"), rigName)

	if running {
		fmt.Printf("  State: %s\n", style.Bold.Render("● running"))
		if sessionInfo != nil {
			fmt.Printf("  Session: %s\n", sessionInfo.Name)
		}
	} else {
		fmt.Printf("  State: %s\n", style.Dim.Render("○ stopped"))
	}

	fmt.Printf("\n  Queue: %d pending\n", queueLen)

	return nil
}

func runRefineryQueue(cmd *cobra.Command, args []string) error {
	queueJSON := commandBoolFlag(cmd, "json")
	rigName := ""
	if len(args) > 0 {
		rigName = args[0]
	}

	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	mgr := ctx.mgr
	rigName = ctx.rigName

	queue, err := mgr.Queue()
	if err != nil {
		return fmt.Errorf("getting queue: %w", err)
	}
	return outputRefineryQueue(queue, rigName, queueJSON)
}

func outputRefineryQueue(queue []refinery.QueueItem, rigName string, queueJSON bool) error {
	if queueJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(queue)
	}

	// Human-readable output
	fmt.Printf("%s Merge queue for '%s':\n\n", style.Bold.Render("📋"), rigName)

	if len(queue) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(empty)"))
		return nil
	}

	for _, item := range queue {
		printRefineryQueueItem(item)
	}

	return nil
}

func printRefineryQueueItem(item refinery.QueueItem) {
	prefix := fmt.Sprintf("  %d.", item.Position)
	if item.Position == 0 {
		prefix = "  ▶"
	}
	issueInfo := ""
	if item.MR.IssueID != "" {
		issueInfo = fmt.Sprintf(" (%s)", item.MR.IssueID)
	}
	fmt.Printf("%s %s %s/%s%s %s\n", prefix, refineryQueueStatus(item), item.MR.Worker, item.MR.Branch, issueInfo, style.Dim.Render(item.Age))
}

func refineryQueueStatus(item refinery.QueueItem) string {
	if item.Position == 0 {
		return style.Bold.Render("[processing]")
	}
	switch item.MR.Status {
	case refinery.MROpen:
		if item.MR.Error != "" {
			return style.Dim.Render("[needs-rework]")
		}
		return style.Dim.Render("[pending]")
	case refinery.MRInProgress:
		return style.Bold.Render("[processing]")
	case refinery.MRClosed:
		return refineryClosedStatus(item.MR.CloseReason)
	default:
		return ""
	}
}

func refineryClosedStatus(reason refinery.CloseReason) string {
	status := map[refinery.CloseReason]string{
		refinery.CloseReasonMerged:     "[merged]",
		refinery.CloseReasonRejected:   "[rejected]",
		refinery.CloseReasonConflict:   "[conflict]",
		refinery.CloseReasonSuperseded: "[superseded]",
	}
	if value, ok := status[reason]; ok {
		if reason == refinery.CloseReasonMerged {
			return style.Bold.Render(value)
		}
		return style.Dim.Render(value)
	}
	return style.Dim.Render("[closed]")
}

func runRefineryAttach(cmd *cobra.Command, args []string) error {
	agentOverride := commandStringFlag(cmd, "agent")
	rigName := ""
	if len(args) > 0 {
		rigName = args[0]
	}

	// Use getRefineryManager to validate rig (and infer from cwd if needed)
	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	mgr := ctx.mgr
	r := ctx.r
	rigName = ctx.rigName

	// Session name follows the same pattern as refinery manager
	sessionID := session.RefinerySessionName(session.PrefixFor(rigName))

	// Check if session exists
	t := tmux.NewTmux()
	running, err := t.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if err := handleRefinerySafetyStop(mgr, r, rigName, sessionID, running); err != nil {
		return err
	}
	if !running {
		if err := autoStartRefinery(mgr, rigName, agentOverride); err != nil {
			return err
		}
	}

	// Attach to session using exec to properly forward TTY
	return attachToTmuxSession(sessionID)
}

func handleRefinerySafetyStop(mgr *refinery.Manager, r *rig.Rig, rigName, sessionID string, running bool) error {
	stop, err := refinery.ActiveSafetyStop(filepath.Dir(r.Path), rigName)
	if err != nil {
		return fmt.Errorf("checking refinery safety stop: %w", err)
	}
	if stop == nil {
		return nil
	}
	if running {
		fmt.Printf("Refinery %s is safety-stopped; stopping leftover session %s.\n", rigName, sessionID)
		if err := mgr.Stop(); err != nil && err != refinery.ErrNotRunning {
			return fmt.Errorf("%w: stopping leftover refinery session: %v", refinery.NewSafetyStoppedError(stop), err)
		}
	}
	return refinery.NewSafetyStoppedError(stop)
}

func autoStartRefinery(mgr *refinery.Manager, rigName, agentOverride string) error {
	fmt.Printf("Refinery not running for %s, starting...\n", rigName)
	if err := mgr.Start(false, agentOverride); err != nil {
		if errors.Is(err, refinery.ErrForkRig) {
			return fmt.Errorf("refinery auto-start skipped: %w", err)
		}
		return fmt.Errorf("starting refinery: %w", err)
	}
	fmt.Printf("%s Refinery started\n", style.Bold.Render("✓"))
	return nil
}

func runRefineryRestart(cmd *cobra.Command, args []string) error {
	force := commandBoolFlag(cmd, "force")
	agentOverride := commandStringFlag(cmd, "agent")
	rigName := ""
	if len(args) > 0 {
		rigName = args[0]
	}

	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	mgr := ctx.mgr
	r := ctx.r
	rigName = ctx.rigName

	if err := checkRigNotParkedOrDocked(rigName); err != nil {
		return err
	}

	if !force {
		if err := mgr.BlockForkRigStart(); err != nil {
			return fmt.Errorf("starting refinery: %w", err)
		}
	}

	fmt.Printf("Restarting refinery for %s...\n", rigName)
	if err := checkRefineryRestartSafety(mgr, r, rigName); err != nil {
		return err
	}

	if err := stopRefineryForRestart(mgr); err != nil {
		return fmt.Errorf("stopping refinery: %w", err)
	}

	if err := startRefineryForRestart(mgr, force, agentOverride); err != nil {
		return fmt.Errorf("starting refinery: %w", err)
	}

	fmt.Printf("%s Refinery restarted for %s\n", style.Bold.Render("✓"), rigName)
	fmt.Printf("  %s\n", style.Dim.Render("Use 'gt refinery attach' to connect"))
	return nil
}

func checkRefineryRestartSafety(mgr *refinery.Manager, r *rig.Rig, rigName string) error {
	stop, err := refinery.ActiveSafetyStop(filepath.Dir(r.Path), rigName)
	if err != nil {
		return fmt.Errorf("checking refinery safety stop: %w", err)
	}
	if stop == nil {
		return nil
	}
	if err := mgr.Stop(); err != nil && err != refinery.ErrNotRunning {
		return fmt.Errorf("%w: stopping leftover refinery session: %v", refinery.NewSafetyStoppedError(stop), err)
	}
	return refinery.NewSafetyStoppedError(stop)
}

func stopRefineryForRestart(mgr *refinery.Manager) error {
	if err := mgr.Stop(); err != nil && err != refinery.ErrNotRunning {
		return err
	}
	return nil
}

func startRefineryForRestart(mgr *refinery.Manager, force bool, agentOverride string) error {
	if force {
		return mgr.StartAllowingForkRig(false, agentOverride)
	}
	return mgr.Start(false, agentOverride)
}

// getWorkerID returns the refinery worker ID from environment or default.
func getWorkerID() string {
	if id := os.Getenv("GT_REFINERY_WORKER"); id != "" {
		return id
	}
	return "refinery-1"
}

func runRefineryClaim(_ *cobra.Command, args []string) error {
	mrID := args[0]
	workerID := getWorkerID()

	// Find beads from current working directory
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigName, err := inferRigFromCwd(townRoot)
	if err != nil {
		return fmt.Errorf("could not determine rig: %w", err)
	}

	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	eng := refinery.NewEngineer(r)
	if err := eng.ClaimMR(mrID, workerID); err != nil {
		return fmt.Errorf("claiming MR: %w", err)
	}

	fmt.Printf("%s Claimed %s for %s\n", style.Bold.Render("✓"), mrID, workerID)
	return nil
}

func runRefineryRelease(_ *cobra.Command, args []string) error {
	mrID := args[0]

	// Find beads from current working directory
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigName, err := inferRigFromCwd(townRoot)
	if err != nil {
		return fmt.Errorf("could not determine rig: %w", err)
	}

	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	eng := refinery.NewEngineer(r)
	if err := eng.ReleaseMR(mrID); err != nil {
		return fmt.Errorf("releasing MR: %w", err)
	}

	fmt.Printf("%s Released %s back to queue\n", style.Bold.Render("✓"), mrID)
	return nil
}

func runRefineryUnclaimed(cmd *cobra.Command, args []string) error {
	unclaimedJSON := commandBoolFlag(cmd, "json")
	rigName := ""
	if len(args) > 0 {
		rigName = args[0]
	}

	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	r := ctx.r
	rigName = ctx.rigName

	// Query beads for merge-request issues without assignee
	b := beads.New(r.Path)
	issues, err := b.ListMergeRequests(beads.ListOptions{
		Status:   "open",
		Label:    "gt:merge-request",
		Priority: -1,
		Rig:      rigName,
	})
	if err != nil {
		return fmt.Errorf("listing merge requests: %w", err)
	}
	return outputUnclaimedRefineryMRs(collectUnclaimedRefineryMRs(issues), rigName, unclaimedJSON)
}

func collectUnclaimedRefineryMRs(issues []*beads.Issue) []*refinery.MRInfo {
	var unclaimed []*refinery.MRInfo
	for _, issue := range issues {
		if issue.Assignee != "" {
			continue
		}
		fields := beads.ParseMRFields(issue)
		if fields == nil {
			continue
		}
		mr := &refinery.MRInfo{
			ID:       issue.ID,
			Branch:   fields.Branch,
			Target:   fields.Target,
			Worker:   fields.Worker,
			Priority: issue.Priority,
		}
		unclaimed = append(unclaimed, mr)
	}
	return unclaimed
}

func outputUnclaimedRefineryMRs(unclaimed []*refinery.MRInfo, rigName string, unclaimedJSON bool) error {
	if unclaimedJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(unclaimed)
	}

	// Human-readable output
	fmt.Printf("%s Unclaimed MRs for '%s':\n\n", style.Bold.Render("📋"), rigName)

	if len(unclaimed) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(none available)"))
		return nil
	}

	for i, mr := range unclaimed {
		printRefineryMRSummary(i, mr)
	}

	return nil
}

func runRefineryReady(cmd *cobra.Command, args []string) error {
	readyJSON := commandBoolFlag(cmd, "json")
	readyAll := commandBoolFlag(cmd, "all")
	rigName := ""
	if len(args) > 0 {
		rigName = args[0]
	}

	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	r := ctx.r
	rigName = ctx.rigName

	// Create engineer for the rig (it has beads access for status checking)
	eng := refinery.NewEngineer(r)

	if readyAll {
		return runRefineryReadyAll(eng, rigName, readyJSON)
	}

	// Get ready MRs (unclaimed AND unblocked)
	ready, err := eng.ListReadyMRs()
	if err != nil {
		return fmt.Errorf("listing ready MRs: %w", err)
	}
	anomalies, err := eng.ListQueueAnomalies(time.Now())
	if err != nil {
		return fmt.Errorf("listing queue anomalies: %w", err)
	}
	return outputRefineryReady(ready, anomalies, rigName, readyJSON)
}

func outputRefineryReady(ready []*refinery.MRInfo, anomalies []*refinery.MRAnomaly, rigName string, readyJSON bool) error {
	if readyJSON {
		type readyOutput struct {
			Ready     []*refinery.MRInfo    `json:"ready"`
			Anomalies []*refinery.MRAnomaly `json:"anomalies,omitempty"`
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(readyOutput{
			Ready:     ready,
			Anomalies: anomalies,
		})
	}

	// Human-readable output
	fmt.Printf("%s Ready MRs for '%s':\n\n", style.Bold.Render("🚀"), rigName)

	if len(ready) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(none ready)"))
		return nil
	}

	for i, mr := range ready {
		printRefineryMRSummary(i, mr)
	}

	printRefineryAnomalies(anomalies)

	return nil
}

func printRefineryMRSummary(index int, mr *refinery.MRInfo) {
	priority := fmt.Sprintf("P%d", mr.Priority)
	fmt.Printf("  %d. [%s] %s → %s\n", index+1, priority, mr.Branch, mr.Target)
	fmt.Printf("     ID: %s  Worker: %s\n", mr.ID, mr.Worker)
}

func printRefineryAnomalies(anomalies []*refinery.MRAnomaly) {
	if len(anomalies) == 0 {
		return
	}
	fmt.Printf("\n%s Queue anomalies:\n\n", style.Bold.Render("⚠"))
	for i, anomaly := range anomalies {
		fmt.Printf("  %d. [%s] %s\n", i+1, anomaly.Type, anomaly.ID)
		fmt.Printf("     Branch: %s\n", anomaly.Branch)
		if anomaly.Assignee != "" {
			fmt.Printf("     Assignee: %s\n", anomaly.Assignee)
		}
		if anomaly.Age > 0 {
			fmt.Printf("     Age: %s\n", anomaly.Age.Truncate(time.Second))
		}
		fmt.Printf("     Detail: %s\n", anomaly.Detail)
	}
}

func runRefineryReadyAll(eng *refinery.Engineer, rigName string, readyJSON bool) error {
	mrs, err := eng.ListAllOpenMRs()
	if err != nil {
		return fmt.Errorf("listing all open MRs: %w", err)
	}

	if readyJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(mrs)
	}
	return outputRefineryReadyAllText(mrs, rigName)
}

func outputRefineryReadyAllText(mrs []*refinery.MRInfo, rigName string) error {
	fmt.Printf("%s All Open MRs for '%s':\n\n", style.Bold.Render("📋"), rigName)

	if len(mrs) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(none)"))
		return nil
	}

	for i, mr := range mrs {
		printRefineryReadyAllMR(i, mr)
	}

	return nil
}

func printRefineryReadyAllMR(index int, mr *refinery.MRInfo) {
	priority := fmt.Sprintf("P%d", mr.Priority)
	fmt.Printf("  %d. [%s] %s → %s\n", index+1, priority, mr.Branch, mr.Target)
	assignee := mr.Assignee
	if assignee == "" {
		assignee = "(unclaimed)"
	}
	age := ""
	if !mr.UpdatedAt.IsZero() {
		age = fmt.Sprintf(" (updated %s ago)", time.Since(mr.UpdatedAt).Truncate(time.Second))
	}
	fmt.Printf("     ID: %s  Worker: %s  Assignee: %s%s\n", mr.ID, mr.Worker, assignee, age)
	printRefineryMRFlags(mr)
}

func printRefineryMRFlags(mr *refinery.MRInfo) {
	var flags []string
	if mr.BlockedBy != "" {
		flags = append(flags, fmt.Sprintf("blocked-by:%s", mr.BlockedBy))
	}
	if !mr.BranchExistsLocal && !mr.BranchExistsRemote {
		flags = append(flags, "no-branch")
	}
	if len(flags) > 0 {
		fmt.Printf("     Flags: %s\n", style.Dim.Render(fmt.Sprintf("[%s]", strings.Join(flags, ", "))))
	}
}

func runRefineryBlocked(cmd *cobra.Command, args []string) error {
	blockedJSON := commandBoolFlag(cmd, "json")
	rigName := ""
	if len(args) > 0 {
		rigName = args[0]
	}

	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	r := ctx.r
	rigName = ctx.rigName

	// Create engineer for the rig (it has beads access for status checking)
	eng := refinery.NewEngineer(r)

	// Get blocked MRs
	blocked, err := eng.ListBlockedMRs()
	if err != nil {
		return fmt.Errorf("listing blocked MRs: %w", err)
	}

	// JSON output
	if blockedJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(blocked)
	}

	// Human-readable output
	fmt.Printf("%s Blocked MRs for '%s':\n\n", style.Bold.Render("🚧"), rigName)

	if len(blocked) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(none blocked)"))
		return nil
	}

	for i, mr := range blocked {
		priority := fmt.Sprintf("P%d", mr.Priority)
		fmt.Printf("  %d. [%s] %s → %s\n", i+1, priority, mr.Branch, mr.Target)
		fmt.Printf("     ID: %s  Worker: %s\n", mr.ID, mr.Worker)
		if mr.BlockedBy != "" {
			fmt.Printf("     Blocked by: %s\n", mr.BlockedBy)
		}
	}

	return nil
}
