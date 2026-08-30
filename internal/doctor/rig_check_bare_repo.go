package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/git"
)

type BareRepoExistsCheck struct {
	FixableCheck
	brokenWorktrees []string          // worktree paths with broken .repo.git references
	pushURLMismatch bool              // config.json push_url differs from .repo.git push URL
	bareRepoCorrupt bool              // .repo.git exists but is not a usable git directory
	recoveredHeads  map[string]string // rig-relative worktree path -> HEAD ref captured before re-clone
}

type bareRepoPushInspect struct {
	warning string
	early   *CheckResult
}

type bareRepoWorktreeRef struct {
	relPath string // rig-relative worktree directory path
	headRef string // contents of the worktree's HEAD (e.g. "ref: refs/heads/foo\n"), empty if unreadable
}

func NewBareRepoExistsCheck() *BareRepoExistsCheck {
	return &BareRepoExistsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "bare-repo-exists",
				CheckDescription: "Verify .repo.git exists when worktrees depend on it",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

func (c *BareRepoExistsCheck) Run(ctx *CheckContext) *CheckResult {
	if ctx.RigName == "" {
		return skipBareRepoCheck(c)
	}
	resetBareRepoCheck(c)
	collectBrokenBareRepoWorktrees(c, ctx.RigPath(), ctx.RigName)
	if result := reportCorruptBareRepo(c, ctx); result != nil {
		return result
	}
	if _, err := os.Stat(filepath.Join(ctx.RigPath(), ".repo.git")); err == nil {
		return evaluateExistingBareRepo(c, ctx)
	}
	return evaluateMissingBareRepo(c, ctx)
}

func skipBareRepoCheck(c *BareRepoExistsCheck) *CheckResult {
	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusOK,
		Message:  "No rig specified (skipping bare repo check)",
		Category: c.Category(),
	}
}

func resetBareRepoCheck(c *BareRepoExistsCheck) {
	c.brokenWorktrees = nil
	c.pushURLMismatch = false
	c.bareRepoCorrupt = false
	c.recoveredHeads = nil
}

func collectBrokenBareRepoWorktrees(c *BareRepoExistsCheck, rigPath, rigName string) {
	for _, wtDir := range findBareRepoWorktreeDirs(rigPath, rigName) {
		if rel := brokenBareRepoWorktreeRel(rigPath, wtDir); rel != "" {
			c.brokenWorktrees = append(c.brokenWorktrees, rel)
		}
	}
}

func brokenBareRepoWorktreeRel(rigPath, wtDir string) string {
	gitFile := filepath.Join(wtDir, ".git")
	info, err := os.Stat(gitFile)
	if err != nil || info.IsDir() {
		return ""
	}
	content, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir: ") {
		return ""
	}
	gitdir := strings.TrimPrefix(line, "gitdir: ")
	if !strings.Contains(gitdir, ".repo.git") {
		return ""
	}
	targetPath := gitdir
	if !filepath.IsAbs(targetPath) {
		targetPath = filepath.Join(wtDir, targetPath)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		return ""
	}
	relPath, _ := filepath.Rel(rigPath, wtDir)
	if relPath == "" {
		return wtDir
	}
	return relPath
}

func reportCorruptBareRepo(c *BareRepoExistsCheck, ctx *CheckContext) *CheckResult {
	bareRepoPath := filepath.Join(ctx.RigPath(), ".repo.git")
	if _, err := os.Stat(bareRepoPath); err != nil {
		return nil
	}
	healthErr := bareRepoHealth(bareRepoPath)
	if healthErr == nil {
		return nil
	}
	c.bareRepoCorrupt = true
	details := []string{
		fmt.Sprintf("Bare repo at %s is structurally broken", bareRepoPath),
		healthErr.Error(),
		"Worktrees and `gt sling --create` cannot operate on this repo until it is re-cloned.",
	}
	if len(c.brokenWorktrees) > 0 {
		details = append(details, fmt.Sprintf("Also: %d worktree(s) have broken references", len(c.brokenWorktrees)))
		details = append(details, c.brokenWorktrees...)
	}
	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusError,
		Message:  "Shared bare repo exists but is unusable (corrupt)",
		Details:  details,
		FixHint:  "Run 'gt doctor --fix --rig " + ctx.RigName + "' to re-clone .repo.git",
		Category: c.Category(),
	}
}

