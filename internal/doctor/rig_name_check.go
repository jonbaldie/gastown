package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RigNameMismatchCheck detects when a rig's config.json has a name or beads
// prefix that doesn't match the authoritative sources (directory name and
// rigs.json registry respectively).
type RigNameMismatchCheck struct {
	FixableCheck
}

// NewRigNameMismatchCheck creates a new rig name mismatch check.
func NewRigNameMismatchCheck() *RigNameMismatchCheck {
	return &RigNameMismatchCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "rig-name-mismatch",
				CheckDescription: "Check rig config.json name and prefix match directory and registry",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// rigConfigLocal is a local type for reading/writing rig config.json without
// importing the rig package (avoids circular dependency).
type rigConfigLocal struct {
	Type      string               `json:"type"`
	Version   int                  `json:"version"`
	Name      string               `json:"name"`
	GitURL    string               `json:"git_url"`
	LocalRepo string               `json:"local_repo,omitempty"`
	CreatedAt json.RawMessage      `json:"created_at"`
	Beads     *rigConfigBeadsLocal `json:"beads,omitempty"`

	// Preserve unknown fields for round-trip fidelity
	DefaultBranch string `json:"default_branch,omitempty"`
}

type rigConfigBeadsLocal struct {
	Prefix string `json:"prefix"`
}

func loadRigConfigLocal(rigPath string) (*rigConfigLocal, error) {
	configPath := filepath.Join(rigPath, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg rigConfigLocal
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveRigConfigLocal(rigPath string, cfg *rigConfigLocal) error {
	configPath := filepath.Join(rigPath, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

// Run checks for name/prefix mismatches between config.json and the
// authoritative sources (directory name and rigs.json).
func (c *RigNameMismatchCheck) Run(ctx *CheckContext) *CheckResult {
	if ctx.RigName == "" {
		return rigNameSkippedResult(c, "No rig specified (skipped)")
	}

	rigPath := ctx.RigPath()
	cfg, err := loadRigConfigLocal(rigPath)
	if err != nil {
		return rigNameSkippedResult(c, "No config.json found (skipped)")
	}

	details := rigNameMismatchDetails(ctx, cfg)
	return rigNameResult(c, details)
}

func rigNameSkippedResult(c *RigNameMismatchCheck, message string) *CheckResult {
	return &CheckResult{Name: c.Name(), Status: StatusOK, Message: message}
}

func rigNameMismatchDetails(ctx *CheckContext, cfg *rigConfigLocal) []string {
	var details []string
	if cfg.Name != ctx.RigName {
		details = append(details, fmt.Sprintf(
			"config.json name is %q but directory is %q", cfg.Name, ctx.RigName))
	}
	if detail, ok := rigPrefixMismatch(ctx, cfg); ok {
		details = append(details, detail)
	}
	return details
}

func rigPrefixMismatch(ctx *CheckContext, cfg *rigConfigLocal) (string, bool) {
	expected, ok := expectedRigPrefix(ctx)
	if !ok || cfg.Beads == nil || cfg.Beads.Prefix == "" || cfg.Beads.Prefix == expected {
		return "", false
	}
	return fmt.Sprintf(
		"config.json beads prefix is %q but rigs.json says %q",
		cfg.Beads.Prefix, expected), true
}

func expectedRigPrefix(ctx *CheckContext) (string, bool) {
	rigsPath := filepath.Join(ctx.TownRoot, "mayor", "rigs.json")
	rigsConfig, err := loadRigsConfig(rigsPath)
	if err != nil {
		return "", false
	}
	entry, ok := rigsConfig.Rigs[ctx.RigName]
	if !ok || entry.BeadsConfig == nil || entry.BeadsConfig.Prefix == "" {
		return "", false
	}
	return entry.BeadsConfig.Prefix, true
}

func rigNameResult(c *RigNameMismatchCheck, details []string) *CheckResult {
	if len(details) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Rig config name and prefix match directory and registry",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d rig config mismatch(es) found", len(details)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to update config.json to match directory name and registry prefix",
	}
}

// Fix updates config.json to match the directory name and rigs.json prefix.
func (c *RigNameMismatchCheck) Fix(ctx *CheckContext) error {
	if ctx.RigName == "" {
		return nil
	}

	rigPath := ctx.RigPath()
	cfg, err := loadRigConfigLocal(rigPath)
	if err != nil {
		return nil // Nothing to fix
	}

	modified := updateRigName(cfg, ctx.RigName)
	modified = updateRigPrefix(cfg, ctx) || modified

	if modified {
		return saveRigConfigLocal(rigPath, cfg)
	}

	return nil
}

func updateRigName(cfg *rigConfigLocal, rigName string) bool {
	if cfg.Name == rigName {
		return false
	}
	cfg.Name = rigName
	return true
}

func updateRigPrefix(cfg *rigConfigLocal, ctx *CheckContext) bool {
	prefix, ok := expectedRigPrefix(ctx)
	if !ok || cfg.Beads == nil || cfg.Beads.Prefix == "" || cfg.Beads.Prefix == prefix {
		return false
	}
	cfg.Beads.Prefix = prefix
	return true
}
