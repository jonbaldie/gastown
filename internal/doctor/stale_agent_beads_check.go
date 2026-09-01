package doctor

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/constants"
)

// StaleAgentBeadsCheck detects agent beads that exist in the database but have
// no corresponding agent on disk. This catches beads inherited from upstream or
// left over after crew members are removed.
//
// Checks crew worker beads and polecat agent beads. Polecats have persistent
// identity (agent beads survive nuke cycles), so stale detection applies to them too.
//
// Also detects orphaned agent beads from deregistered rigs — beads whose prefix
// doesn't match any route in routes.jsonl. These accumulate when a rig is removed
// via gt rig remove but its agent beads in the town database are not cleaned up.
//
// The fix closes stale beads so they no longer pollute bd ready output.
type StaleAgentBeadsCheck struct {
	FixableCheck
}

// NewStaleAgentBeadsCheck creates a new stale agent beads check.
func NewStaleAgentBeadsCheck() *StaleAgentBeadsCheck {
	return &StaleAgentBeadsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "stale-agent-beads",
				CheckDescription: "Detect agent beads for removed workers (crew and polecats)",
				CheckCategory:    CategoryRig,
			},
		},
	}
}

// Run checks for agent beads that have no matching agent on disk.
func (c *StaleAgentBeadsCheck) Run(ctx *CheckContext) *CheckResult {
	return runStaleAgentBeadsCheck(c, ctx)
}

type staleAgentRoutes struct {
	prefixToRig   map[string]rigInfo
	knownPrefixes map[string]bool
}

func runStaleAgentBeadsCheck(c *StaleAgentBeadsCheck, ctx *CheckContext) *CheckResult {
	routes, err := beads.LoadRoutes(filepath.Join(ctx.TownRoot, ".beads"))
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not load routes.jsonl",
		}
	}

	routeInfo := buildStaleAgentRoutes(routes)
	stale := scanKnownRigBeads(ctx, routeInfo.prefixToRig)
	stale = append(stale, scanTownAgentBeads(ctx, routeInfo)...)
	return staleAgentResult(c, dedup(stale))
}

func buildStaleAgentRoutes(routes []beads.Route) staleAgentRoutes {
	result := staleAgentRoutes{
		prefixToRig:   make(map[string]rigInfo),
		knownPrefixes: make(map[string]bool),
	}
	for _, route := range routes {
		prefix := strings.TrimSuffix(route.Prefix, "-")
		result.knownPrefixes[prefix] = true
		parts := strings.Split(route.Path, "/")
		if parts[0] == "." {
			continue
		}
		result.prefixToRig[prefix] = rigInfo{name: parts[0], beadsPath: route.Path}
	}
	return result
}

func scanKnownRigBeads(ctx *CheckContext, prefixToRig map[string]rigInfo) []string {
	if len(prefixToRig) == 0 {
		return nil
	}
	results := make(chan []string, len(prefixToRig))
	for prefix, info := range prefixToRig {
		go func(prefix string, info rigInfo) {
			results <- scanRigAgentBeads(ctx, prefix, info)
		}(prefix, info)
	}
	var stale []string
	for range prefixToRig {
		stale = append(stale, <-results...)
	}
	return stale
}

func scanRigAgentBeads(ctx *CheckContext, prefix string, info rigInfo) []string {
	rigName := info.name
	bd := beads.New(filepath.Join(ctx.TownRoot, info.beadsPath))
	crewDiskSet := workerSet(listCrewWorkers(ctx.TownRoot, rigName))
	polecatDiskSet := workerSet(listPolecats(ctx.TownRoot, rigName))
	crewPrefix := beads.AgentBeadIDWithPrefix(prefix, rigName, constants.RoleCrew, "") + "-"
	polecatPrefix := beads.AgentBeadIDWithPrefix(prefix, rigName, constants.RolePolecat, "") + "-"
	allBeads, err := bd.List(beads.ListOptions{Status: "open", Priority: -1, Label: "gt:agent"})
	if err != nil {
		return nil
	}
	allBeads = append(allBeads, openWispAgentBeads(bd)...)
	return staleRigBeads(allBeads, crewPrefix, polecatPrefix, crewDiskSet, polecatDiskSet)
}

func workerSet(workers []string) map[string]bool {
	set := make(map[string]bool, len(workers))
	for _, worker := range workers {
		set[worker] = true
	}
	return set
}

func openWispAgentBeads(bd *beads.Beads) []*beads.Issue {
	wispMap, _ := bd.ListAgentBeadsFromWisps()
	var open []*beads.Issue
	for _, issue := range wispMap {
		if issue.Status == "open" || issue.Status == "in_progress" || issue.Status == "hooked" {
			open = append(open, issue)
		}
	}
	return open
}

func staleRigBeads(allBeads []*beads.Issue, crewPrefix, polecatPrefix string, crewWorkers, polecatWorkers map[string]bool) []string {
	var stale []string
	for _, issue := range allBeads {
		if staleWorkerBead(issue.ID, crewPrefix, crewWorkers) || staleWorkerBead(issue.ID, polecatPrefix, polecatWorkers) {
			stale = append(stale, issue.ID)
		}
	}
	return stale
}

func staleWorkerBead(id, prefix string, workers map[string]bool) bool {
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	workerName := strings.TrimPrefix(id, prefix)
	return workerName != "" && !workers[workerName]
}

