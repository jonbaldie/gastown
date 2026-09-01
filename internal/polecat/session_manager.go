// Package polecat provides polecat workspace and session management.
package polecat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/util"
)

// debugSession logs non-fatal errors during session startup when GT_DEBUG_SESSION=1.
func debugSession(context string, err error) {
	if os.Getenv("GT_DEBUG_SESSION") != "" && err != nil {
		fmt.Fprintf(os.Stderr, "[session-debug] %s: %v\n", context, err)
	}
}

// Session errors
var (
	ErrSessionRunning  = errors.New("session already running")
	ErrSessionNotFound = errors.New("session not found")
	ErrIssueInvalid    = errors.New("issue not found or tombstoned")
)

// SessionManager handles polecat session lifecycle.
type SessionManager struct {
	tmux *tmux.Tmux
	rig  *rig.Rig
}

// NewSessionManager creates a new polecat session manager for a rig.
func NewSessionManager(t *tmux.Tmux, r *rig.Rig) *SessionManager {
	return &SessionManager{
		tmux: t,
		rig:  r,
	}
}

// SessionStartOptions configures polecat session startup.
type SessionStartOptions struct {
	// WorkDir overrides the default working directory (polecat clone dir).
	WorkDir string

	// Issue is an optional issue ID to work on.
	Issue string

	// Command overrides the default "claude" command.
	Command string

	// Account specifies the account handle to use (overrides default).
	Account string

	// RuntimeConfigDir is resolved config directory for the runtime account.
	// If set, this is injected as an environment variable.
	RuntimeConfigDir string

	// Agent is the agent override for this polecat session (e.g., "codex", "gemini").
	// If set, GT_AGENT is written to the tmux session environment table so that
	// IsAgentAlive and waitForPolecatReady read the correct process names.
	Agent string
}

// SessionInfo contains information about a running polecat session.
type SessionInfo struct {
	// Polecat is the polecat name.
	Polecat string `json:"polecat"`

	// SessionID is the tmux session identifier.
	SessionID string `json:"session_id"`

	// Running indicates if the session is currently active.
	Running bool `json:"running"`

	// RigName is the rig this session belongs to.
	RigName string `json:"rig_name"`

	// Attached indicates if someone is attached to the session.
	Attached bool `json:"attached,omitempty"`

	// Created is when the session was created.
	Created time.Time `json:"created,omitempty"`

	// Windows is the number of tmux windows.
	Windows int `json:"windows,omitempty"`

	// LastActivity is when the session last had activity.
	LastActivity time.Time `json:"last_activity,omitempty"`
}

// SessionName generates the tmux session name for a polecat.
// Validates that the polecat name doesn't contain the rig prefix to prevent
// double-prefix bugs (e.g., "gt-gastown_manager-gastown_manager-142").
func (m *SessionManager) SessionName(polecat string) string {
	sessionName := session.PolecatSessionName(session.PrefixFor(m.rig.Name), polecat)

	// Validate session name format to detect double-prefix bugs
	if err := validateSessionName(sessionName, m.rig.Name); err != nil {
		// Log warning but don't fail - allow the session to be created
		// so we can track and clean up malformed sessions later
		fmt.Fprintf(os.Stderr, "Warning: malformed session name: %v\n", err)
	}

	return sessionName
}

// validateSessionName checks for double-prefix session names.
// Returns an error if the session name has the rig prefix duplicated.
// Example bad name: "gt-gastown_manager-gastown_manager-142"
func validateSessionName(sessionName, rigName string) error {
	// Expected format: gt-<rig>-<name>
	// Check if the name part starts with the rig prefix (indicates double-prefix bug)
	prefix := session.PrefixFor(rigName) + "-"
	if !strings.HasPrefix(sessionName, prefix) {
		return nil // Not our rig, can't validate
	}

	namePart := strings.TrimPrefix(sessionName, prefix)

	// Check if name part starts with rig name followed by hyphen
	// This indicates overflow name included rig prefix: gt-<rig>-<rig>-N
	if strings.HasPrefix(namePart, rigName+"-") {
		return fmt.Errorf("double-prefix detected: %s (expected format: gt-%s-<name>)",
			sessionName, rigName)
	}

	return nil
}

