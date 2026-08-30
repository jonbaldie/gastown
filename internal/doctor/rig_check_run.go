package doctor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/git"
)

func requiredGitExcludes() []string {
	return []string{"/polecats/", "/witness/", "/refinery/", "/mayor/"}
}

func gitExcludeMayorGitDir(c *GitExcludeConfiguredCheck, ctx *CheckContext) (string, *CheckResult) {
	rigPath := ctx.RigPath()
	if rigPath == "" {
		return "", &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No rig specified",
		}
	}
	gitDir := filepath.Join(rigPath, "mayor", "rig", ".git")
	info, err := os.Stat(gitDir)
	if os.IsNotExist(err) {
		return "", &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "No mayor/rig clone found",
			FixHint: "Run rig-is-git-repo check first",
		}
	}
	return resolveGitExcludeGitDir(c, rigPath, gitDir, info)
}

func resolveGitExcludeGitDir(c *GitExcludeConfiguredCheck, rigPath, gitDir string, info os.FileInfo) (string, *CheckResult) {
	if !info.Mode().IsRegular() {
		return gitDir, nil
	}
	content, err := os.ReadFile(gitDir)
	if err != nil {
		return "", &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("Cannot read .git file: %v", err),
		}
	}
	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir: ") {
		return gitDir, nil
	}
	gitDir = strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(rigPath, gitDir)
	}
	return gitDir, nil
}

func loadGitExcludeMissing(c *GitExcludeConfiguredCheck, gitDir string) {
	c.excludePath = filepath.Join(gitDir, "info", "exclude")
	existing := readGitExcludeEntries(c.excludePath)
	c.missingEntries = nil
	for _, required := range requiredGitExcludes() {
		unanchored := strings.TrimPrefix(required, "/")
		if !existing[required] && !existing[unanchored] {
			c.missingEntries = append(c.missingEntries, required)
		}
	}
}

func readGitExcludeEntries(path string) map[string]bool {
	existing := make(map[string]bool)
	file, err := os.Open(path)
	if err != nil {
		return existing
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			existing[line] = true
		}
	}
	_ = file.Close()
	return existing
}

func reportGitExclude(c *GitExcludeConfiguredCheck) *CheckResult {
	if len(c.missingEntries) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Git exclude properly configured",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d Gas Town directories not excluded", len(c.missingEntries)),
		Details: []string{fmt.Sprintf("Missing: %s", strings.Join(c.missingEntries, ", "))},
		FixHint: "Run 'gt doctor --fix' to add missing entries",
	}
}

func collectUnconfiguredHookClones(c *HooksPathConfiguredCheck, rigPath string) {
	for _, clonePath := range hookClonePaths(rigPath) {
		if cloneNeedsHooksPath(clonePath) {
			c.unconfiguredClones = append(c.unconfiguredClones, clonePath)
		}
	}
}

func hookClonePaths(rigPath string) []string {
	clonePaths := []string{
		filepath.Join(rigPath, "mayor", "rig"),
		filepath.Join(rigPath, "refinery", "rig"),
	}
	appendHookCloneDirs(&clonePaths, filepath.Join(rigPath, "crew"))
	appendHookCloneDirs(&clonePaths, filepath.Join(rigPath, "polecats"))
	return clonePaths
}

func appendHookCloneDirs(clonePaths *[]string, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			*clonePaths = append(*clonePaths, filepath.Join(dir, entry.Name()))
		}
	}
}

func cloneNeedsHooksPath(clonePath string) bool {
	if _, err := os.Stat(filepath.Join(clonePath, ".git")); os.IsNotExist(err) {
		return false
	}
	if _, err := os.Stat(filepath.Join(clonePath, ".githooks")); os.IsNotExist(err) {
		return false
	}
	cmd := exec.Command("git", "-C", clonePath, "config", "--get", "core.hooksPath")
	output, err := cmd.Output()
	return err != nil || strings.TrimSpace(string(output)) != ".githooks"
}

