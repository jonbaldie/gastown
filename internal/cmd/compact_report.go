package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

// beadIDLine matches a bead ID printed on its own line by `bd create`.
// We scan `bd create` output (which may include startup warnings such as the
// "beads.role not configured (GH#2950)" notice) for the last line that is
// just a bead ID, instead of trusting that stdout contained only the ID.
//
// IDs look like `hq-1a2b`, `co-rln`, `h25-mrd`, `my-rig-abc`: a rig prefix,
// dash, then a short alphanumeric token.
var beadIDLine = regexp.MustCompile(`(?m)^\s*([a-z][a-z0-9-]*-[a-z0-9]+)\s*$`)

// extractBeadID returns the last line of `output` that is a bare bead ID.
// Returns an error if no bead-ID-shaped line is found.
//
// This guards against `bd` emitting startup warnings before the ID — see
// gastown issue: "Fix compact_report.go beadID capture (corrupts on stdout
// warnings)". Without this, a noisy stdout poisons `beadID`, the subsequent
// `bd close <beadID>` silently fails, the audit bead stays open, and the
// daily-digest idempotency check (filtered by status=closed) never matches —
// producing one duplicate digest mail per patrol cycle.
func extractBeadID(output string) (string, error) {
	matches := beadIDLine.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		preview := strings.TrimSpace(output)
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return "", fmt.Errorf("no bead ID found in bd output: %q", preview)
	}
	return matches[len(matches)-1][1], nil
}

type compactReportOptions struct {
	dryRun  bool
	weekly  bool
	verbose bool
	date    string
	json    bool
}

// wispCategory maps individual wisp types to display categories.
// Matches the design doc: Heartbeats, Patrols, Errors, Untyped.
var wispCategoryMap = map[string]string{
	"heartbeat":  "Heartbeats",
	"ping":       "Heartbeats",
	"patrol":     "Patrols",
	"gc_report":  "Patrols",
	"error":      "Errors",
	"recovery":   "Errors",
	"escalation": "Errors",
}

// categoryOrder is the display order for categories in reports.
var categoryOrder = []string{"Heartbeats", "Patrols", "Errors", "Untyped"}

const zeroPatrolReportingGap = "0 eligible patrol wisps in the report query/window (patrol health not assessed)"

// categoryStats tracks per-category compaction statistics.
type categoryStats struct {
	Deleted  int `json:"deleted"`
	Promoted int `json:"promoted"`
	Active   int `json:"active"`
}

// compactReport is the full daily digest data.
type compactReport struct {
	Date       string                    `json:"date"`
	Categories map[string]*categoryStats `json:"categories"`
	Promotions []compactAction           `json:"promotions,omitempty"`
	Anomalies  []string                  `json:"anomalies,omitempty"`
	Errors     []string                  `json:"errors,omitempty"`
}

// weeklyRollup aggregates daily reports for trend data.
type weeklyRollup struct {
	WeekStart  string                    `json:"week_start"`
	WeekEnd    string                    `json:"week_end"`
	Days       int                       `json:"days"`
	Totals     map[string]*categoryStats `json:"totals"`
	Promotions int                       `json:"total_promotions"`
	Anomalies  []string                  `json:"anomalies,omitempty"`
}

type compactReportEvent struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
}

var compactReportCmd = &cobra.Command{
	Use:   "report",
	Short: "Generate and send compaction digest report",
	Long: `Generate a compaction digest and send it to deacon/ (cc mayor/).

The daily digest shows per-category breakdown of deleted, promoted, and active
wisps, plus any promotions with reasons and detected anomalies.

The weekly rollup (--weekly) aggregates the past 7 days of compaction event
beads and sends trend data to mayor/.

Examples:
  gt compact report              # Run compaction + send daily digest
  gt compact report --dry-run    # Preview the report without sending
  gt compact report --weekly     # Send weekly rollup to mayor/
  gt compact report --json       # Output report as JSON`,
	RunE: runCompactReport,
}

