package dog

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
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
)

// Common errors
var (
	ErrDogExists   = errors.New("dog already exists")
	ErrDogNotFound = errors.New("dog not found")
	ErrDogWorking  = errors.New("dog is currently working")
	ErrNoRigs      = errors.New("no rigs configured")
	ErrInvalidName = errors.New("invalid dog name")
)

// Manager handles dog lifecycle in the kennel.
type Manager struct {
	townRoot   string
	kennelPath string // ~/gt/deacon/dogs/
	rigsConfig *config.RigsConfig
	*lifecycleManager
	*stateManager
	*snapshotManager
	*refreshManager
}

type lifecycleManager struct{ *Manager }
type stateManager struct{ *Manager }
type snapshotManager struct{ *Manager }
type refreshManager struct{ *Manager }

// NewManager creates a new dog manager.
func NewManager(townRoot string, rigsConfig *config.RigsConfig) *Manager {
	m := &Manager{
		townRoot:   townRoot,
		kennelPath: filepath.Join(townRoot, "deacon", "dogs"),
		rigsConfig: rigsConfig,
	}
	m.lifecycleManager = &lifecycleManager{Manager: m}
	m.stateManager = &stateManager{Manager: m}
	m.snapshotManager = &snapshotManager{Manager: m}
	m.refreshManager = &refreshManager{Manager: m}
	return m
}

// lockDog acquires an exclusive file lock for a specific dog's state operations.
// This prevents concurrent load-modify-save races on .dog.json.
// Caller must defer fl.Unlock().
func (m *Manager) lockDog(name string) (*flock.Flock, error) {
	// Keep lock files outside removable dog directories. A dog may be removed
	// and recreated while callers wait; one stable inode preserves exclusion.
	lockDir := filepath.Join(m.kennelPath, ".locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("creating dog lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, name+".lock")
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("acquiring dog lock for %s: %w", name, err)
	}
	return fl, nil
}

func (m *Manager) lockState(name string) (*flock.Flock, *DogState, error) {
	if err := validateDogName(name); err != nil {
		return nil, nil, err
	}
	if !m.exists(name) {
		return nil, nil, ErrDogNotFound
	}
	fl, err := m.lockDog(name)
	if err != nil {
		return nil, nil, err
	}
	state, err := m.loadState(name)
	if err != nil {
		_ = fl.Unlock()
		return nil, nil, fmt.Errorf("loading state: %w", err)
	}
	return fl, state, nil
}

// validateDogName checks that a dog name is safe for use as a directory name.
func validateDogName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name cannot be empty", ErrInvalidName)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: name cannot contain path separators", ErrInvalidName)
	}
	if name == "." || name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("%w: name cannot contain path traversal", ErrInvalidName)
	}
	return nil
}

// dogDir returns the directory for a dog.
func (m *Manager) dogDir(name string) string {
	return filepath.Join(m.kennelPath, name)
}

// exists checks if a dog exists.
func (m *Manager) exists(name string) bool {
	_, err := os.Stat(m.dogDir(name))
	return err == nil
}

// stateFilePath returns the path to a dog's state file.
func (m *Manager) stateFilePath(name string) string {
	return filepath.Join(m.dogDir(name), ".dog.json")
}

