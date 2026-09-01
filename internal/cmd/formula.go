package cmd

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/formula"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

var formulaCmd = &cobra.Command{
	Use:     "formula",
	Aliases: []string{"formulas"},
	GroupID: GroupWork,
	Short:   "Manage workflow formulas",
	RunE:    requireSubcommand,
	Long: `Manage workflow formulas - reusable molecule templates.

Formulas are TOML/JSON files that define workflows with steps, variables,
and composition rules. They can be "poured" to create molecules or "wisped"
for ephemeral patrol cycles.

Commands:
  list    List available formulas from all search paths
  show    Display formula details (steps, variables, composition)
  run     Execute a formula (pour and dispatch)
  create  Create a new formula template

Search paths (in order):
  1. .beads/formulas/ (project)
  2. ~/.beads/formulas/ (user)
  3. $GT_ROOT/.beads/formulas/ (orchestrator)

Examples:
  gt formula list                    # List all formulas
  gt formula show shiny              # Show formula details
  gt formula run shiny --pr=123      # Run formula on PR #123
  gt formula create my-workflow      # Create new formula template`,
}

var formulaListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available formulas",
	Long: `List available formulas from all search paths.

Searches for formula files (.formula.toml, .formula.json) in:
  1. .beads/formulas/ (project)
  2. ~/.beads/formulas/ (user)
  3. $GT_ROOT/.beads/formulas/ (orchestrator)

Examples:
  gt formula list            # List all formulas
  gt formula list --json     # JSON output`,
	RunE: runFormulaList,
}

var formulaShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Display formula details",
	Long: `Display detailed information about a formula.

Shows:
  - Formula metadata (name, type, description)
  - Variables with defaults and constraints
  - Steps with dependencies
  - Composition rules (extends, aspects)

Examples:
  gt formula show shiny
  gt formula show rule-of-five --json`,
	Args: cobra.ExactArgs(1),
	RunE: runFormulaShow,
}

var formulaRunCmd = &cobra.Command{
	Use:   "run [name]",
	Short: "Execute a formula",
	Long: `Execute a formula by pouring it and dispatching work.

This command:
  1. Looks up the formula by name (or uses default from rig config)
  2. Pours it to create a molecule (or uses existing proto)
  3. Dispatches the molecule to available workers

For PR-based workflows, use --pr to specify the GitHub PR number.

If no formula name is provided, uses the default formula configured in
the rig's settings/config.json under workflow.default_formula.

Options:
  --pr=N        Run formula on GitHub PR #N
  --rig=NAME    Target specific rig (default: inferred from cwd, or sole registered rig)
  --agent=ALIAS Override agent/runtime for all legs (e.g., gemini, codex)
  --dry-run     Show what would happen without executing

Agent precedence (highest to lowest):
  1. Per-leg 'agent' field in formula TOML
  2. --agent CLI flag
  3. Formula-level 'agent' field in formula TOML
  4. Rig/town default agent (fallback)

Examples:
  gt formula run shiny                    # Run formula in current rig
  gt formula run                          # Run default formula from rig config
  gt formula run shiny --pr=123           # Run on PR #123
  gt formula run security-audit --rig=beads  # Run in specific rig
  gt formula run release --dry-run        # Preview execution
  gt formula run code-review --agent=gemini  # All legs use gemini`,
	Args: cobra.MaximumNArgs(1),
	RunE: runFormulaRun,
}

var formulaCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new formula template",
	Long: `Create a new formula template file.

Creates a starter formula file in .beads/formulas/ with the given name.
The template includes common sections that you can customize.

Formula types:
  task      Single-step task formula (default)
  workflow  Multi-step workflow with dependencies
  patrol    Repeating patrol cycle (for wisps)

Examples:
  gt formula create my-task                  # Create task formula
  gt formula create my-workflow --type=workflow
  gt formula create nightly-check --type=patrol`,
	Args: cobra.ExactArgs(1),
	RunE: runFormulaCreate,
}

