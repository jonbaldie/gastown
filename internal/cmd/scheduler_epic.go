package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/sling"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
)

// epicScheduleOpts holds options for epic schedule operations.
type epicScheduleOpts struct {
	Formula     string
	HookRawBead bool
	Force       bool
	DryRun      bool
	NoBoot      bool
}

func runEpicScheduleByID(epicID string, opts epicScheduleOpts) error {
	run, err := beginEpicChildWork(epicID, opts)
	if err != nil || run == nil {
		return err
	}
	candidates, skips := collectEpicScheduleCandidates(run)
	return finishEpicSchedule(run, candidates, skips)
}

type epicChildWork struct {
	id       string
	opts     epicScheduleOpts
	townRoot string
	children []epicChild
}

func beginEpicChildWork(epicID string, opts epicScheduleOpts) (*epicChildWork, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, err
	}
	if err := verifyBeadExists(epicID); err != nil {
		return nil, fmt.Errorf("epic '%s' not found", epicID)
	}
	children, err := getEpicChildren(epicID)
	if err != nil {
		return nil, fmt.Errorf("listing children of %s: %w", epicID, err)
	}
	if len(children) == 0 {
		fmt.Printf("Epic %s has no child issues.\n", epicID)
		return nil, nil
	}
	return &epicChildWork{id: epicID, opts: opts, townRoot: townRoot, children: children}, nil
}

func collectEpicScheduleCandidates(run *epicChildWork) ([]convoyWorkCandidate, convoySkipCounts) {
	var childIDs []string
	for _, c := range run.children {
		childIDs = append(childIDs, c.ID)
	}
	scheduledSet := areScheduled(childIDs)
	var candidates []convoyWorkCandidate
	var skips convoySkipCounts
	for _, c := range run.children {
		if candidate, skip := epicCandidateFromChild(run.townRoot, c, run.opts.Force, scheduledSet); skip != "" {
			countConvoySkip(&skips, skip)
			continue
		} else if candidate != nil {
			candidates = append(candidates, *candidate)
		}
	}
	return candidates, skips
}

func epicCandidateFromChild(townRoot string, c epicChild, force bool, scheduledSet map[string]bool) (*convoyWorkCandidate, string) {
	if c.Status == "closed" || c.Status == "tombstone" {
		return nil, "closed"
	}
	if c.Assignee != "" && !force {
		return nil, "assigned"
	}
	if scheduledSet != nil && scheduledSet[c.ID] {
		return nil, "scheduled"
	}
	rigName := resolveRigForBead(townRoot, c.ID)
	if rigName == "" {
		fmt.Printf("  %s %s: cannot resolve rig from prefix %q (town-root or unknown)\n",
			style.Dim.Render("○"), c.ID, beads.ExtractPrefix(c.ID))
		return nil, "norig"
	}
	return &convoyWorkCandidate{ID: c.ID, Title: c.Title, RigName: rigName}, ""
}

func finishEpicSchedule(run *epicChildWork, candidates []convoyWorkCandidate, skips convoySkipCounts) error {
	if len(candidates) == 0 {
		fmt.Printf("No children to schedule from epic %s", run.id)
		if convoyScheduleHasSkips(skips) {
			fmt.Printf(" (%s)", convoyScheduleSkipSummary(skips))
		}
		fmt.Println()
		return nil
	}
	if run.opts.DryRun {
		printEpicScheduleDryRun(run, candidates, skips)
		return nil
	}
	return executeEpicSchedule(run, candidates, skips)
}

func printEpicScheduleDryRun(run *epicChildWork, candidates []convoyWorkCandidate, skips convoySkipCounts) {
	fmt.Printf("%s Would schedule %d child(ren) from epic %s:\n",
		style.Bold.Render("DRY-RUN"), len(candidates), run.id)
	if run.opts.Formula != "" {
		fmt.Printf("  Formula: %s\n", run.opts.Formula)
	} else {
		fmt.Printf("  Hook raw beads (no formula)\n")
	}
	for _, c := range candidates {
		fmt.Printf("  Would schedule: %s -> %s (%s)\n", c.ID, c.RigName, c.Title)
	}
	if convoyScheduleHasSkips(skips) {
		fmt.Printf("\nSkipped: %s\n", convoyScheduleSkipSummary(skips))
	}
}