func init() {
	compactReportCmd.Flags().Bool("dry-run", false, "Preview report without sending")
	compactReportCmd.Flags().Bool("weekly", false, "Generate weekly rollup instead of daily digest")
	compactReportCmd.Flags().BoolP("verbose", "v", false, "Verbose output")
	compactReportCmd.Flags().String("date", "", "Report for specific date (YYYY-MM-DD); default: today")
	compactReportCmd.Flags().Bool("json", false, "Output report as JSON")

	compactCmd.AddCommand(compactReportCmd)
}

func runCompactReport(cmd *cobra.Command, _ []string) error {
	options, err := compactReportOptionsFrom(cmd)
	if err != nil {
		return err
	}
	if options.weekly {
		return runWeeklyRollup(options)
	}
	return runDailyDigest(options)
}

func compactReportOptionsFrom(cmd *cobra.Command) (compactReportOptions, error) {
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return compactReportOptions{}, err
	}
	weekly, err := cmd.Flags().GetBool("weekly")
	if err != nil {
		return compactReportOptions{}, err
	}
	verbose, err := cmd.Flags().GetBool("verbose")
	if err != nil {
		return compactReportOptions{}, err
	}
	date, err := cmd.Flags().GetString("date")
	if err != nil {
		return compactReportOptions{}, err
	}
	outputJSON, err := cmd.Flags().GetBool("json")
	if err != nil {
		return compactReportOptions{}, err
	}
	return compactReportOptions{
		dryRun:  dryRun,
		weekly:  weekly,
		verbose: verbose,
		date:    date,
		json:    outputJSON,
	}, nil
}

func runDailyDigest(options compactReportOptions) error {
	dateStr, err := dailyDigestDate(options.date)
	if err != nil {
		return err
	}
	if dailyDigestAlreadySent(dateStr, options.verbose) {
		return nil
	}

	report, err := loadDailyDigestReport(dateStr)
	if err != nil {
		return err
	}
	return publishDailyDigest(dateStr, report, options)
}

func dailyDigestDate(requested string) (string, error) {
	if requested == "" {
		return time.Now().UTC().Format("2006-01-02"), nil
	}
	if _, err := time.Parse("2006-01-02", requested); err != nil {
		return "", fmt.Errorf("invalid date format (use YYYY-MM-DD): %w", err)
	}
	return requested, nil
}

func dailyDigestAlreadySent(dateStr string, verbose bool) bool {
	existingID, err := findExistingCompactReport(dateStr)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "warning: idempotency check failed: %v\n", err)
		}
		return false
	}
	if existingID == "" {
		return false
	}
	fmt.Printf("%s Compaction digest already sent for %s (bead: %s)\n",
		style.Dim.Render("○"), dateStr, existingID)
	return true
}

func loadDailyDigestReport(dateStr string) (*compactReport, error) {

	// Run compaction with --json to get results
	compactOut, err := exec.Command("gt", "compact", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("running compaction: %w", err)
	}

	var result compactResult
	if err := json.Unmarshal(extractJSONObject(compactOut), &result); err != nil {
		return nil, fmt.Errorf("parsing compaction output: %w", err)
	}

	// Query active wisps for the "Active" column
	workDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting working dir: %w", err)
	}
	bd := beads.New(workDir)
	activeWisps, err := listReportWisps(bd)
	if err != nil {
		return nil, fmt.Errorf("listing active wisps: %w", err)
	}

	report := buildReport(dateStr, &result, activeWisps)
	report.Anomalies = detectAnomalies(report)
	return report, nil
}

func publishDailyDigest(dateStr string, report *compactReport, options compactReportOptions) error {
	if options.json {
		return encodeCompactReportJSON(report)
	}

	markdown := formatDailyDigest(report)
	if options.dryRun {
		fmt.Printf("%s [DRY RUN] Daily compaction digest for %s:\n\n", style.Dim.Render("[dry-run]"), dateStr)
		fmt.Println(markdown)
		return nil
	}

	beadID, err := createCompactReportBead(report, markdown)
	if err != nil {
		return fmt.Errorf("recording compact report audit bead: %w", err)
	}

	if err := sendCompactDigest(dateStr, markdown); err != nil {
		return fmt.Errorf("sending digest: %w", err)
	}

	fmt.Printf("%s Compaction digest sent for %s\n", style.Success.Render("✓"), dateStr)
	if beadID != "" {
		fmt.Printf("  Audit bead: %s\n", beadID)
	}

	return nil
}

