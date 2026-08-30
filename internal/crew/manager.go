package crew

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/jonbaldie/gastown/internal/atomicfile"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/nudge"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/util"
)

// Common errors
var (
	ErrCrewExists      = errors.New("crew worker already exists")
	ErrCrewNotFound    = errors.New("crew worker not found")
	ErrHasChanges      = errors.New("crew worker has uncommitted changes")
	ErrInvalidCrewName = errors.New("invalid crew name")
	ErrSessionRunning  = errors.New("session already running")
	ErrSessionNotFound = errors.New("session not found")
)

// StartOptions configures crew session startup.
type StartOptions struct {
	// Account specifies the account handle to use (overrides default).
	Account string

	// ClaudeConfigDir is resolved CLAUDE_CONFIG_DIR for the account.
	// If set, this is injected as an environment variable.
	ClaudeConfigDir string

	// KillExisting kills any existing session before starting (for restart operations).
	// If false and a session is running, Start() returns ErrSessionRunning.
	KillExisting bool

	// Topic is the startup nudge topic (e.g., "start", "restart", "refresh").
	// Defaults to "start" if empty.
	Topic string

	// Interactive removes --dangerously-skip-permissions for interactive/refresh mode.
	Interactive bool

	// AgentOverride specifies an alternate agent alias (e.g., for testing).
	AgentOverride string

	// ResumeSessionID resumes a previous agent session instead of starting fresh.
	// "last" means resume the most recent session (--resume with no session ID).
	// Any other non-empty value is a specific session ID to resume.
	ResumeSessionID string
}

// validateSessionID checks that a resume session ID contains only safe characters.
// Session IDs from Claude, Gemini, etc. are typically UUIDs or hex strings.
// This rejects shell metacharacters that could cause injection when the ID is
// interpolated into a shell command string.
func validateSessionID(id string) error {
	if id == "" || id == "last" {
		return nil
	}
	for _, character := range id {
		if !isSessionIDCharacter(character) {
			return fmt.Errorf("invalid session ID %q: contains character %q; session IDs may only contain alphanumeric characters, hyphens, underscores, and dots", id, string(character))
		}
	}
	return nil
}

func isSessionIDCharacter(character rune) bool {
	return strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.", character)
}

// buildResumeArgs validates the agent preset supports resume and returns the
// flag(s) to append to the command string. agentName is the resolved agent
// preset name (e.g. "claude", "gemini"). sessionID is "last" for auto-resume
// or a specific session ID.
func buildResumeArgs(agentName, sessionID string) (string, error) {
	preset := config.GetAgentPresetByName(agentName)
	if preset == nil || preset.ResumeFlag == "" {
		return "", fmt.Errorf("agent %q does not support session resume", agentName)
	}
	if preset.ResumeStyle == "subcommand" {
		return "", fmt.Errorf("--resume not yet supported for subcommand-style agents (e.g., %s); use the agent's native resume mechanism", agentName)
	}

	if sessionID == "last" {
		if preset.ContinueFlag == "" {
			return "", fmt.Errorf("agent %q does not support --resume without a session ID (no ContinueFlag configured); use --resume <session-id> instead", agentName)
		}
		return preset.ContinueFlag, nil
	}
	return preset.ResumeFlag + " " + config.ShellQuote(sessionID), nil
}

// validateCrewName checks that a crew name is safe and valid.
// Rejects path traversal attempts and characters that break agent ID parsing.
func validateCrewName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name cannot be empty", ErrInvalidCrewName)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%w: %q is not allowed", ErrInvalidCrewName, name)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: %q contains path separators", ErrInvalidCrewName, name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("%w: %q contains path traversal sequence", ErrInvalidCrewName, name)
	}
	// Reject characters that break agent ID parsing (same as rig names)
	if strings.ContainsAny(name, "-. ") {
		sanitized := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(name)
		sanitized = strings.ToLower(sanitized)
		return fmt.Errorf("%w: %q contains invalid characters; hyphens, dots, and spaces are reserved for agent ID parsing. Try %q instead", ErrInvalidCrewName, name, sanitized)
	}
	return nil
}

