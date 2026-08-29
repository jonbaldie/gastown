package cmd

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jonbaldie/gastown/internal/constants"
	gitpkg "github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/util"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var orphansCmd = &cobra.Command{
	Use:     "orphans",
	GroupID: GroupWork,
	Short:   "Find lost polecat work",
	Long: `Find orphaned commits and unmerged polecat branches.

Polecat work can get lost when:
- Session killed before merge
- Refinery fails to process
- Network issues during push

This command scans for:
1. Orphaned commits via 'git fsck --unreachable' (filtered by --days/--all)
2. Unmerged polecat worktree branches (always shown)

Note: --days and --all only apply to orphaned commits, not polecat branches.

Examples:
  gt orphans              # Last 7 days (default), infers rig from cwd
  gt orphans --rig=gastown # Target a specific rig
  gt orphans --days=14    # Last 2 weeks
  gt orphans --all        # Show all orphans (no date filter)`,
	RunE: runOrphans,
}

// Commit orphan kill command
var orphansKillCmd = &cobra.Command{
	Use:   "kill",
	Short: "Remove all orphans (commits and processes)",
	Long: `Remove orphaned commits and kill orphaned Claude processes.

This command performs a complete orphan cleanup:
1. Finds and removes orphaned commits via 'git gc --prune=now'
2. Finds and kills orphaned Claude processes (PPID=1)

WARNING: This operation is irreversible. Once commits are pruned,
they cannot be recovered.

Note: This only affects orphaned commits and processes. Unmerged polecat
branches (shown by 'gt orphans') must be recovered or cleaned up manually.

The command will:
1. Find orphaned commits (same as 'gt orphans')
2. Find orphaned Claude processes (same as 'gt orphans procs')
3. Show what will be removed/killed
4. Ask for confirmation (unless --force)
5. Run git gc and kill processes

Examples:
  gt orphans kill              # Kill orphans from last 7 days (default)
  gt orphans kill --days=14    # Kill orphans from last 2 weeks
  gt orphans kill --all        # Kill all orphans
  gt orphans kill --dry-run    # Preview without deleting
  gt orphans kill --force      # Skip confirmation prompt`,
	RunE: runOrphansKill,
}

// Process orphan commands
var orphansProcsCmd = &cobra.Command{
	Use:   "procs",
	Short: "Manage orphaned Claude processes",
	Long: `Find and kill Claude processes that have become orphaned (PPID=1).

These are processes that survived session termination and are now
parented to init/launchd. They consume resources and should be killed.

Use --aggressive to detect ALL orphaned Claude processes by cross-referencing
against active tmux sessions. Any Claude process NOT in a gt-* or hq-* session
is considered an orphan. This catches processes that have been reparented to
something other than init (PPID != 1).

Examples:
  gt orphans procs              # List orphaned Claude processes (PPID=1 only)
  gt orphans procs list         # Same as above
  gt orphans procs --aggressive # List ALL orphaned processes (tmux verification)
  gt orphans procs kill         # Kill orphaned processes`,
	RunE: runOrphansListProcesses, // Default to list
}

var orphansProcsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List orphaned Claude processes",
	Long: `List Claude processes that have become orphaned (PPID=1).

These are processes that survived session termination and are now
parented to init/launchd. They consume resources and should be killed.

Use --aggressive to detect ALL orphaned Claude processes by cross-referencing
against active tmux sessions. Any Claude process NOT in a gt-* or hq-* session
is considered an orphan.

Excludes:
- tmux server processes
- Claude.app desktop application processes

Examples:
  gt orphans procs list             # Show orphans with PPID=1
  gt orphans procs list --aggressive # Show ALL orphans (tmux verification)`,
	RunE: runOrphansListProcesses,
}

var orphansProcsKillCmd = &cobra.Command{
	Use:   "kill",
	Short: "Kill orphaned Claude processes",
	Long: `Kill Claude processes that have become orphaned (PPID=1).

Without flags, prompts for confirmation before killing.
Use -f/--force to kill without confirmation.
Use --aggressive to kill ALL orphaned processes (not just PPID=1).

Examples:
  gt orphans procs kill             # Kill with confirmation
  gt orphans procs kill -f          # Force kill without confirmation
  gt orphans procs kill --aggressive # Kill ALL orphans (tmux verification)`,
	RunE: runOrphansKillProcesses,
}