func encodeCompactReportJSON(value any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

// listReportWisps includes infrastructure wisps that the default bd list view
// hides. This is intentionally separate from listWisps, whose result drives
// mutating compaction decisions and must retain its existing scope.
func listReportWisps(bd *beads.Beads) ([]*compactIssue, error) {
	out, err := bd.Run("list", "--include-infra", "--json", "--all", "-n", "0")
	if err != nil {
		return nil, err
	}

	var allIssues []*compactIssue
	if err := json.Unmarshal(extractJSONArray(out), &allIssues); err != nil {
		return nil, fmt.Errorf("parsing report issue list: %w", err)
	}

	var wisps []*compactIssue
	for _, issue := range allIssues {
		if issue.Ephemeral {
			wisps = append(wisps, issue)
		}
	}
	return wisps, nil
}

// buildReport aggregates compaction results by category.
func buildReport(dateStr string, result *compactResult, activeWisps []*compactIssue) *compactReport {
	report := &compactReport{
		Date:       dateStr,
		Categories: make(map[string]*categoryStats),
		Errors:     result.Errors,
	}

	// Initialize all categories
	for _, cat := range categoryOrder {
		report.Categories[cat] = &categoryStats{}
	}

	// Tally deleted by category
	for _, d := range result.Deleted {
		cat := wispTypeToCategory(d.WispType, d.Title)
		report.Categories[cat].Deleted++
	}

	// Tally promoted by category
	for _, p := range result.Promoted {
		cat := wispTypeToCategory(p.WispType, p.Title)
		report.Categories[cat].Promoted++
		report.Promotions = append(report.Promotions, p)
	}

	// Tally active wisps by category
	for _, w := range activeWisps {
		cat := wispTypeToCategory(w.WispType, w.Title)
		report.Categories[cat].Active++
	}

	return report
}

// wispTypeToCategory maps a wisp_type string to its display category.
func wispTypeToCategory(wispType, title string) string {
	if cat, ok := wispCategoryMap[wispType]; ok {
		return cat
	}
	if wispType == "" && strings.Contains(strings.ToLower(title), "patrol") {
		return "Patrols"
	}
	return "Untyped"
}

// detectAnomalies checks for unusual patterns in the compaction data.
func detectAnomalies(report *compactReport) []string {
	var anomalies []string

	for _, cat := range categoryOrder {
		anomalies = append(anomalies, categoryAnomalies(cat, report.Categories[cat])...)
	}

	return anomalies
}

func categoryAnomalies(category string, stats *categoryStats) []string {
	var anomalies []string
	if category == "Heartbeats" && stats.Deleted > 1000 {
		anomalies = append(anomalies, fmt.Sprintf(
			"%dx normal heartbeat volume (possible restart loop)",
			stats.Deleted/300))
	}
	if category == "Patrols" && stats.Active == 0 && stats.Deleted == 0 && stats.Promoted == 0 {
		anomalies = append(anomalies, zeroPatrolReportingGap)
	}
	total := stats.Deleted + stats.Promoted
	if total > 10 && stats.Promoted > total/2 {
		anomalies = append(anomalies,
			fmt.Sprintf("%s: high promotion rate (%d/%d) — review wisp classification",
				category, stats.Promoted, total))
	}
	return anomalies
}

// formatDailyDigest renders the markdown daily digest per the design doc format.
func formatDailyDigest(report *compactReport) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Wisp Compaction: %s\n\n", report.Date))
	writeDailySummary(&sb, report)
	writeDailyPromotions(&sb, report.Promotions)
	writeDigestList(&sb, "Anomalies", report.Anomalies)
	writeDigestList(&sb, "Errors", report.Errors)
	return sb.String()
}