// polecatDir returns the parent directory for a polecat.
// This is polecats/<name>/ - the polecat's home directory.
func (m *SessionManager) polecatDir(polecat string) string {
	return filepath.Join(m.rig.Path, "polecats", polecat)
}

// clonePath returns the path where the git worktree lives.
// New structure: polecats/<name>/<rigname>/ - gives LLMs recognizable repo context.
// Falls back to old structure: polecats/<name>/ for backward compatibility.
func (m *SessionManager) clonePath(polecat string) string {
	// New structure: polecats/<name>/<rigname>/
	newPath := filepath.Join(m.rig.Path, "polecats", polecat, m.rig.Name)
	if info, err := os.Stat(newPath); err == nil && info.IsDir() {
		return newPath
	}

	// Old structure: polecats/<name>/ (backward compat)
	oldPath := filepath.Join(m.rig.Path, "polecats", polecat)
	if info, err := os.Stat(oldPath); err == nil && info.IsDir() {
		// Check if this is actually a git worktree (has .git file or dir)
		gitPath := filepath.Join(oldPath, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			return oldPath
		}
	}

	// Default to new structure for new polecats
	return newPath
}

// freshBranchName returns a unique branch name for a new polecat session.
// Mirrors the naming convention in Manager.buildBranchName:
//   - polecat/<name>/<issue>+<timestamp> when an issue is known
//   - polecat/<name>-<timestamp> otherwise
//
// parseFreshBranchName is the structural inverse.
func sessionFreshBranchName(polecatName, issue string) string {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 36)
	return FormatGeneratedBranchName(polecatName, issue, ts)
}

// freshBranchMeta holds the identity decoded from a branch produced by
// freshBranchName. ok=false means the branch does not match either format.
type freshBranchMeta struct {
	polecat string
	issue   string // empty when the branch has no issue binding
	ok      bool
}

// parseFreshBranchName is the structural inverse of freshBranchName. It
// does not consult git or the filesystem; it recognizes the two formats
// the formatter emits. Used in place of substring heuristics so that
// branch-naming changes can be made in a single place.
func parseFreshBranchName(branch string) freshBranchMeta {
	meta, ok := ParseGeneratedBranchName(branch)
	if !ok {
		return freshBranchMeta{}
	}
	return freshBranchMeta{polecat: meta.Polecat, issue: meta.Issue, ok: true}
}

func sessionCanonicalStartPoint(m *SessionManager, g *git.Git) string {
	defaultBranch := ""
	if rigCfg, err := rig.LoadRigConfig(m.rig.Path); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	if defaultBranch == "" {
		defaultBranch = git.RemoteDefaultBranch(g)
	}
	if defaultBranch == "" {
		return ""
	}
	return fmt.Sprintf("origin/%s", defaultBranch)
}

// shouldCreateFreshSessionBranch decides whether the session manager should
// replace the worktree's current branch with a new polecat branch based on
// the canonical remote base. Decisions are made from structured data —
// parseFreshBranchName output and the computed canonical branch — not from
// substring heuristics on the branch name.
func shouldCreateFreshSessionBranch(currentBranch, issue, canonicalBranch string) bool {
	meta := parseFreshBranchName(currentBranch)

	// Same-issue respawn: keep the existing polecat branch so preserved work
	// for this issue isn't discarded.
	if meta.ok && issue != "" && meta.issue == issue {
		return false
	}

	// On the canonical base branch — need a fresh polecat branch to work on.
	if canonicalBranch != "" && currentBranch == canonicalBranch {
		return true
	}

	// On some other polecat branch belonging to a different issue — fresh
	// branch is safer than inheriting unrelated preserved history.
	return issue != "" && meta.ok
}