// Add creates a new dog in the kennel with worktrees into each rig.
// Each dog gets a worktree per rig (e.g., dogs/alpha/gastown/, dogs/alpha/beads/).
// Worktrees are created from each rig's bare repo (.repo.git) or mayor/rig.
func (m *lifecycleManager) Add(name string) (*Dog, error) {
	core := m.Manager
	dogPath, err := m.prepareDogDirectory(name)
	if err != nil {
		return nil, err
	}

	// Track cleanup on failure
	cleanup := func() { _ = os.RemoveAll(dogPath) }
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()

	// Create worktrees into each rig
	worktrees, err := m.createDogWorktrees(dogPath, name)
	if err != nil {
		return nil, err
	}

	// Create initial state file
	now := time.Now()
	state := &DogState{
		Name:       name,
		State:      StateIdle,
		LastActive: now,
		Worktrees:  worktrees,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := core.saveState(name, state); err != nil {
		return nil, fmt.Errorf("saving state: %w", err)
	}

	success = true
	return &Dog{
		Name:       name,
		State:      StateIdle,
		Path:       dogPath,
		Worktrees:  worktrees,
		LastActive: now,
		CreatedAt:  now,
	}, nil
}

func (m *lifecycleManager) prepareDogDirectory(name string) (string, error) {
	if err := validateDogName(name); err != nil {
		return "", err
	}
	if m.exists(name) {
		return "", ErrDogExists
	}
	if len(m.rigsConfig.Rigs) == 0 {
		return "", ErrNoRigs
	}
	if err := os.MkdirAll(m.kennelPath, 0755); err != nil {
		return "", fmt.Errorf("creating kennel dir: %w", err)
	}
	dogPath := m.dogDir(name)
	if err := os.MkdirAll(dogPath, 0755); err != nil {
		return "", fmt.Errorf("creating dog dir: %w", err)
	}
	return dogPath, nil
}

func (m *lifecycleManager) createDogWorktrees(dogPath, name string) (map[string]string, error) {
	worktrees := make(map[string]string)
	for rigName := range m.rigsConfig.Rigs {
		worktreePath, err := createRigWorktree(m.townRoot, dogPath, name, rigName)
		if err != nil {
			return nil, fmt.Errorf("creating worktree for rig %s: %w", rigName, err)
		}
		worktrees[rigName] = worktreePath
	}
	return worktrees, nil
}

// createRigWorktree creates a worktree for a dog into a specific rig.
// Uses the rig's bare repo (.repo.git) if available, otherwise mayor/rig.
// Branch naming: dog/<dog-name>-<rig>-<timestamp> for uniqueness.
func createRigWorktree(townRoot, dogPath, dogName, rigName string) (string, error) {
	rigPath := filepath.Join(townRoot, rigName)
	worktreePath := filepath.Join(dogPath, rigName)

	// Find the repo base (bare repo or mayor/rig)
	repoGit, err := findRepoBase(rigPath)
	if err != nil {
		return "", fmt.Errorf("finding repo base for %s: %w", rigName, err)
	}

	// Determine the start point for the new worktree
	// Use origin/<default-branch> to ensure we start from the rig's configured branch
	defaultBranch := "main"
	if rigCfg, err := rig.LoadRigConfig(rigPath); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	startPoint := fmt.Sprintf("origin/%s", defaultBranch)

	// Unique branch per dog-rig combination
	branchName := fmt.Sprintf("dog/%s-%s-%d", dogName, rigName, time.Now().UnixMilli())

	// Create worktree with new branch from default branch
	if err := git.WorktreeAddFromRef(repoGit, worktreePath, branchName, startPoint); err != nil {
		return "", fmt.Errorf("creating worktree from %s: %w", startPoint, err)
	}

	return worktreePath, nil
}

// findRepoBase locates the git repo base for a rig.
// Prefers .repo.git (bare repo), falls back to mayor/rig.
func findRepoBase(rigPath string) (*git.Git, error) {
	// Check for shared bare repo
	bareRepoPath := filepath.Join(rigPath, ".repo.git")
	if info, err := os.Stat(bareRepoPath); err == nil && info.IsDir() {
		return git.NewGitWithDir(bareRepoPath, ""), nil
	}

	// Fall back to mayor/rig
	mayorPath := filepath.Join(rigPath, "mayor", "rig")
	if _, err := os.Stat(mayorPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("no repo base found (neither .repo.git nor mayor/rig exists)")
	}
	return git.NewGit(mayorPath), nil
}

// Remove deletes a dog from the kennel.
// Removes all worktrees and the dog directory.
func (m *lifecycleManager) Remove(name string) error {
	if err := validateDogName(name); err != nil {
		return err
	}
	if !m.exists(name) {
		return ErrDogNotFound
	}

	dogPath := m.dogDir(name)

	// Load state to get worktree paths
	state, err := m.loadState(name)
	if err != nil {
		// State file may be missing, proceed with cleanup
		state = &DogState{Worktrees: make(map[string]string)}
	}

	// Remove worktrees from each rig
	for rigName, worktreePath := range state.Worktrees {
		rigPath := filepath.Join(m.townRoot, rigName)
		repoGit, err := findRepoBase(rigPath)
		if err != nil {
			// Log but continue with other rigs
			style.PrintWarning("could not find repo base for %s: %v", rigName, err)
			continue
		}

		// Try to remove worktree properly
		if err := git.WorktreeRemove(repoGit, worktreePath, true); err != nil {
			// Log but continue - will remove directory below
			style.PrintWarning("could not remove worktree %s: %v", worktreePath, err)
		}

		// Prune stale entries
		_ = git.WorktreePrune(repoGit)
	}

	// Remove dog directory
	if err := os.RemoveAll(dogPath); err != nil {
		return fmt.Errorf("removing dog dir: %w", err)
	}

	return nil
}

// List returns all dogs in the kennel.
func (m *lifecycleManager) List() ([]*Dog, error) {
	core := m.Manager
	entries, err := os.ReadDir(core.kennelPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading kennel: %w", err)
	}

	var dogs []*Dog
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dog, err := m.Get(entry.Name())
		if err != nil {
			continue // Skip invalid dogs
		}
		dogs = append(dogs, dog)
	}

	return dogs, nil
}