func writeDailySummary(sb *strings.Builder, report *compactReport) {
	sb.WriteString("### Summary\n")
	sb.WriteString("| Category | Deleted | Promoted | Active |\n")
	sb.WriteString("|----------|---------|----------|--------|\n")

	for _, cat := range categoryOrder {
		stats := report.Categories[cat]
		// Skip empty categories
		if stats.Deleted == 0 && stats.Promoted == 0 && stats.Active == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("| %s | %d | %d | %d |\n",
			cat, stats.Deleted, stats.Promoted, stats.Active))
	}
}

func writeDailyPromotions(sb *strings.Builder, promotions []compactAction) {
	if len(promotions) == 0 {
		return
	}
	sb.WriteString("\n### Promotions\n")
	for _, p := range promotions {
		sb.WriteString(fmt.Sprintf("- %s: %q (reason: %s)\n",
			p.ID, compactTruncate(p.Title, 60), p.Reason))
	}
}

func writeDigestList(sb *strings.Builder, heading string, entries []string) {
	if len(entries) == 0 {
		return
	}
	sb.WriteString("\n### ")
	sb.WriteString(heading)
	sb.WriteString("\n")
	for _, entry := range entries {
		sb.WriteString(fmt.Sprintf("- %s\n", entry))
	}
}

// sendCompactDigest sends the daily digest via gt mail send.
func sendCompactDigest(dateStr, body string) error {
	subject := fmt.Sprintf("Wisp Compaction: %s", dateStr)

	// Send to mayor/ only — deacon/ is not a valid mail address (audit bead
	// serves as the deacon-side record).
	mailCmd := exec.Command("gt", "mail", "send", "mayor/",
		"-s", subject,
		"-m", body,
	)
	mailCmd.Stdout = os.Stdout
	mailCmd.Stderr = os.Stderr
	return mailCmd.Run()
}

// createCompactReportBead creates a permanent audit bead for the daily digest.
func createCompactReportBead(report *compactReport, markdown string) (string, error) {
	payloadJSON, err := json.Marshal(report)
	if err != nil {
		return "", fmt.Errorf("marshaling report payload: %w", err)
	}

	title := fmt.Sprintf("Compaction Report %s", report.Date)
	bdArgs := []string{
		"create",
		"--type=event",
		"--title=" + title,
		"--event-category=wisp.compaction",
		"--event-payload=" + string(payloadJSON),
		"--description=" + markdown,
		"--silent",
	}

	bdCmd := beads.Spawn(bdArgs...)
	output, err := bdCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("creating report bead: %w\nOutput: %s", err, string(output))
	}

	beadID, err := extractBeadID(string(output))
	if err != nil {
		return "", fmt.Errorf("parsing report bead id: %w", err)
	}

	// Auto-close (audit record, not work). Surface failures: if close fails,
	// the bead stays open and findExistingCompactReport (filter status=closed)
	// will never match, causing the digest to re-fire every patrol cycle.
	closeCmd := beads.Spawn("close", beadID, "--reason=daily compaction report")
	if out, err := closeCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("auto-closing report bead %s: %w\nOutput: %s", beadID, err, string(out))
	}

	return beadID, nil
}

// --- Weekly Rollup ---

func runWeeklyRollup(options compactReportOptions) error {
	now := time.Now().UTC()
	weekEnd := now.Format("2006-01-02")
	weekStart := now.AddDate(0, 0, -7).Format("2006-01-02")

	if weeklyRollupAlreadySent(weekStart, weekEnd, options.verbose) {
		return nil
	}

	rollup, err := loadWeeklyRollup(weekStart, weekEnd)
	if err != nil {
		return err
	}
	return publishWeeklyRollup(weekStart, weekEnd, rollup, options)
}

