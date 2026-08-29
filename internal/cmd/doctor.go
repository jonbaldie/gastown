package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/jonbaldie/gastown/internal/doctor"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:     "doctor",
	GroupID: GroupDiag,
	Short:   "Run health checks on the workspace",
	Long: `Run diagnostic checks on the Gas Town workspace.

Doctor checks for common configuration issues, missing files,
and other problems that could affect workspace operation.

Workspace checks:
  - town-config-exists       Check mayor/town.json exists
  - town-config-valid        Check mayor/town.json is valid
  - rigs-registry-exists     Check mayor/rigs.json exists (fixable)
  - rigs-registry-valid      Check registered rigs exist (fixable)
  - mayor-exists             Check mayor/ directory structure
  - disk-space               Check filesystem has sufficient free space

Town root protection:
  - town-git                 Verify town root is under version control
  - town-root-branch         Verify town root is on main branch (fixable)
  - foreign-remotes          Detect git remotes from unrelated repos (fixable)
  - pre-checkout-hook        Verify pre-checkout hook prevents branch switches (fixable)

Infrastructure checks:
  - stale-binary             Check if gt binary is up to date with repo
  - beads-binary             Check that beads (bd) is installed and meets minimum version
  - mayor-binary             Check that the configured Mayor agent binary is on PATH
  - daemon                   Check if daemon is running (fixable)
  - boot-health              Check Boot watchdog health (vet mode)
  - town-beads-config        Verify town .beads/config.yaml exists (fixable)
  - beads-dir-perms          Verify .beads directories are mode 0700 (fixable)

Cleanup checks (fixable):
  - orphan-sessions          Detect orphaned tmux sessions
  - stalled-polecats         Detect polecats with dead sessions and unpushed work (fixable)
  - orphan-processes         Detect orphaned Claude processes
  - session-name-format      Detect sessions with outdated naming format (fixable)
  - wisp-gc                  Detect and clean abandoned wisps (>1h)
  - misclassified-wisps      Detect issues that should be wisps (purges to wisps table, fixable)
  - jsonl-bloat              Detect stale/bloated issues.jsonl vs live database
  - stale-beads-redirect     Detect stale files in .beads directories with redirects

Clone divergence checks:
  - persistent-role-branches Detect witness/refinery not on main (excludes crew)
  - clone-divergence         Detect clones significantly behind origin/main
  - default-branch-all-rigs  Verify default_branch exists on remote for all rigs (fixable)
  - worktree-gitdir-valid    Verify worktree .git files reference existing paths (fixable)

Crew workspace checks:
  - crew-state               Validate crew worker state.json files (fixable)
  - crew-worktrees           Detect stale cross-rig worktrees (fixable)

Migration checks (fixable):
  - sparse-checkout          Detect legacy sparse checkout across all rigs

Rig checks (with --rig flag):
  - rig-is-git-repo          Verify rig is a valid git repository
  - git-exclude-configured   Check .git/info/exclude has Gas Town dirs (fixable)
  - bare-repo-exists         Verify .repo.git exists when worktrees depend on it (fixable)
  - witness-exists           Verify witness/ structure exists (fixable)
  - refinery-exists          Verify refinery/ structure exists (fixable)
  - mayor-clone-exists       Verify mayor/rig/ clone exists (fixable)
  - polecat-clones-valid     Verify polecat directories are valid clones
  - beads-config-valid       Verify beads configuration (fixable)

Routing checks (fixable):
  - routes-config            Check beads routing configuration
  - prefix-mismatch          Detect rigs.json vs routes.jsonl prefix mismatches (fixable)
  - database-prefix          Detect database vs routes.jsonl prefix mismatches (fixable)

Lifecycle checks (fixable):
  - lifecycle-defaults          Ensure daemon.json has all lifecycle patrol entries (fixable)

Formula overlay checks (fixable):
  - overlay-health           Check formula overlay step IDs are valid (fixable)

Migration checks:
  - town-agents-md           Check town-root AGENTS.md identity pair matches embedded version (fixable)

Session hook checks:
  - session-hooks            Check settings.json use session-start.sh
  - claude-settings          Check Claude settings.json match templates (fixable)
  - deprecated-merge-queue-keys  Detect stale deprecated keys in merge_queue config (fixable)
  - stale-task-dispatch      Detect stale task-dispatch guard in settings.json (fixable)

Dolt checks:
  - dolt-binary              Check that dolt is installed and meets minimum version
  - dolt-metadata            Check dolt metadata tables exist
  - dolt-server-reachable    Check dolt sql-server is reachable
  - dolt-orphaned-databases  Detect orphaned dolt databases

Patrol checks:
  - patrol-molecules-exist   Verify patrol molecules exist
  - patrol-hooks-wired       Verify daemon triggers patrols
  - patrol-not-stuck         Detect stale wisps (>1h)
  - patrol-plugins-accessible Verify plugin directories

Use --fix to attempt automatic fixes for issues that support it.
Use --no-start with --fix to suppress starting the daemon and agents.
Use --rig to check a specific rig instead of the entire workspace.
Use --slow to highlight slow checks (default threshold: 1s, e.g. --slow=500ms).`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().Bool("fix", false, "Attempt to automatically fix issues")
	doctorCmd.Flags().BoolP("verbose", "v", false, "Show detailed output")
	doctorCmd.Flags().String("rig", "", "Check specific rig only")
	doctorCmd.Flags().Bool("restart-sessions", false, "Restart patrol sessions when fixing stale settings (use with --fix)")
	doctorCmd.Flags().Bool("no-start", false, "Suppress starting daemon/agents during --fix")
	doctorCmd.Flags().String("slow", "", "Highlight slow checks (optional threshold, default 1s)")
	// Allow --slow without a value (uses default 1s)
	doctorCmd.Flags().Lookup("slow").NoOptDefVal = "1s"
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	fix := commandBoolFlag(cmd, "fix")
	verbose := commandBoolFlag(cmd, "verbose")
	rig := commandStringFlag(cmd, "rig")
	restartSessions := commandBoolFlag(cmd, "restart-sessions")
	noStart := commandBoolFlag(cmd, "no-start")
	slow := commandStringFlag(cmd, "slow")

	// Find town root
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Create check context
	ctx := &doctor.CheckContext{
		TownRoot:        townRoot,
		RigName:         rig,
		Verbose:         verbose,
		RestartSessions: restartSessions,
		NoStart:         noStart,
	}

	d := newDoctorForCommand(rig)

	// Parse slow threshold (0 = disabled)
	var slowThreshold time.Duration
	if slow != "" {
		var err error
		slowThreshold, err = time.ParseDuration(slow)
		if err != nil {
			return fmt.Errorf("invalid --slow duration %q: %w", slow, err)
		}
	}

	// Run checks with streaming output
	fmt.Println() // Initial blank line
	var report *doctor.Report
	if fix {
		report = d.FixStreaming(ctx, os.Stdout, slowThreshold)
	} else {
		report = d.RunStreaming(ctx, os.Stdout, slowThreshold)
	}

	// Print summary (checks were already printed during streaming)
	report.PrintSummaryOnly(os.Stdout, verbose, slowThreshold)

	// Exit with error code if there are errors
	if report.HasErrors() {
		return fmt.Errorf("doctor found %d error(s)", report.Summary.Errors)
	}

	return nil
}