func init() {
	orphansCmd.Flags().Int("days", 7, "Show orphans from last N days")
	orphansCmd.Flags().Bool("all", false, "Show all orphans (no date filter)")
	orphansCmd.PersistentFlags().String("rig", "", "Target rig name (required when not in a rig directory)")

	// Kill commits command flags
	orphansKillCmd.Flags().Bool("dry-run", false, "Preview without deleting")
	orphansKillCmd.Flags().Int("days", 7, "Kill orphans from last N days")
	orphansKillCmd.Flags().Bool("all", false, "Kill all orphans (no date filter)")
	orphansKillCmd.Flags().Bool("force", false, "Skip confirmation prompt")

	// Process orphan kill command flags
	orphansProcsKillCmd.Flags().BoolP("force", "f", false, "Kill without confirmation")

	// Aggressive flag for all procs commands (persistent so it applies to subcommands)
	orphansProcsCmd.PersistentFlags().Bool("aggressive", false, "Use tmux session verification to find ALL orphans (not just PPID=1)")

	// Wire up subcommands
	orphansProcsCmd.AddCommand(orphansProcsListCmd)
	orphansProcsCmd.AddCommand(orphansProcsKillCmd)

	orphansCmd.AddCommand(orphansKillCmd)
	orphansCmd.AddCommand(orphansProcsCmd)

	rootCmd.AddCommand(orphansCmd)
}

type orphansOptions struct {
	days int
	all  bool
	rig  string
}

type orphansKillOptions struct {
	days   int
	all    bool
	dryRun bool
	force  bool
	rig    string
}

type orphanProcessesOptions struct {
	force      bool
	aggressive bool
}

func orphansOptionsFromCommand(cmd *cobra.Command) orphansOptions {
	return orphansOptions{
		days: commandIntFlag(cmd, "days"),
		all:  commandBoolFlag(cmd, "all"),
		rig:  commandStringFlag(cmd, "rig"),
	}
}

func orphansKillOptionsFromCommand(cmd *cobra.Command) orphansKillOptions {
	return orphansKillOptions{
		days:   commandIntFlag(cmd, "days"),
		all:    commandBoolFlag(cmd, "all"),
		dryRun: commandBoolFlag(cmd, "dry-run"),
		force:  commandBoolFlag(cmd, "force"),
		rig:    commandStringFlag(cmd, "rig"),
	}
}

func orphanProcessesOptionsFromCommand(cmd *cobra.Command) orphanProcessesOptions {
	return orphanProcessesOptions{
		force:      commandBoolFlag(cmd, "force"),
		aggressive: commandBoolFlag(cmd, "aggressive"),
	}
}

// OrphanCommit represents an unreachable commit
type OrphanCommit struct {
	SHA     string
	Date    time.Time
	Author  string
	Subject string
}

type orphansScan struct {
	opts          orphansOptions
	rigName       string
	r             *rig.Rig
	mayorPath     string
	commitOrphans []OrphanCommit
	filtered      []OrphanCommit
}

func runOrphans(cmd *cobra.Command, _ []string) error {
	scan, err := beginOrphansScan(orphansOptionsFromCommand(cmd))
	if err != nil {
		return err
	}
	found, err := reportOrphanCommits(scan)
	if err != nil {
		return err
	}
	foundBranches, skipped := reportOrphanBranches(scan)
	printSkippedPolecats(skipped)
	printOrphansEmpty(scan, found || foundBranches)
	return nil
}

func beginOrphansScan(opts orphansOptions) (*orphansScan, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigName, r, err := resolveOrphansRig(opts.rig, townRoot)
	if err != nil {
		return nil, err
	}
	return &orphansScan{
		opts:      opts,
		rigName:   rigName,
		r:         r,
		mayorPath: r.Path + "/mayor/rig",
	}, nil
}

func resolveOrphansRig(rigName, townRoot string) (string, *rig.Rig, error) {
	if rigName != "" {
		_, r, err := getRig(rigName)
		if err != nil {
			return "", nil, err
		}
		return rigName, r, nil
	}
	name, r, err := findCurrentRig(townRoot)
	if err != nil {
		return "", nil, fmt.Errorf("not in a rig directory. Use --rig <name> to specify the target rig, or run from within a rig directory")
	}
	return name, r, nil
}

func filterOrphanCommits(orphans []OrphanCommit, days int, all bool) []OrphanCommit {
	cutoff := time.Now().AddDate(0, 0, -days)
	var filtered []OrphanCommit
	for _, o := range orphans {
		if all || o.Date.After(cutoff) {
			filtered = append(filtered, o)
		}
	}
	return filtered
}

