package cmd

import (
	"encoding/json"
	"fmt"
	"github.com/jonbaldie/gastown/internal/beads"
	"os"
	"sort"
	"strings"

	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var mountainCmd = &cobra.Command{
	Use:         "mountain <epic-id>",
	GroupID:     GroupWork,
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Short:       "Activate Mountain-Eater: stage, label, and launch an epic",
	Long: `Activate the Mountain-Eater on an epic for autonomous grinding.

A mountain is a convoy with the 'mountain' label. This command:
  1. Stages the convoy (validate DAG, compute waves)
  2. Adds the 'mountain' label (enables Deacon audit + Witness failure tracking)
  3. Launches the convoy (dispatches Wave 1)

Regular convoys (no mountain label) continue working as normal.
The mountain label opts a convoy into enhanced stall detection,
skip-after-N-failures, and active progress monitoring.

Use subcommands to manage active mountains:
  gt mountain status [id]    Show mountain progress
  gt mountain pause <id>     Pause a mountain (stop dispatching)
  gt mountain resume <id>    Resume a paused mountain
  gt mountain cancel <id>    Cancel (remove mountain label)

Examples:
  gt mountain gt-epic-auth       Activate mountain on an epic
  gt mountain --force gt-epic-x  Launch even with staging warnings`,
	Args: cobra.ExactArgs(1),
	RunE: runMountain,
}

var mountainStatusCmd = &cobra.Command{
	Use:   "status [epic-id|convoy-id]",
	Short: "Show mountain progress",
	Long: `Show progress for active mountains.

Without arguments, lists all active mountains with progress bars.
With an ID, shows detailed status including active/blocked/skipped tasks.

Accepts either an epic ID or convoy ID.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runMountainStatus,
}

var mountainPauseCmd = &cobra.Command{
	Use:   "pause <epic-id|convoy-id>",
	Short: "Pause a mountain (stop dispatching new waves)",
	Long: `Pause an active mountain. Keeps the mountain label but stops
new wave dispatch. Active polecats continue their current work.

Resume with 'gt mountain resume'.`,
	Args: cobra.ExactArgs(1),
	RunE: runMountainPause,
}

var mountainResumeCmd = &cobra.Command{
	Use:   "resume <epic-id|convoy-id>",
	Short: "Resume a paused mountain",
	Long: `Resume a previously paused mountain. Re-enables wave dispatch
and continues grinding from where it left off.`,
	Args: cobra.ExactArgs(1),
	RunE: runMountainResume,
}

var mountainCancelCmd = &cobra.Command{
	Use:   "cancel <epic-id|convoy-id>",
	Short: "Cancel a mountain (remove label, keep convoy)",
	Long: `Cancel the Mountain-Eater on a convoy. Removes the mountain label
but leaves the convoy for manual management. Active polecats continue
their current work but no new waves will be dispatched with enhanced
monitoring.`,
	Args: cobra.ExactArgs(1),
	RunE: runMountainCancel,
}

func init() {
	mountainCmd.Flags().BoolP("force", "f", false, "Launch even with staging warnings")
	mountainCmd.Flags().Bool("json", false, "Output machine-readable JSON")

	mountainStatusCmd.Flags().Bool("json", false, "Output as JSON")

	mountainCmd.AddCommand(mountainStatusCmd)
	mountainCmd.AddCommand(mountainPauseCmd)
	mountainCmd.AddCommand(mountainResumeCmd)
	mountainCmd.AddCommand(mountainCancelCmd)

	rootCmd.AddCommand(mountainCmd)
}

// runMountain implements `gt mountain <epic-id>`.
// Stages a convoy from the epic, adds the mountain label, and launches Wave 1.
func runMountain(cmd *cobra.Command, args []string) error {
	force := commandBoolFlag(cmd, "force")
	epicID := args[0]
	plan, err := prepareMountain(epicID)
	if err != nil {
		return err
	}
	if plan.status == convoyStatusStagedWarnings && !force {
		return fmt.Errorf("staging has warnings, use --force to proceed")
	}

	convoyID, err := createMountainConvoy(plan)
	if err != nil {
		return err
	}

	results, err := launchMountainConvoy(convoyID, plan, force)
	if err != nil {
		return err
	}
	printMountainLaunch(convoyID, plan.dag, results)
	return nil
}

type mountainPlan struct {
	epic     *bdShowResult
	dag      *ConvoyDAG
	waves    []Wave
	status   string
	warnings []StagingFinding
}

func prepareMountain(epicID string) (*mountainPlan, error) {
	result, err := resolveMountainEpic(epicID)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Validating epic structure...\n")
	fmt.Printf("  Epic: %s %q\n", epicID, result.Title)

	input := &StageInput{Kind: StageInputEpic, IDs: []string{epicID}, RawArgs: []string{epicID}}
	beadList, deps, err := collectBeads(input)
	if err != nil {
		return nil, fmt.Errorf("collect beads: %w", err)
	}
	dag := buildConvoyDAG(beadList, deps)
	errs, warns := mountainFindings(dag, input)
	if len(errs) > 0 {
		fmt.Fprint(os.Stderr, renderErrors(errs))
		return nil, fmt.Errorf("mountain staging failed: %d error(s) found", len(errs))
	}

	waves, gated, err := computeWaves(dag)
	if err != nil {
		return nil, fmt.Errorf("compute waves: %w", err)
	}
	warns = append(warns, mountainGatedWarnings(gated)...)
	status := chooseStatus(errs, warns)
	plan := &mountainPlan{epic: result, dag: dag, waves: waves, status: status, warnings: warns}
	printMountainPlan(plan)
	return plan, nil
}

func resolveMountainEpic(epicID string) (*bdShowResult, error) {
	result, err := bdShow(epicID)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %s: %w", epicID, err)
	}
	if result.IssueType != "epic" {
		return nil, fmt.Errorf("%s is a %s, not an epic — mountains require an epic", epicID, result.IssueType)
	}
	return result, nil
}

func mountainFindings(dag *ConvoyDAG, input *StageInput) (errors, warnings []StagingFinding) {
	findings := append(detectErrors(dag), detectWarnings(dag, input)...)
	return categorizeFindings(findings)
}

func mountainGatedWarnings(gated []GatedTask) []StagingFinding {
	warnings := make([]StagingFinding, 0, len(gated))
	for _, g := range gated {
		warnings = append(warnings, StagingFinding{
			Severity:     "warning",
			Category:     "gated",
			BeadIDs:      []string{g.TaskID},
			Message:      fmt.Sprintf("task %s is gated by non-slingable blocker(s): %s", g.TaskID, strings.Join(g.GatedBy, ", ")),
			SuggestedFix: fmt.Sprintf("close or tombstone %s to include %s in waves", strings.Join(g.GatedBy, ", "), g.TaskID),
		})
	}
	return warnings
}

func printMountainPlan(plan *mountainPlan) {
	slingable, epics := countMountainTypes(plan.dag)
	fmt.Printf("  Tasks: %d (%d slingable, %d epics)\n", len(plan.dag.Nodes), slingable, epics)
	fmt.Printf("  Waves: %d (computed from blocking deps)\n", len(plan.waves))
	fmt.Printf("  Max parallelism: %d\n", maxMountainParallelism(plan.waves))
	if len(plan.warnings) > 0 {
		fmt.Printf("\n  Warnings:\n")
		for _, warning := range plan.warnings {
			fmt.Printf("    %s\n", warning.Message)
		}
	}
	fmt.Printf("\n  Errors: none\n")
}

func countMountainTypes(dag *ConvoyDAG) (slingable, epics int) {
	for _, node := range dag.Nodes {
		if isSlingableType(node.Type) {
			slingable++
		}
		if node.Type == "epic" {
			epics++
		}
	}
	return slingable, epics
}

func maxMountainParallelism(waves []Wave) int {
	maxParallel := 0
	for _, wave := range waves {
		if len(wave.Tasks) > maxParallel {
			maxParallel = len(wave.Tasks)
		}
	}
	return maxParallel
}

func createMountainConvoy(plan *mountainPlan) (string, error) {
	title := "Mountain: " + plan.epic.Title
	convoyID, err := createStagedConvoy(plan.dag, plan.waves, plan.status, title)
	if err != nil {
		return "", fmt.Errorf("create convoy: %w", err)
	}
	fmt.Printf("\nCreating convoy...\n")
	fmt.Printf("  Convoy: %s %q\n", convoyID, title)
	fmt.Printf("  Label: mountain\n")
	if err := bdAddLabelTown(convoyID, "mountain"); err != nil {
		return "", fmt.Errorf("add mountain label: %w", err)
	}
	if err := transitionConvoyToOpen(convoyID, true); err != nil {
		return "", fmt.Errorf("launch convoy: %w", err)
	}
	return convoyID, nil
}

func launchMountainConvoy(convoyID string, plan *mountainPlan, force bool) ([]DispatchResult, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("resolve town root: %w", err)
	}
	if err := checkBlockedRigsForLaunch(plan.dag, townRoot, force); err != nil {
		return nil, err
	}
	results, err := dispatchWave1(convoyID, plan.dag, plan.waves, townRoot)
	if err != nil {
		return nil, fmt.Errorf("dispatch wave 1: %w", err)
	}
	return results, nil
}

func printMountainLaunch(convoyID string, dag *ConvoyDAG, results []DispatchResult) {
	fmt.Printf("\nLaunching Wave 1 (%d tasks)...\n", len(results))
	sort.Slice(results, func(i, j int) bool { return results[i].BeadID < results[j].BeadID })
	for _, result := range results {
		printMountainDispatch(dag, result)
	}
	fmt.Printf("\nMountain active. ConvoyManager will feed subsequent waves.\n")
	fmt.Printf("Deacon will audit progress every ~10 minutes.\n")
	fmt.Printf("Check status: gt mountain status %s\n", convoyID)
}

func printMountainDispatch(dag *ConvoyDAG, result DispatchResult) {
	nodeTitle := ""
	if node := dag.Nodes[result.BeadID]; node != nil {
		nodeTitle = node.Title
	}
	rigName := result.Rig
	if rigName == "" {
		rigName = "auto"
	}
	if result.Success {
		fmt.Printf("  Slung %s → %s", result.BeadID, rigName)
		if nodeTitle != "" {
			fmt.Printf("  (%s)", nodeTitle)
		}
		fmt.Println()
		return
	}
	fmt.Printf("  Failed %s → %s: %v\n", result.BeadID, rigName, result.Error)
}

// bdAddLabelTown adds a label to a bead in the town beads database.
func bdAddLabelTown(beadID, label string) error {
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}
	cmd := beads.Spawn("update", beadID, "--add-label="+label)
	cmd.Dir = townBeads
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bd update %s --add-label=%s: %w\noutput: %s", beadID, label, err, out)
	}
	return nil
}

// bdRemoveLabelTown removes a label from a bead in the town beads database.
func bdRemoveLabelTown(beadID, label string) error {
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}
	cmd := beads.Spawn("update", beadID, "--remove-label="+label)
	cmd.Dir = townBeads
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bd update %s --remove-label=%s: %w\noutput: %s", beadID, label, err, out)
	}
	return nil
}

// mountainConvoyInfo holds convoy data for mountain status display.
type mountainConvoyInfo struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Labels []string `json:"labels"`
}

// runMountainStatus shows status for active mountains.
func runMountainStatus(cmd *cobra.Command, args []string) error {
	jsonOutput := commandBoolFlag(cmd, "json")
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}

	if len(args) == 0 {
		return showAllMountainStatus(townBeads, jsonOutput)
	}

	return showMountainDetail(townBeads, args[0], jsonOutput)
}

// showAllMountainStatus lists all active mountains with progress summary.
func showAllMountainStatus(townBeads string, jsonOutput bool) error {
	convoys, err := findMountainConvoys(townBeads)
	if err != nil {
		return err
	}

	if len(convoys) == 0 {
		fmt.Println("No active mountains.")
		fmt.Println("Activate with: gt mountain <epic-id>")
		return nil
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(convoys)
	}

	fmt.Println("Active Mountains:")
	for _, convoy := range convoys {
		printMountainSummary(convoy)
	}

	return nil
}

func printMountainSummary(convoy mountainConvoyInfo) {
	trackedBeads, _, err := collectConvoyBeads(convoy.ID)
	if err != nil {
		fmt.Printf("  %s %q (error reading beads: %v)\n", convoy.ID, convoy.Title, err)
		return
	}
	total, closed := countMountainProgress(trackedBeads)
	pct := 0
	if total > 0 {
		pct = (closed * 100) / total
	}
	fmt.Printf("  %s %q\n", convoy.ID, convoy.Title)
	fmt.Printf("    Progress: %s %d/%d (%d%%)\n", renderProgressBar(pct, 20), closed, total, pct)
}

func countMountainProgress(beads []BeadInfo) (total, closed int) {
	for _, bead := range beads {
		if !isSlingableType(bead.Type) {
			continue
		}
		total++
		if bead.Status == "closed" {
			closed++
		}
	}
	return total, closed
}

// showMountainDetail shows detailed status for a single mountain.
func showMountainDetail(townBeads, inputID string, jsonOutput bool) error {
	detail, err := loadMountainDetail(townBeads, inputID)
	if err != nil {
		return err
	}
	if jsonOutput {
		return outputMountainDetailJSON(detail)
	}
	printMountainDetailText(detail)
	return nil
}

type mountainDetail struct {
	convoy  mountainConvoyInfo
	dag     *ConvoyDAG
	waves   []Wave
	buckets mountainBuckets
}

type mountainBuckets struct {
	completed []string
	active    []string
	ready     []string
	skipped   []string
	blocked   []string
}

func loadMountainDetail(townBeads, inputID string) (*mountainDetail, error) {
	convoyID, err := resolveMountainID(townBeads, inputID)
	if err != nil {
		return nil, err
	}
	convoy, err := loadMountainConvoy(townBeads, convoyID)
	if err != nil {
		return nil, err
	}
	trackedBeads, deps, err := collectConvoyBeads(convoyID)
	if err != nil {
		return nil, fmt.Errorf("reading tracked beads: %w", err)
	}
	dag := buildConvoyDAG(trackedBeads, deps)
	waves, _, err := computeWaves(dag)
	if err != nil {
		return nil, fmt.Errorf("computing waves: %w", err)
	}
	return &mountainDetail{
		convoy:  convoy,
		dag:     dag,
		waves:   waves,
		buckets: categorizeMountainBeads(townBeads, trackedBeads, dag),
	}, nil
}

func loadMountainConvoy(townBeads, convoyID string) (mountainConvoyInfo, error) {
	showOut, err := runBdJSON(townBeads, "show", convoyID, "--json")
	if err != nil {
		return mountainConvoyInfo{}, fmt.Errorf("convoy %q not found", convoyID)
	}
	var convoys []mountainConvoyInfo
	if err := json.Unmarshal(showOut, &convoys); err != nil {
		return mountainConvoyInfo{}, fmt.Errorf("parsing convoy data: %w", err)
	}
	if len(convoys) == 0 {
		return mountainConvoyInfo{}, fmt.Errorf("convoy %q not found", convoyID)
	}
	if !hasLabel(convoys[0].Labels, "mountain") {
		return mountainConvoyInfo{}, fmt.Errorf("%s is not a mountain (no mountain label)", convoyID)
	}
	return convoys[0], nil
}

func categorizeMountainBeads(townBeads string, trackedBeads []BeadInfo, dag *ConvoyDAG) mountainBuckets {
	var buckets mountainBuckets
	for _, bead := range trackedBeads {
		if !isSlingableType(bead.Type) {
			continue
		}
		switch {
		case bead.Status == "closed":
			buckets.completed = append(buckets.completed, bead.ID)
		case bead.Status == "in_progress" || bead.Status == "hooked":
			buckets.active = append(buckets.active, bead.ID)
		case hasBeadLabel(townBeads, bead.ID, "mountain:skipped"):
			buckets.skipped = append(buckets.skipped, bead.ID)
		case mountainBeadBlocked(dag, bead.ID):
			buckets.blocked = append(buckets.blocked, bead.ID)
		default:
			buckets.ready = append(buckets.ready, bead.ID)
		}
	}
	return buckets
}

func mountainBeadBlocked(dag *ConvoyDAG, beadID string) bool {
	node := dag.Nodes[beadID]
	if node == nil {
		return false
	}
	for _, dep := range node.BlockedBy {
		depNode := dag.Nodes[dep]
		if depNode != nil && depNode.Status != "closed" {
			return true
		}
	}
	return false
}

func outputMountainDetailJSON(detail *mountainDetail) error {
	b := detail.buckets
	total := len(b.completed) + len(b.active) + len(b.ready) + len(b.skipped) + len(b.blocked)
	jsonOut := map[string]interface{}{
		"convoy_id": detail.convoy.ID,
		"title":     detail.convoy.Title,
		"status":    detail.convoy.Status,
		"total":     total,
		"completed": len(b.completed),
		"active":    len(b.active),
		"ready":     len(b.ready),
		"skipped":   len(b.skipped),
		"blocked":   len(b.blocked),
		"waves":     len(detail.waves),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(jsonOut)
}

func printMountainDetailText(detail *mountainDetail) {
	b := detail.buckets
	total := len(b.completed) + len(b.active) + len(b.ready) + len(b.skipped) + len(b.blocked)
	pct := 0
	if total > 0 {
		pct = (len(b.completed) * 100) / total
	}
	fmt.Printf("Mountain: %s %q\n", detail.convoy.ID, detail.convoy.Title)
	fmt.Printf("\nProgress: %d/%d closed (%d%%)\n", len(b.completed), total, pct)
	fmt.Printf("Wave: %d total\n", len(detail.waves))
	printMountainDetailSection("Completed", "✓", b.completed, detail.dag)
	printMountainDetailSection("Active", "⟳", b.active, detail.dag)
	printMountainDetailSection("Ready", "○", b.ready, detail.dag)
	printMountainDetailSection("Skipped", "⊘", b.skipped, detail.dag)
	printMountainBlockedSection(b.blocked, detail.dag)
}

func printMountainDetailSection(label, icon string, ids []string, dag *ConvoyDAG) {
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	fmt.Printf("\n%s (%d):\n", label, len(ids))
	for _, id := range ids {
		fmt.Printf("  %s %s  %s\n", icon, id, mountainNodeTitle(dag, id))
	}
}

func printMountainBlockedSection(ids []string, dag *ConvoyDAG) {
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	fmt.Printf("\nBlocked (%d):\n", len(ids))
	for _, id := range ids {
		node := dag.Nodes[id]
		title := mountainNodeTitle(dag, id)
		blockers := ""
		if node != nil {
			var openBlockers []string
			for _, dep := range node.BlockedBy {
				depNode := dag.Nodes[dep]
				if depNode != nil && depNode.Status != "closed" {
					openBlockers = append(openBlockers, dep)
				}
			}
			if len(openBlockers) > 0 {
				blockers = " (needs: " + strings.Join(openBlockers, ", ") + ")"
			}
		}
		fmt.Printf("  ◌ %s  %s%s\n", id, title, blockers)
	}
}

func mountainNodeTitle(dag *ConvoyDAG, id string) string {
	if node := dag.Nodes[id]; node != nil {
		return node.Title
	}
	return ""
}

// findMountainConvoys lists all open convoys with the mountain label.
func findMountainConvoys(townBeads string) ([]mountainConvoyInfo, error) {
	issues, err := listConvoyIssues(townBeads, "open", false, "mountain")
	if err != nil {
		return nil, fmt.Errorf("listing mountain convoys: %w", err)
	}

	convoys := make([]mountainConvoyInfo, 0, len(issues))
	for _, issue := range issues {
		convoys = append(convoys, mountainConvoyInfo{
			ID:     issue.ID,
			Title:  issue.Title,
			Labels: issue.Labels,
		})
	}

	return convoys, nil
}

// resolveMountainID resolves an epic-id or convoy-id to the mountain convoy ID.
// If the input is already a convoy with the mountain label, returns it directly.
// If the input is an epic, searches for a mountain convoy tracking it.
func resolveMountainID(townBeads, inputID string) (string, error) {
	result, err := bdShow(inputID)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s: %w", inputID, err)
	}

	if isConvoyIssue(result.IssueType, result.Labels) {
		return inputID, nil
	}

	// Input is an epic — find a mountain convoy that tracks it.
	convoys, err := findMountainConvoys(townBeads)
	if err != nil {
		return "", err
	}

	for _, cv := range convoys {
		// Check if the convoy title starts with "Mountain: " + epic title.
		if strings.Contains(cv.Title, result.Title) {
			return cv.ID, nil
		}
	}

	return "", fmt.Errorf("no active mountain found for epic %s", inputID)
}

// hasBeadLabel checks if a bead has a specific label by querying bd show.
func hasBeadLabel(townBeads, beadID, label string) bool {
	out, err := runBdJSON(townBeads, "show", beadID, "--json")
	if err != nil {
		return false
	}

	var results []struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(out, &results); err != nil || len(results) == 0 {
		return false
	}

	return hasLabel(results[0].Labels, label)
}

// renderProgressBar renders a simple Unicode progress bar.
func renderProgressBar(pct, width int) string {
	filled := (pct * width) / 100
	if filled > width {
		filled = width
	}
	var b strings.Builder
	for i := 0; i < width; i++ {
		if i < filled {
			b.WriteRune('█')
		} else {
			b.WriteRune('░')
		}
	}
	return b.String()
}

// runMountainPause pauses an active mountain by setting the convoy to paused status.
func runMountainPause(_ *cobra.Command, args []string) error {
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}

	convoyID, err := resolveMountainID(townBeads, args[0])
	if err != nil {
		return err
	}

	// Add paused label — the ConvoyManager checks this to skip dispatch.
	if err := bdAddLabelTown(convoyID, "mountain:paused"); err != nil {
		return fmt.Errorf("pause mountain: %w", err)
	}

	fmt.Printf("Mountain %s paused.\n", convoyID)
	fmt.Printf("Active polecats will finish their current work.\n")
	fmt.Printf("Resume with: gt mountain resume %s\n", convoyID)
	return nil
}

// runMountainResume resumes a paused mountain.
func runMountainResume(_ *cobra.Command, args []string) error {
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}

	convoyID, err := resolveMountainID(townBeads, args[0])
	if err != nil {
		return err
	}

	if err := bdRemoveLabelTown(convoyID, "mountain:paused"); err != nil {
		return fmt.Errorf("resume mountain: %w", err)
	}

	fmt.Printf("Mountain %s resumed.\n", convoyID)
	fmt.Printf("ConvoyManager will continue dispatching waves.\n")
	return nil
}

// runMountainCancel cancels a mountain by removing the mountain label.
// Leaves the convoy intact for manual management.
func runMountainCancel(_ *cobra.Command, args []string) error {
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}

	convoyID, err := resolveMountainID(townBeads, args[0])
	if err != nil {
		return err
	}

	// Remove mountain label (and paused if present).
	if err := bdRemoveLabelTown(convoyID, "mountain"); err != nil {
		return fmt.Errorf("cancel mountain: %w", err)
	}
	// Best-effort remove paused label too.
	_ = bdRemoveLabelTown(convoyID, "mountain:paused")

	fmt.Printf("Mountain canceled on %s.\n", convoyID)
	fmt.Printf("Convoy remains open for manual management.\n")
	fmt.Printf("Check convoy status: gt convoy status %s\n", convoyID)
	return nil
}
