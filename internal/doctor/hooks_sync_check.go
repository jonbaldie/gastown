package doctor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/hooks"
)

// templateTarget tracks a non-Claude template-based agent file that is out of sync.
type templateTarget struct {
	path           string
	dir            string
	provider       string
	role           string
	hooksDir       string
	settingsFile   string
	useSettingsDir bool
}

// HooksSyncCheck verifies all hook/settings files match what gt hooks sync would generate.
type HooksSyncCheck struct {
	FixableCheck
	outOfSync         []hooks.Target   // Claude targets
	templateOutOfSync []templateTarget // Non-Claude template-based targets
}

// NewHooksSyncCheck creates a new hooks sync validation check.
func NewHooksSyncCheck() *HooksSyncCheck {
	return &HooksSyncCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "hooks-sync",
				CheckDescription: "Verify hooks settings.json files are in sync",
				CheckCategory:    CategoryHooks,
			},
		},
	}
}

// Run checks all managed hook/settings files for sync status.
func (c *HooksSyncCheck) Run(ctx *CheckContext) *CheckResult {
	c.outOfSync = nil
	c.templateOutOfSync = nil

	var details []string
	totalTargets, err := c.scanClaudeTargets(ctx.TownRoot, &details)
	if err != nil {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusWarning,
			Message:  fmt.Sprintf("Failed to discover targets: %v", err),
			Category: c.Category(),
		}
	}

	townSettings, _ := config.LoadOrCreateTownSettings(config.TownSettingsPath(ctx.TownRoot))
	totalTargets += c.scanTemplateTargets(ctx.TownRoot, townSettings, &details)
	return c.result(totalTargets, details)
}

// scanClaudeTargets checks the base-plus-override targets discovered for Claude.
func (c *HooksSyncCheck) scanClaudeTargets(townRoot string, details *[]string) (int, error) {
	targets, err := hooks.DiscoverTargets(townRoot)
	if err != nil {
		return 0, err
	}
	for _, target := range targets {
		detail, outOfSync := inspectClaudeTarget(target)
		if outOfSync {
			c.outOfSync = append(c.outOfSync, target)
		}
		if detail != "" {
			*details = append(*details, detail)
		}
	}
	return len(targets), nil
}

func inspectClaudeTarget(target hooks.Target) (string, bool) {
	expected, err := hooks.ComputeExpected(target.Key)
	if err != nil {
		return fmt.Sprintf("%s: error computing expected: %v", target.DisplayKey(), err), false
	}

	current, err := hooks.LoadSettings(target.Path)
	if err != nil {
		return fmt.Sprintf("%s: error loading: %v", target.DisplayKey(), err), false
	}

	_, statErr := os.Stat(target.Path)
	fileExists := statErr == nil
	if !fileExists {
		return fmt.Sprintf("%s: missing", target.DisplayKey()), true
	}
	if !hooks.HasClaudePromptDefaults(current) {
		return fmt.Sprintf("%s: missing Claude prompt defaults", target.DisplayKey()), true
	}
	if !hooks.HooksEqual(expected, &current.Hooks) {
		return fmt.Sprintf("%s: out of sync", target.DisplayKey()), true
	}
	return "", false
}

// scanTemplateTargets checks non-Claude, template-based agent installations.
func (c *HooksSyncCheck) scanTemplateTargets(townRoot string, townSettings *config.TownSettings, details *[]string) int {
	locations, err := hooks.DiscoverRoleLocations(townRoot)
	if err != nil {
		*details = append(*details, fmt.Sprintf("discovering role locations: %v", err))
		return 0
	}

	total := 0
	for _, loc := range locations {
		spec, ok := templateSpecForLocation(townRoot, townSettings, loc)
		if !ok {
			continue
		}
		for _, dir := range templateCheckDirs(loc, spec.useSettingsDir) {
			total++
			c.inspectTemplateTarget(spec, dir, details)
		}
	}
	return total
}

type templateSpec struct {
	provider       string
	role           string
	hooksDir       string
	settingsFile   string
	useSettingsDir bool
}

func templateSpecForLocation(townRoot string, townSettings *config.TownSettings, loc hooks.RoleLocation) (templateSpec, bool) {
	rigPath := ""
	var rigSettings *config.RigSettings
	if loc.Rig != "" {
		rigPath = filepath.Join(townRoot, loc.Rig)
		rigSettings, _ = config.LoadRigSettings(config.RigSettingsPath(rigPath))
	}
	// ResolveRoleAgentName intentionally uses the configured agent. Resolving the
	// runtime config could fall back to Claude when a binary is absent in CI.
	agentName, _ := config.ResolveRoleAgentName(loc.Role, townRoot, rigPath)
	if agentName == "" {
		return templateSpec{}, false
	}
	preset := config.ResolveAgentPreset(agentName, townSettings, rigSettings)
	if preset == nil || preset.HooksDir == "" || preset.HooksSettingsFile == "" {
		return templateSpec{}, false
	}
	provider := preset.HooksProvider
	if provider == "" {
		provider = string(preset.Name)
	}
	if provider == "claude" {
		return templateSpec{}, false
	}
	return templateSpec{
		provider: provider, role: loc.Role, hooksDir: preset.HooksDir,
		settingsFile: preset.HooksSettingsFile, useSettingsDir: preset.HooksUseSettingsDir,
	}, true
}

