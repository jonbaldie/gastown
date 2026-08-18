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

func gatherWorkstateInput(name string, bd *beads.Beads, clonePath string, state State, beadID string) WorkstateInput {
	input := WorkstateInput{State: state, CleanupStatus: CleanupUnknown}
	if bd == nil {
		input.GitCheckFailed = true
		input.GitCheckFailedReason = "beads-unavailable"
		return input
	}

	agentID := workstateAgentID(name, clonePath)
	agentBD := bd.ForAgentBead()
	_, fields, err := agentBD.GetAgentBead(agentID)
	hookSafe := true
	hookTerminal := false
	if err != nil {
		input.GitCheckFailed = true
		input.GitCheckFailedReason = fmt.Sprintf("agent bead lookup: %v", err)
	}

	activeMR := ""
	sourceHint := ""
	if err == nil && fields != nil {
		var hookErr error
		hookSafe, hookTerminal, hookErr = hookBeadSafeForWorkstate(bd, fields.HookBead)
		if hookErr != nil {
			input.GitCheckFailed = true
			input.GitCheckFailedReason = fmt.Sprintf("hook bead lookup: %v", hookErr)
		}
		if !hookSafe {
			input.HookBead = fields.HookBead
		}
		input.PushFailed = fields.PushFailed
		input.MRFailed = fields.MRFailed
		input.ActiveMR = fields.ActiveMR
		activeMR = fields.ActiveMR
		sourceHint = beadID
		if sourceHint == "" {
			sourceHint = fields.LastSourceIssue
		}
		if sourceHint == "" {
			sourceHint = fields.HookBead
		}
		if fields.CleanupStatus != "" {
			input.CleanupStatus = CleanupStatus(fields.CleanupStatus)
		}
		if blocker := agentStateWorkBlocker(fields.AgentState); blocker != "" {
			input.ActiveWorkBlocker = blocker
			input.ActiveWorkCountsTowardCapacity = agentStateCountsTowardCapacity(fields.AgentState)
		}
	}

	g := git.NewGit(clonePath)
	branch, branchErr := g.CurrentBranch()
	if branchErr != nil {
		input.GitCheckFailed = true
		input.GitCheckFailedReason = fmt.Sprintf("git branch: %v", branchErr)
	} else {
		input.Branch = branch
	}
	targetRefs, targetRefLookupFailed := reuseTargetRefs(bd, fields, branch)
	if targetRefLookupFailed {
		input.MQLookupFailed = true
	}
	if status, err := g.CheckUncommittedWork(); err == nil {
		input.GitDirty = !status.CleanExcludingRuntime()
		input.StashCount = status.StashCount
		input.UnpushedCommits = status.UnpushedCommits
	} else {
		input.GitCheckFailed = true
		input.GitCheckFailedReason = fmt.Sprintf("git worktree: %v", err)
	}
	if branch != "" {
		if preservation, err := g.BranchPreservationStatus(branch, "origin", targetRefs); err == nil {
			input.UnpushedCommits = preservation.UnpreservedPatchCount
		} else {
			input.GitCheckFailed = true
			input.GitCheckFailedReason = fmt.Sprintf("git preservation: %v", err)
		}
	}

	gitSafe := !input.GitCheckFailed && !input.GitDirty && input.StashCount == 0 && input.UnpushedCommits == 0
	if input.CleanupStatus == CleanupUnknown && gitSafe {
		input.CleanupStatus = CleanupClean
	}

	activeMRSafe := true
	sourceTerminal := sourceHint != "" && assignedBeadTerminal(bd, sourceHint)
	if activeMR != "" {
		assessment := AssessActiveMR(bd, ActiveMRInput{ActiveMR: activeMR, SourceIssueHint: sourceHint, RequireGitSafe: true, GitSafe: gitSafe})
		if assessment.Pending {
			input.ActiveMRBlocker = assessment.Reason
		}
		activeMRSafe = !assessment.Pending
		if assessment.SourceTerminal {
			sourceTerminal = true
		}
	}

	assignedBeadID := beadID
	if assignedBeadID == "" {
		assignedBeadID = sourceHint
	}
	applyAssignedWorkBlocker(&input, bd, assignedBeadID)
	input.MQCheckRequired = input.Branch != ""
	input.HasSubmittableWork = hasSubmittableWorkForWorkstate(clonePath, targetRefs)
	input.AssignedBeadTerminal = assignedBeadTerminal(bd, assignedBeadID)
	workTerminal := input.AssignedBeadTerminal || sourceTerminal || hookTerminal
	if CanIgnoreStaleCleanupStatus(input.CleanupStatus, workTerminal, hookSafe, activeMRSafe, gitSafe) {
		input.IgnoreCleanupStatus = true
	}
	input.MQNotRequired = mqNotRequiredSource(bd, assignedBeadID)
	if input.MQCheckRequired && input.HasSubmittableWork && !input.AssignedBeadTerminal && !input.MQNotRequired {
		mr, err := bd.FindMRForBranchAny(input.Branch)
		if err != nil {
			input.MQLookupFailed = true
		} else {
			input.MRSubmitted = mr != nil
		}
	}
	return input
}

func applyAssignedWorkBlocker(input *WorkstateInput, bd *beads.Beads, beadID string) {
	if input == nil || beadID == "" || input.ActiveWorkBlocker != "" {
		return
	}
	bead, err := bd.Show(beadID)
	if err != nil || bead == nil {
		if err != nil && !errors.Is(err, beads.ErrNotFound) {
			input.ActiveWorkBlocker = fmt.Sprintf("assigned_work status=lookup_error: %v", err)
			input.ActiveWorkCountsTowardCapacity = true
		}
		return
	}
	if beads.IsAgentBead(bead) || beads.IsProtectedBead(bead) || beads.IssueStatus(bead.Status).IsTerminal() {
		return
	}
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
	var refs []string
	lookupFailed := false
	if fields.ActiveMR != "" {
		if issue, err := bd.Show(fields.ActiveMR); err == nil {
			if mrFields := beads.ParseMRFields(issue); mrFields != nil && mrFields.Target != "" {
				refs = append(refs, mrFields.Target)
			}
		} else if !errors.Is(err, beads.ErrNotFound) {
			lookupFailed = true
		}
	}
	if branch != "" {
		if issue, err := bd.FindMRForBranchAny(branch); err == nil {
			if mrFields := beads.ParseMRFields(issue); mrFields != nil && mrFields.Target != "" {
				refs = append(refs, mrFields.Target)
			}
		} else if !errors.Is(err, beads.ErrNotFound) {
			lookupFailed = true
		}
	}
	var sourceBeadIDs []string
	if fields.LastSourceIssue != "" && fields.LastSourceIssue != fields.HookBead {
		sourceBeadIDs = append(sourceBeadIDs, fields.LastSourceIssue)
	}
	if fields.HookBead != "" {
		sourceBeadIDs = append(sourceBeadIDs, fields.HookBead)
	}
	for _, sourceBeadID := range sourceBeadIDs {
		bead, err := bd.Show(sourceBeadID)
		if err != nil {
			lookupFailed = true
			continue
		}
		refs = append(refs, attachmentTargetRefs(bd, bead)...)
	}
	return uniqueRefs(refs), lookupFailed
}