func init() {
	// List flags
	formulaListCmd.Flags().Bool("json", false, "Output as JSON")

	// Show flags
	formulaShowCmd.Flags().Bool("json", false, "Output as JSON")

	// Run flags
	formulaRunCmd.Flags().Int("pr", 0, "GitHub PR number to run formula on")
	formulaRunCmd.Flags().String("rig", "", "Target rig (default: inferred from cwd, or sole registered rig)")
	formulaRunCmd.Flags().Bool("dry-run", false, "Preview execution without running")
	formulaRunCmd.Flags().String("agent", "", "Override agent/runtime for all legs (e.g., gemini, codex, claude-haiku)")
	formulaRunCmd.Flags().StringSlice("files", nil, "Files to pass to formula legs (available as {{.files}} in templates)")
	formulaRunCmd.Flags().StringSlice("set", nil, "Set input variables as key=value pairs (available as {{.key}} in templates)")

	// Create flags
	formulaCreateCmd.Flags().String("type", "task", "Formula type: task, workflow, or patrol")

	// Add subcommands
	formulaCmd.AddCommand(formulaListCmd)
	formulaCmd.AddCommand(formulaShowCmd)
	formulaCmd.AddCommand(formulaRunCmd)
	formulaCmd.AddCommand(formulaCreateCmd)

	rootCmd.AddCommand(formulaCmd)
}

type formulaRunOptions struct {
	pr     int
	rig    string
	dryRun bool
	agent  string
	files  []string
	set    []string
}

func formulaRunOptionsFromCommand(cmd *cobra.Command) formulaRunOptions {
	return formulaRunOptions{
		pr:     commandIntFlag(cmd, "pr"),
		rig:    commandStringFlag(cmd, "rig"),
		dryRun: commandBoolFlag(cmd, "dry-run"),
		agent:  commandStringFlag(cmd, "agent"),
		files:  commandStringArrayFlag(cmd, "files"),
		set:    commandStringArrayFlag(cmd, "set"),
	}
}

// runFormulaList delegates to bd formula list
func runFormulaList(cmd *cobra.Command, _ []string) error {
	bdArgs := []string{"formula", "list"}
	if commandBoolFlag(cmd, "json") {
		bdArgs = append(bdArgs, "--json")
	}

	bdCmd := beads.Spawn(bdArgs...)
	bdCmd.Stdout = os.Stdout
	bdCmd.Stderr = os.Stderr
	return bdCmd.Run()
}

// runFormulaShow delegates to bd formula show
func runFormulaShow(cmd *cobra.Command, args []string) error {
	formulaName := args[0]
	bdArgs := []string{"formula", "show", formulaName}
	if commandBoolFlag(cmd, "json") {
		bdArgs = append(bdArgs, "--json")
	}

	bdCmd := beads.Spawn(bdArgs...)
	bdCmd.Stdout = os.Stdout
	bdCmd.Stderr = os.Stderr
	return bdCmd.Run()
}

// runFormulaRun executes a formula by spawning a convoy of polecats.
// For convoy-type formulas, it creates a convoy bead, creates leg beads,
// and slings each leg to a separate polecat with leg-specific prompts.
func runFormulaRun(cmd *cobra.Command, args []string) error {
	opts := formulaRunOptionsFromCommand(cmd)
	targetRig, rigPath, err := resolveFormulaRunTarget(opts)
	if err != nil {
		return err
	}
	formulaName, err := resolveFormulaRunName(args, rigPath)
	if err != nil {
		return err
	}
	f, err := loadFormulaForRun(formulaName)
	if err != nil {
		return err
	}
	if opts.dryRun {
		return dryRunFormula(f, formulaName, targetRig, opts)
	}
	return executeLoadedFormula(f, formulaName, targetRig, opts)
}

func resolveFormulaRunTarget(opts formulaRunOptions) (string, string, error) {
	if opts.rig != "" {
		return formulaRunSpecifiedRig(opts.rig)
	}
	return formulaRunInferredRig()
}