func sessionEnsureCanonicalBranch(m *SessionManager, g *git.Git, polecat string, opts SessionStartOptions) string {
	currentBranch, err := git.CurrentBranch(g)
	if err != nil {
		return ""
	}

	startPoint := sessionCanonicalStartPoint(m, g)
	if startPoint == "" {
		debugSession("canonical session start point unresolved", fmt.Errorf("no default branch in rig config or remote"))
		return currentBranch
	}
	canonicalBranch := strings.TrimPrefix(startPoint, "origin/")
	if !shouldCreateFreshSessionBranch(currentBranch, opts.Issue, canonicalBranch) {
		return currentBranch
	}

	// Refresh origin refs before branching so recovered sessions start from the
	// canonical remote base instead of any preserved local polecat branch.
	if err := git.Fetch(g, "origin"); err != nil {
		debugSession("fetch origin for canonical session branch", err)
	}

	exists, err := git.RefExists(g, startPoint)
	if err != nil {
		debugSession("check canonical session start point", err)
		return currentBranch
	}
	if !exists {
		debugSession("missing canonical session start point", fmt.Errorf("%s", startPoint))
		return currentBranch
	}

	newBranch := sessionFreshBranchName(polecat, opts.Issue)
	if err := git.CheckoutNewBranch(g, newBranch, startPoint); err != nil {
		debugSession("auto-checkout fresh branch on canonical base", err)
		return currentBranch
	}

	return newBranch
}

// hasPolecat checks if the polecat exists in this rig.
func sessionHasPolecat(m *SessionManager, polecat string) bool {
	polecatPath := m.polecatDir(polecat)
	info, err := os.Stat(polecatPath)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// polecatSlot returns a unique integer slot index for this polecat based on its
// position among existing polecat directories. This enables port offsetting and
// resource isolation when multiple polecats run in parallel (GH#954).
func sessionPolecatSlot(m *SessionManager, polecat string) int {
	polecatsDir := filepath.Join(m.rig.Path, "polecats")
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return 0
	}
	slot := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.Name() == polecat {
			return slot
		}
		slot++
	}
	return slot
}

// Start creates and starts a new session for a polecat.
type sessionStartPlan struct {
	m                     *SessionManager
	polecat               string
	opts                  SessionStartOptions
	sessionID             string
	workDir               string
	townRoot              string
	runtimeConfig         *config.RuntimeConfig
	extra                 map[string]string
	beacon                session.BeaconConfig
	fallbackInfo          *runtime.StartupFallbackInfo
	startupNudgeContent   string
	startupPromptFallback string
}

func (m *SessionManager) Start(polecat string, opts SessionStartOptions) error {
	p, err := preparePolecatSessionStart(m, polecat, opts)
	if err != nil {
		return err
	}
	if err := launchPolecatSession(p); err != nil {
		return err
	}
	return finishPolecatSessionStart(p)
}

func preparePolecatSessionStart(m *SessionManager, polecat string, opts SessionStartOptions) (*sessionStartPlan, error) {
	if !sessionHasPolecat(m, polecat) {
		return nil, fmt.Errorf("%w: %s", ErrPolecatNotFound, polecat)
	}
	sessionID := m.SessionName(polecat)
	if err := killStalePolecatTmuxSession(m, sessionID); err != nil {
		return nil, err
	}
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = m.clonePath(polecat)
	}
	if opts.Issue != "" {
		if err := sessionValidateIssue(m, opts.Issue, workDir); err != nil {
			return nil, err
		}
	}
	p := &sessionStartPlan{
		m:         m,
		polecat:   polecat,
		opts:      opts,
		sessionID: sessionID,
		workDir:   workDir,
		townRoot:  filepath.Dir(m.rig.Path),
	}
	if err := resolveSessionStartRuntime(p); err != nil {
		return nil, err
	}
	buildSessionStartBeacon(p)
	buildSessionStartEnv(p)
	return p, nil
}

func killStalePolecatTmuxSession(m *SessionManager, sessionID string) error {
	if _, err := session.KillExistingSession(m.tmux, filepath.Dir(m.rig.Path), sessionID, true); err != nil {
		if errors.Is(err, session.ErrSessionAlive) {
			return fmt.Errorf("%w: %s", ErrSessionRunning, sessionID)
		}
		return fmt.Errorf("killing stale session %s: %w", sessionID, err)
	}
	return nil
}

func resolveSessionStartRuntime(p *sessionStartPlan) error {
	if p.opts.Agent == "" {
		p.runtimeConfig = config.ResolveRoleAgentConfig("polecat", p.townRoot, p.m.rig.Path)
		return nil
	}
	rc, _, err := config.ResolveAgentConfigWithOverride(p.townRoot, p.m.rig.Path, p.opts.Agent)
	if err != nil {
		return fmt.Errorf("resolving agent config for %s: %w", p.opts.Agent, err)
	}
	p.runtimeConfig = rc
	return nil
}

