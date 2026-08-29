// Package cmd provides CLI commands for the gt tool.
package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/crew"
	"github.com/jonbaldie/gastown/internal/deps"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/hooks"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/refinery"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/suggest"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/wisp"
	"github.com/jonbaldie/gastown/internal/witness"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var rigCmd = &cobra.Command{
	Use:     "rig",
	GroupID: GroupWorkspace,
	Short:   "Manage rigs in the workspace",
	RunE:    requireSubcommand,
	Long: `Manage rigs (project containers) in the Gas Town workspace.

A rig is a container for managing a project and its agents:
  - refinery/rig/  Canonical main clone (Refinery's working copy)
  - mayor/rig/     Mayor's working clone for this rig
  - crew/<name>/   Human workspace(s)
  - witness/       Witness agent (no clone)
  - polecats/      Worker directories
  - .beads/        Rig-level issue tracking`,
}

var rigAddCmd = &cobra.Command{
	Use:   "add <name> <git-url>",
	Short: "Add a new rig to the workspace",
	Long: `Add a new rig by cloning a repository.

This creates a rig container with:
  - config.json           Rig configuration
  - .beads/               Rig-level issue tracking (initialized)
  - plugins/              Rig-level plugin directory
  - refinery/rig/         Canonical main clone
  - mayor/rig/            Mayor's working clone
  - crew/                 Empty crew directory (add members with 'gt crew add')
  - witness/              Witness agent directory
  - polecats/             Worker directory (empty)

The command also:
  - Seeds patrol molecules (Deacon, Witness, Refinery)
  - Creates ~/gt/plugins/ (town-level) if it doesn't exist
  - Creates <rig>/plugins/ (rig-level)

Use --adopt to register an existing directory instead of creating new:
  - Reads existing config.json if present
  - Auto-detects git URL from origin remote (git-url argument not required)
  - Adds entry to mayor/rigs.json

For a repo you don't own, use fork mode (fetch upstream, push to fork).
See docs/guides/fork-rig-setup.md for setup, verification, and recovery.
Without --push-url, default merge will push to origin. Use --merge=local
to keep work on a local feature branch.

Example:
  gt rig add gastown https://github.com/jonbaldie/gastown
  gt rig add my_project git@github.com:user/repo.git --prefix mp
  gt rig add existing_rig --adopt
  gt rig add gastown https://github.com/gastownhall/gastown \
    --push-url https://github.com/you/gastown \
    --upstream-url https://github.com/gastownhall/gastown
  gt rig add beads https://github.com/jonbaldie/beads --import-beads`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runRigAdd,
}

var rigListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all rigs in the workspace",
	Long: `List all rigs registered in the Gas Town workspace.

For each rig, displays:
  - Rig name and operational state (OPERATIONAL, PARKED, DOCKED)
  - Witness status (running/stopped)
  - Refinery status (running/stopped)
  - Number of polecats and crew members

Examples:
  gt rig list          # List all rigs with status
  gt rig list --json   # Output as JSON for scripting`,
	RunE: runRigList,
}

var rigRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a rig from the registry (does not delete files)",
	Long: `Remove a rig from the Gas Town registry.

This only removes the rig entry from mayor/rigs.json and cleans up
the beads route. The rig's files on disk are NOT deleted.

If the rig has running tmux sessions (witness, refinery, polecats, crew),
you must shut them down first with 'gt rig shutdown' or use --force to
kill them automatically.

To fully remove a rig, delete the directory manually after unregistering.

Examples:
  gt rig remove myproject                    # Unregister (fails if sessions running)
  gt rig remove myproject --force            # Kill sessions then unregister
  gt rig remove myproject && rm -rf myproject # Unregister and delete files`,
	Args: cobra.ExactArgs(1),
	RunE: runRigRemove,
}

var rigResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset rig state (handoff content, mail, stale issues)",
	Long: `Reset various rig state.

By default, resets all resettable state. Use flags to reset specific items.

Examples:
  gt rig reset              # Reset all state
  gt rig reset --handoff    # Clear handoff content only
  gt rig reset --mail       # Clear stale mail messages only
  gt rig reset --stale      # Reset orphaned in_progress issues
  gt rig reset --stale --dry-run  # Preview what would be reset`,
	RunE: runRigReset,
}

var rigBootCmd = &cobra.Command{
	Use:   "boot <rig>",
	Short: "Start witness and refinery for a rig",
	Long: `Start the witness and refinery agents for a rig.

This is the inverse of 'gt rig shutdown'. It starts:
- The witness (if not already running)
- The refinery (if not already running)

Polecats are NOT started by this command - they are spawned
on demand when work is assigned.

Examples:
  gt rig boot greenplace`,
	Args: cobra.ExactArgs(1),
	RunE: runRigBoot,
}

var rigStartCmd = &cobra.Command{
	Use:   "start <rig>...",
	Short: "Start witness and refinery on patrol for one or more rigs",
	Long: `Start the witness and refinery agents on patrol for one or more rigs.

This is similar to 'gt rig boot' but supports multiple rigs at once.
For each rig, it starts:
- The witness (if not already running)
- The refinery (if not already running)

Polecats are NOT started by this command - they are spawned
on demand when work is assigned.

Examples:
  gt rig start gastown
  gt rig start gastown beads
  gt rig start gastown beads myproject`,
	Args: cobra.MinimumNArgs(1),
	RunE: runRigStart,
}

var rigRebootCmd = &cobra.Command{
	Use:   "reboot <rig>",
	Short: "Restart witness and refinery for a rig",
	Long: `Restart the patrol agents (witness and refinery) for a rig.

This is equivalent to 'gt rig shutdown' followed by 'gt rig boot'.
Useful after polecats complete work and land their changes.

Examples:
  gt rig reboot greenplace
  gt rig reboot beads --force`,
	Args: cobra.ExactArgs(1),
	RunE: runRigReboot,
}

var rigShutdownCmd = &cobra.Command{
	Use:   "shutdown <rig>",
	Short: "Gracefully stop all rig agents",
	Long: `Stop all agents in a rig.

This command gracefully shuts down:
- All polecat sessions
- The refinery (if running)
- The witness (if running)

Before shutdown, checks all polecats for uncommitted work:
- Uncommitted changes (modified/untracked files)
- Stashes
- Unpushed commits

Use --force to force immediate shutdown (prompts if uncommitted work).
Use --nuclear to bypass ALL safety checks (will lose work!).

Examples:
  gt rig shutdown greenplace
  gt rig shutdown greenplace --force
  gt rig shutdown greenplace --nuclear  # DANGER: loses uncommitted work`,
	Args: cobra.ExactArgs(1),
	RunE: runRigShutdown,
}

var rigStatusCmd = &cobra.Command{
	Use:        "status [rig]",
	SuggestFor: []string{"health", "health-check", "healthcheck"},
	Short:      "Show detailed status for a specific rig",
	Long: `Show detailed status for a specific rig including all workers.

If no rig is specified, infers the rig from the current directory.

Displays:
- Rig information (name, path, beads prefix)
- Witness status (running/stopped, uptime)
- Refinery status (running/stopped, uptime, queue size)
- Polecats (name, state, assigned issue, session status)
- Crew members (name, branch, session status, git status)

Examples:
  gt rig status           # Infer rig from current directory
  gt rig status gastown
  gt rig status beads`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRigStatus,
}

var rigStopCmd = &cobra.Command{
	Use:   "stop <rig>...",
	Short: "Stop one or more rigs (shutdown semantics)",
	Long: `Stop all agents in one or more rigs.

This command is similar to 'gt rig shutdown' but supports multiple rigs.
For each rig, it gracefully shuts down:
- All polecat sessions
- The refinery (if running)
- The witness (if running)

Before shutdown, checks all polecats for uncommitted work:
- Uncommitted changes (modified/untracked files)
- Stashes
- Unpushed commits

Use --force to force immediate shutdown (prompts if uncommitted work).
Use --nuclear to bypass ALL safety checks (will lose work!).

Examples:
  gt rig stop gastown
  gt rig stop gastown beads
  gt rig stop --force gastown beads
  gt rig stop --nuclear gastown  # DANGER: loses uncommitted work`,
	Args: cobra.MinimumNArgs(1),
	RunE: runRigStop,
}

var rigRestartCmd = &cobra.Command{
	Use:   "restart <rig>...",
	Short: "Restart one or more rigs (stop then start)",
	Long: `Restart the patrol agents (witness and refinery) for one or more rigs.

This is equivalent to 'gt rig stop' followed by 'gt rig start' for each rig.
Useful after polecats complete work and land their changes.

Before shutdown, checks all polecats for uncommitted work:
- Uncommitted changes (modified/untracked files)
- Stashes
- Unpushed commits

Use --force to force immediate shutdown (prompts if uncommitted work).
Use --nuclear to bypass ALL safety checks (will lose work!).

Examples:
  gt rig restart gastown
  gt rig restart gastown beads
  gt rig restart --force gastown beads
  gt rig restart --nuclear gastown  # DANGER: loses uncommitted work`,
	Args: cobra.MinimumNArgs(1),
	RunE: runRigRestart,
}

// Flags
type rigCommandState struct {
	addPrefix         string
	addLocalRepo      string
	addBranch         string
	addPushURL        string
	addUpstreamURL    string
	addAdopt          bool
	addAdoptURL       string
	addAdoptForce     bool
	addFilter         string
	addSparseCheckout []string
	addImportBeads    bool
	resetHandoff      bool
	resetMail         bool
	resetStale        bool
	resetDryRun       bool
	resetRole         string
	shutdownForce     bool
	shutdownNuclear   bool
	rebootForce       bool
	rebootNuclear     bool
	stopForce         bool
	stopNuclear       bool
	restartForce      bool
	restartNuclear    bool
	listJSON          bool
	removeForce       bool
}

var rigCommandStateInstance = sync.OnceValue(func() *rigCommandState {
	return &rigCommandState{}
})

func rigState() *rigCommandState {
	return rigCommandStateInstance()
}

var (
	// Test seams for checkUncommittedWork.
	listPolecatsForWorkCheck = func(r *rig.Rig) ([]*polecat.Polecat, error) {
		polecatGit := git.NewGit(r.Path)
		polecatMgr := polecat.NewManager(r, polecatGit, nil) // nil tmux: just listing
		return polecatMgr.List()
	}
	checkPolecatWorkStatus = func(clonePath string) (*git.UncommittedWorkStatus, error) {
		pGit := git.NewGit(clonePath)
		return pGit.CheckUncommittedWork()
	}
	isStdinTerminal = func() bool {
		return term.IsTerminal(int(os.Stdin.Fd()))
	}
	promptYesNoUnsafeProceed = promptYesNo
)

