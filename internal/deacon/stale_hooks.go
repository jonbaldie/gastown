// Package deacon provides the Deacon agent infrastructure.
package deacon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// StaleHookConfig holds configurable parameters for stale hook detection.
type StaleHookConfig struct {
	// MaxAge is how long a bead can be hooked before being considered stale.
	MaxAge time.Duration `json:"max_age"`
	// DryRun if true, only reports what would be done without making changes.
	DryRun bool `json:"dry_run"`
}

// DefaultStaleHookConfig returns the default stale hook config.
func DefaultStaleHookConfig() *StaleHookConfig {
	return &StaleHookConfig{
		MaxAge: 1 * time.Hour,
		DryRun: false,
	}
}

// HookedBead represents a bead in hooked status from bd list output.
type HookedBead struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Assignee  string    `json:"assignee"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StaleHookResult represents the result of processing a stale hooked bead.
type StaleHookResult struct {
	BeadID     string `json:"bead_id"`
	Title      string `json:"title"`
	Assignee   string `json:"assignee"`
	Age        string `json:"age"`
	AgentAlive bool   `json:"agent_alive"`
	Unhooked   bool   `json:"unhooked"`
	Error      string `json:"error,omitempty"`
	// PartialWork indicates uncommitted changes or unpushed commits were found
	// in the agent's worktree before unhooking.
	PartialWork   bool   `json:"partial_work,omitempty"`
	WorktreeDirty bool   `json:"worktree_dirty,omitempty"`
	UnpushedCount int    `json:"unpushed_count,omitempty"`
	WorktreeError string `json:"worktree_error,omitempty"`
}

// StaleHookScanResult contains the full results of a stale hook scan.
type StaleHookScanResult struct {
	ScannedAt   time.Time          `json:"scanned_at"`
	TotalHooked int                `json:"total_hooked"`
	StaleCount  int                `json:"stale_count"`
	Unhooked    int                `json:"unhooked"`
	Results     []*StaleHookResult `json:"results"`
}

// ScanStaleHooks finds hooked beads with dead agents and optionally unhooks them.
// Session liveness is checked for ALL hooked beads regardless of age (gt-pqf9x).
// A hooked bead is considered stale if:
//  1. The assignee's tmux session is dead (immediate unhook), OR
//  2. The bead is older than MaxAge AND we can't determine session liveness
//     (e.g., unknown assignee format)
func ScanStaleHooks(townRoot string, cfg *StaleHookConfig) (*StaleHookScanResult, error) {
	if cfg == nil {
		cfg = DefaultStaleHookConfig()
	}

	result := &StaleHookScanResult{
		ScannedAt: time.Now().UTC(),
		Results:   make([]*StaleHookResult, 0),
	}

	// Get all hooked beads
	hookedBeads, err := listHookedBeads(townRoot)
	if err != nil {
		return nil, fmt.Errorf("listing hooked beads: %w", err)
	}

	result.TotalHooked = len(hookedBeads)

	threshold := time.Now().Add(-cfg.MaxAge)
	t := tmux.NewTmux()

	for _, bead := range hookedBeads {
		hookResult, isStale := inspectHookedBead(t, bead, threshold)
		if !isStale {
			continue
		}

		result.StaleCount++
		if processStaleHook(townRoot, bead, cfg, hookResult) {
			result.Unhooked++
		}

		result.Results = append(result.Results, hookResult)
	}

	return result, nil
}

func inspectHookedBead(t *tmux.Tmux, bead *HookedBead, threshold time.Time) (*StaleHookResult, bool) {
	result := &StaleHookResult{
		BeadID:   bead.ID,
		Title:    bead.Title,
		Assignee: bead.Assignee,
		Age:      time.Since(bead.UpdatedAt).Round(time.Minute).String(),
	}
	sessionChecked := false
	if bead.Assignee != "" {
		if sessionName := assigneeToSessionName(bead.Assignee); sessionName != "" {
			alive, _ := t.HasSession(sessionName)
			result.AgentAlive = alive
			sessionChecked = true
		}
	}
	return result, staleHook(sessionChecked, result.AgentAlive, bead.UpdatedAt, threshold)
}

func staleHook(sessionChecked, agentAlive bool, updatedAt, threshold time.Time) bool {
	if sessionChecked {
		return !agentAlive
	}
	return updatedAt.Before(threshold)
}

func processStaleHook(townRoot string, bead *HookedBead, cfg *StaleHookConfig, result *StaleHookResult) bool {
	if result.AgentAlive {
		return false
	}
	checkWorktreeState(townRoot, bead.Assignee, result)
	if cfg.DryRun {
		return false
	}
	if err := unhookBead(townRoot, bead.ID); err != nil {
		result.Error = err.Error()
		return false
	}
	result.Unhooked = true
	return true
}

// listHookedBeads returns all beads with status=hooked.
func listHookedBeads(townRoot string) ([]*HookedBead, error) {
	cmd := beads.Command(townRoot, townBeadsDir(townRoot), beads.ReadOnlyRouting, "list", "--status=hooked", "--json", "--flat", "--limit=0")

	output, err := cmd.Output()
	if err != nil {
		// No hooked beads is not an error
		if strings.Contains(string(output), "no issues found") {
			return nil, nil
		}
		return nil, err
	}

	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 || string(trimmed) == "null" || (trimmed[0] != '[' && trimmed[0] != '{') {
		return nil, nil
	}

	var beads []*HookedBead
	if err := json.Unmarshal(output, &beads); err != nil {
		return nil, fmt.Errorf("parsing hooked beads: %w", err)
	}

	return beads, nil
}

// assigneeToSessionName converts an assignee address to a tmux session name.
// Delegates to session.ParseAddress for consistent parsing across the codebase.
func assigneeToSessionName(assignee string) string {
	identity, err := session.ParseAddress(assignee)
	if err != nil {
		return ""
	}
	return identity.SessionName()
}

// checkWorktreeState checks an agent's worktree for uncommitted changes or
// unpushed commits and populates the result fields. This is best-effort;
// errors are recorded but do not prevent unhooking.
func checkWorktreeState(townRoot, assignee string, result *StaleHookResult) {
	worktreePath := assigneeToWorktreePath(townRoot, assignee)
	if worktreePath == "" {
		return
	}

	g := git.NewGit(worktreePath)
	workStatus, err := git.CheckUncommittedWork(g)
	if err != nil {
		result.WorktreeError = fmt.Sprintf("checking worktree: %v", err)
		return
	}

	if !workStatus.CleanExcludingBeads() {
		result.PartialWork = true
		result.WorktreeDirty = workStatus.HasUncommittedChanges
		result.UnpushedCount = workStatus.UnpushedCommits
	}
}

// assigneeToWorktreePath resolves an assignee address to its git worktree path.
// Returns "" if the assignee format is unrecognized or the worktree doesn't exist.
// Supports polecat format "rig/polecats/name" and crew format "rig/crew/name".
func assigneeToWorktreePath(townRoot, assignee string) string {
	parts := strings.Split(assignee, "/")
	if len(parts) != 3 {
		return ""
	}

	rigName, agentType, name := parts[0], parts[1], parts[2]
	if agentType != "polecats" && agentType != "crew" {
		return ""
	}

	rigPath := filepath.Join(townRoot, rigName)

	// New structure: rig/polecats/<name>/<rigname>/
	newPath := filepath.Join(rigPath, agentType, name, rigName)
	if isGitWorktree(newPath) {
		return newPath
	}

	// Old structure: rig/polecats/<name>/
	oldPath := filepath.Join(rigPath, agentType, name)
	if isGitWorktree(oldPath) {
		return oldPath
	}

	return ""
}

func isGitWorktree(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// unhookBead sets a bead's status back to 'open'.
func unhookBead(townRoot, beadID string) error {
	cmd := beads.Command(townRoot, townBeadsDir(townRoot), beads.MutationRouting, "update", beadID, "--status=open")
	return cmd.Run()
}