func buildSessionStartBeacon(p *sessionStartPlan) {
	p.fallbackInfo = runtime.GetStartupFallbackInfo(p.runtimeConfig)
	p.beacon = session.BeaconConfig{
		Recipient:               session.BeaconRecipient("polecat", p.polecat, p.m.rig.Name),
		Sender:                  "witness",
		Topic:                   "assigned",
		MolID:                   p.opts.Issue,
		IncludePrimeInstruction: p.fallbackInfo.IncludePrimeInBeacon,
		ExcludeWorkInstructions: p.fallbackInfo.SendStartupNudge,
	}
	p.startupNudgeContent = runtime.StartupNudgeContent()
	p.startupPromptFallback = session.BuildStartupPrompt(p.beacon, p.startupNudgeContent)
}

func buildSessionStartEnv(p *sessionStartPlan) {
	p.extra = map[string]string{
		"GT_POLECAT_PATH": p.workDir,
		"GT_TOWN_ROOT":    p.townRoot,
		"POLECAT_SLOT":    fmt.Sprintf("%d", sessionPolecatSlot(p.m, p.polecat)),
	}
	if g := git.NewGit(p.workDir); g != nil {
		if branch := sessionEnsureCanonicalBranch(p.m, g, p.polecat, p.opts); branch != "" {
			p.extra["GT_BRANCH"] = branch
		}
	}
}

func launchPolecatSession(p *sessionStartPlan) error {
	result, err := session.StartSession(p.m.tmux, "polecat", session.Work{
		SessionID:        p.sessionID,
		WorkDir:          p.workDir,
		TownRoot:         p.townRoot,
		RigPath:          p.m.rig.Path,
		RigName:          p.m.rig.Name,
		AgentName:        p.polecat,
		Command:          p.opts.Command,
		Beacon:           p.beacon,
		AgentOverride:    p.opts.Agent,
		RuntimeConfigDir: p.opts.RuntimeConfigDir,
		ExtraEnv:         p.extra,
		Theme:            tmux.ResolveSessionTheme(p.townRoot, p.m.rig.Name, "polecat", p.polecat),
	})
	if err != nil {
		return err
	}
	p.runtimeConfig = result.RuntimeConfig
	return nil
}

func finishPolecatSessionStart(p *sessionStartPlan) error {
	hookSessionStartIssue(p)
	debugSession("SetPaneDiedHook", p.m.tmux.SetPaneDiedHook(p.sessionID, fmt.Sprintf("%s/%s", p.m.rig.Name, p.polecat)))
	deliverSessionStartFallbacks(p)
	return requireSessionStartGTAgent(p)
}

func hookSessionStartIssue(p *sessionStartPlan) {
	if p.opts.Issue == "" {
		return
	}
	agentID := fmt.Sprintf("%s/polecats/%s", p.m.rig.Name, p.polecat)
	if err := sessionHookIssue(p.m, p.opts.Issue, agentID, p.workDir); err != nil {
		style.PrintWarning("could not hook issue %s: %v", p.opts.Issue, err)
	}
}

func deliverSessionStartFallbacks(p *sessionStartPlan) {
	if p.fallbackInfo.SendBeaconNudge {
		debugSession("DeliverStartupPromptFallback",
			runtime.DeliverStartupPromptFallback(p.m.tmux, p.sessionID, p.startupPromptFallback, p.runtimeConfig, constants.ClaudeStartTimeout))
	} else {
		deliverSessionStartNudge(p)
	}
	verifySessionStartNudge(p)
	_ = runtime.RunStartupFallback(p.m.tmux, p.sessionID, "polecat", p.runtimeConfig)
}

func deliverSessionStartNudge(p *sessionStartPlan) {
	if p.fallbackInfo.StartupNudgeDelayMs > 0 {
		primeWaitRC := runtime.RuntimeConfigWithMinDelay(p.runtimeConfig, p.fallbackInfo.StartupNudgeDelayMs)
		debugSession("WaitForPrimeReady", p.m.tmux.WaitForRuntimeReady(p.sessionID, primeWaitRC, constants.ClaudeStartTimeout))
	}
	if p.fallbackInfo.SendStartupNudge {
		debugSession("SendStartupNudge", p.m.tmux.NudgeSession(p.sessionID, p.startupNudgeContent))
	}
}