func init() {
	rootCmd.AddCommand(rigCmd)
	rigCmd.AddCommand(rigAddCmd)
	rigCmd.AddCommand(rigBootCmd)
	rigCmd.AddCommand(rigListCmd)
	rigCmd.AddCommand(rigRebootCmd)
	rigCmd.AddCommand(rigRemoveCmd)
	rigCmd.AddCommand(rigResetCmd)
	rigCmd.AddCommand(rigRestartCmd)
	rigCmd.AddCommand(rigShutdownCmd)
	rigCmd.AddCommand(rigStartCmd)
	rigCmd.AddCommand(rigMenuCmd)
	rigCmd.AddCommand(rigStatusCmd)
	rigCmd.AddCommand(rigStopCmd)

	rigListCmd.Flags().BoolVar(&rigState().listJSON, "json", false, "Output as JSON")

	rigRemoveCmd.Flags().BoolVarP(&rigState().removeForce, "force", "f", false, "Kill running tmux sessions before removing (may lose uncommitted work)")

	rigAddCmd.Flags().StringVar(&rigState().addPrefix, "prefix", "", "Beads issue prefix (default: derived from name)")
	rigAddCmd.Flags().StringVar(&rigState().addLocalRepo, "local-repo", "", "Local repo path to share git objects (optional)")
	rigAddCmd.Flags().StringVar(&rigState().addBranch, "branch", "", "Default branch name (default: auto-detected from remote)")
	rigAddCmd.Flags().StringVar(&rigState().addPushURL, "push-url", "", "Push URL for read-only upstreams, i.e. push to fork (see docs/guides/fork-rig-setup.md)")
	rigAddCmd.Flags().StringVar(&rigState().addUpstreamURL, "upstream-url", "", "Upstream repository URL for fork workflows (see docs/guides/fork-rig-setup.md)")
	rigAddCmd.Flags().BoolVar(&rigState().addAdopt, "adopt", false, "Adopt an existing directory instead of creating new")
	rigAddCmd.Flags().StringVar(&rigState().addAdoptURL, "url", "", "Git remote URL for --adopt (default: auto-detected from origin)")
	rigAddCmd.Flags().BoolVar(&rigState().addAdoptForce, "force", false, "With --adopt, register even if git remote cannot be detected")
	rigAddCmd.Flags().StringVar(&rigState().addFilter, "filter", "", "Partial clone filter (e.g. \"blob:none\", \"tree:0\") to reduce clone size")
	rigAddCmd.Flags().StringSliceVar(&rigState().addSparseCheckout, "sparse-checkout", nil, "Sparse checkout paths (cone mode); comma-separated or repeated")
	rigAddCmd.Flags().BoolVar(&rigState().addImportBeads, "import-beads", false, "Consent to activate tracked Beads data and executable hooks from the source repo")

	rigResetCmd.Flags().BoolVar(&rigState().resetHandoff, "handoff", false, "Clear handoff content")
	rigResetCmd.Flags().BoolVar(&rigState().resetMail, "mail", false, "Clear stale mail messages")
	rigResetCmd.Flags().BoolVar(&rigState().resetStale, "stale", false, "Reset orphaned in_progress issues (no active session)")
	rigResetCmd.Flags().BoolVar(&rigState().resetDryRun, "dry-run", false, "Show what would be reset without making changes")
	rigResetCmd.Flags().StringVar(&rigState().resetRole, "role", "", "Role to reset (default: auto-detect from cwd)")

	rigShutdownCmd.Flags().BoolVarP(&rigState().shutdownForce, "force", "f", false, "Force immediate shutdown (prompts if uncommitted work)")
	rigShutdownCmd.Flags().BoolVar(&rigState().shutdownNuclear, "nuclear", false, "DANGER: Bypass ALL safety checks (loses uncommitted work!)")

	rigRebootCmd.Flags().BoolVarP(&rigState().rebootForce, "force", "f", false, "Force immediate shutdown during reboot (prompts if uncommitted work)")
	rigRebootCmd.Flags().BoolVar(&rigState().rebootNuclear, "nuclear", false, "DANGER: Bypass ALL safety checks during reboot (loses uncommitted work!)")

	rigStopCmd.Flags().BoolVarP(&rigState().stopForce, "force", "f", false, "Force immediate shutdown (prompts if uncommitted work)")
	rigStopCmd.Flags().BoolVar(&rigState().stopNuclear, "nuclear", false, "DANGER: Bypass ALL safety checks (loses uncommitted work!)")

	rigRestartCmd.Flags().BoolVarP(&rigState().restartForce, "force", "f", false, "Force immediate shutdown during restart (prompts if uncommitted work)")
	rigRestartCmd.Flags().BoolVar(&rigState().restartNuclear, "nuclear", false, "DANGER: Bypass ALL safety checks (loses uncommitted work!)")
}

func confirmUnsafeProceed(force bool) bool {
	// If --force and interactive TTY, prompt.
	if force && isStdinTerminal() {
		fmt.Println()
		return promptYesNoUnsafeProceed("Proceed anyway?")
	}

	// Otherwise block with hint.
	if force {
		fmt.Printf("\n%s requires an interactive terminal. Use %s to skip all checks (DANGER: will lose work!)\n",
			style.Bold.Render("--force"), style.Bold.Render("--nuclear"))
	} else {
		fmt.Printf("\nUse %s to proceed with confirmation, or %s to skip all checks (DANGER: will lose work!)\n",
			style.Bold.Render("--force"), style.Bold.Render("--nuclear"))
	}
	return false
}

// checkUncommittedWork checks polecats in a rig for uncommitted work.
// operation is the verb shown in the warning (e.g. "stop", "shutdown", "restart").
// Returns true if the caller should proceed, false if it should abort.
// When force is true and stdin is a TTY, prompts the user to confirm.
// When force is true but stdin is NOT a TTY, blocks (same as no --force).
// All user-facing messages are printed internally.
type polecatWorkProblem struct {
	name   string
	status *git.UncommittedWorkStatus
}

type polecatWorkCheckError struct {
	name string
	err  error
}

func collectPolecatWork(polecats []*polecat.Polecat) ([]polecatWorkProblem, []polecatWorkCheckError) {
	var problemPolecats []polecatWorkProblem
	var checkErrors []polecatWorkCheckError
	for _, p := range polecats {
		status, err := checkPolecatWorkStatus(p.ClonePath)
		if err != nil {
			checkErrors = append(checkErrors, polecatWorkCheckError{name: p.Name, err: err})
			continue
		}
		if status == nil {
			checkErrors = append(checkErrors, polecatWorkCheckError{name: p.Name, err: fmt.Errorf("no status returned")})
			continue
		}
		if !status.Clean() {
			problemPolecats = append(problemPolecats, polecatWorkProblem{name: p.Name, status: status})
		}
	}
	return problemPolecats, checkErrors
}

func checkUncommittedWork(r *rig.Rig, rigName, operation string, force bool) (proceed bool) {
	polecats, err := listPolecatsForWorkCheck(r)
	if err != nil {
		fmt.Printf("%s Could not check polecats for uncommitted work: %v\n",
			style.Warning.Render("⚠"), err)
		return confirmUnsafeProceed(force)
	}
	if len(polecats) == 0 {
		return true
	}

	problemPolecats, checkErrors := collectPolecatWork(polecats)
	if len(problemPolecats) == 0 && len(checkErrors) == 0 {
		return true
	}

	if len(problemPolecats) > 0 {
		fmt.Printf("\n%s Cannot %s %s - polecats have uncommitted work:\n",
			style.Warning.Render("⚠"), operation, rigName)
		for _, pp := range problemPolecats {
			fmt.Printf("  %s: %s\n", style.Bold.Render(pp.name), pp.status.String())
		}
	}
	if len(checkErrors) > 0 {
		fmt.Printf("\n%s Could not verify uncommitted work for:\n", style.Warning.Render("⚠"))
		for _, checkErr := range checkErrors {
			fmt.Printf("  %s: %v\n", style.Bold.Render(checkErr.name), checkErr.err)
		}
	}

	return confirmUnsafeProceed(force)
}

func runRigAdd(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Handle --adopt mode: register existing directory
	if rigState().addAdopt {
		return runRigAdopt(cmd, args)
	}

	// Normal add mode requires git URL
	if len(args) < 2 {
		return fmt.Errorf("git-url is required (or use --adopt to register an existing directory)")
	}
	gitURL := args[1]
	if !isGitRemoteURL(gitURL) {
		return fmt.Errorf("invalid git URL %q: expected a remote URL (e.g. https://, git@host:, ssh://, s3://, file:///abs/path)\n\nTo use a local repo as the source, pass a file:// URL. To register an already-assembled rig directory, use:\n  gt rig add %s --adopt", gitURL, name)
	}

	if err := deps.EnsureBeads(true); err != nil {
		return fmt.Errorf("beads dependency check failed: %w", err)
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	return addRigToTown(townRoot, rig.AddRigOptions{
		Name:           name,
		GitURL:         gitURL,
		PushURL:        rigState().addPushURL,
		UpstreamURL:    rigState().addUpstreamURL,
		BeadsPrefix:    rigState().addPrefix,
		LocalRepo:      rigState().addLocalRepo,
		DefaultBranch:  rigState().addBranch,
		CloneFilter:    rigState().addFilter,
		SparseCheckout: rigState().addSparseCheckout,
		ImportBeads:    rigState().addImportBeads,
	})
}

func newRigBeadsWorkDir(townRoot, name string, newRig *rig.Rig) string {
	if newRig.Config.Prefix == "" {
		return ""
	}
	mayorRigBeads := filepath.Join(townRoot, name, "mayor", "rig", ".beads")
	if _, err := os.Stat(mayorRigBeads); err == nil {
		return filepath.Join(townRoot, name, "mayor", "rig")
	}
	return filepath.Join(townRoot, name)
}

func createNewRigAgentBeads(townRoot string, opts rig.AddRigOptions, newRig *rig.Rig) {
	beadsWorkDir := newRigBeadsWorkDir(townRoot, opts.Name, newRig)
	if beadsWorkDir == "" {
		return
	}
	bd := beads.New(beadsWorkDir)
	fields := &beads.RigFields{Repo: opts.GitURL, Prefix: newRig.Config.Prefix, State: beads.RigStateActive}
	if _, err := bd.CreateRigBead(opts.Name, fields); err != nil {
		fmt.Printf("  %s Could not create rig identity bead: %v\n", style.Warning.Render("!"), err)
	} else {
		rigBeadID := beads.RigBeadIDWithPrefix(newRig.Config.Prefix, opts.Name)
		fmt.Printf("  Created rig identity bead: %s\n", rigBeadID)
	}

	prefix := newRig.Config.Prefix
	witnessID := beads.WitnessBeadIDWithPrefix(prefix, opts.Name)
	if _, err := bd.CreateAgentBead(witnessID,
		fmt.Sprintf("Witness for %s - monitors polecat health and progress.", opts.Name),
		&beads.AgentFields{RoleType: "witness", Rig: opts.Name, AgentState: "idle"},
	); err != nil {
		fmt.Printf("  %s Could not create witness agent bead: %v\n", style.Warning.Render("!"), err)
	} else {
		fmt.Printf("  Created agent bead: %s\n", witnessID)
	}

	refineryID := beads.RefineryBeadIDWithPrefix(prefix, opts.Name)
	if _, err := bd.CreateAgentBead(refineryID,
		fmt.Sprintf("Refinery for %s - processes merge queue.", opts.Name),
		&beads.AgentFields{RoleType: "refinery", Rig: opts.Name, AgentState: "idle"},
	); err != nil {
		fmt.Printf("  %s Could not create refinery agent bead: %v\n", style.Warning.Render("!"), err)
	} else {
		fmt.Printf("  Created agent bead: %s\n", refineryID)
	}
}

func finalizeNewRig(townRoot string, opts rig.AddRigOptions, mgr *rig.Manager) {
	autoAssignNamepoolTheme(townRoot, opts.Name, mgr)
	ensureHooksBase()
	if err := syncRigHooks(townRoot, opts.Name); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to sync hooks for new rig: %v\n", err)
	}
	commitTownConfigChanges(townRoot, opts.Name)
	refreshCycleBindingsOnExistingSessions()
}

func reportNewRig(townRoot string, opts rig.AddRigOptions, newRig *rig.Rig, startTime time.Time) {
	elapsed := time.Since(startTime)
	defaultBranch := "main"
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, opts.Name)); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	fmt.Printf("\n%s Rig created in %.1fs\n", style.Success.Render("✓"), elapsed.Seconds())
	fmt.Printf("\nStructure:\n")
	fmt.Printf("  %s/\n", opts.Name)
	fmt.Printf("  ├── config.json\n")
	fmt.Printf("  ├── .repo.git/        (shared bare repo for refinery+polecats)\n")
	fmt.Printf("  ├── .beads/           (prefix: %s)\n", newRig.Config.Prefix)
	fmt.Printf("  ├── plugins/          (rig-level plugins)\n")
	fmt.Printf("  ├── mayor/rig/        (clone: %s)\n", defaultBranch)
	fmt.Printf("  ├── refinery/rig/     (worktree: %s, sees polecat branches)\n", defaultBranch)
	fmt.Printf("  ├── crew/             (empty - add crew with 'gt crew add')\n")
	fmt.Printf("  ├── witness/\n")
	fmt.Printf("  └── polecats/         (.claude/ scaffolded for polecat sessions)\n")
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  gt crew add <name> --rig %s   # Create your personal workspace\n", opts.Name)
	fmt.Printf("  cd %s/crew/<name>              # Start working\n", filepath.Join(townRoot, opts.Name))
}