// validateNewCrewName checks a name that is about to be given to a crew worker.
//
// It is stricter than validateCrewName because it also refuses the names of
// infrastructure Roles. A crew worker's address normalizes to "<rig>/<name>",
// which collides with the address of a Town or Rig Role of the same name, so
// mail addressed to the worker is delivered to that Role instead, with no
// error. The stricter rule belongs here rather than in validateCrewName, so
// that a worker created before the rule existed can still be listed, stopped,
// and removed.
func validateNewCrewName(name string) error {
	if err := validateCrewName(name); err != nil {
		return err
	}
	if constants.IsReservedAgentName(name) {
		return fmt.Errorf("%w: %q is reserved for infrastructure agents", ErrInvalidCrewName, name)
	}
	return nil
}

// tmuxOps is the tmux seam for crew start and stop. Production uses *tmux.Tmux.
type tmuxOps interface {
	session.TmuxOps
	SetCrewCycleBindings(_ string) error
}

// Manager handles crew worker lifecycle.
type Manager struct {
	rig  *rig.Rig
	git  *git.Git
	tmux tmuxOps
}

// NewManager creates a new crew manager.
func NewManager(r *rig.Rig, g *git.Git) *Manager {
	return &Manager{
		rig:  r,
		git:  g,
		tmux: tmux.NewTmux(),
	}
}

// crewDir returns the directory for a crew worker.
func (m *Manager) crewDir(name string) string {
	return filepath.Join(m.rig.Path, "crew", name)
}

// stateFile returns the state file path for a crew worker.
func (m *Manager) stateFile(name string) string {
	return filepath.Join(m.crewDir(name), "state.json")
}

// exists checks if a crew worker exists.
func (m *Manager) exists(name string) bool {
	_, err := os.Stat(m.crewDir(name))
	return err == nil
}

// lockCrew acquires an exclusive file lock for a specific crew worker.
// This prevents concurrent gt processes from racing on the same crew worker's
// filesystem operations (Add, Remove, Rename, Start).
// Caller must defer fl.Unlock().
func (m *Manager) lockCrew(name string) (*flock.Flock, error) {
	lockDir := filepath.Join(m.rig.Path, ".runtime", "locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("creating lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, fmt.Sprintf("crew-%s.lock", name))
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("acquiring crew lock for %s: %w", name, err)
	}
	return fl, nil
}

// Add creates a new crew worker with a clone of the rig.
func (m *Manager) Add(name string, createBranch bool) (*CrewWorker, error) {
	if err := validateCrewName(name); err != nil {
		return nil, err
	}
	fl, err := m.lockCrew(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fl.Unlock() }()
	return m.addLocked(name, createBranch)
}

// addLocked creates a new crew worker, assumes caller holds lockCrew(name).
func (m *Manager) addLocked(name string, createBranch bool) (*CrewWorker, error) {
	if err := validateNewCrewName(name); err != nil {
		return nil, err
	}
	if m.exists(name) {
		return nil, ErrCrewExists
	}
	crewPath := m.crewDir(name)
	defaultBranch, err := prepareCrewClone(m, crewPath)
	if err != nil {
		return nil, err
	}
	branchName, err := prepareCrewBranch(crewPath, name, defaultBranch, createBranch)
	if err != nil {
		return nil, err
	}
	if err := prepareCrewWorkspace(m.rig, name, crewPath); err != nil {
		return nil, err
	}

	return saveNewCrewWorker(m, name, crewPath, branchName)
}

func prepareCrewClone(m *Manager, crewPath string) (string, error) {
	if err := os.MkdirAll(filepath.Join(m.rig.Path, "crew"), 0755); err != nil {
		return "", fmt.Errorf("creating crew dir: %w", err)
	}
	if m.rig.GitURL == "" {
		return "", fmt.Errorf("rig %q has no git URL configured — crew workspaces require a clonable repository (set git_url in rigs.json or re-add the rig with a remote URL)", m.rig.Name)
	}
	defaultBranch := m.rig.DefaultBranch()
	if err := cloneCrewRepository(m.git, m.rig, crewPath, defaultBranch); err != nil {
		return "", err
	}
	if err := m.syncRemotesFromRig(crewPath); err != nil {
		if syncErr := handleInitialRemoteSyncError(m.rig, crewPath, err); syncErr != nil {
			return "", syncErr
		}
	}
	return defaultBranch, nil
}