func scanTownAgentBeads(ctx *CheckContext, routes staleAgentRoutes) []string {
	townBd := beads.New(beads.GetTownBeadsPath(ctx.TownRoot))
	townAgents, err := townBd.ListAgentBeads()
	if err != nil {
		return nil
	}
	var stale []string
	for id, issue := range townAgents {
		if isStaleTownAgentBead(ctx, id, issue, routes) {
			stale = append(stale, id)
		}
	}
	return stale
}

func isStaleTownAgentBead(ctx *CheckContext, id string, issue *beads.Issue, routes staleAgentRoutes) bool {
	if !isActiveAgentBead(issue) {
		return false
	}
	rig, role, _, ok := beads.ParseAgentBeadID(id)
	if !ok || rig == "" {
		return false
	}
	idPrefix := agentIDPrefix(id)
	if idPrefix == "" {
		return false
	}
	if !routes.knownPrefixes[idPrefix] {
		return true
	}
	info, exists := routes.prefixToRig[idPrefix]
	return exists && missingKnownRigWorker(ctx, id, idPrefix, info, role)
}

func isActiveAgentBead(issue *beads.Issue) bool {
	if issue == nil || issue.Type != "agent" {
		return false
	}
	return issue.Status == "open" || issue.Status == "in_progress" || issue.Status == "hooked"
}

func agentIDPrefix(id string) string {
	idx := strings.Index(id, "-")
	if idx < 0 {
		return ""
	}
	return id[:idx]
}

func missingKnownRigWorker(ctx *CheckContext, id, prefix string, info rigInfo, role string) bool {
	if role != constants.RoleCrew && role != constants.RolePolecat {
		return false
	}
	workerName := parseCrewOrPolecatFromID(id, prefix, info.name, role)
	if workerName == "" {
		return false
	}
	workers := listCrewWorkers(ctx.TownRoot, info.name)
	if role == constants.RolePolecat {
		workers = listPolecats(ctx.TownRoot, info.name)
	}
	for _, worker := range workers {
		if worker == workerName {
			return false
		}
	}
	return true
}

func staleAgentResult(c *StaleAgentBeadsCheck, stale []string) *CheckResult {
	if len(stale) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No stale agent beads found",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d stale agent bead(s) for removed workers", len(stale)),
		Details: stale,
		FixHint: "Run 'gt doctor --fix' to close stale agent beads",
	}
}

// parseCrewOrPolecatFromID extracts the worker name from a crew or polecat bead ID.
// Returns the worker name, or empty string if the ID doesn't match the expected pattern.
func parseCrewOrPolecatFromID(id, prefix, rigName, role string) string {
	// Build the expected prefix pattern: prefix-rig-role- or prefix-role- (collapsed)
	var idPrefix string
	if prefix == rigName {
		idPrefix = prefix + "-" + role + "-"
	} else {
		idPrefix = prefix + "-" + rigName + "-" + role + "-"
	}
	if strings.HasPrefix(id, idPrefix) {
		return strings.TrimPrefix(id, idPrefix)
	}
	return ""
}

// dedup removes duplicate strings from a slice, preserving order.
func dedup(s []string) []string {
	if len(s) == 0 {
		return s
	}
	seen := make(map[string]bool, len(s))
	result := make([]string, 0, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	return result
}

// Fix closes stale agent beads for crew members that no longer exist on disk.
// For beads with known prefixes, closes via the rig's beads client.
// For orphan beads from deregistered rigs (unknown prefix), closes via the
// town beads client since that's where they were found by Phase 2 detection.
func (c *StaleAgentBeadsCheck) Fix(ctx *CheckContext) error {
	result := c.Run(ctx)
	if result.Status == StatusOK {
		return nil
	}
	return fixStaleAgentBeads(ctx, result.Details)
}

func fixStaleAgentBeads(ctx *CheckContext, stale []string) error {
	routes, err := beads.LoadRoutes(filepath.Join(ctx.TownRoot, ".beads"))
	if err != nil {
		return fmt.Errorf("loading routes.jsonl: %w", err)
	}

	prefixToPath := staleAgentPrefixPaths(ctx.TownRoot, routes)
	townBeadsPath := beads.GetTownBeadsPath(ctx.TownRoot)
	townBd := beads.New(townBeadsPath)

	var errs []error
	for _, beadID := range stale {
		if err := closeStaleAgentBead(beadID, prefixToPath, townBd); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func staleAgentPrefixPaths(townRoot string, routes []beads.Route) map[string]string {
	prefixToPath := make(map[string]string)
	for _, route := range routes {
		parts := strings.Split(route.Path, "/")
		if parts[0] == "." {
			continue
		}
		prefix := strings.TrimSuffix(route.Prefix, "-")
		prefixToPath[prefix] = filepath.Join(townRoot, route.Path)
	}
	return prefixToPath
}

func closeStaleAgentBead(beadID string, prefixToPath map[string]string, townBd *beads.Beads) error {
	bd := townBd
	for prefix, path := range prefixToPath {
		if strings.HasPrefix(beadID, prefix+"-") {
			bd = beads.New(path)
			break
		}
	}
	closedStatus := "closed"
	if err := bd.Update(beadID, beads.UpdateOptions{Status: &closedStatus}); err != nil {
		return fmt.Errorf("closing stale bead %s: %w", beadID, err)
	}
	return nil
}
