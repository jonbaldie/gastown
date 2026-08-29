package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/refinery"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

var mqCmd = &cobra.Command{
	Use:     "mq",
	Aliases: []string{"mr"},
	GroupID: GroupWork,
	Short:   "Merge queue operations",
	RunE:    requireSubcommand,
	Long: `Manage merge requests and the merge queue for a rig.

Alias: 'gt mr' is equivalent to 'gt mq' (merge request vs merge queue).

The merge queue tracks work branches from polecats waiting to be merged.
Use these commands to view, submit, retry, and manage merge requests.`,
}

var mqSubmitCmd = &cobra.Command{
	Use:   "submit",
	Short: "Submit current branch to the merge queue",
	Long: `Submit the current branch to the merge queue.

Creates a merge-request bead that will be processed by the Refinery.

Auto-detection:
  - Branch: current git branch
  - Issue: parsed from branch name (e.g., polecat/Nux/gp-xyz → gt-xyz)
  - Worker: parsed from branch name
  - Rig: detected from current directory
  - Target: automatically determined (see below)
  - Priority: inherited from source issue

Target branch auto-detection:
  1. If --epic is specified: target the integration branch for <epic> (using configured template)
  2. If source issue has a parent epic with an integration branch: target it
  3. Otherwise: target main

This ensures batch work on epics automatically flows to integration branches.

Polecat auto-cleanup:
  When run from a polecat work branch (polecat/<worker>/<issue>), this command
  automatically triggers polecat shutdown after submitting the MR. The polecat
  sends a lifecycle request to its Witness and waits for termination.

  Use --no-cleanup to disable this behavior (e.g., if you want to submit
  multiple MRs or continue working).

Examples:
  gt mq submit                           # Auto-detect everything + auto-cleanup
  gt mq submit --issue gp-abc            # Explicit issue
  gt mq submit --epic gt-xyz             # Target integration branch explicitly
  gt mq submit --priority 0              # Override priority (P0)
  gt mq submit --no-cleanup              # Submit without auto-cleanup`,
	RunE: runMqSubmit,
}

var mqRetryCmd = &cobra.Command{
	Use:   "retry <rig> <mr-id>",
	Short: "Retry a failed merge request",
	Long: `Retry a failed merge request.

Resets a failed MR so it can be processed again by the refinery.
The MR must be in a failed state (open with an error).

Examples:
  gt mq retry greenplace gp-mr-abc123
  gt mq retry greenplace gp-mr-abc123 --now`,
	Args: cobra.ExactArgs(2),
	RunE: runMQRetry,
}

var mqListCmd = &cobra.Command{
	Use:   "list <rig>",
	Short: "Show the merge queue",
	Long: `Show the merge queue for a rig.

Lists all pending merge requests waiting to be processed.

Output format:
  ID          STATUS       PRIORITY  BRANCH                    WORKER  AGE
  gt-mr-001   ready        P0        polecat/Nux/gp-xyz        Nux     5m
  gt-mr-002   in_progress  P1        polecat/Toast/gt-abc      Toast   12m
  gt-mr-003   blocked      P1        polecat/Capable/gt-def    Capable 8m
              (waiting on gt-mr-001)

Examples:
  gt mq list greenplace
  gt mq list greenplace --ready
  gt mq list greenplace --status=open
  gt mq list greenplace --worker=Nux`,
	Args: cobra.ExactArgs(1),
	RunE: runMQList,
}

var mqRejectCmd = &cobra.Command{
	Use:   "reject <rig> <mr-id-or-branch>",
	Short: "Reject a merge request",
	Long: `Manually reject a merge request.

This closes the MR with a 'rejected' status without merging.
The source issue is NOT closed (work is not done).

Examples:
  gt mq reject greenplace polecat/Nux/gp-xyz --reason "Does not meet requirements"
  gt mq reject greenplace mr-Nux-12345 --reason "Superseded by other work" --notify`,
	Args: cobra.ExactArgs(2),
	RunE: runMQReject,
}

// Post-merge flags
var mqPostMergeCmd = &cobra.Command{
	Use:   "post-merge <rig> <mr-id>",
	Short: "Run post-merge cleanup (close MR, delete branch)",
	Long: `Perform post-merge cleanup after a successful merge.

This command consolidates post-merge steps into a single atomic operation:
	 1. Verify the target branch contains the submitted source head
	 2. Close the MR bead (status: merged)
	 3. Close the source issue
	 4. Delete the remote polecat branch at the submitted head (unless --skip-branch-delete)

Designed for use by the refinery formula after a successful merge to main.
The branch name is read from the MR bead, so no manual branch argument is needed.

Examples:
  gt mq post-merge gastown gt-mr-abc123
  gt mq post-merge gastown gt-mr-abc123 --skip-branch-delete`,
	Args: cobra.ExactArgs(2),
	RunE: runMQPostMerge,
}