func reportOrphanCommits(scan *orphansScan) (bool, error) {
	fmt.Printf("Scanning for orphaned commits in %s...\n\n", scan.rigName)
	orphans, err := findOrphanCommits(scan.mayorPath)
	if err != nil {
		return false, fmt.Errorf("finding orphans: %w", err)
	}
	scan.commitOrphans = orphans
	scan.filtered = filterOrphanCommits(orphans, scan.opts.days, scan.opts.all)
	if len(scan.filtered) == 0 {
		return false, nil
	}
	printOrphanCommitList(scan.filtered)
	return true, nil
}

func printOrphanCommitList(filtered []OrphanCommit) {
	fmt.Printf("%s Found %d orphaned commit(s):\n\n", style.Warning.Render("⚠"), len(filtered))
	for _, o := range filtered {
		fmt.Printf("  %s %s\n", style.Bold.Render(shortHash(o.SHA)), o.Subject)
		fmt.Printf("    %s by %s\n\n", style.Dim.Render(formatAge(o.Date)), o.Author)
	}
	fmt.Printf("%s\n", style.Dim.Render("To recover a commit:"))
	fmt.Printf("%s\n", style.Dim.Render("  git cherry-pick <sha>     # Apply to current branch"))
	fmt.Printf("%s\n", style.Dim.Render("  git show <sha>            # View full commit"))
	fmt.Printf("%s\n\n", style.Dim.Render("  git branch rescue <sha>   # Create branch from commit"))
}

func reportOrphanBranches(scan *orphansScan) (bool, []skippedPolecat) {
	defaultBranch := scan.r.DefaultBranch()
	fmt.Printf("Scanning polecat worktrees for unmerged branches...\n\n")
	polecatBranches, skipped, err := findOrphanPolecatBranches(scan.r.Path, scan.rigName, defaultBranch)
	if err != nil {
		fmt.Printf("%s Could not scan polecat worktrees: %v\n\n", style.Dim.Render("ℹ"), err)
		return false, skipped
	}
	if len(polecatBranches) == 0 {
		return false, skipped
	}
	printOrphanBranchList(polecatBranches, defaultBranch)
	return true, skipped
}

func printOrphanBranchList(polecatBranches []OrphanBranch, defaultBranch string) {
	fmt.Printf("%s Found %d unmerged polecat branch(es):\n\n", style.Warning.Render("⚠"), len(polecatBranches))
	for _, b := range polecatBranches {
		fmt.Printf("  %s %s (%d commit(s) ahead of %s)\n",
			style.Bold.Render(b.Polecat), b.Branch, b.AheadCount, defaultBranch)
		if b.LatestSubject != "" {
			fmt.Printf("    %s %s\n", style.Dim.Render("latest:"), b.LatestSubject)
		}
		if b.HasUncommitted {
			fmt.Printf("    %s\n", style.Warning.Render("has uncommitted changes"))
		}
		fmt.Printf("    %s %s\n", style.Dim.Render("path:"), b.WorktreePath)
		fmt.Println()
	}
	fmt.Printf("%s\n", style.Dim.Render("To recover polecat work:"))
	fmt.Printf("  %s\n", style.Dim.Render("cd <path>                   # Enter the worktree (see path above)"))
	fmt.Printf("  %s\n\n", style.Dim.Render(fmt.Sprintf("git log %s..HEAD        # View unmerged commits", defaultBranch)))
}

func printSkippedPolecats(skipped []skippedPolecat) {
	if len(skipped) == 0 {
		return
	}
	fmt.Printf("%s Skipped %d polecat(s) due to errors:\n", style.Warning.Render("⚠"), len(skipped))
	for _, s := range skipped {
		fmt.Printf("  %s: %s\n", s.Polecat, s.Err)
	}
	fmt.Println()
}

func printOrphansEmpty(scan *orphansScan, foundAnything bool) {
	if foundAnything {
		return
	}
	if len(scan.commitOrphans) > 0 && len(scan.filtered) == 0 {
		fmt.Printf("%s No orphaned commits in the last %d days\n", style.Bold.Render("✓"), scan.opts.days)
		fmt.Printf("%s Use --days=N or --all to see older orphans\n", style.Dim.Render("Hint:"))
		return
	}
	fmt.Printf("%s No orphaned work found\n", style.Bold.Render("✓"))
}

// OrphanBranch represents a polecat worktree with unmerged work.
type OrphanBranch struct {
	Polecat        string // Polecat name
	Branch         string // Branch name
	AheadCount     int    // Commits ahead of default branch
	LatestSubject  string // Subject of the latest commit
	HasUncommitted bool   // Whether the worktree has uncommitted changes
	WorktreePath   string // Actual resolved worktree path
}

// skippedPolecat records a polecat that was skipped due to errors during scanning.
type skippedPolecat struct {
	Polecat string
	Err     string
}