func templateCheckDirs(loc hooks.RoleLocation, useSettingsDir bool) []string {
	if loc.Rig == "" || useSettingsDir {
		return []string{loc.Dir}
	}
	return hooks.DiscoverWorktrees(loc.Dir)
}

func (c *HooksSyncCheck) inspectTemplateTarget(spec templateSpec, dir string, details *[]string) {
	targetPath := filepath.Join(dir, spec.hooksDir, spec.settingsFile)
	target := templateTarget{
		path: targetPath, dir: dir, provider: spec.provider,
		role: spec.role, hooksDir: spec.hooksDir,
		settingsFile: spec.settingsFile, useSettingsDir: spec.useSettingsDir,
	}

	expected, err := hooks.ComputeExpectedTemplate(spec.provider, spec.settingsFile, spec.role)
	if err != nil {
		*details = append(*details, fmt.Sprintf("%s (%s): error computing template: %v", targetPath, spec.provider, err))
		return
	}
	actual, err := os.ReadFile(targetPath)
	if err != nil {
		c.templateOutOfSync = append(c.templateOutOfSync, target)
		*details = append(*details, fmt.Sprintf("%s (%s): missing", targetPath, spec.provider))
		return
	}

	inSync := bytes.Equal(expected, actual)
	if filepath.Ext(spec.settingsFile) == ".json" {
		inSync = hooks.TemplateContentEqual(expected, actual)
	}
	if !inSync {
		c.templateOutOfSync = append(c.templateOutOfSync, target)
		*details = append(*details, fmt.Sprintf("%s (%s): out of sync", targetPath, spec.provider))
	}
}

func (c *HooksSyncCheck) result(totalTargets int, details []string) *CheckResult {

	outOfSyncCount := len(c.outOfSync) + len(c.templateOutOfSync)
	if outOfSyncCount == 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  fmt.Sprintf("All %d hook targets in sync", totalTargets),
			Category: c.Category(),
		}
	}

	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusWarning,
		Message:  fmt.Sprintf("%d target(s) out of sync", outOfSyncCount),
		Details:  details,
		FixHint:  "Run 'gt doctor --fix hooks-sync' to regenerate settings files",
		Category: c.Category(),
	}
}

// Fix brings all out-of-sync targets back into sync.
func (c *HooksSyncCheck) Fix(_ *CheckContext) error {
	if len(c.outOfSync) == 0 && len(c.templateOutOfSync) == 0 {
		return nil
	}

	errs := c.fixClaudeTargets()
	errs = append(errs, c.fixTemplateTargets()...)
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (c *HooksSyncCheck) fixClaudeTargets() []string {
	var errs []string
	for _, target := range c.outOfSync {
		if err := fixClaudeTarget(target); err != nil {
			errs = append(errs, err.Error())
		}
	}
	return errs
}

func fixClaudeTarget(target hooks.Target) error {
	expected, err := hooks.ComputeExpected(target.Key)
	if err != nil {
		return fmt.Errorf("%s: %v", target.DisplayKey(), err)
	}
	current, err := hooks.LoadSettings(target.Path)
	if err != nil {
		return fmt.Errorf("%s: %v", target.DisplayKey(), err)
	}
	current.Hooks = *expected
	if current.EnabledPlugins == nil {
		current.EnabledPlugins = make(map[string]bool)
	}
	current.EnabledPlugins["beads@beads-marketplace"] = false

	claudeDir := filepath.Dir(target.Path)
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("%s: creating dir: %v", target.DisplayKey(), err)
	}
	data, err := hooks.MarshalSettings(current)
	if err != nil {
		return fmt.Errorf("%s: marshal: %v", target.DisplayKey(), err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(target.Path, data, 0644); err != nil {
		return fmt.Errorf("%s: write: %v", target.DisplayKey(), err)
	}
	return nil
}

func (c *HooksSyncCheck) fixTemplateTargets() []string {
	var errs []string
	for _, tt := range c.templateOutOfSync {
		_, err := hooks.SyncForRole(tt.provider, tt.dir, tt.dir, tt.role,
			tt.hooksDir, tt.settingsFile, tt.useSettingsDir)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", tt.path, err))
		}
	}
	return errs
}
