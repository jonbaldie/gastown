// Package cmd provides polecat spawning utilities for gt sling.
package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/witness"
	"github.com/jonbaldie/gastown/internal/workspace"
)

const minPolecatDirsPerRig = 30

// SpawnedPolecatInfo contains info about a spawned polecat session.
type SpawnedPolecatInfo struct {
	RigName     string // Rig name (e.g., "gastown")
	PolecatName string // Polecat name (e.g., "Toast")
	ClonePath   string // Path to polecat's git worktree
	SessionName string // Tmux session name (e.g., "gt-gastown-p-Toast")
	Pane        string // Tmux pane ID (empty until StartSession is called)
	BaseBranch  string // Effective base branch (e.g., "main", "integration/epic-id")
	Branch      string // Git branch name (for cleanup on rollback)

	// Internal fields for deferred session start
	account string
	agent   string
}

// AgentID returns the agent identifier (e.g., "gastown/polecats/Toast")
func (s *SpawnedPolecatInfo) AgentID() string {
	return fmt.Sprintf("%s/polecats/%s", s.RigName, s.PolecatName)
}

// SessionStarted returns true if the tmux session has been started.
func (s *SpawnedPolecatInfo) SessionStarted() bool {
	return s.Pane != ""
}

// SlingSpawnOptions contains options for spawning a polecat via sling.
type SlingSpawnOptions struct {
	TownRoot      string // Gas Town workspace root; falls back to cwd when empty
	Force         bool   // Force spawn even if polecat has uncommitted work
	Account       string // Claude Code account handle to use
	Create        bool   // Create polecat if it doesn't exist (currently always true for sling)
	HookBead      string // Bead ID to set as hook_bead at spawn time (atomic assignment)
	Agent         string // Agent override for this spawn (e.g., "gemini", "codex", "claude-haiku")
	BaseBranch    string // Override base branch for polecat worktree (e.g., "develop", "release/v2")
	ResumeBranch  string // Resume an existing branch (e.g. PR head) instead of creating polecat/<name>/<bead>+<ts>
	SkipAdmission bool   // Caller already holds a polecat admission reservation
}

func effectivePolecatDirCap(configured int) int {
	if configured < minPolecatDirsPerRig {
		return minPolecatDirsPerRig
	}
	return configured
}

func reclaimBrokenIdlePolecatForSling(polecatMgr *polecat.Manager) (bool, error) {
	polecats, err := polecat.List(polecatMgr)
	if err != nil {
		return false, err
	}

	for _, candidate := range polecats {
		if candidate == nil || candidate.State != polecat.StateIdle || candidate.Issue != "" {
			continue
		}
		verifyErr := verifyWorktreeExists(candidate.ClonePath)
		if verifyErr == nil || !polecat.IsStructuralWorktreeError(verifyErr) {
			continue
		}

		fmt.Printf("  Reclaiming broken idle polecat %s before allocation: %v\n", candidate.Name, verifyErr)
		if err := polecat.ReclaimBrokenIdlePolecat(polecatMgr, candidate.Name); err != nil {
			fmt.Printf("  Broken idle polecat %s was not safe to reclaim: %v\n", candidate.Name, err)
			continue
		}
		fmt.Printf("  %s Broken idle polecat %s reclaimed before assigning new work\n", style.Bold.Render("✓"), candidate.Name)
		return true, nil
	}

	return false, nil
}

// SpawnPolecatForSling creates a fresh polecat and optionally starts its session.
// This is used by gt sling when the target is a rig name.
// The caller (sling) handles hook attachment and nudging.
func SpawnPolecatForSling(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
	s, err := beginPolecatSlingSpawn(rigName, opts)
	if err != nil {
		return nil, err
	}
	if s.admission != nil {
		defer s.admission.Release()
	}
	if err := preparePolecatSlingSpawn(s); err != nil {
		return nil, err
	}
	info, reused, err := reuseIdlePolecatForSling(s)
	if reused || err != nil {
		return info, err
	}
	if err := enforcePolecatDirCap(s); err != nil {
		return nil, err
	}
	return allocateNewPolecatForSling(s)
}