func evaluateExistingBareRepo(c *BareRepoExistsCheck, ctx *CheckContext) *CheckResult {
	ins := inspectBareRepoPushURL(c, ctx)
	if ins.early != nil {
		return ins.early
	}
	return reportExistingBareRepo(c, ctx, ins.warning)
}

func inspectBareRepoPushURL(c *BareRepoExistsCheck, ctx *CheckContext) bareRepoPushInspect {
	c.pushURLMismatch = false
	configPath := filepath.Join(ctx.RigPath(), "config.json")
	cfgData, readErr := os.ReadFile(configPath)
	if readErr != nil {
		return inspectBareRepoConfigRead(c, configPath, readErr)
	}
	return inspectBareRepoConfigData(c, ctx, configPath, cfgData)
}

func inspectBareRepoConfigRead(c *BareRepoExistsCheck, configPath string, readErr error) bareRepoPushInspect {
	if os.IsNotExist(readErr) {
		return bareRepoPushInspect{}
	}
	if len(c.brokenWorktrees) == 0 {
		return bareRepoPushInspect{early: &CheckResult{
			Name:     c.Name(),
			Status:   StatusWarning,
			Message:  "Shared bare repo exists but config.json is unreadable",
			Details:  []string{readErr.Error()},
			FixHint:  "Check file permissions on " + configPath,
			Category: c.Category(),
		}}
	}
	return bareRepoPushInspect{warning: "config.json unreadable: " + readErr.Error()}
}

func inspectBareRepoConfigData(c *BareRepoExistsCheck, ctx *CheckContext, configPath string, cfgData []byte) bareRepoPushInspect {
	var cfg struct {
		PushURL string `json:"push_url,omitempty"`
	}
	if jsonErr := json.Unmarshal(cfgData, &cfg); jsonErr != nil {
		if len(c.brokenWorktrees) == 0 {
			return bareRepoPushInspect{early: &CheckResult{
				Name:     c.Name(),
				Status:   StatusWarning,
				Message:  "Shared bare repo exists but config.json is malformed",
				Details:  []string{jsonErr.Error()},
				FixHint:  "Check config.json syntax in " + configPath,
				Category: c.Category(),
			}}
		}
		return bareRepoPushInspect{warning: "config.json malformed: " + jsonErr.Error()}
	}
	return inspectBareRepoRemoteURLs(c, ctx, strings.TrimSpace(cfg.PushURL))
}

func inspectBareRepoRemoteURLs(c *BareRepoExistsCheck, ctx *CheckContext, cfgPushURL string) bareRepoPushInspect {
	bareGit := git.NewGitWithDir(filepath.Join(ctx.RigPath(), ".repo.git"), "")
	actualPush, pushErr := bareGit.GetPushURL("origin")
	_, fetchErr := bareGit.RemoteURL("origin")
	if pushErr != nil || fetchErr != nil {
		return inspectBareRepoRemoteQueryFail(c, ctx, pushErr, fetchErr)
	}
	if cfgPushURL != "" && actualPush != cfgPushURL {
		c.pushURLMismatch = true
	}
	return bareRepoPushInspect{}
}

func inspectBareRepoRemoteQueryFail(c *BareRepoExistsCheck, ctx *CheckContext, pushErr, fetchErr error) bareRepoPushInspect {
	if len(c.brokenWorktrees) == 0 {
		details := []string{}
		if pushErr != nil {
			details = append(details, "push URL query failed: "+pushErr.Error())
		}
		if fetchErr != nil {
			details = append(details, "fetch URL query failed: "+fetchErr.Error())
		}
		return bareRepoPushInspect{early: &CheckResult{
			Name:     c.Name(),
			Status:   StatusWarning,
			Message:  "Cannot validate push URL — git remote query failed",
			Details:  details,
			FixHint:  "Check .repo.git remote configuration for " + ctx.RigName,
			Category: c.Category(),
		}}
	}
	return bareRepoPushInspect{warning: fmt.Sprintf("git remote query failed (push: %v, fetch: %v)", pushErr, fetchErr)}
}