// resolvePolecatWorktree determines the worktree path for a polecat,
// mirroring the canonical clonePath logic in polecat/session_manager.go.
func resolvePolecatWorktree(polecatsDir, polecatName, rigName string) string {
	// New structure: polecats/<name>/<rigname>/
	newPath := filepath.Join(polecatsDir, polecatName, rigName)
	if info, err := os.Stat(newPath); err == nil && info.IsDir() {
		return newPath
	}

	// Old structure: polecats/<name>/ (backward compat)
	oldPath := filepath.Join(polecatsDir, polecatName)
	if info, err := os.Stat(oldPath); err == nil && info.IsDir() {
		gitPath := filepath.Join(oldPath, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			return oldPath
		}
	}

	return "" // No valid worktree found
}

// findOrphanPolecatBranches scans polecat worktrees for branches with
// commits that have not been merged to the default branch.
func findOrphanPolecatBranches(rigPath, rigName, defaultBranch string) ([]OrphanBranch, []skippedPolecat, error) {
	polecatsDir := filepath.Join(rigPath, constants.DirPolecats)
	entries, err := os.ReadDir(polecatsDir)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading polecats dir: %w", err)
	}
	var branches []OrphanBranch
	var skipped []skippedPolecat
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		branch, skip, ok := inspectPolecatWorktree(polecatsDir, entry.Name(), rigName, defaultBranch)
		if skip != nil {
			skipped = append(skipped, *skip)
			continue
		}
		if ok {
			branches = append(branches, branch)
		}
	}
	return branches, skipped, nil
}

func inspectPolecatWorktree(polecatsDir, polecatName, rigName, defaultBranch string) (OrphanBranch, *skippedPolecat, bool) {
	worktreePath := resolvePolecatWorktree(polecatsDir, polecatName, rigName)
	if worktreePath == "" {
		return OrphanBranch{}, nil, false
	}
	branch, skip := polecatUnmergedBranch(worktreePath, polecatName, defaultBranch)
	if skip != nil || branch == "" {
		return OrphanBranch{}, skip, false
	}
	count, skip := polecatAheadCount(worktreePath, polecatName, defaultBranch)
	if skip != nil || count == 0 {
		return OrphanBranch{}, skip, false
	}
	subject, uncommitted, skip := polecatWorktreeState(worktreePath, polecatName)
	if skip != nil {
		return OrphanBranch{}, skip, false
	}
	return OrphanBranch{
		Polecat:        polecatName,
		Branch:         branch,
		AheadCount:     count,
		LatestSubject:  subject,
		HasUncommitted: uncommitted,
		WorktreePath:   worktreePath,
	}, nil, true
}

func polecatUnmergedBranch(worktreePath, polecatName, defaultBranch string) (string, *skippedPolecat) {
	branch, err := gitpkg.NewGit(worktreePath).CurrentBranch()
	if err != nil {
		return "", &skippedPolecat{polecatName, fmt.Sprintf("cannot determine branch: %v", err)}
	}
	branch = strings.TrimSpace(branch)
	if branch == "" || branch == "HEAD" || branch == defaultBranch {
		return "", nil
	}
	return branch, nil
}

func polecatAheadCount(worktreePath, polecatName, defaultBranch string) (int, *skippedPolecat) {
	countOut, err := exec.Command("git", "-C", worktreePath, "rev-list", "--count", defaultBranch+"..HEAD").Output()
	if err != nil {
		countOut, err = exec.Command("git", "-C", worktreePath, "rev-list", "--count", "origin/"+defaultBranch+"..HEAD").Output()
		if err != nil {
			return 0, &skippedPolecat{polecatName, fmt.Sprintf("rev-list failed: %v", err)}
		}
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(countOut)))
	if err != nil {
		return 0, nil
	}
	return count, nil
}

func polecatWorktreeState(worktreePath, polecatName string) (string, bool, *skippedPolecat) {
	logOut, err := exec.Command("git", "-C", worktreePath, "log", "-1", "--format=%s").Output()
	if err != nil {
		return "", false, &skippedPolecat{polecatName, fmt.Sprintf("git log failed: %v", err)}
	}
	gitStatus, err := gitpkg.NewGit(worktreePath).Status()
	if err != nil {
		return "", false, &skippedPolecat{polecatName, fmt.Sprintf("git status failed: %v", err)}
	}
	return strings.TrimSpace(string(logOut)), !gitStatus.Clean, nil
}

// findOrphanCommits runs git fsck and parses orphaned commits
func findOrphanCommits(repoPath string) ([]OrphanCommit, error) {
	shas, err := unreachableCommitSHAs(repoPath)
	if err != nil || len(shas) == 0 {
		return nil, err
	}
	return collectOrphanCommitDetails(repoPath, shas), nil
}