func validRigCloneFilter(filter string) bool {
	for _, supported := range []string{"blob:none", "tree:0"} {
		if filter == supported {
			return true
		}
	}
	return false
}

func validateRigAddOptions(opts *rig.AddRigOptions) error {
	opts.PushURL = strings.TrimSpace(opts.PushURL)
	opts.UpstreamURL = strings.TrimSpace(opts.UpstreamURL)
	warnThirdPartyRigAdd(opts.GitURL, opts.PushURL)
	if opts.PushURL != "" && !isGitRemoteURL(opts.PushURL) {
		return fmt.Errorf("invalid push URL %q: expected a remote URL (e.g. https://, git@host:, ssh://, s3://)", opts.PushURL)
	}
	if opts.UpstreamURL != "" && !isGitRemoteURL(opts.UpstreamURL) {
		return fmt.Errorf("invalid upstream URL %q: expected a remote URL (e.g. https://, git@host:, ssh://, s3://)", opts.UpstreamURL)
	}
	if opts.CloneFilter != "" {
		if !validRigCloneFilter(opts.CloneFilter) {
			return fmt.Errorf("invalid --filter %q: supported values are %v", opts.CloneFilter, []string{"blob:none", "tree:0"})
		}
		fmt.Printf("  Partial clone: --filter=%s\n", opts.CloneFilter)
	}
	if len(opts.SparseCheckout) > 0 {
		fmt.Printf("  Sparse checkout: %v\n", opts.SparseCheckout)
	}
	return nil
}

func addRigToTown(townRoot string, opts rig.AddRigOptions) error {
	if !isGitRemoteURL(opts.GitURL) {
		return fmt.Errorf("invalid git URL %q: expected a remote URL (e.g. https://, git@host:, ssh://, s3://, file:///abs/path)\n\nTo use a local repo as the source, pass a file:// URL. To register an already-assembled rig directory, use:\n  gt rig add %s --adopt", opts.GitURL, opts.Name)
	}

	if err := deps.EnsureBeads(true); err != nil {
		return fmt.Errorf("beads dependency check failed: %w", err)
	}

	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{
			Version: 1,
			Rigs:    make(map[string]config.RigEntry),
		}
	}

	g := git.NewGit(townRoot)
	mgr := rig.NewManager(townRoot, rigsConfig, g)

	fmt.Printf("Creating rig %s...\n", style.Bold.Render(opts.Name))
	fmt.Printf("  Repository: %s\n", opts.GitURL)
	if opts.LocalRepo != "" {
		fmt.Printf("  Local repo: %s\n", opts.LocalRepo)
	}
	if err := validateRigAddOptions(&opts); err != nil {
		return err
	}

	startTime := time.Now()

	newRig, err := mgr.AddRig(opts)
	if err != nil {
		return fmt.Errorf("adding rig: %w", err)
	}

	if err := config.AddRigToDaemonPatrols(townRoot, opts.Name); err != nil {
		fmt.Printf("  %s Could not update daemon.json patrols: %v\n", style.Warning.Render("!"), err)
	}

	createNewRigAgentBeads(townRoot, opts, newRig)
	finalizeNewRig(townRoot, opts, mgr)
	reportNewRig(townRoot, opts, newRig, startTime)

	return nil
}

// GetRigLED returns the LED indicator for a rig based on session and operational state.
// Used by both rig list and statusline for consistent indicators:
//   - 🟢 = both witness and refinery running (fully active)
//   - 🟡 = one session running (partially active)
//   - ⚫ = nothing running (stopped)
//   - 🅿️ = parked (intentionally paused)
//   - 🛑 = docked (global shutdown)
func GetRigLED(hasWitness, hasRefinery bool, opState string) string {
	// Check operational state FIRST — parked/docked overrides session state.
	// Sessions may still be running during the race window after park/dock
	// but before sessions are killed (GH#2555).
	switch opState {
	case "PARKED":
		return "🅿️"
	case "DOCKED":
		return "🛑"
	}

	if hasWitness && hasRefinery {
		return "🟢"
	}
	if hasWitness || hasRefinery {
		return "🟡"
	}
	return "⚫"
}

// rigStatePriority returns a sort priority for a rig's state.
// Lower values sort first: active > partial > stopped > parked > docked.
func rigStatePriority(hasWitness, hasRefinery bool, opState string) int {
	if hasWitness && hasRefinery {
		return 0
	}
	if hasWitness || hasRefinery {
		return 1
	}
	switch opState {
	case "PARKED":
		return 3
	case "DOCKED":
		return 4
	default:
		return 2
	}
}

type rigListInfo struct {
	Name        string `json:"name"`
	BeadsPrefix string `json:"beads_prefix"`
	Status      string `json:"status"`
	Witness     string `json:"witness"`
	Refinery    string `json:"refinery"`
	Polecats    int    `json:"polecats"`
	Crew        int    `json:"crew"`
	// sorting fields (not exported to JSON)
	sortPrio int
}

func loadRigListConfig() (string, *config.RigsConfig, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		return townRoot, nil, err
	}
	return townRoot, rigsConfig, nil
}

func rigListInfoFor(name, townRoot string, mgr *rig.Manager, t *tmux.Tmux) rigListInfo {
	prefix := session.PrefixFor(name)
	r, err := mgr.GetRig(name)
	if err != nil {
		return rigListInfo{Name: name, BeadsPrefix: prefix, Status: "error", sortPrio: 99}
	}
	opState, _ := getRigOperationalState(townRoot, name)
	witnessRunning, _ := t.HasSession(session.WitnessSessionName(prefix))
	refineryRunning, _ := t.HasSession(session.RefinerySessionName(prefix))
	witnessStatus := "stopped"
	if witnessRunning {
		witnessStatus = "running"
	}
	refineryStatus := "stopped"
	if refineryRunning {
		refineryStatus = "running"
	}
	summary := rig.Summary(r)
	return rigListInfo{
		Name:        name,
		BeadsPrefix: prefix,
		Status:      strings.ToLower(opState),
		Witness:     witnessStatus,
		Refinery:    refineryStatus,
		Polecats:    summary.PolecatCount,
		Crew:        summary.CrewCount,
		sortPrio:    rigStatePriority(witnessRunning, refineryRunning, opState),
	}
}

func sortRigList(rigs []rigListInfo) {
	sort.Slice(rigs, func(i, j int) bool {
		if rigs[i].sortPrio != rigs[j].sortPrio {
			return rigs[i].sortPrio < rigs[j].sortPrio
		}
		return rigs[i].Name < rigs[j].Name
	})
}

func encodeRigListJSON(rigs []rigListInfo) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rigs)
}

func rigListLEDEntry(ri rigListInfo) {
	if ri.Status == "error" {
		fmt.Printf("  %s %s\n", style.Warning.Render("!"), ri.Name)
		return
	}
	led := GetRigLED(ri.Witness == "running", ri.Refinery == "running", strings.ToUpper(ri.Status))
	space := " "
	if led == "🅿️" {
		space = "  "
	}
	fmt.Printf("%s%s%s\n", led, space, style.Bold.Render(ri.Name))
	witnessIcon := style.Dim.Render("○")
	if ri.Witness == "running" {
		witnessIcon = style.Success.Render("●")
	}
	refineryIcon := style.Dim.Render("○")
	if ri.Refinery == "running" {
		refineryIcon = style.Success.Render("●")
	}
	fmt.Printf("   Witness: %s %s  Refinery: %s %s\n", witnessIcon, ri.Witness, refineryIcon, ri.Refinery)
	fmt.Printf("   Polecats: %d  Crew: %d\n", ri.Polecats, ri.Crew)
	fmt.Println()
}

func renderRigList(townRoot string, rigs []rigListInfo) {
	fmt.Printf("Rigs in %s:\n\n", townRoot)
	for _, ri := range rigs {
		rigListLEDEntry(ri)
	}
}

func runRigList(_ *cobra.Command, _ []string) error {
	townRoot, rigsConfig, err := loadRigListConfig()
	if err != nil {
		fmt.Println("No rigs configured.")
		return nil
	}
	if len(rigsConfig.Rigs) == 0 {
		fmt.Println("No rigs configured.")
		fmt.Printf("\nAdd one with: %s\n", style.Dim.Render("gt rig add <name> <git-url>"))
		return nil
	}
	mgr := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot))
	t := tmux.NewTmux()
	rigs := make([]rigListInfo, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		rigs = append(rigs, rigListInfoFor(name, townRoot, mgr, t))
	}
	sortRigList(rigs)
	if rigState().listJSON {
		return encodeRigListJSON(rigs)
	}
	renderRigList(townRoot, rigs)

	return nil
}

var rigMenuCmd = &cobra.Command{
	Use:    "menu",
	Short:  "Show interactive rig menu in tmux",
	Long:   `Display a tmux popup menu listing all rigs with status indicators and per-rig actions.`,
	Hidden: true, // Internal command called by keybinding
	RunE:   runRigMenu,
}

type rigMenuEntry struct {
	name     string
	led      string
	running  bool
	opState  string
	sortPrio int
}

func collectRigMenuEntries(townRoot string, rigsConfig *config.RigsConfig, t *tmux.Tmux) []rigMenuEntry {
	var rigs []rigMenuEntry
	for name := range rigsConfig.Rigs {
		prefix := session.PrefixFor(name)
		opState, _ := getRigOperationalState(townRoot, name)

		witnessSession := session.WitnessSessionName(prefix)
		refinerySession := session.RefinerySessionName(prefix)
		hasWitness, _ := t.HasSession(witnessSession)
		hasRefinery, _ := t.HasSession(refinerySession)

		led := GetRigLED(hasWitness, hasRefinery, opState)
		rigs = append(rigs, rigMenuEntry{
			name:     name,
			led:      led,
			running:  hasWitness || hasRefinery,
			opState:  opState,
			sortPrio: rigStatePriority(hasWitness, hasRefinery, opState),
		})
	}
	return rigs
}

func sortRigMenuEntries(rigs []rigMenuEntry) {
	sort.Slice(rigs, func(i, j int) bool {
		if rigs[i].sortPrio != rigs[j].sortPrio {
			return rigs[i].sortPrio < rigs[j].sortPrio
		}
		return rigs[i].name < rigs[j].name
	})
}

func appendRigMenuEntry(menuArgs []string, r rigMenuEntry, keyIndex int) []string {
	space := " "
	if r.led == "🅿️" {
		space = "  "
	}
	label := fmt.Sprintf("%s%s%s", r.led, space, r.name)
	key := shortcutKey(keyIndex)
	action := fmt.Sprintf("display-popup -E -w 80 -h 25 -T ' %s ' 'gt rig status %s; echo; echo \"Press any key to close\"; read -rsn1'", r.name, r.name)
	menuArgs = append(menuArgs, label, key, action)

	if r.running {
		menuArgs = append(menuArgs,
			"   Stop", "", fmt.Sprintf("run-shell 'gt rig stop %s'", r.name),
			"   Reboot", "", fmt.Sprintf("run-shell 'gt rig reboot %s'", r.name),
		)
	} else if r.opState == "PARKED" {
		menuArgs = append(menuArgs,
			"   Unpark", "", fmt.Sprintf("run-shell 'gt rig unpark %s'", r.name),
			"   Start", "", fmt.Sprintf("run-shell 'gt rig start %s'", r.name),
		)
	} else if r.opState == "DOCKED" {
		menuArgs = append(menuArgs,
			"   Undock", "", fmt.Sprintf("run-shell 'gt rig undock %s'", r.name),
		)
	} else {
		menuArgs = append(menuArgs,
			"   Start", "", fmt.Sprintf("run-shell 'gt rig start %s'", r.name),
		)
	}

	if r.opState != "PARKED" && r.opState != "DOCKED" {
		menuArgs = append(menuArgs,
			"   Park", "", fmt.Sprintf("run-shell 'gt rig park %s'", r.name),
		)
	}
	return append(menuArgs, "")
}