// Get returns a specific dog by name.
// Returns ErrDogNotFound if the dog directory or .dog.json state file doesn't exist.
func (m *lifecycleManager) Get(name string) (*Dog, error) {
	core := m.Manager
	if err := validateDogName(name); err != nil {
		return nil, err
	}
	if !core.exists(name) {
		return nil, ErrDogNotFound
	}

	state, err := m.loadState(name)
	if err != nil {
		// No .dog.json means this isn't a valid dog worker
		// (e.g., "boot" is the boot watchdog using .boot-status.json, not a dog)
		return nil, ErrDogNotFound
	}

	return &Dog{
		Name:          name,
		State:         state.State,
		Path:          m.dogDir(name),
		Worktrees:     state.Worktrees,
		LastActive:    state.LastActive,
		Work:          state.Work,
		WorkKind:      state.WorkKind,
		WorkSourceID:  state.WorkSourceID,
		WorkStartedAt: state.WorkStartedAt,
		CreatedAt:     state.CreatedAt,
	}, nil
}

// SetState updates a dog's state and last-active timestamp.
func (m *stateManager) SetState(name string, state State) error {
	core := m.Manager
	if err := validateDogName(name); err != nil {
		return err
	}
	if !core.exists(name) {
		return ErrDogNotFound
	}

	// Acquire per-dog lock to prevent concurrent load-modify-save races
	fl, err := m.lockDog(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()

	dogState, err := m.loadState(name)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	dogState.State = state
	dogState.LastActive = time.Now()
	dogState.UpdatedAt = time.Now()

	return m.saveState(name, dogState)
}

// AssignWork assigns untyped legacy work to a dog and sets it to working state.
func (m *stateManager) AssignWork(name, work string) error {
	return m.AssignWorkWithKind(name, work, "")
}

// AssignWorkWithKind assigns typed work to a dog and sets it to working state.
func (m *stateManager) AssignWorkWithKind(name, work string, kind WorkKind) error {
	core := m.Manager
	if err := validateDogName(name); err != nil {
		return err
	}
	if !core.exists(name) {
		return ErrDogNotFound
	}

	// Acquire per-dog lock to prevent concurrent load-modify-save races
	fl, err := m.lockDog(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	state.State = StateWorking
	state.Work = work
	state.WorkKind = kind
	applyWorkSourceID(state, work, kind)
	state.WorkStartedAt = time.Now()
	state.LastActive = time.Now()
	state.UpdatedAt = time.Now()

	return m.saveState(name, state)
}

func applyWorkSourceID(state *DogState, work string, kind WorkKind) {
	if kind == WorkKindBead {
		state.WorkSourceID = work
		return
	}
	state.WorkSourceID = ""
}

// AssignWorkIfIdle assigns work only if the dog is still idle, returning the
// saved state so callers can later perform exact compare-and-clear cleanup.
func (m *stateManager) AssignWorkIfIdle(name, work string) (*DogState, error) {
	return m.AssignWorkIfIdleWithKind(name, work, "")
}

// AssignWorkIfIdleWithKind records whether work is a source bead or formula.
func (m *stateManager) AssignWorkIfIdleWithKind(name, work string, kind WorkKind) (*DogState, error) {
	core := m.Manager
	if err := validateDogName(name); err != nil {
		return nil, err
	}
	if !core.exists(name) {
		return nil, ErrDogNotFound
	}

	fl, err := m.lockDog(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}
	if state.State != StateIdle {
		return nil, ErrDogWorking
	}

	state.State = StateWorking
	state.Work = work
	state.WorkKind = kind
	applyWorkSourceID(state, work, kind)
	state.WorkStartedAt = time.Now()
	state.LastActive = time.Now()
	state.UpdatedAt = time.Now()

	if err := m.saveState(name, state); err != nil {
		return nil, err
	}
	return state, nil
}

// SetWorkSourceIfMatches records the exact source bead ID for formula work
// after the source is hooked. Bead dispatches already store the source ID in
// Work at assignment time.
func (m *stateManager) SetWorkSourceIfMatches(name, expectedWork string, expectedStartedAt time.Time, sourceID string) (bool, error) {
	core := m.Manager
	if err := validateDogName(name); err != nil {
		return false, err
	}
	if !core.exists(name) {
		return false, ErrDogNotFound
	}

	fl, err := m.lockDog(name)
	if err != nil {
		return false, err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}
	if state.Work != expectedWork || !state.WorkStartedAt.Equal(expectedStartedAt) {
		return false, nil
	}

	state.WorkSourceID = sourceID
	state.UpdatedAt = time.Now()
	if err := m.saveState(name, state); err != nil {
		return false, err
	}
	return true, nil
}

// ClearWork clears a dog's work assignment and sets it to idle.
func (m *stateManager) ClearWork(name string) error {
	core := m.Manager
	if err := validateDogName(name); err != nil {
		return err
	}
	if !core.exists(name) {
		return ErrDogNotFound
	}

	// Acquire per-dog lock to prevent concurrent load-modify-save races
	fl, err := m.lockDog(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	clearWorkFields(state)

	return m.saveState(name, state)
}

func clearWorkFields(state *DogState) {
	state.State = StateIdle
	state.Work = ""
	state.WorkKind = ""
	state.WorkSourceID = ""
	state.WorkStartedAt = time.Time{}
	state.LastActive = time.Now()
	state.UpdatedAt = time.Now()
}

// ClearWorkIfMatches clears a dog's work assignment only if it still matches
// the expected work and assignment timestamp. The compare-and-clear happens
// under the dog lock so failed dispatch cleanup cannot erase a newer assignment.
func (m *snapshotManager) ClearWorkIfMatches(name, expectedWork string, expectedStartedAt time.Time) (bool, error) {
	core := m.Manager
	if err := validateDogName(name); err != nil {
		return false, err
	}
	if !core.exists(name) {
		return false, ErrDogNotFound
	}

	fl, err := m.lockDog(name)
	if err != nil {
		return false, err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}
	if state.Work != expectedWork || !state.WorkStartedAt.Equal(expectedStartedAt) {
		return false, nil
	}

	clearWorkFields(state)

	return true, m.saveState(name, state)
}

// ClearWorkIfMatchesAfter runs beforeClear and clears the matching assignment
// while holding the dog lock. This lets callers update external authoritative
// state before making the dog idle, without a concurrent reassignment racing
// between the two operations. If beforeClear returns false, state is preserved.
func (m *snapshotManager) ClearWorkIfMatchesAfter(name, expectedWork string, expectedStartedAt time.Time, beforeClear func() bool) (bool, error) {
	core := m.Manager
	if err := validateDogName(name); err != nil {
		return false, err
	}
	if !core.exists(name) {
		return false, ErrDogNotFound
	}

	fl, err := m.lockDog(name)
	if err != nil {
		return false, err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}
	if state.Work != expectedWork || !state.WorkStartedAt.Equal(expectedStartedAt) {
		return false, nil
	}
	if beforeClear != nil && !beforeClear() {
		return false, nil
	}

	clearWorkFields(state)

	return true, m.saveState(name, state)
}

// RemoveIfMatches removes a dog only if its assignment still matches the
// caller's snapshot. Idle dogs are matched by empty work and a zero timestamp.
func (m *snapshotManager) RemoveIfMatches(name, expectedWork string, expectedStartedAt time.Time) (bool, error) {
	return m.RemoveIfMatchesAfter(name, expectedWork, expectedStartedAt, nil)
}

// RemoveIfMatchesAfter runs beforeRemove and removes the dog while holding its
// assignment lock. Dispatch cannot assign work between session teardown and
// kennel removal.
func (m *snapshotManager) RemoveIfMatchesAfter(name, expectedWork string, expectedStartedAt time.Time, beforeRemove func() error) (bool, error) {
	return m.RemoveIfSnapshotMatchesAfter(name, expectedWork, expectedStartedAt, time.Time{}, beforeRemove)
}

// RemoveIfSnapshotMatchesAfter also checks LastActive when expectedLastActive
// is non-zero, preventing a stale idle snapshot from matching a dog that was
// assigned and completed in the meantime.
func (m *snapshotManager) RemoveIfSnapshotMatchesAfter(name, expectedWork string, expectedStartedAt, expectedLastActive time.Time, beforeRemove func() error) (bool, error) {
	core := m.Manager
	fl, state, err := core.lockState(name)
	if err != nil {
		return false, err
	}
	defer func() { _ = fl.Unlock() }()
	if !snapshotMatches(state, expectedWork, expectedStartedAt, expectedLastActive) {
		return false, nil
	}
	if err := runBeforeRemove(beforeRemove); err != nil {
		return false, err
	}
	m.removeRegisteredWorktrees(state)
	if err := os.RemoveAll(m.dogDir(name)); err != nil {
		return false, fmt.Errorf("removing dog directory: %w", err)
	}
	return true, nil
}

func snapshotMatches(state *DogState, expectedWork string, expectedStartedAt, expectedLastActive time.Time) bool {
	return state.Work == expectedWork && state.WorkStartedAt.Equal(expectedStartedAt) &&
		(expectedLastActive.IsZero() || state.LastActive.Equal(expectedLastActive))
}

func runBeforeRemove(beforeRemove func() error) error {
	if beforeRemove == nil {
		return nil
	}
	return beforeRemove()
}

func (m *snapshotManager) removeRegisteredWorktrees(state *DogState) {
	for rigName, worktreePath := range state.Worktrees {
		rigPath := filepath.Join(m.townRoot, rigName)
		repoGit, err := findRepoBase(rigPath)
		if err != nil {
			style.PrintWarning("could not find repo base for %s: %v", rigName, err)
			continue
		}
		if err := git.WorktreeRemove(repoGit, worktreePath, true); err != nil {
			style.PrintWarning("could not remove worktree %s: %v", worktreePath, err)
		}
		_ = git.WorktreePrune(repoGit)
	}
}

// WithWorkIfMatches runs action under the dog lock only when the assignment
// still matches the caller's snapshot.
func (m *snapshotManager) WithWorkIfMatches(name, expectedWork string, expectedStartedAt time.Time, action func() error) (bool, error) {
	return m.WithSnapshotIfMatches(name, expectedWork, expectedStartedAt, time.Time{}, action)
}

// WithSnapshotIfMatches optionally includes LastActive in snapshot matching.
func (m *snapshotManager) WithSnapshotIfMatches(name, expectedWork string, expectedStartedAt, expectedLastActive time.Time, action func() error) (bool, error) {
	core := m.Manager
	fl, state, err := core.lockState(name)
	if err != nil {
		return false, err
	}
	defer func() { _ = fl.Unlock() }()
	if !snapshotMatches(state, expectedWork, expectedStartedAt, expectedLastActive) {
		return false, nil
	}
	if err := runBeforeRemove(action); err != nil {
		return false, err
	}
	return true, nil
}

// Refresh recreates all worktrees for a dog with fresh branches.
// This is useful when worktrees have drifted or become stale.
// Each rig is refreshed atomically with a state save, so a failure at rig N
// leaves rigs 1..N-1 correctly updated and rigs N+1..M untouched.
func (m *refreshManager) Refresh(name string) error {
	core := m.Manager
	fl, state, err := core.lockState(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()
	if state.State == StateWorking {
		return ErrDogWorking
	}
	for rigName := range m.rigsConfig.Rigs {
		if err := m.refreshRigState(name, rigName, state, true); err != nil {
			return err
		}
	}
	return nil
}

// RefreshRig recreates the worktree for a specific rig.
func (m *refreshManager) RefreshRig(name, rigName string) error {
	if _, ok := m.rigsConfig.Rigs[rigName]; !ok {
		return fmt.Errorf("rig %s not found in config", rigName)
	}
	fl, state, err := m.lockState(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()
	if state.State == StateWorking {
		return ErrDogWorking
	}
	return m.refreshRigState(name, rigName, state, false)
}

func (m *refreshManager) refreshRigState(name, rigName string, state *DogState, saveFindFailure bool) error {
	rigPath := filepath.Join(m.townRoot, rigName)
	oldWorktreePath := state.Worktrees[rigName]
	repoGit, err := findRepoBase(rigPath)
	if err != nil {
		if saveFindFailure {
			updateDogActivity(state)
			_ = m.saveState(name, state)
		}
		return fmt.Errorf("finding repo base for %s: %w", rigName, err)
	}
	if oldWorktreePath != "" {
		_ = git.WorktreeRemove(repoGit, oldWorktreePath, true)
		_ = os.RemoveAll(oldWorktreePath)
		_ = git.WorktreePrune(repoGit)
	}
	_ = git.Fetch(repoGit, "origin")
	worktreePath, err := createRigWorktree(m.townRoot, m.dogDir(name), name, rigName)
	if err != nil {
		delete(state.Worktrees, rigName)
		state.UpdatedAt = time.Now()
		_ = m.saveState(name, state)
		return fmt.Errorf("creating worktree for %s: %w", rigName, err)
	}
	state.Worktrees[rigName] = worktreePath
	updateDogActivity(state)
	if err := m.saveState(name, state); err != nil {
		return fmt.Errorf("saving state after refreshing %s: %w", rigName, err)
	}
	return nil
}

func updateDogActivity(state *DogState) {
	now := time.Now()
	state.LastActive = now
	state.UpdatedAt = now
}

// CleanupStaleBranches removes orphaned dog branches from all rigs.
// Returns total branches deleted across all rigs.
func (m *refreshManager) CleanupStaleBranches() (int, error) {
	core := m.Manager
	totalDeleted := 0

	for rigName := range core.rigsConfig.Rigs {
		rigPath := filepath.Join(core.townRoot, rigName)
		repoGit, err := findRepoBase(rigPath)
		if err != nil {
			continue
		}

		deleted, err := m.cleanupStaleBranchesForRig(repoGit, rigName)
		if err != nil {
			style.PrintWarning("cleanup failed for rig %s: %v", rigName, err)
			continue
		}
		totalDeleted += deleted
	}

	return totalDeleted, nil
}

// cleanupStaleBranchesForRig removes orphaned dog branches in a specific rig.
func (m *refreshManager) cleanupStaleBranchesForRig(repoGit *git.Git, rigName string) (int, error) {
	// List all dog branches
	branches, err := git.ListBranches(repoGit, "dog/*")
	if err != nil {
		return 0, err
	}

	if len(branches) == 0 {
		return 0, nil
	}

	// Get list of current dogs
	dogs, err := m.List()
	if err != nil {
		return 0, err
	}

	currentBranches := currentDogBranches(dogs, rigName)
	return deleteOrphanBranches(repoGit, branches, currentBranches), nil
}

func currentDogBranches(dogs []*Dog, rigName string) map[string]bool {
	current := make(map[string]bool)
	for _, dog := range dogs {
		worktreePath, ok := dog.Worktrees[rigName]
		if !ok {
			continue
		}
		if branch, err := git.CurrentBranch(git.NewGit(worktreePath)); err == nil {
			current[branch] = true
		}
	}
	return current
}

func deleteOrphanBranches(repoGit *git.Git, branches []string, current map[string]bool) int {
	deleted := 0
	for _, branch := range branches {
		if current[branch] {
			continue
		}
		if err := git.DeleteBranch(repoGit, branch, true); err != nil {
			style.PrintWarning("could not delete branch %s: %v", branch, err)
			continue
		}
		deleted++
	}
	return deleted
}

// loadState loads a dog's state from .dog.json.
func (m *Manager) loadState(name string) (*DogState, error) {
	data, err := os.ReadFile(m.stateFilePath(name))
	if err != nil {
		return nil, err
	}

	var state DogState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	return &state, nil
}

// saveState saves a dog's state to .dog.json using atomic write (write-to-temp + rename).
// This prevents concurrent loadState from seeing a truncated/empty file.
func (m *Manager) saveState(name string, state *DogState) error {
	return atomicfile.WriteJSON(m.stateFilePath(name), state)
}

// GetIdleDog returns an idle dog suitable for work assignment.
// Returns nil if no idle dogs are available.
func (m *lifecycleManager) GetIdleDog() (*Dog, error) {
	dogs, err := m.List()
	if err != nil {
		return nil, err
	}

	for _, dog := range dogs {
		if dog.State == StateIdle {
			return dog, nil
		}
	}

	return nil, nil // No idle dogs
}

// IdleCount returns the number of idle dogs.
func (m *lifecycleManager) IdleCount() (int, error) {
	dogs, err := m.List()
	if err != nil {
		return 0, err
	}

	count := 0
	for _, dog := range dogs {
		if dog.State == StateIdle {
			count++
		}
	}
	return count, nil
}

// WorkingCount returns the number of working dogs.
func (m *lifecycleManager) WorkingCount() (int, error) {
	dogs, err := m.List()
	if err != nil {
		return 0, err
	}

	count := 0
	for _, dog := range dogs {
		if dog.State == StateWorking {
			count++
		}
	}
	return count, nil
}