func unreachableCommitSHAs(repoPath string) ([]string, error) {
	fsckCmd := exec.Command("git", "fsck", "--unreachable", "--no-reflogs")
	fsckCmd.Dir = repoPath
	var fsckOut, fsckErr bytes.Buffer
	fsckCmd.Stdout = &fsckOut
	fsckCmd.Stderr = &fsckErr
	if err := fsckCmd.Run(); err != nil && fsckOut.Len() == 0 {
		return nil, fsckFailure(err, fsckErr.String())
	}
	return parseUnreachableCommitSHAs(&fsckOut), nil
}

func fsckFailure(err error, stderr string) error {
	errMsg := strings.TrimSpace(stderr)
	if errMsg != "" {
		return fmt.Errorf("git fsck failed: %w (%s)", err, errMsg)
	}
	return fmt.Errorf("git fsck failed: %w", err)
}

func parseUnreachableCommitSHAs(fsckOut *bytes.Buffer) []string {
	var commitSHAs []string
	scanner := bufio.NewScanner(fsckOut)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "unreachable commit ") {
			commitSHAs = append(commitSHAs, strings.TrimPrefix(line, "unreachable commit "))
		}
	}
	return commitSHAs
}

func collectOrphanCommitDetails(repoPath string, commitSHAs []string) []OrphanCommit {
	var orphans []OrphanCommit
	for _, sha := range commitSHAs {
		commit, err := getCommitDetails(repoPath, sha)
		if err != nil || isNoiseCommit(commit.Subject) {
			continue
		}
		orphans = append(orphans, commit)
	}
	return orphans
}

// getCommitDetails retrieves commit metadata
func getCommitDetails(repoPath, sha string) (OrphanCommit, error) {
	// Format: timestamp|author|subject
	cmd := exec.Command("git", "log", "-1", "--format=%at|%an|%s", sha)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return OrphanCommit{}, err
	}

	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
	if len(parts) < 3 {
		return OrphanCommit{}, fmt.Errorf("unexpected format")
	}

	timestamp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return OrphanCommit{}, err
	}

	return OrphanCommit{
		SHA:     sha,
		Date:    time.Unix(timestamp, 0),
		Author:  parts[1],
		Subject: parts[2],
	}, nil
}

// isNoiseCommit returns true for stash-related or routine sync commits
func isNoiseCommit(subject string) bool {
	// Git stash creates commits with these prefixes
	noisePrefixes := []string{
		"WIP on ",
		"index on ",
		"On ",              // "On branch: message"
		"stash@{",          // Direct stash reference
		"untracked files ", // Stash with untracked
	}

	for _, prefix := range noisePrefixes {
		if strings.HasPrefix(subject, prefix) {
			return true
		}
	}

	return false
}