func weeklyRollupAlreadySent(weekStart, weekEnd string, verbose bool) bool {
	existingID, err := findExistingWeeklyRollup(weekStart, weekEnd)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "warning: weekly idempotency check failed: %v\n", err)
		}
		return false
	}
	if existingID == "" {
		return false
	}
	fmt.Printf("%s Weekly rollup already sent for %s to %s (bead: %s)\n",
		style.Dim.Render("○"), weekStart, weekEnd, existingID)
	return true
}

func loadWeeklyRollup(weekStart, weekEnd string) (*weeklyRollup, error) {
	reports, err := queryCompactionReports(weekStart, weekEnd)
	if err != nil {
		return nil, fmt.Errorf("querying compaction reports: %w", err)
	}
	rollup := &weeklyRollup{
		WeekStart: weekStart,
		WeekEnd:   weekEnd,
		Days:      len(reports),
		Totals:    make(map[string]*categoryStats),
	}

	// Initialize totals
	for _, cat := range categoryOrder {
		rollup.Totals[cat] = &categoryStats{}
	}

	// Aggregate
	for _, report := range reports {
		for cat, stats := range report.Categories {
			if _, ok := rollup.Totals[cat]; !ok {
				rollup.Totals[cat] = &categoryStats{}
			}
			rollup.Totals[cat].Deleted += stats.Deleted
			rollup.Totals[cat].Promoted += stats.Promoted
			rollup.Totals[cat].Active = stats.Active // Use latest active count
		}
		rollup.Promotions += len(report.Promotions)
		for _, anomaly := range report.Anomalies {
			rollup.Anomalies = append(rollup.Anomalies, normalizeCompactionAnomaly(anomaly))
		}
	}
	return rollup, nil
}

func publishWeeklyRollup(weekStart, weekEnd string, rollup *weeklyRollup, options compactReportOptions) error {
	if options.json {
		return encodeCompactReportJSON(rollup)
	}

	markdown := formatWeeklyRollup(rollup)
	if options.dryRun {
		fmt.Printf("%s [DRY RUN] Weekly compaction rollup (%s to %s):\n\n",
			style.Dim.Render("[dry-run]"), weekStart, weekEnd)
		fmt.Println(markdown)
		return nil
	}

	beadID, beadErr := createWeeklyRollupBead(rollup, markdown)
	if beadErr != nil {
		return fmt.Errorf("recording weekly rollup audit bead: %w", beadErr)
	}

	subject := fmt.Sprintf("Weekly Wisp Compaction: %s to %s", weekStart, weekEnd)
	mailCmd := exec.Command("gt", "mail", "send", "mayor/",
		"-s", subject,
		"-m", markdown,
	)
	mailCmd.Stdout = os.Stdout
	mailCmd.Stderr = os.Stderr
	if err := mailCmd.Run(); err != nil {
		return fmt.Errorf("sending weekly rollup: %w", err)
	}

	fmt.Printf("%s Weekly compaction rollup sent to mayor/ (%s to %s)\n",
		style.Success.Render("✓"), weekStart, weekEnd)
	if beadID != "" {
		fmt.Printf("  Audit bead: %s\n", beadID)
	}

	return nil
}

// queryCompactionReports queries compaction report event beads in a date range.
func queryCompactionReports(startDate, endDate string) ([]*compactReport, error) {
	listCmd := beads.Spawn("list",
		"--type=event",
		"--status=all",
		"--json",
		"--limit=0",
	)
	listOutput, err := listCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing event beads: %w", err)
	}

	var events []compactReportEvent
	if err := json.Unmarshal(extractJSONArray(listOutput), &events); err != nil {
		return nil, fmt.Errorf("parsing event list: %w", err)
	}

	reports, matchingEvents := collectCompactionReports(events, startDate, endDate)
	if matchingEvents > 0 && len(reports) == 0 {
		return nil, fmt.Errorf("found %d matching compaction report event(s), but no usable payload", matchingEvents)
	}

	sort.Slice(reports, func(i, j int) bool {
		return reports[i].Date < reports[j].Date
	})
	return reports, nil
}