func buildRigMenuArgs(rigs []rigMenuEntry) []string {
	menuArgs := []string{
		"display-menu",
		"-T", "#[align=centre,fg=cyan,bold]⛽ Rigs", //nolint:misspell // tmux uses British spelling
		"-x", "C",
		"-y", "C",
		"--",
	}
	keyIndex := 0
	for _, r := range rigs {
		menuArgs = appendRigMenuEntry(menuArgs, r, keyIndex)
		keyIndex++
	}
	return menuArgs
}

func runRigMenu(_ *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil || len(rigsConfig.Rigs) == 0 {
		return fmt.Errorf("no rigs configured")
	}
	rigs := collectRigMenuEntries(townRoot, rigsConfig, tmux.NewTmux())
	sortRigMenuEntries(rigs)
	menuArgs := buildRigMenuArgs(rigs)
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}

	execCmd := exec.Command(tmuxPath, menuArgs...)
	execCmd.Stdin = os.Stdin
	execCmd.Stdout = os.Stdout
	execCmd.Stderr = os.Stderr
	return execCmd.Run()
}

type rigRemovalContext struct {
	townRoot    string
	rigsPath    string
	rigsConfig  *config.RigsConfig
	beadsPrefix string
	mgr         *rig.Manager
}

func loadRigRemovalContext(name string) (rigRemovalContext, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return rigRemovalContext{}, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		return rigRemovalContext{}, fmt.Errorf("loading rigs config: %w", err)
	}
	var beadsPrefix string
	if entry, ok := rigsConfig.Rigs[name]; ok && entry.BeadsConfig != nil {
		beadsPrefix = entry.BeadsConfig.Prefix
	}
	return rigRemovalContext{
		townRoot:    townRoot,
		rigsPath:    rigsPath,
		rigsConfig:  rigsConfig,
		beadsPrefix: beadsPrefix,
		mgr:         rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot)),
	}, nil
}

func checkRigRemovalSessions(name string, t *tmux.Tmux, force bool) ([]string, error) {
	sessions, sessErr := findRigSessions(t, name)
	if sessErr != nil {
		if !force {
			return nil, fmt.Errorf("could not verify session state for rig %s: %w (use --force to skip check)", name, sessErr)
		}
		fmt.Printf("  %s Could not check tmux sessions: %v (proceeding due to --force)\n", style.Warning.Render("!"), sessErr)
	}
	if len(sessions) == 0 || force {
		return sessions, nil
	}
	fmt.Printf("%s Rig %s has %d running tmux session(s):\n", style.Warning.Render("⚠"), name, len(sessions))
	for _, s := range sessions {
		fmt.Printf("  - %s\n", s)
	}
	fmt.Printf("\nShut them down first:\n")
	fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("gt rig shutdown %s", name)))
	fmt.Printf("Or force removal:\n")
	fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("gt rig remove %s --force", name)))
	return nil, fmt.Errorf("refusing to remove rig with running sessions")
}

func killRigRemovalSessions(name string, t *tmux.Tmux, sessions []string) error {
	// --force: kill all rig sessions (WARNING: may lose uncommitted work)
	fmt.Printf("Killing %d tmux session(s) for rig %s...\n", len(sessions), name)
	var killErrors []string
	for _, s := range sessions {
		if err := t.KillSessionWithProcesses(s); err != nil {
			fmt.Printf("  %s Failed to kill session %s: %v\n", style.Warning.Render("!"), s, err)
			killErrors = append(killErrors, s)
		} else {
			fmt.Printf("  Killed %s\n", s)
		}
	}
	if len(killErrors) > 0 {
		return fmt.Errorf("aborting remove: failed to kill %d session(s) (%s); rig left registered to avoid orphaned sessions",
			len(killErrors), strings.Join(killErrors, ", "))
	}
	return nil
}

func removeRigRegistration(ctx rigRemovalContext, name string) error {
	if err := ctx.mgr.RemoveRig(name); err == nil {
		return nil
	} else if !errors.Is(err, rig.ErrRigNotFound) {
		return fmt.Errorf("removing rig: %w", err)
	}
	rigPath := filepath.Join(ctx.townRoot, name)
	if info, statErr := os.Stat(rigPath); statErr == nil && info.IsDir() {
		fmt.Printf("%s Rig %q is not registered but directory exists at %s\n\n", style.Warning.Render("!"), name, rigPath)
		fmt.Printf("This is an inconsistent state. To fix it, either:\n")
		fmt.Printf("  Adopt the directory:  %s\n", style.Dim.Render(fmt.Sprintf("gt rig add %s --adopt", name)))
		fmt.Printf("  Delete the directory: %s\n", style.Dim.Render(fmt.Sprintf("rm -rf %s", rigPath)))
		return fmt.Errorf("rig %q not in registry but directory exists", name)
	}
	suggestions := suggest.FindSimilar(name, ctx.mgr.ListRigNames(), 3)
	return fmt.Errorf("removing rig: %s", suggest.FormatSuggestion("rig", name, suggestions, ""))
}

func finishRigRemoval(ctx rigRemovalContext, name string) error {
	if err := config.SaveRigsConfig(ctx.rigsPath, ctx.rigsConfig); err != nil {
		return fmt.Errorf("saving rigs config: %w", err)
	}
	if err := config.RemoveRigFromDaemonPatrols(ctx.townRoot, name); err != nil {
		fmt.Printf("  %s Could not update daemon.json patrols: %v\n", style.Warning.Render("!"), err)
	}
	if ctx.beadsPrefix != "" {
		if err := beads.RemoveRoute(ctx.townRoot, ctx.beadsPrefix+"-"); err != nil {
			fmt.Printf("  %s Could not remove route from routes.jsonl: %v\n", style.Warning.Render("!"), err)
		}
	}
	fmt.Printf("%s Rig %s removed from registry\n", style.Success.Render("✓"), name)
	rigPath := filepath.Join(ctx.townRoot, name)
	fmt.Printf("\nNote: Files at %s were NOT deleted.\n", rigPath)
	fmt.Printf("To delete: %s\n", style.Dim.Render(fmt.Sprintf("rm -rf %s", rigPath)))
	return nil
}

func runRigRemove(_ *cobra.Command, args []string) error {
	name := args[0]
	ctx, err := loadRigRemovalContext(name)
	if err != nil {
		return err
	}
	t := tmux.NewTmux()
	sessions, err := checkRigRemovalSessions(name, t, rigState().removeForce)
	if err != nil {
		return err
	}
	if len(sessions) > 0 {
		if err := killRigRemovalSessions(name, t, sessions); err != nil {
			return err
		}
	}
	if err := removeRigRegistration(ctx, name); err != nil {
		return err
	}
	return finishRigRemoval(ctx, name)
}

// refreshCycleBindingsOnExistingSessions forces a refresh of the tmux C-b n/p
// cycle bindings on any existing session. This is needed after gt rig add so
// the new rig's prefix is included in the grep pattern.
// Non-fatal: failure only means existing sessions need a restart to pick up the
// new prefix.
func refreshCycleBindingsOnExistingSessions() {
	t := tmux.NewTmux()
	sessions, err := t.ListSessions()
	if err != nil || len(sessions) == 0 {
		return
	}
	// Refresh bindings using any existing session as context.
	// SetCycleBindings' stale-pattern check will detect the mismatch and re-bind.
	_ = t.SetCycleBindings(sessions[0])
}

type rigAdoptContext struct {
	name       string
	townRoot   string
	rigsPath   string
	rigsConfig *config.RigsConfig
	mgr        *rig.Manager
}

func loadRigAdoptContext(name string) (rigAdoptContext, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return rigAdoptContext{}, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{
			Version: 1,
			Rigs:    make(map[string]config.RigEntry),
		}
	}
	return rigAdoptContext{
		name:       name,
		townRoot:   townRoot,
		rigsPath:   rigsPath,
		rigsConfig: rigsConfig,
		mgr:        rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot)),
	}, nil
}

func validateRigAdoptURLs() error {
	state := rigState()
	if state.addAdoptURL != "" && !isGitRemoteURL(state.addAdoptURL) {
		return fmt.Errorf("invalid git URL %q: expected a remote URL (e.g. https://, git@host:, ssh://, s3://, file:///abs/path)", state.addAdoptURL)
	}
	state.addPushURL = strings.TrimSpace(state.addPushURL)
	if state.addPushURL != "" && !isGitRemoteURL(state.addPushURL) {
		return fmt.Errorf("invalid push URL %q: expected a remote URL (e.g. https://, git@host:, ssh://, s3://, file:///abs/path)", state.addPushURL)
	}
	state.addUpstreamURL = strings.TrimSpace(state.addUpstreamURL)
	if state.addUpstreamURL != "" && !isGitRemoteURL(state.addUpstreamURL) {
		return fmt.Errorf("invalid upstream URL %q: expected a remote URL (e.g. https://, git@host:, ssh://, s3://, file:///abs/path)", state.addUpstreamURL)
	}
	return nil
}

func registerAdoptedRig(ctx rigAdoptContext) (*rig.RegisterRigResult, error) {
	if err := validateRigAdoptURLs(); err != nil {
		return nil, err
	}
	result, err := ctx.mgr.RegisterRig(rig.RegisterRigOptions{
		Name:        ctx.name,
		GitURL:      rigState().addAdoptURL,
		PushURL:     rigState().addPushURL,
		UpstreamURL: rigState().addUpstreamURL,
		BeadsPrefix: rigState().addPrefix,
		Force:       rigState().addAdoptForce,
	})
	if err != nil {
		return nil, fmt.Errorf("adopting rig: %w", err)
	}
	return result, nil
}

func configureAdoptedRig(ctx rigAdoptContext, result *rig.RegisterRigResult) error {
	if err := config.SaveRigsConfig(ctx.rigsPath, ctx.rigsConfig); err != nil {
		return fmt.Errorf("saving rigs config: %w", err)
	}
	if err := config.AddRigToDaemonPatrols(ctx.townRoot, ctx.name); err != nil {
		fmt.Printf("  %s Could not update daemon.json patrols: %v\n", style.Warning.Render("!"), err)
	}
	if result.BeadsPrefix != "" {
		routePath := ctx.name
		mayorRigBeads := filepath.Join(ctx.townRoot, ctx.name, "mayor", "rig", ".beads")
		if _, err := os.Stat(mayorRigBeads); err == nil {
			routePath = ctx.name + "/mayor/rig"
		}
		route := beads.Route{
			Prefix: result.BeadsPrefix + "-",
			Path:   routePath,
		}
		if err := beads.AppendRoute(ctx.townRoot, route); err != nil {
			fmt.Printf("  %s Could not update routes.jsonl: %v\n", style.Warning.Render("!"), err)
		}
	}
	commitTownConfigChanges(ctx.townRoot, ctx.name)
	return nil
}

func detectAdoptedBeadsPrefix(beadsDir string) string {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	metaBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return ""
	}
	var meta struct {
		Backend string `json:"backend"`
	}
	if json.Unmarshal(metaBytes, &meta) != nil || meta.Backend != "dolt" {
		return ""
	}
	workDir := filepath.Dir(beadsDir)
	bdCmd := beads.Spawn("config", "get", "issue_prefix")
	bdCmd.Dir = workDir
	if out, bdErr := bdCmd.Output(); bdErr == nil {
		if detected := strings.TrimSpace(string(out)); detected != "" {
			return detected
		}
	}
	var fullMeta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if json.Unmarshal(metaBytes, &fullMeta) == nil && strings.HasPrefix(fullMeta.DoltDatabase, "beads_") {
		return strings.TrimPrefix(fullMeta.DoltDatabase, "beads_")
	}
	return ""
}

