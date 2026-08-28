// Package beads molecule support - composable workflow templates.
package beads

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jonbaldie/gastown/internal/telemetry"
)

// MoleculeStep represents a parsed step from a molecule definition.
type MoleculeStep struct {
	Ref          string         // Step reference (from "## Step: <ref>")
	Title        string         // Step title (first non-empty line or ref)
	Instructions string         // Prose instructions for this step
	Needs        []string       // Step refs this step depends on
	WaitsFor     []string       // Dynamic wait conditions (e.g., "all-children")
	Tier         string         // Optional tier hint: haiku, sonnet, opus
	Type         string         // Step type: "task" (default), "wait", etc.
	Backoff      *BackoffConfig // Backoff configuration for wait-type steps
}

// BackoffConfig defines exponential backoff parameters for wait-type steps.
// Used by patrol agents to implement cost-saving await-signal patterns.
type BackoffConfig struct {
	Base       string // Base interval (e.g., "30s")
	Multiplier int    // Multiplier for exponential growth (default: 2)
	Max        string // Maximum interval cap (e.g., "10m")
}

// stepHeaderRegex matches "## Step: <ref>" with optional whitespace.
var stepHeaderRegex = regexp.MustCompile(`(?i)^##\s*Step:\s*(\S+)\s*$`)

// needsLineRegex matches "Needs: step1, step2, ..." lines.
var needsLineRegex = regexp.MustCompile(`(?i)^Needs:\s*(.+)$`)

// tierLineRegex matches "Tier: haiku|sonnet|opus" lines.
var tierLineRegex = regexp.MustCompile(`(?i)^Tier:\s*(haiku|sonnet|opus)\s*$`)

// waitsForLineRegex matches "WaitsFor: condition1, condition2, ..." lines.
// Common conditions: "all-children" (fanout gate for dynamically bonded children)
var waitsForLineRegex = regexp.MustCompile(`(?i)^WaitsFor:\s*(.+)$`)

// typeLineRegex matches "Type: task|wait|..." lines.
// Common types: "task" (default), "wait" (await-signal with backoff)
var typeLineRegex = regexp.MustCompile(`(?i)^Type:\s*(\w+)\s*$`)

// backoffLineRegex matches "Backoff: base=30s, multiplier=2, max=10m" lines.
// Parses backoff configuration for wait-type steps.
var backoffLineRegex = regexp.MustCompile(`(?i)^Backoff:\s*(.+)$`)

// templateVarRegex matches {{variable}} placeholders.
var templateVarRegex = regexp.MustCompile(`\{\{(\w+)\}\}`)

// ParseMoleculeSteps extracts step definitions from a molecule's description.
//
// The expected format is:
//
//	## Step: <ref>
//	<prose instructions>
//	Needs: <step>, <step>  # optional
//	Tier: haiku|sonnet|opus  # optional
//	Type: task|wait  # optional, default is "task"
//	Backoff: base=30s, multiplier=2, max=10m  # optional, for wait-type steps
//
// Returns an empty slice if no steps are found.
func ParseMoleculeSteps(description string) ([]MoleculeStep, error) {
	if description == "" {
		return nil, nil
	}

	var steps []MoleculeStep
	var currentRef string
	var contentLines []string
	for _, line := range strings.Split(description, "\n") {
		if matches := stepHeaderRegex.FindStringSubmatch(line); matches != nil {
			steps = appendMoleculeStep(steps, currentRef, contentLines)
			currentRef = matches[1]
			contentLines = nil
			continue
		}

		if currentRef != "" {
			contentLines = append(contentLines, line)
		}
	}
	return appendMoleculeStep(steps, currentRef, contentLines), nil
}

func appendMoleculeStep(steps []MoleculeStep, ref string, contentLines []string) []MoleculeStep {
	if ref == "" {
		return steps
	}
	return append(steps, parseMoleculeStep(ref, contentLines))
}