func reportExistingBareRepo(c *BareRepoExistsCheck, ctx *CheckContext, configWarning string) *CheckResult {
	if len(c.brokenWorktrees) == 0 && !c.pushURLMismatch {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  "Shared bare repo exists and worktrees are valid",
			Category: c.Category(),
		}
	}
	if c.pushURLMismatch && len(c.brokenWorktrees) == 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusWarning,
			Message:  "Shared bare repo push URL does not match config.json",
			Details:  []string{"Note: manual config.json edits require 'gt rig add <name> --adopt' to propagate to town.json"},
			FixHint:  "Run 'gt doctor --fix --rig " + ctx.RigName + "' to update push URL",
			Category: c.Category(),
		}
	}
	if c.pushURLMismatch {
		details := []string{fmt.Sprintf("Push URL mismatch and %d broken worktree(s)", len(c.brokenWorktrees))}
		if configWarning != "" {
			details = append(details, configWarning)
		}
		details = append(details, c.brokenWorktrees...)
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusError,
			Message:  fmt.Sprintf("Push URL mismatch and %d broken worktree(s)", len(c.brokenWorktrees)),
			Details:  details,
			FixHint:  "Run 'gt doctor --fix --rig " + ctx.RigName + "' to repair",
			Category: c.Category(),
		}
	}
	details := []string{fmt.Sprintf("Bare repo exists at %s but %d worktree(s) have broken references", filepath.Join(ctx.RigPath(), ".repo.git"), len(c.brokenWorktrees))}
	if configWarning != "" {
		details = append(details, configWarning)
	}
	details = append(details, c.brokenWorktrees...)
	return &CheckResult{
		Name:     c.Name(),
		Status:   StatusError,
		Message:  fmt.Sprintf("%d worktree(s) have broken references in .repo.git", len(c.brokenWorktrees)),
		Details:  details,
		FixHint:  "Run 'gt doctor --fix --rig " + ctx.RigName + "' to recreate worktree entries",
		Category: c.Category(),
	}
}

func evaluateMissingBareRepo(c *BareRepoExistsCheck, ctx *CheckContext) *CheckResult {
	if len(c.brokenWorktrees) == 0 {
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusOK,
			Message:  "No worktrees depend on .repo.git",
			Category: c.Category(),
		}
	}
	bareRepoPath := filepath.Join(ctx.RigPath(), ".repo.git")
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("%d worktree(s) reference missing .repo.git", len(c.brokenWorktrees)),
		Details: append(
			[]string{"Missing: " + bareRepoPath},
			c.brokenWorktrees...,
		),
		FixHint:  "Run 'gt doctor --fix --rig " + ctx.RigName + "' to recreate .repo.git from remote",
		Category: c.Category(),
	}
}

func (c *BareRepoExistsCheck) Fix(ctx *CheckContext) error {
	if ctx.RigName == "" {
		return nil
	}
	if err := removeCorruptBareRepo(c, ctx); err != nil {
		return err
	}
	if err := fixBareRepoPushURL(c, ctx); err != nil {
		return err
	}
	if len(c.brokenWorktrees) == 0 && !c.bareRepoCorrupt {
		return nil
	}
	if err := recreateMissingBareRepo(ctx); err != nil {
		return err
	}
	reregisterBrokenBareRepoWorktrees(c, ctx)
	return nil
}

func removeCorruptBareRepo(c *BareRepoExistsCheck, ctx *CheckContext) error {
	if !c.bareRepoCorrupt {
		return nil
	}
	bareRepoPath := filepath.Join(ctx.RigPath(), ".repo.git")
	if _, err := os.Stat(bareRepoPath); err != nil {
		return nil
	}
	if healthErr := bareRepoHealth(bareRepoPath); healthErr == nil {
		c.bareRepoCorrupt = false
		return nil
	}
	return deleteCorruptBareRepo(c, ctx, bareRepoPath)
}

