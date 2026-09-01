package formula

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// ParseFile reads and parses a formula.toml file.
func ParseFile(path string) (*Formula, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is from trusted formula directory
	if err != nil {
		return nil, fmt.Errorf("reading formula file: %w", err)
	}
	return Parse(data)
}

// Parse parses formula.toml content from bytes.
func Parse(data []byte) (*Formula, error) {
	var f Formula
	if _, err := toml.Decode(string(data), &f); err != nil {
		return nil, fmt.Errorf("parsing TOML: %w", err)
	}

	// Infer type from content if not explicitly set
	f.inferType()

	if err := f.Validate(); err != nil {
		return nil, err
	}

	return &f, nil
}

// inferType sets the formula type based on content when not explicitly set.
func (f *Formula) inferType() {
	if f.Type != "" {
		return // Type already set
	}

	// Infer from content
	if len(f.Extends) > 0 {
		f.Type = TypeWorkflow // Composition formulas inherit steps after Resolve()
	} else if len(f.Steps) > 0 {
		f.Type = TypeWorkflow
	} else if len(f.Legs) > 0 {
		f.Type = TypeConvoy
	} else if len(f.Template) > 0 {
		f.Type = TypeExpansion
	} else if len(f.Aspects) > 0 {
		f.Type = TypeAspect
	}
}

// Validate checks that the formula has all required fields and valid structure.
func (f *Formula) Validate() error {
	// Check required common fields
	if f.Name == "" {
		return fmt.Errorf("formula field is required")
	}

	if !f.Type.IsValid() {
		return fmt.Errorf("invalid formula type %q (must be convoy, workflow, expansion, or aspect)", f.Type)
	}

	// Type-specific validation
	switch f.Type {
	case TypeConvoy:
		return f.validateConvoy()
	case TypeWorkflow:
		return f.validateWorkflow()
	case TypeExpansion:
		return f.validateExpansion()
	case TypeAspect:
		return f.validateAspect()
	}

	return nil
}

func (f *Formula) validateConvoy() error {
	if len(f.Legs) == 0 {
		return fmt.Errorf("convoy formula requires at least one leg")
	}

	seen, err := validateLegIDs(f.Legs)
	if err != nil {
		return err
	}
	if err := validateSynthesisDependencies(f.Synthesis, seen); err != nil {
		return err
	}
	return validateRequiredUnless(f.Inputs)
}

func validateLegIDs(legs []Leg) (map[string]bool, error) {
	seen := make(map[string]bool)
	for _, leg := range legs {
		if leg.ID == "" {
			return nil, fmt.Errorf("leg missing required id field")
		}
		if seen[leg.ID] {
			return nil, fmt.Errorf("duplicate leg id: %s", leg.ID)
		}
		seen[leg.ID] = true
	}
	return seen, nil
}

func validateSynthesisDependencies(synthesis *Synthesis, legIDs map[string]bool) error {
	if synthesis == nil {
		return nil
	}
	for _, dep := range synthesis.DependsOn {
		if !legIDs[dep] {
			return fmt.Errorf("synthesis depends_on references unknown leg: %s", dep)
		}
	}
	return nil
}

func validateRequiredUnless(inputs map[string]Input) error {
	for name, input := range inputs {
		for _, ref := range input.RequiredUnless {
			if _, ok := inputs[ref]; !ok {
				return fmt.Errorf("input %q has required_unless referencing unknown input %q", name, ref)
			}
		}
	}
	return nil
}

func (f *Formula) validateWorkflow() error {
	// Allow empty steps when extends is set — steps come from parent after Resolve().
	if len(f.Steps) == 0 && len(f.Extends) == 0 {
		return fmt.Errorf("workflow formula requires at least one step")
	}

	seen, err := validateWorkflowStepIDs(f.Steps)
	if err != nil {
		return err
	}
	if err := validateWorkflowNeeds(f.Steps, seen); err != nil {
		return err
	}

	// Check for cycles
	if err := f.checkCycles(); err != nil {
		return err
	}

	return nil
}

func validateWorkflowStepIDs(steps []Step) (map[string]bool, error) {
	seen := make(map[string]bool)
	for _, step := range steps {
		if step.ID == "" {
			return nil, fmt.Errorf("step missing required id field")
		}
		if seen[step.ID] {
			return nil, fmt.Errorf("duplicate step id: %s", step.ID)
		}
		seen[step.ID] = true
	}
	return seen, nil
}

func validateWorkflowNeeds(steps []Step, seen map[string]bool) error {
	for _, step := range steps {
		for _, need := range step.Needs {
			if !seen[need] {
				return fmt.Errorf("step %q needs unknown step: %s", step.ID, need)
			}
		}
	}
	return nil
}

