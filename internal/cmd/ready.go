package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var readyCmd = &cobra.Command{
	Use:     "ready",
	GroupID: GroupWork,
	Short:   "Show work ready across town",
	Long: `Display all ready work items across the town and all rigs.

Aggregates ready issues from:
- Town beads (hq-* items: convoys, cross-rig coordination)
- Each rig's beads (project-level issues, MRs)

Ready items have no blockers and can be worked immediately.
Results are sorted by priority (highest first) then by source.

Examples:
  gt ready              # Show all ready work
  gt ready --json       # Output as JSON
  gt ready --rig=gastown  # Show only one rig`,
	RunE: runReady,
}

func init() {
	readyCmd.Flags().Bool("json", false, "Output as JSON")
	readyCmd.Flags().String("rig", "", "Filter to a specific rig")
	rootCmd.AddCommand(readyCmd)
}

// ReadySource represents ready items from a single source (town or rig).
type ReadySource struct {
	Name   string         `json:"name"`   // "town" or rig name
	Issues []*beads.Issue `json:"issues"` // Ready issues from this source
	Error  string         `json:"error,omitempty"`
}

// ReadyResult is the aggregated result of gt ready.
type ReadyResult struct {
	Sources  []ReadySource `json:"sources"`
	Summary  ReadySummary  `json:"summary"`
	TownRoot string        `json:"town_root,omitempty"`
}

// ReadySummary provides counts for the ready report.
type ReadySummary struct {
	Total    int            `json:"total"`
	BySource map[string]int `json:"by_source"`
	P0Count  int            `json:"p0_count"`
	P1Count  int            `json:"p1_count"`
	P2Count  int            `json:"p2_count"`
	P3Count  int            `json:"p3_count"`
	P4Count  int            `json:"p4_count"`
}

func runReady(cmd *cobra.Command, _ []string) error {
	jsonOutput := commandBoolFlag(cmd, "json")
	rigName := commandStringFlag(cmd, "rig")
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigs, err := discoverReadyRigs(townRoot, rigName)
	if err != nil {
		return err
	}
	sources := collectReadySources(townRoot, rigName, rigs)
	sortReadySources(sources)
	result := ReadyResult{
		Sources:  sources,
		Summary:  summarizeReadySources(sources),
		TownRoot: townRoot,
	}
	return printReadyResult(result, jsonOutput)
}

func discoverReadyRigs(townRoot, rigName string) ([]*rig.Rig, error) {
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}
	rigs, err := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot)).DiscoverRigs()
	if err != nil {
		return nil, fmt.Errorf("discovering rigs: %w", err)
	}
	if rigName == "" {
		return rigs, nil
	}
	for _, r := range rigs {
		if r.Name == rigName {
			return []*rig.Rig{r}, nil
		}
	}
	return nil, fmt.Errorf("rig not found: %s", rigName)
}

func collectReadySources(townRoot, rigName string, rigs []*rig.Rig) []ReadySource {
	var wg sync.WaitGroup
	var mu sync.Mutex
	sources := make([]ReadySource, 0, len(rigs)+1)
	if rigName == "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src := loadTownReadySource(townRoot)
			mu.Lock()
			sources = append(sources, src)
			mu.Unlock()
		}()
	}
	for _, r := range rigs {
		wg.Add(1)
		go func(r *rig.Rig) {
			defer wg.Done()
			src := loadRigReadySource(townRoot, r)
			mu.Lock()
			sources = append(sources, src)
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	return sources
}

func loadTownReadySource(townRoot string) ReadySource {
	townBeadsPath := beads.GetTownBeadsPath(townRoot)
	issues, err := beads.New(townBeadsPath).Ready()
	src := ReadySource{Name: "town"}
	if err != nil {
		src.Error = err.Error()
		return src
	}
	src.Issues = filterReadySourceIssues(townRoot, "town", townBeadsPath, issues)
	return src
}

func loadRigReadySource(townRoot string, r *rig.Rig) ReadySource {
	beadsPath := r.BeadsPath()
	issues, err := beads.New(beadsPath).Ready()
	src := ReadySource{Name: r.Name}
	if err != nil {
		src.Error = err.Error()
		return src
	}
	src.Issues = filterReadySourceIssues(townRoot, r.Name, beadsPath, issues)
	return src
}

func filterReadySourceIssues(townRoot, sourceName, beadsPath string, issues []*beads.Issue) []*beads.Issue {
	filtered := filterFormulaScaffolds(issues, getFormulaNames(beadsPath))
	filtered = filterWisps(filtered, getWispIDs(beadsPath))
	filtered = filterReadyIssuesByRoute(townRoot, sourceName, filtered)
	return filterIdentityBeads(filtered)
}

func sortReadySources(sources []ReadySource) {
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Name == "town" {
			return true
		}
		if sources[j].Name == "town" {
			return false
		}
		return sources[i].Name < sources[j].Name
	})
	for i := range sources {
		sort.Slice(sources[i].Issues, func(a, b int) bool {
			return sources[i].Issues[a].Priority < sources[i].Issues[b].Priority
		})
	}
}