func parseMoleculeStep(ref string, contentLines []string) MoleculeStep {
	step := MoleculeStep{Ref: ref}
	var instructionLines []string
	for _, line := range contentLines {
		if !parseMoleculeStepDirective(&step, strings.TrimSpace(line)) {
			instructionLines = append(instructionLines, line)
		}
	}
	setMoleculeStepInstructions(&step, instructionLines)
	return step
}

func parseMoleculeStepDirective(step *MoleculeStep, line string) bool {
	if matches := needsLineRegex.FindStringSubmatch(line); matches != nil {
		step.Needs = append(step.Needs, splitCommaSeparatedValues(matches[1])...)
		return true
	}
	if matches := tierLineRegex.FindStringSubmatch(line); matches != nil {
		step.Tier = strings.ToLower(matches[1])
		return true
	}
	if matches := waitsForLineRegex.FindStringSubmatch(line); matches != nil {
		step.WaitsFor = append(step.WaitsFor, splitCommaSeparatedValues(matches[1])...)
		return true
	}
	if matches := typeLineRegex.FindStringSubmatch(line); matches != nil {
		step.Type = strings.ToLower(matches[1])
		return true
	}
	if matches := backoffLineRegex.FindStringSubmatch(line); matches != nil {
		step.Backoff = parseBackoffConfig(matches[1])
		return true
	}
	return false
}

