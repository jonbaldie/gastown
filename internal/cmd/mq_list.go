package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/refinery"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

type mqScoredIssue struct {
	issue           *beads.Issue
	fields          *beads.MRFields
	score           float64
	branchMissing   bool
	branchVerifyErr bool
}

type verifiedMQIssue struct {
	*beads.Issue
	BranchExists *bool `json:"branch_exists,omitempty"`
	VerifyError  bool  `json:"verify_error,omitempty"`
}

type mqListOptions struct {
	ready  bool
	status string
	worker string
	epic   string
	json   bool
	verify bool
}

func buildMQListOptions(rigName string, options mqListOptions) beads.ListOptions {
	opts := beads.ListOptions{
		Label:    "gt:merge-request",
		Priority: -1,
		Rig:      rigName,
	}
	if options.status != "" {
		opts.Status = options.status
	} else if !options.ready {
		opts.Status = "open"
	}
	return opts
}

func loadMQListIssues(b *beads.Beads, opts beads.ListOptions, options mqListOptions) ([]*beads.Issue, error) {
	if options.ready {
		opts.Status = "open"
		allOpen, err := b.ListMergeRequests(opts)
		if err != nil {
			return nil, fmt.Errorf("querying ready MRs: %w", err)
		}
		var issues []*beads.Issue
		for _, issue := range allOpen {
			if !isMergeRequestReadyForSelection(issue) {
				continue
			}
			issues = append(issues, issue)
		}
		return issues, nil
	}
	issues, err := b.ListMergeRequests(opts)
	if err != nil {
		return nil, fmt.Errorf("querying merge queue: %w", err)
	}
	return issues, nil
}

func mqListStatusMatches(issue *beads.Issue, options mqListOptions) bool {
	switch {
	case options.ready:
		return issue.Status == "open"
	case options.status != "" && !strings.EqualFold(options.status, "all"):
		return strings.EqualFold(issue.Status, options.status)
	case options.status == "":
		return issue.Status == "open"
	default:
		return true
	}
}

func mqListWorkerMatches(fields *beads.MRFields, options mqListOptions) bool {
	if options.worker == "" {
		return true
	}
	worker := ""
	if fields != nil {
		worker = fields.Worker
	}
	return strings.EqualFold(worker, options.worker)
}

func mqListEpicMatches(fields *beads.MRFields, b *beads.Beads, refineryPath string, options mqListOptions) bool {
	if options.epic == "" {
		return true
	}
	target := ""
	if fields != nil {
		target = fields.Target
	}
	expectedTarget := resolveIntegrationBranchName(b, refineryPath, options.epic)
	return target == expectedTarget
}

func mqListIssueMatches(fields *beads.MRFields, rigName string, b *beads.Beads, refineryPath string, options mqListOptions) bool {
	if fields != nil && fields.Rig != "" && !strings.EqualFold(fields.Rig, rigName) {
		return false
	}
	if !mqListWorkerMatches(fields, options) {
		return false
	}
	if !mqListEpicMatches(fields, b, refineryPath, options) {
		return false
	}
	return true
}

func scoreMQListIssues(issues []*beads.Issue, rigName, refineryPath string, b *beads.Beads, gitClient branchVerifier, options mqListOptions) []mqScoredIssue {
	now := time.Now()
	var scored []mqScoredIssue
	for _, issue := range issues {
		if !mqListStatusMatches(issue, options) {
			continue
		}
		fields := beads.ParseMRFields(issue)
		if !mqListIssueMatches(fields, rigName, b, refineryPath, options) {
			continue
		}
		branchMissing, branchVerifyErr := verifyBranch(options.verify, gitClient, fields)
		score := calculateMRScore(issue, fields, now)
		scored = append(scored, mqScoredIssue{
			issue:           issue,
			fields:          fields,
			score:           score,
			branchMissing:   branchMissing,
			branchVerifyErr: branchVerifyErr,
		})
	}
	return scored
}

func filteredMQListIssues(scored []mqScoredIssue) []*beads.Issue {
	var filtered []*beads.Issue
	for _, item := range scored {
		filtered = append(filtered, item.issue)
	}
	return filtered
}

func verifiedMQListItem(item mqScoredIssue) verifiedMQIssue {
	verified := verifiedMQIssue{Issue: item.issue}
	if item.fields == nil || item.fields.Branch == "" {
		return verified
	}
	if item.branchVerifyErr {
		verified.VerifyError = true
		return verified
	}
	exists := !item.branchMissing
	verified.BranchExists = &exists
	return verified
}

