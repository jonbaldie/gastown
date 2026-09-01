package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
)

// RigIsGitRepoCheck verifies the rig has a valid mayor/rig git clone.
// Note: The rig directory itself is not a git repo - it contains clones.
type RigIsGitRepoCheck struct {
	BaseCheck
}

// NewRigIsGitRepoCheck creates a new rig git repo check.
func NewRigIsGitRepoCheck() *RigIsGitRepoCheck {
	return &RigIsGitRepoCheck{
		BaseCheck: BaseCheck{
			CheckName:        "rig-is-git-repo",
			CheckDescription: "Verify rig has a valid mayor/rig git clone",
			CheckCategory:    CategoryRig,
		},
	}
}

// Run checks if the rig has a valid mayor/rig git clone.
func (c *RigIsGitRepoCheck) Run(ctx *CheckContext) *CheckResult {
	rigPath := ctx.RigPath()
	if rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No rig specified",
		}
	}

	// Check mayor/rig/ which is the authoritative clone for the rig
	mayorRigPath := filepath.Join(rigPath, "mayor", "rig")
	gitPath := filepath.Join(mayorRigPath, ".git")
	info, err := os.Stat(gitPath)
	if os.IsNotExist(err) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No mayor/rig clone found",
			Details: []string{fmt.Sprintf("Missing: %s", gitPath)},
			FixHint: "Clone the repository to mayor/rig/",
		}
	}
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("Cannot access mayor/rig/.git: %v", err),
		}
	}

	// Verify git status works
	cmd := exec.Command("git", "-C", mayorRigPath, "status", "--porcelain")
	if err := cmd.Run(); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "git status failed on mayor/rig",
			Details: []string{fmt.Sprintf("Error: %v", err)},
			FixHint: "Check git configuration and repository integrity",
		}
	}

	gitType := "clone"
	if info.Mode().IsRegular() {
		gitType = "worktree"
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("Valid mayor/rig %s", gitType),
	}
}

// GitExcludeConfiguredCheck verifies .git/info/exclude has Gas Town directories.
type GitExcludeConfiguredCheck struct {
	FixableCheck
	missingEntries []string
	excludePath    string
}

// NewGitExcludeConfiguredCheck creates a new git exclude check.
func NewGitExcludeConfiguredCheck() *GitExcludeConfiguredCheck {
	return &GitExcludeConfiguredCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "git-exclude-configured",
				CheckDescription: "Check .git/info/exclude has Gas Town directories",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if .git/info/exclude contains required entries.
func (c *GitExcludeConfiguredCheck) Run(ctx *CheckContext) *CheckResult {
	gitDir, result := gitExcludeMayorGitDir(c, ctx)
	if result != nil {
		return result
	}
	loadGitExcludeMissing(c, gitDir)
	_ = c.excludePath
	_ = c.missingEntries
	return reportGitExclude(c)
}

// Fix appends missing entries to .git/info/exclude.
func (c *GitExcludeConfiguredCheck) Fix(_ *CheckContext) error {
	if len(c.missingEntries) == 0 {
		return nil
	}

	// Ensure info directory exists
	infoDir := filepath.Dir(c.excludePath)
	if err := os.MkdirAll(infoDir, 0755); err != nil {
		return fmt.Errorf("failed to create info directory: %w", err)
	}

	// Append missing entries
	f, err := os.OpenFile(c.excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open exclude file: %w", err)
	}
	defer f.Close()

	// Add a header comment if file is empty or new
	info, _ := f.Stat()
	if info.Size() == 0 {
		if _, err := f.WriteString("# Gas Town directories\n"); err != nil {
			return err
		}
	} else {
		// Add newline before new entries
		if _, err := f.WriteString("\n# Gas Town directories\n"); err != nil {
			return err
		}
	}

	for _, entry := range c.missingEntries {
		if _, err := f.WriteString(entry + "\n"); err != nil {
			return err
		}
	}

	return nil
}

// HooksPathConfiguredCheck verifies all clones have core.hooksPath set to .githooks.
// This ensures the pre-push hook blocks pushes to invalid branches (no internal PRs).
type HooksPathConfiguredCheck struct {
	FixableCheck
	unconfiguredClones []string
}

// NewHooksPathConfiguredCheck creates a new hooks path check.
func NewHooksPathConfiguredCheck() *HooksPathConfiguredCheck {
	return &HooksPathConfiguredCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "hooks-path-configured",
				CheckDescription: "Check core.hooksPath is set for all clones",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if all clones have core.hooksPath configured.
func (c *HooksPathConfiguredCheck) Run(ctx *CheckContext) *CheckResult {
	rigPath := ctx.RigPath()
	if rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No rig specified",
		}
	}
	c.unconfiguredClones = nil
	collectUnconfiguredHookClones(c, rigPath)
	_ = c.unconfiguredClones
	return reportHooksPath(c, rigPath)
}