func formulaRunSpecifiedRig(targetRig string) (string, string, error) {
	rigPath := ""
	townRoot, err := workspace.FindFromCwd()
	if err == nil && townRoot != "" {
		rigPath = filepath.Join(townRoot, targetRig)
	}
	return targetRig, rigPath, nil
}

func formulaRunInferredRig() (string, string, error) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return "", "", fmt.Errorf("cannot determine target rig: not in a Gas Town workspace; use --rig=NAME")
	}
	rigName, r, rigErr := findCurrentRig(townRoot)
	if rigErr == nil && rigName != "" {
		rigPath := ""
		if r != nil {
			rigPath = r.Path
		}
		return rigName, rigPath, nil
	}
	return autoInferRig(townRoot)
}

func resolveFormulaRunName(args []string, rigPath string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	formulaName := ""
	if rigPath != "" {
		formulaName = config.GetDefaultFormula(rigPath)
	}
	if formulaName == "" {
		return "", fmt.Errorf("no formula specified and no default formula configured\n\nTo set a default formula, add to your rig's settings/config.json:\n  \"workflow\": {\n    \"default_formula\": \"<formula-name>\"\n  }")
	}
	fmt.Printf("%s Using default formula: %s\n", style.Dim.Render("Note:"), formulaName)
	return formulaName, nil
}

func loadFormulaForRun(formulaName string) (*formula.Formula, error) {
	formulaPath, err := findFormulaFile(formulaName)
	if err != nil {
		return nil, fmt.Errorf("finding formula: %w", err)
	}
	f, err := parseFormulaFile(formulaPath)
	if err != nil {
		return nil, fmt.Errorf("parsing formula: %w", err)
	}
	return f, nil
}

func executeLoadedFormula(f *formula.Formula, formulaName, targetRig string, opts formulaRunOptions) error {
	switch f.Type {
	case formula.TypeConvoy:
		return executeConvoyFormula(f, formulaName, targetRig, opts)
	case formula.TypeWorkflow:
		return executeWorkflowFormula(f, formulaName, targetRig, opts)
	default:
		printUnsupportedFormulaType(formulaName, targetRig, string(f.Type))
		return nil
	}
}

func printUnsupportedFormulaType(formulaName, targetRig, formulaType string) {
	fmt.Printf("%s Formula type '%s' not yet supported for execution.\n",
		style.Dim.Render("Note:"), formulaType)
	fmt.Printf("Currently only 'convoy' and 'workflow' formulas can be run.\n")
	fmt.Printf("\nTo run '%s' manually:\n", formulaName)
	fmt.Printf("  1. View formula:   gt formula show %s\n", formulaName)
	fmt.Printf("  2. Cook to proto:  bd cook %s\n", formulaName)
	fmt.Printf("  3. Pour molecule:  bd pour %s\n", formulaName)
	fmt.Printf("  4. Sling to rig:   gt sling <mol-id> %s\n", targetRig)
}

// dryRunFormula shows what would happen without executing
func dryRunFormula(f *formula.Formula, formulaName, targetRig string, opts formulaRunOptions) error {
	printDryRunFormulaHeader(f, formulaName, targetRig, opts)
	if f.Type == formula.TypeConvoy && len(f.Legs) > 0 {
		dryRunConvoyFormula(f, formulaName, opts)
	}
	if f.Type == formula.TypeWorkflow && len(f.Steps) > 0 {
		dryRunWorkflowFormula(f)
	}
	return nil
}

func printDryRunFormulaHeader(f *formula.Formula, formulaName, targetRig string, opts formulaRunOptions) {
	fmt.Printf("%s Would execute formula:\n", style.Dim.Render("[dry-run]"))
	fmt.Printf("  Formula: %s\n", style.Bold.Render(formulaName))
	fmt.Printf("  Type:    %s\n", f.Type)
	fmt.Printf("  Rig:     %s\n", targetRig)
	if opts.pr > 0 {
		fmt.Printf("  PR:      #%d\n", opts.pr)
	}
	effectiveAgent := opts.agent
	if effectiveAgent == "" {
		effectiveAgent = f.Agent
	}
	if effectiveAgent != "" {
		fmt.Printf("  Agent:   %s\n", effectiveAgent)
	}
	if len(opts.set) > 0 {
		fmt.Printf("  Set:")
		for _, s := range opts.set {
			fmt.Printf(" %s", s)
		}
		fmt.Println()
	}
}

