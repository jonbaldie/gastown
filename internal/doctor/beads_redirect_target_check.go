package doctor

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
)

// BeadsRedirectTargetCheck validates that .beads/redirect files in crew/polecat/refinery
// worktrees point to targets that actually exist and have a working beads setup.
//
// This catches setup issues when cloning to a new machine where redirects might
// reference paths that don't exist yet (e.g., canonical beads location not initialized).
type BeadsRedirectTargetCheck struct {
	FixableCheck
	brokenTargets []brokenTarget // Cached for Fix
}

// brokenTarget represents a redirect whose target is missing or broken.
type brokenTarget struct {
	worktreePath string // Full path to the worktree
	target       string // Raw content of the redirect file
	resolvedPath string // Resolved absolute path of the target
	reason       string // Why the target is broken
}

// NewBeadsRedirectTargetCheck creates a new beads redirect target check.
func NewBeadsRedirectTargetCheck() *BeadsRedirectTargetCheck {
	return &BeadsRedirectTargetCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "beads-redirect-target",
				CheckDescription: "Check that beads redirect targets exist and are accessible",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks all worktree redirect files to verify their targets exist and are valid.
func (c *BeadsRedirectTargetCheck) Run(ctx *CheckContext) *CheckResult {
	rigDirs, err := findRigDirs(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("Could not scan rigs: %v", err),
		}
	}

	broken := findBrokenRedirectTargets(ctx.TownRoot, rigDirs)

	c.brokenTargets = broken

	if len(broken) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "All beads redirect targets are valid",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d broken redirect target(s)", len(broken)),
		Details: brokenRedirectDetails(ctx.TownRoot, broken),
		FixHint: "Run 'gt doctor --fix' to repair redirects, or 'bd init' to initialize beads",
	}
}

func findBrokenRedirectTargets(townRoot string, rigDirs []string) []brokenTarget {
	var broken []brokenTarget
	for _, rigDir := range rigDirs {
		for _, worktreePath := range getWorktreePaths(rigDir) {
			if target, ok := inspectRedirectTarget(townRoot, worktreePath); ok {
				broken = append(broken, target)
			}
		}
	}
	return broken
}

func inspectRedirectTarget(townRoot, worktreePath string) (brokenTarget, bool) {
	if !dirExists(worktreePath) {
		return brokenTarget{}, false
	}
	data, err := os.ReadFile(filepath.Join(worktreePath, ".beads", "redirect"))
	if err != nil {
		// No redirect file — not our concern (StaleBeadsRedirectCheck handles missing redirects).
		return brokenTarget{}, false
	}
	target := strings.TrimSpace(string(data))
	if target == "" {
		return brokenTarget{}, false
	}
	resolved := resolveRedirectTarget(worktreePath, target)
	return brokenRedirectTarget(townRoot, worktreePath, target, resolved)
}

func resolveRedirectTarget(worktreePath, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(worktreePath, target))
}

func brokenRedirectTarget(townRoot, worktreePath, target, resolved string) (brokenTarget, bool) {
	relWorktree, _ := filepath.Rel(townRoot, worktreePath)
	info, err := os.Stat(resolved)
	switch {
	case os.IsNotExist(err):
		return brokenTarget{worktreePath: worktreePath, target: target, resolvedPath: resolved, reason: fmt.Sprintf("target does not exist: %s", relWorktree)}, true
	case err != nil:
		return brokenTarget{worktreePath: worktreePath, target: target, resolvedPath: resolved, reason: fmt.Sprintf("target not accessible: %s (%v)", relWorktree, err)}, true
	case !info.IsDir():
		return brokenTarget{worktreePath: worktreePath, target: target, resolvedPath: resolved, reason: fmt.Sprintf("target is not a directory: %s", relWorktree)}, true
	case !hasBeadsSetup(resolved):
		return brokenTarget{worktreePath: worktreePath, target: target, resolvedPath: resolved, reason: fmt.Sprintf("target has no beads setup: %s", relWorktree)}, true
	default:
		return brokenTarget{}, false
	}
}

func brokenRedirectDetails(townRoot string, broken []brokenTarget) []string {
	details := make([]string, 0, len(broken))
	for _, target := range broken {
		relWorktree, _ := filepath.Rel(townRoot, target.worktreePath)
		relTarget, _ := filepath.Rel(townRoot, target.resolvedPath)
		details = append(details, fmt.Sprintf("%s → %s (%s)", relWorktree, relTarget, target.reason))
	}
	return details
}