func newDoctorForCommand(rig string) *doctor.Doctor {
	d := doctor.NewDoctor()
	registerDoctorFoundationChecks(d)
	registerDoctorInfrastructureChecks(d)
	registerDoctorTownChecks(d)
	registerDoctorPatrolChecks(d)
	registerDoctorConfigChecks(d)
	registerDoctorWorkspaceChecks(d)
	registerDoctorLifecycleChecks(d)
	registerDoctorDataChecks(d)
	if rig != "" {
		d.RegisterAll(doctor.RigChecks()...)
	}
	return d
}

func registerDoctorFoundationChecks(d *doctor.Doctor) {
	// Register workspace-level checks first (fundamental)
	d.RegisterAll(doctor.WorkspaceChecks()...)

	d.Register(doctor.NewGlobalStateCheck())

	// Disk space check — most fundamental resource check. Low disk space is the
	// root cause of cascading failures (Dolt data loss, polecat death, lost commits).
	// Must run before infrastructure checks that might fail confusingly on full disks.
	d.Register(doctor.NewDiskSpaceCheck())

	// Infrastructure prerequisites — these must pass before any check that
	// shells out to bd/dolt or queries the database. Order matters:
	// 1. gt binary freshness
	// 2. bd binary exists
	// 3. dolt binary exists
	// 4. Dolt server is reachable (everything downstream depends on this)
	d.Register(doctor.NewStaleBinaryCheck())
	d.Register(doctor.NewBeadsBinaryCheck())
	d.Register(doctor.NewDoltBinaryCheck())
	d.Register(doctor.NewClaudeBinaryCheck())
	d.Register(doctor.NewMayorBinaryCheck())
	d.Register(doctor.NewGroqCompoundCheck())
	d.Register(doctor.NewDoltServerReachableCheck())
}