// Fix configures core.hooksPath for all unconfigured clones.
func (c *HooksPathConfiguredCheck) Fix(_ *CheckContext) error {
	for _, clonePath := range c.unconfiguredClones {
		cmd := exec.Command("git", "-C", clonePath, "config", "core.hooksPath", ".githooks")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to configure hooks for %s: %w", clonePath, err)
		}
	}
	return nil
}

// WitnessExistsCheck verifies the witness directory structure exists.
type WitnessExistsCheck struct {
	FixableCheck
	rigPath     string
	needsCreate bool
	needsClone  bool
	needsMail   bool
}

// NewWitnessExistsCheck creates a new witness exists check.
func NewWitnessExistsCheck() *WitnessExistsCheck {
	return &WitnessExistsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "witness-exists",
				CheckDescription: "Verify witness/ directory structure exists",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if the witness directory structure exists.
func (c *WitnessExistsCheck) Run(ctx *CheckContext) *CheckResult {
	c.rigPath = ctx.RigPath()
	if c.rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No rig specified",
		}
	}

	witnessDir := filepath.Join(c.rigPath, "witness")
	rigClone := filepath.Join(witnessDir, "rig")
	mailInbox := filepath.Join(witnessDir, "mail", "inbox.jsonl")

	var issues []string
	c.needsCreate = false
	c.needsClone = false
	c.needsMail = false

	// Check witness/ directory
	if _, err := os.Stat(witnessDir); os.IsNotExist(err) {
		issues = append(issues, "Missing: witness/")
		c.needsCreate = true
	} else {
		// Check witness/rig/ clone
		rigGit := filepath.Join(rigClone, ".git")
		if _, err := os.Stat(rigGit); os.IsNotExist(err) {
			issues = append(issues, "Missing: witness/rig/ (git clone)")
			c.needsClone = true
		}

		// Check witness/mail/inbox.jsonl
		if _, err := os.Stat(mailInbox); os.IsNotExist(err) {
			issues = append(issues, "Missing: witness/mail/inbox.jsonl")
			c.needsMail = true
		}
	}

	if len(issues) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Witness structure exists",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: "Witness structure incomplete",
		Details: issues,
		FixHint: "Run 'gt doctor --fix' to create missing structure",
	}
}

// Fix creates missing witness structure.
func (c *WitnessExistsCheck) Fix(_ *CheckContext) error {
	witnessDir := filepath.Join(c.rigPath, "witness")

	if c.needsCreate {
		if err := os.MkdirAll(witnessDir, 0755); err != nil {
			return fmt.Errorf("failed to create witness/: %w", err)
		}
	}

	if c.needsMail {
		mailDir := filepath.Join(witnessDir, "mail")
		if err := os.MkdirAll(mailDir, 0755); err != nil {
			return fmt.Errorf("failed to create witness/mail/: %w", err)
		}
		inboxPath := filepath.Join(mailDir, "inbox.jsonl")
		if err := os.WriteFile(inboxPath, []byte{}, 0644); err != nil {
			return fmt.Errorf("failed to create inbox.jsonl: %w", err)
		}
	}

	// Note: Cannot auto-fix clone without knowing the repo URL
	if c.needsClone {
		return fmt.Errorf("cannot auto-create witness/rig/ clone (requires repo URL)")
	}

	return nil
}

// RefineryExistsCheck verifies the refinery directory structure exists.
type RefineryExistsCheck struct {
	FixableCheck
	rigPath     string
	needsCreate bool
	needsClone  bool
	needsMail   bool
}

