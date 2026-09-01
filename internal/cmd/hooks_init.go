package cmd

import (
	"fmt"

	"github.com/jonbaldie/gastown/internal/hooks"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var hooksInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Bootstrap base config from existing settings.json files",
	Long: `Bootstrap the hooks base config by analyzing existing settings.json files.

This scans all managed .claude/settings.json files in the workspace,
finds hooks that are common across all targets (becomes the base config),
and identifies per-target differences (becomes overrides).

After init, run 'gt hooks diff' to verify no changes would be made.

Examples:
  gt hooks init             # Bootstrap base and overrides
  gt hooks init --dry-run   # Show what would be written without writing`,
	RunE: runHooksInit,
}

func init() {
	hooksCmd.AddCommand(hooksInitCmd)
	hooksInitCmd.Flags().Bool("dry-run", false, "Show what would be written without writing")
}

func runHooksInit(cmd *cobra.Command, _ []string) error {
	dryRun := commandBoolFlag(cmd, "dry-run")
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	if err := ensureHooksInitBaseAbsent(); err != nil {
		return err
	}
	found, err := collectHooksInitTargets(townRoot)
	if err != nil {
		return err
	}

	if len(found) == 0 {
		return initializeDefaultHooksBase(dryRun)
	}

	fmt.Printf("Found hooks in %d settings file(s)\n\n", len(found))

	// Find common hooks across all targets (intersection = base)
	base := findCommonHooks(found)

	return finalizeHooksInit(base, buildHooksInitOverrides(base, found), dryRun)
}

func ensureHooksInitBaseAbsent() error {
	if _, err := hooks.LoadBase(); err == nil {
		return fmt.Errorf("base config already exists at %s\nUse 'gt hooks base' to edit it", hooks.BasePath())
	}
	return nil
}

func collectHooksInitTargets(townRoot string) ([]targetHooks, error) {
	targets, err := hooks.DiscoverTargets(townRoot)
	if err != nil {
		return nil, fmt.Errorf("discovering targets: %w", err)
	}

	var found []targetHooks
	for _, target := range targets {
		settings, err := hooks.LoadSettings(target.Path)
		if err != nil || !hooksConfigHasEntries(&settings.Hooks) {
			continue
		}
		found = append(found, targetHooks{target: target, config: &settings.Hooks})
	}
	return found, nil
}

func hooksConfigHasEntries(config *hooks.HooksConfig) bool {
	for _, eventType := range hooks.EventTypes {
		if len(config.GetEntries(eventType)) > 0 {
			return true
		}
	}
	return false
}

func initializeDefaultHooksBase(dryRun bool) error {
	fmt.Println("No existing hooks found in workspace settings files.")
	fmt.Println("Creating default base config...")
	base := hooks.DefaultBase()
	if dryRun {
		data, _ := hooks.MarshalConfig(base)
		fmt.Printf("\nWould write to %s:\n%s\n", hooks.BasePath(), string(data))
		return nil
	}
	if err := hooks.SaveBase(base); err != nil {
		return fmt.Errorf("saving base config: %w", err)
	}
	fmt.Printf("%s Created base config at %s\n", style.Success.Render("✓"), hooks.BasePath())
	return nil
}

type hooksInitOverride struct {
	key    string
	config *hooks.HooksConfig
}

func buildHooksInitOverrides(base *hooks.HooksConfig, found []targetHooks) []hooksInitOverride {
	var overrides []hooksInitOverride
	seen := make(map[string]bool)
	for _, th := range found {
		diff := computeDiff(base, th.config)
		if diff == nil || seen[th.target.Key] {
			continue
		}
		seen[th.target.Key] = true
		overrides = append(overrides, hooksInitOverride{key: th.target.Key, config: diff})
	}
	return overrides
}

func finalizeHooksInit(base *hooks.HooksConfig, overrides []hooksInitOverride, dryRun bool) error {
	if dryRun {
		printHooksInitDryRun(base, overrides)
		return nil
	}
	return saveHooksInitResults(base, overrides)
}

func printHooksInitDryRun(base *hooks.HooksConfig, overrides []hooksInitOverride) {
	data, _ := hooks.MarshalConfig(base)
	fmt.Printf("Would write base config to %s:\n%s\n\n", hooks.BasePath(), string(data))
	for _, override := range overrides {
		data, _ := hooks.MarshalConfig(override.config)
		fmt.Printf("Would write override %s to %s:\n%s\n\n",
			override.key, hooks.OverridePath(override.key), string(data))
	}
	fmt.Printf("%s %d base + %d override(s) would be created\n",
		style.Dim.Render("(dry-run)"), 1, len(overrides))
}