func applyAdoptedBeadsPrefix(result *rig.RegisterRigResult, detected string) error {
	if detected == "" {
		return nil
	}
	if rigState().addPrefix != "" && strings.TrimSuffix(rigState().addPrefix, "-") != detected {
		return fmt.Errorf("prefix mismatch: source repo uses '%s' but --prefix '%s' was provided", detected, rigState().addPrefix)
	}
	if result.BeadsPrefix == "" {
		result.BeadsPrefix = detected
	}
	return nil
}

func adoptedBeadsNeedInit(beadsDir string) bool {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
		return true
	}
	metaBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return false
	}
	var meta struct {
		Backend string `json:"backend"`
	}
	if json.Unmarshal(metaBytes, &meta) != nil || meta.Backend != "dolt" {
		return false
	}
	_, err = os.Stat(filepath.Join(beadsDir, "dolt"))
	return os.IsNotExist(err)
}

func initAdoptedBeadsDatabase(townRoot, rigPath, name, beadsDir string, mgr *rig.Manager, prefix string) {
	if prefix == "" {
		return
	}
	if running, _, sErr := doltserver.IsRunning(townRoot); sErr != nil || !running {
		fmt.Printf("  %s Could not init bd database: Dolt server is not running\n", style.Warning.Render("!"))
		return
	}
	if err := mgr.InitBeads(rigPath, prefix, name); err != nil {
		fmt.Printf("  %s Could not init bd database: %v\n", style.Warning.Render("!"), err)
		return
	}
	if beadsDir != "" {
		fmt.Printf("  %s Initialized beads database (Dolt)\n", style.Success.Render("✓"))
	} else {
		fmt.Printf("  %s Initialized beads database\n", style.Success.Render("✓"))
	}
}

func prepareAdoptedBeads(townRoot, rigPath, name string, mgr *rig.Manager, result *rig.RegisterRigResult) error {
	existingBeadsDirs := listExistingRigBeadsDirs(rigPath)
	beadsDir, initFresh := adoptedRigBeadsPlan(existingBeadsDirs, result.BeadsPrefix)
	if beadsDir != "" {
		if err := applyAdoptedBeadsPrefix(result, detectAdoptedBeadsPrefix(beadsDir)); err != nil {
			return err
		}
		if adoptedBeadsNeedInit(beadsDir) {
			initAdoptedBeadsDatabase(townRoot, rigPath, name, beadsDir, mgr, result.BeadsPrefix)
		}
	}
	if initFresh {
		if running, _, sErr := doltserver.IsRunning(townRoot); sErr != nil || !running {
			fmt.Printf("  %s Could not init beads database: Dolt server is not running\n", style.Warning.Render("!"))
		} else if err := mgr.InitBeads(rigPath, result.BeadsPrefix, name); err != nil {
			fmt.Printf("  %s Could not init beads database: %v\n", style.Warning.Render("!"), err)
		} else {
			fmt.Printf("  %s Initialized beads database\n", style.Success.Render("✓"))
		}
	}
	return nil
}

func createAdoptedRigIdentityBead(bd *beads.Beads, name string, result *rig.RegisterRigResult) {
	rigBeadID := beads.RigBeadIDWithPrefix(result.BeadsPrefix, name)
	if _, err := bd.Show(rigBeadID); err == nil {
		return
	}
	fields := &beads.RigFields{Repo: result.GitURL, Prefix: result.BeadsPrefix, State: beads.RigStateActive}
	if _, err := bd.CreateRigBead(name, fields); err != nil {
		fmt.Printf("  %s Could not create rig identity bead: %v\n", style.Warning.Render("!"), err)
	} else {
		fmt.Printf("  %s Created rig identity bead: %s\n", style.Success.Render("✓"), rigBeadID)
	}
}

func createAdoptedAgentBead(bd *beads.Beads, id, kind, description string, fields *beads.AgentFields) {
	if _, err := bd.Show(id); err == nil {
		return
	}
	if _, err := bd.CreateAgentBead(id, description, fields); err != nil {
		fmt.Printf("  %s Could not create %s agent bead: %v\n", style.Warning.Render("!"), kind, err)
	} else {
		fmt.Printf("  %s Created agent bead: %s\n", style.Success.Render("✓"), id)
	}
}

func createAdoptedRigBeads(rigPath, name string, result *rig.RegisterRigResult) {
	if result.BeadsPrefix == "" {
		return
	}
	mayorRigBeads := filepath.Join(rigPath, "mayor", "rig", ".beads")
	beadsWorkDir := rigPath
	if _, err := os.Stat(mayorRigBeads); err == nil {
		beadsWorkDir = filepath.Join(rigPath, "mayor", "rig")
	}
	bd := beads.New(beadsWorkDir)
	createAdoptedRigIdentityBead(bd, name, result)
	prefix := result.BeadsPrefix
	witnessID := beads.WitnessBeadIDWithPrefix(prefix, name)
	createAdoptedAgentBead(bd, witnessID, "witness",
		fmt.Sprintf("Witness for %s - monitors polecat health and progress.", name),
		&beads.AgentFields{RoleType: "witness", Rig: name, AgentState: "idle"})
	refineryID := beads.RefineryBeadIDWithPrefix(prefix, name)
	createAdoptedAgentBead(bd, refineryID, "refinery",
		fmt.Sprintf("Refinery for %s - processes merge queue.", name),
		&beads.AgentFields{RoleType: "refinery", Rig: name, AgentState: "idle"})
}

func syncAdoptedRigHooks(townRoot, name string) {
	ensureHooksBase()
	if err := syncRigHooks(townRoot, name); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to sync hooks for adopted rig: %v\n", err)
	}
}

func reportAdoptedRig(name string, result *rig.RegisterRigResult) {
	fmt.Printf("\n%s Rig %s adopted\n", style.Success.Render("✓"), name)
	if result.FromConfig {
		fmt.Printf("  %s Read configuration from existing config.json\n", style.Dim.Render("ℹ"))
	}
	fmt.Printf("  Repository: %s\n", result.GitURL)
	fmt.Printf("  Prefix: %s\n", result.BeadsPrefix)
	if result.DefaultBranch != "" {
		fmt.Printf("  Default branch: %s\n", result.DefaultBranch)
	}
}

func runRigAdopt(_ *cobra.Command, args []string) error {
	name := args[0]
	ctx, err := loadRigAdoptContext(name)
	if err != nil {
		return err
	}
	fmt.Printf("Adopting existing rig %s...\n", style.Bold.Render(name))
	result, err := registerAdoptedRig(ctx)
	if err != nil {
		return err
	}
	if err := configureAdoptedRig(ctx, result); err != nil {
		return err
	}
	townRoot := ctx.townRoot
	mgr := ctx.mgr

	// Check for tracked beads and initialize database if missing (Issue #72)
	rigPath := filepath.Join(ctx.townRoot, name)
	if err := prepareAdoptedBeads(townRoot, rigPath, name, mgr, result); err != nil {
		return err
	}

	createAdoptedRigBeads(rigPath, name, result)

	// Auto-assign a namepool theme that doesn't collide with other rigs (gas-21k).
	autoAssignNamepoolTheme(townRoot, name, mgr)

	// Ensure hooks-base.json exists and sync hooks for the adopted rig.
	syncAdoptedRigHooks(townRoot, name)

	reportAdoptedRig(name, result)

	return nil
}

func rigBeadsDirCandidates(rigPath string) []string {
	return []string{
		filepath.Join(rigPath, ".beads"),
		filepath.Join(rigPath, "mayor", "rig", ".beads"),
	}
}

func listExistingRigBeadsDirs(rigPath string) []string {
	var existing []string
	for _, beadsDir := range rigBeadsDirCandidates(rigPath) {
		if _, err := os.Stat(beadsDir); err == nil {
			existing = append(existing, beadsDir)
		}
	}
	return existing
}

func adoptedRigBeadsPlan(existing []string, beadsPrefix string) (dirToHandle string, initFresh bool) {
	if len(existing) > 0 {
		return existing[0], false
	}
	return "", beadsPrefix != ""
}

func resolveRigResetRole(cwd, townRoot string) (string, error) {
	if rigState().resetRole != "" {
		return rigState().resetRole, nil
	}
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return "", fmt.Errorf("detecting role: %w", err)
	}
	if roleInfo.Role == RoleUnknown {
		return "", fmt.Errorf("could not detect role; use --role to specify")
	}
	return string(roleInfo.Role), nil
}

func resetRigHandoff(townBd *beads.Beads, roleKey string) error {
	if err := townBd.ClearHandoffContent(roleKey); err != nil {
		return fmt.Errorf("clearing handoff content: %w", err)
	}
	fmt.Printf("%s Cleared handoff content for %s\n", style.Success.Render("✓"), roleKey)
	return nil
}

func resetRigMail(townBd *beads.Beads) error {
	result, err := townBd.ClearMail("Cleared during reset")
	if err != nil {
		return fmt.Errorf("clearing mail: %w", err)
	}
	if result.Closed > 0 || result.Cleared > 0 {
		fmt.Printf("%s Cleared mail: %d closed, %d pinned cleared\n",
			style.Success.Render("✓"), result.Closed, result.Cleared)
	} else {
		fmt.Printf("%s No mail to clear\n", style.Success.Render("✓"))
	}
	return nil
}

func maybeResetRigHandoff(resetAll bool, townBd *beads.Beads, roleKey string) error {
	if !resetAll && !rigState().resetHandoff {
		return nil
	}
	return resetRigHandoff(townBd, roleKey)
}

func maybeResetRigMail(resetAll bool, townBd *beads.Beads) error {
	if !resetAll && !rigState().resetMail {
		return nil
	}
	return resetRigMail(townBd)
}

func maybeResetRigStale(resetAll bool, rigBd *beads.Beads) error {
	if !resetAll && !rigState().resetStale {
		return nil
	}
	if err := runResetStale(rigBd, rigState().resetDryRun); err != nil {
		return fmt.Errorf("resetting stale issues: %w", err)
	}
	return nil
}

func runRigReset(_ *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}
	roleKey, err := resolveRigResetRole(cwd, townRoot)
	if err != nil {
		return err
	}
	state := rigState()
	resetAll := !state.resetHandoff && !state.resetMail && !state.resetStale
	townBd := beads.New(townRoot)
	rigBd := beads.New(cwd)
	if err := maybeResetRigHandoff(resetAll, townBd, roleKey); err != nil {
		return err
	}
	if err := maybeResetRigMail(resetAll, townBd); err != nil {
		return err
	}
	if err := maybeResetRigStale(resetAll, rigBd); err != nil {
		return err
	}
	return nil
}

// runResetStale resets in_progress issues whose assigned agent no longer has a session.
type staleIssueResult struct {
	reset             bool
	skippedPersistent bool
}

func staleIssueStatus(t *tmux.Tmux, issue *beads.Issue) (persistent, stale bool) {
	if issue.Assignee == "" {
		return false, false
	}
	sessionName, isPersistent := assigneeToSessionName(issue.Assignee)
	if sessionName == "" {
		return false, false
	}
	hasSession, err := t.HasSession(sessionName)
	if err != nil || hasSession {
		return false, false
	}
	return isPersistent, true
}

func resetStaleIssue(bd *beads.Beads, t *tmux.Tmux, issue *beads.Issue, dryRun bool) staleIssueResult {
	isPersistent, stale := staleIssueStatus(t, issue)
	if !stale {
		return staleIssueResult{}
	}
	if isPersistent {
		if dryRun {
			fmt.Printf("  %s: %s %s\n",
				style.Dim.Render(issue.ID),
				issue.Assignee,
				style.Dim.Render("(persistent, skipped)"))
		}
		return staleIssueResult{skippedPersistent: true}
	}
	if dryRun {
		fmt.Printf("  %s: %s (no session) → open\n",
			style.Bold.Render(issue.ID),
			issue.Assignee)
		return staleIssueResult{reset: true}
	}
	openStatus := "open"
	emptyAssignee := ""
	if err := bd.Update(issue.ID, beads.UpdateOptions{
		Status:   &openStatus,
		Assignee: &emptyAssignee,
	}); err != nil {
		fmt.Printf("  %s Failed to reset %s: %v\n",
			style.Warning.Render("⚠"),
			issue.ID, err)
		return staleIssueResult{}
	}
	return staleIssueResult{reset: true}
}

