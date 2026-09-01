package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/hooks"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var hooksSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Regenerate all agent hook/settings files",
	Long: `Regenerate hook and settings files for all agents across the workspace.

For Claude agents (settings.json merge):
1. Load base config
2. Apply role override (if exists)
3. Apply rig+role override (if exists)
4. Merge hooks section into existing settings.json (preserving all fields)
5. Write updated settings.json

For template-based agents (OpenCode, Gemini, Copilot, etc.):
1. Resolve the agent configured for each role
2. Compare deployed hook file against current template
3. Overwrite if content differs

Examples:
  gt hooks sync             # Regenerate all hook/settings files
  gt hooks sync --dry-run   # Show what would change without writing`,
	RunE: runHooksSync,
}

func init() {
	hooksCmd.AddCommand(hooksSyncCmd)
	hooksSyncCmd.Flags().Bool("dry-run", false, "Show what would change without writing")
}

type hooksSyncSummary struct {
	updated         int
	unchanged       int
	created         int
	errors          int
	integrityErrors int
	failedTargets   []string
}

func (s *hooksSyncSummary) add(other hooksSyncSummary) {
	s.updated += other.updated
	s.unchanged += other.unchanged
	s.created += other.created
	s.errors += other.errors
	s.integrityErrors += other.integrityErrors
	s.failedTargets = append(s.failedTargets, other.failedTargets...)
}

func runHooksSync(cmd *cobra.Command, _ []string) error {
	dryRun := commandBoolFlag(cmd, "dry-run")
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	targets, err := hooks.DiscoverTargets(townRoot)
	if err != nil {
		return fmt.Errorf("discovering targets: %w", err)
	}

	if dryRun {
		fmt.Println("Dry run - showing what would change...")
		fmt.Println()
	} else {
		fmt.Println("Syncing hooks...")
	}

	summary := syncClaudeHookTargets(townRoot, targets, dryRun)
	summary.add(syncTemplateHookTargets(townRoot, dryRun))
	return finishHooksSync(summary, dryRun)
}

func syncClaudeHookTargets(townRoot string, targets []hooks.Target, dryRun bool) hooksSyncSummary {
	var summary hooksSyncSummary
	for _, target := range targets {
		result, err := syncTarget(target, dryRun)
		if err != nil {
			label := "sync error"
			if hooks.IsSettingsIntegrityError(err) {
				label = "integrity violation"
				summary.integrityErrors++
			}
			fmt.Printf(
				"  %s %s (%s): %v\n",
				style.Error.Render("✖"),
				target.DisplayKey(),
				label,
				err,
			)
			summary.errors++
			summary.failedTargets = append(summary.failedTargets, target.DisplayKey())
			continue
		}

		relPath, pathErr := filepath.Rel(townRoot, target.Path)
		if pathErr != nil {
			relPath = target.Path
		}
		recordClaudeSyncResult(&summary, result, relPath, dryRun)
	}
	return summary
}

func recordClaudeSyncResult(summary *hooksSyncSummary, result syncResult, relPath string, dryRun bool) {
	switch result {
	case syncCreated:
		if dryRun {
			fmt.Printf("  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would create)"))
		} else {
			fmt.Printf("  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(created)"))
		}
		summary.created++
	case syncUpdated:
		if dryRun {
			fmt.Printf("  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would update)"))
		} else {
			fmt.Printf("  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(updated)"))
		}
		summary.updated++
	case syncUnchanged:
		fmt.Printf("  %s %s %s\n", style.Dim.Render("·"), relPath, style.Dim.Render("(unchanged)"))
		summary.unchanged++
	}
}

func syncTemplateHookTargets(townRoot string, dryRun bool) hooksSyncSummary {
	var summary hooksSyncSummary
	townSettings, _ := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))

	locations, err := hooks.DiscoverRoleLocations(townRoot)
	if err != nil {
		fmt.Printf("  %s discovering role locations: %v\n", style.Error.Render("✖"), err)
		summary.errors++
		return summary
	}

	for _, loc := range locations {
		summary.add(syncTemplateHookLocation(townRoot, townSettings, loc, dryRun))
	}
	return summary
}

func syncTemplateHookLocation(townRoot string, townSettings *config.TownSettings, loc hooks.RoleLocation, dryRun bool) hooksSyncSummary {
	hooksProvider, preset, syncDirs, ok := resolveTemplateHookLocation(townRoot, townSettings, loc)
	if !ok {
		return hooksSyncSummary{}
	}

	var summary hooksSyncSummary
	for _, dir := range syncDirs {
		summary.add(syncTemplateHookDir(townRoot, loc, hooksProvider, preset, dir, dryRun))
	}
	return summary
}

