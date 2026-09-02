package cmd

import (
	"fmt"
	"sort"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

var mqNextCmd = &cobra.Command{
	Use:   "next <rig>",
	Short: "Show the highest-priority merge request",
	Long: `Show the next merge request to process based on priority score.

The priority scoring function considers:
  - Convoy age: Older convoys get higher priority (starvation prevention)
  - Issue priority: P0 > P1 > P2 > P3 > P4
  - Retry count: MRs that fail repeatedly get deprioritized
  - MR age: FIFO tiebreaker for same priority/convoy

Use --strategy=fifo for first-in-first-out ordering instead.

Examples:
  gt mq next gastown                    # Show highest-priority MR
  gt mq next gastown --strategy=fifo    # Show oldest MR instead
  gt mq next gastown --quiet            # Just print the MR ID
  gt mq next gastown --json             # Output as JSON`,
	Args: cobra.ExactArgs(1),
	RunE: runMQNext,
}

func init() {
	mqNextCmd.Flags().String("strategy", "priority", "Ordering strategy: 'priority' or 'fifo'")
	mqNextCmd.Flags().Bool("json", false, "Output as JSON")
	mqNextCmd.Flags().BoolP("quiet", "q", false, "Just print the MR ID")

	mqCmd.AddCommand(mqNextCmd)
}

func runMQNext(cmd *cobra.Command, args []string) error {
	rigName := args[0]
	strategy := commandStringFlag(cmd, "strategy")
	jsonOutput := commandBoolFlag(cmd, "json")
	quiet := commandBoolFlag(cmd, "quiet")

	ready, err := readyMergeRequests(rigName)
	if err != nil {
		return err
	}
	if len(ready) == 0 {
		return reportNoReadyMergeRequests(quiet)
	}

	now := time.Now()
	sortReadyMergeRequests(ready, strategy, now)
	return outputNextMergeRequest(ready, quiet, jsonOutput, now)
}

func readyMergeRequests(rigName string) ([]*beads.Issue, error) {
	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return nil, err
	}
	r := ctx.r

	b := beads.New(r.BeadsPath())
	issues, err := b.ListMergeRequests(beads.ListOptions{
		Label:    "gt:merge-request",
		Status:   "open",
		Priority: -1,
		Rig:      rigName,
	})
	if err != nil {
		return nil, fmt.Errorf("querying merge queue: %w", err)
	}
	return filterReadyMergeRequests(issues), nil
}

func filterReadyMergeRequests(issues []*beads.Issue) []*beads.Issue {
	ready := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		if isMergeRequestReadyForSelection(issue) {
			ready = append(ready, issue)
		}
	}
	return ready
}

func reportNoReadyMergeRequests(quiet bool) error {
	if quiet {
		return nil
	}
	fmt.Printf("%s No ready merge requests in queue\n", style.Dim.Render("ℹ"))
	return nil
}

func sortReadyMergeRequests(ready []*beads.Issue, strategy string, now time.Time) {
	if strategy == "fifo" {
		sortReadyMergeRequestsFIFO(ready)
		return
	}
	sortReadyMergeRequestsByPriority(ready, now)
}

func sortReadyMergeRequestsFIFO(ready []*beads.Issue) {
	sort.Slice(ready, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, ready[i].CreatedAt)
		tj, _ := time.Parse(time.RFC3339, ready[j].CreatedAt)
		return ti.Before(tj)
	})
}

type scoredMQNextIssue struct {
	issue *beads.Issue
	score float64
}

func sortReadyMergeRequestsByPriority(ready []*beads.Issue, now time.Time) {
	scored := make([]scoredMQNextIssue, len(ready))
	for i, issue := range ready {
		fields := beads.ParseMRFields(issue)
		scored[i] = scoredMQNextIssue{
			issue: issue,
			score: calculateMRScore(issue, fields, now),
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	for i, scoredIssue := range scored {
		ready[i] = scoredIssue.issue
	}
}

func outputNextMergeRequest(ready []*beads.Issue, quiet, jsonOutput bool, now time.Time) error {
	next := ready[0]
	fields := beads.ParseMRFields(next)
	if quiet {
		fmt.Println(next.ID)
		return nil
	}
	if jsonOutput {
		return outputJSON(next)
	}
	printNextMergeRequest(next, fields, ready, now)
	return nil
}

func printNextMergeRequest(next *beads.Issue, fields *beads.MRFields, ready []*beads.Issue, now time.Time) {
	fmt.Printf("%s Next MR to process:\n\n", style.Bold.Render("🎯"))
	fmt.Printf("  ID:       %s\n", next.ID)
	fmt.Printf("  Score:    %.1f\n", calculateMRScore(next, fields, now))
	fmt.Printf("  Priority: P%d\n", next.Priority)
	printNextMergeRequestFields(fields)
	fmt.Printf("  Age:      %s\n", formatMRAge(next.CreatedAt))
	if len(ready) > 1 {
		fmt.Printf("\n  %s\n", style.Dim.Render(fmt.Sprintf("(%d more in queue)", len(ready)-1)))
	}
}

func printNextMergeRequestFields(fields *beads.MRFields) {
	if fields == nil {
		return
	}
	if fields.Branch != "" {
		fmt.Printf("  Branch:   %s\n", fields.Branch)
	}
	if fields.Worker != "" {
		fmt.Printf("  Worker:   %s\n", fields.Worker)
	}
	if fields.ConvoyID != "" {
		fmt.Printf("  Convoy:   %s\n", fields.ConvoyID)
	}
	if fields.RetryCount > 0 {
		fmt.Printf("  Retries:  %d\n", fields.RetryCount)
	}
}