type polecatSlingSpawn struct {
	rigName    string
	opts       SlingSpawnOptions
	townRoot   string
	r          *rig.Rig
	polecatMgr *polecat.Manager
	t          *tmux.Tmux
	admission  *polecatAdmissionHandle
}

func beginPolecatSlingSpawn(rigName string, opts SlingSpawnOptions) (*polecatSlingSpawn, error) {
	townRoot := opts.TownRoot
	if townRoot == "" {
		var err error
		townRoot, err = workspace.FindFromCwdOrError()
		if err != nil {
			return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
		}
	}
	r, err := loadSlingSpawnRig(townRoot, rigName)
	if err != nil {
		return nil, err
	}
	t := tmux.NewTmux()
	polecatMgr := polecat.NewManager(r, git.NewGit(r.Path), t)
	if err := polecat.CheckDoltHealth(polecatMgr); err != nil {
		return nil, fmt.Errorf("pre-spawn health check failed: %w", err)
	}
	if err := polecat.CheckDoltServerCapacity(polecatMgr); err != nil {
		return nil, fmt.Errorf("admission control: %w", err)
	}
	if err := rejectParkedSlingSpawn(townRoot, rigName); err != nil {
		return nil, err
	}
	admission, err := reserveSlingSpawnAdmission(townRoot, rigName, opts)
	if err != nil {
		return nil, err
	}
	return &polecatSlingSpawn{
		rigName:    rigName,
		opts:       opts,
		townRoot:   townRoot,
		r:          r,
		polecatMgr: polecatMgr,
		t:          t,
		admission:  admission,
	}, nil
}

func loadSlingSpawnRig(townRoot, rigName string) (*rig.Rig, error) {
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}
	r, err := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot)).GetRig(rigName)
	if err != nil {
		return nil, fmt.Errorf("rig '%s' not found", rigName)
	}
	return r, nil
}

func rejectParkedSlingSpawn(townRoot, rigName string) error {
	blocked, reason := IsRigParkedOrDocked(townRoot, rigName)
	if !blocked {
		return nil
	}
	undoCmd := "gt rig unpark"
	if reason == "docked" {
		undoCmd = "gt rig undock"
	}
	return fmt.Errorf("cannot sling to %s rig %q\n%s %s", reason, rigName, undoCmd, rigName)
}

func reserveSlingSpawnAdmission(townRoot, rigName string, opts SlingSpawnOptions) (*polecatAdmissionHandle, error) {
	if opts.SkipAdmission {
		return nil, nil
	}
	admission, _, err := acquirePolecatAdmissionFn(townRoot, rigName, opts.HookBead, "spawn-or-reuse")
	if err != nil {
		return nil, err
	}
	return admission, nil
}

func preparePolecatSlingSpawn(s *polecatSlingSpawn) error {
	if err := guardBeadRespawn(s.townRoot, s.rigName, s.opts); err != nil {
		return err
	}
	reclaimed, err := reclaimBrokenIdlePolecatForSling(s.polecatMgr)
	if err != nil {
		style.PrintWarning("could not reclaim broken idle polecat before allocation: %v", err)
		return nil
	}
	if reclaimed {
		fmt.Println("  Allocating fresh polecat after reclaiming broken idle sandbox...")
	}
	return nil
}

func guardBeadRespawn(townRoot, rigName string, opts SlingSpawnOptions) error {
	if opts.HookBead == "" || opts.Force {
		return nil
	}
	if !witness.ShouldBlockRespawn(townRoot, opts.HookBead) {
		witness.RecordBeadRespawn(townRoot, opts.HookBead)
		return nil
	}
	maxRespawns := config.LoadOperationalConfig(townRoot).GetWitnessConfig().MaxBeadRespawnsV()
	return fmt.Errorf("respawn limit reached for %s (%d attempts). "+
		"This bead keeps failing — investigate before re-dispatching.\n"+
		"Override: gt sling %s %s --force\n"+
		"Reset:    gt sling respawn-reset %s",
		opts.HookBead, maxRespawns,
		opts.HookBead, rigName, opts.HookBead)
}

