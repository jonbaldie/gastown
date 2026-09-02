package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/doctor"
	"github.com/jonbaldie/gastown/internal/formula"
	"github.com/jonbaldie/gastown/internal/hooks"
	"github.com/jonbaldie/gastown/internal/instructions"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/templates"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var upgradeCmd = &cobra.Command{
	Use:     "upgrade",
	GroupID: GroupDiag,
	Short:   "Run post-install migration and sync workspace state",
	Long: `Run post-binary-install migrations to bring the workspace up to date.

This is the user-facing entry point for upgrading Gas Town after installing
a new binary. It orchestrates all migration steps in the right order:

  1. Structural checks   Run gt doctor --fix to repair workspace structure
  2. CLAUDE.md sync       Update town root CLAUDE.md from embedded template
  3. Daemon defaults      Ensure daemon.json has lifecycle defaults
  4. Hooks sync           Regenerate settings.json from hook registry
  5. Formula update       Update formulas from embedded copies

Each step reports what changed. Use --dry-run to preview without modifying.

Examples:
  gt upgrade                  # Run all migration steps
  gt upgrade --dry-run        # Show what would change
  gt upgrade --verbose        # Show detailed output
  gt upgrade --no-start       # Suppress starting daemon during doctor fix`,
	RunE:         runUpgrade,
	SilenceUsage: true,
}

func init() {
	upgradeCmd.Flags().Bool("dry-run", false, "Show what would change without modifying anything")
	upgradeCmd.Flags().BoolP("verbose", "v", false, "Show detailed output")
	upgradeCmd.Flags().Bool("no-start", false, "Suppress starting daemon/agents during doctor fix")
	rootCmd.AddCommand(upgradeCmd)
}

type upgradeOptions struct {
	dryRun  bool
	verbose bool
	noStart bool
}

// upgradeResult tracks what changed in each step.
type upgradeResult struct {
	step    string
	changed int
	skipped int
	details []string
}

