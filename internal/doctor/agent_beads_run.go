package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
)

type rigInfo struct {
	name      string
	beadsPath string
}

type agentBeadIndex struct {
	issues map[string]*beads.Issue
	wisps  map[string]bool
}

type agentBeadsRun struct {
	ctx          *CheckContext
	prefixToRig  map[string]rigInfo
	index        agentBeadIndex
	missing      []string
	missingLabel []string
	checked      int
}

func newAgentBeadIndex() agentBeadIndex {
	return agentBeadIndex{
		issues: make(map[string]*beads.Issue),
		wisps:  make(map[string]bool),
	}
}

func (idx *agentBeadIndex) loadFrom(bd *beads.Beads) {
	if agents, err := bd.ListAgentBeads(); err == nil {
		for id, issue := range agents {
			idx.issues[id] = issue
		}
	}
	if wisps, _ := bd.ListWispIDs(); wisps != nil {
		for id := range wisps {
			idx.wisps[id] = true
		}
	}
}

func loadAgentBeadsPrefixMap(ctx *CheckContext) (map[string]rigInfo, error) {
	routes, err := beads.LoadRoutes(filepath.Join(ctx.TownRoot, ".beads"))
	if err != nil {
		return nil, err
	}
	prefixToRig := make(map[string]rigInfo)
	for _, r := range routes {
		parts := strings.Split(r.Path, "/")
		if len(parts) < 1 || parts[0] == "." {
			continue
		}
		rigName := parts[0]
		if ctx.RigName != "" && rigName != ctx.RigName {
			continue
		}
		prefix := strings.TrimSuffix(r.Prefix, "-")
		prefixToRig[prefix] = rigInfo{name: rigName, beadsPath: r.Path}
	}
	return prefixToRig, nil
}

func runAgentBeadsCheck(c *AgentBeadsCheck, ctx *CheckContext) *CheckResult {
	prefixToRig, err := loadAgentBeadsPrefixMap(ctx)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not load routes.jsonl",
		}
	}
	r := &agentBeadsRun{
		ctx:         ctx,
		prefixToRig: prefixToRig,
		index:       newAgentBeadIndex(),
	}
	r.loadAll()
	r.checkID(beads.DeaconBeadIDTown())
	r.checkID(beads.MayorBeadIDTown())
	if len(prefixToRig) > 0 {
		r.checkRigAgents()
	}
	return r.result(c, len(prefixToRig) == 0)
}

func (r *agentBeadsRun) loadAll() {
	r.index.loadFrom(beads.New(beads.GetTownBeadsPath(r.ctx.TownRoot)))
	for _, info := range r.prefixToRig {
		r.index.loadFrom(beads.New(filepath.Join(r.ctx.TownRoot, info.beadsPath)))
	}
}

func (r *agentBeadsRun) checkID(id string) {
	if issue, exists := r.index.issues[id]; exists {
		if !beads.HasLabel(issue, "gt:agent") {
			r.missingLabel = append(r.missingLabel, id)
		}
	} else if !r.index.wisps[id] {
		r.missing = append(r.missing, id)
	}
	r.checked++
}

func (r *agentBeadsRun) checkRigAgents() {
	for prefix, info := range r.prefixToRig {
		r.checkID(beads.WitnessBeadIDWithPrefix(prefix, info.name))
		r.checkID(beads.RefineryBeadIDWithPrefix(prefix, info.name))
		for _, workerName := range listCrewWorkers(r.ctx.TownRoot, info.name) {
			r.checkID(beads.CrewBeadIDWithPrefix(prefix, info.name, workerName))
		}
		for _, polecatName := range listPolecats(r.ctx.TownRoot, info.name) {
			r.checkID(beads.PolecatBeadIDWithPrefix(prefix, info.name, polecatName))
		}
	}
}

func (r *agentBeadsRun) result(c *AgentBeadsCheck, emptyPrefix bool) *CheckResult {
	if len(r.missing) == 0 && len(r.missingLabel) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d agent beads exist with gt:agent label", r.checked),
		}
	}
	if emptyPrefix {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d agent bead(s) missing, %d missing gt:agent label", len(r.missing), len(r.missingLabel)),
			Details: append(r.missing, r.missingLabel...),
			FixHint: "Run 'gt doctor --fix' to create missing agent beads and add labels",
		}
	}
	if len(r.missing) > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d agent bead(s) missing", len(r.missing)),
			Details: r.missing,
			FixHint: "Run 'gt doctor --fix' to create missing agent beads",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d agent bead(s) missing gt:agent label", len(r.missingLabel)),
		Details: r.missingLabel,
		FixHint: "Run 'gt doctor --fix' to add missing labels",
	}
}