// formatAge returns a human-readable age string
func formatAge(t time.Time) string {
	d := time.Since(t)

	if d < time.Hour {
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", days)
}

// runOrphansKill removes orphaned commits and kills orphaned processes
type orphansKillRun struct {
	opts            orphansKillOptions
	rigName         string
	mayorPath       string
	commitOrphans   []OrphanCommit
	filteredCommits []OrphanCommit
	procOrphans     []OrphanProcess
}

func runOrphansKill(cmd *cobra.Command, _ []string) error {
	k, err := beginOrphansKill(cmd)
	if err != nil {
		return err
	}
	if err := loadOrphansKillTargets(k); err != nil {
		return err
	}
	if len(k.filteredCommits) == 0 && len(k.procOrphans) == 0 {
		fmt.Printf("%s No orphans found\n", style.Bold.Render("✓"))
		return nil
	}
	printOrphansKillPlan(k)
	if k.opts.dryRun {
		fmt.Printf("%s Dry run - no changes made\n", style.Dim.Render("ℹ"))
		return nil
	}
	if !confirmOrphansKill(k) {
		return nil
	}
	return executeOrphansKill(k)
}

func beginOrphansKill(cmd *cobra.Command) (*orphansKillRun, error) {
	opts := orphansKillOptionsFromCommand(cmd)
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigName, r, err := resolveOrphansRig(opts.rig, townRoot)
	if err != nil {
		return nil, err
	}
	return &orphansKillRun{
		opts:      opts,
		rigName:   rigName,
		mayorPath: r.Path + "/mayor/rig",
	}, nil
}

func loadOrphansKillTargets(k *orphansKillRun) error {
	fmt.Printf("Scanning for orphaned commits in %s...\n", k.rigName)
	commitOrphans, err := findOrphanCommits(k.mayorPath)
	if err != nil {
		return fmt.Errorf("finding orphan commits: %w", err)
	}
	k.commitOrphans = commitOrphans
	k.filteredCommits = filterOrphanCommits(commitOrphans, k.opts.days, k.opts.all)
	fmt.Printf("Scanning for orphaned Claude processes...\n\n")
	procOrphans, err := findOrphanProcesses()
	if err != nil {
		return fmt.Errorf("finding orphan processes: %w", err)
	}
	k.procOrphans = procOrphans
	return nil
}

func printOrphansKillPlan(k *orphansKillRun) {
	if len(k.filteredCommits) > 0 {
		fmt.Printf("%s Found %d orphaned commit(s) to remove:\n\n", style.Warning.Render("⚠"), len(k.filteredCommits))
		for _, o := range k.filteredCommits {
			fmt.Printf("  %s %s\n", style.Bold.Render(shortHash(o.SHA)), o.Subject)
			fmt.Printf("    %s by %s\n\n", style.Dim.Render(formatAge(o.Date)), o.Author)
		}
	} else if len(k.commitOrphans) > 0 {
		fmt.Printf("%s No orphaned commits in the last %d days (use --days=N or --all)\n\n",
			style.Dim.Render("ℹ"), k.opts.days)
	}
	if len(k.procOrphans) == 0 {
		return
	}
	fmt.Printf("%s Found %d orphaned Claude process(es) to kill:\n\n", style.Warning.Render("⚠"), len(k.procOrphans))
	for _, o := range k.procOrphans {
		fmt.Printf("  %s %s\n", style.Bold.Render(fmt.Sprintf("PID %d", o.PID)), truncateOrphanArgs(o.Args))
	}
	fmt.Println()
}

func truncateOrphanArgs(args string) string {
	if len(args) > 80 {
		return args[:77] + "..."
	}
	return args
}

func confirmOrphansKill(k *orphansKillRun) bool {
	if k.opts.force {
		return true
	}
	fmt.Printf("%s\n", style.Warning.Render("WARNING: This operation is irreversible!"))
	fmt.Printf("Remove %d orphan(s)? [y/N] ", len(k.filteredCommits)+len(k.procOrphans))
	var response string
	_, _ = fmt.Scanln(&response)
	if strings.ToLower(strings.TrimSpace(response)) != "y" {
		fmt.Printf("%s Canceled\n", style.Dim.Render("ℹ"))
		return false
	}
	return true
}

func executeOrphansKill(k *orphansKillRun) error {
	if err := pruneOrphanCommits(k); err != nil {
		return err
	}
	killOrphanProcessList(k.procOrphans, k.opts.force)
	fmt.Printf("\n%s Orphan cleanup complete\n", style.Bold.Render("✓"))
	return nil
}

func pruneOrphanCommits(k *orphansKillRun) error {
	if len(k.filteredCommits) == 0 {
		return nil
	}
	fmt.Printf("\nRunning git gc --prune=now...\n")
	gcCmd := exec.Command("git", "gc", "--prune=now")
	gcCmd.Dir = k.mayorPath
	gcCmd.Stdout = os.Stdout
	gcCmd.Stderr = os.Stderr
	if err := gcCmd.Run(); err != nil {
		return fmt.Errorf("git gc failed: %w", err)
	}
	fmt.Printf("%s Removed %d orphaned commit(s)\n", style.Bold.Render("✓"), len(k.filteredCommits))
	return nil
}

func killOrphanProcessList(orphans []OrphanProcess, force bool) {
	if len(orphans) == 0 {
		return
	}
	fmt.Printf("\nKilling orphaned processes...\n")
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	killed, failed := signalOrphanProcesses(orphans, signal)
	fmt.Printf("%s %d process(es) killed", style.Bold.Render("✓"), killed)
	if failed > 0 {
		fmt.Printf(", %d failed", failed)
	}
	fmt.Println()
}

func signalOrphanProcesses(orphans []OrphanProcess, signal os.Signal) (int, int) {
	var killed, failed int
	for _, o := range orphans {
		switch signalOrphanPID(o.PID, signal) {
		case orphanSignalKilled:
			killed++
		case orphanSignalFailed:
			failed++
		}
	}
	return killed, failed
}

type orphanSignalResult int

const (
	orphanSignalKilled orphanSignalResult = iota
	orphanSignalFailed
	orphanSignalGone
)

func signalOrphanPID(pid int, signal os.Signal) orphanSignalResult {
	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Printf("  %s PID %d: %v\n", style.Error.Render("✗"), pid, err)
		return orphanSignalFailed
	}
	if err := proc.Signal(signal); err != nil {
		if err == os.ErrProcessDone {
			fmt.Printf("  %s PID %d: already terminated\n", style.Dim.Render("○"), pid)
			return orphanSignalGone
		}
		fmt.Printf("  %s PID %d: %v\n", style.Error.Render("✗"), pid, err)
		return orphanSignalFailed
	}
	fmt.Printf("  %s PID %d killed\n", style.Bold.Render("✓"), pid)
	return orphanSignalKilled
}