// Fix attempts to repair broken redirect targets by recomputing redirects.
// If the canonical beads location exists, SetupRedirect will point to it.
// If a target directory exists but is missing config.yaml, it is created
// from metadata.json defaults (this handles partial rig setups where the
// dolt database was initialized but config.yaml was not written).
// If no canonical location exists, the fix cannot proceed automatically.
func (c *BeadsRedirectTargetCheck) Fix(ctx *CheckContext) error {
	var unfixable []string

	for _, target := range c.brokenTargets {
		if err := fixBrokenRedirectTarget(ctx, target); err != nil {
			relWorktree, _ := filepath.Rel(ctx.TownRoot, target.worktreePath)
			if relWorktree == "." || relWorktree == "" {
				relWorktree = target.worktreePath
			}
			unfixable = append(unfixable, relWorktree)
		}
	}

	if len(unfixable) > 0 {
		return fmt.Errorf("could not fix %d redirect(s) (no canonical beads found): %s",
			len(unfixable), strings.Join(unfixable, ", "))
	}

	return nil
}

func fixBrokenRedirectTarget(ctx *CheckContext, target brokenTarget) error {
	// If the redirect target directory exists but lacks beads setup, try to create
	// config.yaml from metadata.json before recomputing the redirect.
	if strings.Contains(target.reason, "no beads setup") && dirExists(target.resolvedPath) {
		rigName := extractRigName(ctx.TownRoot, target.worktreePath)
		if err := beads.EnsureConfigYAMLFromMetadataIfMissing(target.resolvedPath, rigName); err != nil {
			log.Printf("[doctor] beads-redirect-target: could not create config.yaml from metadata in %s: %v", target.resolvedPath, err)
		} else if hasBeadsSetup(target.resolvedPath) {
			return nil
		}
	}

	rigBeads, mayorBeads, ok := canonicalBeadsPaths(ctx.TownRoot, target.worktreePath)
	if !ok {
		return fmt.Errorf("invalid worktree path")
	}
	if !hasBeadsSetup(rigBeads) && !hasBeadsSetup(mayorBeads) {
		return fmt.Errorf("no canonical beads found")
	}
	return recomputeRedirect(ctx.TownRoot, target.worktreePath)
}

func canonicalBeadsPaths(townRoot, worktreePath string) (string, string, bool) {
	relPath, err := filepath.Rel(townRoot, worktreePath)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	rigRoot := filepath.Join(townRoot, parts[0])
	return filepath.Join(rigRoot, ".beads"), filepath.Join(rigRoot, "mayor", "rig", ".beads"), true
}

// extractRigName derives the rig name from a worktree path within a town.
// For example, "/town/myrig/refinery/rig" returns "myrig".
func extractRigName(townRoot, worktreePath string) string {
	relPath, err := filepath.Rel(townRoot, worktreePath)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// hasBeadsSetup checks whether a .beads directory has a working setup.
// A valid beads directory should have at least one of:
// - dolt/ directory (dolt database)
// - redirect file (chain to another location)
// - config.yaml (beads configuration)
func hasBeadsSetup(beadsDir string) bool {
	markers := []string{"dolt", "redirect", "config.yaml"}
	for _, marker := range markers {
		if _, err := os.Stat(filepath.Join(beadsDir, marker)); err == nil {
			return true
		}
	}
	return false
}

// recomputeRedirect rewrites a worktree's .beads/redirect to point to the correct target.
func recomputeRedirect(townRoot, worktreePath string) error {
	relPath, err := filepath.Rel(townRoot, worktreePath)
	if err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) < 2 {
		return fmt.Errorf("invalid worktree path")
	}

	rigRoot := filepath.Join(townRoot, parts[0])
	rigBeads := filepath.Join(rigRoot, ".beads")
	mayorBeads := filepath.Join(rigRoot, "mayor", "rig", ".beads")

	// Compute depth from worktree to rig root
	depth := len(parts) - 1
	upPath := strings.Repeat("../", depth)

	var redirectContent string
	switch {
	case hasBeadsSetup(rigBeads):
		// Check if rig beads has its own redirect (tracked beads case)
		rigRedirectFile := filepath.Join(rigBeads, "redirect")
		if data, err := os.ReadFile(rigRedirectFile); err == nil {
			rigTarget := strings.TrimSpace(string(data))
			if rigTarget != "" {
				// Skip intermediate hop, redirect directly to final destination.
				// Absolute paths are passed through as-is (same logic as beads.ComputeRedirectTarget).
				if filepath.IsAbs(rigTarget) {
					redirectContent = rigTarget
				} else {
					redirectContent = upPath + rigTarget
				}
				break
			}
		}
		redirectContent = upPath + ".beads"
	case hasBeadsSetup(mayorBeads):
		redirectContent = upPath + "mayor/rig/.beads"
	default:
		return fmt.Errorf("no valid beads location found")
	}

	// Ensure .beads directory exists
	worktreeBeads := filepath.Join(worktreePath, ".beads")
	if err := os.MkdirAll(worktreeBeads, 0755); err != nil {
		return err
	}

	// Write redirect file
	redirectFile := filepath.Join(worktreeBeads, "redirect")
	return os.WriteFile(redirectFile, []byte(redirectContent+"\n"), 0644)
}
