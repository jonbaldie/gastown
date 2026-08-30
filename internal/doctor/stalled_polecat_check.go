package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// StalledPolecatCheck detects polecats whose tmux sessions have died but whose
// worktrees still contain unpushed commits. These are the most dangerous failure
// mode after disk space exhaustion: the polecat appears dead, and nuking it
// would permanently lose the committed work on its branch.
//
// This check warns about at-risk branches so they can be pushed before cleanup.
type StalledPolecatCheck struct {
	FixableCheck
	stalledPolecats []stalledPolecatInfo // Cached during Run for use in Fix
}

type stalledPolecatInfo struct {
	name          string
	rigName       string
	branch        string
	unpushedCount int
	clonePath     string
}

// NewStalledPolecatCheck creates a new stalled polecat check.
func NewStalledPolecatCheck() *StalledPolecatCheck {
	return &StalledPolecatCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "stalled-polecats",
				CheckDescription: "Detect polecats with dead sessions and unpushed work",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// Run checks all rigs for polecats with dead sessions and unpushed commits.
func (c *StalledPolecatCheck) Run(ctx *CheckContext) *CheckResult {
	t := tmux.NewTmux()
	stalled, checked := c.inspectRigs(ctx, t)

	c.stalledPolecats = stalled
	return c.result(stalled, checked)
}

func (c *StalledPolecatCheck) inspectRigs(ctx *CheckContext, t *tmux.Tmux) ([]stalledPolecatInfo, int) {
	var stalled []stalledPolecatInfo
	checked := 0
	for _, rigName := range c.findRigs(ctx) {
		rigStalled, rigChecked := c.inspectRig(ctx, t, rigName)
		stalled = append(stalled, rigStalled...)
		checked += rigChecked
	}
	return stalled, checked
}

func (c *StalledPolecatCheck) inspectRig(ctx *CheckContext, t *tmux.Tmux, rigName string) ([]stalledPolecatInfo, int) {
	entries, err := os.ReadDir(filepath.Join(ctx.TownRoot, rigName, "polecats"))
	if err != nil {
		return nil, 0
	}
	var stalled []stalledPolecatInfo
	checked := 0
	for _, entry := range entries {
		if !isStalledPolecatEntry(entry) {
			continue
		}
		checked++
		if info, ok := c.inspectPolecat(ctx, t, rigName, entry.Name()); ok {
			stalled = append(stalled, info)
		}
	}
	return stalled, checked
}

func isStalledPolecatEntry(entry os.DirEntry) bool {
	return entry.IsDir() && !strings.HasPrefix(entry.Name(), ".")
}

func (c *StalledPolecatCheck) inspectPolecat(ctx *CheckContext, t *tmux.Tmux, rigName, polecatName string) (stalledPolecatInfo, bool) {
	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
	alive, err := t.HasSession(sessionName)
	if err != nil || alive {
		return stalledPolecatInfo{}, false
	}
	clonePath := c.resolveClonePath(ctx.TownRoot, rigName, polecatName)
	if clonePath == "" {
		return stalledPolecatInfo{}, false
	}
	polecatGit := git.NewGit(clonePath)
	branch, err := git.CurrentBranch(polecatGit)
	if err != nil || branch == "" {
		return stalledPolecatInfo{}, false
	}
	pushed, unpushedCount, err := git.BranchPushedToRemote(polecatGit, branch, "origin")
	if err != nil || pushed || unpushedCount == 0 {
		return stalledPolecatInfo{}, false
	}
	return stalledPolecatInfo{
		name:          polecatName,
		rigName:       rigName,
		branch:        branch,
		unpushedCount: unpushedCount,
		clonePath:     clonePath,
	}, true
}

func (c *StalledPolecatCheck) result(stalled []stalledPolecatInfo, checked int) *CheckResult {
	if len(stalled) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: stalledPolecatOKMessage(checked),
		}
	}

	return &CheckResult{
		Name:   c.Name(),
		Status: StatusWarning,
		Message: fmt.Sprintf("Found %d stalled polecat(s) with unpushed work at risk of loss",
			len(stalled)),
		Details: stalledPolecatDetails(stalled),
		FixHint: "Run 'gt doctor --fix' to push stalled branches to remote",
	}
}

func stalledPolecatOKMessage(checked int) string {
	if checked > 0 {
		return fmt.Sprintf("Checked %d polecat(s), no unpushed work at risk", checked)
	}
	return "No stalled polecats with unpushed work"
}

func stalledPolecatDetails(stalled []stalledPolecatInfo) []string {
	details := make([]string, len(stalled))
	for i, info := range stalled {
		details[i] = fmt.Sprintf("STALLED: %s/%s — branch %s has %d unpushed commit(s)",
			info.rigName, info.name, info.branch, info.unpushedCount)
	}
	return details
}

// Fix pushes branches from stalled polecats to the remote.
func (c *StalledPolecatCheck) Fix(_ *CheckContext) error {
	if len(c.stalledPolecats) == 0 {
		return nil
	}

	var lastErr error
	for _, s := range c.stalledPolecats {
		polecatGit := git.NewGit(s.clonePath)
		if err := git.Push(polecatGit, "origin", s.branch, false); err != nil {
			lastErr = fmt.Errorf("pushing %s/%s branch %s: %w", s.rigName, s.name, s.branch, err)
		}
	}
	return lastErr
}

// findRigs returns the list of rig names to check.
func (c *StalledPolecatCheck) findRigs(ctx *CheckContext) []string {
	if ctx.RigName != "" {
		return []string{ctx.RigName}
	}

	// Scan town root for rig directories (directories containing polecats/)
	entries, err := os.ReadDir(ctx.TownRoot)
	if err != nil {
		return nil
	}

	var rigs []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == "mayor" {
			continue
		}
		polecatsDir := filepath.Join(ctx.TownRoot, entry.Name(), "polecats")
		if info, err := os.Stat(polecatsDir); err == nil && info.IsDir() {
			rigs = append(rigs, entry.Name())
		}
	}
	return rigs
}

// resolveClonePath finds the worktree path for a polecat.
// Handles both new (polecats/<name>/<rigname>/) and old (polecats/<name>/) structures.
func (c *StalledPolecatCheck) resolveClonePath(townRoot, rigName, polecatName string) string {
	// New structure: polecats/<name>/<rigname>/
	newPath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if info, err := os.Stat(newPath); err == nil && info.IsDir() {
		return newPath
	}

	// Old structure: polecats/<name>/
	oldPath := filepath.Join(townRoot, rigName, "polecats", polecatName)
	if info, err := os.Stat(filepath.Join(oldPath, ".git")); err == nil && !info.IsDir() {
		return oldPath
	}

	return ""
}