type mqPostMergeManager interface {
	FindMRForPostMerge(_ string) (*refinery.MergeRequest, error)
	PostMergeMR(_ *refinery.MergeRequest) (*refinery.PostMergeResult, error)
}

type mqPostMergeGit interface {
	VerifyPushedCommitReachableFromPushTarget(_, _, _ string) error
	PushRemoteBranchTip(_, _ string) (string, error)
	HasOpenPullRequest(_ git.PullRequestRef) bool
	Rev(_ string) (string, error)
	DeleteRemoteBranchIfAt(_, _, _ string) error
	DeleteBranch(_ string, _ bool) error
}

type mqPostMergeBranchCleanup struct {
	Branch        string
	NoBranch      bool
	Skipped       bool
	Disabled      bool
	OpenPR        bool
	AlreadyGone   bool
	RemoteDeleted bool
	LocalDeleted  bool
}

var mqStatusCmd = &cobra.Command{
	Use:   "status <id>",
	Short: "Show detailed merge request status",
	Long: `Display detailed information about a merge request.

Shows all MR fields, current status with timestamps, dependencies,
blockers, and processing history.

Example:
  gt mq status gp-mr-abc123`,
	Args: cobra.ExactArgs(1),
	RunE: runMqStatus,
}

var mqIntegrationCmd = &cobra.Command{
	Use:   "integration",
	Short: "Manage integration branches for epics",
	RunE:  requireSubcommand,
	Long: `Manage integration branches for batch work on epics.

Integration branches allow multiple MRs for an epic to target a shared
branch instead of main. After all epic work is complete, the integration
branch is landed to main as a single atomic unit.

Commands:
  create  Create an integration branch for an epic
  land    Merge integration branch to main
  status  Show integration branch status`,
}

var mqIntegrationCreateCmd = &cobra.Command{
	Use:   "create <epic-id>",
	Short: "Create an integration branch for an epic",
	Long: `Create an integration branch for batch work on an epic.

Creates a branch from main and pushes it to origin. Future MRs for this
epic's children can target this branch.

Branch naming:
  Default: integration/<sanitized-title> (e.g., integration/add-user-auth)
  Config:  Set merge_queue.integration_branch_template in rig settings
  Override: Use --branch flag for one-off customization

Template variables:
  {title}  - Sanitized epic title (e.g., "add-user-authentication")
  {epic}   - Full epic ID (e.g., "RA-123")
  {prefix} - Epic prefix before first hyphen (e.g., "RA")
  {user}   - Git user.name (e.g., "klauern")

If two epics produce the same branch name, a numeric suffix from the
epic ID is appended automatically (e.g., integration/add-auth-123).

Actions:
  1. Verify epic exists
  2. Create branch from main (using template or --branch)
  3. Push to origin
  4. Store actual branch name in epic metadata

Examples:
  gt mq integration create gt-auth-epic
  # Creates integration/add-user-authentication (from epic title)

  gt mq integration create RA-123 --branch "klauern/PROJ-1234/{epic}"
  # Creates klauern/PROJ-1234/RA-123`,
	Args: cobra.ExactArgs(1),
	RunE: runMqIntegrationCreate,
}

var mqIntegrationLandCmd = &cobra.Command{
	Use:   "land <epic-id>",
	Short: "Merge integration branch to main",
	Long: `Merge an epic's integration branch to main.

Lands all work for an epic by merging its integration branch to main
as a single atomic merge commit.

Actions:
  1. Verify all MRs targeting integration/<epic> are merged
  2. Verify integration branch exists
  3. Merge integration/<epic> to main (--no-ff)
  4. Run tests on main
  5. Push to origin
  6. Delete integration branch
  7. Update epic status

Options:
  --force       Land even if some MRs still open
  --skip-tests  Skip test run
  --dry-run     Preview only, make no changes

Examples:
  gt mq integration land gt-auth-epic
  gt mq integration land gt-auth-epic --dry-run
  gt mq integration land gt-auth-epic --force --skip-tests`,
	Args: cobra.ExactArgs(1),
	RunE: runMqIntegrationLand,
}