func fixAgentBeads(_ *AgentBeadsCheck, ctx *CheckContext) error {
	idx := newAgentBeadIndex()
	townBeadsPath := beads.GetTownBeadsPath(ctx.TownRoot)
	townBd := beads.New(townBeadsPath)
	idx.loadFrom(townBd)
	var errs []error
	errs = append(errs, fixTownAgentBeads(&idx, townBd, townBeadsPath)...)
	prefixToRig, err := loadAgentBeadsPrefixMap(ctx)
	if err != nil {
		return fmt.Errorf("loading routes.jsonl: %w", err)
	}
	if len(prefixToRig) == 0 {
		return errors.Join(errs...)
	}
	for _, info := range prefixToRig {
		idx.loadFrom(beads.New(filepath.Join(ctx.TownRoot, info.beadsPath)))
	}
	for prefix, info := range prefixToRig {
		errs = append(errs, fixRigAgentBeads(ctx, &idx, prefix, info)...)
	}
	return errors.Join(errs...)
}

func fixTownAgentBeads(idx *agentBeadIndex, townBd *beads.Beads, townBeadsPath string) []error {
	var errs []error
	if err := ensureAgentBead(idx, townBd, townBeadsPath, beads.DeaconBeadIDTown(),
		"Deacon (daemon beacon) - receives mechanical heartbeats, runs town plugins and monitoring.",
		&beads.AgentFields{RoleType: "deacon", AgentState: "idle"},
	); err != nil {
		errs = append(errs, err)
	}
	if err := ensureAgentBead(idx, townBd, townBeadsPath, beads.MayorBeadIDTown(),
		"Mayor - global coordinator, handles cross-rig communication and escalations.",
		&beads.AgentFields{RoleType: "mayor", AgentState: "idle"},
	); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func fixRigAgentBeads(ctx *CheckContext, idx *agentBeadIndex, prefix string, info rigInfo) []error {
	rigBeadsPath := filepath.Join(ctx.TownRoot, info.beadsPath)
	bd := beads.New(rigBeadsPath)
	var errs []error
	if err := ensureAgentBead(idx, bd, rigBeadsPath, beads.WitnessBeadIDWithPrefix(prefix, info.name),
		fmt.Sprintf("Witness for %s - monitors polecat health and progress.", info.name),
		&beads.AgentFields{RoleType: "witness", Rig: info.name, AgentState: "idle"},
	); err != nil {
		errs = append(errs, err)
	}
	if err := ensureAgentBead(idx, bd, rigBeadsPath, beads.RefineryBeadIDWithPrefix(prefix, info.name),
		fmt.Sprintf("Refinery for %s - processes merge queue.", info.name),
		&beads.AgentFields{RoleType: "refinery", Rig: info.name, AgentState: "idle"},
	); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, fixCrewAgentBeads(ctx, idx, bd, rigBeadsPath, prefix, info.name)...)
	return append(errs, fixPolecatAgentBeads(ctx, idx, bd, rigBeadsPath, prefix, info.name)...)
}