func cloneCrewRepository(g *git.Git, r *rig.Rig, crewPath, defaultBranch string) error {
	if r.LocalRepo == "" {
		if err := git.CloneBranch(g, r.GitURL, crewPath, defaultBranch); err == nil {
			return nil
		} else {
			style.PrintWarning("could not clone branch %s, falling back to default: %v", defaultBranch, err)
		}
		return wrapCloneError(git.Clone(g, r.GitURL, crewPath))
	}
	if err := git.CloneBranchWithReference(g, r.GitURL, crewPath, defaultBranch, r.LocalRepo); err == nil {
		return nil
	} else {
		style.PrintWarning("could not clone branch %s with reference: %v", defaultBranch, err)
	}
	if err := git.CloneBranch(g, r.GitURL, crewPath, defaultBranch); err == nil {
		return nil
	} else {
		style.PrintWarning("could not clone branch %s: %v", defaultBranch, err)
	}
	if err := git.CloneWithReference(g, r.GitURL, crewPath, r.LocalRepo); err == nil {
		return nil
	} else {
		style.PrintWarning("could not clone with reference: %v", err)
	}
	return wrapCloneError(git.Clone(g, r.GitURL, crewPath))
}

func wrapCloneError(err error) error {
	if err != nil {
		return fmt.Errorf("cloning rig: %w", err)
	}
	return nil
}

func handleInitialRemoteSyncError(r *rig.Rig, crewPath string, syncErr error) error {
	if r.PushURL == "" {
		style.PrintWarning("could not sync remotes from rig: %v", syncErr)
		return nil
	}
	if err := os.RemoveAll(crewPath); err != nil {
		style.PrintWarning("could not clean up orphaned clone at %s: %v", crewPath, err)
	}
	return fmt.Errorf("syncing remotes from rig (push URL required): %w", syncErr)
}

func prepareCrewBranch(crewPath, name, defaultBranch string, create bool) (string, error) {
	if !create {
		return defaultBranch, nil
	}
	branchName := fmt.Sprintf("crew/%s", name)
	crewGit := git.NewGit(crewPath)
	if err := git.CreateBranch(crewGit, branchName); err != nil {
		_ = os.RemoveAll(crewPath)
		return "", fmt.Errorf("creating branch: %w", err)
	}
	if err := git.Checkout(crewGit, branchName); err != nil {
		_ = os.RemoveAll(crewPath)
		return "", fmt.Errorf("checking out branch: %w", err)
	}
	return branchName, nil
}

func prepareCrewWorkspace(r *rig.Rig, name, crewPath string) error {
	if err := os.MkdirAll(filepath.Join(crewPath, "mail"), 0755); err != nil {
		_ = os.RemoveAll(crewPath)
		return fmt.Errorf("creating mail dir: %w", err)
	}
	if err := rig.Provision(r.Path, crewPath, "crew"); err != nil {
		style.PrintWarning("could not provision crew workspace: %v", err)
	}
	townRoot := filepath.Dir(r.Path)
	runtimeConfig := config.ResolveWorkerAgentConfig(name, townRoot, r.Path)
	if err := runtime.EnsureSettingsForRole(config.RoleSettingsDir("crew", r.Path), crewPath, "crew", runtimeConfig); err != nil {
		style.PrintWarning("could not install runtime settings: %v", err)
	}
	return nil
}

func saveNewCrewWorker(m *Manager, name, crewPath, branchName string) (*CrewWorker, error) {
	now := time.Now()
	crew := &CrewWorker{Name: name, Rig: m.rig.Name, ClonePath: crewPath, Branch: branchName, CreatedAt: now, UpdatedAt: now}
	if err := m.saveState(crew); err != nil {
		_ = os.RemoveAll(crewPath)
		return nil, fmt.Errorf("saving state: %w", err)
	}
	return crew, nil
}