func runUpgrade(cmd *cobra.Command, _ []string) error {
	opts := upgradeOptions{
		dryRun:  commandBoolFlag(cmd, "dry-run"),
		verbose: commandBoolFlag(cmd, "verbose"),
		noStart: commandBoolFlag(cmd, "no-start"),
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	if opts.dryRun {
		fmt.Printf("\n%s Dry run — showing what would change\n", style.Bold.Render("gt upgrade"))
	} else {
		fmt.Printf("\n%s Post-install migration\n", style.Bold.Render("gt upgrade"))
	}

	var results []upgradeResult

	// Step 1: Run doctor --fix for structural checks
	r1 := upgradeDoctor(townRoot, opts)
	results = append(results, r1)

	// Step 2: Sync AGENTS.md from embedded template
	r2 := upgradeAgentsMD(townRoot, opts.dryRun)
	results = append(results, r2)

	// Step 3: Ensure daemon.json lifecycle defaults
	r3 := upgradeDaemonConfig(townRoot, opts.dryRun)
	results = append(results, r3)

	// Step 4: Sync hooks registry to settings.json
	r4 := upgradeHooksSync(townRoot, opts)
	results = append(results, r4)

	// Step 5: Update formulas from embedded copies
	r5 := upgradeFormulas(townRoot, opts.dryRun)
	results = append(results, r5)

	// Print summary
	printUpgradeSummary(results, opts.dryRun)

	return nil
}

// upgradeDoctor runs doctor --fix and returns the result.
func upgradeDoctor(townRoot string, opts upgradeOptions) upgradeResult {
	result := upgradeResult{step: "Structural checks"}

	fmt.Printf("\n  %s %s\n", style.Bold.Render("1."), "Running structural checks (doctor --fix)...")

	ctx := &doctor.CheckContext{
		TownRoot: townRoot,
		Verbose:  opts.verbose,
		NoStart:  opts.noStart,
	}

	d := doctor.NewDoctor()

	// Register the same checks as gt doctor (subset most relevant to upgrade)
	d.RegisterAll(doctor.WorkspaceChecks()...)
	d.Register(doctor.NewGlobalStateCheck())
	d.Register(doctor.NewStaleBinaryCheck(resolveCommitHash()))
	d.Register(doctor.NewBeadsBinaryCheck())
	d.Register(doctor.NewDoltBinaryCheck())
	d.Register(doctor.NewClaudeBinaryCheck())
	d.Register(doctor.NewMayorBinaryCheck())
	d.Register(doctor.NewDoltServerReachableCheck())
	d.Register(doctor.NewTownGitCheck())
	d.Register(doctor.NewTownRootBranchCheck())
	d.Register(doctor.NewPreCheckoutHookCheck())
	d.Register(doctor.NewClaudeSettingsCheck())
	d.Register(doctor.NewDaemonCheck())
	d.Register(doctor.NewTownBeadsConfigCheck())
	d.Register(doctor.NewBeadsDirPermsCheck())
	d.Register(doctor.NewCustomTypesCheck())
	d.Register(doctor.NewCustomStatusesCheck())
	d.Register(doctor.NewFormulaCheck())
	d.Register(doctor.NewPrefixConflictCheck())
	d.Register(doctor.NewPrefixMismatchCheck())
	d.Register(doctor.NewDatabasePrefixCheck())
	d.Register(doctor.NewRoutesCheck())
	d.Register(doctor.NewSettingsCheck())
	d.Register(doctor.NewSessionHookCheck())
	d.Register(doctor.NewDeprecatedMergeQueueKeysCheck())
	d.Register(doctor.NewStaleTaskDispatchCheck())
	d.Register(doctor.NewHooksSyncCheck())
	d.Register(doctor.NewStaleDoltPortCheck())
	d.Register(doctor.NewStaleSQLServerInfoCheck())
	d.Register(doctor.NewSparseCheckoutCheck())
	d.Register(doctor.NewPrimingCheck())
	d.Register(doctor.NewLifecycleHygieneCheck())
	d.Register(doctor.NewWorktreeGitdirCheck())

	// Identity bead repair: backfill missing rig, agent, and role beads (GH#2766).
	// Previously omitted from upgrade, leaving identity gaps that gt doctor --fix
	// could repair but gt upgrade would not.
	d.Register(doctor.NewAgentBeadsCheck())
	d.Register(doctor.NewRigBeadsCheck())
	d.Register(doctor.NewRoleBeadsCheck())

	var report *doctor.Report
	if opts.dryRun {
		report = d.RunStreaming(ctx, os.Stdout, 0)
	} else {
		report = d.FixStreaming(ctx, os.Stdout, 0)
	}

	result.changed = report.Summary.Fixed
	if report.HasErrors() {
		result.details = append(result.details, fmt.Sprintf("%d error(s) remain", report.Summary.Errors))
	}
	if report.Summary.Warnings > 0 {
		result.details = append(result.details, fmt.Sprintf("%d warning(s)", report.Summary.Warnings))
	}
	if result.changed > 0 {
		result.details = append(result.details, fmt.Sprintf("%d fixed", result.changed))
	}

	return result
}

// upgradeAgentsMD syncs the town-root identity pair from the embedded template.
func upgradeAgentsMD(townRoot string, dryRun bool) upgradeResult {
	result := upgradeResult{step: "AGENTS.md sync"}

	fmt.Printf("\n  %s %s\n", style.Bold.Render("2."), "Syncing AGENTS.md from template...")

	expected := templates.TownRootAgentsMD()
	current, err := readTownAgentsMD(townRoot)
	if err != nil && !os.IsNotExist(err) {
		result.details = append(result.details, fmt.Sprintf("error reading: %v", err))
		fmt.Printf("     %s Could not read AGENTS.md: %v\n", style.ErrorPrefix, err)
		return result
	}

	pairValid := instructions.TownPairValid(townRoot)
	if err == nil && string(current) == expected && pairValid {
		fmt.Printf("     %s AGENTS.md %s\n", style.SuccessPrefix, style.Dim.Render("up-to-date"))
		return result
	}

	if dryRun {
		return reportAgentsMDDryRun(result, err, pairValid)
	}
	return provisionAgentsMD(townRoot, expected, result, err)
}

func readTownAgentsMD(townRoot string) ([]byte, error) {
	current, err := os.ReadFile(filepath.Join(townRoot, instructions.CanonicalFile))
	if !os.IsNotExist(err) {
		return current, err
	}
	if data, readErr := os.ReadFile(filepath.Join(townRoot, instructions.AliasFile)); readErr == nil {
		return data, nil
	}
	return current, err
}

func reportAgentsMDDryRun(result upgradeResult, readErr error, pairValid bool) upgradeResult {
	status := "would update"
	if os.IsNotExist(readErr) {
		status = "would create"
	}
	fmt.Printf("     %s AGENTS.md %s\n", style.WarningPrefix, style.Dim.Render(status))
	result.changed = 1
	if !pairValid {
		result.changed++
	}
	return result
}

func provisionAgentsMD(townRoot, expected string, result upgradeResult, readErr error) upgradeResult {
	changed, provErr := instructions.Provision(townRoot, expected, "")
	if provErr != nil {
		result.details = append(result.details, fmt.Sprintf("error writing: %v", provErr))
		fmt.Printf("     %s Could not write instruction pair: %v\n", style.ErrorPrefix, provErr)
		return result
	}
	if !changed {
		fmt.Printf("     %s AGENTS.md %s\n", style.SuccessPrefix, style.Dim.Render("up-to-date"))
		return result
	}

	if os.IsNotExist(readErr) {
		fmt.Printf("     %s AGENTS.md %s\n", style.SuccessPrefix, style.Dim.Render("created"))
		result.changed = 1
	} else {
		fmt.Printf("     %s AGENTS.md %s\n", style.SuccessPrefix, style.Dim.Render("updated"))
		result.changed = 1
	}
	fmt.Printf("     %s CLAUDE.md %s\n", style.SuccessPrefix, style.Dim.Render("symlink to AGENTS.md"))
	result.changed++

	return result
}

// upgradeDaemonConfig ensures daemon.json has lifecycle defaults.
func upgradeDaemonConfig(townRoot string, dryRun bool) upgradeResult {
	result := upgradeResult{step: "Daemon config"}

	fmt.Printf("\n  %s %s\n", style.Bold.Render("3."), "Ensuring daemon.json lifecycle defaults...")

	daemonPath := config.DaemonPatrolConfigPath(townRoot)

	_, err := os.Stat(daemonPath)
	if err == nil {
		// File exists — validate it loads correctly
		if _, loadErr := config.LoadDaemonPatrolConfig(daemonPath); loadErr != nil {
			result.details = append(result.details, fmt.Sprintf("invalid config: %v", loadErr))
			fmt.Printf("     %s daemon.json exists but invalid: %v\n", style.WarningPrefix, loadErr)
			return result
		}
		fmt.Printf("     %s daemon.json %s\n", style.SuccessPrefix, style.Dim.Render("present and valid"))
		return result
	}

	if !os.IsNotExist(err) {
		result.details = append(result.details, fmt.Sprintf("error checking: %v", err))
		fmt.Printf("     %s Could not check daemon.json: %v\n", style.ErrorPrefix, err)
		return result
	}

	// File doesn't exist — create with defaults
	if dryRun {
		fmt.Printf("     %s daemon.json %s\n", style.WarningPrefix, style.Dim.Render("would create with defaults"))
		result.changed = 1
		return result
	}

	if err := config.EnsureDaemonPatrolConfig(townRoot); err != nil {
		result.details = append(result.details, fmt.Sprintf("error creating: %v", err))
		fmt.Printf("     %s Could not create daemon.json: %v\n", style.ErrorPrefix, err)
		return result
	}

	fmt.Printf("     %s daemon.json %s\n", style.SuccessPrefix, style.Dim.Render("created with defaults"))
	result.changed = 1

	return result
}

// upgradeHooksSync syncs hook registry to all settings.json files.
func upgradeHooksSync(townRoot string, opts upgradeOptions) upgradeResult {
	result := upgradeResult{step: "Hooks sync"}

	fmt.Printf("\n  %s %s\n", style.Bold.Render("4."), "Syncing hooks to settings.json...")

	targets, err := hooks.DiscoverTargets(townRoot)
	if err != nil {
		result.details = append(result.details, fmt.Sprintf("discover error: %v", err))
		fmt.Printf("     %s Could not discover targets: %v\n", style.ErrorPrefix, err)
		return result
	}

	summary := syncUpgradeTargets(townRoot, targets, opts)
	result.changed = summary.updated + summary.created
	if summary.errors > 0 {
		result.details = append(result.details, fmt.Sprintf("%d sync errors", summary.errors))
	}
	printUpgradeHooksSummary(summary, opts)

	return result
}

type hookSyncSummary struct {
	updated   int
	created   int
	unchanged int
	errors    int
}

func syncUpgradeTargets(townRoot string, targets []hooks.Target, opts upgradeOptions) hookSyncSummary {
	var summary hookSyncSummary
	for _, target := range targets {
		syncRes, err := syncTarget(target, opts.dryRun)
		if err != nil {
			summary.errors++
			if opts.verbose {
				relPath, _ := filepath.Rel(townRoot, target.Path)
				fmt.Printf("     %s %s: %v\n", style.ErrorPrefix, relPath, err)
			}
			continue
		}

		relPath, pathErr := filepath.Rel(townRoot, target.Path)
		if pathErr != nil {
			relPath = target.Path
		}
		summary.record(syncRes)
		printUpgradeHookTarget(relPath, syncRes, opts)
	}
	return summary
}

func (s *hookSyncSummary) record(result syncResult) {
	switch result {
	case syncCreated:
		s.created++
	case syncUpdated:
		s.updated++
	case syncUnchanged:
		s.unchanged++
	}
}

func printUpgradeHookTarget(relPath string, syncRes syncResult, opts upgradeOptions) {
	if !opts.verbose || syncRes == syncUnchanged {
		return
	}
	prefix := style.SuccessPrefix
	verb := "updated"
	if opts.dryRun {
		prefix = style.WarningPrefix
		verb = "would update"
	}
	if syncRes == syncCreated {
		verb = "created"
		if opts.dryRun {
			verb = "would create"
		}
	}
	fmt.Printf("     %s %s %s\n", prefix, relPath, style.Dim.Render("("+verb+")"))
}

func printUpgradeHooksSummary(summary hookSyncSummary, opts upgradeOptions) {
	var parts []string
	if summary.updated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", summary.updated))
	}
	if summary.created > 0 {
		parts = append(parts, fmt.Sprintf("%d created", summary.created))
	}
	if summary.unchanged > 0 {
		parts = append(parts, fmt.Sprintf("%d unchanged", summary.unchanged))
	}
	if summary.errors > 0 {
		parts = append(parts, fmt.Sprintf("%d errors", summary.errors))
	}
	prefix := style.SuccessPrefix
	if opts.dryRun && summary.updated+summary.created > 0 {
		prefix = style.WarningPrefix
	}
	fmt.Printf("     %s %s %s\n", prefix, "settings.json", style.Dim.Render(strings.Join(parts, ", ")))
}