// NewRefineryExistsCheck creates a new refinery exists check.
func NewRefineryExistsCheck() *RefineryExistsCheck {
	return &RefineryExistsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "refinery-exists",
				CheckDescription: "Verify refinery/ directory structure exists",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if the refinery directory structure exists.
func (c *RefineryExistsCheck) Run(ctx *CheckContext) *CheckResult {
	c.rigPath = ctx.RigPath()
	if c.rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No rig specified",
		}
	}

	refineryDir := filepath.Join(c.rigPath, "refinery")
	rigClone := filepath.Join(refineryDir, "rig")
	mailInbox := filepath.Join(refineryDir, "mail", "inbox.jsonl")

	var issues []string
	c.needsCreate = false
	c.needsClone = false
	c.needsMail = false

	// Check refinery/ directory
	if _, err := os.Stat(refineryDir); os.IsNotExist(err) {
		issues = append(issues, "Missing: refinery/")
		c.needsCreate = true
	} else {
		// Check refinery/rig/ clone
		rigGit := filepath.Join(rigClone, ".git")
		if _, err := os.Stat(rigGit); os.IsNotExist(err) {
			issues = append(issues, "Missing: refinery/rig/ (git clone)")
			c.needsClone = true
		}

		// Check refinery/mail/inbox.jsonl
		if _, err := os.Stat(mailInbox); os.IsNotExist(err) {
			issues = append(issues, "Missing: refinery/mail/inbox.jsonl")
			c.needsMail = true
		}
	}

	if len(issues) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Refinery structure exists",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: "Refinery structure incomplete",
		Details: issues,
		FixHint: "Run 'gt doctor --fix' to create missing structure",
	}
}

// Fix creates missing refinery structure.
func (c *RefineryExistsCheck) Fix(_ *CheckContext) error {
	_ = c.needsCreate
	_ = c.needsMail
	_ = c.needsClone
	refineryDir := filepath.Join(c.rigPath, "refinery")
	if err := createRefineryDirIfNeeded(c, refineryDir); err != nil {
		return err
	}
	if err := createRefineryMailIfNeeded(c, refineryDir); err != nil {
		return err
	}
	return createRefineryWorktreeIfNeeded(c, refineryDir)
}

// MayorCloneExistsCheck verifies the mayor/rig clone exists.
type MayorCloneExistsCheck struct {
	FixableCheck
	rigPath     string
	needsCreate bool
	needsClone  bool
}

// NewMayorCloneExistsCheck creates a new mayor clone check.
func NewMayorCloneExistsCheck() *MayorCloneExistsCheck {
	return &MayorCloneExistsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "mayor-clone-exists",
				CheckDescription: "Verify mayor/rig/ git clone exists",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if the mayor/rig clone exists.
func (c *MayorCloneExistsCheck) Run(ctx *CheckContext) *CheckResult {
	c.rigPath = ctx.RigPath()
	if c.rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No rig specified",
		}
	}

	mayorDir := filepath.Join(c.rigPath, "mayor")
	rigClone := filepath.Join(mayorDir, "rig")

	var issues []string
	c.needsCreate = false
	c.needsClone = false

	// Check mayor/ directory
	if _, err := os.Stat(mayorDir); os.IsNotExist(err) {
		issues = append(issues, "Missing: mayor/")
		c.needsCreate = true
	} else {
		// Check mayor/rig/ clone
		rigGit := filepath.Join(rigClone, ".git")
		if _, err := os.Stat(rigGit); os.IsNotExist(err) {
			issues = append(issues, "Missing: mayor/rig/ (git clone)")
			c.needsClone = true
		}
	}

	if len(issues) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Mayor clone exists",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: "Mayor structure incomplete",
		Details: issues,
		FixHint: "Run 'gt doctor --fix' to create structure (clone requires repo URL)",
	}
}

// Fix creates missing mayor structure.
func (c *MayorCloneExistsCheck) Fix(_ *CheckContext) error {
	mayorDir := filepath.Join(c.rigPath, "mayor")

	if c.needsCreate {
		if err := os.MkdirAll(mayorDir, 0755); err != nil {
			return fmt.Errorf("failed to create mayor/: %w", err)
		}
	}

	// Note: Cannot auto-fix clone without knowing the repo URL
	if c.needsClone {
		return fmt.Errorf("cannot auto-create mayor/rig/ clone (requires repo URL)")
	}

	return nil
}

// PolecatClonesValidCheck verifies each polecat directory is a valid clone.
type PolecatClonesValidCheck struct {
	BaseCheck
}

// NewPolecatClonesValidCheck creates a new polecat clones check.
func NewPolecatClonesValidCheck() *PolecatClonesValidCheck {
	return &PolecatClonesValidCheck{
		BaseCheck: BaseCheck{
			CheckName:        "polecat-clones-valid",
			CheckDescription: "Verify polecat directories are valid git clones",
			CheckCategory:    CategoryRig,
		},
	}
}

// Run checks if each polecat directory is a valid git clone.
func (c *PolecatClonesValidCheck) Run(ctx *CheckContext) *CheckResult {
	entries, result := listPolecatCloneEntries(c, ctx)
	if result != nil {
		return result
	}
	scan := inspectPolecatClones(ctx, entries)
	return reportPolecatClones(c, scan)
}