func fixCrewAgentBeads(ctx *CheckContext, idx *agentBeadIndex, bd *beads.Beads, rigBeadsPath, prefix, rigName string) []error {
	var errs []error
	for _, workerName := range listCrewWorkers(ctx.TownRoot, rigName) {
		if err := ensureAgentBead(idx, bd, rigBeadsPath, beads.CrewBeadIDWithPrefix(prefix, rigName, workerName),
			fmt.Sprintf("Crew worker %s in %s - human-managed persistent workspace.", workerName, rigName),
			&beads.AgentFields{RoleType: "crew", Rig: rigName, AgentState: "idle"},
		); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func fixPolecatAgentBeads(ctx *CheckContext, idx *agentBeadIndex, bd *beads.Beads, rigBeadsPath, prefix, rigName string) []error {
	var errs []error
	for _, polecatName := range listPolecats(ctx.TownRoot, rigName) {
		if err := ensureAgentBead(idx, bd, rigBeadsPath, beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName),
			fmt.Sprintf("Polecat worker %s in %s - autonomous worker with persistent identity.", polecatName, rigName),
			&beads.AgentFields{RoleType: "polecat", Rig: rigName, AgentState: "idle"},
		); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func ensureAgentBead(idx *agentBeadIndex, bd *beads.Beads, workDir, id, desc string, fields *beads.AgentFields) error {
	if issue, exists := idx.issues[id]; exists {
		return ensureIssueAgentLabel(bd, workDir, id, issue)
	}
	if idx.wisps[id] {
		ensureOpenWispAgentLabel(bd, id)
		return nil
	}
	reopened, err := reopenClosedAgentBead(bd, id)
	if reopened || err != nil {
		return err
	}
	return createMissingAgentBead(bd, workDir, id, desc, fields)
}

func ensureIssueAgentLabel(bd *beads.Beads, workDir, id string, issue *beads.Issue) error {
	if beads.HasLabel(issue, "gt:agent") {
		return nil
	}
	err := bd.Update(id, beads.UpdateOptions{AddLabels: []string{"gt:agent"}})
	if err != nil {
		sqlErr := addLabelSQL(workDir, id, "gt:agent")
		if sqlErr != nil {
			return fmt.Errorf("adding gt:agent label to %s: bd update: %w; SQL fallback: %v", id, err, sqlErr)
		}
		return nil
	}
	if !verifyLabelAdded(workDir, id, "gt:agent") {
		sqlErr := addLabelSQL(workDir, id, "gt:agent")
		if sqlErr != nil {
			return fmt.Errorf("adding gt:agent label to %s: bd update was no-op, SQL fallback: %w", id, sqlErr)
		}
	}
	return nil
}

func ensureOpenWispAgentLabel(bd *beads.Beads, id string) {
	issue, err := bd.Show(id)
	if err != nil || issue == nil || beads.HasLabel(issue, "gt:agent") {
		return
	}
	_ = bd.Update(id, beads.UpdateOptions{AddLabels: []string{"gt:agent"}})
}

func reopenClosedAgentBead(bd *beads.Beads, id string) (bool, error) {
	issue, err := bd.Show(id)
	if err != nil || issue == nil || issue.Status != "closed" {
		return false, nil
	}
	openStatus := "open"
	if err := bd.Update(id, beads.UpdateOptions{Status: &openStatus}); err != nil {
		return false, fmt.Errorf("reopening closed agent bead %s: %w", id, err)
	}
	if !beads.HasLabel(issue, "gt:agent") {
		_ = bd.Update(id, beads.UpdateOptions{AddLabels: []string{"gt:agent"}})
	}
	return true, nil
}

func createMissingAgentBead(bd *beads.Beads, workDir, id, desc string, fields *beads.AgentFields) error {
	if _, err := bd.CreateAgentBead(id, desc, fields); err != nil {
		return fmt.Errorf("creating %s: %w", id, err)
	}
	_ = addWispLabelSQL(workDir, id, "gt:agent")
	return nil
}

func listCrewWorkers(townRoot, rigName string) []string {
	return listCanonicalRoleDirs(filepath.Join(townRoot, rigName, "crew"))
}

func listPolecats(townRoot, rigName string) []string {
	return listCanonicalRoleDirs(filepath.Join(townRoot, rigName, "polecats"))
}

func listCanonicalRoleDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dotGit := filepath.Join(dir, entry.Name(), ".git")
		if info, err := os.Lstat(dotGit); err == nil && !info.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	return names
}

func addLabelSQL(workDir, beadID, label string) error {
	escapedID := strings.ReplaceAll(beadID, "'", "''")
	escapedLabel := strings.ReplaceAll(label, "'", "''")
	query := fmt.Sprintf("INSERT IGNORE INTO labels (issue_id, label) VALUES ('%s', '%s')", escapedID, escapedLabel)
	return execBdSQLWrite(workDir, query)
}

func addWispLabelSQL(workDir, beadID, label string) error {
	escapedID := strings.ReplaceAll(beadID, "'", "''")
	escapedLabel := strings.ReplaceAll(label, "'", "''")
	query := fmt.Sprintf("INSERT IGNORE INTO wisp_labels (issue_id, label) VALUES ('%s', '%s')", escapedID, escapedLabel)
	return execBdSQLWrite(workDir, query)
}

func verifyLabelAdded(workDir, beadID, label string) bool {
	escapedID := strings.ReplaceAll(beadID, "'", "''")
	escapedLabel := strings.ReplaceAll(label, "'", "''")
	query := fmt.Sprintf("SELECT 1 FROM labels WHERE issue_id = '%s' AND label = '%s' LIMIT 1", escapedID, escapedLabel)
	cmd := beads.Spawn("sql", query) //nolint:gosec // G204: query uses escaped internal values
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "1")
}
