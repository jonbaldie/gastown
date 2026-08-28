package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
)

// IdleTimeoutCheck verifies that all rigs have dolt.idle-timeout set to "0"
// to prevent per-rig idle-monitors from spawning duplicate Dolt servers.
// Gas Town uses a centralized Dolt server managed by systemd.
type IdleTimeoutCheck struct {
	FixableCheck
}

// NewIdleTimeoutCheck creates a new idle timeout check.
func NewIdleTimeoutCheck() *IdleTimeoutCheck {
	return &IdleTimeoutCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "idle-timeout-config",
				CheckDescription: "Verify all rigs have dolt.idle-timeout set to \"0\" (centralized Dolt)",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if all rigs have dolt.idle-timeout set to "0".
func (c *IdleTimeoutCheck) Run(ctx *CheckContext) *CheckResult {
	// Load routes to get rig info
	townBeadsDir := filepath.Join(ctx.TownRoot, ".beads")
	routes, err := beads.LoadRoutes(townBeadsDir)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not load routes.jsonl",
		}
	}

	rigSet := uniqueRigSet(routes)

	if len(rigSet) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs to check",
		}
	}

	missing, checked := inspectIdleTimeoutRigs(ctx.TownRoot, rigSet)
	return idleTimeoutResult(c.Name(), missing, checked)
}

func uniqueRigSet(routes []beads.Route) map[string]string {
	rigSet := make(map[string]string)
	for _, route := range routes {
		parts := strings.Split(route.Path, "/")
		if len(parts) >= 1 && parts[0] != "." {
			rigName := parts[0]
			if _, exists := rigSet[rigName]; !exists {
				rigSet[rigName] = route.Path
			}
		}
	}
	return rigSet
}

func inspectIdleTimeoutRigs(townRoot string, rigSet map[string]string) ([]string, int) {
	var missing []string
	checked := 0
	for rigName, beadsPath := range rigSet {
		missingName, isMissing := inspectIdleTimeoutRig(townRoot, rigName, beadsPath)
		if isMissing {
			missing = append(missing, missingName)
		}
		checked++
	}
	return missing, checked
}

func inspectIdleTimeoutRig(townRoot, rigName, beadsPath string) (string, bool) {
	rigPath := filepath.Join(townRoot, beadsPath)
	configPath := filepath.Join(rigPath, ".beads", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Sprintf("%s (config.yaml missing)", rigName), true
	}
	return rigName, !hasIdleTimeout(string(data))
}

func hasIdleTimeout(content string) bool {
	return strings.Contains(content, "dolt.idle-timeout:") &&
		strings.Contains(content, "dolt.idle-timeout: \"0\"")
}

func idleTimeoutResult(name string, missing []string, checked int) *CheckResult {
	if len(missing) == 0 {
		return &CheckResult{
			Name:    name,
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d rigs have dolt.idle-timeout set to \"0\"", checked),
		}
	}

	return &CheckResult{
		Name:    name,
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d rig(s) missing dolt.idle-timeout: \"0\"", len(missing)),
		Details: missing,
		FixHint: "Run 'gt doctor --fix' to add idle-timeout config to all rigs",
	}
}

// Fix adds dolt.idle-timeout: "0" to all rig config.yaml files.
func (c *IdleTimeoutCheck) Fix(ctx *CheckContext) error {
	// Load routes to get rig info
	townBeadsDir := filepath.Join(ctx.TownRoot, ".beads")
	routes, err := beads.LoadRoutes(townBeadsDir)
	if err != nil {
		return fmt.Errorf("loading routes.jsonl: %w", err)
	}

	rigSet := uniqueRigSet(routes)

	// Fix each rig
	for rigName, beadsPath := range rigSet {
		// beadsPath from routes is the rig path (e.g., "gastown/mayor/rig" or "gastown")
		rigPath := filepath.Join(ctx.TownRoot, beadsPath)
		// The .beads directory is within the rig path
		rigBeadsPath := filepath.Join(rigPath, ".beads")

		// Use EnsureConfigYAML to add idle-timeout if missing
		// This is idempotent - won't modify if already correct
		if err := beads.EnsureConfigYAML(rigBeadsPath, ""); err != nil {
			return fmt.Errorf("fixing %s: %w", rigName, err)
		}
	}

	return nil
}