func reportHooksPath(c *HooksPathConfiguredCheck, rigPath string) *CheckResult {
	if len(c.unconfiguredClones) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "All clones have hooks configured",
		}
	}
	var details []string
	for _, clonePath := range c.unconfiguredClones {
		relPath, _ := filepath.Rel(rigPath, clonePath)
		if relPath == "" {
			relPath = clonePath
		}
		details = append(details, relPath)
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d clone(s) missing hooks configuration", len(c.unconfiguredClones)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to configure hooks",
	}
}

func createRefineryDirIfNeeded(c *RefineryExistsCheck, refineryDir string) error {
	if !c.needsCreate {
		return nil
	}
	if err := os.MkdirAll(refineryDir, 0755); err != nil {
		return fmt.Errorf("failed to create refinery/: %w", err)
	}
	return nil
}

func createRefineryMailIfNeeded(c *RefineryExistsCheck, refineryDir string) error {
	if !c.needsMail {
		return nil
	}
	mailDir := filepath.Join(refineryDir, "mail")
	if err := os.MkdirAll(mailDir, 0755); err != nil {
		return fmt.Errorf("failed to create refinery/mail/: %w", err)
	}
	if err := os.WriteFile(filepath.Join(mailDir, "inbox.jsonl"), []byte{}, 0644); err != nil {
		return fmt.Errorf("failed to create inbox.jsonl: %w", err)
	}
	return nil
}

func createRefineryWorktreeIfNeeded(c *RefineryExistsCheck, refineryDir string) error {
	if !c.needsClone {
		return nil
	}
	bareRepoPath := filepath.Join(c.rigPath, ".repo.git")
	if _, err := os.Stat(bareRepoPath); os.IsNotExist(err) {
		return fmt.Errorf("cannot auto-create refinery/rig/ worktree: bare repo not found at %s", bareRepoPath)
	}
	bareGit := git.NewGitWithDir(bareRepoPath, "")
	_ = git.WorktreePrune(bareGit)
	rigClone := filepath.Join(refineryDir, "rig")
	if err := git.WorktreeAddExisting(bareGit, rigClone, refineryDefaultBranch(c.rigPath)); err != nil {
		return fmt.Errorf("creating refinery worktree from bare repo: %w", err)
	}
	_ = git.ConfigureHooksPath(git.NewGit(rigClone))
	return nil
}

func refineryDefaultBranch(rigPath string) string {
	defaultBranch := "main"
	data, err := os.ReadFile(filepath.Join(rigPath, "settings", "rig.json"))
	if err != nil {
		return defaultBranch
	}
	var cfg struct {
		DefaultBranch string `json:"default_branch"`
	}
	if json.Unmarshal(data, &cfg) == nil && cfg.DefaultBranch != "" {
		return cfg.DefaultBranch
	}
	return defaultBranch
}

func listPolecatCloneEntries(c *PolecatClonesValidCheck, ctx *CheckContext) ([]os.DirEntry, *CheckResult) {
	rigPath := ctx.RigPath()
	if rigPath == "" {
		return nil, &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No rig specified",
		}
	}
	entries, err := os.ReadDir(filepath.Join(rigPath, "polecats"))
	if os.IsNotExist(err) {
		return nil, &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No polecats/ directory (none deployed)",
		}
	}
	if err != nil {
		return nil, &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("Cannot read polecats/: %v", err),
		}
	}
	return entries, nil
}

type polecatCloneScan struct {
	issues     []string
	warnings   []string
	validCount int
}

func inspectPolecatClones(ctx *CheckContext, entries []os.DirEntry) polecatCloneScan {
	var scan polecatCloneScan
	polecatsDir := filepath.Join(ctx.RigPath(), "polecats")
	for _, entry := range entries {
		inspectOnePolecatClone(&scan, polecatsDir, ctx.RigName, entry)
	}
	return scan
}

