package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/formula"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
)

type convoyFormulaRun struct {
	f                 *formula.Formula
	formulaName       string
	targetRig         string
	opts              formulaRunOptions
	townBeads         string
	rigPrefix         string
	rigBeadsDir       string
	convoyID          string
	reviewID          string
	targetDescription string
	prTitle           string
	changedFiles      []map[string]interface{}
	setVars           map[string]interface{}
	outputDir         string
}

func executeConvoyFormula(f *formula.Formula, formulaName, targetRig string, opts formulaRunOptions) error {
	fmt.Printf("%s Executing convoy formula: %s\n\n",
		style.Bold.Render("🚚"), formulaName)
	c, err := beginConvoyFormulaRun(f, formulaName, targetRig, opts)
	if err != nil {
		return err
	}
	if err := createConvoyFormulaBead(c); err != nil {
		return err
	}
	fillConvoyRunContext(c)
	createConvoyOutputDir(c)
	legBeads := createConvoyLegBeads(c)
	synthesisBeadID := createConvoySynthesisBead(c, legBeads)
	slingCount := slingConvoyLegs(c, legBeads)
	printConvoyDispatched(c.convoyID, slingCount, synthesisBeadID)
	return nil
}

func beginConvoyFormulaRun(f *formula.Formula, formulaName, targetRig string, opts formulaRunOptions) (*convoyFormulaRun, error) {
	townBeads, rigPrefix, rigBeadsDir, err := resolveConvoyBeads(targetRig)
	if err != nil {
		return nil, err
	}
	return &convoyFormulaRun{
		f:           f,
		formulaName: formulaName,
		targetRig:   targetRig,
		opts:        opts,
		townBeads:   townBeads,
		rigPrefix:   rigPrefix,
		rigBeadsDir: rigBeadsDir,
	}, nil
}

func resolveConvoyBeads(targetRig string) (townBeads, rigPrefix, rigBeadsDir string, err error) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return "", "", "", fmt.Errorf("finding town root: %w", err)
	}
	townBeads = filepath.Join(townRoot, ".beads")
	rigPrefix = beads.GetPrefixForRig(townRoot, targetRig)
	rigBeadsDir = townBeads
	if rigPrefix == "hq" {
		return townBeads, rigPrefix, rigBeadsDir, nil
	}
	routes, _ := beads.LoadRoutes(townBeads)
	for _, r := range routes {
		parts := strings.SplitN(r.Path, "/", 2)
		if len(parts) > 0 && parts[0] == targetRig {
			rigBeadsDir = filepath.Join(townRoot, r.Path, ".beads")
			break
		}
	}
	return townBeads, rigPrefix, rigBeadsDir, nil
}

func createConvoyFormulaBead(c *convoyFormulaRun) error {
	c.convoyID = fmt.Sprintf("%s-cv-%s", c.rigPrefix, generateFormulaShortID())
	convoyTitle := fmt.Sprintf("%s: %s", c.formulaName, c.f.Description)
	if len(convoyTitle) > 80 {
		convoyTitle = convoyTitle[:77] + "..."
	}
	description := fmt.Sprintf("Formula convoy: %s\n\nLegs: %d\nRig: %s",
		c.formulaName, len(c.f.Legs), c.targetRig)
	if c.opts.pr > 0 {
		description += fmt.Sprintf("\nPR: #%d", c.opts.pr)
	}
	if beads.IsFlagLikeTitle(convoyTitle) {
		return fmt.Errorf("refusing to create formula convoy: title %q looks like a CLI flag", convoyTitle)
	}
	createArgs := []string{
		"create",
		"--type=task",
		"--id=" + c.convoyID,
		"--title=" + convoyTitle,
		"--description=" + description,
		"--labels=gt:convoy",
	}
	if beads.NeedsForceForID(c.convoyID) {
		createArgs = append(createArgs, "--force")
	}
	createCmd := beads.Spawn(createArgs...)
	createCmd.Dir = c.townBeads
	createCmd.Stderr = os.Stderr
	if err := createCmd.Run(); err != nil {
		return fmt.Errorf("creating convoy bead: %w", err)
	}
	fmt.Printf("%s Created convoy: %s\n", style.Bold.Render("✓"), c.convoyID)
	return nil
}