// syncRemotesFromRig copies remote configuration from the mayor/rig repo to a crew clone.
// This ensures crew clones have the same origin (fork) and upstream as the rig,
// preventing repo ID mismatches and broken formula slinging.
func (m *Manager) syncRemotesFromRig(crewPath string) error {
	rigRepoPath := filepath.Join(m.rig.Path, "mayor", "rig")
	if _, err := os.Stat(rigRepoPath); err != nil {
		return fmt.Errorf("mayor/rig not found at %s", rigRepoPath)
	}
	rigGit := git.NewGit(rigRepoPath)
	crewGit := git.NewGit(crewPath)
	remotes, err := git.Remotes(rigGit)
	if err != nil {
		return fmt.Errorf("reading rig remotes: %w", err)
	}
	for _, remote := range remotes {
		if err := syncCrewRemote(m.rig, rigGit, crewGit, remote); err != nil {
			return err
		}
	}
	return nil
}

func syncCrewRemote(r *rig.Rig, rigGit, crewGit *git.Git, remote string) error {
	if remote == "" || remote == "mayor" {
		return nil
	}
	fetchURL, err := git.RemoteURL(rigGit, remote)
	if err != nil {
		return nil
	}
	syncCrewFetchURL(crewGit, remote, fetchURL)
	if remote == "origin" {
		return syncOriginPushURL(r, crewGit, remote)
	}
	syncSecondaryPushURL(rigGit, crewGit, remote, fetchURL)
	return nil
}

func syncCrewFetchURL(crewGit *git.Git, remote, fetchURL string) {
	existingURL, err := git.RemoteURL(crewGit, remote)
	if err != nil {
		if _, err := git.AddRemote(crewGit, remote, fetchURL); err != nil {
			style.PrintWarning("could not add remote %s: %v", remote, err)
		}
		return
	}
	if existingURL != fetchURL {
		if _, err := git.SetRemoteURL(crewGit, remote, fetchURL); err != nil {
			style.PrintWarning("could not update remote %s: %v", remote, err)
		}
	}
}

func syncOriginPushURL(r *rig.Rig, crewGit *git.Git, remote string) error {
	pushURL := strings.TrimSpace(r.PushURL)
	if pushURL == "" {
		clearStalePushURL(crewGit, remote)
		return nil
	}
	if err := git.ConfigurePushURL(crewGit, remote, pushURL); err != nil {
		return fmt.Errorf("syncing origin push URL: %w", err)
	}
	return nil
}

func syncSecondaryPushURL(rigGit, crewGit *git.Git, remote, fetchURL string) {
	pushURL, err := git.GetPushURL(rigGit, remote)
	if err != nil {
		style.PrintWarning("could not read push URL for %s, skipping: %v", remote, err)
		return
	}
	if pushURL == "" || pushURL == fetchURL {
		clearStalePushURL(crewGit, remote)
		return
	}
	if err := git.ConfigurePushURL(crewGit, remote, pushURL); err != nil {
		style.PrintWarning("could not sync push URL for %s: %v", remote, err)
	}
}

func clearStalePushURL(crewGit *git.Git, remote string) {
	pushURL, pushErr := git.GetPushURL(crewGit, remote)
	fetchURL, fetchErr := git.RemoteURL(crewGit, remote)
	if pushErr != nil || fetchErr != nil {
		style.PrintWarning("could not read crew remote URLs for %s, skipping cleanup: push=%v fetch=%v", remote, pushErr, fetchErr)
		return
	}
	if pushURL == fetchURL {
		return
	}
	style.PrintWarning("clearing stale push URL for %s (was: %s)", remote, util.RedactURL(pushURL))
	if err := git.ClearPushURL(crewGit, remote); err != nil {
		style.PrintWarning("could not clear push URL for %s: %v", remote, err)
	}
}

// Remove deletes a crew worker.
func (m *Manager) Remove(name string, force bool) error {
	return removeCrew(m, name, force)
}

func removeCrew(m *Manager, name string, force bool) error {
	if err := validateCrewName(name); err != nil {
		return err
	}
	fl, err := m.lockCrew(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()
	if !m.exists(name) {
		return ErrCrewNotFound
	}

	crewPath := m.crewDir(name)

	if !force {
		crewGit := git.NewGit(crewPath)
		hasChanges, err := git.HasUncommittedChanges(crewGit)
		if err == nil && hasChanges {
			return ErrHasChanges
		}
	}

	// Remove directory
	if err := os.RemoveAll(crewPath); err != nil {
		return fmt.Errorf("removing crew dir: %w", err)
	}

	return nil
}

// List returns all crew workers in the rig.
func (m *Manager) List() ([]*CrewWorker, error) {
	return listCrew(m)
}

func listCrew(m *Manager) ([]*CrewWorker, error) {
	crewBaseDir := filepath.Join(m.rig.Path, "crew")

	entries, err := os.ReadDir(crewBaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading crew dir: %w", err)
	}

	var workers []*CrewWorker
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		worker, err := m.Get(entry.Name())
		if err != nil {
			continue // Skip invalid workers
		}
		workers = append(workers, worker)
	}

	return workers, nil
}