func detectSlingBaseBranch(r *rig.Rig, opts SlingSpawnOptions) string {
	baseBranch := opts.BaseBranch
	if opts.ResumeBranch != "" {
		return baseBranch
	}
	if baseBranch == "" && opts.HookBead != "" {
		baseBranch = detectIntegrationBaseBranch(r, opts.HookBead)
	}
	if baseBranch != "" && !strings.HasPrefix(baseBranch, "origin/") {
		baseBranch = "origin/" + baseBranch
	}
	return baseBranch
}

func detectIntegrationBaseBranch(r *rig.Rig, hookBead string) string {
	settingsPath := filepath.Join(r.Path, "settings", "config.json")
	polecatIntegrationEnabled := true
	if settings, err := config.LoadRigSettings(settingsPath); err == nil && settings.MergeQueue != nil {
		polecatIntegrationEnabled = config.IsPolecatIntegrationEnabled(settings.MergeQueue)
	}
	if !polecatIntegrationEnabled {
		return ""
	}
	repoGit, repoErr := getRigGit(r.Path)
	if repoErr != nil {
		return ""
	}
	detected, detectErr := beads.DetectIntegrationBranch(beads.New(r.Path), git.Checker{Git: repoGit}, hookBead)
	if detectErr != nil || detected == "" {
		return ""
	}
	fmt.Printf("  Auto-detected integration branch: %s\n", detected)
	return "origin/" + detected
}

func reuseIdlePolecatForSling(s *polecatSlingSpawn) (*SpawnedPolecatInfo, bool, error) {
	idlePolecat, findErr := polecat.FindIdlePolecat(s.polecatMgr)
	if findErr != nil || idlePolecat == nil {
		return nil, false, nil
	}
	polecatName := idlePolecat.Name
	fmt.Printf("Reusing idle polecat: %s\n", polecatName)
	baseBranch := detectSlingBaseBranch(s.r, s.opts)
	addOpts := polecat.AddOptions{
		HookBead:     s.opts.HookBead,
		BaseBranch:   baseBranch,
		ResumeBranch: s.opts.ResumeBranch,
	}
	if !tryReuseIdlePolecat(s.polecatMgr, polecatName, addOpts) {
		return nil, false, nil
	}
	info, err := finishReusedPolecat(s, polecatName, baseBranch)
	return info, err == nil, err
}

func tryReuseIdlePolecat(polecatMgr *polecat.Manager, polecatName string, addOpts polecat.AddOptions) bool {
	if _, err := polecat.ReuseIdlePolecat(polecatMgr, polecatName, addOpts); err != nil {
		if errors.Is(err, polecat.ErrPolecatNeedsRecovery) {
			fmt.Printf("  Idle polecat %s needs recovery before reuse: %v; allocating new...\n", polecatName, err)
			return false
		}
		fmt.Printf("  Branch-only reuse failed for idle polecat %s: %v; allocating new...\n", polecatName, err)
		return false
	}
	return true
}

func finishReusedPolecat(s *polecatSlingSpawn, polecatName, baseBranch string) (*SpawnedPolecatInfo, error) {
	polecatObj, err := polecat.Get(s.polecatMgr, polecatName)
	if err != nil {
		return nil, fmt.Errorf("getting idle polecat after reuse: %w", err)
	}
	if err := verifyWorktreeExists(polecatObj.ClonePath); err != nil {
		return nil, fmt.Errorf("worktree verification failed for reused %s: %w", polecatName, err)
	}
	sessionName := polecat.NewSessionManager(s.t, s.r).SessionName(polecatName)
	fmt.Printf("%s Polecat %s reused (idle → working, session start deferred)\n", style.Bold.Render("✓"), polecatName)
	_ = events.LogFeed(events.TypeSpawn, "gt", events.SpawnPayload(s.rigName, polecatName))
	return newSpawnedPolecatInfo(s, polecatName, polecatObj.ClonePath, sessionName, polecatObj.Branch, baseBranch), nil
}