// OrphanProcess represents a Claude process that has become orphaned (PPID=1)
type OrphanProcess struct {
	PID  int
	Args string
}

// findOrphanProcesses finds Claude processes with PPID=1 (orphaned)
func findOrphanProcesses() ([]OrphanProcess, error) {
	out, err := exec.Command("ps", "-eo", "pid,ppid,args").Output()
	if err != nil {
		return nil, fmt.Errorf("running ps: %w", err)
	}
	var orphans []OrphanProcess
	scanner := bufio.NewScanner(bytes.NewReader(out))
	if scanner.Scan() {
		// First line is header, skip it
	}
	for scanner.Scan() {
		proc, ok := parseOrphanProcessLine(scanner.Text())
		if ok {
			orphans = append(orphans, proc)
		}
	}
	return orphans, nil
}

func parseOrphanProcessLine(line string) (OrphanProcess, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return OrphanProcess{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return OrphanProcess{}, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid != 1 {
		return OrphanProcess{}, false
	}
	args := strings.Join(fields[2:], " ")
	if !isClaudeProcess(args) || isExcludedProcess(args) {
		return OrphanProcess{}, false
	}
	return OrphanProcess{PID: pid, Args: args}, true
}

// isClaudeProcess checks if the process is claude-related
func isClaudeProcess(args string) bool {
	argsLower := strings.ToLower(args)
	return strings.Contains(argsLower, "claude")
}

// isExcludedProcess checks if the process should be excluded from orphan list
func isExcludedProcess(args string) bool {
	// Exclude any tmux process (server, new-session, etc.)
	// These may contain "claude" in args but are tmux processes, not actual Claude processes
	if strings.HasPrefix(args, "tmux ") || strings.HasPrefix(args, "/usr/bin/tmux") {
		return true
	}

	// Exclude Claude.app desktop application processes
	if strings.Contains(args, "Claude.app") || strings.Contains(args, "/Applications/Claude") {
		return true
	}

	// Exclude Claude Helper processes (part of Claude.app)
	if strings.Contains(args, "Claude Helper") {
		return true
	}

	return false
}

// runOrphansListProcesses lists orphaned Claude processes
func runOrphansListProcesses(cmd *cobra.Command, _ []string) error {
	opts := orphanProcessesOptionsFromCommand(cmd)
	if opts.aggressive {
		return runOrphansListProcessesAggressive()
	}

	orphans, err := findOrphanProcesses()
	if err != nil {
		return fmt.Errorf("finding orphan processes: %w", err)
	}

	if len(orphans) == 0 {
		fmt.Printf("%s No orphaned Claude processes found (PPID=1)\n", style.Bold.Render("✓"))
		fmt.Printf("%s Use --aggressive to find orphans via tmux session verification\n", style.Dim.Render("Hint:"))
		return nil
	}

	fmt.Printf("%s Found %d orphaned Claude process(es) with PPID=1:\n\n", style.Warning.Render("⚠"), len(orphans))

	for _, o := range orphans {
		// Truncate args for display
		displayArgs := o.Args
		if len(displayArgs) > 80 {
			displayArgs = displayArgs[:77] + "..."
		}
		fmt.Printf("  %s %s\n", style.Bold.Render(fmt.Sprintf("PID %d", o.PID)), displayArgs)
	}

	fmt.Printf("\n%s\n", style.Dim.Render("Use 'gt orphans procs kill' to terminate these processes"))
	fmt.Printf("%s\n", style.Dim.Render("Use --aggressive to find more orphans via tmux session verification"))

	return nil
}

// runOrphansListProcessesAggressive lists orphans using tmux session verification.
// This finds ALL Claude processes not in any gt-* or hq-* tmux session.
func runOrphansListProcessesAggressive() error {
	zombies, err := util.FindZombieClaudeProcesses()
	if err != nil {
		return fmt.Errorf("finding zombie processes: %w", err)
	}

	if len(zombies) == 0 {
		fmt.Printf("%s No orphaned Claude processes found (aggressive mode)\n", style.Bold.Render("✓"))
		return nil
	}

	fmt.Printf("%s Found %d orphaned Claude process(es) not in any tmux session:\n\n", style.Warning.Render("⚠"), len(zombies))

	for _, z := range zombies {
		ageStr := formatProcessAge(z.Age)
		fmt.Printf("  %s %s (age: %s, tty: %s)\n",
			style.Bold.Render(fmt.Sprintf("PID %d", z.PID)),
			z.Cmd,
			style.Dim.Render(ageStr),
			z.TTY)
	}

	fmt.Printf("\n%s\n", style.Dim.Render("Use 'gt orphans procs kill --aggressive' to terminate these processes"))

	return nil
}

// formatProcessAge formats seconds into a human-readable age string
func formatProcessAge(seconds int) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm%ds", seconds/60, seconds%60)
	}
	hours := seconds / 3600
	mins := (seconds % 3600) / 60
	return fmt.Sprintf("%dh%dm", hours, mins)
}

