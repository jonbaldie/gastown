package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/instructions"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
)

func autoDetectDoneCleanupStatus(g *git.Git, cwd, branch string) {
	if doneState().cleanupStatus != "" {
		return
	}
	workStatus, err := g.CheckUncommittedWork()
	if err != nil {
		style.PrintWarning("could not auto-detect cleanup status: %v", err)
		return
	}
	pushed, unpushedCount, pushErr := g.BranchPushedToRemote(branch, "origin")
	if pushErr != nil {
		style.PrintWarning("could not check if branch is pushed: %v", pushErr)
	}
	doneState().cleanupStatus = cleanupStatusFromWorkState(workStatus, cwd, pushed, unpushedCount, pushErr)
}

func autoPopDoneStashes(g *git.Git) {
	if doneState().cleanupStatus != "stash" {
		return
	}
	entries, err := g.StashListForBranch()
	if err != nil {
		style.PrintWarning("auto-pop: could not list stashes: %v — orphaned stashes may remain", err)
		return
	}
	if len(entries) == 0 {
		return
	}
	fmt.Printf("\n%s %d stash(es) detected on this branch — auto-popping (gt-pvx safety net)\n",
		style.Bold.Render("⚠"), len(entries))
	if !popDoneStashesOldestFirst(g, entries) {
		return
	}
	workStatus, wsErr := g.CheckUncommittedWork()
	if wsErr == nil && workStatus.HasUncommittedChanges {
		doneState().cleanupStatus = "uncommitted"
		fmt.Printf("%s Stash content moved to working tree — will auto-commit below.\n",
			style.Bold.Render("✓"))
		return
	}
	doneState().cleanupStatus = ""
}

func popDoneStashesOldestFirst(g *git.Git, entries []git.StashEntry) bool {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		fmt.Printf("  popping %s — %s\n", e.Ref, e.Message)
		if popErr := g.StashPop(e.Ref); popErr != nil {
			style.PrintWarning("auto-pop %s failed (likely conflict): %v", e.Ref, popErr)
			style.PrintWarning("stopping pop chain — resolve conflict manually then re-run gt done")
			return false
		}
		var err error
		entries, err = g.StashListForBranch()
		if err != nil || len(entries) == 0 {
			break
		}
	}
	return true
}

func autoCommitDoneUncommittedWork(g *git.Git, cwd, branch string) error {
	if doneState().cleanupStatus != "uncommitted" {
		return nil
	}
	workStatus, err := g.CheckUncommittedWork()
	if err != nil || !workStatus.HasUncommittedChanges || workStatus.CleanExcludingSafetyNet(cwd) {
		return nil
	}
	if len(workStatus.UnmergedFiles) > 0 {
		return fmt.Errorf("cannot auto-save unmerged conflicts: %s\nResolve conflicts first, or use --status DEFERRED to exit without completing", strings.Join(workStatus.UnmergedFiles, ", "))
	}

	fmt.Printf("\n%s Uncommitted changes detected — auto-saving to prevent work loss\n", style.Bold.Render("⚠"))
	fmt.Printf("  Files: %s\n\n", workStatus.String())
	if addErr := g.StageSafetyNet(); addErr != nil {
		style.PrintWarning("auto-commit: git add failed: %v — uncommitted work may be at risk", addErr)
		return nil
	}
	unstageDoneOverlayFiles(g, cwd)
	commitDoneSafetyNet(g, branch)
	return nil
}

func unstageDoneOverlayFiles(g *git.Git, cwd string) {
	_ = g.ResetFiles("CLAUDE.local.md")
	_ = g.ResetFiles("AGENTS.local.md")
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		data, readErr := os.ReadFile(filepath.Join(cwd, name))
		if readErr != nil {
			continue
		}
		if instructions.IsGasTownOverlay(string(data)) {
			_ = g.ResetFiles(name)
		}
	}
}

func commitDoneSafetyNet(g *git.Git, branch string) {
	autoMsg := "fix: auto-save uncommitted implementation work (gt-pvx safety net)"
	if issueFromBranch := parseBranchName(branch).Issue; issueFromBranch != "" {
		autoMsg = fmt.Sprintf("fix: auto-save uncommitted implementation work (%s, gt-pvx safety net)", issueFromBranch)
	}
	staged, stagedErr := g.HasStagedChanges()
	if stagedErr != nil {
		style.PrintWarning("auto-commit: checking staged changes failed: %v — uncommitted work may be at risk", stagedErr)
		return
	}
	if !staged {
		fmt.Printf("  No source changes to auto-save (binaries and runtime artifacts stay uncommitted).\n\n")
		return
	}
	if commitErr := g.Commit(autoMsg); commitErr != nil {
		style.PrintWarning("auto-commit: git commit failed: %v — uncommitted work may be at risk", commitErr)
		return
	}
	fmt.Printf("%s Auto-committed uncommitted work (safety net)\n", style.Bold.Render("✓"))
	fmt.Printf("  The agent should have committed before running gt done.\n")
	fmt.Printf("  This auto-save prevents work loss.\n\n")
	doneState().cleanupStatus = "unpushed"
}