// Get returns a specific crew worker by name.
func (m *Manager) Get(name string) (*CrewWorker, error) {
	if err := validateCrewName(name); err != nil {
		return nil, err
	}
	fl, err := m.lockCrew(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fl.Unlock() }()
	return m.getLocked(name)
}

// getLocked returns a crew worker, assumes caller holds lockCrew(name).
func (m *Manager) getLocked(name string) (*CrewWorker, error) {
	if !m.exists(name) {
		return nil, ErrCrewNotFound
	}

	return m.loadState(name)
}

// saveState persists crew worker state to disk using atomic write.
func (m *Manager) saveState(crew *CrewWorker) error {
	stateFile := m.stateFile(crew.Name)
	if err := atomicfile.WriteJSON(stateFile, crew); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}

	return nil
}

// loadState reads crew worker state from disk.
func (m *Manager) loadState(name string) (*CrewWorker, error) {
	return loadCrewState(m, name)
}

func loadCrewState(m *Manager, name string) (*CrewWorker, error) {
	stateFile := m.stateFile(name)

	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Return minimal crew worker if state file missing
			return &CrewWorker{
				Name:      name,
				Rig:       m.rig.Name,
				ClonePath: m.crewDir(name),
			}, nil
		}
		return nil, fmt.Errorf("reading state: %w", err)
	}

	var crew CrewWorker
	if err := json.Unmarshal(data, &crew); err != nil {
		return nil, fmt.Errorf("parsing state: %w", err)
	}

	// Directory name is source of truth for Name and ClonePath.
	// state.json can become stale after directory rename, copy, or corruption.
	crew.Name = name
	crew.ClonePath = m.crewDir(name)

	// Rig only needs backfill when empty (less likely to drift)
	if crew.Rig == "" {
		crew.Rig = m.rig.Name
	}

	return &crew, nil
}

// Rename renames a crew worker from oldName to newName.
func (m *Manager) Rename(oldName, newName string) error {
	if err := validateNewCrewName(newName); err != nil {
		return err
	}
	firstLock, secondLock, err := lockCrewPair(m, oldName, newName)
	if err != nil {
		return err
	}
	defer func() { _ = firstLock.Unlock() }()
	defer func() { _ = secondLock.Unlock() }()
	return renameCrewLocked(m, oldName, newName)
}

func lockCrewPair(m *Manager, oldName, newName string) (*flock.Flock, *flock.Flock, error) {
	first, second := oldName, newName
	if first > second {
		first, second = second, first
	}
	firstLock, err := m.lockCrew(first)
	if err != nil {
		return nil, nil, err
	}
	secondLock, err := m.lockCrew(second)
	if err != nil {
		_ = firstLock.Unlock()
		return nil, nil, err
	}
	return firstLock, secondLock, nil
}

func renameCrewLocked(m *Manager, oldName, newName string) error {
	if !m.exists(oldName) {
		return ErrCrewNotFound
	}
	if m.exists(newName) {
		return ErrCrewExists
	}

	oldPath := m.crewDir(oldName)
	newPath := m.crewDir(newName)

	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("renaming crew dir: %w", err)
	}
	crew, err := m.loadState(newName)
	if err != nil {
		_ = os.Rename(newPath, oldPath)
		return fmt.Errorf("loading state: %w", err)
	}

	crew.Name = newName
	crew.ClonePath = newPath
	crew.UpdatedAt = time.Now()

	if err := m.saveState(crew); err != nil {
		_ = os.Rename(newPath, oldPath)
		return fmt.Errorf("saving state: %w", err)
	}

	return nil
}