// upgradeFormulas updates formulas from embedded copies.
func upgradeFormulas(townRoot string, dryRun bool) upgradeResult {
	if dryRun {
		return checkFormulaUpgrade(townRoot)
	}
	return applyFormulaUpgrade(townRoot)
}

func checkFormulaUpgrade(townRoot string) upgradeResult {
	result := upgradeResult{step: "Formulas"}

	fmt.Printf("\n  %s %s\n", style.Bold.Render("5."), "Updating formulas from embedded copies...")

	// In dry-run mode, just check health.
	report, err := formula.CheckFormulaHealth(townRoot)
	if err != nil {
		result.details = append(result.details, fmt.Sprintf("health check error: %v", err))
		fmt.Printf("     %s Could not check formulas: %v\n", style.ErrorPrefix, err)
		return result
	}

	needsUpdate := report.Outdated + report.Missing + report.New + report.Untracked
	if needsUpdate == 0 {
		fmt.Printf("     %s %d formulas %s\n", style.SuccessPrefix, report.OK, style.Dim.Render("up-to-date"))
		return result
	}

	result.changed = needsUpdate
	if report.Outdated > 0 {
		result.details = append(result.details, fmt.Sprintf("%d would update", report.Outdated))
	}
	if report.Missing > 0 {
		result.details = append(result.details, fmt.Sprintf("%d would reinstall", report.Missing))
	}
	if report.New > 0 {
		result.details = append(result.details, fmt.Sprintf("%d would install", report.New))
	}
	if report.Modified > 0 {
		result.skipped = report.Modified
		result.details = append(result.details, fmt.Sprintf("%d locally modified (skipped)", report.Modified))
	}

	fmt.Printf("     %s formulas: %s\n", style.WarningPrefix, style.Dim.Render(strings.Join(result.details, ", ")))
	return result
}