func registerDoctorInfrastructureChecks(d *doctor.Doctor) {
	// Keep git and daemon prerequisites together so failures remain easy to scan.
	d.Register(doctor.NewTownGitCheck())
	d.Register(doctor.NewTownRootBranchCheck())
	d.Register(doctor.NewForeignRemoteCheck())
	d.Register(doctor.NewPreCheckoutHookCheck())
	// Claude settings must be fixed BEFORE the daemon starts, so sessions
	// launched by the daemon find correct settings files. If daemon runs first,
	// its EnsureSettingsForRole sees stale files → returns early → sessions
	// start with missing PATH exports. See gt-99u.
	d.Register(doctor.NewClaudeSettingsCheck())
	d.Register(doctor.NewDaemonCheck())
	d.Register(doctor.NewTmuxGlobalEnvCheck())
	d.Register(doctor.NewBootHealthCheck())
	d.Register(doctor.NewTownBeadsConfigCheck())
	d.Register(doctor.NewBeadsDirPermsCheck())
	d.Register(doctor.NewCustomTypesCheck())
	d.Register(doctor.NewCustomStatusesCheck())
	d.Register(doctor.NewFormulaCheck())
	d.Register(doctor.NewOverlayHealthCheck())
	d.Register(doctor.NewPrefixConflictCheck())
	d.Register(doctor.NewRigNameMismatchCheck())
	d.Register(doctor.NewRigConfigSyncCheck())
	d.Register(doctor.NewStaleDoltPortCheck())
	d.Register(doctor.NewStaleSQLServerInfoCheck())
}

func registerDoctorTownChecks(d *doctor.Doctor) {
	d.Register(doctor.NewPrefixMismatchCheck())
	d.Register(doctor.NewDatabasePrefixCheck())
	d.Register(doctor.NewIdleTimeoutCheck())
	d.Register(doctor.NewRoutesCheck())
	d.Register(doctor.NewRigRoutesJSONLCheck())
	d.Register(doctor.NewRoutingModeCheck())
	d.Register(doctor.NewMalformedSessionNameCheck())
	d.Register(doctor.NewOrphanSessionCheck())
	d.Register(doctor.NewZombieSessionCheck())
	d.Register(doctor.NewBlockedPaneCheck())
	d.Register(doctor.NewStalledPolecatCheck())
	d.Register(doctor.NewOrphanProcessCheck())
	d.Register(doctor.NewWispGCCheck())
	d.Register(doctor.NewCheckMisclassifiedWisps())
	d.Register(doctor.NewCheckJSONLBloat())
	d.Register(doctor.NewStaleBeadsRedirectCheck())
	d.Register(doctor.NewBeadsRedirectTargetCheck())
	d.Register(doctor.NewStaleRuntimeFilesCheck())
	d.Register(doctor.NewBranchCheck())
	d.Register(doctor.NewCloneDivergenceCheck())
	d.Register(doctor.NewDefaultBranchAllRigsCheck())
	d.Register(doctor.NewIdentityCollisionCheck())
	d.Register(doctor.NewLinkedPaneCheck())
	d.Register(doctor.NewSocketSplitBrainCheck())
	d.Register(doctor.NewThemeCheck())
	d.Register(doctor.NewCrashReportCheck())
	d.Register(doctor.NewEnvVarsCheck())
}