// Pristine ensures a crew worker is up-to-date with remote.
// It runs git pull --rebase.
func (m *Manager) Pristine(name string) (*PristineResult, error) {
	return pristineCrew(m, name)
}

func pristineCrew(m *Manager, name string) (*PristineResult, error) {
	if err := validateCrewName(name); err != nil {
		return nil, err
	}
	if !m.exists(name) {
		return nil, ErrCrewNotFound
	}

	crewPath := m.crewDir(name)
	crewGit := git.NewGit(crewPath)

	result := &PristineResult{
		Name: name,
	}

	// Check for uncommitted changes
	hasChanges, err := git.HasUncommittedChanges(crewGit)
	if err != nil {
		return nil, fmt.Errorf("checking changes: %w", err)
	}
	result.HadChanges = hasChanges

	// Pull latest (use origin and current branch)
	if err := git.Pull(crewGit, "origin", ""); err != nil {
		result.PullError = err.Error()
	} else {
		result.Pulled = true
	}

	// Note: With Dolt backend, beads changes are persisted immediately - no sync needed
	result.Synced = true

	return result, nil
}

// PristineResult captures the results of a pristine operation.
type PristineResult struct {
	Name       string `json:"name"`
	HadChanges bool   `json:"had_changes"`
	Pulled     bool   `json:"pulled"`
	PullError  string `json:"pull_error,omitempty"`
	Synced     bool   `json:"synced"`
	SyncError  string `json:"sync_error,omitempty"`
}

// SessionName returns the tmux session name for a crew member.
func (m *Manager) SessionName(name string) string {
	return session.CrewSessionName(session.PrefixFor(m.rig.Name), name)
}