func applyFormulaUpgrade(townRoot string) upgradeResult {
	result := upgradeResult{step: "Formulas"}
	fmt.Printf("\n  %s %s\n", style.Bold.Render("5."), "Updating formulas from embedded copies...")
	counts, err := formula.UpdateFormulas(townRoot)
	if err != nil {
		result.details = append(result.details, fmt.Sprintf("update error: %v", err))
		fmt.Printf("     %s Could not update formulas: %v\n", style.ErrorPrefix, err)
		return result
	}

	result.changed = counts.Updated + counts.Reinstalled
	result.skipped = counts.Skipped

	if result.changed == 0 && result.skipped == 0 {
		// Check total count for display
		report, _ := formula.CheckFormulaHealth(townRoot)
		count := 0
		if report != nil {
			count = report.OK + report.Modified
		}
		fmt.Printf("     %s %d formulas %s\n", style.SuccessPrefix, count, style.Dim.Render("up-to-date"))
		return result
	}

	var parts []string
	if counts.Updated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", counts.Updated))
	}
	if counts.Reinstalled > 0 {
		parts = append(parts, fmt.Sprintf("%d reinstalled", counts.Reinstalled))
	}
	if counts.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (modified)", counts.Skipped))
	}

	fmt.Printf("     %s formulas: %s\n", style.SuccessPrefix, style.Dim.Render(strings.Join(parts, ", ")))

	return result
}