// runOrphansKillProcesses kills orphaned Claude processes
func runOrphansKillProcesses(cmd *cobra.Command, _ []string) error {
	opts := orphanProcessesOptionsFromCommand(cmd)
	if opts.aggressive {
		return runOrphansKillProcessesAggressive(opts.force)
	}
	orphans, err := findOrphanProcesses()
	if err != nil {
		return fmt.Errorf("finding orphan processes: %w", err)
	}
	if len(orphans) == 0 {
		fmt.Printf("%s No orphaned Claude processes found (PPID=1)\n", style.Bold.Render("✓"))
		fmt.Printf("%s Use --aggressive to find orphans via tmux session verification\n", style.Dim.Render("Hint:"))
		return nil
	}
	printOrphanProcessKillList(orphans)
	if !confirmOrphanProcessKill(opts.force, len(orphans)) {
		return nil
	}
	return signalAndSummarizeOrphanProcesses(orphans, opts.force)
}

func printOrphanProcessKillList(orphans []OrphanProcess) {
	fmt.Printf("%s Found %d orphaned Claude process(es) with PPID=1:\n\n", style.Warning.Render("⚠"), len(orphans))
	for _, o := range orphans {
		fmt.Printf("  %s %s\n", style.Bold.Render(fmt.Sprintf("PID %d", o.PID)), truncateOrphanArgs(o.Args))
	}
	fmt.Println()
}

func confirmOrphanProcessKill(force bool, count int) bool {
	if force {
		return true
	}
	fmt.Printf("Kill these %d process(es)? [y/N] ", count)
	var response string
	_, _ = fmt.Scanln(&response)
	response = strings.ToLower(strings.TrimSpace(response))
	if response != "y" && response != "yes" {
		fmt.Println("Aborted")
		return false
	}
	return true
}

func signalAndSummarizeOrphanProcesses(orphans []OrphanProcess, force bool) error {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	killed, failed := signalOrphanProcesses(orphans, signal)
	fmt.Printf("\n%s %d killed", style.Bold.Render("Summary:"), killed)
	if failed > 0 {
		fmt.Printf(", %d failed", failed)
	}
	fmt.Println()
	return nil
}

// runOrphansKillProcessesAggressive kills orphans using tmux session verification.
// This kills ALL Claude processes not in any gt-* or hq-* tmux session.
func runOrphansKillProcessesAggressive(force bool) error {
	zombies, err := util.FindZombieClaudeProcesses()
	if err != nil {
		return fmt.Errorf("finding zombie processes: %w", err)
	}
	if len(zombies) == 0 {
		fmt.Printf("%s No orphaned Claude processes found (aggressive mode)\n", style.Bold.Render("✓"))
		return nil
	}
	printAggressiveZombieList(zombies)
	if !confirmOrphanProcessKill(force, len(zombies)) {
		return nil
	}
	return signalAndSummarizeOrphanProcesses(orphanProcessesFromZombies(zombies), force)
}

func printAggressiveZombieList(zombies []util.ZombieProcess) {
	fmt.Printf("%s Found %d orphaned Claude process(es) not in any tmux session:\n\n", style.Warning.Render("⚠"), len(zombies))
	for _, z := range zombies {
		fmt.Printf("  %s %s (age: %s, tty: %s)\n",
			style.Bold.Render(fmt.Sprintf("PID %d", z.PID)),
			z.Cmd,
			style.Dim.Render(formatProcessAge(z.Age)),
			z.TTY)
	}
	fmt.Println()
}

func orphanProcessesFromZombies(zombies []util.ZombieProcess) []OrphanProcess {
	out := make([]OrphanProcess, len(zombies))
	for i, z := range zombies {
		out[i] = OrphanProcess{PID: z.PID, Args: z.Cmd}
	}
	return out
}