// Start creates and starts a tmux session for a crew member.
// If the crew member doesn't exist, it will be created first.
func (m *Manager) Start(name string, opts StartOptions) error {
	if err := validateCrewName(name); err != nil {
		return err
	}
	fl, err := m.lockCrew(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()
	worker, err := prepareCrewStart(m, name)
	if err != nil {
		return err
	}
	townRoot := filepath.Dir(m.rig.Path)
	command, err := buildCrewStartupCommand(m, name, townRoot, opts)
	if err != nil {
		return err
	}
	sessionID := m.SessionName(name)
	if err := prepareCrewSession(m.tmux, townRoot, sessionID, opts.KillExisting); err != nil {
		return err
	}
	if opts.Interactive {
		command = strings.Replace(command, " --dangerously-skip-permissions", "", 1)
	}
	if err := startCrewSession(m, worker, name, townRoot, sessionID, command, opts); err != nil {
		return err
	}
	finishCrewSessionStart(m.tmux, townRoot, sessionID, name, opts.Interactive)
	return nil
}

func finishCrewSessionStart(t tmuxOps, townRoot, sessionID, name string, interactive bool) {
	_ = t.SetCrewCycleBindings(sessionID)
	if interactive {
		return
	}
	if _, err := nudge.StartPoller(townRoot, sessionID); err != nil {
		style.PrintWarning("could not start nudge poller for %s: %v", name, err)
	}
}

func prepareCrewStart(m *Manager, name string) (*CrewWorker, error) {
	worker, err := m.getLocked(name)
	if err == ErrCrewNotFound {
		worker, err = m.addLocked(name, false)
		if err != nil {
			return nil, fmt.Errorf("creating crew workspace: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("getting crew worker: %w", err)
	}
	if err := m.syncRemotesFromRig(worker.ClonePath); err != nil && m.rig.PushURL != "" {
		style.PrintWarning("could not sync remotes to crew %s: %v", name, err)
	}
	townRoot := filepath.Dir(m.rig.Path)
	runtimeConfig := config.ResolveWorkerAgentConfig(name, townRoot, m.rig.Path)
	if err := runtime.EnsureSettingsForRole(config.RoleSettingsDir("crew", m.rig.Path), worker.ClonePath, "crew", runtimeConfig); err != nil {
		return nil, fmt.Errorf("ensuring runtime settings: %w", err)
	}
	return worker, nil
}

func buildCrewStartupCommand(m *Manager, name, townRoot string, opts StartOptions) (string, error) {
	if opts.ResumeSessionID != "" {
		return buildCrewResumeCommand(m, name, townRoot, opts)
	}
	topic := crewStartTopic(opts.Topic)
	beacon := session.FormatStartupBeacon(session.BeaconConfig{Recipient: session.BeaconRecipient("crew", name, m.rig.Name), Sender: "human", Topic: topic})
	command, err := config.BuildStartupCommandFromConfig(config.AgentEnvConfig{
		Role: "crew", Rig: m.rig.Name, AgentName: name, TownRoot: townRoot, Prompt: beacon,
		Topic: topic, SessionName: m.SessionName(name),
	}, m.rig.Path, beacon, opts.AgentOverride)
	if err != nil {
		return "", fmt.Errorf("building startup command: %w", err)
	}
	return command, nil
}

func buildCrewResumeCommand(m *Manager, name, townRoot string, opts StartOptions) (string, error) {
	if err := validateSessionID(opts.ResumeSessionID); err != nil {
		return "", err
	}
	command, err := config.BuildCrewStartupCommandWithAgentOverride(m.rig.Name, name, m.rig.Path, "", opts.AgentOverride)
	if err != nil {
		return "", fmt.Errorf("building resume command: %w", err)
	}
	agentName := opts.AgentOverride
	if agentName == "" {
		agentName = resolvedCrewAgentName(name, townRoot, m.rig.Path)
	}
	resumeArgs, err := buildResumeArgs(agentName, opts.ResumeSessionID)
	if err != nil {
		return "", err
	}
	return command + " " + resumeArgs, nil
}

func resolvedCrewAgentName(name, townRoot, rigPath string) string {
	if runtimeConfig := config.ResolveWorkerAgentConfig(name, townRoot, rigPath); runtimeConfig != nil && runtimeConfig.Provider != "" {
		return runtimeConfig.Provider
	}
	return "claude"
}

func crewStartTopic(topic string) string {
	if topic == "" {
		return "start"
	}
	return topic
}

func prepareCrewSession(t tmuxOps, townRoot, sessionID string, killExisting bool) error {
	_, err := session.KillExistingSession(t, townRoot, sessionID, !killExisting)
	if errors.Is(err, session.ErrSessionAlive) {
		return fmt.Errorf("%w: %s", ErrSessionRunning, sessionID)
	}
	return err
}

func crewSessionTheme(townRoot, rigName, name string) *tmux.Theme {
	theme := tmux.ResolveSessionTheme(townRoot, rigName, "crew", name)
	if theme == nil {
		return nil
	}
	theme.Window = session.ResolveWindowTint(rigName, "crew")
	if theme.Window == nil && session.IsWindowTintEnabled(rigName) {
		theme.Window = &tmux.WindowStyle{BG: tmux.DarkenColor(theme.BG, session.ResolveTintFactor(rigName)), FG: theme.FG}
	}
	return theme
}

func startCrewSession(m *Manager, worker *CrewWorker, name, townRoot, sessionID, command string, opts StartOptions) error {
	topic := crewStartTopic(opts.Topic)
	_, err := session.StartSession(m.tmux, "crew", session.Work{
		SessionID: sessionID, WorkDir: worker.ClonePath, TownRoot: townRoot, RigPath: m.rig.Path,
		RigName: m.rig.Name, AgentName: name, Command: command, AgentOverride: opts.AgentOverride,
		RuntimeConfigDir: opts.ClaudeConfigDir, Theme: crewSessionTheme(townRoot, m.rig.Name, name),
		Beacon:      session.BeaconConfig{Recipient: session.BeaconRecipient("crew", name, m.rig.Name), Sender: "human", Topic: topic},
		Interactive: opts.Interactive,
	})
	return err
}

// Stop terminates a crew member's tmux session.
func (m *Manager) Stop(name string) error {
	if err := validateCrewName(name); err != nil {
		return err
	}

	err := session.StopSession(m.tmux, filepath.Dir(m.rig.Path), m.SessionName(name), false)
	if errors.Is(err, session.ErrNotFound) {
		return ErrSessionNotFound
	}
	return err
}

// IsRunning checks if a crew member's session is active.
func (m *Manager) IsRunning(name string) (bool, error) {
	t := m.tmux
	sessionID := m.SessionName(name)
	return t.HasSession(sessionID)
}
