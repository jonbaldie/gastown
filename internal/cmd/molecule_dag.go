package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

// DAGNode represents a node in the dependency graph.
type DAGNode struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	Status       string     `json:"status"`
	Parallel     bool       `json:"parallel,omitempty"`
	Dependencies []string   `json:"dependencies,omitempty"`
	Dependents   []string   `json:"dependents,omitempty"`
	Tier         int        `json:"tier"` // Execution tier (0 = root, higher = later)
	Children     []*DAGNode `json:"children,omitempty"`
}

// DAGInfo contains the full DAG information for a molecule.
type DAGInfo struct {
	RootID       string              `json:"root_id"`
	RootTitle    string              `json:"root_title"`
	TotalNodes   int                 `json:"total_nodes"`
	Tiers        int                 `json:"tiers"`
	CriticalPath []string            `json:"critical_path,omitempty"`
	Nodes        map[string]*DAGNode `json:"nodes"`
	TierGroups   [][]string          `json:"tier_groups"` // Nodes grouped by tier
}

var moleculeDagCmd = &cobra.Command{
	Use:   "dag <molecule-id>",
	Short: "Visualize molecule dependency DAG",
	Long: `Display the dependency DAG (Directed Acyclic Graph) for a molecule.

Shows the dependency structure with execution tiers and status:
  ✓ done        - Step completed
  ⧖ in_progress - Step being worked on
  ○ ready       - Step ready to execute (all deps met)
  ◌ blocked     - Step waiting on dependencies

Examples:
  gt mol dag gs-wisp-abc     # Show DAG for molecule
  gt mol dag gs-wisp-abc --json  # JSON output
  gt mol dag gs-wisp-abc --tree  # Tree view (default)
  gt mol dag gs-wisp-abc --tiers # Group by execution tier`,
	Args: cobra.ExactArgs(1),
	RunE: runMoleculeDag,
}

func init() {
	moleculeDagCmd.Flags().Bool("tiers", false, "Group output by execution tier")
	moleculeDagCmd.Flags().Bool("tree", true, "Show tree view (default)")
	moleculeDagCmd.Flags().Bool("json", false, "Output as JSON")
}

func runMoleculeDag(cmd *cobra.Command, args []string) error {
	jsonOutput, showTiers, err := moleculeDagOptions(cmd)
	if err != nil {
		return err
	}
	dag, err := loadMoleculeDAG(args[0])
	if err != nil {
		return err
	}

	if jsonOutput {
		return writeDAGJSON(dag)
	}

	if showTiers {
		return outputDAGTiers(dag)
	}
	return outputDAGTree(dag)
}

func loadMoleculeDAG(rootID string) (*DAGInfo, error) {
	workDir, err := findLocalBeadsDir()
	if err != nil {
		return nil, fmt.Errorf("not in a beads workspace: %w", err)
	}

	b := beads.New(workDir)
	root, err := b.Show(rootID)
	if err != nil {
		return nil, fmt.Errorf("getting root issue: %w", err)
	}

	children, err := b.List(beads.ListOptions{
		Parent:   rootID,
		Status:   "all",
		Priority: -1,
	})
	if err != nil {
		return nil, fmt.Errorf("listing children: %w", err)
	}
	if len(children) == 0 {
		return nil, fmt.Errorf("no steps found for %s (not a molecule root?)", rootID)
	}

	dag, err := buildDAG(b, root, children)
	if err != nil {
		return nil, fmt.Errorf("building DAG: %w", err)
	}
	return dag, nil
}

func writeDAGJSON(dag *DAGInfo) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(dag)
}

func moleculeDagOptions(cmd *cobra.Command) (jsonOutput, showTiers bool, err error) {
	if cmd == nil {
		return false, false, nil
	}
	jsonOutput, err = cmd.Flags().GetBool("json")
	if err != nil {
		return false, false, err
	}
	showTiers, err = cmd.Flags().GetBool("tiers")
	return jsonOutput, showTiers, err
}

// buildDAG constructs the DAG from molecule children.
func buildDAG(b *beads.Beads, root *beads.Issue, children []*beads.Issue) (*DAGInfo, error) {
	dag := &DAGInfo{
		RootID:    root.ID,
		RootTitle: root.Title,
		Nodes:     make(map[string]*DAGNode),
	}

	stepsMap, err := b.ShowMultiple(dagStepIDs(children))
	if err != nil {
		return nil, fmt.Errorf("fetching step details: %w", err)
	}

	closedIDs := closedDAGIDs(children)
	for _, child := range children {
		dag.Nodes[child.ID] = buildDAGNode(child, stepsMap[child.ID], closedIDs)
		dag.TotalNodes++
	}

	linkDAGDependents(dag)

	// Compute tiers using topological sort
	computeTiers(dag)

	// Find critical path
	dag.CriticalPath = findCriticalPath(dag)

	return dag, nil
}