func summarizeReadySources(sources []ReadySource) ReadySummary {
	summary := ReadySummary{BySource: make(map[string]int)}
	for _, src := range sources {
		count := len(src.Issues)
		summary.Total += count
		summary.BySource[src.Name] = count
		countReadyPriorities(&summary, src.Issues)
	}
	return summary
}

func countReadyPriorities(summary *ReadySummary, issues []*beads.Issue) {
	for _, issue := range issues {
		switch issue.Priority {
		case 0:
			summary.P0Count++
		case 1:
			summary.P1Count++
		case 2:
			summary.P2Count++
		case 3:
			summary.P3Count++
		case 4:
			summary.P4Count++
		}
	}
}

func printReadyResult(result ReadyResult, jsonOutput bool) error {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	if err := printReadyHuman(result); err != nil {
		return err
	}
	return reportReadySourceErrors(result)
}

func reportReadySourceErrors(result ReadyResult) error {
	var failedSources []string
	for _, src := range result.Sources {
		if src.Error != "" {
			failedSources = append(failedSources, src.Name)
		}
	}
	if len(failedSources) == 0 {
		return nil
	}
	if len(failedSources) == len(result.Sources) {
		return fmt.Errorf("all sources failed to load: %s", strings.Join(failedSources, ", "))
	}
	style.PrintWarning("some sources failed to load: %s (results may be incomplete)", strings.Join(failedSources, ", "))
	return nil
}

func printReadyHuman(result ReadyResult) error {
	if result.Summary.Total == 0 {
		fmt.Println("No ready work across town.")
		return nil
	}
	fmt.Printf("%s Ready work across town:\n\n", style.Bold.Render("📋"))
	for _, src := range result.Sources {
		printReadySource(src)
	}
	printReadySummary(result.Summary)
	return nil
}

func printReadySource(src ReadySource) {
	if src.Error != "" {
		fmt.Printf("%s %s\n", style.Dim.Render(src.Name+"/"), style.Warning.Render("(error: "+src.Error+")"))
		return
	}
	count := len(src.Issues)
	if count == 0 {
		fmt.Printf("%s %s\n", style.Dim.Render(src.Name+"/"), style.Dim.Render("(none)"))
		return
	}
	fmt.Printf("%s (%d items)\n", style.Bold.Render(src.Name+"/"), count)
	for _, issue := range src.Issues {
		printReadyIssue(issue)
	}
	fmt.Println()
}

func printReadyIssue(issue *beads.Issue) {
	title := issue.Title
	if len(title) > 60 {
		title = title[:57] + "..."
	}
	fmt.Printf("  [%s] %s %s\n", readyPriorityStyle(issue.Priority), style.Dim.Render(issue.ID), title)
}

func readyPriorityStyle(priority int) string {
	priorityStr := fmt.Sprintf("P%d", priority)
	switch priority {
	case 0, 1:
		return style.Error.Render(priorityStr)
	case 2:
		return style.Warning.Render(priorityStr)
	default:
		return style.Dim.Render(priorityStr)
	}
}

func printReadySummary(summary ReadySummary) {
	parts := readyPriorityParts(summary)
	if len(parts) > 0 {
		fmt.Printf("Total: %d items ready (%s)\n", summary.Total, strings.Join(parts, ", "))
		return
	}
	fmt.Printf("Total: %d items ready\n", summary.Total)
}

func readyPriorityParts(summary ReadySummary) []string {
	var parts []string
	if summary.P0Count > 0 {
		parts = append(parts, fmt.Sprintf("%d P0", summary.P0Count))
	}
	if summary.P1Count > 0 {
		parts = append(parts, fmt.Sprintf("%d P1", summary.P1Count))
	}
	if summary.P2Count > 0 {
		parts = append(parts, fmt.Sprintf("%d P2", summary.P2Count))
	}
	if summary.P3Count > 0 {
		parts = append(parts, fmt.Sprintf("%d P3", summary.P3Count))
	}
	if summary.P4Count > 0 {
		parts = append(parts, fmt.Sprintf("%d P4", summary.P4Count))
	}
	return parts
}