type dryRunConvoyCtx struct {
	f                 *formula.Formula
	formulaName       string
	opts              formulaRunOptions
	reviewID          string
	targetDescription string
	prTitle           string
	changedFiles      []map[string]interface{}
	setVars           map[string]interface{}
	outputDir         string
}

func dryRunConvoyFormula(f *formula.Formula, formulaName string, opts formulaRunOptions) {
	ctx := &dryRunConvoyCtx{
		f:           f,
		formulaName: formulaName,
		opts:        opts,
		reviewID:    generateFormulaShortID(),
		setVars:     parseSetVars(opts.set),
	}
	ctx.targetDescription = "local files"
	if opts.pr > 0 {
		ctx.targetDescription = fmt.Sprintf("PR #%d", opts.pr)
	}
	ctx.prTitle, ctx.changedFiles = dryRunConvoyPRInfo(opts)
	ctx.outputDir = dryRunConvoyOutputDir(ctx)
	fmt.Printf("\n  Legs (%d parallel):\n", len(f.Legs))
	for _, leg := range f.Legs {
		printDryRunConvoyLeg(ctx, leg)
	}
	printDryRunConvoySynthesis(f, ctx.outputDir)
}

func dryRunConvoyPRInfo(opts formulaRunOptions) (string, []map[string]interface{}) {
	if opts.pr <= 0 {
		return "", nil
	}
	prTitle, changedFiles := fetchPRInfo(opts.pr)
	if prTitle != "" {
		fmt.Printf("  PR Title: %s\n", prTitle)
	}
	if len(changedFiles) > 0 {
		fmt.Printf("  Changed files: %d\n", len(changedFiles))
	}
	return prTitle, changedFiles
}

func dryRunConvoyOutputDir(ctx *dryRunConvoyCtx) string {
	if ctx.f.Output == nil || ctx.f.Output.Directory == "" {
		return ""
	}
	dirCtx := formulaTemplateContext(ctx.formulaName, ctx.targetDescription, ctx.reviewID,
		ctx.opts.pr, ctx.prTitle, ctx.changedFiles, ctx.opts.files, ctx.setVars)
	outputDir := renderTemplateOrDefault(ctx.f.Output.Directory, dirCtx, ".reviews/"+ctx.reviewID)
	fmt.Printf("\n  Output directory: %s\n", outputDir)
	return outputDir
}

func printDryRunConvoyLeg(ctx *dryRunConvoyCtx, leg formula.Leg) {
	agentSuffix := resolveFormulaLegAgent(leg.Agent, ctx.opts.agent, ctx.f.Agent)
	if agentSuffix != "" {
		agentSuffix = fmt.Sprintf(" [agent: %s]", agentSuffix)
	}
	if ctx.f.Output == nil || ctx.outputDir == "" {
		fmt.Printf("    • %s: %s%s\n", leg.ID, leg.Title, agentSuffix)
		return
	}
	legCtx := formulaTemplateContext(ctx.formulaName, ctx.targetDescription, ctx.reviewID,
		ctx.opts.pr, ctx.prTitle, ctx.changedFiles, ctx.opts.files, ctx.setVars)
	legCtx["leg"] = map[string]interface{}{
		"id":          leg.ID,
		"title":       leg.Title,
		"focus":       leg.Focus,
		"description": leg.Description,
	}
	legPattern := renderTemplateOrDefault(ctx.f.Output.LegPattern, legCtx, leg.ID+"-findings.md")
	fmt.Printf("    • %s: %s%s\n      → %s\n", leg.ID, leg.Title, agentSuffix, filepath.Join(ctx.outputDir, legPattern))
}