func verifySessionStartNudge(p *sessionStartPlan) {
	if p.fallbackInfo.SendStartupNudge {
		verifyContent := p.startupNudgeContent
		if p.fallbackInfo.SendBeaconNudge {
			verifyContent = p.startupPromptFallback
		}
		sessionVerifyStartupNudge(p.m, p.sessionID, p.runtimeConfig, verifyContent)
		return
	}
	if !p.fallbackInfo.SendBeaconNudge {
		go sessionVerifyStartupNudge(p.m, p.sessionID, p.runtimeConfig, p.startupNudgeContent)
	}
}

func requireSessionStartGTAgent(p *sessionStartPlan) error {
	gtAgent, _ := p.m.tmux.GetEnvironment(p.sessionID, "GT_AGENT")
	if gtAgent != "" {
		TouchSessionHeartbeat(p.townRoot, p.sessionID)
		return nil
	}
	_ = p.m.tmux.KillSessionWithProcesses(p.sessionID)
	return fmt.Errorf("GT_AGENT not set in session %s (command=%q); "+
		"witness patrol will misidentify this polecat as a zombie and auto-nuke it. "+
		"Ensure RuntimeConfig.ResolvedAgent is set during agent config resolution",
		p.sessionID, p.runtimeConfig.Command)
}

// IsSessionStale checks if a tmux session's pane process has died.
// A stale session exists in tmux but its main process (the agent) is no longer running.
// This happens when the agent crashes during startup but tmux keeps the dead pane.
// Delegates to isSessionProcessDead to avoid duplicating process-check logic (gt-qgzj1h).
func SessionIsStale(m *SessionManager, sessionID string) bool {
	return isSessionProcessDead(m.tmux, sessionID, filepath.Dir(m.rig.Path))
}

// Stop terminates a polecat session.
func (m *SessionManager) Stop(polecat string, force bool) error {
	sessionID := m.SessionName(polecat)

	err := session.StopSession(m.tmux, filepath.Dir(m.rig.Path), sessionID, !force)
	if errors.Is(err, session.ErrNotFound) {
		return ErrSessionNotFound
	}
	return err
}

// IsRunning checks if a polecat session is active and healthy.
// Checks both tmux session existence AND agent process liveness to avoid
// reporting zombie sessions (tmux alive but Claude dead) as "running".
func (m *SessionManager) IsRunning(polecat string) (bool, error) {
	sessionID := m.SessionName(polecat)
	status := m.tmux.CheckSessionHealth(sessionID, 0)
	return status == tmux.SessionHealthy, nil
}

// Status returns detailed status for a polecat session.
func (m *SessionManager) Status(polecat string) (*SessionInfo, error) {
	sessionID := m.SessionName(polecat)
	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("checking session: %w", err)
	}
	info := &SessionInfo{
		Polecat:   polecat,
		SessionID: sessionID,
		Running:   running,
		RigName:   m.rig.Name,
	}
	if !running {
		return info, nil
	}
	return fillRunningSessionInfo(m, info, sessionID)
}

func fillRunningSessionInfo(m *SessionManager, info *SessionInfo, sessionID string) (*SessionInfo, error) {
	tmuxInfo, err := m.tmux.GetSessionInfo(sessionID)
	if err != nil {
		return info, nil
	}
	info.Attached = tmuxInfo.Attached
	info.Windows = tmuxInfo.Windows
	parseSessionCreated(info, tmuxInfo.Created)
	parseSessionActivity(info, tmuxInfo.Activity)
	return info, nil
}

func parseSessionCreated(info *SessionInfo, created string) {
	if created == "" {
		return
	}
	formats := []string{
		"2006-01-02 15:04:05",
		"Mon Jan 2 15:04:05 2006",
		"Mon Jan _2 15:04:05 2006",
		time.ANSIC,
		time.UnixDate,
	}
	for _, format := range formats {
		if t, err := time.Parse(format, created); err == nil {
			info.Created = t
			return
		}
	}
}