func fillConvoyRunContext(c *convoyFormulaRun) {
	c.reviewID = generateFormulaShortID()
	c.targetDescription = "local files"
	if c.opts.pr > 0 {
		c.targetDescription = fmt.Sprintf("PR #%d", c.opts.pr)
		c.prTitle, c.changedFiles = fetchPRInfo(c.opts.pr)
	}
	c.setVars = parseSetVars(c.opts.set)
}

func createConvoyOutputDir(c *convoyFormulaRun) {
	if c.f.Output == nil || c.f.Output.Directory == "" {
		return
	}
	dirCtx := formulaTemplateContext(c.formulaName, c.targetDescription, c.reviewID,
		c.opts.pr, c.prTitle, c.changedFiles, c.opts.files, c.setVars)
	c.outputDir = renderTemplateOrDefault(c.f.Output.Directory, dirCtx, ".reviews/"+c.reviewID)
	if err := os.MkdirAll(c.outputDir, 0755); err != nil {
		fmt.Printf("%s Failed to create output directory %s: %v\n",
			style.Dim.Render("Warning:"), c.outputDir, err)
		return
	}
	fmt.Printf("  %s Output directory: %s\n", style.Dim.Render("📁"), c.outputDir)
}

func createConvoyLegBeads(c *convoyFormulaRun) map[string]string {
	legBeads := make(map[string]string)
	for _, leg := range c.f.Legs {
		if beadID, ok := createConvoyLegBead(c, leg); ok {
			legBeads[leg.ID] = beadID
		}
	}
	return legBeads
}

func createConvoyLegBead(c *convoyFormulaRun, leg formula.Leg) (string, bool) {
	legBeadID := fmt.Sprintf("%s-leg-%s", c.rigPrefix, generateFormulaShortID())
	legDesc := convoyLegDescription(c, leg)
	legArgs := []string{
		"create",
		"--type=task",
		"--id=" + legBeadID,
		"--title=" + leg.Title,
		"--description=" + legDesc,
	}
	if beads.NeedsForceForID(legBeadID) {
		legArgs = append(legArgs, "--force")
	}
	if err := BdCmd(legArgs...).
		WithAutoCommit().
		Dir(c.rigBeadsDir).
		Stderr(os.Stderr).
		Run(); err != nil {
		fmt.Printf("%s Failed to create leg bead for %s: %v\n",
			style.Dim.Render("Warning:"), leg.ID, err)
		return "", false
	}
	if err := addTrackingRelationFn(c.townBeads, c.convoyID, legBeadID); err != nil {
		fmt.Printf("%s Failed to track leg %s: %v\n",
			style.Dim.Render("Warning:"), leg.ID, err)
	}
	fmt.Printf("  %s Created leg: %s (%s)\n", style.Dim.Render("○"), leg.ID, legBeadID)
	return legBeadID, true
}

func convoyLegDescription(c *convoyFormulaRun, leg formula.Leg) string {
	if c.f.Prompts == nil {
		return leg.Description
	}
	basePrompt, ok := c.f.Prompts["base"]
	if !ok {
		return leg.Description
	}
	legCtx := formulaTemplateContext(c.formulaName, c.targetDescription, c.reviewID,
		c.opts.pr, c.prTitle, c.changedFiles, c.opts.files, c.setVars)
	legCtx["leg"] = map[string]interface{}{
		"id":          leg.ID,
		"title":       leg.Title,
		"focus":       leg.Focus,
		"description": leg.Description,
	}
	if c.f.Output != nil {
		legPattern := renderTemplateOrDefault(c.f.Output.LegPattern, legCtx, leg.ID+"-findings.md")
		legCtx["output_path"] = filepath.Join(c.outputDir, legPattern)
		addOutputTemplateContext(legCtx, c.outputDir, c.f.Output.Synthesis)
	}
	renderedPrompt, err := renderTemplate(basePrompt, legCtx)
	if err != nil {
		fmt.Printf("%s Failed to render template for %s: %v\n",
			style.Dim.Render("Warning:"), leg.ID, err)
		renderedPrompt = basePrompt
	}
	return fmt.Sprintf("%s\n\n---\nBase Prompt:\n%s", leg.Description, renderedPrompt)
}