func splitCommaSeparatedValues(value string) []string {
	var values []string
	for _, value := range strings.Split(value, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func setMoleculeStepInstructions(step *MoleculeStep, instructionLines []string) {
	step.Instructions = strings.TrimSpace(strings.Join(instructionLines, "\n"))
	if step.Instructions != "" {
		step.Title = strings.TrimSpace(strings.SplitN(step.Instructions, "\n", 2)[0])
	}
	if step.Title == "" {
		step.Title = step.Ref
	}
}

// parseBackoffConfig parses a backoff configuration string.
// Expected format: "base=30s, multiplier=2, max=10m"
// Returns nil if parsing fails.
func parseBackoffConfig(configStr string) *BackoffConfig {
	cfg := &BackoffConfig{
		Multiplier: 2, // Default multiplier
	}

	// Split by comma and parse key=value pairs
	parts := strings.Split(configStr, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Split by = to get key and value
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}

		key := strings.TrimSpace(strings.ToLower(kv[0]))
		value := strings.TrimSpace(kv[1])

		switch key {
		case "base":
			cfg.Base = value
		case "multiplier":
			if m, err := strconv.Atoi(value); err == nil {
				cfg.Multiplier = m
			}
		case "max":
			cfg.Max = value
		}
	}

	// Return nil if no base was specified (incomplete config)
	if cfg.Base == "" {
		return nil
	}

	return cfg
}

// ExpandTemplateVars replaces {{variable}} placeholders in text using the provided context map.
// Unknown variables are left as-is.
func ExpandTemplateVars(text string, ctx map[string]string) string {
	if ctx == nil {
		return text
	}

	return templateVarRegex.ReplaceAllStringFunc(text, func(match string) string {
		// Extract variable name from {{name}}
		varName := match[2 : len(match)-2]
		if value, ok := ctx[varName]; ok {
			return value
		}
		return match // Leave unknown variables as-is
	})
}

// InstantiateOptions configures molecule instantiation behavior.
type InstantiateOptions struct {
	// Context map for {{variable}} substitution
	Context map[string]string
}

// InstantiateMolecule creates child issues from a molecule template.
//
// This function supports two molecule formats (format bridge pattern):
//
//  1. New format (child issues): If the molecule proto has child issues,
//     those children are used as templates. Dependencies are copied from
//     the template children's DependsOn relationships.
//
//  2. Old format (embedded markdown): If the molecule has no children,
//     steps are parsed from the Description field using ParseMoleculeSteps().
//     Dependencies are extracted from "Needs:" declarations in the markdown.
//
// For each step, this creates:
//   - A child issue with ID "{parent.ID}.{step.Ref}"
//   - Title from step title
//   - Description from step instructions (with template vars expanded)
//   - Type: task
//   - Priority: inherited from parent
//   - Dependencies wired according to template
//
// The function is atomic via bd CLI - either all issues are created or none.
// Returns the created step issues.
func (b *Beads) InstantiateMolecule(ctx context.Context, mol *Issue, parent *Issue, opts InstantiateOptions) ([]*Issue, error) {
	if mol == nil {
		return nil, fmt.Errorf("molecule issue is nil")
	}
	if parent == nil {
		return nil, fmt.Errorf("parent issue is nil")
	}

	// FORMAT BRIDGE: Try new format first (child issues), fall back to old format (markdown)
	templateChildren, err := b.List(ListOptions{
		Parent:   mol.ID,
		Status:   "all",
		Priority: -1,
	})
	if err != nil {
		// Non-fatal - might not have children, continue to old format
		templateChildren = nil
	}

	if len(templateChildren) > 0 {
		// NEW FORMAT: Use child issues as templates
		return b.instantiateFromChildren(ctx, mol, parent, templateChildren, opts)
	}

	// OLD FORMAT: Parse steps from molecule description
	return b.instantiateFromMarkdown(ctx, mol, parent, opts)
}

// instantiateFromChildren creates steps from template child issues (new format).
func (b *Beads) instantiateFromChildren(ctx context.Context, mol *Issue, parent *Issue, templates []*Issue, opts InstantiateOptions) ([]*Issue, error) {
	createdIssues, templateToNew, err := b.createTemplateChildren(ctx, mol, parent, templates, opts)
	if err != nil {
		return nil, err
	}
	if err := b.wireTemplateDependencies(templates, templateToNew); err != nil {
		return createdIssues, err
	}
	return createdIssues, nil
}

func (b *Beads) createTemplateChildren(ctx context.Context, mol *Issue, parent *Issue, templates []*Issue, opts InstantiateOptions) ([]*Issue, map[string]string, error) {
	var createdIssues []*Issue
	templateToNew := make(map[string]string, len(templates))
	for _, tmpl := range templates {
		child, err := b.Create(moleculeTemplateCreateOptions(mol, parent, tmpl, opts))
		if err != nil {
			b.closeMoleculeIssues(createdIssues)
			return nil, nil, fmt.Errorf("creating step from template %q: %w", tmpl.ID, err)
		}
		telemetry.RecordBeadCreate(ctx, child.ID, parent.ID, mol.ID)
		createdIssues = append(createdIssues, child)
		templateToNew[tmpl.ID] = child.ID
	}
	return createdIssues, templateToNew, nil
}

func moleculeTemplateCreateOptions(mol, parent, tmpl *Issue, opts InstantiateOptions) CreateOptions {
	description := moleculeTemplateDescription(mol.ID, tmpl, opts.Context)
	stepType := tmpl.Type
	if stepType == "" {
		stepType = "task"
	}
	return CreateOptions{
		Title:       tmpl.Title,
		Labels:      []string{"gt:" + stepType},
		Priority:    parent.Priority,
		Description: description,
		Parent:      parent.ID,
	}
}

func moleculeTemplateDescription(moleculeID string, tmpl *Issue, context map[string]string) string {
	description := tmpl.Description
	if context != nil {
		description = ExpandTemplateVars(description, context)
	}
	if description != "" {
		description += "\n\n"
	}
	return description + fmt.Sprintf("instantiated_from: %s\ntemplate_step: %s", moleculeID, tmpl.ID)
}

func (b *Beads) wireTemplateDependencies(templates []*Issue, templateToNew map[string]string) error {
	for _, tmpl := range templates {
		for _, dependency := range tmpl.DependsOn {
			if err := b.addTemplateDependency(templateToNew, tmpl.ID, dependency); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Beads) addTemplateDependency(templateToNew map[string]string, templateID, dependencyID string) error {
	newDependencyID, ok := templateToNew[dependencyID]
	if !ok {
		return nil
	}
	newChildID := templateToNew[templateID]
	if err := b.AddDependency(newChildID, newDependencyID); err != nil {
		return fmt.Errorf("adding dependency %s -> %s: %w", newChildID, newDependencyID, err)
	}
	return nil
}

func (b *Beads) closeMoleculeIssues(issues []*Issue) {
	for _, issue := range issues {
		_ = b.Close(issue.ID)
	}
}

// instantiateFromMarkdown creates steps from embedded markdown (old format).
func (b *Beads) instantiateFromMarkdown(ctx context.Context, mol *Issue, parent *Issue, opts InstantiateOptions) ([]*Issue, error) {
	steps, err := markdownMoleculeSteps(mol.Description)
	if err != nil {
		return nil, err
	}
	createdIssues, stepIssueIDs, err := b.createMarkdownChildren(ctx, mol, parent, steps, opts)
	if err != nil {
		return nil, err
	}
	if err := b.wireMarkdownDependencies(steps, stepIssueIDs); err != nil {
		return createdIssues, err
	}
	return createdIssues, nil
}

func markdownMoleculeSteps(description string) ([]MoleculeStep, error) {
	steps, err := ParseMoleculeSteps(description)
	if err != nil {
		return nil, fmt.Errorf("parsing molecule steps: %w", err)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("molecule has no steps defined")
	}
	if err := validateMarkdownDependencies(steps); err != nil {
		return nil, err
	}
	return steps, nil
}

func validateMarkdownDependencies(steps []MoleculeStep) error {
	stepRefs := make(map[string]bool, len(steps))
	for _, step := range steps {
		stepRefs[step.Ref] = true
	}
	for _, step := range steps {
		for _, need := range step.Needs {
			if !stepRefs[need] {
				return fmt.Errorf("step %q depends on unknown step %q", step.Ref, need)
			}
		}
	}
	return nil
}

func (b *Beads) createMarkdownChildren(ctx context.Context, mol *Issue, parent *Issue, steps []MoleculeStep, opts InstantiateOptions) ([]*Issue, map[string]string, error) {
	var createdIssues []*Issue
	stepIssueIDs := make(map[string]string, len(steps))
	for _, step := range steps {
		child, err := b.Create(markdownStepCreateOptions(mol, parent, step, opts))
		if err != nil {
			b.closeMoleculeIssues(createdIssues)
			return nil, nil, fmt.Errorf("creating step %q: %w", step.Ref, err)
		}
		telemetry.RecordBeadCreate(ctx, child.ID, parent.ID, mol.ID)
		createdIssues = append(createdIssues, child)
		stepIssueIDs[step.Ref] = child.ID
	}
	return createdIssues, stepIssueIDs, nil
}

func markdownStepCreateOptions(mol, parent *Issue, step MoleculeStep, opts InstantiateOptions) CreateOptions {
	return CreateOptions{
		Title:       step.Title,
		Labels:      []string{"gt:task"},
		Priority:    parent.Priority,
		Description: markdownStepDescription(mol.ID, step, opts.Context),
		Parent:      parent.ID,
	}
}

func markdownStepDescription(moleculeID string, step MoleculeStep, context map[string]string) string {
	description := step.Instructions
	if context != nil {
		description = ExpandTemplateVars(description, context)
	}
	if description != "" {
		description += "\n\n"
	}
	description += fmt.Sprintf("instantiated_from: %s\nstep: %s", moleculeID, step.Ref)
	if step.Tier != "" {
		description += fmt.Sprintf("\ntier: %s", step.Tier)
	}
	return description
}

func (b *Beads) wireMarkdownDependencies(steps []MoleculeStep, stepIssueIDs map[string]string) error {
	for _, step := range steps {
		for _, need := range step.Needs {
			childID := stepIssueIDs[step.Ref]
			dependsOnID := stepIssueIDs[need]
			if err := b.AddDependency(childID, dependsOnID); err != nil {
				return fmt.Errorf("adding dependency %s -> %s: %w", childID, dependsOnID, err)
			}
		}
	}
	return nil
}

// ValidateMolecule checks if an issue is a valid molecule definition.
// Returns an error describing the problem, or nil if valid.
//
// Note: This function only validates the old format (embedded markdown steps).
// For new format molecules (with child issues), validation is implicit during
// instantiation - if the molecule has children, those are used as templates.
// Use InstantiateMolecule directly for new format molecules; this function
// will report "no steps defined" for new format molecules since it cannot
// access child issues without a Beads client.
func ValidateMolecule(mol *Issue) error {
	if mol == nil {
		return fmt.Errorf("molecule is nil")
	}

	steps, err := moleculeStepsForValidation(mol)
	if err != nil {
		return err
	}
	stepRefs, err := validateMoleculeStepRefs(steps)
	if err != nil {
		return err
	}
	if err := validateMoleculeDependencies(steps, stepRefs); err != nil {
		return err
	}
	return detectCycles(steps)
}

func moleculeStepsForValidation(mol *Issue) ([]MoleculeStep, error) {
	if mol.Type != "molecule" {
		return nil, fmt.Errorf("issue type is %q, expected molecule", mol.Type)
	}

	steps, err := ParseMoleculeSteps(mol.Description)
	if err != nil {
		return nil, fmt.Errorf("parsing steps: %w", err)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("molecule has no steps defined")
	}
	return steps, nil
}

func validateMoleculeStepRefs(steps []MoleculeStep) (map[string]bool, error) {
	stepRefs := make(map[string]bool, len(steps))
	for _, step := range steps {
		if step.Ref == "" {
			return nil, fmt.Errorf("step has empty ref")
		}
		if stepRefs[step.Ref] {
			return nil, fmt.Errorf("duplicate step ref: %s", step.Ref)
		}
		stepRefs[step.Ref] = true
	}
	return stepRefs, nil
}

func validateMoleculeDependencies(steps []MoleculeStep, stepRefs map[string]bool) error {
	for _, step := range steps {
		for _, need := range step.Needs {
			if !stepRefs[need] {
				return fmt.Errorf("step %q depends on unknown step %q", step.Ref, need)
			}
			if need == step.Ref {
				return fmt.Errorf("step %q has self-dependency", step.Ref)
			}
		}
	}
	return nil
}

// detectCycles checks for circular dependencies in the step graph using DFS.
// Returns an error describing the cycle if one is found.
func detectCycles(steps []MoleculeStep) error {
	deps := moleculeStepDependencies(steps)

	// Track visit state: 0 = unvisited, 1 = visiting (in stack), 2 = visited
	state := make(map[string]int)

	// DFS from each node to find cycles
	var path []string
	var dfs func(node string) error

	dfs = func(node string) error {
		if state[node] == 2 {
			return nil // Already fully processed
		}
		if state[node] == 1 {
			return cycleDependencyError(path, node)
		}

		state[node] = 1 // Mark as visiting
		path = append(path, node)

		for _, dep := range deps[node] {
			if err := dfs(dep); err != nil {
				return err
			}
		}

		path = path[:len(path)-1] // Pop from path
		state[node] = 2           // Mark as visited
		return nil
	}

	for _, step := range steps {
		if state[step.Ref] == 0 {
			if err := dfs(step.Ref); err != nil {
				return err
			}
		}
	}

	return nil
}

func cycleDependencyError(path []string, node string) error {
	for index, pathNode := range path {
		if pathNode == node {
			cycle := append(path[index:], node)
			return fmt.Errorf("cycle detected in step dependencies: %s", formatCycle(cycle))
		}
	}
	return fmt.Errorf("cycle detected in step dependencies: %s", node)
}

func moleculeStepDependencies(steps []MoleculeStep) map[string][]string {
	deps := make(map[string][]string, len(steps))
	for _, step := range steps {
		deps[step.Ref] = step.Needs
	}
	return deps
}

// formatCycle formats a cycle path as "a -> b -> c -> a".
func formatCycle(cycle []string) string {
	return strings.Join(cycle, " -> ")
}
