package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/sling"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
)

// convoyScheduleOpts holds options for convoy schedule operations.
type convoyScheduleOpts struct {
	Formula     string
	HookRawBead bool
	Force       bool
	DryRun      bool
	NoBoot      bool
}

// runConvoyScheduleByID schedules all open tracked issues of a convoy.
func runConvoyScheduleByID(convoyID string, opts convoyScheduleOpts) error {
	run, err := beginConvoyTrackedWork(convoyID, opts)
	if err != nil || run == nil {
		return err
	}
	candidates, skips := collectConvoyScheduleCandidates(run)
	return finishConvoySchedule(run, candidates, skips)
}

type convoyTrackedWork struct {
	id       string
	opts     convoyScheduleOpts
	townRoot string
	tracked  []trackedIssueInfo
}

type convoyWorkCandidate struct {
	ID      string
	Title   string
	RigName string
}

type convoySkipCounts struct {
	closed    int
	assigned  int
	scheduled int
	noRig     int
}

func beginConvoyTrackedWork(convoyID string, opts convoyScheduleOpts) (*convoyTrackedWork, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, err
	}
	if err := verifyBeadExists(convoyID); err != nil {
		return nil, fmt.Errorf("convoy '%s' not found", convoyID)
	}
	tracked, err := getTrackedIssues(filepath.Join(townRoot, ".beads"), convoyID)
	if err != nil {
		return nil, fmt.Errorf("getting tracked issues: %w", err)
	}
	if len(tracked) == 0 {
		fmt.Printf("Convoy %s has no tracked issues.\n", convoyID)
		return nil, nil
	}
	return &convoyTrackedWork{id: convoyID, opts: opts, townRoot: townRoot, tracked: tracked}, nil
}

func collectConvoyScheduleCandidates(run *convoyTrackedWork) ([]convoyWorkCandidate, convoySkipCounts) {
	var beadIDs []string
	for _, t := range run.tracked {
		beadIDs = append(beadIDs, t.ID)
	}
	scheduledSet := areScheduled(beadIDs)
	var candidates []convoyWorkCandidate
	var skips convoySkipCounts
	for _, t := range run.tracked {
		if candidate, skip := convoyCandidateFromTracked(run.townRoot, t, run.opts.Force, scheduledSet); skip != "" {
			countConvoySkip(&skips, skip)
			continue
		} else if candidate != nil {
			candidates = append(candidates, *candidate)
		}
	}
	return candidates, skips
}

func convoyCandidateFromTracked(townRoot string, t trackedIssueInfo, force bool, scheduledSet map[string]bool) (*convoyWorkCandidate, string) {
	if t.Status == "closed" || t.Status == "tombstone" {
		return nil, "closed"
	}
	if t.Assignee != "" && !force {
		return nil, "assigned"
	}
	if scheduledSet != nil && scheduledSet[t.ID] {
		return nil, "scheduled"
	}
	rigName := resolveRigForBead(townRoot, t.ID)
	if rigName == "" {
		fmt.Printf("  %s %s: cannot resolve rig from prefix %q (town-root or unknown)\n",
			style.Dim.Render("○"), t.ID, beads.ExtractPrefix(t.ID))
		return nil, "norig"
	}
	return &convoyWorkCandidate{ID: t.ID, Title: t.Title, RigName: rigName}, ""
}

func countConvoySkip(skips *convoySkipCounts, skip string) {
	switch skip {
	case "closed":
		skips.closed++
	case "assigned":
		skips.assigned++
	case "scheduled":
		skips.scheduled++
	case "norig":
		skips.noRig++
	}
}

func convoyScheduleSkipSummary(skips convoySkipCounts) string {
	return fmt.Sprintf("%d closed, %d assigned, %d already scheduled, %d no rig",
		skips.closed, skips.assigned, skips.scheduled, skips.noRig)
}

func convoyScheduleHasSkips(skips convoySkipCounts) bool {
	return skips.closed > 0 || skips.assigned > 0 || skips.scheduled > 0 || skips.noRig > 0
}

func finishConvoySchedule(run *convoyTrackedWork, candidates []convoyWorkCandidate, skips convoySkipCounts) error {
	if len(candidates) == 0 {
		fmt.Printf("No issues to schedule from convoy %s", run.id)
		if convoyScheduleHasSkips(skips) {
			fmt.Printf(" (%s)", convoyScheduleSkipSummary(skips))
		}
		fmt.Println()
		return nil
	}
	if run.opts.DryRun {
		printConvoyScheduleDryRun(run, candidates, skips)
		return nil
	}
	return executeConvoySchedule(run, candidates, skips)
}