var mqIntegrationStatusCmd = &cobra.Command{
	Use:   "status <epic-id>",
	Short: "Show integration branch status for an epic",
	Long: `Display the status of an integration branch.

Shows:
  - Integration branch name and creation date
  - Number of commits ahead of main
  - Merged MRs (closed, targeting integration branch)
  - Pending MRs (open, targeting integration branch)

Example:
  gt mq integration status gt-auth-epic`,
	Args: cobra.ExactArgs(1),
	RunE: runMqIntegrationStatus,
}

func init() {
	// Submit flags
	mqSubmitCmd.Flags().String("branch", "", "Source branch (default: current branch)")
	mqSubmitCmd.Flags().String("issue", "", "Source issue ID (default: parse from branch name)")
	mqSubmitCmd.Flags().String("epic", "", "Target epic's integration branch instead of main")
	mqSubmitCmd.Flags().IntP("priority", "p", -1, "Override priority (0-4, default: inherit from issue)")
	mqSubmitCmd.Flags().Bool("no-cleanup", false, "Don't auto-cleanup after submit (for polecats)")
	mqSubmitCmd.Flags().Bool("skip-deps", false, "Skip molecule step dependency check")
	mqSubmitCmd.Flags().Bool("resubmit", false, "Resubmit after a fix (skips dependency check)")

	// Retry flags
	mqRetryCmd.Flags().Bool("now", false, "Immediately process instead of waiting for refinery loop")

	// List flags
	mqListCmd.Flags().Bool("ready", false, "Show only ready-to-merge (no blockers)")
	mqListCmd.Flags().String("status", "", "Filter by status (open, in_progress, closed)")
	mqListCmd.Flags().String("worker", "", "Filter by worker name")
	mqListCmd.Flags().String("epic", "", "Show MRs targeting integration/<epic>")
	mqListCmd.Flags().Bool("json", false, "Output as JSON")
	mqListCmd.Flags().Bool("verify", false, "Verify branches exist in git (shows MISSING for deleted branches)")

	// Reject flags
	mqRejectCmd.Flags().StringP("reason", "r", "", "Reason for rejection (required unless --stdin)")
	mqRejectCmd.Flags().Bool("notify", false, "Send mail notification to worker")
	mqRejectCmd.Flags().Bool("stdin", false, "Read reason from stdin (avoids shell quoting issues)")

	// Status flags
	mqStatusCmd.Flags().Bool("json", false, "Output as JSON")

	// Post-merge flags
	mqPostMergeCmd.Flags().Bool("skip-branch-delete", false, "Skip remote branch deletion")

	// Add subcommands
	mqCmd.AddCommand(mqSubmitCmd)
	mqCmd.AddCommand(mqRetryCmd)
	mqCmd.AddCommand(mqListCmd)
	mqCmd.AddCommand(mqRejectCmd)
	mqCmd.AddCommand(mqStatusCmd)
	mqCmd.AddCommand(mqPostMergeCmd)

	// Integration branch subcommands
	mqIntegrationCreateCmd.Flags().String("branch", "", "Override branch name template (supports {title}, {epic}, {prefix}, {user})")
	mqIntegrationCreateCmd.Flags().String("base-branch", "", "Create integration branch from this branch instead of main")
	mqIntegrationCreateCmd.Flags().Bool("force", false, "Recreate integration branch even if one already exists")
	mqIntegrationCmd.AddCommand(mqIntegrationCreateCmd)

	// Integration land flags
	mqIntegrationLandCmd.Flags().Bool("force", false, "Land even if some MRs still open")
	mqIntegrationLandCmd.Flags().Bool("skip-tests", false, "Skip test run")
	mqIntegrationLandCmd.Flags().Bool("dry-run", false, "Preview only, make no changes")
	mqIntegrationCmd.AddCommand(mqIntegrationLandCmd)

	// Integration status flags
	mqIntegrationStatusCmd.Flags().Bool("json", false, "Output as JSON")
	mqIntegrationCmd.AddCommand(mqIntegrationStatusCmd)

	mqCmd.AddCommand(mqIntegrationCmd)

	rootCmd.AddCommand(mqCmd)
}