func renderMQListJSON(scored []mqScoredIssue, verify bool) error {
	if !verify {
		return outputJSON(filteredMQListIssues(scored))
	}
	var verified []verifiedMQIssue
	for _, item := range scored {
		verified = append(verified, verifiedMQListItem(item))
	}
	return outputJSON(verified)
}

func mqListStatusCell(issue *beads.Issue) string {
	displayStatus := issue.Status
	if issue.Status == "open" {
		if beads.HasUnresolvedBlockers(issue) {
			displayStatus = "blocked"
		} else {
			displayStatus = "ready"
		}
	}
	switch displayStatus {
	case "ready":
		return style.Success.Render("ready")
	case "in_progress":
		return style.Warning.Render("active")
	case "blocked":
		return style.Dim.Render("blocked")
	case "closed":
		return style.Dim.Render("closed")
	default:
		return displayStatus
	}
}

func mqListConvoyCell(fields *beads.MRFields) (branch, target, convoy string) {
	if fields != nil {
		branch = fields.Branch
		target = fields.Target
		convoy = fields.ConvoyID
	}
	if target == "" {
		target = style.Dim.Render("(unset)")
	}
	if convoy == "" {
		return branch, target, style.Dim.Render("(none)")
	}
	if len(convoy) > 12 {
		convoy = convoy[:12]
	}
	return branch, target, convoy
}

func mqListPriorityCell(priority int) string {
	value := fmt.Sprintf("P%d", priority)
	if priority <= 1 {
		return style.Error.Render(value)
	}
	if priority == 2 {
		return style.Warning.Render(value)
	}
	return value
}

func mqListGitCell(item mqScoredIssue, verify bool) string {
	if !verify {
		return ""
	}
	if item.branchVerifyErr {
		return style.Warning.Render("ERR")
	}
	if item.branchMissing {
		return style.Error.Render("MISSING")
	}
	return style.Success.Render("OK")
}

func addMQListRow(table *style.Table, item mqScoredIssue, verify bool) {
	branch, target, convoy := mqListConvoyCell(item.fields)
	displayID := item.issue.ID
	if len(displayID) > 12 {
		displayID = displayID[:12]
	}
	row := []string{
		displayID,
		fmt.Sprintf("%.1f", item.score),
		mqListPriorityCell(item.issue.Priority),
		convoy,
		branch,
		target,
		mqListStatusCell(item.issue),
	}
	if verify {
		row = append(row, mqListGitCell(item, true))
	}
	row = append(row, style.Dim.Render(formatMRAge(item.issue.CreatedAt)))
	table.AddRow(row...)
}

func renderMQListMissingBranchSummary(scored []mqScoredIssue, verify bool) {
	if !verify {
		return
	}
	missingCount := 0
	for _, item := range scored {
		if item.branchMissing {
			missingCount++
		}
	}
	if missingCount > 0 {
		fmt.Printf("\n  %s %d MR(s) with missing branches\n",
			style.Error.Render("⚠"), missingCount)
	}
}

func renderMQListBlockers(scored []mqScoredIssue) {
	for _, item := range scored {
		issue := item.issue
		displayStatus := issue.Status
		if issue.Status == "open" && beads.HasUnresolvedBlockers(issue) {
			displayStatus = "blocked"
		}
		blockerID := beads.FirstUnresolvedBlockerID(issue)
		if displayStatus != "blocked" || blockerID == "" {
			continue
		}
		displayID := issue.ID
		if len(displayID) > 12 {
			displayID = displayID[:12]
		}
		fmt.Printf("  %s %s\n", style.Dim.Render(displayID+":"),
			style.Dim.Render(fmt.Sprintf("waiting on %s", blockerID)))
	}
}

func renderMQListText(rigName string, scored []mqScoredIssue, verify bool) error {
	fmt.Printf("%s Merge queue for '%s':\n\n", style.Bold.Render("📋"), rigName)
	if len(scored) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(empty)"))
		return nil
	}
	table := style.NewTable(buildMQListColumns(verify)...)
	for _, item := range scored {
		addMQListRow(table, item, verify)
	}
	fmt.Print(table.Render())
	renderMQListMissingBranchSummary(scored, verify)
	renderMQListBlockers(scored)
	return nil
}

