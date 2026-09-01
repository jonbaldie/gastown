package polecat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
)

// InspectWorkstate is the one Workstate implementation. Callers pass a
// Polecat name, the rig Beads store, the clone path, the known session
// state, and an optional assigned Bead ID. This function reads agent Beads,
// Hook Bead status, git on the clone, the active merge request, and
// merge-queue facts, then returns the disposition.
func InspectWorkstate(name string, bd *beads.Beads, clonePath string, state State, beadID string) WorkstateDisposition {
	return DecideWorkstate(gatherWorkstateInput(name, bd, clonePath, state, beadID))
}

// ClonePathFor returns the Polecat clone path, preferring the nested
// polecats/<name>/<rig>/ layout and falling back to the older
// polecats/<name>/ worktree.
func ClonePathFor(rigPath, rigName, name string) string {
	newPath := filepath.Join(rigPath, "polecats", name, rigName)
	if info, err := os.Stat(newPath); err == nil && info.IsDir() {
		return newPath
	}
	oldPath := filepath.Join(rigPath, "polecats", name)
	if info, err := os.Stat(oldPath); err == nil && info.IsDir() {
		if _, err := os.Stat(filepath.Join(oldPath, ".git")); err == nil {
			return oldPath
		}
	}
	return newPath
}

type workstateGather struct {
	input          WorkstateInput
	bd             *beads.Beads
	clonePath      string
	beadID         string
	fields         *beads.AgentFields
	activeMR       string
	sourceHint     string
	targetRefs     []string
	hookSafe       bool
	hookTerminal   bool
	gitSafe        bool
	activeMRSafe   bool
	sourceTerminal bool
}

func gatherWorkstateInput(name string, bd *beads.Beads, clonePath string, state State, beadID string) WorkstateInput {
	g := workstateGather{
		input:        WorkstateInput{State: state, CleanupStatus: CleanupUnknown},
		bd:           bd,
		clonePath:    clonePath,
		beadID:       beadID,
		hookSafe:     true,
		activeMRSafe: true,
	}
	if bd == nil {
		g.input.GitCheckFailed = true
		g.input.GitCheckFailedReason = "beads-unavailable"
		return g.input
	}
	applyAgentBeadWorkstate(&g, name)
	applyGitWorkstate(&g)
	applyActiveMRWorkstate(&g)
	applyAssignedMQWorkstate(&g)
	return g.input
}

func applyAgentBeadWorkstate(g *workstateGather, name string) {
	agentID := workstateAgentID(name, g.clonePath)
	_, fields, err := g.bd.ForAgentBead().GetAgentBead(agentID)
	if err != nil {
		g.input.GitCheckFailed = true
		g.input.GitCheckFailedReason = fmt.Sprintf("agent bead lookup: %v", err)
		return
	}
	if fields == nil {
		return
	}
	g.fields = fields
	applyHookBeadWorkstate(g)
	applyAgentFieldWorkstate(g)
}

func applyHookBeadWorkstate(g *workstateGather) {
	var hookErr error
	g.hookSafe, g.hookTerminal, hookErr = hookBeadSafeForWorkstate(g.bd, g.fields.HookBead)
	if hookErr != nil {
		g.input.GitCheckFailed = true
		g.input.GitCheckFailedReason = fmt.Sprintf("hook bead lookup: %v", hookErr)
	}
	if !g.hookSafe {
		g.input.HookBead = g.fields.HookBead
	}
}