// findCurrentRig determines the current rig from the working directory.
// Returns the rig name and rig object, or an error if not in a rig.
func findCurrentRig(townRoot string) (string, *rig.Rig, error) {
	rigName, err := currentRigName(townRoot)
	if err != nil {
		return "", nil, err
	}

	// Load rig manager and get the rig
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig := loadRigsConfig(rigsConfigPath)

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		return "", nil, fmt.Errorf("rig '%s' not found: %w", rigName, err)
	}

	return rigName, r, nil
}

func currentRigName(townRoot string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting current directory: %w", err)
	}
	relPath, err := filepath.Rel(townRoot, cwd)
	if err != nil {
		return "", fmt.Errorf("computing relative path: %w", err)
	}

	parts := strings.Split(relPath, string(filepath.Separator))
	if len(parts) > 0 && parts[0] != "" && parts[0] != "." {
		return parts[0], nil
	}
	if rigName := os.Getenv("GT_RIG"); rigName != "" {
		return rigName, nil
	}
	return "", fmt.Errorf("not inside a rig directory (and GT_RIG not set)")
}

func loadRigsConfig(path string) *config.RigsConfig {
	rigsConfig, err := config.LoadRigsConfig(path)
	if err != nil {
		return &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}
	return rigsConfig
}

func readMQBoolFlag(cmd *cobra.Command, name string) (bool, error) {
	if cmd == nil {
		return false, nil
	}
	value, err := cmd.Flags().GetBool(name)
	if err != nil {
		return false, fmt.Errorf("reading --%s: %w", name, err)
	}
	return value, nil
}

func readMQStringFlag(cmd *cobra.Command, name string) (string, error) {
	if cmd == nil {
		return "", nil
	}
	value, err := cmd.Flags().GetString(name)
	if err != nil {
		return "", fmt.Errorf("reading --%s: %w", name, err)
	}
	return value, nil
}

func runMQRetry(cmd *cobra.Command, args []string) error {
	now, err := readMQBoolFlag(cmd, "now")
	if err != nil {
		return err
	}
	rigName := args[0]
	mrID := args[1]

	mgr, _, _, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}

	// Get the MR first to show info
	mr, err := mgr.GetMR(mrID)
	if err != nil {
		if err == refinery.ErrMRNotFound {
			return fmt.Errorf("merge request '%s' not found in rig '%s'", mrID, rigName)
		}
		return fmt.Errorf("getting merge request: %w", err)
	}

	// Show what we're retrying
	fmt.Printf("Retrying merge request: %s\n", mrID)
	fmt.Printf("  Branch: %s\n", mr.Branch)
	fmt.Printf("  Worker: %s\n", mr.Worker)
	if mr.Error != "" {
		fmt.Printf("  Previous error: %s\n", style.Dim.Render(mr.Error))
	}

	// Perform the retry
	if err := mgr.Retry(mrID, now); err != nil {
		if err == refinery.ErrMRNotFailed {
			return fmt.Errorf("merge request '%s' has not failed (status: %s)", mrID, mr.Status)
		}
		return fmt.Errorf("retrying merge request: %w", err)
	}

	if now {
		fmt.Printf("%s Merge request processed\n", style.Bold.Render("✓"))
	} else {
		fmt.Printf("%s Merge request queued for retry\n", style.Bold.Render("✓"))
		fmt.Printf("  %s\n", style.Dim.Render("Will be processed on next refinery cycle"))
	}

	return nil
}

func runMQReject(cmd *cobra.Command, args []string) error {
	reason, notify, err := readMQRejectOptions(cmd)
	if err != nil {
		return err
	}

	rigName := args[0]
	mrIDOrBranch := args[1]

	mgr, _, _, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}

	result, err := mgr.RejectMR(mrIDOrBranch, reason, notify)
	if err != nil {
		return fmt.Errorf("rejecting MR: %w", err)
	}

	fmt.Printf("%s Rejected: %s\n", style.Bold.Render("✗"), result.Branch)
	fmt.Printf("  Worker: %s\n", result.Worker)
	fmt.Printf("  Reason: %s\n", reason)

	if result.IssueID != "" {
		fmt.Printf("  Issue:  %s %s\n", result.IssueID, style.Dim.Render("(not closed - work not done)"))
	}

	if notify {
		fmt.Printf("  %s\n", style.Dim.Render("Worker notified via mail"))
	}

	return nil
}