// BeadsConfigValidCheck verifies beads configuration if .beads/ exists.
type BeadsConfigValidCheck struct {
	FixableCheck
	rigPath string
}

// NewBeadsConfigValidCheck creates a new beads config check.
func NewBeadsConfigValidCheck() *BeadsConfigValidCheck {
	return &BeadsConfigValidCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "beads-config-valid",
				CheckDescription: "Verify beads configuration if .beads/ exists",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if beads is properly configured.
func (c *BeadsConfigValidCheck) Run(ctx *CheckContext) *CheckResult {
	c.rigPath = ctx.RigPath()
	if c.rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No rig specified",
		}
	}

	beadsDir := filepath.Join(c.rigPath, ".beads")
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No .beads/ directory (beads not configured)",
		}
	}

	// Check if bd command works
	cmd := beads.Spawn("stats", "--json")
	cmd.Dir = c.rigPath
	if err := cmd.Run(); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "bd command failed",
			Details: []string{fmt.Sprintf("Error: %v", err)},
			FixHint: "Check beads installation and .beads/ configuration",
		}
	}

	// Note: With Dolt backend, there's no sync status to check.
	// Beads changes are persisted immediately.

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "Beads configured and accessible",
	}
}

// Fix is a no-op with Dolt backend (no sync needed).
func (c *BeadsConfigValidCheck) Fix(_ *CheckContext) error {
	// With Dolt backend, beads changes are persisted immediately - no sync needed
	return nil
}

// BeadsRedirectCheck verifies that rig-level beads redirect exists for tracked beads.
// When a repo has .beads/ tracked in git (at mayor/rig/.beads), the rig root needs
// a redirect file pointing to that location.
type BeadsRedirectCheck struct {
	FixableCheck
}

// NewBeadsRedirectCheck creates a new beads redirect check.
func NewBeadsRedirectCheck() *BeadsRedirectCheck {
	return &BeadsRedirectCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "beads-redirect",
				CheckDescription: "Verify rig-level beads redirect for tracked beads",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if the rig-level beads redirect exists when needed.
func (c *BeadsRedirectCheck) Run(ctx *CheckContext) *CheckResult {
	if ctx.RigName == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rig specified (skipping redirect check)",
		}
	}
	if result := checkBeadsRedirectWithoutTracked(c, ctx); result != nil {
		return result
	}
	return checkBeadsRedirectWithTracked(c, ctx)
}

// Fix creates or corrects the rig-level beads redirect, or initializes beads if missing.
func (c *BeadsRedirectCheck) Fix(ctx *CheckContext) error {
	if ctx.RigName == "" {
		return nil
	}
	state := beadsRedirectFixState(ctx)
	if !state.hasTracked && !state.hasLocal {
		return initMissingRigBeads(ctx)
	}
	if state.hasTracked {
		return writeBeadsRedirect(ctx, state.hasLocal)
	}
	return nil
}

// hasBeadsData checks if a beads directory has actual data (issues.db, config.yaml)
// as opposed to just being a redirect-only directory.
func hasBeadsData(beadsDir string) bool {
	// Check for actual beads data files (Dolt-only — issues.jsonl is no longer supported)
	dataFiles := []string{"issues.db", "config.yaml"}
	for _, f := range dataFiles {
		if _, err := os.Stat(filepath.Join(beadsDir, f)); err == nil {
			return true
		}
	}
	return false
}

// bareRepoHealth returns nil if .repo.git is structurally usable as a bare repo,
// or an error describing why it is not. Catches the recurring corruption mode
// where .repo.git is reduced to objects/ + worktrees/ (no HEAD, refs, config),
// and also rejects a non-bare repo masquerading as .repo.git (which would let
// refspec repair write into a working tree's git dir).
func bareRepoHealth(bareRepoPath string) error {
	if _, err := os.Stat(filepath.Join(bareRepoPath, "HEAD")); err != nil {
		return fmt.Errorf("HEAD missing: %w", err)
	}
	cmd := exec.Command("git", "-C", bareRepoPath, "rev-parse", "--git-dir")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("git rev-parse --git-dir failed: %s", msg)
	}
	stderr.Reset()
	bareCmd := exec.Command("git", "-C", bareRepoPath, "rev-parse", "--is-bare-repository")
	bareCmd.Stderr = &stderr
	out, err := bareCmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("git rev-parse --is-bare-repository failed: %s", msg)
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf(".repo.git is not a bare repository")
	}
	return nil
}