func dagStepIDs(children []*beads.Issue) []string {
	stepIDs := make([]string, 0, len(children))
	for _, child := range children {
		stepIDs = append(stepIDs, child.ID)
	}
	return stepIDs
}

func closedDAGIDs(children []*beads.Issue) map[string]bool {
	closedIDs := make(map[string]bool)
	for _, child := range children {
		if child.Status == "closed" {
			closedIDs[child.ID] = true
		}
	}
	return closedIDs
}

func buildDAGNode(child, step *beads.Issue, closedIDs map[string]bool) *DAGNode {
	if step == nil {
		step = child
	}

	node := &DAGNode{
		ID:       child.ID,
		Title:    child.Title,
		Status:   child.Status,
		Parallel: strings.Contains(step.Description, "parallel: true") || strings.Contains(step.Description, "parallel=true"),
	}
	for _, dep := range step.Dependencies {
		if isBlockingDepType(dep.DependencyType) {
			node.Dependencies = append(node.Dependencies, dep.ID)
		}
	}
	if node.Status == "open" {
		node.Status = dagNodeStatus(node.Dependencies, closedIDs)
	}
	return node
}

func dagNodeStatus(dependencies []string, closedIDs map[string]bool) string {
	for _, depID := range dependencies {
		if !closedIDs[depID] {
			return "blocked"
		}
	}
	return "ready"
}

func linkDAGDependents(dag *DAGInfo) {
	for id, node := range dag.Nodes {
		for _, depID := range node.Dependencies {
			if depNode, ok := dag.Nodes[depID]; ok {
				depNode.Dependents = append(depNode.Dependents, id)
			}
		}
	}
}

// computeTiers assigns execution tiers to each node.
// Tier 0 = nodes with no dependencies, higher tiers depend on lower ones.
func computeTiers(dag *DAGInfo) {
	// Calculate in-degrees
	inDegree := make(map[string]int)
	for id, node := range dag.Nodes {
		inDegree[id] = len(node.Dependencies)
	}

	// Kahn's algorithm for tier assignment
	currentTier := 0
	processed := 0
	tierGroups := [][]string{}

	for processed < dag.TotalNodes {
		// Find all nodes with in-degree 0 (current tier)
		var tierNodes []string
		for id, degree := range inDegree {
			if degree == 0 {
				tierNodes = append(tierNodes, id)
			}
		}

		if len(tierNodes) == 0 {
			// Cycle detected (shouldn't happen with validated molecules)
			break
		}

		// Sort for deterministic output
		sort.Strings(tierNodes)
		tierGroups = append(tierGroups, tierNodes)

		// Assign tier and remove from graph
		for _, id := range tierNodes {
			dag.Nodes[id].Tier = currentTier
			delete(inDegree, id)
			processed++

			// Decrement in-degree of dependents
			for _, depID := range dag.Nodes[id].Dependents {
				if _, ok := inDegree[depID]; ok {
					inDegree[depID]--
				}
			}
		}

		currentTier++
	}

	dag.Tiers = currentTier
	dag.TierGroups = tierGroups
}

// findCriticalPath finds the longest path through the DAG.
func findCriticalPath(dag *DAGInfo) []string {
	// DFS to find longest path
	memo := make(map[string][]string)

	var dfs func(id string) []string
	dfs = func(id string) []string {
		if path, ok := memo[id]; ok {
			return path
		}

		node := dag.Nodes[id]
		if node == nil {
			return nil
		}

		longestSuffix := []string{}
		for _, depID := range node.Dependents {
			suffix := dfs(depID)
			if len(suffix) > len(longestSuffix) {
				longestSuffix = suffix
			}
		}

		path := append([]string{id}, longestSuffix...)
		memo[id] = path
		return path
	}

	// Find longest path starting from tier 0 nodes
	var criticalPath []string
	for _, tierNodes := range dag.TierGroups {
		for _, id := range tierNodes {
			if dag.Nodes[id].Tier == 0 {
				path := dfs(id)
				if len(path) > len(criticalPath) {
					criticalPath = path
				}
			}
		}
		break // Only check tier 0
	}

	return criticalPath
}