func executeEpicSchedule(run *epicChildWork, candidates []convoyWorkCandidate, skips convoySkipCounts) error {
	fmt.Printf("%s Scheduling %d child(ren) from epic %s...\n",
		style.Bold.Render("📋"), len(candidates), run.id)
	successCount := 0
	for _, c := range candidates {
		err := scheduleBead(c.ID, c.RigName, ScheduleOptions{
			ScheduleWork: ScheduleWork{Formula: run.opts.Formula},
			Force:        run.opts.Force,
			HookRawBead:  run.opts.HookRawBead,
			NoConvoy:     true,
		})
		if err != nil {
			fmt.Printf("  %s %s: %v\n", style.Dim.Render("✗"), c.ID, err)
			continue
		}
		successCount++
	}
	fmt.Printf("\n%s Scheduled %d/%d child(ren) from epic %s\n",
		style.Bold.Render("📊"), successCount, len(candidates), run.id)
	if convoyScheduleHasSkips(skips) {
		fmt.Printf("  Skipped: %s\n", convoyScheduleSkipSummary(skips))
	}
	if successCount == 0 {
		return fmt.Errorf("all %d schedule attempts failed for epic %s", len(candidates), run.id)
	}
	return nil
}

func runEpicSlingByID(epicID string, opts epicScheduleOpts) error {
	run, err := beginEpicChildWork(epicID, opts)
	if err != nil || run == nil {
		return err
	}
	candidates, skips := collectEpicSlingCandidates(run)
	return finishEpicSling(run, candidates, skips)
}

func collectEpicSlingCandidates(run *epicChildWork) ([]convoyWorkCandidate, convoySkipCounts) {
	var candidates []convoyWorkCandidate
	var skips convoySkipCounts
	for _, c := range run.children {
		if candidate, skip := epicCandidateFromChild(run.townRoot, c, run.opts.Force, nil); skip != "" {
			countConvoySkip(&skips, skip)
			continue
		} else if candidate != nil {
			candidates = append(candidates, *candidate)
		}
	}
	return candidates, skips
}

func finishEpicSling(run *epicChildWork, candidates []convoyWorkCandidate, skips convoySkipCounts) error {
	if len(candidates) == 0 {
		fmt.Printf("No children to dispatch from epic %s", run.id)
		if convoySlingHasSkips(skips) {
			fmt.Printf(" (%s)", convoySlingSkipSummary(skips))
		}
		fmt.Println()
		return nil
	}
	if run.opts.DryRun {
		printEpicSlingDryRun(run, candidates, skips)
		return nil
	}
	return executeEpicSling(run, candidates, skips)
}

func printEpicSlingDryRun(run *epicChildWork, candidates []convoyWorkCandidate, skips convoySkipCounts) {
	fmt.Printf("%s Would dispatch %d child(ren) from epic %s:\n",
		style.Bold.Render("DRY-RUN"), len(candidates), run.id)
	for _, c := range candidates {
		fmt.Printf("  Would dispatch: %s -> %s (%s)\n", c.ID, c.RigName, c.Title)
	}
	if convoySlingHasSkips(skips) {
		fmt.Printf("\nSkipped: %s\n", convoySlingSkipSummary(skips))
	}
}

func executeEpicSling(run *epicChildWork, candidates []convoyWorkCandidate, skips convoySkipCounts) error {
	fmt.Printf("%s Dispatching %d child(ren) from epic %s...\n",
		style.Bold.Render("▶"), len(candidates), run.id)
	successCount, successfulRigs := dispatchEpicSlingCandidates(run, candidates)
	if !run.opts.NoBoot {
		for rig := range successfulRigs {
			wakeRigAgents(rig)
		}
	}
	return summarizeEpicSling(run, candidates, skips, successCount)
}

