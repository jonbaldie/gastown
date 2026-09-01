package doctor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jonbaldie/gastown/internal/git"
)

// SparseCheckoutCheck detects legacy sparse checkout configurations that should be removed.
// Sparse checkout was previously used to exclude .claude/ from source repos, but this
// prevented valid .claude/ files in rigged repos from being used. Now that gastown's
// repo no longer has .claude/ files, sparse checkout is no longer needed.
//
// This check runs in both modes:
//   - With --rig: checks only the specified rig
//   - Without --rig: iterates over all rig directories in the town root
type SparseCheckoutCheck struct {
	FixableCheck
	townRoot      string
	affectedRepos []string // repos with legacy sparse checkout that should be removed
}

// NewSparseCheckoutCheck creates a new sparse checkout check.
func NewSparseCheckoutCheck() *SparseCheckoutCheck {
	return &SparseCheckoutCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "sparse-checkout",
				CheckDescription: "Check for legacy sparse checkout configuration that should be removed",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if any git repos have legacy sparse checkout configured.
func (c *SparseCheckoutCheck) Run(ctx *CheckContext) *CheckResult {
	c.townRoot = ctx.TownRoot
	c.affectedRepos = nil

	// Collect rig paths to check
	var rigPaths []string
	if ctx.RigPath() != "" {
		// Single-rig mode
		rigPaths = []string{ctx.RigPath()}
	} else {
		// Town-wide mode: discover all rig directories
		rigPaths = c.discoverRigPaths(ctx.TownRoot)
	}

	if len(rigPaths) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs found to check",
		}
	}

	// Check all rigs for legacy sparse checkout
	for _, rigPath := range rigPaths {
		c.checkRig(rigPath)
	}

	if len(c.affectedRepos) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No legacy sparse checkout configurations found",
		}
	}

	// Build details with relative paths from town root
	var details []string
	for _, repoPath := range c.affectedRepos {
		relPath, _ := filepath.Rel(c.townRoot, repoPath)
		if relPath == "" {
			relPath = repoPath
		}
		details = append(details, relPath)
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d repo(s) have legacy sparse checkout that should be removed", len(c.affectedRepos)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to remove sparse checkout and restore .claude/ files",
	}
}

// discoverRigPaths finds all rig directories in the town root.
// Skips known non-rig directories (mayor, deacon, daemon, .git, etc.).
func (c *SparseCheckoutCheck) discoverRigPaths(townRoot string) []string {
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return nil
	}

	var rigPaths []string
	for _, entry := range entries {
		rigPath, ok := sparseRigPath(townRoot, entry)
		if !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(rigPath, "config.json")); err == nil {
			rigPaths = append(rigPaths, rigPath)
		}
	}
	return rigPaths
}

func sparseRigPath(townRoot string, entry os.DirEntry) (string, bool) {
	if !entry.IsDir() || sparseRigNameExcluded(entry.Name()) {
		return "", false
	}
	return filepath.Join(townRoot, entry.Name()), true
}

func sparseRigNameExcluded(name string) bool {
	switch name {
	case "mayor", "deacon", "daemon", ".git", "docs":
		return true
	default:
		return len(name) > 0 && name[0] == '.'
	}
}

// checkRig checks all worktree repos within a single rig for legacy sparse checkout.
func (c *SparseCheckoutCheck) checkRig(rigPath string) {
	repoPaths := sparseRigRepos(rigPath)
	for _, repoPath := range repoPaths {
		if sparseCheckoutNeedsCheck(repoPath) && git.IsSparseCheckoutConfigured(repoPath) {
			c.affectedRepos = append(c.affectedRepos, repoPath)
		}
	}
}

func sparseRigRepos(rigPath string) []string {
	repoPaths := []string{
		filepath.Join(rigPath, "mayor", "rig"),
		filepath.Join(rigPath, "refinery", "rig"),
	}
	repoPaths = appendSparseCrewRepos(repoPaths, filepath.Join(rigPath, "crew"))
	return appendSparsePolecatRepos(repoPaths, filepath.Join(rigPath, "polecats"), filepath.Base(rigPath))
}

func appendSparseCrewRepos(repoPaths []string, crewDir string) []string {
	entries, err := os.ReadDir(crewDir)
	if err != nil {
		return repoPaths
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "README.md" {
			repoPaths = append(repoPaths, filepath.Join(crewDir, entry.Name()))
		}
	}
	return repoPaths
}

func appendSparsePolecatRepos(repoPaths []string, polecatDir, rigName string) []string {
	entries, err := os.ReadDir(polecatDir)
	if err != nil {
		return repoPaths
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		entryPath := filepath.Join(polecatDir, entry.Name())
		worktreePath := filepath.Join(entryPath, rigName)
		if _, err := os.Stat(worktreePath); err == nil {
			repoPaths = append(repoPaths, worktreePath)
		} else {
			repoPaths = append(repoPaths, entryPath)
		}
	}
	return repoPaths
}

func sparseCheckoutNeedsCheck(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, ".git"))
	return !os.IsNotExist(err)
}

// Fix removes sparse checkout configuration from affected repos.
func (c *SparseCheckoutCheck) Fix(_ *CheckContext) error {
	for _, repoPath := range c.affectedRepos {
		if err := git.RemoveSparseCheckout(repoPath); err != nil {
			relPath, _ := filepath.Rel(c.townRoot, repoPath)
			return fmt.Errorf("failed to remove sparse checkout for %s: %w", relPath, err)
		}
	}
	return nil
}