func printDryRunConvoySynthesis(f *formula.Formula, outputDir string) {
	if f.Synthesis == nil {
		return
	}
	fmt.Printf("\n  Synthesis:\n")
	if f.Output != nil && outputDir != "" {
		fmt.Printf("    • %s\n      → %s\n", f.Synthesis.Title, filepath.Join(outputDir, f.Output.Synthesis))
		return
	}
	fmt.Printf("    • %s\n", f.Synthesis.Title)
}

func dryRunWorkflowFormula(f *formula.Formula) {
	fmt.Printf("\n  Steps (%d sequential):\n", len(f.Steps))
	for i, step := range f.Steps {
		needsStr := ""
		if len(step.Needs) > 0 {
			needsStr = fmt.Sprintf(" [needs: %s]", strings.Join(step.Needs, ", "))
		}
		readyStr := ""
		if len(step.Needs) == 0 {
			readyStr = " ← ready"
		}
		fmt.Printf("    %d. %s: %s%s%s\n", i+1, step.ID, step.Title, needsStr, readyStr)
	}
}

// truncate shortens a string to maxLen, appending "..." if truncated.
// Truncates at the first newline if one appears before maxLen.
func truncate(s string, maxLen int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < maxLen {
		s = s[:i]
	}
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// parseSetVars parses --set key=value pairs into a map for template rendering.
func parseSetVars(setArgs []string) map[string]interface{} {
	vars := make(map[string]interface{})
	for _, arg := range setArgs {
		if idx := strings.IndexByte(arg, '='); idx > 0 {
			vars[arg[:idx]] = arg[idx+1:]
		}
	}
	return vars
}

func formulaTemplateContext(formulaName, targetDescription, reviewID string, prNumber int, prTitle string, changedFiles []map[string]interface{}, files []string, setVars map[string]interface{}) map[string]interface{} {
	ctx := map[string]interface{}{
		"formula_name":       formulaName,
		"target_description": targetDescription,
		"review_id":          reviewID,
		"pr_number":          prNumber,
		"pr_title":           prTitle,
		"changed_files":      changedFiles,
		"files":              files,
	}
	for k, v := range setVars {
		ctx[k] = v
	}
	return ctx
}

func addOutputTemplateContext(ctx map[string]interface{}, outputDir, synthesisFile string) {
	ctx["output"] = map[string]interface{}{
		"directory": outputDir,
		"synthesis": synthesisFile,
	}
}

var formulaVarPlaceholder = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

func substituteFormulaVars(text string, vars map[string]interface{}) string {
	if len(vars) == 0 {
		return text
	}
	return formulaVarPlaceholder.ReplaceAllStringFunc(text, func(match string) string {
		sub := formulaVarPlaceholder.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		v, ok := vars[sub[1]]
		if !ok {
			return match
		}
		return fmt.Sprint(v)
	})
}

// findFormulaFile searches for a formula file by name
func findFormulaFile(name string) (string, error) {
	// Search paths in order
	searchPaths := []string{}

	// 1. Project .beads/formulas/
	if cwd, err := os.Getwd(); err == nil {
		searchPaths = append(searchPaths, filepath.Join(cwd, ".beads", "formulas"))
	}

	// 2. Town .beads/formulas/
	if townRoot, err := workspace.FindFromCwd(); err == nil {
		searchPaths = append(searchPaths, filepath.Join(townRoot, ".beads", "formulas"))
	}

	// 3. User ~/.beads/formulas/
	if home, err := os.UserHomeDir(); err == nil {
		searchPaths = append(searchPaths, filepath.Join(home, ".beads", "formulas"))
	}

	// Try each path with common extensions
	extensions := []string{".formula.toml", ".formula.json"}
	for _, basePath := range searchPaths {
		for _, ext := range extensions {
			path := filepath.Join(basePath, name+ext)
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
	}

	return "", fmt.Errorf("formula '%s' not found in search paths", name)
}

// parseFormulaFile parses a formula file using the formula package's TOML parser.
func parseFormulaFile(path string) (*formula.Formula, error) {
	return formula.ParseFile(path)
}

// renderTemplate renders a Go text/template with the given context map
func renderTemplate(tmplText string, ctx map[string]interface{}) (string, error) {
	tmpl, err := template.New("prompt").Parse(tmplText)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return buf.String(), nil
}

// renderTemplateOrDefault renders a template, returning defaultVal on error
func renderTemplateOrDefault(tmplText string, ctx map[string]interface{}, defaultVal string) string {
	if tmplText == "" {
		return defaultVal
	}
	result, err := renderTemplate(tmplText, ctx)
	if err != nil {
		return defaultVal
	}
	return result
}

// fetchPRInfo fetches PR title and changed files from GitHub using gh CLI
func fetchPRInfo(prNumber int) (string, []map[string]interface{}) {
	var prTitle string
	var changedFiles []map[string]interface{}

	// Get PR title
	titleCmd := exec.Command("gh", "pr", "view", fmt.Sprintf("%d", prNumber), "--json", "title", "--jq", ".title")
	titleOut, err := titleCmd.Output()
	if err == nil {
		prTitle = strings.TrimSpace(string(titleOut))
	}

	// Get changed files with stats
	filesCmd := exec.Command("gh", "pr", "view", fmt.Sprintf("%d", prNumber), "--json", "files", "--jq", ".files[] | \"\\(.path) \\(.additions) \\(.deletions)\"")
	filesOut, err := filesCmd.Output()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(filesOut)), "\n") {
			if line == "" {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				additions, err := strconv.Atoi(parts[1])
				if err != nil {
					continue
				}
				deletions, err := strconv.Atoi(parts[2])
				if err != nil {
					continue
				}
				changedFiles = append(changedFiles, map[string]interface{}{
					"path":      parts[0],
					"additions": additions,
					"deletions": deletions,
				})
			}
		}
	}

	return prTitle, changedFiles
}

