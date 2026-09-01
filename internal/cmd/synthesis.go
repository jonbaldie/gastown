package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/formula"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var synthesisCmd = &cobra.Command{
	Use:     "synthesis",
	Aliases: []string{"synth"},
	GroupID: GroupWork,
	Short:   "Manage convoy synthesis steps",
	RunE:    requireSubcommand,
	Long: `Manage synthesis steps for convoy formulas.

Synthesis is the final step in a convoy workflow that combines outputs
from all parallel legs into a unified deliverable.

Commands:
  start     Start synthesis for a convoy (checks all legs complete)
  status    Show synthesis readiness and leg outputs
  close     Close convoy after synthesis complete

Examples:
  gt synthesis status hq-cv-abc     # Check if ready for synthesis
  gt synthesis start hq-cv-abc      # Start synthesis step
  gt synthesis close hq-cv-abc      # Close convoy after synthesis`,
}

var synthesisStartCmd = &cobra.Command{
	Use:   "start <convoy-id>",
	Short: "Start synthesis for a convoy",
	Long: `Start the synthesis step for a convoy.

This command:
  1. Verifies all legs are complete
  2. Collects outputs from all legs
  3. Creates a synthesis bead with combined context
  4. Slings the synthesis to a polecat

Options:
  --rig=NAME      Target rig for synthesis polecat (default: current)
  --review-id=ID  Override review ID for output paths
  --force         Start synthesis even if some legs incomplete
  --dry-run       Show what would happen without executing`,
	Args: cobra.ExactArgs(1),
	RunE: runSynthesisStart,
}

var synthesisStatusCmd = &cobra.Command{
	Use:   "status <convoy-id>",
	Short: "Show synthesis readiness",
	Long: `Show whether a convoy is ready for synthesis.

Displays:
  - Convoy metadata
  - Leg completion status
  - Available leg outputs
  - Formula synthesis configuration`,
	Args: cobra.ExactArgs(1),
	RunE: runSynthesisStatus,
}

var synthesisCloseCmd = &cobra.Command{
	Use:   "close <convoy-id>",
	Short: "Close convoy after synthesis",
	Long: `Close a convoy after synthesis is complete.

This marks the convoy as complete and triggers any configured notifications.`,
	Args: cobra.ExactArgs(1),
	RunE: runSynthesisClose,
}

func init() {
	// Start flags
	synthesisStartCmd.Flags().String("rig", "", "Target rig for synthesis polecat")
	synthesisStartCmd.Flags().Bool("dry-run", false, "Preview execution")
	synthesisStartCmd.Flags().Bool("force", false, "Start even if legs incomplete")
	synthesisStartCmd.Flags().String("review-id", "", "Override review ID")

	// Add subcommands
	synthesisCmd.AddCommand(synthesisStartCmd)
	synthesisCmd.AddCommand(synthesisStatusCmd)
	synthesisCmd.AddCommand(synthesisCloseCmd)

	rootCmd.AddCommand(synthesisCmd)
}

