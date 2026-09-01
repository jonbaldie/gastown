package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
)

// RigRoutesJSONLCheck detects and fixes routes.jsonl files in rig .beads directories.
//
// Rig-level routes.jsonl files are problematic because:
// 1. bd's routing walks up to find town root (via mayor/town.json) and uses town-level routes.jsonl
// 2. If a rig has its own routes.jsonl, bd uses it and never finds town routes, breaking cross-rig routing
// 3. These files often exist due to a bug where bd's auto-export wrote issue data to routes.jsonl
//
// Fix: Delete routes.jsonl unconditionally. The Dolt database is the source
// of truth.
type RigRoutesJSONLCheck struct {
	FixableCheck
	// affectedRigs tracks which rigs have routes.jsonl
	affectedRigs []rigRoutesInfo
}

type rigRoutesInfo struct {
	rigName    string
	routesPath string
}

// NewRigRoutesJSONLCheck creates a new check for rig-level routes.jsonl files.
func NewRigRoutesJSONLCheck() *RigRoutesJSONLCheck {
	return &RigRoutesJSONLCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "rig-routes-jsonl",
				CheckDescription: "Check for routes.jsonl in rig .beads directories",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks for routes.jsonl files in rig .beads directories.
func (c *RigRoutesJSONLCheck) Run(ctx *CheckContext) *CheckResult {
	c.affectedRigs = nil // Reset

	// Get list of rigs from multiple sources
	rigDirs := c.findRigDirectories(ctx.TownRoot)

	if len(rigDirs) == 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  "No rigs to check",
			Category: c.Category(),
		}
	}

	var problems []string

	for _, rigDir := range rigDirs {
		rigName := filepath.Base(rigDir)
		beadsDir := filepath.Join(rigDir, ".beads")
		routesPath := filepath.Join(beadsDir, beads.RoutesFileName)

		// Check if routes.jsonl exists in this rig's .beads directory
		if _, err := os.Stat(routesPath); os.IsNotExist(err) {
			continue // Good - no rig-level routes.jsonl
		}

		// routes.jsonl exists - it should be deleted
		problems = append(problems, fmt.Sprintf("%s: has routes.jsonl (will delete - breaks cross-rig routing)", rigName))

		c.affectedRigs = append(c.affectedRigs, rigRoutesInfo{
			rigName:    rigName,
			routesPath: routesPath,
		})
	}

	if len(c.affectedRigs) == 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  fmt.Sprintf("No rig-level routes.jsonl files (%d rigs checked)", len(rigDirs)),
			Category: c.Category(),
		}
	}

	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusWarning,
		Message:  fmt.Sprintf("%d rig(s) have routes.jsonl (breaks routing)", len(c.affectedRigs)),
		Details:  problems,
		FixHint:  "Run 'gt doctor --fix' to delete these files",
		Category: c.Category(),
	}
}

// Fix deletes routes.jsonl files in rig .beads directories.
// The Dolt database is the source of truth.
func (c *RigRoutesJSONLCheck) Fix(ctx *CheckContext) error {
	// Re-run check to populate affectedRigs if needed
	if len(c.affectedRigs) == 0 {
		result := c.Run(ctx)
		if result.Status == StatusOK {
			return nil // Nothing to fix
		}
	}

	for _, info := range c.affectedRigs {
		if err := os.Remove(info.routesPath); err != nil {
			return fmt.Errorf("deleting %s: %w", info.routesPath, err)
		}
	}

	return nil
}

// findRigDirectories finds all rig directories in the town.
func (c *RigRoutesJSONLCheck) findRigDirectories(townRoot string) []string {
	var rigDirs []string
	seen := make(map[string]bool)
	rigDirs = appendRegistryRigDirs(rigDirs, seen, townRoot)
	rigDirs = appendRouteRigDirs(rigDirs, seen, townRoot)
	rigDirs = appendDiscoveredRigDirs(rigDirs, seen, townRoot)
	return rigDirs
}

func appendRegistryRigDirs(rigDirs []string, seen map[string]bool, townRoot string) []string {
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		return rigDirs
	}
	for rigName := range rigsConfig.Rigs {
		rigPath := filepath.Join(townRoot, rigName)
		rigDirs = appendExistingRigDir(rigDirs, seen, rigPath)
	}
	return rigDirs
}

func appendRouteRigDirs(rigDirs []string, seen map[string]bool, townRoot string) []string {
	townBeadsDir := filepath.Join(townRoot, ".beads")
	routes, err := beads.LoadRoutes(townBeadsDir)
	if err != nil {
		return rigDirs
	}
	for _, route := range routes {
		rigPath, ok := routeRigPath(townRoot, route.Path)
		if !ok {
			continue
		}
		rigDirs = appendExistingRigDir(rigDirs, seen, rigPath)
	}
	return rigDirs
}

func routeRigPath(townRoot, routePath string) (string, bool) {
	if routePath == "." || routePath == "" {
		return "", false
	}
	parts := strings.Split(routePath, "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", false
	}
	return filepath.Join(townRoot, parts[0]), true
}

func appendDiscoveredRigDirs(rigDirs []string, seen map[string]bool, townRoot string) []string {
	townBeadsDir := filepath.Join(townRoot, ".beads")
	townBeadsInfo, townBeadsErr := os.Stat(townBeadsDir)
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return rigDirs
	}
	for _, entry := range entries {
		rigPath, ok := discoveredRigPath(townRoot, entry)
		if !ok {
			continue
		}
		beadsDirInfo, err := os.Stat(filepath.Join(rigPath, ".beads"))
		if err != nil {
			continue
		}
		if townBeadsErr == nil && os.SameFile(townBeadsInfo, beadsDirInfo) {
			continue
		}
		rigDirs = appendExistingRigDir(rigDirs, seen, rigPath)
	}
	return rigDirs
}

func discoveredRigPath(townRoot string, entry os.DirEntry) (string, bool) {
	if !entry.IsDir() {
		return "", false
	}
	switch entry.Name() {
	case "mayor", ".beads", ".git":
		return "", false
	}
	return filepath.Join(townRoot, entry.Name()), true
}

func appendExistingRigDir(rigDirs []string, seen map[string]bool, rigPath string) []string {
	if _, err := os.Stat(rigPath); err != nil || seen[rigPath] {
		return rigDirs
	}
	seen[rigPath] = true
	return append(rigDirs, rigPath)
}