// outputDAGTree outputs the DAG as a tree.
func outputDAGTree(dag *DAGInfo) error {
	fmt.Printf("\n%s %s\n", style.Bold.Render("🌳 DAG:"), dag.RootTitle)
	fmt.Printf("   Root: %s\n", dag.RootID)
	fmt.Printf("   Nodes: %d | Tiers: %d\n", dag.TotalNodes, dag.Tiers)

	if len(dag.CriticalPath) > 0 {
		fmt.Printf("   Critical path: %s\n", strings.Join(dag.CriticalPath, " → "))
	}
	fmt.Println()

	// Build tree structure for display
	// Start with tier 0 nodes (roots)
	if len(dag.TierGroups) > 0 {
		for i, id := range dag.TierGroups[0] {
			isLast := i == len(dag.TierGroups[0])-1
			printNode(dag, id, "", isLast, make(map[string]bool))
		}
	}

	// Legend
	fmt.Println()
	fmt.Printf("   %s done  %s in_progress  %s ready  %s blocked\n",
		style.Bold.Render("✓"), style.Bold.Render("⧖"), style.Bold.Render("○"), style.Dim.Render("◌"))

	return nil
}

// printNode recursively prints a node and its dependents.
func printNode(dag *DAGInfo, id, prefix string, isLast bool, visited map[string]bool) {
	if visited[id] {
		return // Prevent cycles in display
	}
	visited[id] = true

	node := dag.Nodes[id]
	if node == nil {
		return
	}

	fmt.Printf("%s%s %s %s%s\n", prefix, dagConnector(isLast), dagStatusIcon(node.Status), node.ID, dagParallelMark(node, " ∥"))

	// Print dependents (children in the DAG)
	childPrefix := dagChildPrefix(prefix, isLast)
	for i, depID := range node.Dependents {
		isLastChild := i == len(node.Dependents)-1
		printNode(dag, depID, childPrefix, isLastChild, visited)
	}
}

func dagConnector(isLast bool) string {
	if isLast {
		return "└─"
	}
	return "├─"
}

func dagStatusIcon(status string) string {
	switch status {
	case "closed":
		return style.Bold.Render("✓")
	case "in_progress":
		return style.Bold.Render("⧖")
	case "ready":
		return style.Bold.Render("○")
	default:
		return style.Dim.Render("◌")
	}
}

func dagParallelMark(node *DAGNode, mark string) string {
	if node.Parallel {
		return mark
	}
	return ""
}

func dagChildPrefix(prefix string, isLast bool) string {
	if isLast {
		return prefix + "   "
	}
	return prefix + "│  "
}

// outputDAGTiers outputs the DAG grouped by execution tier.
func outputDAGTiers(dag *DAGInfo) error {
	fmt.Printf("\n%s %s\n", style.Bold.Render("📊 DAG Tiers:"), dag.RootTitle)
	fmt.Printf("   Root: %s\n", dag.RootID)
	fmt.Printf("   Nodes: %d | Tiers: %d\n", dag.TotalNodes, dag.Tiers)
	fmt.Println()

	for tier, nodes := range dag.TierGroups {
		printDAGTier(dag, tier, nodes)
		fmt.Println()
	}

	// Critical path
	if len(dag.CriticalPath) > 0 {
		fmt.Printf("   %s %s\n", style.Bold.Render("Critical path:"), strings.Join(dag.CriticalPath, " → "))
	}

	// Legend
	fmt.Println()
	fmt.Printf("   %s done  %s in_progress  %s ready  %s blocked\n",
		style.Bold.Render("✓"), style.Bold.Render("⧖"), style.Bold.Render("○"), style.Dim.Render("◌"))

	return nil
}

func printDAGTier(dag *DAGInfo, tier int, nodes []string) {
	fmt.Printf("   %s Tier %d%s\n", style.Bold.Render("─"), tier, dagTierLabel(tier, dag.Tiers))
	for _, id := range nodes {
		if line, ok := formatDAGTierNode(dag.Nodes[id]); ok {
			fmt.Println(line)
		}
	}
}

func dagTierLabel(tier, total int) string {
	if tier == 0 {
		return " (entry)"
	}
	if tier == total-1 {
		return " (exit)"
	}
	return ""
}

func formatDAGTierNode(node *DAGNode) (string, bool) {
	if node == nil {
		return "", false
	}
	depStr := ""
	if len(node.Dependencies) > 0 {
		depStr = fmt.Sprintf(" ← %s", strings.Join(node.Dependencies, ", "))
	}
	return fmt.Sprintf("       %s %s%s%s", dagStatusIcon(node.Status), node.ID, dagParallelMark(node, " [parallel]"), depStr), true
}