func dispatchEpicSlingCandidates(run *epicChildWork, candidates []convoyWorkCandidate) (int, map[string]bool) {
	successCount := 0
	successfulRigs := make(map[string]bool)
	for i, c := range candidates {
		if hitConvoySlingBatchLimit(i) {
			break
		}
		fmt.Printf("\n[%d/%d] Dispatching %s → %s...\n", i+1, len(candidates), c.ID, c.RigName)
		if err := dispatchEpicSlingCandidate(run, c); err != nil {
			fmt.Printf("  %s %s: %v\n", style.Dim.Render("✗"), c.ID, err)
			continue
		}
		successCount++
		successfulRigs[c.RigName] = true
		if i < len(candidates)-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return successCount, successfulRigs
}

func dispatchEpicSlingCandidate(run *epicChildWork, c convoyWorkCandidate) error {
	_, err := executeDeepSling(context.Background(), sling.Intent{
		BeadID:  c.ID,
		RigName: c.RigName,
		Formula: run.opts.Formula,
		IntentExecutionOptions: sling.IntentExecutionOptions{
			Force:         run.opts.Force,
			HookRawBead:   run.opts.HookRawBead,
			NoConvoy:      true,
			NoBoot:        true,
			CallerContext: "epic-sling",
			TownRoot:      run.townRoot,
			BeadsDir:      filepath.Join(run.townRoot, ".beads"),
		},
	})
	return err
}

func summarizeEpicSling(run *epicChildWork, candidates []convoyWorkCandidate, skips convoySkipCounts, successCount int) error {
	fmt.Printf("\n%s Dispatched %d/%d child(ren) from epic %s\n",
		style.Bold.Render("📊"), successCount, len(candidates), run.id)
	if convoySlingHasSkips(skips) {
		fmt.Printf("  Skipped: %s\n", convoySlingSkipSummary(skips))
	}
	if successCount == 0 {
		return fmt.Errorf("all %d dispatch attempts failed for epic %s", len(candidates), run.id)
	}
	return nil
}

// epicChild holds info about a child issue of an epic.
type epicChild struct {
	ID       string
	Title    string
	Status   string
	Assignee string
	Labels   []string
}

// getEpicChildren returns child issues of an epic via dependency lookup.
// Prefers raw SQL (bdDepListRawIDs) which handles cross-database deps correctly.
// Falls back to bd dep list for older bd versions (see GH #2624, #2832).
func getEpicChildren(epicID string) ([]epicChild, error) {
	dir := resolveBeadDir(epicID)

	// bd sql queries the database discovered from cmd.Dir. When the epic lives
	// in a rig database (not HQ), we must resolve to the rig's directory so
	// bd sql queries the correct database. resolveBeadDir returns the town root
	// (for bd CLI routing), but bd sql doesn't use routes.jsonl.
	sqlDir := dir
	if prefix := beads.ExtractPrefix(epicID); prefix != "" {
		townRoot, err := workspace.FindFromCwd()
		if err == nil {
			if rigPath := beads.GetRigPathForPrefix(townRoot, prefix); rigPath != "" {
				sqlDir = rigPath
			}
		}
	}

	// Prefer raw SQL — handles cross-database deps. Falls back to bd dep list
	// if bd sql is not available (older bd versions).
	childIDs, err := bdDepListRawIDs(sqlDir, epicID, "down", "tracks")
	if err != nil {
		// bd sql not supported — fall back to bd dep list.
		childIDs, err = bdDepListFallback(dir, epicID)
		if err != nil {
			return nil, fmt.Errorf("querying epic children for %s: %w", epicID, err)
		}
	}

	children := make([]epicChild, 0, len(childIDs))
	for _, id := range childIDs {
		info, err := getBeadInfo(id)
		if err != nil {
			children = append(children, epicChild{
				ID: id,
			})
			continue
		}
		children = append(children, epicChild{
			ID:       id,
			Title:    info.Title,
			Status:   info.Status,
			Assignee: info.Assignee,
			Labels:   info.Labels,
		})
	}

	return children, nil
}

// bdDepListFallback uses bd dep list to get child dependency IDs.
// This is the legacy path — it uses a SQL JOIN with the issues table which
// silently drops cross-database dependencies. Used as fallback when bd sql
// is not available.
func bdDepListFallback(dir, epicID string) ([]string, error) {
	stdout, err := BdCmd("dep", "list", epicID,
		"--direction=down", "--type=tracks", "--json").
		AllowStale().
		Dir(dir).
		StripBeadsDir().
		Stderr(io.Discard).
		Output()
	if err != nil {
		if len(stdout) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("bd dep list %s: %w", epicID, err)
	}

	var deps []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(stdout, &deps); err != nil {
		return nil, fmt.Errorf("parsing dependency list: %w", err)
	}

	ids := make([]string, 0, len(deps))
	for _, dep := range deps {
		id := beads.ExtractIssueID(dep.ID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