type doneIssueContext struct {
	issueID     string
	worker      string
	agentBeadID string
	sender      string
	checkpoints map[DoneCheckpoint]string
}

func resolveDoneIssueContext(cwd, townRoot, branch, sender string) (doneIssueContext, error) {
	info := parseBranchName(branch)
	issueID := doneState().issue
	if issueID == "" {
		issueID = info.Issue
	}
	ctx := doneIssueContext{
		issueID:     issueID,
		worker:      info.Worker,
		sender:      sender,
		checkpoints: map[DoneCheckpoint]string{},
	}
	loadDoneAgentBead(&ctx, cwd, townRoot)
	if err := applyDoneAssignedIssue(&ctx, cwd, branch, info.Issue); err != nil {
		return doneIssueContext{}, err
	}
	writeDoneIntentAndCheckpoints(&ctx, cwd)
	return ctx, nil
}

func loadDoneAgentBead(ctx *doneIssueContext, cwd, townRoot string) {
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return
	}
	if actor := roleInfo.ActorString(); actor != "" {
		ctx.sender = actor
	}
	roleCtx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	ctx.agentBeadID = getAgentBeadID(roleCtx)
	ensureAgentBeadExists(beads.New(cwd).ForAgentBead(), ctx.agentBeadID, roleCtx)
}

func applyDoneAssignedIssue(ctx *doneIssueContext, cwd, branch, branchIssue string) error {
	assigned := loadDoneAssignedIssues(cwd, ctx.sender)
	if err := inferDoneIssueFromHook(ctx, assigned); err != nil {
		return err
	}
	return applyStaleBranchIssue(ctx, assigned, branch, branchIssue)
}

func loadDoneAssignedIssues(cwd, sender string) []string {
	if sender == "" {
		return nil
	}
	return findAssignedBeadsForAgent(cwd, sender)
}

func inferDoneIssueFromHook(ctx *doneIssueContext, assigned []string) error {
	if ctx.issueID != "" || ctx.sender == "" {
		return nil
	}
	hookIssue, ambiguous := selectAssignedIssue("", assigned)
	if hookIssue != "" {
		ctx.issueID = hookIssue
		return nil
	}
	if ambiguous {
		return fmt.Errorf("multiple active assignments found for %s; cannot infer issue from hook. Use --issue to disambiguate", ctx.sender)
	}
	return nil
}

func applyStaleBranchIssue(ctx *doneIssueContext, assigned []string, branch, branchIssue string) error {
	if doneState().issue != "" || branchIssue == "" || ctx.sender == "" {
		return nil
	}
	hookIssue, ambiguous := selectAssignedIssue(branchIssue, assigned)
	if isStaleBranchIssue(branchIssue, hookIssue) {
		style.PrintWarning("branch %q embeds issue %s but your hooked bead is %s — submitting for %s (stale branch reuse?)", branch, branchIssue, hookIssue, hookIssue)
		fmt.Printf("  Fresh branches must be named polecat/<name>/<bead-id>+<suffix> for the bead you are working.\n")
		fmt.Printf("  Use --issue to override if the branch-derived id is actually correct.\n\n")
		ctx.issueID = hookIssue
		return nil
	}
	if ambiguous {
		return fmt.Errorf("branch %q embeds issue %s but %s has multiple active assignments; use --issue to disambiguate", branch, branchIssue, ctx.sender)
	}
	return nil
}

func writeDoneIntentAndCheckpoints(ctx *doneIssueContext, cwd string) {
	if ctx.agentBeadID == "" {
		return
	}
	bd := beads.New(cwd).ForAgentBead()
	setDoneIntentLabel(bd, ctx.agentBeadID, strings.ToUpper(doneState().status))
	ctx.checkpoints = readDoneCheckpoints(bd, ctx.agentBeadID)
	if len(ctx.checkpoints) > 0 {
		fmt.Printf("%s Resuming gt done from checkpoint (previous run was interrupted)\n", style.Bold.Render("→"))
	}
}

func doneDefaultBranch(townRoot, rigName string) string {
	defaultBranch := "main"
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	return defaultBranch
}

func touchDoneExitingHeartbeat(townRoot, issueID string) {
	sessionName := os.Getenv("GT_SESSION")
	if sessionName == "" || townRoot == "" {
		return
	}
	polecat.TouchSessionHeartbeatWithState(townRoot, sessionName, polecat.HeartbeatExiting, "gt done", issueID)
}