func createConvoySynthesisBead(c *convoyFormulaRun, legBeads map[string]string) string {
	if c.f.Synthesis == nil {
		return ""
	}
	synthesisBeadID := fmt.Sprintf("%s-syn-%s", c.rigPrefix, generateFormulaShortID())
	synArgs := []string{
		"create",
		"--type=task",
		"--id=" + synthesisBeadID,
		"--title=" + c.f.Synthesis.Title,
		"--description=" + convoySynthesisDescription(c),
	}
	if beads.NeedsForceForID(synthesisBeadID) {
		synArgs = append(synArgs, "--force")
	}
	if err := BdCmd(synArgs...).
		WithAutoCommit().
		Dir(c.rigBeadsDir).
		Stderr(os.Stderr).
		Run(); err != nil {
		fmt.Printf("%s Failed to create synthesis bead: %v\n",
			style.Dim.Render("Warning:"), err)
		return ""
	}
	_ = addTrackingRelationFn(c.townBeads, c.convoyID, synthesisBeadID)
	for _, legBeadID := range legBeads {
		_ = BdCmd("dep", "add", synthesisBeadID, legBeadID).
			WithAutoCommit().
			Dir(c.rigBeadsDir).
			Run()
	}
	fmt.Printf("  %s Created synthesis: %s\n", style.Dim.Render("★"), synthesisBeadID)
	return synthesisBeadID
}

func convoySynthesisDescription(c *convoyFormulaRun) string {
	synDesc := c.f.Synthesis.Description
	if synDesc == "" {
		synDesc = "Synthesize findings from all legs into unified output"
	}
	synCtx := formulaTemplateContext(c.formulaName, c.targetDescription, c.reviewID,
		c.opts.pr, c.prTitle, c.changedFiles, c.opts.files, c.setVars)
	if c.f.Output != nil {
		addOutputTemplateContext(synCtx, c.outputDir, c.f.Output.Synthesis)
	}
	rendered, err := renderTemplate(synDesc, synCtx)
	if err != nil {
		fmt.Printf("%s Failed to render synthesis template: %v\n",
			style.Dim.Render("Warning:"), err)
		return synDesc
	}
	return rendered
}

func slingConvoyLegs(c *convoyFormulaRun, legBeads map[string]string) int {
	fmt.Printf("\n%s Dispatching legs to polecats...\n\n", style.Bold.Render("→"))
	slingCount := 0
	for _, leg := range c.f.Legs {
		if slingConvoyLeg(c, leg, legBeads) {
			slingCount++
		}
	}
	return slingCount
}

func slingConvoyLeg(c *convoyFormulaRun, leg formula.Leg, legBeads map[string]string) bool {
	legBeadID, ok := legBeads[leg.ID]
	if !ok {
		return false
	}
	_ = fmt.Sprintf("Convoy leg: %s\nFocus: %s", leg.Title, leg.Focus)
	legAgent := resolveFormulaLegAgent(leg.Agent, c.opts.agent, c.f.Agent)
	slingArgs := buildConvoyLegSlingArgs(legBeadID, c.targetRig, leg.Description, leg.Title, legAgent, leg.ReviewOnly || c.f.ReviewOnly)
	slingCmd := exec.Command("gt", slingArgs...)
	slingCmd.Stdout = os.Stdout
	slingCmd.Stderr = os.Stderr
	if err := slingCmd.Run(); err != nil {
		fmt.Printf("%s Failed to sling leg %s: %v\n",
			style.Dim.Render("Warning:"), leg.ID, err)
		commentArgs := []string{"comments", "add", legBeadID, fmt.Sprintf("Failed to sling: %v", err)}
		commentCmd := beads.Spawn(commentArgs...)
		commentCmd.Dir = c.townBeads
		_ = commentCmd.Run()
		return false
	}
	return true
}

func printConvoyDispatched(convoyID string, slingCount int, synthesisBeadID string) {
	fmt.Printf("\n%s Convoy dispatched!\n", style.Bold.Render("✓"))
	fmt.Printf("  Convoy:  %s\n", convoyID)
	fmt.Printf("  Legs:    %d dispatched\n", slingCount)
	if synthesisBeadID != "" {
		fmt.Printf("  Synthesis: %s (blocked until legs complete)\n", synthesisBeadID)
	}
	fmt.Printf("\n  Track progress: gt convoy status %s\n", convoyID)
}

func buildConvoyLegSlingArgs(beadID, targetRig, description, title, agent string, reviewOnly bool) []string {
	args := []string{
		"sling", beadID, targetRig,
		"-a", description,
		"-s", title,
		"--no-convoy",
	}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	if reviewOnly {
		args = append(args, "--review-only")
	}
	return args
}