func runMQList(cmd *cobra.Command, args []string) error {
	rigName := args[0]
	options, err := readMQListOptions(cmd)
	if err != nil {
		return err
	}

	ctx, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	r := ctx.r

	// Create beads wrapper for the rig - use BeadsPath() to get the git-synced location
	b := beads.New(r.BeadsPath())
	opts := buildMQListOptions(rigName, options)
	issues, err := loadMQListIssues(b, opts, options)
	if err != nil {
		return err
	}

	var gitClient branchVerifier
	if options.verify {
		refineryRigPath := filepath.Join(r.Path, "refinery", "rig")
		gitClient = gitBranchVerifier{git.NewGit(refineryRigPath)}
	}
	scored := scoreMQListIssues(issues, rigName, r.Path, b, gitClient, options)
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	if options.json {
		return renderMQListJSON(scored, options.verify)
	}
	return renderMQListText(rigName, scored, options.verify)
}

func readMQListOptions(cmd *cobra.Command) (mqListOptions, error) {
	options := mqListOptions{}
	var err error
	if options.ready, err = readMQBoolFlag(cmd, "ready"); err != nil {
		return options, err
	}
	if options.status, err = readMQStringFlag(cmd, "status"); err != nil {
		return options, err
	}
	if options.worker, err = readMQStringFlag(cmd, "worker"); err != nil {
		return options, err
	}
	if options.epic, err = readMQStringFlag(cmd, "epic"); err != nil {
		return options, err
	}
	if options.json, err = readMQBoolFlag(cmd, "json"); err != nil {
		return options, err
	}
	if options.verify, err = readMQBoolFlag(cmd, "verify"); err != nil {
		return options, err
	}
	return options, nil
}

// formatMRAge formats the age of an MR from its created_at timestamp.
func formatMRAge(createdAt string) string {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		// Try other formats
		t, err = time.Parse("2006-01-02T15:04:05Z", createdAt)
		if err != nil {
			return "?"
		}
	}

	d := time.Since(t)

	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// outputJSON outputs data as JSON.
func outputJSON(data interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}

func buildMQListColumns(verify bool) []style.Column {
	columns := []style.Column{
		{Name: "ID", Width: 12},
		{Name: "SCORE", Width: 7, Align: style.AlignRight},
		{Name: "PRI", Width: 4},
		{Name: "CONVOY", Width: 12},
		{Name: "BRANCH", Width: 24},
		{Name: "TARGET", Width: 24},
		{Name: "STATUS", Width: 10},
	}
	if verify {
		columns = append(columns, style.Column{Name: "GIT", Width: 8})
	}
	return append(columns, style.Column{Name: "AGE", Width: 6, Align: style.AlignRight})
}

// calculateMRScore computes the priority score for an MR using the refinery scoring function.
// Higher scores mean higher priority (process first).
func calculateMRScore(issue *beads.Issue, fields *beads.MRFields, now time.Time) float64 {
	// Parse MR creation time
	mrCreatedAt, err := time.Parse(time.RFC3339, issue.CreatedAt)
	if err != nil {
		mrCreatedAt, err = time.Parse("2006-01-02T15:04:05Z", issue.CreatedAt)
		if err != nil {
			mrCreatedAt = now // Fallback to now if parsing fails
		}
	}

	// Build score input
	input := refinery.ScoreInput{
		Priority:    issue.Priority,
		MRCreatedAt: mrCreatedAt,
		Now:         now,
	}

	// Add fields from MR metadata if available
	if fields != nil {
		input.RetryCount = fields.RetryCount

		// Parse convoy created at if available
		if fields.ConvoyCreatedAt != "" {
			if convoyTime, err := time.Parse(time.RFC3339, fields.ConvoyCreatedAt); err == nil {
				input.ConvoyCreatedAt = &convoyTime
			}
		}
	}

	return refinery.ScoreMRWithDefaults(input)
}

// branchVerifier abstracts git branch existence checks for testability.
type branchVerifier interface {
	BranchExists(_ string) (bool, error)
	RemoteTrackingBranchExists(_, _ string) (bool, error)
}

// verifyBranch checks if a branch exists locally or as a remote-tracking ref.
// Returns (missing, verifyErr).
func verifyBranch(verify bool, client branchVerifier, fields *beads.MRFields) (bool, bool) {
	if !verify || client == nil || fields == nil || fields.Branch == "" {
		return false, false
	}
	localExists, err := client.BranchExists(fields.Branch)
	if err != nil {
		return false, true
	}
	if localExists {
		return false, false
	}
	// Also check remote-tracking ref (polecats often only have origin refs)
	remoteExists, rerr := client.RemoteTrackingBranchExists("origin", fields.Branch)
	if rerr != nil {
		return false, true
	}
	if !remoteExists {
		return true, false
	}
	return false, false
}