func resolveTemplateHookLocation(townRoot string, townSettings *config.TownSettings, loc hooks.RoleLocation) (string, *config.AgentPresetInfo, []string, bool) {
	rigPath, rigSettings := templateHookRigConfig(townRoot, loc)

	// Use ResolveRoleAgentName (not ResolveRoleAgentConfig) so that hooks are
	// installed based on the *configured* agent, not the *resolved* one.
	// ResolveRoleAgentConfig falls back to claude when the agent binary is not
	// found in PATH (e.g., in CI or on a fresh machine), which would silently
	// skip creating opencode/gemini/etc. plugin files.
	agentName, _ := config.ResolveRoleAgentName(loc.Role, townRoot, rigPath)
	if agentName == "" {
		return "", nil, nil, false
	}

	preset := config.ResolveAgentPreset(agentName, townSettings, rigSettings)
	if preset == nil || preset.HooksDir == "" || preset.HooksSettingsFile == "" {
		return "", nil, nil, false
	}

	hooksProvider := preset.HooksProvider
	if hooksProvider == "" {
		hooksProvider = string(preset.Name)
	}

	// Claude targets are already handled by DiscoverTargets + syncTarget above.
	if hooksProvider == "claude" {
		return "", nil, nil, false
	}

	if loc.Rig == "" || preset.HooksUseSettingsDir {
		return hooksProvider, preset, []string{loc.Dir}, true
	}
	return hooksProvider, preset, hooks.DiscoverWorktrees(loc.Dir), true
}

func templateHookRigConfig(townRoot string, loc hooks.RoleLocation) (string, *config.RigSettings) {
	if loc.Rig == "" {
		return "", nil
	}
	rigPath := filepath.Join(townRoot, loc.Rig)
	rigSettings, _ := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	return rigPath, rigSettings
}

func syncTemplateHookDir(townRoot string, loc hooks.RoleLocation, hooksProvider string, preset *config.AgentPresetInfo, dir string, dryRun bool) hooksSyncSummary {
	var summary hooksSyncSummary
	targetPath := filepath.Join(dir, preset.HooksDir, preset.HooksSettingsFile)
	relPath, pathErr := filepath.Rel(townRoot, targetPath)
	if pathErr != nil {
		relPath = targetPath
	}

	if dryRun {
		if _, statErr := os.Stat(targetPath); statErr == nil {
			fmt.Printf("  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would check "+hooksProvider+")"))
		} else {
			fmt.Printf("  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would create "+hooksProvider+")"))
			summary.created++
		}
		return summary
	}

	result, err := hooks.SyncForRole(hooksProvider, dir, dir, loc.Role,
		preset.HooksDir, preset.HooksSettingsFile, preset.HooksUseSettingsDir)
	if err != nil {
		fmt.Printf("  %s %s (%s): %v\n", style.Error.Render("✖"), relPath, hooksProvider, err)
		summary.errors++
		summary.failedTargets = append(summary.failedTargets, relPath)
		return summary
	}

	switch result {
	case hooks.SyncCreated:
		fmt.Printf("  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(created "+hooksProvider+")"))
		summary.created++
	case hooks.SyncUpdated:
		fmt.Printf("  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(updated "+hooksProvider+")"))
		summary.updated++
	case hooks.SyncUnchanged:
		fmt.Printf("  %s %s %s\n", style.Dim.Render("·"), relPath, style.Dim.Render("(unchanged "+hooksProvider+")"))
		summary.unchanged++
	}
	return summary
}

func finishHooksSync(summary hooksSyncSummary, dryRun bool) error {
	fmt.Println()
	total := summary.updated + summary.unchanged + summary.created + summary.errors
	if dryRun {
		fmt.Printf("Would sync %d targets (%d to create, %d to update, %d unchanged",
			total, summary.created, summary.updated, summary.unchanged)
	} else {
		fmt.Printf("Synced %d targets (%d created, %d updated, %d unchanged",
			total, summary.created, summary.updated, summary.unchanged)
	}
	if summary.errors > 0 {
		fmt.Printf(", %s", style.Error.Render(fmt.Sprintf("%d errors", summary.errors)))
	}
	fmt.Println(")")

	if summary.errors == 0 {
		return nil
	}
	if summary.integrityErrors > 0 {
		return fmt.Errorf(
			"hooks sync failed closed: %d integrity violation(s) across %s",
			summary.integrityErrors,
			strings.Join(summary.failedTargets, ", "),
		)
	}
	return fmt.Errorf(
		"hooks sync failed: %d target(s) failed (%s)",
		summary.errors,
		strings.Join(summary.failedTargets, ", "),
	)
}

type syncResult int

const (
	syncUnchanged syncResult = iota
	syncUpdated
	syncCreated
)

// syncTarget syncs a single target's .claude/settings.json.
// Uses MarshalSettings/UnmarshalSettings to preserve unknown fields.
func syncTarget(target hooks.Target, dryRun bool) (syncResult, error) {
	result, err := hooks.SyncManagedClaudeSettings(target, dryRun)
	if err != nil {
		return 0, err
	}

	switch result {
	case hooks.SyncCreated:
		return syncCreated, nil
	case hooks.SyncUpdated:
		return syncUpdated, nil
	case hooks.SyncUnchanged:
		return syncUnchanged, nil
	default:
		return 0, fmt.Errorf("unknown sync result: %d", result)
	}
}