func collectCompactionReports(events []compactReportEvent, startDate, endDate string) ([]*compactReport, int) {
	var reports []*compactReport
	reportIndexByDate := make(map[string]int)
	reportCreatedAtByDate := make(map[string]string)
	matchingEvents := 0
	for _, evt := range events {
		report, matching := parseCompactionReportEvent(evt, startDate, endDate)
		if !matching {
			continue
		}
		matchingEvents++
		if report == nil {
			continue
		}
		reports = appendCompactionReport(reports, reportIndexByDate, reportCreatedAtByDate, evt, report)
	}
	return reports, matchingEvents
}

func parseCompactionReportEvent(evt compactReportEvent, startDate, endDate string) (*compactReport, bool) {
	if !strings.HasPrefix(evt.Title, "Compaction Report ") {
		return nil, false
	}
	evtDate := strings.TrimPrefix(evt.Title, "Compaction Report ")
	if evtDate < startDate || evtDate > endDate || evt.Payload == "" {
		return nil, true
	}
	var report compactReport
	if err := json.Unmarshal([]byte(evt.Payload), &report); err != nil {
		return nil, true
	}
	return &report, true
}

func appendCompactionReport(reports []*compactReport, reportIndexByDate map[string]int, reportCreatedAtByDate map[string]string, evt compactReportEvent, report *compactReport) []*compactReport {
	if idx, exists := reportIndexByDate[report.Date]; exists {
		if evt.CreatedAt > reportCreatedAtByDate[report.Date] {
			reports[idx] = report
			reportCreatedAtByDate[report.Date] = evt.CreatedAt
		}
		return reports
	}
	reportIndexByDate[report.Date] = len(reports)
	reportCreatedAtByDate[report.Date] = evt.CreatedAt
	return append(reports, report)
}

func normalizeCompactionAnomaly(anomaly string) string {
	if anomaly == "0 patrol wisps (patrol agents may be down)" {
		return zeroPatrolReportingGap
	}
	return anomaly
}

// formatWeeklyRollup renders the markdown weekly rollup.
func formatWeeklyRollup(rollup *weeklyRollup) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Weekly Wisp Compaction: %s to %s\n\n", rollup.WeekStart, rollup.WeekEnd))
	sb.WriteString(fmt.Sprintf("**Days reported:** %d\n\n", rollup.Days))
	writeWeeklyCoverage(&sb, rollup.Days)
	totalDeleted, totalPromoted := writeWeeklyTotals(&sb, rollup)
	writeWeeklyRates(&sb, rollup, totalDeleted, totalPromoted)
	writeWeeklyAnomalies(&sb, rollup.Anomalies)
	return sb.String()
}

func writeWeeklyCoverage(sb *strings.Builder, days int) {
	if days != 0 {
		return
	}
	sb.WriteString("### Coverage\n")
	sb.WriteString("- No eligible daily compaction reports were found in this date range; patrol health was not assessed.\n\n")
}

func writeWeeklyTotals(sb *strings.Builder, rollup *weeklyRollup) (int, int) {
	sb.WriteString("### Totals\n")
	sb.WriteString("| Category | Deleted | Promoted | Active (latest) |\n")
	sb.WriteString("|----------|---------|----------|----------------|\n")

	totalDeleted := 0
	totalPromoted := 0

	for _, cat := range categoryOrder {
		stats := rollup.Totals[cat]
		if stats.Deleted == 0 && stats.Promoted == 0 && stats.Active == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("| %s | %d | %d | %d |\n",
			cat, stats.Deleted, stats.Promoted, stats.Active))
		totalDeleted += stats.Deleted
		totalPromoted += stats.Promoted
	}
	return totalDeleted, totalPromoted
}