// LegOutput represents collected output from a convoy leg.
type LegOutput struct {
	LegID    string `json:"leg_id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	FilePath string `json:"file_path,omitempty"`
	Content  string `json:"content,omitempty"`
	HasFile  bool   `json:"has_file"`
}

// ConvoyMeta holds metadata about a convoy including its formula.
type ConvoyMeta struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	Formula     string   `json:"formula,omitempty"`      // Formula name
	FormulaPath string   `json:"formula_path,omitempty"` // Path to formula file
	ReviewID    string   `json:"review_id,omitempty"`    // Review ID for output paths
	LegIssues   []string `json:"leg_issues,omitempty"`   // Tracked leg issue IDs
}

// runSynthesisStart implements gt synthesis start.
func runSynthesisStart(cmd *cobra.Command, args []string) error {
	targetRig := commandStringFlag(cmd, "rig")
	dryRun := commandBoolFlag(cmd, "dry-run")
	force := commandBoolFlag(cmd, "force")
	reviewID := commandStringFlag(cmd, "review-id")
	convoyID := args[0]

	// Get convoy metadata
	meta, err := getConvoyMeta(convoyID)
	if err != nil {
		return fmt.Errorf("getting convoy metadata: %w", err)
	}

	fmt.Printf("%s Checking synthesis readiness for %s...\n", style.Bold.Render("🔬"), convoyID)
	f, err := loadSynthesisFormula(meta)
	if err != nil {
		return fmt.Errorf("loading formula: %w", err)
	}

	// Check leg completion status
	legOutputs, allComplete, err := collectLegOutputs(meta, f)
	if err != nil {
		return fmt.Errorf("collecting leg outputs: %w", err)
	}

	printSynthesisReadiness(legOutputs)
	if !allComplete && !force {
		printIncompleteSynthesisLegs(legOutputs)
		return nil
	}

	reviewID = resolveSynthesisReviewID(reviewID, meta, convoyID)
	targetRig = resolveSynthesisTargetRig(targetRig)

	if dryRun {
		printSynthesisDryRun(convoyID, reviewID, targetRig, legOutputs, f)
		return nil
	}
	return executeSynthesisStart(convoyID, reviewID, targetRig, meta, f, legOutputs)
}

func loadSynthesisFormula(meta *ConvoyMeta) (*formula.Formula, error) {
	if meta.FormulaPath != "" {
		return formula.ParseFile(meta.FormulaPath)
	}
	if meta.Formula == "" {
		return nil, nil
	}
	formulaPath, err := findFormula(meta.Formula)
	if err != nil {
		return nil, nil
	}
	return formula.ParseFile(formulaPath)
}

func printSynthesisReadiness(legOutputs []LegOutput) {
	fmt.Printf("  Legs: %d/%d complete\n", countCompletedLegs(legOutputs), len(legOutputs))
}

func countCompletedLegs(legOutputs []LegOutput) int {
	completedCount := 0
	for _, leg := range legOutputs {
		if leg.Status == "closed" {
			completedCount++
		}
	}
	return completedCount
}

func printIncompleteSynthesisLegs(legOutputs []LegOutput) {
	fmt.Printf("\n%s Not all legs complete. Use --force to proceed anyway.\n", style.Warning.Render("⚠"))
	fmt.Printf("\nIncomplete legs:\n")
	for _, leg := range legOutputs {
		if leg.Status != "closed" {
			fmt.Printf("  ○ %s: %s [%s]\n", leg.LegID, leg.Title, leg.Status)
		}
	}
}

func resolveSynthesisReviewID(reviewID string, meta *ConvoyMeta, convoyID string) string {
	if reviewID != "" {
		return reviewID
	}
	if meta.ReviewID != "" {
		return meta.ReviewID
	}
	return strings.TrimPrefix(convoyID, "hq-cv-")
}

func resolveSynthesisTargetRig(targetRig string) string {
	if targetRig != "" {
		return targetRig
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err == nil {
		if rigName, _, rigErr := findCurrentRig(townRoot); rigErr == nil && rigName != "" {
			return rigName
		}
	}
	return "gastown"
}

func printSynthesisDryRun(convoyID, reviewID, targetRig string, legOutputs []LegOutput, f *formula.Formula) {
	fmt.Printf("\n%s Would start synthesis:\n", style.Dim.Render("[dry-run]"))
	fmt.Printf("  Convoy:    %s\n", convoyID)
	fmt.Printf("  Review ID: %s\n", reviewID)
	fmt.Printf("  Target:    %s\n", targetRig)
	fmt.Printf("  Legs:      %d outputs collected\n", len(legOutputs))
	if f != nil && f.Synthesis != nil {
		fmt.Printf("  Synthesis: %s\n", f.Synthesis.Title)
	}
}

func executeSynthesisStart(convoyID, reviewID, targetRig string, meta *ConvoyMeta, f *formula.Formula, legOutputs []LegOutput) error {
	synthesisID, err := createSynthesisBead(convoyID, meta, f, legOutputs, reviewID)
	if err != nil {
		return fmt.Errorf("creating synthesis bead: %w", err)
	}
	fmt.Printf("%s Created synthesis bead: %s\n", style.Bold.Render("✓"), synthesisID)

	// Sling to target rig
	fmt.Printf("  Slinging to %s...\n", targetRig)
	if err := slingSynthesis(synthesisID, targetRig); err != nil {
		return fmt.Errorf("slinging synthesis: %w", err)
	}

	fmt.Printf("%s Synthesis started\n", style.Bold.Render("✓"))
	fmt.Printf("  Monitor: gt convoy status %s\n", convoyID)

	return nil
}

// runSynthesisStatus implements gt synthesis status.
func runSynthesisStatus(_ *cobra.Command, args []string) error {
	convoyID := args[0]

	meta, err := getConvoyMeta(convoyID)
	if err != nil {
		return fmt.Errorf("getting convoy metadata: %w", err)
	}

	f, _ := loadSynthesisFormula(meta)

	// Collect leg outputs
	legOutputs, allComplete, err := collectLegOutputs(meta, f)
	if err != nil {
		return fmt.Errorf("collecting leg outputs: %w", err)
	}

	printSynthesisStatus(convoyID, meta, legOutputs, allComplete, f)
	return nil
}

func printSynthesisStatus(convoyID string, meta *ConvoyMeta, legOutputs []LegOutput, allComplete bool, f *formula.Formula) {
	fmt.Printf("🚚 %s %s\n\n", style.Bold.Render(convoyID+":"), meta.Title)
	fmt.Printf("  Status: %s\n", formatConvoyStatus(meta.Status))
	if meta.Formula != "" {
		fmt.Printf("  Formula: %s\n", meta.Formula)
	}
	printSynthesisLegStatus(legOutputs)
	printSynthesisReadinessStatus(convoyID, legOutputs, allComplete)
	printSynthesisConfig(f)
}

func printSynthesisLegStatus(legOutputs []LegOutput) {
	fmt.Printf("\n  %s\n", style.Bold.Render("Legs:"))
	for _, leg := range legOutputs {
		status := "○"
		if leg.Status == "closed" {
			status = "✓"
		}
		fileStatus := ""
		if leg.HasFile {
			fileStatus = style.Dim.Render(" (output: ✓)")
		}
		fmt.Printf("    %s %s: %s [%s]%s\n", status, leg.LegID, leg.Title, leg.Status, fileStatus)
	}
}

func printSynthesisReadinessStatus(convoyID string, legOutputs []LegOutput, allComplete bool) {
	fmt.Printf("\n  %s\n", style.Bold.Render("Synthesis:"))
	if allComplete {
		fmt.Printf("    %s Ready - all legs complete\n", style.Success.Render("✓"))
		fmt.Printf("    Run: gt synthesis start %s\n", convoyID)
		return
	}
	completedCount := countCompletedLegs(legOutputs)
	fmt.Printf("    %s Waiting - %d/%d legs complete\n", style.Warning.Render("○"), completedCount, len(legOutputs))
}

func printSynthesisConfig(f *formula.Formula) {
	if f == nil || f.Synthesis == nil {
		return
	}
	fmt.Printf("\n  %s\n", style.Bold.Render("Synthesis Config:"))
	fmt.Printf("    Title: %s\n", f.Synthesis.Title)
	if f.Output != nil && f.Output.Synthesis != "" {
		fmt.Printf("    Output: %s\n", f.Output.Synthesis)
	}
}

// runSynthesisClose implements gt synthesis close.
func runSynthesisClose(_ *cobra.Command, args []string) error {
	convoyID := args[0]

	townBeads, err := getTownBeadsDir()
	if err != nil {
		return err
	}

	// Read convoy to validate lifecycle state before closing
	showArgs := []string{"show", convoyID, "--json"}
	showCmd := beads.Spawn(showArgs...)
	showCmd.Dir = townBeads
	var showOut bytes.Buffer
	showCmd.Stdout = &showOut
	if err := showCmd.Run(); err != nil {
		return fmt.Errorf("reading convoy '%s': %w", convoyID, err)
	}
	var convoys []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(showOut.Bytes(), &convoys); err != nil || len(convoys) == 0 {
		return fmt.Errorf("parsing convoy '%s': invalid response", convoyID)
	}
	status := convoys[0].Status

	if err := ensureKnownConvoyStatus(status); err != nil {
		return fmt.Errorf("convoy '%s' has invalid lifecycle state: %w", convoyID, err)
	}

	// Idempotent: if already closed, just report it
	if normalizeConvoyStatus(status) == convoyStatusClosed {
		fmt.Printf("%s Convoy %s is already closed\n", style.Dim.Render("○"), convoyID)
		return nil
	}

	// Close the convoy
	closeArgs := []string{"close", convoyID, "--reason=synthesis complete"}
	if sessionID := runtime.SessionIDFromEnv(); sessionID != "" {
		closeArgs = append(closeArgs, "--session="+sessionID)
	}
	closeCmd := beads.Spawn(closeArgs...)
	closeCmd.Dir = townBeads
	closeCmd.Stderr = os.Stderr

	if err := closeCmd.Run(); err != nil {
		return fmt.Errorf("closing convoy: %w", err)
	}

	fmt.Printf("%s Convoy closed: %s\n", style.Bold.Render("✓"), convoyID)

	// TODO: Trigger notification if configured
	// Parse description for "Notify: <address>" and send mail

	return nil
}

type convoyShowRecord struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	Description string   `json:"description"`
	Type        string   `json:"issue_type"`
	Labels      []string `json:"labels"`
}

// getConvoyMeta retrieves convoy metadata from beads.
func getConvoyMeta(convoyID string) (*ConvoyMeta, error) {
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return nil, err
	}
	convoy, err := showConvoyRecord(townBeads, convoyID)
	if err != nil {
		return nil, err
	}
	if !isConvoyIssue(convoy.Type, convoy.Labels) {
		return nil, fmt.Errorf("'%s' is not a convoy", convoyID)
	}
	meta := convoyMetaFromRecord(convoy)
	if err := attachConvoyLegIssues(townBeads, convoyID, meta); err != nil {
		return nil, err
	}
	return meta, nil
}

func showConvoyRecord(townBeads, convoyID string) (*convoyShowRecord, error) {
	showCmd := beads.Spawn("show", convoyID, "--json")
	showCmd.Dir = townBeads
	var stdout bytes.Buffer
	showCmd.Stdout = &stdout
	if err := showCmd.Run(); err != nil {
		return nil, fmt.Errorf("convoy '%s' not found", convoyID)
	}
	var convoys []convoyShowRecord
	if err := json.Unmarshal(stdout.Bytes(), &convoys); err != nil {
		return nil, fmt.Errorf("parsing convoy data: %w", err)
	}
	if len(convoys) == 0 {
		return nil, fmt.Errorf("'%s' is not a convoy", convoyID)
	}
	return &convoys[0], nil
}

func convoyMetaFromRecord(convoy *convoyShowRecord) *ConvoyMeta {
	meta := &ConvoyMeta{
		ID:     convoy.ID,
		Title:  convoy.Title,
		Status: convoy.Status,
	}
	applyConvoyDescriptionFields(meta, convoy.Description)
	return meta
}

func applyConvoyDescriptionFields(meta *ConvoyMeta, description string) {
	for _, line := range strings.Split(description, "\n") {
		applyConvoyDescriptionLine(meta, strings.TrimSpace(line))
	}
}

func applyConvoyDescriptionLine(meta *ConvoyMeta, line string) {
	colonIdx := strings.Index(line, ":")
	if colonIdx == -1 {
		return
	}
	key := strings.ToLower(strings.TrimSpace(line[:colonIdx]))
	value := strings.TrimSpace(line[colonIdx+1:])
	switch key {
	case "formula":
		meta.Formula = value
	case "formula_path", "formula-path":
		meta.FormulaPath = value
	case "review_id", "review-id":
		meta.ReviewID = value
	}
}

func attachConvoyLegIssues(townBeads, convoyID string, meta *ConvoyMeta) error {
	tracked, err := getTrackedIssues(townBeads, convoyID)
	if err != nil {
		return fmt.Errorf("getting tracked issues for convoy %s: %w", convoyID, err)
	}
	for _, t := range tracked {
		meta.LegIssues = append(meta.LegIssues, t.ID)
	}
	return nil
}

// collectLegOutputs gathers outputs from all convoy legs.
func collectLegOutputs(meta *ConvoyMeta, f *formula.Formula) ([]LegOutput, bool, error) { //nolint:unparam // error return kept for future use
	outputs, allComplete := collectTrackedLegOutputs(meta)
	if f != nil && f.Output != nil && meta.ReviewID != "" {
		outputs = attachFormulaLegFiles(outputs, f, meta.ReviewID)
	}
	return outputs, allComplete, nil
}

func collectTrackedLegOutputs(meta *ConvoyMeta) ([]LegOutput, bool) {
	var outputs []LegOutput
	allComplete := true
	for _, issueID := range meta.LegIssues {
		output := trackedLegOutput(issueID)
		if output.Status != "closed" {
			allComplete = false
		}
		outputs = append(outputs, output)
	}
	return outputs, allComplete
}

func trackedLegOutput(issueID string) LegOutput {
	output := LegOutput{
		LegID: issueID,
		Title: "(unknown)",
	}
	details := getIssueDetails(issueID)
	if details != nil {
		output.Title = details.Title
		output.Status = details.Status
	}
	return output
}

func attachFormulaLegFiles(outputs []LegOutput, f *formula.Formula, reviewID string) []LegOutput {
	for _, leg := range f.Legs {
		outputPath := expandOutputPath(f.Output.Directory, f.Output.LegPattern, reviewID, leg.ID)
		content, err := os.ReadFile(outputPath)
		if err != nil {
			continue
		}
		outputs = attachOrAppendLegFile(outputs, leg, outputPath, string(content))
	}
	return outputs
}

func attachOrAppendLegFile(outputs []LegOutput, leg formula.Leg, outputPath, content string) []LegOutput {
	for i := range outputs {
		if outputs[i].LegID == leg.ID {
			outputs[i].FilePath = outputPath
			outputs[i].Content = content
			outputs[i].HasFile = true
			return outputs
		}
	}
	return append(outputs, LegOutput{
		LegID:    leg.ID,
		Title:    leg.Title,
		Status:   "closed", // If file exists, assume complete
		FilePath: outputPath,
		Content:  content,
		HasFile:  true,
	})
}

// expandOutputPath expands template variables in output paths.
// Supports Go template output syntax plus legacy bare placeholders.
func expandOutputPath(directory, pattern, reviewID, legID string) string {
	dir := expandOutputTemplate(directory, reviewID, legID)
	file := expandOutputTemplate(pattern, reviewID, legID)
	return filepath.Join(dir, file)
}

func expandOutputTemplate(tmplText, reviewID, legID string) string {
	ctx := map[string]interface{}{
		"review_id": reviewID,
		"leg": map[string]interface{}{
			"id": legID,
		},
	}
	if rendered, err := renderTemplate(tmplText, ctx); err == nil {
		return rendered
	}

	text := strings.ReplaceAll(tmplText, "{{review_id}}", reviewID)
	return strings.ReplaceAll(text, "{{leg.id}}", legID)
}

// createSynthesisBead creates a bead for the synthesis step.
func createSynthesisBead(convoyID string, meta *ConvoyMeta, f *formula.Formula,
	legOutputs []LegOutput, reviewID string) (string, error) {
	title := synthesisBeadTitle(meta, f)
	if beads.IsFlagLikeTitle(title) {
		return "", fmt.Errorf("refusing to create synthesis bead: title %q looks like a CLI flag", title)
	}
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return "", err
	}
	beadID, err := createTownTaskBead(townBeads, title, synthesisBeadDescription(convoyID, meta, f, legOutputs, reviewID))
	if err != nil {
		return "", err
	}
	_ = addTrackingRelationFn(townBeads, convoyID, beadID) // Non-fatal if this fails
	return beadID, nil
}

func synthesisBeadTitle(meta *ConvoyMeta, f *formula.Formula) string {
	if f != nil && f.Synthesis != nil && f.Synthesis.Title != "" {
		return f.Synthesis.Title + ": " + meta.Title
	}
	return "Synthesis: " + meta.Title
}

func synthesisBeadDescription(convoyID string, meta *ConvoyMeta, f *formula.Formula, legOutputs []LegOutput, reviewID string) string {
	var desc strings.Builder
	desc.WriteString(fmt.Sprintf("convoy: %s\n", convoyID))
	desc.WriteString(fmt.Sprintf("review_id: %s\n", reviewID))
	desc.WriteString("\n")
	outputDir, outputSynthesis := synthesisOutputPaths(f, reviewID)
	appendSynthesisInstructions(&desc, meta, f, reviewID, outputDir, outputSynthesis)
	appendSynthesisLegOutputs(&desc, legOutputs)
	appendSynthesisOutputPath(&desc, f, outputDir)
	return desc.String()
}

func synthesisOutputPaths(f *formula.Formula, reviewID string) (string, string) {
	if f == nil || f.Output == nil {
		return "", ""
	}
	return expandOutputTemplate(f.Output.Directory, reviewID, ""), f.Output.Synthesis
}

func appendSynthesisInstructions(desc *strings.Builder, meta *ConvoyMeta, f *formula.Formula, reviewID, outputDir, outputSynthesis string) {
	if f == nil || f.Synthesis == nil || f.Synthesis.Description == "" {
		return
	}
	formulaName := meta.Formula
	if formulaName == "" {
		formulaName = f.Name
	}
	synCtx := formulaTemplateContext(formulaName, meta.Title, reviewID, 0, "", nil, nil, nil)
	synCtx["problem"] = meta.Title
	addOutputTemplateContext(synCtx, outputDir, outputSynthesis)
	synDesc := renderTemplateOrDefault(f.Synthesis.Description, synCtx, f.Synthesis.Description)
	desc.WriteString("## Instructions\n\n")
	desc.WriteString(synDesc)
	desc.WriteString("\n\n")
}

func appendSynthesisLegOutputs(desc *strings.Builder, legOutputs []LegOutput) {
	desc.WriteString("## Leg Outputs\n\n")
	for _, leg := range legOutputs {
		appendOneSynthesisLegOutput(desc, leg)
	}
}

func appendOneSynthesisLegOutput(desc *strings.Builder, leg LegOutput) {
	desc.WriteString(fmt.Sprintf("### %s: %s\n\n", leg.LegID, leg.Title))
	if leg.Content != "" {
		desc.WriteString(leg.Content)
		desc.WriteString("\n\n")
		return
	}
	if leg.FilePath != "" {
		desc.WriteString(fmt.Sprintf("Output file: %s\n\n", leg.FilePath))
		return
	}
	desc.WriteString("(no output available)\n\n")
}

func appendSynthesisOutputPath(desc *strings.Builder, f *formula.Formula, outputDir string) {
	if f == nil || f.Output == nil || f.Output.Synthesis == "" {
		return
	}
	outputPath := filepath.Join(outputDir, f.Output.Synthesis)
	desc.WriteString(fmt.Sprintf("\n## Output\n\nWrite synthesis to: %s\n", outputPath))
}

func createTownTaskBead(townBeads, title, desc string) (string, error) {
	createCmd := beads.Spawn(
		"create",
		"--type=task",
		"--title="+title,
		"--description="+desc,
		"--json",
	)
	createCmd.Dir = townBeads
	var stdout bytes.Buffer
	createCmd.Stdout = &stdout
	createCmd.Stderr = os.Stderr
	if err := createCmd.Run(); err != nil {
		return "", fmt.Errorf("creating synthesis bead: %w", err)
	}
	return parseCreatedBeadID(stdout.Bytes())
}

func parseCreatedBeadID(raw []byte) (string, error) {
	var result struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(raw, &result)
	if err == nil {
		return result.ID, nil
	}
	out := strings.TrimSpace(string(raw))
	if looksLikeIssueID(out) {
		return out, nil
	}
	return "", fmt.Errorf("parsing created bead: %w", err)
}

// slingSynthesis slings the synthesis bead to a rig.
func slingSynthesis(beadID, targetRig string) error {
	slingArgs := []string{"sling", beadID, targetRig}
	slingCmd := exec.Command("gt", slingArgs...)
	slingCmd.Stdout = os.Stdout
	slingCmd.Stderr = os.Stderr

	return slingCmd.Run()
}

// findFormula searches for a formula file by name.
func findFormula(name string) (string, error) {
	// Search paths
	searchPaths := []string{
		".beads/formulas",
	}

	// Add home directory formulas
	if home, err := os.UserHomeDir(); err == nil {
		searchPaths = append(searchPaths, filepath.Join(home, ".beads", "formulas"))
	}

	// Add GT_ROOT formulas if set
	if gtRoot := os.Getenv("GT_ROOT"); gtRoot != "" {
		searchPaths = append(searchPaths, filepath.Join(gtRoot, ".beads", "formulas"))
	}

	// Try each search path
	for _, searchPath := range searchPaths {
		// Try with .formula.toml extension
		path := filepath.Join(searchPath, name+".formula.toml")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}

		// Try with .formula.json extension
		path = filepath.Join(searchPath, name+".formula.json")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("formula '%s' not found", name)
}

// CheckSynthesisReady checks if a convoy is ready for synthesis.
// Returns true if all tracked legs are complete.
func CheckSynthesisReady(convoyID string) (bool, error) {
	meta, err := getConvoyMeta(convoyID)
	if err != nil {
		return false, err
	}

	_, allComplete, err := collectLegOutputs(meta, nil)
	return allComplete, err
}

// TriggerSynthesisIfReady checks convoy status and starts synthesis if ready.
// This can be called by the witness when a leg completes.
func TriggerSynthesisIfReady(convoyID, targetRig string) error {
	ready, err := CheckSynthesisReady(convoyID)
	if err != nil {
		return err
	}
	if !ready {
		return nil // Not ready yet
	}
	fmt.Printf("%s All legs complete, starting synthesis...\n", style.Bold.Render("🔬"))
	return startReadySynthesis(convoyID, targetRig)
}

func startReadySynthesis(convoyID, targetRig string) error {
	meta, err := getConvoyMeta(convoyID)
	if err != nil {
		return err
	}
	f, _ := loadSynthesisFormula(meta)
	legOutputs, _, _ := collectLegOutputs(meta, f)
	reviewID := resolveSynthesisReviewID("", meta, convoyID)
	synthesisID, err := createSynthesisBead(convoyID, meta, f, legOutputs, reviewID)
	if err != nil {
		return fmt.Errorf("creating synthesis bead: %w", err)
	}
	if err := slingSynthesis(synthesisID, targetRig); err != nil {
		return fmt.Errorf("slinging synthesis: %w", err)
	}
	return nil
}