func registerDoctorPatrolChecks(d *doctor.Doctor) {
	// Patrol system checks
	d.Register(doctor.NewPatrolMoleculesExistCheck())
	d.Register(doctor.NewPatrolHooksWiredCheck())
	d.Register(doctor.NewPatrolNotStuckCheck())
	d.Register(doctor.NewPatrolPluginsAccessibleCheck())
	d.Register(doctor.NewPatrolPluginDriftCheck())
	d.Register(doctor.NewAgentBeadsCheck())
	d.Register(doctor.NewStaleAgentBeadsCheck())
	d.Register(doctor.NewRigBeadsCheck())
	d.Register(doctor.NewRoleBeadsCheck())
	// Stale attachment detection belongs in the Deacon molecule.
}

func registerDoctorConfigChecks(d *doctor.Doctor) {
	// Config architecture checks
	d.Register(doctor.NewSettingsCheck())
	d.Register(doctor.NewSessionHookCheck())
	d.Register(doctor.NewRuntimeGitignoreCheck())
	d.Register(doctor.NewLegacyGastownCheck())
	// ClaudeSettingsCheck is registered before DaemonCheck above.
	d.Register(doctor.NewDeprecatedMergeQueueKeysCheck())
	d.Register(doctor.NewLandWorktreeGitignoreCheck())
	d.Register(doctor.NewHooksPathAllRigsCheck())

	// Sparse checkout migration runs across all rigs, not just --rig mode.
	d.Register(doctor.NewSparseCheckoutCheck())

	// Priming subsystem check
	d.Register(doctor.NewPrimingCheck())

	// Town-root CLAUDE.md version check (migration check for behavioral norms)
	d.Register(doctor.NewTownAgentsMDCheck())
}

func registerDoctorWorkspaceChecks(d *doctor.Doctor) {
	// Crew workspace checks
	d.Register(doctor.NewCrewStateCheck())
	d.Register(doctor.NewCrewWorktreeCheck())
	d.Register(doctor.NewCommandsCheck())
	d.Register(doctor.NewSkillsCheck())

	// Hook attachment checks
	d.Register(doctor.NewHookAttachmentValidCheck())
	d.Register(doctor.NewHookSingletonCheck())
	d.Register(doctor.NewOrphanedAttachmentsCheck())

	// Hooks sync check
	d.Register(doctor.NewStaleTaskDispatchCheck())
	d.Register(doctor.NewHooksSyncCheck())
	d.Register(doctor.NewHooksBaseCheck())

	// Worktree gitdir validity runs across all rigs, or a selected rig.
	d.Register(doctor.NewWorktreeGitdirCheck())
}

func registerDoctorLifecycleChecks(d *doctor.Doctor) {
	// Lifecycle hygiene checks
	d.Register(doctor.NewLifecycleHygieneCheck())
	d.Register(doctor.NewLifecycleDefaultsCheck())
}

func registerDoctorDataChecks(d *doctor.Doctor) {
	// Dolt data health checks. Binary and server checks run above as prerequisites.
	d.Register(doctor.NewDoltMetadataCheck())
	d.Register(doctor.NewDoltOrphanedDatabaseCheck())
	d.Register(doctor.NewUnregisteredBeadsDirsCheck())
	d.Register(doctor.NewNullAssigneeCheck())
}