func reportStaleReset(dryRun bool, resetCount, skippedCount int, resetIssues []string) {
	if dryRun {
		if resetCount > 0 || skippedCount > 0 {
			fmt.Printf("\n%s Would reset %d issues, skip %d persistent\n",
				style.Dim.Render("(dry-run)"),
				resetCount, skippedCount)
		} else {
			fmt.Printf("%s No stale issues found\n", style.Success.Render("✓"))
		}
		return
	}
	if resetCount > 0 {
		fmt.Printf("%s Reset %d stale issues: %v\n",
			style.Success.Render("✓"),
			resetCount, resetIssues)
	} else {
		fmt.Printf("%s No stale issues to reset\n", style.Success.Render("✓"))
	}
	if skippedCount > 0 {
		fmt.Printf("  Skipped %d persistent (crew) issues\n", skippedCount)
	}
}

func runResetStale(bd *beads.Beads, dryRun bool) error {
	issues, err := bd.List(beads.ListOptions{Status: "in_progress", Priority: -1})
	if err != nil {
		return fmt.Errorf("listing in_progress issues: %w", err)
	}
	if len(issues) == 0 {
		fmt.Printf("%s No in_progress issues found\n", style.Success.Render("✓"))
		return nil
	}

	t := tmux.NewTmux()
	var resetCount, skippedCount int
	var resetIssues []string
	for _, issue := range issues {
		result := resetStaleIssue(bd, t, issue, dryRun)
		if result.reset {
			resetCount++
			resetIssues = append(resetIssues, issue.ID)
		}
		if result.skippedPersistent {
			skippedCount++
		}
	}
	reportStaleReset(dryRun, resetCount, skippedCount, resetIssues)
	return nil
}

// assigneeToSessionName converts an assignee (rig/name, rig/crew/name, or rig/polecats/name)
// to tmux session name.
// Returns the session name and whether this is a persistent identity (crew).
func assigneeToSessionName(assignee string) (sessionName string, isPersistent bool) {
	parts := strings.Split(assignee, "/")

	switch len(parts) {
	case 2:
		// rig/polecatName -> gt-rig-polecatName
		return session.PolecatSessionName(session.PrefixFor(parts[0]), parts[1]), false
	case 3:
		// rig/crew/name -> gt-rig-crew-name
		if parts[1] == "crew" {
			return session.CrewSessionName(session.PrefixFor(parts[0]), parts[2]), true
		}
		// rig/polecats/name -> gt-rig-name
		if parts[1] == "polecats" {
			return session.PolecatSessionName(session.PrefixFor(parts[0]), parts[2]), false
		}
		// Other 3-part formats not recognized
		return "", false
	default:
		return "", false
	}
}

// Helper to check if path exists
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isAgentSessionHealthy(t *tmux.Tmux, sessionName string) bool {
	return t.CheckSessionHealth(sessionName, 0) == tmux.SessionHealthy
}

func resolveRigForBoot(rigName string) (string, *rig.Rig, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		return "", nil, fmt.Errorf("rig '%s' not found", rigName)
	}
	return townRoot, r, nil
}

func startRigBootServices(r *rig.Rig) ([]string, []string, error) {
	var started []string
	var skipped []string
	// Start() treats healthy sessions as already running and recreates zombie
	// sessions whose tmux pane remains after the agent exits.
	witMgr := witness.NewManager(r)
	if err := witMgr.Start(false, "", nil); err != nil {
		if err == witness.ErrAlreadyRunning {
			skipped = append(skipped, "witness (already running)")
		} else {
			return nil, nil, fmt.Errorf("starting witness: %w", err)
		}
	} else {
		started = append(started, "witness")
	}

	refMgr := refinery.NewManager(r)
	if err := refMgr.Start(false, ""); err != nil { // false = background mode
		if errors.Is(err, refinery.ErrAlreadyRunning) {
			skipped = append(skipped, "refinery (already running)")
		} else if errors.Is(err, refinery.ErrForkRig) {
			skipped = append(skipped, "refinery (fork-backed rig; use PR workflow)")
		} else {
			return nil, nil, fmt.Errorf("starting refinery: %w", err)
		}
	} else {
		started = append(started, "refinery")
	}
	return started, skipped, nil
}

func runRigBoot(_ *cobra.Command, args []string) error {
	rigName := args[0]
	townRoot, r, err := resolveRigForBoot(rigName)
	if err != nil {
		return err
	}
	if blocked, reason := IsRigParkedOrDocked(townRoot, rigName); blocked {
		return fmt.Errorf("rig '%s' is %s - use 'gt rig unpark' or 'gt rig undock' first", rigName, reason)
	}

	fmt.Printf("Booting rig %s...\n", style.Bold.Render(rigName))
	started, skipped, err := startRigBootServices(r)
	if err != nil {
		return err
	}

	if len(started) > 0 {
		fmt.Printf("%s Started: %s\n", style.Success.Render("✓"), strings.Join(started, ", "))
	}
	if len(skipped) > 0 {
		fmt.Printf("%s Skipped: %s\n", style.Dim.Render("•"), strings.Join(skipped, ", "))
	}

	return nil
}

func loadRigManagerForStart() (string, *rig.Manager, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}
	g := git.NewGit(townRoot)
	return townRoot, rig.NewManager(townRoot, rigsConfig, g), nil
}

func startRigServicesForStart(r *rig.Rig) ([]string, []string, bool) {
	var started []string
	var skipped []string
	hasError := false
	// Start() treats healthy sessions as already running and recreates zombie
	// sessions whose tmux pane remains after the agent exits.
	witMgr := witness.NewManager(r)
	if err := witMgr.Start(false, "", nil); err != nil {
		if err == witness.ErrAlreadyRunning {
			skipped = append(skipped, "witness")
		} else {
			fmt.Printf("  %s Failed to start witness: %v\n", style.Warning.Render("⚠"), err)
			hasError = true
		}
	} else {
		started = append(started, "witness")
	}

	refMgr := refinery.NewManager(r)
	if err := refMgr.Start(false, ""); err != nil {
		if errors.Is(err, refinery.ErrAlreadyRunning) {
			skipped = append(skipped, "refinery")
		} else if errors.Is(err, refinery.ErrForkRig) {
			skipped = append(skipped, "refinery (fork-backed rig; use PR workflow)")
		} else {
			fmt.Printf("  %s Failed to start refinery: %v\n", style.Warning.Render("⚠"), err)
			hasError = true
		}
	} else {
		started = append(started, "refinery")
	}
	return started, skipped, hasError
}

func startRigByName(townRoot string, rigMgr *rig.Manager, rigName string) (found, failed bool) {
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		fmt.Printf("%s Rig '%s' not found\n", style.Warning.Render("⚠"), rigName)
		return false, true
	}
	if blocked, reason := IsRigParkedOrDocked(townRoot, rigName); blocked {
		fmt.Printf("%s Rig '%s' is %s - skipping (use 'gt rig unpark' or 'gt rig undock' first)\n",
			style.Warning.Render("⚠"), rigName, reason)
		return false, false
	}

	fmt.Printf("Starting rig %s...\n", style.Bold.Render(rigName))
	started, skipped, hasError := startRigServicesForStart(r)
	if len(started) > 0 {
		fmt.Printf("  %s Started: %s\n", style.Success.Render("✓"), strings.Join(started, ", "))
	}
	if len(skipped) > 0 {
		fmt.Printf("  %s Skipped: %s\n", style.Dim.Render("•"), strings.Join(skipped, ", "))
	}
	fmt.Println()
	return true, hasError
}

func runRigStart(_ *cobra.Command, args []string) error {
	townRoot, rigMgr, err := loadRigManagerForStart()
	if err != nil {
		return err
	}
	var successRigs []string
	var failedRigs []string
	for _, rigName := range args {
		found, failed := startRigByName(townRoot, rigMgr, rigName)
		if failed {
			failedRigs = append(failedRigs, rigName)
		} else if found {
			successRigs = append(successRigs, rigName)
		}
	}

	// Summary
	if len(successRigs) > 0 {
		fmt.Printf("%s Started rigs: %s\n", style.Success.Render("✓"), strings.Join(successRigs, ", "))
	}
	if len(failedRigs) > 0 {
		fmt.Printf("%s Failed rigs: %s\n", style.Warning.Render("⚠"), strings.Join(failedRigs, ", "))
		return fmt.Errorf("some rigs failed to start")
	}

	return nil
}

func stopRigPolecats(r *rig.Rig, force bool) []string {
	t := tmux.NewTmux()
	polecatMgr := polecat.NewSessionManager(t, r)
	infos, err := polecatMgr.ListPolecats()
	if err == nil && len(infos) > 0 {
		fmt.Printf("  Stopping %d polecat session(s)...\n", len(infos))
		if err := polecatMgr.StopAll(force); err != nil {
			return []string{fmt.Sprintf("polecat sessions: %v", err)}
		}
	}
	return nil
}

func stopRigRefinery(r *rig.Rig) []string {
	refMgr := refinery.NewManager(r)
	if running, _ := refMgr.IsRunning(); running {
		fmt.Printf("  Stopping refinery...\n")
		if err := refMgr.Stop(); err != nil {
			return []string{fmt.Sprintf("refinery: %v", err)}
		}
	}
	return nil
}

func stopRigWitness(r *rig.Rig) []string {
	witMgr := witness.NewManager(r)
	if running, _ := witMgr.IsRunning(); running {
		fmt.Printf("  Stopping witness...\n")
		if err := witMgr.Stop(); err != nil {
			return []string{fmt.Sprintf("witness: %v", err)}
		}
	}
	return nil
}

func stopRigAgents(r *rig.Rig, force bool) []string {
	var errors []string
	errors = append(errors, stopRigPolecats(r, force)...)
	errors = append(errors, stopRigRefinery(r)...)
	errors = append(errors, stopRigWitness(r)...)
	return errors
}

func reportRigShutdownErrors(errors []string) error {
	if len(errors) == 0 {
		return nil
	}
	fmt.Printf("\n%s Some agents failed to stop:\n", style.Warning.Render("⚠"))
	for _, e := range errors {
		fmt.Printf("  - %s\n", e)
	}
	return fmt.Errorf("shutdown incomplete")
}

func runRigShutdown(_ *cobra.Command, args []string) error {
	rigName := args[0]
	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}
	state := rigState()
	if !state.shutdownNuclear && !checkUncommittedWork(r, rigName, "shutdown", state.shutdownForce) {
		return fmt.Errorf("refusing to shutdown with uncommitted work")
	}

	fmt.Printf("Shutting down rig %s...\n", style.Bold.Render(rigName))
	if err := reportRigShutdownErrors(stopRigAgents(r, state.shutdownForce)); err != nil {
		return err
	}

	fmt.Printf("%s Rig %s shut down successfully\n", style.Success.Render("✓"), rigName)
	return nil
}

func runRigReboot(cmd *cobra.Command, args []string) error {
	rigName := args[0]

	fmt.Printf("Rebooting rig %s...\n\n", style.Bold.Render(rigName))

	// Propagate reboot flags to shutdown state.
	state := rigState()
	state.shutdownForce = state.rebootForce
	state.shutdownNuclear = state.rebootNuclear

	// Shutdown first
	if err := runRigShutdown(cmd, args); err != nil {
		// If shutdown fails due to uncommitted work, propagate the error
		return err
	}

	fmt.Println() // Blank line between shutdown and boot

	// Boot
	if err := runRigBoot(cmd, args); err != nil {
		return fmt.Errorf("boot failed: %w", err)
	}

	fmt.Printf("\n%s Rig %s rebooted successfully\n", style.Success.Render("✓"), rigName)
	return nil
}

type rigStatusData struct {
	witnessRunning  bool
	refineryRunning bool
	refineryQueue   []refinery.QueueItem
	polecats        []*polecat.Polecat
	polecatsErr     error
	crewWorkers     []*crew.CrewWorker
	crewErr         error
}