// getFormulaNames reads the formulas directory and returns a set of formula names.
// Formula names are derived from filenames by removing the ".formula.toml" suffix.
func getFormulaNames(beadsPath string) map[string]bool {
	formulasDir := filepath.Join(beadsPath, "formulas")
	entries, err := os.ReadDir(formulasDir)
	if err != nil {
		return nil
	}

	names := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".formula.toml") {
			// Remove suffix to get formula name
			formulaName := strings.TrimSuffix(name, ".formula.toml")
			names[formulaName] = true
		}
	}
	return names
}

// filterFormulaScaffolds removes formula scaffold issues from the list.
// Formula scaffolds are issues whose ID matches a formula name exactly
// or starts with "<formula-name>." (step scaffolds).
func filterFormulaScaffolds(issues []*beads.Issue, formulaNames map[string]bool) []*beads.Issue {
	if formulaNames == nil || len(formulaNames) == 0 {
		return issues
	}

	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		// Check if this is a formula scaffold (exact match)
		if formulaNames[issue.ID] {
			continue
		}

		// Check if this is a step scaffold (formula-name.step-id)
		if idx := strings.Index(issue.ID, "."); idx > 0 {
			prefix := issue.ID[:idx]
			if formulaNames[prefix] {
				continue
			}
		}

		filtered = append(filtered, issue)
	}
	return filtered
}

// getWispIDs queries Dolt for wisp IDs that shouldn't appear in ready work.
// Wisps are ephemeral issues (wisp/ephemeral flag) used for operational workflows.
// This is a defense-in-depth exclusion - bd ready should already filter wisps,
// but we double-check at the display layer to ensure operational work doesn't leak.
func getWispIDs(beadsPath string) map[string]bool {
	output, err := BdCmd("mol", "wisp", "list", "--json").
		Dir(beadsPath).
		StripBeadsDir().
		Stderr(io.Discard).
		Output()
	if err != nil {
		return nil // Wisp table may not exist or Dolt unavailable
	}

	// bd mol wisp list --json returns {"wisps": [...], "count": N, ...}
	var wrapper struct {
		Wisps []struct {
			ID string `json:"id"`
		} `json:"wisps"`
	}
	if err := json.Unmarshal(output, &wrapper); err != nil {
		return nil
	}

	wispIDs := make(map[string]bool, len(wrapper.Wisps))
	for _, w := range wrapper.Wisps {
		wispIDs[w.ID] = true
	}
	return wispIDs
}

// filterIdentityBeads removes agent, role, and rig identity beads from the list.
// These are status trackers, not actionable work items.
//
// Since bd ready --json doesn't include labels, we filter by:
//   - issue_type "agent" (agent lifecycle beads)
//   - Labels if present (gt:agent, gt:role, gt:rig)
//   - ID suffix "-role" (role definition beads like hq-crew-role)
//   - ID prefix matching "<prefix>-rig-" (rig identity beads like gt-rig-gastown)
func filterIdentityBeads(issues []*beads.Issue) []*beads.Issue {
	identityLabels := map[string]bool{
		"gt:agent": true,
		"gt:role":  true,
		"gt:rig":   true,
	}

	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		// Filter by issue_type (agent beads)
		if beads.IsAgentBead(issue) {
			continue
		}

		// Filter by labels (when available)
		skip := false
		for _, label := range issue.Labels {
			if identityLabels[label] {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Filter role definition beads (IDs ending in "-role")
		if strings.HasSuffix(issue.ID, "-role") {
			continue
		}

		// Filter rig identity beads (IDs containing "-rig-")
		if strings.Contains(issue.ID, "-rig-") {
			continue
		}

		filtered = append(filtered, issue)
	}
	return filtered
}

// filterReadyIssuesByRoute keeps only issues whose prefix route matches the
// source that reported them. Ready rows are actionable: the dashboard renders a
// Sling button for each row, so the displayed ID must resolve through the same
// routes.jsonl path that produced it.
func filterReadyIssuesByRoute(townRoot, source string, issues []*beads.Issue) []*beads.Issue {
	if townRoot == "" {
		return issues
	}

	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		if readyIssueRoutesToSource(townRoot, source, issue.ID) {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

func readyIssueRoutesToSource(townRoot, source, issueID string) bool {
	prefix := beads.ExtractPrefix(issueID)
	if prefix == "" {
		return false
	}

	routePath := beads.GetRigPathForPrefix(townRoot, prefix)
	if routePath == "" {
		return false
	}

	if source == "town" {
		return routePath == townRoot
	}

	return beads.GetRigNameForPrefix(townRoot, prefix) == source
}

// filterWisps removes wisp issues from the list.
// Wisps are ephemeral operational work that shouldn't appear in ready work.
func filterWisps(issues []*beads.Issue, wispIDs map[string]bool) []*beads.Issue {
	if wispIDs == nil || len(wispIDs) == 0 {
		return issues
	}

	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		if !wispIDs[issue.ID] {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}