func parseSessionActivity(info *SessionInfo, activity string) {
	if activity == "" {
		return
	}
	var activityUnix int64
	if _, err := fmt.Sscanf(activity, "%d", &activityUnix); err == nil && activityUnix > 0 {
		info.LastActivity = time.Unix(activityUnix, 0)
	}
}

// List returns information about all sessions for this rig.
// This includes polecats, witness, refinery, and crew sessions.
// Use ListPolecats() to get only polecat sessions.
func (m *SessionManager) List() ([]SessionInfo, error) {
	sessions, err := m.tmux.ListSessions()
	if err != nil {
		return nil, err
	}

	prefix := session.PrefixFor(m.rig.Name) + "-"
	var infos []SessionInfo

	for _, sessionID := range sessions {
		if !strings.HasPrefix(sessionID, prefix) {
			continue
		}

		polecat := strings.TrimPrefix(sessionID, prefix)
		infos = append(infos, SessionInfo{
			Polecat:   polecat,
			SessionID: sessionID,
			Running:   true,
			RigName:   m.rig.Name,
		})
	}

	return infos, nil
}

// ListPolecats returns information only about polecat sessions for this rig.
// Filters out witness, refinery, and crew sessions.
func (m *SessionManager) ListPolecats() ([]SessionInfo, error) {
	infos, err := m.List()
	if err != nil {
		return nil, err
	}

	var filtered []SessionInfo
	for _, info := range infos {
		// Skip non-polecat sessions
		if info.Polecat == "witness" || info.Polecat == "refinery" || strings.HasPrefix(info.Polecat, "crew-") {
			continue
		}
		filtered = append(filtered, info)
	}

	return filtered, nil
}

// Attach attaches to a polecat session.
func (m *SessionManager) Attach(polecat string) error {
	sessionID := m.SessionName(polecat)

	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return ErrSessionNotFound
	}

	return m.tmux.AttachSession(sessionID)
}

// Capture returns the recent output from a polecat session.
func (m *SessionManager) Capture(polecat string, lines int) (string, error) {
	sessionID := m.SessionName(polecat)

	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return "", fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return "", ErrSessionNotFound
	}

	return m.tmux.CapturePane(sessionID, lines)
}

// CaptureSession returns the recent output from a session by raw session ID.
func CaptureSession(m *SessionManager, sessionID string, lines int) (string, error) {
	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return "", fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return "", ErrSessionNotFound
	}

	return m.tmux.CapturePane(sessionID, lines)
}

// Inject sends a message to a polecat session.
func Inject(m *SessionManager, polecat, message string) error {
	sessionID := m.SessionName(polecat)

	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return ErrSessionNotFound
	}

	debounceMs := 200 + (len(message)/1024)*100
	if debounceMs > 1500 {
		debounceMs = 1500
	}

	return m.tmux.SendKeysDebounced(sessionID, message, debounceMs)
}

// StopAll terminates all polecat sessions for this rig.
func (m *SessionManager) StopAll(force bool) error {
	infos, err := m.ListPolecats()
	if err != nil {
		return err
	}

	var errs []error
	for _, info := range infos {
		if err := m.Stop(info.Polecat, force); err != nil {
			errs = append(errs, fmt.Errorf("stopping %s: %w", info.Polecat, err))
		}
	}

	return errors.Join(errs...)
}

// resolveBeadsDir determines the correct working directory for bd commands
// on a given issue. This enables cross-rig beads resolution via routes.jsonl.
// This is the core fix for GitHub issue #1056.
func sessionResolveBeadsDir(m *SessionManager, issueID, fallbackDir string) string {
	townRoot := filepath.Dir(m.rig.Path)
	return beads.ResolveHookDir(townRoot, issueID, fallbackDir)
}