func inspectOnePolecatClone(scan *polecatCloneScan, polecatsDir, rigName string, entry os.DirEntry) {
	if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
		return
	}
	polecatName := entry.Name()
	polecatPath := filepath.Join(polecatsDir, polecatName, rigName)
	if _, err := os.Stat(polecatPath); os.IsNotExist(err) {
		polecatPath = filepath.Join(polecatsDir, polecatName)
	}
	if _, err := os.Stat(filepath.Join(polecatPath, ".git")); os.IsNotExist(err) {
		scan.issues = append(scan.issues, fmt.Sprintf("%s: not a git clone", polecatName))
		return
	}
	cmd := exec.Command("git", "-C", polecatPath, "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		scan.issues = append(scan.issues, fmt.Sprintf("%s: git status failed", polecatName))
		return
	}
	if len(output) > 0 {
		scan.warnings = append(scan.warnings, fmt.Sprintf("%s: has uncommitted changes", polecatName))
	}
	appendPolecatBranchWarning(scan, polecatPath, polecatName)
	scan.validCount++
}

func appendPolecatBranchWarning(scan *polecatCloneScan, polecatPath, polecatName string) {
	cmd := exec.Command("git", "-C", polecatPath, "branch", "--show-current")
	branchOutput, err := cmd.Output()
	if err != nil {
		return
	}
	branch := strings.TrimSpace(string(branchOutput))
	if !strings.HasPrefix(branch, constants.BranchPolecatPrefix) {
		scan.warnings = append(scan.warnings, fmt.Sprintf("%s: on branch '%s' (expected %s*)", polecatName, branch, constants.BranchPolecatPrefix))
	}
}

func reportPolecatClones(c *PolecatClonesValidCheck, scan polecatCloneScan) *CheckResult {
	if len(scan.issues) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d polecat(s) invalid", len(scan.issues)),
			Details: append(scan.issues, scan.warnings...),
			FixHint: "Cannot auto-fix (data loss risk)",
		}
	}
	if len(scan.warnings) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d polecat(s) valid, %d warning(s)", scan.validCount, len(scan.warnings)),
			Details: scan.warnings,
		}
	}
	if scan.validCount == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No polecats deployed",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("%d polecat(s) valid", scan.validCount),
	}
}

func checkBeadsRedirectWithoutTracked(c *BeadsRedirectCheck, ctx *CheckContext) *CheckResult {
	rigPath := ctx.RigPath()
	mayorRigBeads := filepath.Join(rigPath, "mayor", "rig", ".beads")
	if _, err := os.Stat(mayorRigBeads); !os.IsNotExist(err) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".beads")); os.IsNotExist(err) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "No .beads directory found at rig root",
			Details: []string{
				"Beads database not initialized for this rig",
				"This prevents issue tracking for this rig",
			},
			FixHint: "Run 'gt doctor --fix --rig " + ctx.RigName + "' to initialize beads",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "Rig uses local beads (no redirect needed)",
	}
}

func checkBeadsRedirectWithTracked(c *BeadsRedirectCheck, ctx *CheckContext) *CheckResult {
	rigBeadsDir := filepath.Join(ctx.RigPath(), ".beads")
	redirectPath := filepath.Join(rigBeadsDir, "redirect")
	hasLocalData := hasBeadsData(rigBeadsDir)
	_, err := os.Stat(redirectPath)
	redirectExists := err == nil
	if hasLocalData && !redirectExists {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Conflicting local beads found with tracked beads",
			Details: []string{
				"Tracked beads exist at: mayor/rig/.beads",
				"Local beads with data exist at: .beads/",
				"Fix will remove local beads and create redirect to tracked beads",
			},
			FixHint: "Run 'gt doctor --fix --rig " + ctx.RigName + "' to fix",
		}
	}
	if !redirectExists {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Missing rig-level beads redirect for tracked beads",
			Details: []string{
				"Tracked beads exist at: mayor/rig/.beads",
				"Missing redirect at: .beads/redirect",
				"Without this redirect, bd commands from rig root won't find beads",
			},
			FixHint: "Run 'gt doctor --fix' to create the redirect",
		}
	}
	return verifyBeadsRedirectTarget(c, ctx, redirectPath)
}