// generateFormulaShortID generates a short random ID (5 lowercase chars)
func generateFormulaShortID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.EncodeToString(b)[:5])
}

// runFormulaCreate creates a new formula template
func runFormulaCreate(cmd *cobra.Command, args []string) error {
	formulaName := args[0]

	// Find or create formulas directory
	formulasDir := ".beads/formulas"

	// Check if we're in a beads-enabled directory
	if _, err := os.Stat(".beads"); os.IsNotExist(err) {
		// Try user formulas directory
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot find home directory: %w", err)
		}
		formulasDir = filepath.Join(home, ".beads", "formulas")
	}

	// Ensure directory exists
	if err := os.MkdirAll(formulasDir, 0755); err != nil {
		return fmt.Errorf("creating formulas directory: %w", err)
	}

	// Generate filename
	filename := filepath.Join(formulasDir, formulaName+".formula.toml")

	// Check if file already exists
	if _, err := os.Stat(filename); err == nil {
		return fmt.Errorf("formula already exists: %s", filename)
	}

	// Generate template based on type
	var template string
	switch formulaType := commandStringFlag(cmd, "type"); formulaType {
	case "task":
		template = generateTaskTemplate(formulaName)
	case "workflow":
		template = generateWorkflowTemplate(formulaName)
	case "patrol":
		template = generatePatrolTemplate(formulaName)
	default:
		return fmt.Errorf("unknown formula type: %s (use: task, workflow, or patrol)", formulaType)
	}

	// Write the file
	if err := os.WriteFile(filename, []byte(template), 0644); err != nil {
		return fmt.Errorf("writing formula file: %w", err)
	}

	fmt.Printf("%s Created formula: %s\n", style.Bold.Render("✓"), filename)
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  1. Edit the formula: %s\n", filename)
	fmt.Printf("  2. View it:          gt formula show %s\n", formulaName)
	fmt.Printf("  3. Run it:           gt formula run %s\n", formulaName)

	return nil
}