func enforcePolecatDirCap(s *polecatSlingSpawn) error {
	maxPolecatDirsPerRig := effectivePolecatDirCap(s.r.GetIntConfig("max_polecats"))
	rigPolecatDir := filepath.Join(s.townRoot, s.rigName, "polecats")
	entries, err := os.ReadDir(rigPolecatDir)
	if err != nil {
		return nil
	}
	dirCount := 0
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirCount++
		}
	}
	if dirCount < maxPolecatDirsPerRig {
		return nil
	}
	return fmt.Errorf("rig %s has %d polecat directories (max %d). "+
		"Resolve recovery-needed polecats before allocating more slots: gt polecat list %s",
		s.rigName, dirCount, maxPolecatDirsPerRig, s.rigName)
}

func allocateNewPolecatForSling(s *polecatSlingSpawn) (*SpawnedPolecatInfo, error) {
	baseBranch := detectSlingBaseBranch(s.r, s.opts)
	addOpts := polecat.AddOptions{
		HookBead:     s.opts.HookBead,
		BaseBranch:   baseBranch,
		ResumeBranch: s.opts.ResumeBranch,
	}
	polecatName, _, err := polecat.AllocateAndAdd(s.polecatMgr, addOpts)
	if err != nil {
		return nil, fmt.Errorf("allocating and creating polecat: %w", err)
	}
	fmt.Printf("Created polecat: %s\n", polecatName)
	polecatObj, err := polecat.Get(s.polecatMgr, polecatName)
	if err != nil {
		return nil, fmt.Errorf("getting polecat after creation: %w", err)
	}
	if err := verifyWorktreeExists(polecatObj.ClonePath); err != nil {
		_ = polecat.Remove(s.polecatMgr, polecatName, true)
		return nil, fmt.Errorf("worktree verification failed for %s: %w\nHint: try 'gt polecat nuke %s/%s --force' to clean up",
			polecatName, err, s.rigName, polecatName)
	}
	sessionName := polecat.NewSessionManager(s.t, s.r).SessionName(polecatName)
	fmt.Printf("%s Polecat %s spawned (session start deferred)\n", style.Bold.Render("✓"), polecatName)
	_ = events.LogFeed(events.TypeSpawn, "gt", events.SpawnPayload(s.rigName, polecatName))
	return newSpawnedPolecatInfo(s, polecatName, polecatObj.ClonePath, sessionName, polecatObj.Branch, baseBranch), nil
}

func newSpawnedPolecatInfo(s *polecatSlingSpawn, polecatName, clonePath, sessionName, branch, baseBranch string) *SpawnedPolecatInfo {
	effectiveBranch := strings.TrimPrefix(baseBranch, "origin/")
	if effectiveBranch == "" {
		effectiveBranch = s.r.DefaultBranch()
	}
	if s.opts.ResumeBranch != "" {
		effectiveBranch = s.opts.ResumeBranch
	}
	return &SpawnedPolecatInfo{
		RigName:     s.rigName,
		PolecatName: polecatName,
		ClonePath:   clonePath,
		SessionName: sessionName,
		Pane:        "",
		BaseBranch:  effectiveBranch,
		Branch:      branch,
		account:     s.opts.Account,
		agent:       s.opts.Agent,
	}
}

// StartSession starts the tmux session for a spawned polecat.
// This is called after the molecule/bead is attached, so the polecat
// sees its work when gt prime runs on session start.
// Returns the pane ID after session start.
func (s *SpawnedPolecatInfo) StartSession() (string, error) {
	if s.SessionStarted() {
		return s.Pane, nil
	}
	prepared, err := prepareSpawnedPolecatSession(s)
	if err != nil {
		return "", err
	}
	fmt.Printf("Starting session for %s/%s...\n", s.RigName, s.PolecatName)
	startOpts := polecat.SessionStartOptions{
		RuntimeConfigDir: prepared.claudeConfigDir,
		Agent:            s.agent,
	}
	if err := polecat.NewSessionManager(prepared.t, prepared.r).Start(s.PolecatName, startOpts); err != nil {
		return "", fmt.Errorf("starting session: %w", err)
	}
	waitSpawnedPolecatRuntime(s, prepared.r, prepared.t)
	updateSpawnedPolecatWorkingState(s, prepared.r, prepared.t)
	return finishSpawnedPolecatSession(s, prepared.t)
}