func (f *Formula) validateExpansion() error {
	if len(f.Template) == 0 {
		return fmt.Errorf("expansion formula requires at least one template")
	}

	// Check template IDs are unique
	seen := make(map[string]bool)
	for _, tmpl := range f.Template {
		if tmpl.ID == "" {
			return fmt.Errorf("template missing required id field")
		}
		if seen[tmpl.ID] {
			return fmt.Errorf("duplicate template id: %s", tmpl.ID)
		}
		seen[tmpl.ID] = true
	}

	// Validate template needs references
	for _, tmpl := range f.Template {
		for _, need := range tmpl.Needs {
			if !seen[need] {
				return fmt.Errorf("template %q needs unknown template: %s", tmpl.ID, need)
			}
		}
	}

	// Check for cycles
	if err := f.checkExpansionCycles(); err != nil {
		return err
	}

	return nil
}

func (f *Formula) validateAspect() error {
	if len(f.Aspects) == 0 {
		return fmt.Errorf("aspect formula requires at least one aspect")
	}

	// Check aspect IDs are unique
	seen := make(map[string]bool)
	for _, aspect := range f.Aspects {
		if aspect.ID == "" {
			return fmt.Errorf("aspect missing required id field")
		}
		if seen[aspect.ID] {
			return fmt.Errorf("duplicate aspect id: %s", aspect.ID)
		}
		seen[aspect.ID] = true
	}

	return nil
}

// checkCycles detects circular dependencies in steps.
func (f *Formula) checkCycles() error {
	deps := make(map[string][]string)
	for _, step := range f.Steps {
		deps[step.ID] = step.Needs
	}
	return checkDependencyCycles(deps)
}

// checkExpansionCycles detects circular dependencies in expansion templates.
func (f *Formula) checkExpansionCycles() error {
	deps := make(map[string][]string)
	for _, tmpl := range f.Template {
		deps[tmpl.ID] = tmpl.Needs
	}
	return checkDependencyCycles(deps)
}

// checkDependencyCycles detects cycles in a dependency graph.
func checkDependencyCycles(deps map[string][]string) error {
	visited := make(map[string]bool)
	inStack := make(map[string]bool)

	var visit func(id string) error
	visit = func(id string) error {
		if inStack[id] {
			return fmt.Errorf("cycle detected involving: %s", id)
		}
		if visited[id] {
			return nil
		}
		visited[id] = true
		inStack[id] = true

		for _, dep := range deps[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}

		inStack[id] = false
		return nil
	}

	// Sort keys for deterministic cycle detection order
	ids := make([]string, 0, len(deps))
	for id := range deps {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}

	return nil
}

// TopologicalSort returns steps in dependency order (dependencies before dependents).
// Only applicable to workflow and expansion formulas.
// Returns an error if there are cycles.
func (f *Formula) TopologicalSort() ([]string, error) {
	items, deps, err := topologicalData(f)
	if err != nil {
		return nil, err
	}
	return topologicalOrder(items, deps)
}

func topologicalData(f *Formula) ([]string, map[string][]string, error) {
	switch f.Type {
	case TypeWorkflow:
		return workflowTopologicalData(f.Steps)
	case TypeExpansion:
		return expansionTopologicalData(f.Template)
	case TypeConvoy:
		return legIDs(f.Legs), nil, nil
	case TypeAspect:
		return aspectIDs(f.Aspects), nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported formula type for topological sort")
	}
}

func workflowTopologicalData(steps []Step) ([]string, map[string][]string, error) {
	items := make([]string, 0, len(steps))
	deps := make(map[string][]string, len(steps))
	for _, step := range steps {
		items = append(items, step.ID)
		deps[step.ID] = step.Needs
	}
	return items, deps, nil
}

func expansionTopologicalData(templates []Template) ([]string, map[string][]string, error) {
	items := make([]string, 0, len(templates))
	deps := make(map[string][]string, len(templates))
	for _, tmpl := range templates {
		items = append(items, tmpl.ID)
		deps[tmpl.ID] = tmpl.Needs
	}
	return items, deps, nil
}

func legIDs(legs []Leg) []string {
	items := make([]string, 0, len(legs))
	for _, leg := range legs {
		items = append(items, leg.ID)
	}
	return items
}

func aspectIDs(aspects []Aspect) []string {
	items := make([]string, 0, len(aspects))
	for _, aspect := range aspects {
		items = append(items, aspect.ID)
	}
	return items
}