func saveHooksInitResults(base *hooks.HooksConfig, overrides []hooksInitOverride) error {
	if err := hooks.SaveBase(base); err != nil {
		return fmt.Errorf("saving base config: %w", err)
	}
	fmt.Printf("%s Created base config at %s\n", style.Success.Render("✓"), hooks.BasePath())
	for _, override := range overrides {
		if err := hooks.SaveOverride(override.key, override.config); err != nil {
			fmt.Printf("  %s Failed to write override %s: %v\n", style.Warning.Render("!"), override.key, err)
			continue
		}
		fmt.Printf("%s Created override %s\n", style.Success.Render("✓"), override.key)
	}
	fmt.Printf("\nVerify with: %s\n", style.Dim.Render("gt hooks diff"))
	return nil
}

// findCommonHooks finds hook entries common across all targets.
// An entry is "common" if every target has the same matcher+hooks for that event type.
// Collects candidate entries from ALL targets to compute a proper intersection.
func findCommonHooks(targets []targetHooks) *hooks.HooksConfig {
	if len(targets) == 0 {
		return hooks.DefaultBase()
	}

	result := &hooks.HooksConfig{}

	for _, et := range hooks.EventTypes {
		common := commonHooksForEvent(targets, et)
		if len(common) > 0 {
			result.SetEntries(et, common)
		}
	}

	return result
}

type hooksEntryKey struct {
	matcher string
	hooks   string
}

func commonHooksForEvent(targets []targetHooks, eventType string) []hooks.HookEntry {
	candidates := candidateHooksForEvent(targets, eventType)
	var common []hooks.HookEntry
	for _, entry := range candidates {
		if hookEntryIsCommon(targets, eventType, entry) {
			common = append(common, entry)
		}
	}
	return common
}

func candidateHooksForEvent(targets []targetHooks, eventType string) map[hooksEntryKey]hooks.HookEntry {
	seen := make(map[hooksEntryKey]hooks.HookEntry)
	for _, th := range targets {
		for _, entry := range th.config.GetEntries(eventType) {
			key := hooksEntryKey{matcher: entry.Matcher, hooks: hooksFingerprint(entry.Hooks)}
			if _, ok := seen[key]; !ok {
				seen[key] = entry
			}
		}
	}
	return seen
}

func hookEntryIsCommon(targets []targetHooks, eventType string, entry hooks.HookEntry) bool {
	for _, target := range targets {
		if !hookEntryInConfig(target.config, eventType, entry) {
			return false
		}
	}
	return true
}

func hookEntryInConfig(config *hooks.HooksConfig, eventType string, entry hooks.HookEntry) bool {
	for _, candidate := range config.GetEntries(eventType) {
		if candidate.Matcher == entry.Matcher && hooksListEqual(candidate.Hooks, entry.Hooks) {
			return true
		}
	}
	return false
}

// hooksFingerprint returns a string key for a slice of hooks, used for deduplication.
func hooksFingerprint(hks []hooks.Hook) string {
	var s string
	for _, h := range hks {
		s += h.Type + ":" + h.Command + ";"
	}
	return s
}

// computeDiff returns hooks in target that are not in base, or nil if identical.
func computeDiff(base, target *hooks.HooksConfig) *hooks.HooksConfig {
	if hooks.HooksEqual(base, target) {
		return nil
	}

	diff := &hooks.HooksConfig{}
	for _, et := range hooks.EventTypes {
		diffEntries := diffEntriesForEvent(base, target, et)
		if len(diffEntries) > 0 {
			diff.SetEntries(et, diffEntries)
		}
	}

	if !hooksConfigHasEntries(diff) {
		return nil
	}
	return diff
}

func diffEntriesForEvent(base, target *hooks.HooksConfig, eventType string) []hooks.HookEntry {
	var diff []hooks.HookEntry
	for _, entry := range target.GetEntries(eventType) {
		if !hookEntryInConfig(base, eventType, entry) {
			diff = append(diff, entry)
		}
	}
	return diff
}

// hooksListEqual checks if two hook lists are identical.
func hooksListEqual(a, b []hooks.Hook) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Command != b[i].Command {
			return false
		}
	}
	return true
}

// targetHooks pairs a target with its parsed hooks config.
type targetHooks struct {
	target hooks.Target
	config *hooks.HooksConfig
}
