package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
)

type claudeSettingsScan struct {
	hasModifiedFiles bool
	hasMissingFiles  bool
	hasStaleFiles    bool
	details          []string
}

func runClaudeSettingsCheck(c *ClaudeSettingsCheck, ctx *CheckContext) *CheckResult {
	c.staleSettings = nil
	scan := &claudeSettingsScan{}
	for _, sf := range findClaudeSettingsFiles(ctx.TownRoot) {
		classifyClaudeSettingsFile(c, ctx, sf, scan)
	}
	if len(c.staleSettings) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "All Claude settings.json files are up to date",
		}
	}
	message, fixHint := claudeSettingsResultCopy(c, scan)
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: message,
		Details: scan.details,
		FixHint: fixHint,
	}
}

func classifyClaudeSettingsFile(c *ClaudeSettingsCheck, ctx *CheckContext, sf staleSettingsInfo, scan *claudeSettingsScan) {
	if sf.missingFile {
		recordMissingClaudeSettings(c, ctx, sf, scan)
		return
	}
	if sf.wrongLocation {
		recordWrongLocationClaudeSettings(c, sf, scan)
		return
	}
	recordStaleContentClaudeSettings(c, sf, scan)
}

func recordMissingClaudeSettings(c *ClaudeSettingsCheck, ctx *CheckContext, sf staleSettingsInfo, scan *claudeSettingsScan) {
	if !roleUsesClaudeSettings(ctx.TownRoot, sf.rigName, sf.agentType) {
		scan.details = append(scan.details, fmt.Sprintf("%s: missing (not required for non-Claude %s)", sf.path, sf.agentType))
		return
	}
	c.staleSettings = append(c.staleSettings, sf)
	scan.details = append(scan.details, fmt.Sprintf("%s: missing (restart %s to create)", sf.path, sf.agentType))
	scan.hasMissingFiles = true
}

func recordWrongLocationClaudeSettings(c *ClaudeSettingsCheck, sf staleSettingsInfo, scan *claudeSettingsScan) {
	sf.gitStatus = gitSettingsFileStatus(sf.path)
	baseName := filepath.Base(sf.path)
	isGastownSettings := baseName == "settings.json" || baseName == "settings.local.json"
	if sf.gitStatus == gitStatusIgnored && !isGastownSettings {
		return
	}
	c.staleSettings = append(c.staleSettings, sf)
	scan.hasStaleFiles = true
	statusMsg, modified := wrongLocationStatusMsg(sf.gitStatus)
	if modified {
		scan.hasModifiedFiles = true
	}
	scan.details = append(scan.details, fmt.Sprintf("%s: %s", sf.path, statusMsg))
}

func wrongLocationStatusMsg(status gitFileStatus) (string, bool) {
	switch status {
	case gitStatusIgnored:
		return "wrong location, gitignored (safe to delete)", false
	case gitStatusUntracked:
		return "wrong location, untracked (safe to delete)", false
	case gitStatusTrackedClean:
		return "wrong location, tracked but unmodified (safe to delete)", false
	case gitStatusTrackedModified:
		return "wrong location, tracked with local modifications (manual review needed)", true
	default:
		return "wrong location (inside source repo)", false
	}
}

func recordStaleContentClaudeSettings(c *ClaudeSettingsCheck, sf staleSettingsInfo, scan *claudeSettingsScan) {
	missing := checkClaudeSettings(sf.path, sf.agentType)
	if len(missing) == 0 {
		return
	}
	sf.missing = missing
	c.staleSettings = append(c.staleSettings, sf)
	scan.hasStaleFiles = true
	scan.details = append(scan.details, fmt.Sprintf("%s: missing %s", sf.path, strings.Join(missing, ", ")))
}

func claudeSettingsResultCopy(c *ClaudeSettingsCheck, scan *claudeSettingsScan) (string, string) {
	if scan.hasMissingFiles && !scan.hasStaleFiles {
		return fmt.Sprintf("Found %d agent(s) missing settings.json", len(c.staleSettings)),
			"Run 'gt up --restore' to restart agents and create settings"
	}
	if scan.hasStaleFiles && !scan.hasMissingFiles {
		message := fmt.Sprintf("Found %d stale Claude config file(s)", len(c.staleSettings))
		if scan.hasModifiedFiles {
			return message, "Run 'gt doctor --fix' to fix safe issues. Files with local modifications require manual review."
		}
		return message, "Run 'gt doctor --fix' to delete stale files, then 'gt up --restore' to create new settings"
	}
	message := fmt.Sprintf("Found %d Claude settings issue(s)", len(c.staleSettings))
	if scan.hasModifiedFiles {
		return message, "Run 'gt doctor --fix' to fix safe issues, then 'gt up --restore'. Files with local modifications require manual review."
	}
	return message, "Run 'gt doctor --fix' to delete stale files, then 'gt up --restore' to create new settings"
}