func generateTaskTemplate(name string) string {
	// Sanitize name for use in template
	title := strings.ReplaceAll(name, "-", " ")
	title = cases.Title(language.English).String(title)

	return fmt.Sprintf(`# Formula: %s
# Type: task
# Created by: gt formula create

description = """%s task.

Add a detailed description here."""
formula = "%s"
version = 1

# Single step task
[[steps]]
id = "do-task"
title = "Execute task"
description = """
Perform the main task work.

**Steps:**
1. Understand the requirements
2. Implement the changes
3. Verify the work
"""

# Variables that can be passed when running the formula
# [vars]
# [vars.issue]
# description = "Issue ID to work on"
# required = true
#
# [vars.target]
# description = "Target branch"
# default = "main"
`, name, title, name)
}

func generateWorkflowTemplate(name string) string {
	title := strings.ReplaceAll(name, "-", " ")
	title = cases.Title(language.English).String(title)

	return fmt.Sprintf(`# Formula: %s
# Type: workflow
# Created by: gt formula create
#
# pour = true  — Steps materialized as sub-wisps (checkpoint recovery on crash)
# pour = false — Steps read inline (root-only, restart on failure) [DEFAULT]

description = """%s workflow.

A multi-step workflow with dependencies between steps."""
formula = "%s"
version = 1

# Step 1: Setup
[[steps]]
id = "setup"
title = "Setup environment"
description = """
Prepare the environment for the workflow.

**Steps:**
1. Check prerequisites
2. Set up working environment
"""

# Step 2: Implementation (depends on setup)
[[steps]]
id = "implement"
title = "Implement changes"
needs = ["setup"]
description = """
Make the necessary code changes.

**Steps:**
1. Understand requirements
2. Write code
3. Test locally
"""

# Step 3: Test (depends on implementation)
[[steps]]
id = "test"
title = "Run tests"
needs = ["implement"]
description = """
Verify the changes work correctly.

**Steps:**
1. Run unit tests
2. Run integration tests
3. Check for regressions
"""

# Step 4: Complete (depends on tests)
[[steps]]
id = "complete"
title = "Complete workflow"
needs = ["test"]
description = """
Finalize and clean up.

**Steps:**
1. Commit final changes
2. Clean up temporary files
"""

# Variables
[vars]
[vars.issue]
description = "Issue ID to work on"
required = true
`, name, title, name)
}

func generatePatrolTemplate(name string) string {
	title := strings.ReplaceAll(name, "-", " ")
	title = cases.Title(language.English).String(title)

	return fmt.Sprintf(`# Formula: %s
# Type: patrol
# Created by: gt formula create
#
# Patrol formulas are for repeating cycles (wisps).
# They run continuously and are NOT synced to git.

description = """%s patrol.

A patrol formula for periodic checks. Patrol formulas create wisps
(ephemeral molecules) that are NOT synced to git."""
formula = "%s"
version = 1

# The patrol step(s)
[[steps]]
id = "check"
title = "Run patrol check"
description = """
Perform the patrol inspection.

**Check for:**
1. Health indicators
2. Warning signs
3. Items needing attention

**On findings:**
- Log the issue
- Escalate if critical
"""

# Optional: remediation step
# [[steps]]
# id = "remediate"
# title = "Fix issues"
# needs = ["check"]
# description = """
# Fix any issues found during the check.
# """

# Variables (optional)
# [vars]
# [vars.verbose]
# description = "Enable verbose output"
# default = "false"
`, name, title, name)
}

// resolveFormulaLegAgent returns the effective agent for a convoy leg using
// the precedence: per-leg > CLI --agent > formula-level. Returns "" if no
// agent override applies. See GH#2118.
func resolveFormulaLegAgent(legAgent, cliAgent, formulaAgent string) string {
	if legAgent != "" {
		return legAgent
	}
	if cliAgent != "" {
		return cliAgent
	}
	return formulaAgent
}

// promptYesNo asks the user a yes/no question
func promptYesNo(question string) bool {
	fmt.Printf("%s [y/N]: ", question)
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}