// validateIssue checks that an issue exists and is not in a terminal state.
// This must be called before starting a session to avoid CPU spin loops
// from agents retrying work on invalid issues.
func sessionValidateIssue(m *SessionManager, issueID, workDir string) error {
	bdWorkDir := sessionResolveBeadsDir(m, issueID, workDir)

	ctx, cancel := context.WithTimeout(context.Background(), constants.BdCommandTimeout)
	defer cancel()
	cmd := beads.SpawnContext(ctx, "show", issueID, "--json") //nolint:gosec // G204: bd is a trusted internal tool
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = bdWorkDir
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%w: %s", ErrIssueInvalid, issueID)
	}

	var issues []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &issues); err != nil {
		return fmt.Errorf("parsing issue: %w", err)
	}
	if len(issues) == 0 {
		return fmt.Errorf("%w: %s", ErrIssueInvalid, issueID)
	}
	if beads.IssueStatus(issues[0].Status).IsTerminal() {
		return fmt.Errorf("%w: %s has terminal status %s", ErrIssueInvalid, issueID, issues[0].Status)
	}
	return nil
}

// verifyStartupNudgeDelivery checks if the polecat started working after the
// startup nudge and retries the nudge if the agent is truly idle.
// This fixes the Mode B race condition (GH#1379) where the startup nudge arrives
// before Claude Code is ready, causing the polecat to sit idle.
//
// Uses IsIdle (not IsAtPrompt) to distinguish "idle at prompt" from "busy
// processing". IsIdle checks for the "esc to interrupt" busy indicator in
// Claude's status bar — if present, the agent is actively working even though
// the ❯ prompt may still be visible in the pane. This prevents the false-
// positive retries that interrupted Claude mid-processing (GH#3031).
//
// Non-fatal: if verification fails or times out, the session is left running.
// The witness zombie patrol will eventually detect and handle truly idle polecats.
func sessionVerifyStartupNudge(m *SessionManager, sessionID string, rc *config.RuntimeConfig, retryContent string) {
	if !canVerifyStartupNudge(rc) {
		return
	}
	retryStartupNudgeDelivery(m, sessionID, startupNudgeRetryContent(retryContent))
}

func canVerifyStartupNudge(rc *config.RuntimeConfig) bool {
	return rc != nil && rc.Tmux != nil && rc.Tmux.ReadyPromptPrefix != ""
}

func startupNudgeRetryContent(retryContent string) string {
	if strings.TrimSpace(retryContent) != "" {
		return retryContent
	}
	return runtime.StartupNudgeContent()
}

func retryStartupNudgeDelivery(m *SessionManager, sessionID, retryContent string) {
	sessionCfg := config.LoadOperationalConfig(filepath.Dir(m.rig.Path)).GetSessionConfig()
	verifyDelay := sessionCfg.StartupNudgeVerifyDelayD()
	maxRetries := sessionCfg.StartupNudgeMaxRetriesV()
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if !retryOneStartupNudge(m, sessionID, retryContent, attempt, maxRetries, verifyDelay) {
			return
		}
	}
	if m.tmux.IsIdle(sessionID) {
		fmt.Fprintf(os.Stderr, "[startup-nudge] WARNING: agent %s still idle after %d nudge retries\n",
			sessionID, maxRetries)
	}
}

func retryOneStartupNudge(m *SessionManager, sessionID, retryContent string, attempt, maxRetries int, verifyDelay time.Duration) bool {
	time.Sleep(verifyDelay)
	running, err := m.tmux.HasSession(sessionID)
	if err != nil || !running {
		return false
	}
	if !m.tmux.IsIdle(sessionID) {
		return false
	}
	fmt.Fprintf(os.Stderr, "[startup-nudge] attempt %d/%d: agent %s idle at prompt, retrying nudge\n",
		attempt, maxRetries, sessionID)
	if err := m.tmux.NudgeSession(sessionID, retryContent); err != nil {
		fmt.Fprintf(os.Stderr, "[startup-nudge] retry nudge failed for %s: %v\n", sessionID, err)
		return false
	}
	return true
}

// hookIssue pins an issue to a polecat's hook using bd update.
func sessionHookIssue(m *SessionManager, issueID, agentID, workDir string) error {
	bdWorkDir := sessionResolveBeadsDir(m, issueID, workDir)

	ctx, cancel := context.WithTimeout(context.Background(), constants.BdCommandTimeout)
	defer cancel()
	cmd := beads.SpawnContext(ctx, "update", issueID, "--status=hooked", "--assignee="+agentID) //nolint:gosec // G204: bd is a trusted internal tool
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = bdWorkDir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bd update failed: %w", err)
	}
	fmt.Printf("✓ Hooked issue %s to %s\n", issueID, agentID)
	return nil
}