// printUpgradeSummary prints a final summary of what changed.
func printUpgradeSummary(results []upgradeResult, dryRun bool) {
	totalChanged := 0
	var issues []string

	for _, r := range results {
		totalChanged += r.changed
		for _, d := range r.details {
			if strings.Contains(d, "error") {
				issues = append(issues, fmt.Sprintf("%s: %s", r.step, d))
			}
		}
	}

	fmt.Println()
	if dryRun {
		if totalChanged == 0 {
			fmt.Printf("  %s Workspace is up-to-date — nothing to change\n", style.SuccessPrefix)
		} else {
			fmt.Printf("  %s Dry run complete — %d change(s) would be applied\n", style.WarningPrefix, totalChanged)
			fmt.Printf("     Run %s to apply\n", style.Dim.Render("gt upgrade"))
		}
	} else {
		if totalChanged == 0 {
			fmt.Printf("  %s Workspace is up-to-date\n", style.SuccessPrefix)
		} else {
			fmt.Printf("  %s Upgrade complete — %d change(s) applied\n", style.SuccessPrefix, totalChanged)
		}
	}

	if len(issues) > 0 {
		fmt.Println()
		fmt.Printf("  %s Issues:\n", style.WarningPrefix)
		for _, issue := range issues {
			fmt.Printf("     %s %s\n", style.ArrowPrefix, issue)
		}
	}

	fmt.Println()
}
