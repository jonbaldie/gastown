package doctor

import (
	"os"
	"path/filepath"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/instructions"
)

// gitFileStatus represents the git status of a file.
type gitFileStatus string

const (
	gitStatusUntracked       gitFileStatus = "untracked"        // File not tracked by git
	gitStatusTrackedClean    gitFileStatus = "tracked-clean"    // Tracked, no local modifications
	gitStatusTrackedModified gitFileStatus = "tracked-modified" // Tracked with local modifications
	gitStatusIgnored         gitFileStatus = "ignored"          // File is gitignored
	gitStatusUnknown         gitFileStatus = "unknown"          // Not in a git repo or error
)

// ClaudeSettingsCheck verifies that Claude settings.json files match the expected templates.
// Detects stale settings files that are missing required hooks or configuration.
type ClaudeSettingsCheck struct {
	FixableCheck
	staleSettings []staleSettingsInfo
}

type staleSettingsInfo struct {
	path          string        // Full path to settings file
	agentType     string        // e.g., "witness", "refinery", "deacon", "mayor"
	rigName       string        // Rig name (empty for town-level agents)
	sessionName   string        // tmux session name for cycling
	missing       []string      // What's missing from the settings
	wrongLocation bool          // True if file is in wrong location (should be deleted)
	missingFile   bool          // True if settings.local.json doesn't exist (needs agent restart)
	gitStatus     gitFileStatus // Git status for wrong-location files (for safe deletion)
}

// NewClaudeSettingsCheck creates a new Claude settings validation check.
func NewClaudeSettingsCheck() *ClaudeSettingsCheck {
	return &ClaudeSettingsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "claude-settings",
				CheckDescription: "Verify Claude settings.json files match expected templates",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

func (c *ClaudeSettingsCheck) Run(ctx *CheckContext) *CheckResult {
	result := runClaudeSettingsCheck(c, ctx)
	_ = c.staleSettings
	for _, sf := range c.staleSettings {
		_, _, _, _, _, _, _, _ = sf.path, sf.agentType, sf.rigName, sf.sessionName, sf.missing, sf.wrongLocation, sf.missingFile, sf.gitStatus
	}
	return result
}

func (c *ClaudeSettingsCheck) Fix(ctx *CheckContext) error {
	_ = c.staleSettings
	return fixClaudeSettings(c, ctx)
}

// fileExists checks if a file exists.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// roleUsesClaudeSettings reports whether the configured agent for role writes
// a Claude .claude/settings.json. Missing that file is only an error for
// Claude-backed roles.
func roleUsesClaudeSettings(townRoot, rigName, role string) bool {
	rigPath := ""
	if rigName != "" {
		rigPath = filepath.Join(townRoot, rigName)
	}
	agentName, _ := config.ResolveRoleAgentName(role, townRoot, rigPath)
	if agentName == "" {
		return true
	}
	townSettings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		townSettings = config.NewTownSettings()
	}
	var rigSettings *config.RigSettings
	if rigPath != "" {
		if loaded, loadErr := config.LoadRigSettings(config.RigSettingsPath(rigPath)); loadErr == nil {
			rigSettings = loaded
		}
	}
	preset := config.ResolveAgentPreset(agentName, townSettings, rigSettings)
	if preset == nil {
		return true
	}
	hooksProvider := preset.HooksProvider
	if hooksProvider == "" {
		hooksProvider = string(preset.Name)
	}
	return hooksProvider == "claude"
}

// isIdentityAnchor checks if a CLAUDE.md file is the Gas Town town-root
// identity file. This includes both the minimal bootstrap anchor (<20 lines)
// and the expanded version with operational norms (Dolt awareness,
// communication hygiene, etc.). Both formats are intentional Gas Town files
// and should NOT be flagged as "wrong location".
//
// A Gas Town CLAUDE.md is identified by:
// - Starting with "# Gas Town" (the standard header)
// - Containing "prime" (the recovery instruction)
func isIdentityAnchor(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return instructions.IsIdentityText(string(data))
}