func applyAgentFieldWorkstate(g *workstateGather) {
	g.input.PushFailed = g.fields.PushFailed
	g.input.MRFailed = g.fields.MRFailed
	g.input.ActiveMR = g.fields.ActiveMR
	g.activeMR = g.fields.ActiveMR
	g.sourceHint = firstNonEmpty(g.beadID, g.fields.LastSourceIssue, g.fields.HookBead)
	if g.fields.CleanupStatus != "" {
		g.input.CleanupStatus = CleanupStatus(g.fields.CleanupStatus)
	}
	if blocker := agentStateWorkBlocker(g.fields.AgentState); blocker != "" {
		g.input.ActiveWorkBlocker = blocker
		g.input.ActiveWorkCountsTowardCapacity = agentStateCountsTowardCapacity(g.fields.AgentState)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func applyGitWorkstate(g *workstateGather) {
	repo := git.NewGit(g.clonePath)
	applyGitBranchWorkstate(g, repo)
	applyGitDirtyWorkstate(g, repo)
	applyGitPreservationWorkstate(g, repo)
	g.gitSafe = !g.input.GitCheckFailed && !g.input.GitDirty && g.input.StashCount == 0 && g.input.UnpushedCommits == 0
	if g.input.CleanupStatus == CleanupUnknown && g.gitSafe {
		g.input.CleanupStatus = CleanupClean
	}
}

func applyGitBranchWorkstate(g *workstateGather, repo *git.Git) {
	branch, branchErr := git.CurrentBranch(repo)
	if branchErr != nil {
		g.input.GitCheckFailed = true
		g.input.GitCheckFailedReason = fmt.Sprintf("git branch: %v", branchErr)
	} else {
		g.input.Branch = branch
	}
	targetRefs, targetRefLookupFailed := reuseTargetRefs(g.bd, g.fields, branch)
	g.targetRefs = targetRefs
	if targetRefLookupFailed {
		g.input.MQLookupFailed = true
	}
}

func applyGitDirtyWorkstate(g *workstateGather, repo *git.Git) {
	status, err := git.CheckUncommittedWork(repo)
	if err != nil {
		g.input.GitCheckFailed = true
		g.input.GitCheckFailedReason = fmt.Sprintf("git worktree: %v", err)
		return
	}
	g.input.GitDirty = !status.CleanExcludingRuntime()
	g.input.StashCount = status.StashCount
	g.input.UnpushedCommits = status.UnpushedCommits
}

func applyGitPreservationWorkstate(g *workstateGather, repo *git.Git) {
	if g.input.Branch == "" {
		return
	}
	preservation, err := git.CheckBranchPreservation(repo, g.input.Branch, "origin", g.targetRefs)
	if err != nil {
		g.input.GitCheckFailed = true
		g.input.GitCheckFailedReason = fmt.Sprintf("git preservation: %v", err)
		return
	}
	g.input.UnpushedCommits = preservation.UnpreservedPatchCount
}

func applyActiveMRWorkstate(g *workstateGather) {
	g.sourceTerminal = g.sourceHint != "" && assignedBeadTerminal(g.bd, g.sourceHint)
	if g.activeMR == "" {
		return
	}
	assessment := AssessActiveMR(g.bd, ActiveMRInput{ActiveMR: g.activeMR, SourceIssueHint: g.sourceHint, RequireGitSafe: true, GitSafe: g.gitSafe})
	if assessment.Pending {
		g.input.ActiveMRBlocker = assessment.Reason
	}
	g.activeMRSafe = !assessment.Pending
	if assessment.SourceTerminal {
		g.sourceTerminal = true
	}
}

func applyAssignedMQWorkstate(g *workstateGather) {
	assignedBeadID := firstNonEmpty(g.beadID, g.sourceHint)
	applyAssignedWorkBlocker(&g.input, g.bd, assignedBeadID)
	g.input.MQCheckRequired = g.input.Branch != ""
	g.input.HasSubmittableWork = hasSubmittableWorkForWorkstate(g.clonePath, g.targetRefs)
	g.input.AssignedBeadTerminal = assignedBeadTerminal(g.bd, assignedBeadID)
	workTerminal := g.input.AssignedBeadTerminal || g.sourceTerminal || g.hookTerminal
	if CanIgnoreStaleCleanupStatus(g.input.CleanupStatus, workTerminal, g.hookSafe, g.activeMRSafe, g.gitSafe) {
		g.input.IgnoreCleanupStatus = true
	}
	g.input.MQNotRequired = mqNotRequiredSource(g.bd, assignedBeadID)
	applyMRSubmittedWorkstate(g)
}

func applyMRSubmittedWorkstate(g *workstateGather) {
	if !g.input.MQCheckRequired || !g.input.HasSubmittableWork || g.input.AssignedBeadTerminal || g.input.MQNotRequired {
		return
	}
	mr, err := g.bd.FindMRForBranchAny(g.input.Branch)
	if err != nil {
		g.input.MQLookupFailed = true
		return
	}
	g.input.MRSubmitted = mr != nil
}

func applyAssignedWorkBlocker(input *WorkstateInput, bd *beads.Beads, beadID string) {
	if input == nil || beadID == "" || input.ActiveWorkBlocker != "" {
		return
	}
	bead, err := bd.Show(beadID)
	if !assignedBeadUsable(input, bead, err) {
		return
	}
	applyAssignedWorkCapacity(input, bead)
}

func assignedBeadUsable(input *WorkstateInput, bead *beads.Issue, err error) bool {
	if err != nil || bead == nil {
		recordAssignedLookupError(input, err)
		return false
	}
	return !beads.IsAgentBead(bead) && !beads.IsProtectedBead(bead) && !beads.IssueStatus(bead.Status).IsTerminal()
}

func recordAssignedLookupError(input *WorkstateInput, err error) {
	if err == nil || errors.Is(err, beads.ErrNotFound) {
		return
	}
	input.ActiveWorkBlocker = fmt.Sprintf("assigned_work status=lookup_error: %v", err)
	input.ActiveWorkCountsTowardCapacity = true
}

func applyAssignedWorkCapacity(input *WorkstateInput, bead *beads.Issue) {
	input.ActiveWorkBlocker = fmt.Sprintf("assigned_work=%s status=%s", bead.ID, bead.Status)
	switch beads.IssueStatus(bead.Status) {
	case beads.IssueStatusHooked, beads.StatusInProgress, beads.StatusOpen:
		input.ActiveWorkCountsTowardCapacity = true
	}
}

func agentStateWorkBlocker(state string) string {
	agentState := beads.AgentState(strings.TrimSpace(state))
	if agentState == "" || agentState == beads.AgentStateIdle || agentState == beads.AgentStateDone || agentState == beads.AgentStateNuked {
		return ""
	}
	if agentState.IsActive() || agentState.ProtectsFromCleanup() || agentState == beads.AgentStateEscalated {
		return fmt.Sprintf("agent_state=%s", agentState)
	}
	return ""
}

func agentStateCountsTowardCapacity(state string) bool {
	return beads.AgentState(strings.TrimSpace(state)).IsActive()
}

func workstateAgentID(name, clonePath string) string {
	townRoot, rigName := inferTownAndRig(name, clonePath)
	if rigName == "" {
		return ""
	}
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	if prefix == "" {
		prefix = "gt"
	}
	return beads.PolecatBeadIDWithPrefix(prefix, rigName, name)
}

func inferTownAndRig(name, clonePath string) (townRoot, rigName string) {
	clean := filepath.Clean(clonePath)
	parent := filepath.Dir(clean)
	grand := filepath.Dir(parent)
	if filepath.Base(parent) == name && filepath.Base(grand) == "polecats" {
		rigName = filepath.Base(clean)
		rigPath := filepath.Dir(grand)
		return filepath.Dir(rigPath), filepath.Base(rigPath)
	}
	if filepath.Base(clean) == name && filepath.Base(parent) == "polecats" {
		rigPath := filepath.Dir(parent)
		return filepath.Dir(rigPath), filepath.Base(rigPath)
	}
	return "", ""
}

func hookBeadSafeForWorkstate(bd *beads.Beads, hookBead string) (safe bool, terminal bool, err error) {
	if hookBead == "" {
		return true, false, nil
	}
	if bd == nil {
		return false, false, fmt.Errorf("beads unavailable")
	}
	bead, err := bd.Show(hookBead)
	if err != nil {
		return false, false, err
	}
	if bead == nil {
		return false, false, fmt.Errorf("hook bead %s missing", hookBead)
	}
	if beads.IssueStatus(bead.Status).IsTerminal() {
		return true, true, nil
	}
	return false, false, nil
}

func assignedBeadTerminal(bd *beads.Beads, beadID string) bool {
	if beadID == "" || bd == nil {
		return false
	}
	bead, err := bd.Show(beadID)
	return err == nil && bead != nil && beads.IssueStatus(bead.Status).IsTerminal()
}

func mqNotRequiredSource(bd *beads.Beads, beadID string) bool {
	if beadID == "" || bd == nil {
		return false
	}
	bead, err := bd.Show(beadID)
	if err != nil || bead == nil {
		return false
	}
	attachment := beads.ParseAttachmentFields(bead)
	if attachment == nil {
		return false
	}
	return attachment.NoMerge || attachment.ReviewOnly || strings.EqualFold(strings.TrimSpace(attachment.MergeStrategy), "local")
}

func reuseTargetRefs(bd *beads.Beads, fields *beads.AgentFields, branch string) ([]string, bool) {
	if fields == nil || bd == nil {
		return nil, false
	}
	issue, err, present := showIssue(bd, fields.ActiveMR)
	refs, failed := appendIssueTargetRef(nil, false, issue, err, present)
	issue, err, present = findBranchMR(bd, branch)
	refs, failed = appendIssueTargetRef(refs, failed, issue, err, present)
	extra, extraFailed := attachmentRefsForSources(bd, sourceBeadIDsForRefs(fields))
	return uniqueRefs(append(refs, extra...)), failed || extraFailed
}

func showIssue(bd *beads.Beads, id string) (*beads.Issue, error, bool) {
	if id == "" {
		return nil, nil, false
	}
	issue, err := bd.Show(id)
	return issue, err, true
}

func findBranchMR(bd *beads.Beads, branch string) (*beads.Issue, error, bool) {
	if branch == "" {
		return nil, nil, false
	}
	issue, err := bd.FindMRForBranchAny(branch)
	return issue, err, true
}

func appendIssueTargetRef(refs []string, failed bool, issue *beads.Issue, err error, present bool) ([]string, bool) {
	if !present {
		return refs, failed
	}
	if err != nil {
		if !errors.Is(err, beads.ErrNotFound) {
			return refs, true
		}
		return refs, failed
	}
	if mrFields := beads.ParseMRFields(issue); mrFields != nil && mrFields.Target != "" {
		refs = append(refs, mrFields.Target)
	}
	return refs, failed
}

func sourceBeadIDsForRefs(fields *beads.AgentFields) []string {
	var sourceBeadIDs []string
	if fields.LastSourceIssue != "" && fields.LastSourceIssue != fields.HookBead {
		sourceBeadIDs = append(sourceBeadIDs, fields.LastSourceIssue)
	}
	if fields.HookBead != "" {
		sourceBeadIDs = append(sourceBeadIDs, fields.HookBead)
	}
	return sourceBeadIDs
}

func attachmentRefsForSources(bd *beads.Beads, sourceBeadIDs []string) ([]string, bool) {
	var refs []string
	lookupFailed := false
	for _, sourceBeadID := range sourceBeadIDs {
		bead, err := bd.Show(sourceBeadID)
		if err != nil {
			lookupFailed = true
			continue
		}
		refs = append(refs, attachmentTargetRefs(bd, bead)...)
	}
	return refs, lookupFailed
}