// BareRepoRefspecCheck verifies that the shared bare repo has the correct refspec configured.
// Without this, worktrees created from the bare repo cannot fetch and see origin/* refs.
// See: https://github.com/anthropics/gastown/issues/286
type BareRepoRefspecCheck struct {
	FixableCheck
}

// NewBareRepoRefspecCheck creates a new bare repo refspec check.
func NewBareRepoRefspecCheck() *BareRepoRefspecCheck {
	return &BareRepoRefspecCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "bare-repo-refspec",
				CheckDescription: "Verify bare repo has correct refspec for worktrees",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if the bare repo has the correct remote.origin.fetch refspec.
func (c *BareRepoRefspecCheck) Run(ctx *CheckContext) *CheckResult {
	if ctx.RigName == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rig specified, skipping bare repo check",
		}
	}

	bareRepoPath := filepath.Join(ctx.RigPath(), ".repo.git")
	if _, err := os.Stat(bareRepoPath); os.IsNotExist(err) {
		// No bare repo - might be using a different architecture
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No shared bare repo found (using individual clones)",
		}
	}

	// Before checking refspec, verify the bare repo is fundamentally usable.
	// Without this guard, a partial-shell .repo.git (objects/ + worktrees/ only)
	// will pass after Fix auto-creates a config file, masking the real corruption.
	if healthErr := bareRepoHealth(bareRepoPath); healthErr != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Bare repo is structurally broken — refspec check cannot verify",
			Details: []string{
				healthErr.Error(),
				"Configuring a refspec on a corrupt repo would silently mask the corruption.",
			},
			FixHint: "Run 'gt doctor --fix --rig " + ctx.RigName + "' (bare-repo-exists check will re-clone)",
		}
	}

	// Check the refspec
	cmd := exec.Command("git", "-C", bareRepoPath, "config", "--get", "remote.origin.fetch")
	out, err := cmd.Output()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Bare repo missing remote.origin.fetch refspec",
			Details: []string{
				"Worktrees cannot fetch or see origin/* refs without this config",
				"This breaks refinery merge operations and causes stale origin/main",
			},
			FixHint: "Run 'gt doctor --fix' to configure the refspec",
		}
	}

	refspec := strings.TrimSpace(string(out))
	expectedRefspec := "+refs/heads/*:refs/remotes/origin/*"
	if refspec != expectedRefspec {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Bare repo has non-standard refspec",
			Details: []string{
				fmt.Sprintf("Current: %s", refspec),
				fmt.Sprintf("Expected: %s", expectedRefspec),
			},
			FixHint: "Run 'gt doctor --fix' to update the refspec",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "Bare repo refspec configured correctly",
	}
}

// Fix sets the correct refspec on the bare repo.
func (c *BareRepoRefspecCheck) Fix(ctx *CheckContext) error {
	if ctx.RigName == "" {
		return nil
	}

	bareRepoPath := filepath.Join(ctx.RigPath(), ".repo.git")
	if _, err := os.Stat(bareRepoPath); os.IsNotExist(err) {
		return nil // No bare repo to fix
	}

	// Refuse to write config into a structurally broken bare repo. `git config`
	// auto-creates .repo.git/config, which would make subsequent BareRepoRefspecCheck
	// runs return OK and hide the real corruption from operators.
	if healthErr := bareRepoHealth(bareRepoPath); healthErr != nil {
		return fmt.Errorf("refusing to set refspec: bare repo is structurally broken (%s); run bare-repo-exists fix to re-clone", healthErr)
	}

	cmd := exec.Command("git", "-C", bareRepoPath, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setting refspec: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// DefaultBranchExistsCheck verifies that the configured default_branch exists
// as a remote tracking ref in the bare repo.
type DefaultBranchExistsCheck struct {
	FixableCheck
}

// NewDefaultBranchExistsCheck creates a new default branch exists check.
func NewDefaultBranchExistsCheck() *DefaultBranchExistsCheck {
	return &DefaultBranchExistsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "default-branch-exists",
				CheckDescription: "Verify configured default_branch exists on remote",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks if the configured default_branch exists as origin/<branch> in the bare repo.
func (c *DefaultBranchExistsCheck) Run(ctx *CheckContext) *CheckResult {
	rigPath := ctx.RigPath()
	if rigPath == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No rig specified",
		}
	}

	// Load rig config to get default_branch
	configPath := filepath.Join(rigPath, "config.json")
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "No config.json found",
		}
	}
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("Cannot read config.json: %v", err),
		}
	}

	// Parse just the default_branch field
	type rigConfig struct {
		DefaultBranch string `json:"default_branch"`
	}
	var cfg rigConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("Cannot parse config.json: %v", err),
		}
	}

	if cfg.DefaultBranch == "" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No default_branch configured (will use 'main')",
		}
	}

	// Check bare repo for the ref
	bareRepoPath := filepath.Join(rigPath, ".repo.git")
	if _, err := os.Stat(bareRepoPath); os.IsNotExist(err) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No shared bare repo (skipping ref check)",
		}
	}

	ref := originTrackingRef(cfg.DefaultBranch)
	if !gitRefExists(bareRepoPath, ref) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("default_branch %q not found on remote", cfg.DefaultBranch),
			Details: []string{
				fmt.Sprintf("Ref %s does not exist in bare repo", ref),
				"Polecat spawn will fail with a cryptic git error",
			},
			FixHint: fmt.Sprintf("Run 'gt doctor --fix --rig %s' to fetch origin tracking refs, or fix the branch name in %s/config.json", ctx.RigName, rigPath),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("default_branch %q exists on remote", cfg.DefaultBranch),
	}
}