type rigStatusPolecatInfo struct {
	name       string
	state      polecat.State
	issue      string
	hasSession bool
}

type rigStatusCrewInfo struct {
	name       string
	hasSession bool
	branch     string
	dirty      bool
}

func resolveRigStatusName(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	roleInfo, err := GetRole()
	if err != nil {
		return "", fmt.Errorf("detecting rig from current directory: %w", err)
	}
	if roleInfo.Rig == "" {
		return "", fmt.Errorf("could not detect rig from current directory; please specify rig name")
	}
	return roleInfo.Rig, nil
}

func renderRigOperationalStatus(opState, opSource string) {
	if opState == "OPERATIONAL" {
		fmt.Printf("  Status: %s\n", style.Success.Render(opState))
	} else if opState == "PARKED" {
		fmt.Printf("  Status: %s (%s)\n", style.Warning.Render(opState), opSource)
	} else if opState == "DOCKED" {
		fmt.Printf("  Status: %s (%s)\n", style.Dim.Render(opState), opSource)
	}
}

func renderRigStatusHeader(townRoot, rigName string, r *rig.Rig) {
	fmt.Printf("%s\n", style.Bold.Render(rigName))
	opState, opSource := getRigOperationalState(townRoot, rigName)
	renderRigOperationalStatus(opState, opSource)
	fmt.Printf("  Path: %s\n", r.Path)
	if r.Config != nil && r.Config.Prefix != "" {
		fmt.Printf("  Beads prefix: %s-\n", r.Config.Prefix)
	}
	fmt.Println()
}

func gatherRigStatusData(townRoot string, r *rig.Rig, t *tmux.Tmux) rigStatusData {
	var data rigStatusData
	var wg sync.WaitGroup
	witMgr := witness.NewManager(r)
	wg.Add(1)
	go func() {
		defer wg.Done()
		data.witnessRunning, _ = witMgr.IsRunning()
	}()
	refMgr := refinery.NewManager(r)
	wg.Add(1)
	go func() {
		defer wg.Done()
		data.refineryRunning, _ = refMgr.IsRunning()
		if data.refineryRunning {
			data.refineryQueue, _ = refMgr.Queue()
		}
	}()
	polecatMgr := polecat.NewManager(r, git.NewGit(r.Path), t)
	wg.Add(1)
	go func() {
		defer wg.Done()
		data.polecats, data.polecatsErr = polecatMgr.List()
	}()
	crewMgr := crew.NewManager(r, git.NewGit(townRoot))
	wg.Add(1)
	go func() {
		defer wg.Done()
		data.crewWorkers, data.crewErr = crewMgr.List()
	}()
	wg.Wait()
	return data
}

func collectRigStatusSessions(rigName string, t *tmux.Tmux, data rigStatusData) ([]rigStatusPolecatInfo, []rigStatusCrewInfo) {
	var pInfos []rigStatusPolecatInfo
	var cInfos []rigStatusCrewInfo
	var wg sync.WaitGroup
	if data.polecatsErr == nil && len(data.polecats) > 0 {
		pInfos = make([]rigStatusPolecatInfo, len(data.polecats))
		for i, p := range data.polecats {
			pInfos[i] = rigStatusPolecatInfo{name: p.Name, state: p.State, issue: p.Issue}
			wg.Add(1)
			go func(idx int, p *polecat.Polecat) {
				defer wg.Done()
				sessionName := session.PolecatSessionName(session.PrefixFor(rigName), p.Name)
				pInfos[idx].hasSession = isAgentSessionHealthy(t, sessionName)
			}(i, p)
		}
	}
	if data.crewErr == nil && len(data.crewWorkers) > 0 {
		cInfos = make([]rigStatusCrewInfo, len(data.crewWorkers))
		for i, w := range data.crewWorkers {
			cInfos[i] = rigStatusCrewInfo{name: w.Name}
			wg.Add(1)
			go func(idx int, w *crew.CrewWorker) {
				defer wg.Done()
				sessionName := crewSessionName(rigName, w.Name)
				cInfos[idx].hasSession = isAgentSessionHealthy(t, sessionName)
				crewGit := git.NewGit(w.ClonePath)
				cInfos[idx].branch, _ = crewGit.CurrentBranch()
				gitStatus, _ := crewGit.Status()
				if gitStatus != nil && !gitStatus.Clean {
					cInfos[idx].dirty = true
				}
			}(i, w)
		}
	}
	wg.Wait()
	return pInfos, cInfos
}

func renderRigStatusServices(data rigStatusData) {
	fmt.Printf("%s\n", style.Bold.Render("Witness"))
	if data.witnessRunning {
		fmt.Printf("  %s running\n", style.Success.Render("●"))
	} else {
		fmt.Printf("  %s stopped\n", style.Dim.Render("○"))
	}
	fmt.Println()
	fmt.Printf("%s\n", style.Bold.Render("Refinery"))
	if data.refineryRunning {
		fmt.Printf("  %s running\n", style.Success.Render("●"))
		if len(data.refineryQueue) > 0 {
			fmt.Printf("  Queue: %d items\n", len(data.refineryQueue))
		}
	} else {
		fmt.Printf("  %s stopped\n", style.Dim.Render("○"))
	}
	fmt.Println()
}

func rigStatusPolecatDisplay(pi rigStatusPolecatInfo) (string, string) {
	sessionIcon := style.Dim.Render("○")
	if pi.hasSession {
		sessionIcon = style.Success.Render("●")
	}
	displayState := pi.state
	if pi.hasSession && displayState == polecat.StateDone {
		displayState = polecat.StateWorking
	} else if !pi.hasSession && displayState == polecat.StateWorking {
		displayState = polecat.StateStalled
	}
	stateStr := string(displayState)
	if pi.issue != "" {
		stateStr = fmt.Sprintf("%s → %s", displayState, pi.issue)
	}
	return sessionIcon, stateStr
}

func renderRigStatusPolecats(data rigStatusData, pInfos []rigStatusPolecatInfo) {
	fmt.Printf("%s", style.Bold.Render("Polecats"))
	if data.polecatsErr != nil || len(data.polecats) == 0 {
		fmt.Printf(" (none)\n")
	} else {
		fmt.Printf(" (%d)\n", len(data.polecats))
		for _, pi := range pInfos {
			sessionIcon, stateStr := rigStatusPolecatDisplay(pi)
			fmt.Printf("  %s %s: %s\n", sessionIcon, pi.name, stateStr)
		}
	}
	fmt.Println()
}

func renderRigStatusCrew(data rigStatusData, cInfos []rigStatusCrewInfo) {
	fmt.Printf("%s", style.Bold.Render("Crew"))
	if data.crewErr != nil || len(data.crewWorkers) == 0 {
		fmt.Printf(" (none)\n")
		return
	}
	fmt.Printf(" (%d)\n", len(data.crewWorkers))
	for _, ci := range cInfos {
		sessionIcon := style.Dim.Render("○")
		if ci.hasSession {
			sessionIcon = style.Success.Render("●")
		}
		gitInfo := ""
		if ci.dirty {
			gitInfo = style.Warning.Render(" (dirty)")
		}
		fmt.Printf("  %s %s: %s%s\n", sessionIcon, ci.name, ci.branch, gitInfo)
	}
}

func runRigStatus(_ *cobra.Command, args []string) error {
	rigName, err := resolveRigStatusName(args)
	if err != nil {
		return err
	}
	townRoot, r, err := getRig(rigName)
	if err != nil {
		return err
	}
	t := tmux.NewTmux()
	renderRigStatusHeader(townRoot, rigName, r)
	data := gatherRigStatusData(townRoot, r, t)
	pInfos, cInfos := collectRigStatusSessions(rigName, t, data)
	renderRigStatusServices(data)
	renderRigStatusPolecats(data, pInfos)
	renderRigStatusCrew(data, cInfos)
	return nil
}

func stopRigByName(rigMgr *rig.Manager, rigName string, force, nuclear bool) (succeeded, failed bool) {
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		fmt.Printf("%s Rig '%s' not found\n", style.Warning.Render("⚠"), rigName)
		return false, true
	}
	if !nuclear && !checkUncommittedWork(r, rigName, "stop", force) {
		return false, true
	}
	fmt.Printf("Stopping rig %s...\n", style.Bold.Render(rigName))
	if errors := stopRigAgents(r, force); len(errors) > 0 {
		fmt.Printf("%s Some agents in %s failed to stop:\n", style.Warning.Render("⚠"), rigName)
		for _, e := range errors {
			fmt.Printf("  - %s\n", e)
		}
		return false, true
	}
	fmt.Printf("%s Rig %s stopped\n", style.Success.Render("✓"), rigName)
	return true, false
}

func reportRigStopSummary(args []string, succeeded, failed []string) error {
	if len(args) > 1 {
		fmt.Println()
		if len(succeeded) > 0 {
			fmt.Printf("%s Stopped: %s\n", style.Success.Render("✓"), strings.Join(succeeded, ", "))
		}
		if len(failed) > 0 {
			fmt.Printf("%s Failed: %s\n", style.Warning.Render("⚠"), strings.Join(failed, ", "))
			return fmt.Errorf("some rigs failed to stop")
		}
	} else if len(failed) > 0 {
		return fmt.Errorf("rig failed to stop")
	}
	return nil
}

func runRigStop(_ *cobra.Command, args []string) error {
	_, rigMgr, err := loadRigManagerForStart()
	if err != nil {
		return err
	}
	var succeeded []string
	var failed []string
	state := rigState()
	for _, rigName := range args {
		stopped, stopFailed := stopRigByName(rigMgr, rigName, state.stopForce, state.stopNuclear)
		if stopFailed {
			failed = append(failed, rigName)
		} else if stopped {
			succeeded = append(succeeded, rigName)
		}
	}
	return reportRigStopSummary(args, succeeded, failed)
}

func stopRigForRestart(r *rig.Rig, force bool) []string {
	fmt.Printf("  Stopping...\n")
	t := tmux.NewTmux()
	polecatMgr := polecat.NewSessionManager(t, r)
	var stopErrors []string
	infos, err := polecatMgr.ListPolecats()
	if err == nil && len(infos) > 0 {
		fmt.Printf("    Stopping %d polecat session(s)...\n", len(infos))
		if err := polecatMgr.StopAll(force); err != nil {
			stopErrors = append(stopErrors, fmt.Sprintf("polecat sessions: %v", err))
		}
	}
	refMgr := refinery.NewManager(r)
	if running, _ := refMgr.IsRunning(); running {
		fmt.Printf("    Stopping refinery...\n")
		if err := refMgr.Stop(); err != nil {
			stopErrors = append(stopErrors, fmt.Sprintf("refinery: %v", err))
		}
	}
	witMgr := witness.NewManager(r)
	if running, _ := witMgr.IsRunning(); running {
		fmt.Printf("    Stopping witness...\n")
		if err := witMgr.Stop(); err != nil {
			stopErrors = append(stopErrors, fmt.Sprintf("witness: %v", err))
		}
	}
	return stopErrors
}

func startRigForRestart(r *rig.Rig) ([]string, []string, []string) {
	fmt.Printf("  Starting...\n")
	var started []string
	var skipped []string
	var startErrors []string
	witMgr := witness.NewManager(r)
	if err := witMgr.Start(false, "", nil); err != nil {
		if err == witness.ErrAlreadyRunning {
			skipped = append(skipped, "witness")
		} else {
			fmt.Printf("    %s Failed to start witness: %v\n", style.Warning.Render("⚠"), err)
			startErrors = append(startErrors, fmt.Sprintf("witness: %v", err))
		}
	} else {
		started = append(started, "witness")
	}
	refMgr := refinery.NewManager(r)
	if err := refMgr.Start(false, ""); err != nil {
		if errors.Is(err, refinery.ErrAlreadyRunning) {
			skipped = append(skipped, "refinery")
		} else if errors.Is(err, refinery.ErrForkRig) {
			skipped = append(skipped, "refinery (fork-backed rig; use PR workflow)")
		} else {
			fmt.Printf("    %s Failed to start refinery: %v\n", style.Warning.Render("⚠"), err)
			startErrors = append(startErrors, fmt.Sprintf("refinery: %v", err))
		}
	} else {
		started = append(started, "refinery")
	}
	if len(started) > 0 {
		fmt.Printf("  %s Started: %s\n", style.Success.Render("✓"), strings.Join(started, ", "))
	}
	if len(skipped) > 0 {
		fmt.Printf("  %s Skipped: %s\n", style.Dim.Render("•"), strings.Join(skipped, ", "))
	}
	return started, skipped, startErrors
}