func verifyBeadsRedirectTarget(c *BeadsRedirectCheck, ctx *CheckContext, redirectPath string) *CheckResult {
	content, err := os.ReadFile(redirectPath)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("Could not read redirect file: %v", err),
		}
	}
	target := strings.TrimSpace(string(content))
	if target != "mayor/rig/.beads" {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("Redirect points to %q, expected mayor/rig/.beads", target),
			FixHint: "Run 'gt doctor --fix --rig " + ctx.RigName + "' to correct the redirect",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "Rig-level beads redirect is correctly configured",
	}
}

type beadsRedirectState struct {
	hasTracked bool
	hasLocal   bool
}

func beadsRedirectFixState(ctx *CheckContext) beadsRedirectState {
	rigPath := ctx.RigPath()
	_, trackedErr := os.Stat(filepath.Join(rigPath, "mayor", "rig", ".beads"))
	_, localErr := os.Stat(filepath.Join(rigPath, ".beads"))
	return beadsRedirectState{
		hasTracked: !os.IsNotExist(trackedErr),
		hasLocal:   !os.IsNotExist(localErr),
	}
}

func initMissingRigBeads(ctx *CheckContext) error {
	rigBeadsDir := filepath.Join(ctx.RigPath(), ".beads")
	prefix := config.GetRigPrefix(ctx.TownRoot, ctx.RigName)
	if err := beads.EnsureDir(rigBeadsDir); err != nil {
		return fmt.Errorf("creating .beads directory: %w", err)
	}
	bdEnv := append(stripEnvPrefixes(os.Environ(), "BEADS_DIR=", "BEADS_DB=", "BEADS_DOLT_SERVER_DATABASE="),
		"BEADS_DIR="+rigBeadsDir,
		"BEADS_DOLT_SERVER_DATABASE="+ctx.RigName,
	)
	if err := runBeadsRedirectInit(ctx, rigBeadsDir, prefix, bdEnv); err != nil {
		return err
	}
	if err := doltserver.EnsureMetadataForBeadsDir(ctx.TownRoot, rigBeadsDir, ctx.RigName, ctx.RigName); err != nil {
		return fmt.Errorf("ensuring metadata.json: %w", err)
	}
	return nil
}

func runBeadsRedirectInit(ctx *CheckContext, rigBeadsDir, prefix string, bdEnv []string) error {
	initArgs := []string{"init"}
	if prefix != "" {
		initArgs = append(initArgs, "--prefix", prefix)
	}
	initArgs = append(initArgs, "--database", ctx.RigName, "--server", "--server-port", strconv.Itoa(doltserver.DefaultConfig(ctx.TownRoot).Port))
	cmd := beads.Spawn(initArgs...)
	cmd.Dir = ctx.RigPath()
	cmd.Env = bdEnv
	_, err := cmd.CombinedOutput()
	if chmodErr := beads.EnsureDir(rigBeadsDir); chmodErr != nil {
		return chmodErr
	}
	if err != nil {
		if writeErr := beads.EnsureConfigYAML(rigBeadsDir, prefix); writeErr != nil {
			return fmt.Errorf("bd init failed (%v) and fallback config creation failed: %w", err, writeErr)
		}
		return nil
	}
	configureBeadsRedirectTypes(ctx, bdEnv)
	return nil
}

func configureBeadsRedirectTypes(ctx *CheckContext, bdEnv []string) {
	for _, cfg := range []struct{ key, value string }{
		{"types.custom", constants.BeadsCustomTypes},
		{"types.infra", constants.BeadsInfraTypes},
	} {
		configCmd := beads.Spawn("config", "set", cfg.key, cfg.value)
		configCmd.Dir = ctx.RigPath()
		configCmd.Env = bdEnv
		_, _ = configCmd.CombinedOutput()
	}
}