func topologicalOrder(items []string, deps map[string][]string) ([]string, error) {
	inDegree := make(map[string]int)
	for _, id := range items {
		inDegree[id] = 0
	}
	for _, id := range items {
		for range deps[id] {
			inDegree[id]++
		}
	}

	queue := topologicalQueue(items, inDegree)
	dependents := reverseDependencies(items, deps)
	result := drainTopologicalQueue(queue, inDegree, dependents)

	if len(result) != len(items) {
		return nil, fmt.Errorf("cycle detected in dependencies")
	}

	return result, nil
}

func topologicalQueue(items []string, inDegree map[string]int) []string {
	var queue []string
	for _, id := range items {
		if inDegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	return queue
}

func reverseDependencies(items []string, deps map[string][]string) map[string][]string {
	dependents := make(map[string][]string)
	for _, id := range items {
		for _, dep := range deps[id] {
			dependents[dep] = append(dependents[dep], id)
		}
	}
	return dependents
}

func drainTopologicalQueue(queue []string, inDegree map[string]int, dependents map[string][]string) []string {
	var result []string
	for {
		if len(queue) == 0 {
			break
		}
		id := queue[0]
		queue = queue[1:]
		result = append(result, id)
		for _, dependent := range dependents[id] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}
	return result
}

// ReadySteps returns steps that have no unmet dependencies.
// completed is a set of step IDs that have been completed.
func (f *Formula) ReadySteps(completed map[string]bool) []string {
	switch f.Type {
	case TypeWorkflow:
		return readyWorkflowSteps(f.Steps, completed)
	case TypeExpansion:
		return readyExpansionSteps(f.Template, completed)
	case TypeConvoy:
		// All legs are ready unless already completed
		return readyLegs(f.Legs, completed)
	case TypeAspect:
		// All aspects are ready unless already completed
		return readyAspects(f.Aspects, completed)
	}

	return nil
}

func readyWorkflowSteps(steps []Step, completed map[string]bool) []string {
	var ready []string
	for _, step := range steps {
		if completed[step.ID] || !needsCompleted(step.Needs, completed) {
			continue
		}
		ready = append(ready, step.ID)
	}
	return ready
}

func readyExpansionSteps(templates []Template, completed map[string]bool) []string {
	var ready []string
	for _, tmpl := range templates {
		if completed[tmpl.ID] || !needsCompleted(tmpl.Needs, completed) {
			continue
		}
		ready = append(ready, tmpl.ID)
	}
	return ready
}

func needsCompleted(needs []string, completed map[string]bool) bool {
	for _, need := range needs {
		if !completed[need] {
			return false
		}
	}
	return true
}

func readyLegs(legs []Leg, completed map[string]bool) []string {
	var ready []string
	for _, leg := range legs {
		if !completed[leg.ID] {
			ready = append(ready, leg.ID)
		}
	}
	return ready
}

func readyAspects(aspects []Aspect, completed map[string]bool) []string {
	var ready []string
	for _, aspect := range aspects {
		if !completed[aspect.ID] {
			ready = append(ready, aspect.ID)
		}
	}
	return ready
}

// GetStep returns a step by ID, or nil if not found.
func (f *Formula) GetStep(id string) *Step {
	for i := range f.Steps {
		if f.Steps[i].ID == id {
			return &f.Steps[i]
		}
	}
	return nil
}

// ParallelReadySteps returns ready steps grouped by whether they can run in parallel.
// Returns (parallelSteps, sequentialStep) where:
// - parallelSteps: steps marked with parallel=true that share the same needs
// - sequentialStep: the first non-parallel ready step, or nil if all are parallel
// If multiple parallel steps are ready, they should all be executed concurrently.
func (f *Formula) ParallelReadySteps(completed map[string]bool) (parallel []string, sequential string) {
	ready := f.ReadySteps(completed)
	if len(ready) == 0 {
		return nil, ""
	}

	// For non-workflow formulas, return all as parallel (convoy/aspect are inherently parallel)
	if f.Type != TypeWorkflow {
		return ready, ""
	}

	// Group by parallel flag
	var parallelIDs []string
	var sequentialIDs []string
	for _, id := range ready {
		step := f.GetStep(id)
		if step != nil && step.Parallel {
			parallelIDs = append(parallelIDs, id)
		} else {
			sequentialIDs = append(sequentialIDs, id)
		}
	}

	// If we have parallel steps, return them all for concurrent execution
	if len(parallelIDs) > 0 {
		return parallelIDs, ""
	}

	// Otherwise return the first sequential step
	if len(sequentialIDs) > 0 {
		return nil, sequentialIDs[0]
	}

	return nil, ""
}

// GetLeg returns a leg by ID, or nil if not found.
func (f *Formula) GetLeg(id string) *Leg {
	for i := range f.Legs {
		if f.Legs[i].ID == id {
			return &f.Legs[i]
		}
	}
	return nil
}

// GetTemplate returns a template by ID, or nil if not found.
func (f *Formula) GetTemplate(id string) *Template {
	for i := range f.Template {
		if f.Template[i].ID == id {
			return &f.Template[i]
		}
	}
	return nil
}

// GetAspect returns an aspect by ID, or nil if not found.
func (f *Formula) GetAspect(id string) *Aspect {
	for i := range f.Aspects {
		if f.Aspects[i].ID == id {
			return &f.Aspects[i]
		}
	}
	return nil
}

// Resolve processes the extends and compose rules of a formula, returning a new
// formula with all inherited steps merged and expansion rules applied.
//
// Parent formulas named in extends are loaded from the embedded formula FS first,
// then from any additional searchPaths (in order). searchPaths may be nil.
//
// Cycles in extends chains are detected and reported as errors.
func Resolve(formula *Formula, searchPaths []string) (*Formula, error) {
	return resolveChain(formula, searchPaths, nil)
}

// resolveChain is the recursive workhorse for Resolve; chain tracks the current
// extends chain for cycle detection.
func resolveChain(formula *Formula, searchPaths []string, chain []string) (*Formula, error) {
	if err := checkExtendsCycle(formula.Name, chain); err != nil {
		return nil, err
	}

	// No inheritance or composition — validate and return as-is.
	if len(formula.Extends) == 0 && formula.Compose == nil {
		if err := formula.Validate(); err != nil {
			return nil, err
		}
		return formula, nil
	}

	chain = append(chain, formula.Name)
	merged := newMergedFormula(formula)
	if err := mergeParentFormulas(merged, formula.Extends, searchPaths, chain); err != nil {
		return nil, err
	}
	mergeChildFormula(merged, formula)
	if err := applyComposeRules(merged, formula.Compose, searchPaths); err != nil {
		return nil, err
	}

	if err := merged.Validate(); err != nil {
		return nil, err
	}
	return merged, nil
}

func checkExtendsCycle(formulaName string, chain []string) error {
	for _, name := range chain {
		if name == formulaName {
			return fmt.Errorf("circular extends detected: %s", strings.Join(append(chain, formulaName), " -> "))
		}
	}
	return nil
}

func newMergedFormula(formula *Formula) *Formula {
	merged := &Formula{
		Name:        formula.Name,
		Description: formula.Description,
		Type:        formula.Type,
		Version:     formula.Version,
		Pour:        formula.Pour,
		Agent:       formula.Agent,
		Vars:        make(map[string]Var),
		FormulaComposition: FormulaComposition{
			Compose: formula.Compose,
		},
	}
	if merged.Type == "" {
		merged.Type = TypeWorkflow
	}
	return merged
}

func mergeParentFormulas(merged *Formula, extends, searchPaths, chain []string) error {
	for _, parentName := range extends {
		parent, err := loadFormulaByName(parentName, searchPaths)
		if err != nil {
			return fmt.Errorf("extends %q: %w", parentName, err)
		}
		parent, err = resolveChain(parent, searchPaths, chain)
		if err != nil {
			return fmt.Errorf("resolve parent %q: %w", parentName, err)
		}

		// Inherit vars (child overrides take precedence later).
		for name, v := range parent.Vars {
			if _, exists := merged.Vars[name]; !exists {
				merged.Vars[name] = v
			}
		}
		// Inherit steps (parent steps come first).
		merged.Steps = append(merged.Steps, parent.Steps...)

		// Use parent description as fallback.
		if merged.Description == "" {
			merged.Description = parent.Description
		}
	}
	return nil
}

func mergeChildFormula(merged, formula *Formula) {
	for name, v := range formula.Vars {
		merged.Vars[name] = v
	}
	// Append child's own steps after parent steps.
	merged.Steps = append(merged.Steps, formula.Steps...)
	// Child description takes priority.
	if formula.Description != "" {
		merged.Description = formula.Description
	}
}

func applyComposeRules(merged *Formula, compose *ComposeRules, searchPaths []string) error {
	if compose == nil {
		return nil
	}
	for _, rule := range compose.Expand {
		expanded, err := applyExpandRule(merged.Steps, rule, searchPaths)
		if err != nil {
			return fmt.Errorf("compose expand %q with %q: %w", rule.Target, rule.With, err)
		}
		merged.Steps = expanded
	}
	// compose.aspects is recorded but not yet acted upon (future work).
	return nil
}

// loadFormulaByName loads a formula by name: embedded FS first, then searchPaths.
func loadFormulaByName(name string, searchPaths []string) (*Formula, error) {
	// Try the embedded formula filesystem first.
	data, err := GetEmbeddedFormulaContent(name)
	if err == nil {
		return Parse(data)
	}

	// Fall back to on-disk search paths.
	for _, dir := range searchPaths {
		path := filepath.Join(dir, name+".formula.toml")
		if data, err2 := os.ReadFile(path); err2 == nil { //nolint:gosec // G304: path from controlled search paths
			return Parse(data)
		}
	}

	return nil, fmt.Errorf("formula %q not found in embedded FS or search paths", name)
}

// applyExpandRule replaces a target step in steps with the template steps from an
// expansion formula.  Steps that depended on the target are updated to depend on
// the last expanded step instead.
func applyExpandRule(steps []Step, rule *ExpandRule, searchPaths []string) ([]Step, error) {
	expansion, err := loadExpansionFormula(rule.With, searchPaths)
	if err != nil {
		return nil, err
	}
	targetIdx, targetStep, err := findTargetStep(steps, rule.Target)
	if err != nil {
		return nil, err
	}
	expanded := buildExpandedSteps(expansion.Template, rule, targetStep)
	return replaceExpandedStep(steps, targetIdx, expanded, rule.Target), nil
}

func loadExpansionFormula(name string, searchPaths []string) (*Formula, error) {
	expansion, err := loadFormulaByName(name, searchPaths)
	if err != nil {
		return nil, fmt.Errorf("expansion formula %q: %w", name, err)
	}
	if expansion.Type != TypeExpansion {
		return nil, fmt.Errorf("formula %q is type %q, want %q", name, expansion.Type, TypeExpansion)
	}
	if len(expansion.Template) == 0 {
		return nil, fmt.Errorf("expansion formula %q has no template steps", name)
	}
	return expansion, nil
}

func findTargetStep(steps []Step, target string) (int, Step, error) {
	for i, step := range steps {
		if step.ID == target {
			return i, step, nil
		}
	}
	return -1, Step{}, fmt.Errorf("target step %q not found in formula steps", target)
}

func buildExpandedSteps(templates []Template, rule *ExpandRule, targetStep Step) []Step {
	expanded := make([]Step, 0, len(templates))
	for _, tmpl := range templates {
		expanded = append(expanded, buildExpandedStep(tmpl, rule, targetStep))
	}
	return expanded
}

func buildExpandedStep(tmpl Template, rule *ExpandRule, targetStep Step) Step {
	step := Step{
		ID:          expandPlaceholders(tmpl.ID, rule.Target, targetStep),
		Title:       expandPlaceholders(tmpl.Title, rule.Target, targetStep),
		Description: expandPlaceholders(tmpl.Description, rule.Target, targetStep),
		Acceptance:  expandPlaceholders(tmpl.Acceptance, rule.Target, targetStep),
	}
	if len(tmpl.Needs) == 0 {
		// First expanded step inherits the target's own needs.
		step.Needs = append([]string(nil), targetStep.Needs...)
		return step
	}
	step.Needs = make([]string, len(tmpl.Needs))
	for i, need := range tmpl.Needs {
		step.Needs[i] = expandPlaceholders(need, rule.Target, targetStep)
	}
	return step
}

func replaceExpandedStep(steps []Step, targetIdx int, expanded []Step, target string) []Step {
	lastExpanded := expanded[len(expanded)-1].ID
	result := make([]Step, 0, len(steps)-1+len(expanded))
	for i, step := range steps {
		if i == targetIdx {
			result = append(result, expanded...)
			continue
		}
		result = append(result, rewriteStepDependency(step, target, lastExpanded))
	}
	return result
}

func rewriteStepDependency(step Step, target, replacement string) Step {
	updated := false
	for j, need := range step.Needs {
		if need == target {
			if !updated {
				step.Needs = append([]string(nil), step.Needs...)
				updated = true
			}
			step.Needs[j] = replacement
		}
	}
	return step
}

// expandPlaceholders replaces {target} and {target.title}/{target.description}
// in expansion template strings with the actual target step values.
func expandPlaceholders(s, targetID string, targetStep Step) string {
	s = strings.ReplaceAll(s, "{target.title}", targetStep.Title)
	s = strings.ReplaceAll(s, "{target.description}", targetStep.Description)
	s = strings.ReplaceAll(s, "{target}", targetID)
	return s
}