func printConvoyScheduleDryRun(run *convoyTrackedWork, candidates []convoyWorkCandidate, skips convoySkipCounts) {
	fmt.Printf("%s Would schedule %d issue(s) from convoy %s:\n",
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

func executeConvoySchedule(run *convoyTrackedWork, candidates []convoyWorkCandidate, skips convoySkipCounts) error {
	fmt.Printf("%s Scheduling %d issue(s) from convoy %s...\n",
		style.Bold.Render("📋"), len(candidates), run.id)
	successCount := 0
	for _, c := range candidates {
		err := scheduleBead(c.ID, c.RigName, ScheduleOptions{
			ScheduleWork: ScheduleWork{Formula: run.opts.Formula},
			NoConvoy:     true,
			Force:        run.opts.Force,
			HookRawBead:  run.opts.HookRawBead,
		})
		if err != nil {
			fmt.Printf("  %s %s: %v\n", style.Dim.Render("✗"), c.ID, err)
			continue
		}
		successCount++
	}
	fmt.Printf("\n%s Scheduled %d/%d issue(s) from convoy %s\n",
		style.Bold.Render("📊"), successCount, len(candidates), run.id)
	if convoyScheduleHasSkips(skips) {
		fmt.Printf("  Skipped: %s\n", convoyScheduleSkipSummary(skips))
	}
	if successCount == 0 {
		return fmt.Errorf("all %d schedule attempts failed for convoy %s", len(candidates), run.id)
	}
	return nil
}

func runConvoySlingByID(convoyID string, opts convoyScheduleOpts) error {
	run, err := beginConvoyTrackedWork(convoyID, opts)
	if err != nil || run == nil {
		return err
	}
	candidates, skips := collectConvoySlingCandidates(run)
	return finishConvoySling(run, candidates, skips)
}

func collectConvoySlingCandidates(run *convoyTrackedWork) ([]convoyWorkCandidate, convoySkipCounts) {
	var candidates []convoyWorkCandidate
	var skips convoySkipCounts
	for _, t := range run.tracked {
		if candidate, skip := convoyCandidateFromTracked(run.townRoot, t, run.opts.Force, nil); skip != "" {
			countConvoySkip(&skips, skip)
			continue
		} else if candidate != nil {
			candidates = append(candidates, *candidate)
		}
	}
	return candidates, skips
}

func convoySlingSkipSummary(skips convoySkipCounts) string {
	return fmt.Sprintf("%d closed, %d assigned, %d no rig", skips.closed, skips.assigned, skips.noRig)
}

func convoySlingHasSkips(skips convoySkipCounts) bool {
	return skips.closed > 0 || skips.assigned > 0 || skips.noRig > 0
}

func finishConvoySling(run *convoyTrackedWork, candidates []convoyWorkCandidate, skips convoySkipCounts) error {
	if len(candidates) == 0 {
		fmt.Printf("No issues to dispatch from convoy %s", run.id)
		if convoySlingHasSkips(skips) {
			fmt.Printf(" (%s)", convoySlingSkipSummary(skips))
		}
		fmt.Println()
		return nil
	}
	if run.opts.DryRun {
		printConvoySlingDryRun(run, candidates, skips)
		return nil
	}
	return executeConvoySling(run, candidates, skips)
}

func printConvoySlingDryRun(run *convoyTrackedWork, candidates []convoyWorkCandidate, skips convoySkipCounts) {
	fmt.Printf("%s Would dispatch %d issue(s) from convoy %s:\n",
		style.Bold.Render("DRY-RUN"), len(candidates), run.id)
	for _, c := range candidates {
		fmt.Printf("  Would dispatch: %s -> %s (%s)\n", c.ID, c.RigName, c.Title)
	}
	if convoySlingHasSkips(skips) {
		fmt.Printf("\nSkipped: %s\n", convoySlingSkipSummary(skips))
	}
}

func executeConvoySling(run *convoyTrackedWork, candidates []convoyWorkCandidate, skips convoySkipCounts) error {
	fmt.Printf("%s Dispatching %d issue(s) from convoy %s...\n",
		style.Bold.Render("▶"), len(candidates), run.id)
	successCount, successfulRigs := dispatchConvoySlingCandidates(run, candidates)
	if !run.opts.NoBoot {
		for rig := range successfulRigs {
			wakeRigAgents(rig)
		}
	}
	return summarizeConvoySling(run, candidates, skips, successCount)
}

func dispatchConvoySlingCandidates(run *convoyTrackedWork, candidates []convoyWorkCandidate) (int, map[string]bool) {
	successCount := 0
	successfulRigs := make(map[string]bool)
	for i, c := range candidates {
		if hitConvoySlingBatchLimit(i) {
			break
		}
		fmt.Printf("\n[%d/%d] Dispatching %s → %s...\n", i+1, len(candidates), c.ID, c.RigName)
		if err := dispatchConvoySlingCandidate(run, c); err != nil {
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

func hitConvoySlingBatchLimit(i int) bool {
	if slingState().maxConcurrent <= 0 || i < slingState().maxConcurrent {
		return false
	}
	fmt.Printf("  %s Reached --max-concurrent spawn batch size (%d), remaining will be scheduled next cycle\n", style.Dim.Render("○"), slingState().maxConcurrent)
	return true
}

func summarizeConvoySling(run *convoyTrackedWork, candidates []convoyWorkCandidate, skips convoySkipCounts, successCount int) error {
	fmt.Printf("\n%s Dispatched %d/%d issue(s) from convoy %s\n",
		style.Bold.Render("📊"), successCount, len(candidates), run.id)
	if convoySlingHasSkips(skips) {
		fmt.Printf("  Skipped: %s\n", convoySlingSkipSummary(skips))
	}
	if successCount == 0 {
		return fmt.Errorf("all %d dispatch attempts failed for convoy %s", len(candidates), run.id)
	}
	return nil
}

func dispatchConvoySlingCandidate(run *convoyTrackedWork, c convoyWorkCandidate) error {
	_, err := executeDeepSling(context.Background(), sling.Intent{
		BeadID:  c.ID,
		RigName: c.RigName,
		Formula: run.opts.Formula,
		Convoy:  run.id,
		IntentExecutionOptions: sling.IntentExecutionOptions{
			Force:         run.opts.Force,
			HookRawBead:   run.opts.HookRawBead,
			NoConvoy:      true,
			NoBoot:        true,
			CallerContext: "convoy-sling",
			TownRoot:      run.townRoot,
			BeadsDir:      filepath.Join(run.townRoot, ".beads"),
		},
	})
	return err
}