func reportRigRestartErrors(phase string, errors []string) {
	fmt.Printf("  %s %s errors:\n", style.Warning.Render("⚠"), phase)
	for _, e := range errors {
		fmt.Printf("    - %s\n", e)
	}
}

func restartRigByName(rigMgr *rig.Manager, rigName string, force, nuclear bool) (succeeded, failed bool) {
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		fmt.Printf("%s Rig '%s' not found\n", style.Warning.Render("⚠"), rigName)
		return false, true
	}
	if !nuclear && !checkUncommittedWork(r, rigName, "restart", force) {
		return false, true
	}
	fmt.Printf("Restarting rig %s...\n", style.Bold.Render(rigName))
	if stopErrors := stopRigForRestart(r, force); len(stopErrors) > 0 {
		reportRigRestartErrors("Stop", stopErrors)
		return false, true
	}
	_, _, startErrors := startRigForRestart(r)
	if len(startErrors) > 0 {
		reportRigRestartErrors("Start", startErrors)
		return false, true
	}
	fmt.Printf("%s Rig %s restarted\n", style.Success.Render("✓"), rigName)
	fmt.Println()
	return true, false
}

func reportRigRestartSummary(args, succeeded, failed []string) error {
	if len(args) > 1 {
		if len(succeeded) > 0 {
			fmt.Printf("%s Restarted: %s\n", style.Success.Render("✓"), strings.Join(succeeded, ", "))
		}
		if len(failed) > 0 {
			fmt.Printf("%s Failed: %s\n", style.Warning.Render("⚠"), strings.Join(failed, ", "))
			return fmt.Errorf("some rigs failed to restart")
		}
	} else if len(failed) > 0 {
		return fmt.Errorf("rig failed to restart")
	}
	return nil
}

func runRigRestart(_ *cobra.Command, args []string) error {
	_, rigMgr, err := loadRigManagerForStart()
	if err != nil {
		return err
	}
	var succeeded []string
	var failed []string
	state := rigState()
	for _, rigName := range args {
		restarted, restartFailed := restartRigByName(rigMgr, rigName, state.restartForce, state.restartNuclear)
		if restartFailed {
			failed = append(failed, rigName)
		} else if restarted {
			succeeded = append(succeeded, rigName)
		}
	}
	return reportRigRestartSummary(args, succeeded, failed)
}

// getRigOperationalState returns the operational state and source for a rig.
// It checks the wisp layer first (local/ephemeral), then rig bead labels (global).
// Returns state ("OPERATIONAL", "PARKED", or "DOCKED") and source ("local", "global - synced", or "default").
func rigOperationalStateForStatus(status string) (string, bool) {
	switch strings.ToLower(status) {
	case "parked":
		return "PARKED", true
	case "docked":
		return "DOCKED", true
	default:
		return "", false
	}
}

func localRigOperationalState(townRoot, rigName string) (string, bool) {
	status := wisp.NewConfig(townRoot, rigName).GetString("status")
	if status == "" {
		return "", false
	}
	state, ok := rigOperationalStateForStatus(status)
	return state, ok
}

func globalRigOperationalState(townRoot, rigName string) (string, bool) {
	rigPath := filepath.Join(townRoot, rigName)
	rigBeadsDir := beads.ResolveBeadsDir(rigPath)
	bd := beads.NewWithBeadsDir(rigPath, rigBeadsDir)
	var prefix string
	if rigCfg, err := rig.LoadRigConfig(rigPath); err == nil && rigCfg.Beads != nil {
		prefix = rigCfg.Beads.Prefix
	} else {
		// Fall back to registry (mayor/rigs.json) when config.json is missing
		prefix = config.GetRigPrefix(townRoot, rigName)
	}

	if prefix == "" {
		return "", false
	}
	rigBeadID := fmt.Sprintf("%s-rig-%s", prefix, rigName)
	issue, err := bd.Show(rigBeadID)
	if err != nil {
		return "", false
	}
	for _, label := range issue.Labels {
		if strings.HasPrefix(label, "status:") {
			state, ok := rigOperationalStateForStatus(strings.TrimPrefix(label, "status:"))
			if ok {
				return state, true
			}
		}
	}
	return "", false
}

func getRigOperationalState(townRoot, rigName string) (state string, source string) {
	if state, ok := localRigOperationalState(townRoot, rigName); ok {
		return state, "local"
	}
	if state, ok := globalRigOperationalState(townRoot, rigName); ok {
		return state, "global - synced"
	}
	return "OPERATIONAL", "default"
}

// ensureHooksBase creates ~/.gt/hooks-base.json from current defaults if it
// doesn't exist yet. Without this file, gt hooks diff has no reference point
// and cannot detect drift when default hooks change after initial setup.
// Non-fatal: missing base config is caught by gt doctor hooks-base-missing.
func ensureHooksBase() {
	if _, err := hooks.LoadBase(); err == nil {
		return // already exists
	}
	base := hooks.DefaultBase()
	if err := hooks.SaveBase(base); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not create hooks-base.json: %v\n", err)
		return
	}
	fmt.Printf("  Created hooks-base.json at %s\n", hooks.BasePath())
}

// syncRigHooks syncs hooks for a specific rig's targets after rig creation.
func syncRigHooks(townRoot, rigName string) error {
	targets, err := hooks.DiscoverTargets(townRoot)
	if err != nil {
		return err
	}

	synced := 0
	for _, target := range targets {
		if target.Rig != rigName {
			continue
		}
		if _, err := syncTarget(target, false); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: failed to sync hooks for %s: %v\n", target.DisplayKey(), err)
			continue
		}
		synced++
	}

	if synced > 0 {
		fmt.Printf("  Synced hooks for %d target(s)\n", synced)
	}
	return nil
}

// findRigSessions returns all tmux sessions belonging to the given rig.
// All rig sessions share the "<rigPrefix>-" prefix, so this catches witness,
// refinery, polecat, and crew sessions in one pass.
func findRigSessions(t *tmux.Tmux, rigName string) ([]string, error) {
	prefix := session.PrefixFor(rigName) + "-"
	all, err := t.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("listing tmux sessions: %w", err)
	}
	var matches []string
	for _, name := range all {
		if strings.HasPrefix(name, prefix) {
			matches = append(matches, name)
		}
	}
	return matches, nil
}

// commitTownConfigChanges commits town-level config files (rigs.json, daemon.json,
// routes.jsonl) to the town repo after rig add/adopt. Without this commit, changes
// are silently reverted by any process that does a git restore/checkout.
func commitTownConfigChanges(townRoot, rigName string) {
	g := git.NewGit(townRoot)

	// Collect the town-level files that rig add/adopt modifies.
	files := []string{
		filepath.Join("mayor", "rigs.json"),
		filepath.Join("mayor", "daemon.json"),
		filepath.Join(".beads", "routes.jsonl"),
	}

	// Only stage files that actually exist (adopt may not touch all of them).
	var toAdd []string
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(townRoot, f)); err == nil {
			toAdd = append(toAdd, f)
		}
	}
	if len(toAdd) == 0 {
		return
	}

	if err := g.Add(toAdd...); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not stage town config files: %v\n", err)
		return
	}

	msg := fmt.Sprintf("chore: register rig %s in town config", rigName)
	if err := g.Commit(msg); err != nil {
		// If nothing changed (already committed), git commit returns an error — that's fine.
		if !strings.Contains(err.Error(), "nothing to commit") {
			fmt.Fprintf(os.Stderr, "  Warning: could not commit town config files: %v\n", err)
		}
	}
}

// warnThirdPartyRigAdd tells the user that a public third-party clone will
// push to origin unless they pass --push-url or use --merge=local.
func warnThirdPartyRigAdd(gitURL, pushURL string) {
	msg := thirdPartyRigAddWarning(gitURL, pushURL)
	if msg == "" {
		return
	}
	style.PrintWarning("%s", msg)
}

func thirdPartyRigAddWarning(gitURL, pushURL string) string {
	if strings.TrimSpace(pushURL) != "" {
		return ""
	}
	if !looksLikeHostedGitRemote(gitURL) {
		return ""
	}
	return "default merge will push to origin. For a repo you do not own, pass --push-url <your-fork> or use --merge=local. See docs/guides/fork-rig-setup.md"
}

func looksLikeHostedGitRemote(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return false
	}
	return strings.Contains(s, "github.com/") ||
		strings.Contains(s, "gitlab.com/") ||
		strings.Contains(s, "bitbucket.org/") ||
		strings.Contains(s, "codeberg.org/")
}

// isGitRemoteURL returns true if s looks like a remote git URL rather than a
// local path. Accepts any scheme:// URL (including file:// for explicit local
// mirrors) as well as SCP-style SSH URLs.
func isGitRemoteURL(s string) bool {
	if isLocalGitPath(s) {
		return false
	}
	if hasValidGitURLScheme(s) {
		return true
	}
	return isSCPStyleSSHURL(s)
}

func isLocalGitPath(s string) bool {
	return strings.HasPrefix(s, "-") ||
		strings.HasPrefix(s, "/") ||
		(len(s) >= 3 && s[1] == ':' && (s[2] == '/' || s[2] == '\\')) ||
		strings.HasPrefix(s, "./") ||
		strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "~/")
}

func hasValidGitURLScheme(s string) bool {
	idx := strings.Index(s, "://")
	if idx <= 0 {
		return false
	}
	for _, c := range s[:idx] {
		if !isGitURLSchemeRune(c) {
			return false
		}
	}
	return true
}

func isGitURLSchemeRune(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'
}

func isSCPStyleSSHURL(s string) bool {
	// Accept SCP-style SSH URLs (user@host:path) where user and host are non-empty
	// and host contains no slashes (distinguishes from file:// or path-like strings)
	atIdx := strings.Index(s, "@")
	colonIdx := strings.Index(s, ":")
	return atIdx > 0 && colonIdx > atIdx+1 && !strings.Contains(s[:colonIdx], "/")
}

// autoAssignNamepoolTheme picks a namepool theme for a new rig that doesn't collide
// with themes already in use by other rigs. This ensures polecat names are unique
// across rigs (gas-21k). If all built-in themes are taken, falls back to hash-based
// selection where collisions are possible but unavoidable.
func autoAssignNamepoolTheme(townRoot, rigName string, mgr *rig.Manager) {
	usedThemes := mgr.UsedNamepoolThemes(polecat.ThemeForRig)
	chosenTheme := polecat.ThemeForRigAvoiding(rigName, usedThemes)
	settingsPath := filepath.Join(townRoot, rigName, "settings", "config.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		fmt.Printf("  %s Could not create settings directory: %v\n", style.Warning.Render("!"), err)
		return
	}
	rigSettings, err := config.LoadRigSettings(settingsPath)
	if err != nil {
		rigSettings = &config.RigSettings{
			Type:    "rig-settings",
			Version: 1,
		}
	}
	// Only set namepool theme if not already configured
	if rigSettings.Namepool != nil && rigSettings.Namepool.Style != "" {
		return
	}
	rigSettings.Namepool = &config.NamepoolConfig{
		Style: chosenTheme,
	}
	if err := config.SaveRigSettings(settingsPath, rigSettings); err != nil {
		fmt.Printf("  %s Could not save namepool theme: %v\n", style.Warning.Render("!"), err)
	} else {
		fmt.Printf("  Namepool theme: %s (auto-assigned for cross-rig uniqueness)\n", chosenTheme)
	}
}