func writeWeeklyRates(sb *strings.Builder, rollup *weeklyRollup, totalDeleted, totalPromoted int) {
	sb.WriteString("\n### Rates\n")
	sb.WriteString(fmt.Sprintf("- **Total deleted:** %d\n", totalDeleted))
	sb.WriteString(fmt.Sprintf("- **Total promoted:** %d\n", totalPromoted))
	if totalDeleted+totalPromoted > 0 {
		rate := float64(totalPromoted) / float64(totalDeleted+totalPromoted) * 100
		sb.WriteString(fmt.Sprintf("- **Promotion rate:** %.1f%%\n", rate))
	}
	if rollup.Days > 0 {
		sb.WriteString(fmt.Sprintf("- **Avg deleted/day:** %d\n", totalDeleted/rollup.Days))
	}
}

func writeWeeklyAnomalies(sb *strings.Builder, anomalies []string) {
	if len(anomalies) == 0 {
		return
	}
	sb.WriteString("\n### Anomalies This Week\n")
	seen := make(map[string]bool)
	for _, anomaly := range anomalies {
		if seen[anomaly] {
			continue
		}
		sb.WriteString(fmt.Sprintf("- %s\n", anomaly))
		seen[anomaly] = true
	}
}

// findExistingCompactReport checks if a compaction digest already exists for the given date.
// Returns the bead ID if found, empty string if not found.
func findExistingCompactReport(dateStr string) (string, error) {
	expectedTitle := fmt.Sprintf("Compaction Report %s", dateStr)

	listCmd := beads.Spawn("list",
		"--type=event",
		"--status=closed",
		"--json",
		"--limit=50",
	)
	listOutput, err := listCmd.Output()
	if err != nil {
		return "", err
	}

	var events []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(extractJSONArray(listOutput), &events); err != nil {
		return "", err
	}

	for _, evt := range events {
		if evt.Title == expectedTitle {
			return evt.ID, nil
		}
	}
	return "", nil
}

// findExistingWeeklyRollup checks if a weekly rollup already exists for the given week.
// Returns the bead ID if found, empty string if not found.
func findExistingWeeklyRollup(weekStart, weekEnd string) (string, error) {
	expectedTitle := fmt.Sprintf("Weekly Compaction Rollup %s to %s", weekStart, weekEnd)

	listCmd := beads.Spawn("list",
		"--type=event",
		"--json",
		"--limit=20",
	)
	listOutput, err := listCmd.Output()
	if err != nil {
		return "", err
	}

	var events []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(extractJSONArray(listOutput), &events); err != nil {
		return "", err
	}

	for _, evt := range events {
		if evt.Title == expectedTitle {
			return evt.ID, nil
		}
	}
	return "", nil
}

// extractJSONObject finds the first '{' byte in data and returns from that
// point onward. Strips non-JSON prefix from subprocess output.
func extractJSONObject(data []byte) []byte {
	idx := bytes.IndexByte(data, '{')
	if idx < 0 {
		return data
	}
	return data[idx:]
}

// createWeeklyRollupBead creates a permanent audit bead for the weekly rollup.
func createWeeklyRollupBead(rollup *weeklyRollup, markdown string) (string, error) {
	payloadJSON, err := json.Marshal(rollup)
	if err != nil {
		return "", fmt.Errorf("marshaling rollup payload: %w", err)
	}

	title := fmt.Sprintf("Weekly Compaction Rollup %s to %s", rollup.WeekStart, rollup.WeekEnd)
	bdArgs := []string{
		"create",
		"--type=event",
		"--title=" + title,
		"--event-category=wisp.compaction.weekly",
		"--event-payload=" + string(payloadJSON),
		"--description=" + markdown,
		"--silent",
	}

	bdCmd := beads.Spawn(bdArgs...)
	output, err := bdCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("creating weekly rollup bead: %w\nOutput: %s", err, string(output))
	}

	beadID, err := extractBeadID(string(output))
	if err != nil {
		return "", fmt.Errorf("parsing weekly rollup bead id: %w", err)
	}

	// Auto-close (audit record, not work). Surface failures so mail is not sent
	// without a matching audit record for future idempotency checks.
	closeCmd := beads.Spawn("close", beadID, "--reason=weekly compaction rollup")
	if out, err := closeCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("auto-closing rollup bead %s: %w\nOutput: %s", beadID, err, string(out))
	}

	return beadID, nil
}