func writeBeadsRedirect(ctx *CheckContext, hasLocal bool) error {
	rigBeadsDir := filepath.Join(ctx.RigPath(), ".beads")
	if hasLocal && hasBeadsData(rigBeadsDir) {
		if err := os.RemoveAll(rigBeadsDir); err != nil {
			return fmt.Errorf("removing conflicting local beads: %w", err)
		}
	}
	if err := beads.EnsureDir(rigBeadsDir); err != nil {
		return fmt.Errorf("creating .beads directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		return fmt.Errorf("writing redirect file: %w", err)
	}
	return nil
}

type defaultBranchScan struct {
	errors      []string
	rigsChecked int
}

func scanDefaultBranchAllRigs(townRoot string, entries []os.DirEntry) defaultBranchScan {
	var scan defaultBranchScan
	for _, entry := range entries {
		if isSkippedTownRigEntry(entry) {
			continue
		}
		scanOneDefaultBranchRig(&scan, townRoot, entry.Name())
	}
	return scan
}

func isSkippedTownRigEntry(entry os.DirEntry) bool {
	return !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == "mayor" || entry.Name() == "docs" || entry.Name() == "scripts"
}

func scanOneDefaultBranchRig(scan *defaultBranchScan, townRoot, rigName string) {
	rigPath := filepath.Join(townRoot, rigName)
	data, err := os.ReadFile(filepath.Join(rigPath, "config.json"))
	if err != nil {
		return
	}
	var cfg struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.DefaultBranch == "" {
		return
	}
	scan.rigsChecked++
	bareRepoPath := filepath.Join(rigPath, ".repo.git")
	if _, err := os.Stat(bareRepoPath); os.IsNotExist(err) {
		return
	}
	if !gitRefExists(bareRepoPath, originTrackingRef(cfg.DefaultBranch)) {
		scan.errors = append(scan.errors, fmt.Sprintf("%s: default_branch %q not found on remote", rigName, cfg.DefaultBranch))
	}
}

func reportDefaultBranchAllRigs(c *DefaultBranchAllRigsCheck, scan defaultBranchScan) *CheckResult {
	if len(scan.errors) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d rig(s) with invalid default_branch", len(scan.errors)),
			Details: scan.errors,
			FixHint: "Run 'gt doctor --fix' to fetch origin tracking refs, or fix the branch name in <rig>/config.json",
		}
	}
	if scan.rigsChecked == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs with custom default_branch configured",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("All %d rig(s) with custom default_branch validated", scan.rigsChecked),
	}
}

func fixDefaultBranchAllRigs(townRoot string, entries []os.DirEntry) []string {
	var failed []string
	for _, entry := range entries {
		if isSkippedTownRigEntry(entry) {
			continue
		}
		if msg := fixOneDefaultBranchRig(townRoot, entry.Name()); msg != "" {
			failed = append(failed, msg)
		}
	}
	return failed
}

func fixOneDefaultBranchRig(townRoot, rigName string) string {
	rigPath := filepath.Join(townRoot, rigName)
	data, err := os.ReadFile(filepath.Join(rigPath, "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.DefaultBranch == "" {
		return ""
	}
	bareRepoPath := filepath.Join(rigPath, ".repo.git")
	if _, err := os.Stat(bareRepoPath); os.IsNotExist(err) {
		return ""
	}
	if gitRefExists(bareRepoPath, originTrackingRef(cfg.DefaultBranch)) {
		return ""
	}
	if err := fetchOriginTrackingRefs(bareRepoPath); err != nil {
		return fmt.Sprintf("%s: %v", rigName, err)
	}
	if !gitRefExists(bareRepoPath, originTrackingRef(cfg.DefaultBranch)) {
		return fmt.Sprintf("%s: default_branch %q still missing after fetch", rigName, cfg.DefaultBranch)
	}
	return ""
}