func readMQRejectOptions(cmd *cobra.Command) (string, bool, error) {
	reason, err := readMQStringFlag(cmd, "reason")
	if err != nil {
		return "", false, err
	}
	notify, err := readMQBoolFlag(cmd, "notify")
	if err != nil {
		return "", false, err
	}
	stdin, err := readMQBoolFlag(cmd, "stdin")
	if err != nil {
		return "", false, err
	}

	if stdin {
		if reason != "" {
			return "", false, fmt.Errorf("cannot use --stdin with --reason/-r")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", false, fmt.Errorf("reading stdin: %w", err)
		}
		reason = strings.TrimRight(string(data), "\n")
	}
	if reason == "" {
		return "", false, fmt.Errorf("required flag \"reason\" not set (use --reason/-r or --stdin)")
	}
	return reason, notify, nil
}

func runMQPostMerge(cmd *cobra.Command, args []string) error {
	rigName := args[0]
	mrID := args[1]

	mgr, r, _, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	rigGit, err := getRigGit(r.Path)
	if err != nil {
		return fmt.Errorf("post-merge proof: %w", err)
	}

	skipBranchDelete := false
	if cmd != nil {
		skipBranchDelete, err = cmd.Flags().GetBool("skip-branch-delete")
		if err != nil {
			return fmt.Errorf("reading --skip-branch-delete: %w", err)
		}
	}
	result, branchCleanup, err := runVerifiedMQPostMerge(mgr, r.Path, rigGit, mrID, skipBranchDelete)
	if err != nil {
		return fmt.Errorf("post-merge cleanup: %w", err)
	}

	return printMQPostMergeResult(result, branchCleanup)
}

func printMQPostMergeResult(result *refinery.PostMergeResult, branchCleanup mqPostMergeBranchCleanup) error {
	mr := result.MR
	fmt.Printf("%s Post-merge: %s\n", style.Bold.Render("✓"), mr.ID)
	fmt.Printf("  Branch: %s\n", mr.Branch)
	fmt.Printf("  Worker: %s\n", mr.Worker)

	if result.MRClosed {
		fmt.Printf("  %s MR closed (merged)\n", style.Success.Render("✓"))
	}
	if result.SourceIssueClosed {
		fmt.Printf("  %s Source issue closed: %s\n", style.Success.Render("✓"), result.SourceIssueID)
	} else if result.SourceIssueNotFound {
		fmt.Printf("  %s Source issue: %s %s\n", style.Dim.Render("○"), result.SourceIssueID, style.Dim.Render("(already closed or not found)"))
	}

	printMQPostMergeBranchCleanup(mr.Branch, branchCleanup)
	return nil
}

func printMQPostMergeBranchCleanup(branch string, branchCleanup mqPostMergeBranchCleanup) {
	if branchCleanup.NoBranch {
		fmt.Printf("  %s No branch name in MR (skipping branch delete)\n", style.Dim.Render("○"))
	} else if branchCleanup.Skipped {
		fmt.Printf("  %s Branch delete skipped (--skip-branch-delete)\n", style.Dim.Render("○"))
	} else if branchCleanup.Disabled {
		fmt.Printf("  %s Branch delete disabled by config\n", style.Dim.Render("○"))
	} else if branchCleanup.OpenPR {
		fmt.Printf("  %s Skipping remote branch delete for %s: open PR exists (gas-fk4)\n", style.Dim.Render("○"), branch)
	} else if branchCleanup.AlreadyGone {
		fmt.Printf("  %s Remote branch already absent: %s\n", style.Dim.Render("○"), branch)
	} else if branchCleanup.RemoteDeleted {
		fmt.Printf("  %s Deleted remote branch: %s\n", style.Success.Render("✓"), branch)
	}

	if branchCleanup.LocalDeleted {
		fmt.Printf("  %s Deleted local branch: %s\n", style.Success.Render("✓"), branch)
	}
}

func runVerifiedMQPostMerge(mgr mqPostMergeManager, rigPath string, rigGit mqPostMergeGit, mrID string, skipBranchDelete bool) (*refinery.PostMergeResult, mqPostMergeBranchCleanup, error) {
	mr, err := mgr.FindMRForPostMerge(mrID)
	if err != nil {
		return nil, mqPostMergeBranchCleanup{}, err
	}
	if err := verifyMQPostMergeProof(rigGit, mr); err != nil {
		return nil, mqPostMergeBranchCleanup{}, err
	}

	result, err := mgr.PostMergeMR(mr)
	if err != nil {
		return result, mqPostMergeBranchCleanup{}, err
	}

	branchCleanup, err := cleanupMQPostMergeBranch(rigPath, rigGit, result.MR, skipBranchDelete)
	return result, branchCleanup, err
}

