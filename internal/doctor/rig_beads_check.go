package doctor

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/rig"
)

// RigBeadsCheck verifies that rig identity beads exist for all rigs.
// Rig identity beads track rig metadata like git URL, prefix, and operational state.
// They are created by gt rig add (see gt-zmznh) but may be missing for legacy rigs.
type RigBeadsCheck struct {
	FixableCheck
}

type rigRouteInfo struct {
	prefix    string
	beadsPath string
}

// NewRigBeadsCheck creates a new rig identity beads check.
func NewRigBeadsCheck() *RigBeadsCheck {
	return &RigBeadsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "rig-beads-exist",
				CheckDescription: "Verify rig identity beads exist for all rigs",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if rig identity beads exist for all rigs.
func (c *RigBeadsCheck) Run(ctx *CheckContext) *CheckResult {
	routes, err := loadRigRoutes(ctx.TownRoot)
	if err != nil {
		return rigBeadsRoutesResult(c)
	}

	rigSet := uniqueRigRoutes(routes)

	if len(rigSet) == 0 {
		return noRigBeadsResult(c)
	}

	missing, checked := missingRigBeads(ctx.TownRoot, rigSet)
	return rigBeadsResult(c, missing, checked)
}

func loadRigRoutes(townRoot string) ([]beads.Route, error) {
	return beads.LoadRoutes(filepath.Join(townRoot, ".beads"))
}

func rigBeadsRoutesResult(c *RigBeadsCheck) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: "Could not load routes.jsonl",
	}
}

func uniqueRigRoutes(routes []beads.Route) map[string]rigRouteInfo {
	rigSet := make(map[string]rigRouteInfo)
	for _, route := range routes {
		parts := strings.Split(route.Path, "/")
		if len(parts) == 0 || parts[0] == "." {
			continue
		}
		rigName := parts[0]
		if _, exists := rigSet[rigName]; exists {
			continue
		}
		rigSet[rigName] = rigRouteInfo{
			prefix:    strings.TrimSuffix(route.Prefix, "-"),
			beadsPath: route.Path,
		}
	}
	return rigSet
}

func noRigBeadsResult(c *RigBeadsCheck) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "No rigs to check",
	}
}

func missingRigBeads(townRoot string, rigSet map[string]rigRouteInfo) ([]string, int) {
	var missing []string
	checked := 0
	for rigName, info := range rigSet {
		rigBeadsPath := filepath.Join(townRoot, info.beadsPath)
		bd := beads.New(rigBeadsPath)
		rigBeadID := beads.RigBeadIDWithPrefix(info.prefix, rigName)
		if _, err := bd.Show(rigBeadID); err != nil {
			missing = append(missing, rigBeadID)
		}
		checked++
	}
	return missing, checked
}

func rigBeadsResult(c *RigBeadsCheck, missing []string, checked int) *CheckResult {
	if len(missing) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d rig identity beads exist", checked),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("%d rig identity bead(s) missing", len(missing)),
		Details: missing,
		FixHint: "Run 'gt doctor --fix' to create missing rig identity beads",
	}
}

// Fix creates missing rig identity beads.
func (c *RigBeadsCheck) Fix(ctx *CheckContext) error {
	routes, err := loadRigRoutes(ctx.TownRoot)
	if err != nil {
		return fmt.Errorf("loading routes.jsonl: %w", err)
	}

	rigSet := uniqueRigRoutes(routes)

	if len(rigSet) == 0 {
		return nil // No rigs to process
	}
	return ensureRigBeads(ctx, rigSet)
}

func ensureRigBeads(ctx *CheckContext, rigSet map[string]rigRouteInfo) error {
	var errs []error
	for rigName, info := range rigSet {
		if err := ensureRigBead(ctx, rigName, info); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func ensureRigBead(ctx *CheckContext, rigName string, info rigRouteInfo) error {
	rigBeadsPath := filepath.Join(ctx.TownRoot, info.beadsPath)
	bd := beads.New(rigBeadsPath)
	gitURL := rigGitURL(filepath.Join(ctx.TownRoot, rigName))
	fields := &beads.RigFields{
		Repo:   gitURL,
		Prefix: info.prefix,
		State:  beads.RigStateActive,
	}
	rigBeadID := beads.RigBeadIDWithPrefix(info.prefix, rigName)
	if _, err := bd.EnsureRigBead(rigName, fields); err != nil {
		return fmt.Errorf("ensuring %s: %w", rigBeadID, err)
	}
	return nil
}

func rigGitURL(rigPath string) string {
	cfg, err := rig.LoadRigConfig(rigPath)
	if err != nil {
		return ""
	}
	return cfg.GitURL
}