func deleteCorruptBareRepo(c *BareRepoExistsCheck, ctx *CheckContext, bareRepoPath string) error {
	refs := collectBareRepoReferences(ctx.RigPath(), ctx.RigName)
	c.recoveredHeads = make(map[string]string, len(refs))
	c.brokenWorktrees = c.brokenWorktrees[:0]
	for _, r := range refs {
		c.brokenWorktrees = append(c.brokenWorktrees, r.relPath)
		if r.headRef != "" {
			c.recoveredHeads[r.relPath] = r.headRef
		}
	}
	if rmErr := os.RemoveAll(bareRepoPath); rmErr != nil {
		return fmt.Errorf("removing corrupt .repo.git: %w", rmErr)
	}
	return nil
}

func fixBareRepoPushURL(c *BareRepoExistsCheck, ctx *CheckContext) error {
	if !c.pushURLMismatch {
		return nil
	}
	bareRepoPath := filepath.Join(ctx.RigPath(), ".repo.git")
	if _, statErr := os.Stat(bareRepoPath); statErr != nil {
		return nil
	}
	configPath := filepath.Join(ctx.RigPath(), "config.json")
	cfgData, readErr := os.ReadFile(configPath)
	if readErr != nil {
		return fmt.Errorf("cannot read config.json to fix push URL: %w", readErr)
	}
	var cfg struct {
		PushURL string `json:"push_url,omitempty"`
	}
	if jsonErr := json.Unmarshal(cfgData, &cfg); jsonErr != nil {
		return fmt.Errorf("cannot parse config.json to fix push URL: %w", jsonErr)
	}
	cfgPushURL := strings.TrimSpace(cfg.PushURL)
	if cfgPushURL == "" {
		return nil
	}
	if err := git.NewGitWithDir(bareRepoPath, "").ConfigurePushURL("origin", cfgPushURL); err != nil {
		return fmt.Errorf("updating push URL on .repo.git: %w", err)
	}
	return nil
}

func recreateMissingBareRepo(ctx *CheckContext) error {
	bareRepoPath := filepath.Join(ctx.RigPath(), ".repo.git")
	if _, err := os.Stat(bareRepoPath); err == nil {
		return nil
	}
	cfg, err := loadBareRepoCloneConfig(ctx)
	if err != nil {
		return err
	}
	return cloneBareRepo(bareRepoPath, cfg.gitURL, cfg.pushURL)
}

type bareRepoCloneConfig struct {
	gitURL  string
	pushURL string
}