func fixClaudeSettings(c *ClaudeSettingsCheck, ctx *CheckContext) error {
	var errors []string
	var skipped []string
	needsRestart := false
	t := tmux.NewTmux()
	for _, sf := range c.staleSettings {
		if shouldSkipClaudeSettingsFix(sf, &skipped) {
			continue
		}
		restarted, err := applyClaudeSettingsFix(ctx, t, sf)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}
		if restarted {
			needsRestart = true
		}
	}
	reportClaudeSettingsFix(ctx, skipped, needsRestart)
	if len(errors) > 0 {
		return fmt.Errorf("%s", strings.Join(errors, "; "))
	}
	return nil
}

func shouldSkipClaudeSettingsFix(sf staleSettingsInfo, skipped *[]string) bool {
	if !sf.wrongLocation && len(sf.missing) == 0 {
		return true
	}
	if sf.missingFile {
		return true
	}
	if sf.gitStatus == gitStatusTrackedModified {
		*skipped = append(*skipped, fmt.Sprintf("%s: tracked with local modifications, skipping", sf.path))
		return true
	}
	if sf.gitStatus == gitStatusTrackedClean {
		*skipped = append(*skipped, fmt.Sprintf("%s: tracked in customer repo, skipping", sf.path))
		return true
	}
	return false
}

func applyClaudeSettingsFix(ctx *CheckContext, t *tmux.Tmux, sf staleSettingsInfo) (bool, error) {
	if err := os.Remove(sf.path); err != nil {
		return false, fmt.Errorf("failed to delete %s: %v", sf.path, err)
	}
	fmt.Printf("  Deleted stale: %s\n", sf.path)
	if sf.wrongLocation {
		_ = os.Remove(filepath.Dir(sf.path))
	}
	if sf.agentType == "rig-root" {
		fmt.Printf("\n  %s Rig-root settings removed. Per-role settings in %s/{witness,polecats,...}/.claude/ are authoritative.\n", style.Warning.Render("⚠"), sf.rigName)
		return true, nil
	}
	if sf.agentType == "mayor" && !strings.Contains(sf.path, "/mayor/") {
		recreateTownRootMayorSettings(ctx, sf)
		return true, nil
	}
	if err := recreateRoleClaudeSettings(ctx, t, sf); err != nil {
		return true, err
	}
	return true, nil
}

func recreateTownRootMayorSettings(ctx *CheckContext, sf staleSettingsInfo) {
	mayorDir := filepath.Join(ctx.TownRoot, "mayor")
	if strings.HasSuffix(filepath.Dir(sf.path), ".claude") {
		if err := os.MkdirAll(mayorDir, 0755); err == nil {
			runtimeConfig := config.ResolveRoleAgentConfig("mayor", ctx.TownRoot, mayorDir)
			_ = runtime.EnsureSettingsForRole(mayorDir, mayorDir, "mayor", runtimeConfig)
		}
	}
	fmt.Printf("\n  %s Town-root settings were moved. Restart agents to pick up new config:\n", style.Warning.Render("⚠"))
	fmt.Printf("      gt up --restore\n\n")
}

func recreateRoleClaudeSettings(ctx *CheckContext, t *tmux.Tmux, sf staleSettingsInfo) error {
	settingsDir := filepath.Dir(filepath.Dir(sf.path))
	workDir := settingsDir
	rigPath := ""
	if sf.rigName != "" {
		rigPath = filepath.Join(ctx.TownRoot, sf.rigName)
		if sd := config.RoleSettingsDir(sf.agentType, rigPath); sd != "" {
			settingsDir = sd
			workDir = sd
		}
	}
	runtimeConfig := config.ResolveRoleAgentConfig(sf.agentType, ctx.TownRoot, rigPath)
	if err := runtime.EnsureSettingsForRole(settingsDir, workDir, sf.agentType, runtimeConfig); err != nil {
		return fmt.Errorf("failed to recreate settings for %s: %v", sf.path, err)
	}
	maybeCycleClaudeSession(ctx, t, sf)
	return nil
}

func maybeCycleClaudeSession(ctx *CheckContext, t *tmux.Tmux, sf staleSettingsInfo) {
	if !ctx.RestartSessions {
		return
	}
	if sf.agentType != "witness" && sf.agentType != "refinery" && sf.agentType != "deacon" && sf.agentType != "mayor" {
		return
	}
	running, _ := t.HasSession(sf.sessionName)
	if running {
		_ = t.KillSessionWithProcesses(sf.sessionName)
	}
}

func reportClaudeSettingsFix(ctx *CheckContext, skipped []string, needsRestart bool) {
	for _, s := range skipped {
		fmt.Printf("  Warning: %s\n", s)
	}
	if needsRestart && !ctx.RestartSessions {
		fmt.Printf("\n  %s Restart agents to create new settings:\n", style.Warning.Render("⚠"))
		fmt.Printf("      gt up --restore\n")
		fmt.Printf("\n  If you had custom Claude settings edits, re-apply them via 'gt hooks override <role>'.\n\n")
	}
}
