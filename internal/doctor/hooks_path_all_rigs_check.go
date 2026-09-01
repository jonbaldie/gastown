package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HooksPathAllRigsCheck verifies all clones across all rigs have core.hooksPath set.
// This runs globally (without --rig) to ensure the pre-push hook is active everywhere.
// The pre-push hook enforces integration branch landing guardrails.
type HooksPathAllRigsCheck struct {
	FixableCheck
	unconfiguredClones []string
}

// NewHooksPathAllRigsCheck creates a new global hooks path check.
func NewHooksPathAllRigsCheck() *HooksPathAllRigsCheck {
	return &HooksPathAllRigsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "hooks-path-all-rigs",
				CheckDescription: "Check core.hooksPath is set for all clones across all rigs",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks all clones in all rigs for core.hooksPath configuration.
func (c *HooksPathAllRigsCheck) Run(ctx *CheckContext) *CheckResult {
	rigs := findAllRigs(ctx.TownRoot)
	if len(rigs) == 0 {
		return hooksPathNoRigsResult(c)
	}

	c.unconfiguredClones = nil
	totalClones := c.inspectRigs(rigs)
	return c.result(ctx, totalClones)
}

func hooksPathNoRigsResult(c *HooksPathAllRigsCheck) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "No rigs found",
	}
}

func (c *HooksPathAllRigsCheck) inspectRigs(rigs []string) int {
	totalClones := 0
	for _, rigPath := range rigs {
		for _, clonePath := range findRigClones(rigPath) {
			if !usesHooks(clonePath) {
				continue
			}
			totalClones++
			if !hooksConfigured(clonePath) {
				c.unconfiguredClones = append(c.unconfiguredClones, clonePath)
			}
		}
	}
	return totalClones
}

func usesHooks(clonePath string) bool {
	_, err := os.Stat(filepath.Join(clonePath, ".githooks"))
	return !os.IsNotExist(err)
}

func hooksConfigured(clonePath string) bool {
	cmd := exec.Command("git", "-C", clonePath, "config", "--get", "core.hooksPath")
	output, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(output)) == ".githooks"
}

func (c *HooksPathAllRigsCheck) result(ctx *CheckContext, totalClones int) *CheckResult {
	if len(c.unconfiguredClones) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d clone(s) have hooks configured", totalClones),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d clone(s) missing core.hooksPath across all rigs", len(c.unconfiguredClones)),
		Details: relativeClonePaths(ctx.TownRoot, c.unconfiguredClones),
		FixHint: "Run 'gt doctor --fix' to configure hooks",
	}
}

func relativeClonePaths(townRoot string, clonePaths []string) []string {
	var details []string
	for _, clonePath := range clonePaths {
		relPath, _ := filepath.Rel(townRoot, clonePath)
		if relPath == "" {
			relPath = clonePath
		}
		details = append(details, relPath)
	}
	return details
}

// Fix configures core.hooksPath for all unconfigured clones.
func (c *HooksPathAllRigsCheck) Fix(_ *CheckContext) error {
	for _, clonePath := range c.unconfiguredClones {
		cmd := exec.Command("git", "-C", clonePath, "config", "core.hooksPath", ".githooks")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to configure hooks for %s: %w", clonePath, err)
		}
	}
	return nil
}

// findRigClones returns all git clone paths within a rig.
func findRigClones(rigPath string) []string {
	clones := []string{
		filepath.Join(rigPath, "mayor", "rig"),
		filepath.Join(rigPath, "refinery", "rig"),
	}
	clones = appendCrewClones(clones, filepath.Join(rigPath, "crew"))
	clones = appendPolecatClones(clones, filepath.Join(rigPath, "polecats"))
	return existingGitClones(clones)
}

func appendCrewClones(clones []string, crewDir string) []string {
	entries, err := os.ReadDir(crewDir)
	if err != nil {
		return clones
	}
	for _, entry := range entries {
		if entry.IsDir() {
			clones = append(clones, filepath.Join(crewDir, entry.Name()))
		}
	}
	return clones
}

func appendPolecatClones(clones []string, polecatDir string) []string {
	entries, err := os.ReadDir(polecatDir)
	if err != nil {
		return clones
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		clones = appendNestedPolecatClones(clones, filepath.Join(polecatDir, entry.Name()))
	}
	return clones
}

func appendNestedPolecatClones(clones []string, polecatDir string) []string {
	entries, err := os.ReadDir(polecatDir)
	if err != nil {
		return clones
	}
	for _, entry := range entries {
		if entry.IsDir() {
			clones = append(clones, filepath.Join(polecatDir, entry.Name()))
		}
	}
	return clones
}

func existingGitClones(clones []string) []string {
	var valid []string
	for _, clonePath := range clones {
		if _, err := os.Stat(filepath.Join(clonePath, ".git")); err == nil {
			valid = append(valid, clonePath)
		}
	}
	return valid
}