type spawnedPolecatSession struct {
	r               *rig.Rig
	t               *tmux.Tmux
	claudeConfigDir string
}

func prepareSpawnedPolecatSession(s *SpawnedPolecatInfo) (spawnedPolecatSession, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return spawnedPolecatSession{}, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	r, err := loadSlingSpawnRig(townRoot, s.RigName)
	if err != nil {
		return spawnedPolecatSession{}, err
	}
	claudeConfigDir, _, err := config.ResolveAccountConfigDir(constants.MayorAccountsPath(townRoot), s.account)
	if err != nil {
		return spawnedPolecatSession{}, fmt.Errorf("resolving account: %w", err)
	}
	return spawnedPolecatSession{r: r, t: tmux.NewTmux(), claudeConfigDir: claudeConfigDir}, nil
}

func waitSpawnedPolecatRuntime(s *SpawnedPolecatInfo, r *rig.Rig, t *tmux.Tmux) {
	spawnTownRoot := filepath.Dir(r.Path)
	runtimeConfig := config.ResolveRoleAgentConfig("polecat", spawnTownRoot, r.Path)
	if s.agent != "" {
		rc, _, err := config.ResolveAgentConfigWithOverride(spawnTownRoot, r.Path, s.agent)
		if err != nil {
			style.PrintWarning("resolving agent config for %s: %v (using default)", s.agent, err)
		} else {
			runtimeConfig = rc
		}
	}
	if err := t.WaitForRuntimeReady(s.SessionName, runtimeConfig, 30*time.Second); err != nil {
		style.PrintWarning("runtime may not be fully ready: %v", err)
	}
}

func updateSpawnedPolecatWorkingState(s *SpawnedPolecatInfo, r *rig.Rig, t *tmux.Tmux) {
	polecatMgr := polecat.NewManager(r, git.NewGit(r.Path), t)
	if err := polecat.SetAgentStateWithRetry(polecatMgr, s.PolecatName, "working"); err != nil {
		style.PrintWarning("could not update agent state after retries: %v", err)
	}
	if err := polecat.SetState(polecatMgr, s.PolecatName, polecat.StateWorking); err != nil {
		style.PrintWarning("could not update issue status to in_progress: %v", err)
	}
}

func finishSpawnedPolecatSession(s *SpawnedPolecatInfo, t *tmux.Tmux) (string, error) {
	pane, err := getSessionPane(s.SessionName)
	if err != nil {
		_ = t.KillSession(s.SessionName)
		return "", fmt.Errorf("getting pane for %s (session likely died during startup): %w", s.SessionName, err)
	}
	s.Pane = pane
	return pane, nil
}

// IsRigName checks if a target string is a rig name (not a role or path).
// Returns the rig name and true if it's a valid rig.
func IsRigName(target string) (string, bool) {
	// If it contains a slash, it's a path format (rig/role or rig/crew/name)
	if strings.Contains(target, "/") {
		return "", false
	}

	// Check known non-rig role names
	switch strings.ToLower(target) {
	case constants.RoleMayor, "may", constants.RoleDeacon, "dea", constants.RoleCrew, constants.RoleWitness, "wit", constants.RoleRefinery, "ref":
		return "", false
	}

	// Try to load as a rig
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", false
	}

	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return "", false
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	_, err = rigMgr.GetRig(target)
	if err != nil {
		return "", false
	}

	return target, true
}

// verifyWorktreeExists checks that a git worktree was actually created at the given path
// and that it is a functional git repository. Returns an error if the worktree is missing,
// has a broken .git reference, or fails basic git validation. (GH#2056)
func verifyWorktreeExists(clonePath string) error {
	return polecat.VerifyWorktreeExists(clonePath)
}
