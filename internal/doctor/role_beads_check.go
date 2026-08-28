package doctor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/jonbaldie/gastown/internal/config"
)

// RoleConfigCheck verifies that role configuration is valid.
// Role definitions are now config-based (internal/config/roles/*.toml),
// not stored as beads. Built-in defaults are embedded in the binary.
// This check validates any user-provided overrides at:
//   - <town>/roles/<role>.toml (town-level overrides)
//   - <rig>/roles/<role>.toml (rig-level overrides)
type RoleConfigCheck struct {
	BaseCheck
}

// NewRoleBeadsCheck creates a new role config check.
// Note: Function name kept as NewRoleBeadsCheck for backward compatibility
// with existing doctor.go registration code.
func NewRoleBeadsCheck() *RoleConfigCheck {
	return &RoleConfigCheck{
		BaseCheck: BaseCheck{
			CheckName:        "role-config-valid",
			CheckDescription: "Verify role configuration is valid",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run checks if role config is valid.
func (c *RoleConfigCheck) Run(ctx *CheckContext) *CheckResult {
	warnings, overrideCount := townRoleOverrides(ctx.TownRoot)
	rigWarnings, rigOverrides := rigRoleOverrides(ctx.TownRoot)
	warnings = append(warnings, rigWarnings...)
	overrideCount += rigOverrides
	return roleConfigResult(c, warnings, overrideCount)
}

func townRoleOverrides(townRoot string) ([]string, int) {
	return scanRoleOverrides(filepath.Join(townRoot, "roles"), "town override")
}

func rigRoleOverrides(townRoot string) ([]string, int) {
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return nil, 0
	}
	var warnings []string
	overrideCount := 0
	for _, entry := range entries {
		if !entry.IsDir() || !isRoleRig(townRoot, entry.Name()) {
			continue
		}
		rigWarnings, rigOverrides := scanRoleOverrides(
			filepath.Join(townRoot, entry.Name(), "roles"),
			fmt.Sprintf("rig %s override", entry.Name()),
		)
		warnings = append(warnings, rigWarnings...)
		overrideCount += rigOverrides
	}
	return warnings, overrideCount
}

func isRoleRig(townRoot, rigName string) bool {
	_, err := os.Stat(filepath.Join(townRoot, rigName, "rig.json"))
	return err == nil
}

func scanRoleOverrides(roleDir, label string) ([]string, int) {
	entries, err := os.ReadDir(roleDir)
	if err != nil {
		return nil, 0
	}
	var warnings []string
	overrideCount := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		overrideCount++
		if err := validateRoleOverride(filepath.Join(roleDir, entry.Name())); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s %s: %v", label, entry.Name(), err))
		}
	}
	return warnings, overrideCount
}

func roleConfigResult(c *RoleConfigCheck, warnings []string, overrideCount int) *CheckResult {
	if len(warnings) > 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusWarning,
			Message:  fmt.Sprintf("%d role config override(s) have issues", len(warnings)),
			Details:  warnings,
			FixHint:  "Check TOML syntax in role override files",
			Category: c.Category(),
		}
	}

	msg := "Role config uses built-in defaults"
	if overrideCount > 0 {
		msg = fmt.Sprintf("Role config valid (%d override file(s))", overrideCount)
	}

	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusOK,
		Message:  msg,
		Category: c.Category(),
	}
}

// validateRoleOverride checks if a role override file is valid TOML.
func validateRoleOverride(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var def config.RoleDefinition
	if err := toml.Unmarshal(data, &def); err != nil {
		return fmt.Errorf("invalid TOML: %w", err)
	}

	return nil
}