// Fix fetches origin tracking refs in the rig's bare repo so origin/<default_branch>
// exists after a local clone.
func (c *DefaultBranchExistsCheck) Fix(ctx *CheckContext) error {
	if ctx.RigName == "" {
		return nil
	}
	bareRepoPath := filepath.Join(ctx.RigPath(), ".repo.git")
	if _, err := os.Stat(bareRepoPath); os.IsNotExist(err) {
		return nil
	}
	return fetchOriginTrackingRefs(bareRepoPath)
}

func originTrackingRef(branch string) string {
	return fmt.Sprintf("refs/remotes/origin/%s", branch)
}

func gitRefExists(repoPath, ref string) bool {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", "--quiet", ref)
	return cmd.Run() == nil
}

func fetchOriginTrackingRefs(repoPath string) error {
	cmd := exec.Command("git", "-c", "protocol.file.allow=always", "-C", repoPath, "fetch", "origin")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fetching origin: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// DefaultBranchAllRigsCheck validates default_branch for all rigs in the workspace.
// Unlike DefaultBranchExistsCheck (which requires --rig), this runs globally.
type DefaultBranchAllRigsCheck struct {
	FixableCheck
}

// NewDefaultBranchAllRigsCheck creates a new global default branch check.
func NewDefaultBranchAllRigsCheck() *DefaultBranchAllRigsCheck {
	return &DefaultBranchAllRigsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "default-branch-all-rigs",
				CheckDescription: "Verify default_branch exists on remote for all rigs",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks default_branch for every discovered rig.
func (c *DefaultBranchAllRigsCheck) Run(ctx *CheckContext) *CheckResult {
	entries, err := os.ReadDir(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("Cannot read town root: %v", err),
		}
	}
	scan := scanDefaultBranchAllRigs(ctx.TownRoot, entries)
	return reportDefaultBranchAllRigs(c, scan)
}

// Fix fetches origin tracking refs for every rig whose default_branch is missing
// as refs/remotes/origin/<branch>. Local clones may have the branch as
// refs/heads/<branch> but never materialized the tracking ref.
func (c *DefaultBranchAllRigsCheck) Fix(ctx *CheckContext) error {
	entries, err := os.ReadDir(ctx.TownRoot)
	if err != nil {
		return fmt.Errorf("reading town root: %w", err)
	}
	failed := fixDefaultBranchAllRigs(ctx.TownRoot, entries)
	if len(failed) > 0 {
		return fmt.Errorf("could not restore origin tracking refs: %s", strings.Join(failed, "; "))
	}
	return nil
}

// RigChecks returns all rig-level health checks.
func RigChecks() []Check {
	return []Check{
		NewRigIsGitRepoCheck(),
		NewGitExcludeConfiguredCheck(),
		NewHooksPathConfiguredCheck(),
		NewBareRepoExistsCheck(),
		NewBareRepoRefspecCheck(),
		NewDefaultBranchExistsCheck(),
		NewWitnessExistsCheck(),
		NewRefineryExistsCheck(),
		NewMayorCloneExistsCheck(),
		NewPolecatClonesValidCheck(),
		NewBeadsConfigValidCheck(),
		NewBeadsRedirectCheck(),
		NewTestutilSymlinkCheck(),
	}
}