func loadBareRepoCloneConfig(ctx *CheckContext) (bareRepoCloneConfig, error) {
	configPath := filepath.Join(ctx.RigPath(), "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return bareRepoCloneConfig{}, fmt.Errorf("cannot read config.json to get git_url: %w", err)
	}
	var cfg struct {
		GitURL  string `json:"git_url"`
		PushURL string `json:"push_url,omitempty"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return bareRepoCloneConfig{}, fmt.Errorf("cannot parse config.json: %w", err)
	}
	if cfg.GitURL == "" {
		return bareRepoCloneConfig{}, fmt.Errorf("config.json has no git_url, cannot recreate .repo.git")
	}
	return bareRepoCloneConfig{gitURL: cfg.GitURL, pushURL: cfg.PushURL}, nil
}

func cloneBareRepo(bareRepoPath, gitURL, pushURL string) error {
	cmd := exec.Command("git", "clone", "--bare", "--single-branch", "--depth", "1", gitURL, bareRepoPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cloning bare repo: %s", strings.TrimSpace(stderr.String()))
	}
	stderr.Reset()
	configCmd := exec.Command("git", "-C", bareRepoPath, "config",
		"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	configCmd.Stderr = &stderr
	if err := configCmd.Run(); err != nil {
		return fmt.Errorf("configuring refspec: %s", strings.TrimSpace(stderr.String()))
	}
	if pushURL == "" {
		return nil
	}
	stderr.Reset()
	pushURLCmd := exec.Command("git", "-C", bareRepoPath, "remote", "set-url", "--push", "origin", pushURL)
	pushURLCmd.Stderr = &stderr
	if err := pushURLCmd.Run(); err != nil {
		return fmt.Errorf("configuring push URL: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

func reregisterBrokenBareRepoWorktrees(c *BareRepoExistsCheck, ctx *CheckContext) {
	for _, relPath := range c.brokenWorktrees {
		reregisterBrokenBareRepoWorktree(c, ctx, relPath)
	}
}

func reregisterBrokenBareRepoWorktree(c *BareRepoExistsCheck, ctx *CheckContext, relPath string) {
	wtPath := filepath.Join(ctx.RigPath(), relPath)
	content, err := os.ReadFile(filepath.Join(wtPath, ".git"))
	if err != nil {
		return
	}
	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir: ") {
		return
	}
	gitdir := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(wtPath, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	wtMetaDir := filepath.Join(ctx.RigPath(), ".repo.git", "worktrees", filepath.Base(gitdir))
	if err := os.MkdirAll(wtMetaDir, 0755); err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(wtMetaDir, "gitdir"), []byte(wtPath+"/.git\n"), 0644); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(wtMetaDir, "HEAD"), []byte(recoveredBareRepoHead(c, relPath, gitdir)), 0644)
}

func recoveredBareRepoHead(c *BareRepoExistsCheck, relPath, gitdir string) string {
	if saved, ok := c.recoveredHeads[relPath]; ok && saved != "" {
		return saved
	}
	if oldHead, err := os.ReadFile(filepath.Join(gitdir, "HEAD")); err == nil {
		return string(oldHead)
	}
	return "ref: refs/heads/main\n"
}

func collectBareRepoReferences(rigPath, rigName string) []bareRepoWorktreeRef {
	bareRepoPrefix := filepath.Clean(filepath.Join(rigPath, ".repo.git", "worktrees")) + string(filepath.Separator)
	var refs []bareRepoWorktreeRef
	for _, wtDir := range findBareRepoWorktreeDirs(rigPath, rigName) {
		if ref, ok := parseBareRepoWorktreeRef(rigPath, bareRepoPrefix, wtDir); ok {
			refs = append(refs, ref)
		}
	}
	return refs
}

func parseBareRepoWorktreeRef(rigPath, bareRepoPrefix, wtDir string) (bareRepoWorktreeRef, bool) {
	gitFile := filepath.Join(wtDir, ".git")
	info, err := os.Stat(gitFile)
	if err != nil || info.IsDir() {
		return bareRepoWorktreeRef{}, false
	}
	content, err := os.ReadFile(gitFile)
	if err != nil {
		return bareRepoWorktreeRef{}, false
	}
	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir: ") {
		return bareRepoWorktreeRef{}, false
	}
	gitdir := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(wtDir, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	if !strings.HasPrefix(gitdir, bareRepoPrefix) {
		return bareRepoWorktreeRef{}, false
	}
	relPath, _ := filepath.Rel(rigPath, wtDir)
	if relPath == "" {
		relPath = wtDir
	}
	ref := bareRepoWorktreeRef{relPath: relPath}
	if headBytes, err := os.ReadFile(filepath.Join(gitdir, "HEAD")); err == nil {
		ref.headRef = string(headBytes)
	}
	return ref, true
}

func findBareRepoWorktreeDirs(rigPath, rigName string) []string {
	var dirs []string
	appendBareRepoDirIfExists(&dirs, filepath.Join(rigPath, "refinery", "rig"))
	appendBareRepoDirIfExists(&dirs, filepath.Join(rigPath, "witness", "rig"))
	appendPolecatBareRepoDirs(&dirs, filepath.Join(rigPath, "polecats"), rigName)
	return dirs
}

func appendBareRepoDirIfExists(dirs *[]string, path string) {
	if _, err := os.Stat(path); err == nil {
		*dirs = append(*dirs, path)
	}
}

func appendPolecatBareRepoDirs(dirs *[]string, polecatsDir, rigName string) {
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		appendOnePolecatBareRepoDir(dirs, polecatsDir, rigName, entry)
	}
}

func appendOnePolecatBareRepoDir(dirs *[]string, polecatsDir, rigName string, entry os.DirEntry) {
	if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
		return
	}
	newPath := filepath.Join(polecatsDir, entry.Name(), rigName)
	if _, err := os.Stat(newPath); err == nil {
		*dirs = append(*dirs, newPath)
	}
	oldPath := filepath.Join(polecatsDir, entry.Name())
	if oldPath == newPath {
		return
	}
	if _, err := os.Stat(filepath.Join(oldPath, ".git")); err == nil {
		*dirs = append(*dirs, oldPath)
	}
}