func verifyMQPostMergeProof(rigGit mqPostMergeGit, mr *refinery.MergeRequest) error {
	if mr == nil {
		return fmt.Errorf("merge proof failed: merge request is missing")
	}
	target := strings.TrimSpace(mr.TargetBranch)
	if target == "" {
		return fmt.Errorf("merge proof failed for MR %s: missing target branch", mr.ID)
	}
	if source := strings.TrimSpace(mr.Branch); source != "" && source == target {
		return fmt.Errorf("merge proof failed for MR %s: source branch %s matches target branch", mr.ID, source)
	}
	commit := strings.TrimSpace(mr.CommitSHA)
	if commit == "" {
		return fmt.Errorf("merge proof failed for MR %s: missing submitted commit_sha", mr.ID)
	}
	if err := rigGit.VerifyPushedCommitReachableFromPushTarget("origin", target, commit); err != nil {
		return fmt.Errorf("merge proof failed for MR %s: target %s does not contain submitted head %s: %w", mr.ID, target, commit, err)
	}
	return nil
}

func cleanupMQPostMergeBranch(rigPath string, rigGit mqPostMergeGit, mr *refinery.MergeRequest, skipBranchDelete bool) (mqPostMergeBranchCleanup, error) {
	cleanup := mqPostMergeBranchCleanup{}
	if mr == nil {
		return cleanup, fmt.Errorf("remote branch delete: merge request is missing")
	}

	cleanup.Branch = strings.TrimSpace(mr.Branch)
	if cleanup.Branch == "" {
		cleanup.NoBranch = true
		return cleanup, nil
	}
	if skipBranchDelete {
		cleanup.Skipped = true
		return cleanup, nil
	}
	if !mqDeleteMergedBranchesEnabled(rigPath) {
		cleanup.Disabled = true
		return cleanup, nil
	}

	expectedHead := strings.TrimSpace(mr.CommitSHA)
	if expectedHead == "" {
		return cleanup, fmt.Errorf("remote branch delete %s: missing submitted commit_sha", cleanup.Branch)
	}

	if err := applyMQPostMergeRemoteCleanup(rigGit, mr, &cleanup, expectedHead); err != nil {
		return cleanup, err
	}

	if deleteMQPostMergeLocalBranchIfAt(rigGit, cleanup.Branch, expectedHead) {
		cleanup.LocalDeleted = true
	}
	return cleanup, nil
}

func applyMQPostMergeRemoteCleanup(rigGit mqPostMergeGit, mr *refinery.MergeRequest, cleanup *mqPostMergeBranchCleanup, expectedHead string) error {
	// Deleting a branch with an open PR causes GitHub to auto-close the PR as
	// "closed" (not "merged"), destroying the PR audit trail. (gas-fk4)
	if rigGit.HasOpenPullRequest(git.PullRequestRef{URL: mr.PRURL, Number: mr.PRNumber, Branch: cleanup.Branch, HeadSHA: expectedHead}) {
		cleanup.OpenPR = true
		return nil
	}

	remoteTip, err := rigGit.PushRemoteBranchTip("origin", cleanup.Branch)
	if err != nil {
		return fmt.Errorf("remote branch delete %s: read remote branch tip: %w", cleanup.Branch, err)
	}
	if strings.TrimSpace(remoteTip) == "" {
		cleanup.AlreadyGone = true
		return nil
	}
	if err := rigGit.DeleteRemoteBranchIfAt("origin", cleanup.Branch, expectedHead); err != nil {
		return fmt.Errorf("remote branch delete %s at %s: %w", cleanup.Branch, expectedHead, err)
	}
	cleanup.RemoteDeleted = true
	return nil
}

func deleteMQPostMergeLocalBranchIfAt(rigGit mqPostMergeGit, branch, expectedHead string) bool {
	localHead, err := rigGit.Rev("refs/heads/" + branch + "^{commit}")
	if err != nil || strings.TrimSpace(localHead) != strings.TrimSpace(expectedHead) {
		return false
	}
	return rigGit.DeleteBranch(branch, false) == nil
}

func mqDeleteMergedBranchesEnabled(rigPath string) bool {
	settingsPath := filepath.Join(rigPath, "settings", "config.json")
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil || settings.MergeQueue == nil {
		return true
	}
	return config.IsDeleteMergedBranchesEnabled(settings.MergeQueue)
}
