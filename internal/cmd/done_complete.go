package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/style"
)

type doneCompletedStage int

const (
	doneCompletedNotify doneCompletedStage = iota
	doneCompletedAfterPush
	doneCompletedContinue
)

func runDoneCompleted(flow *doneFlow) (doneCompletedStage, error) {
	stage, err := prepareDoneCompleted(flow)
	if err != nil || stage != doneCompletedContinue {
		return stage, err
	}
	return submitDoneCompleted(flow)
}

func prepareDoneCompleted(flow *doneFlow) (doneCompletedStage, error) {
	if err := verifyDoneCompletedBranch(flow.repo.branch, flow.repo.defaultBranch); err != nil {
		return 0, err
	}
	if err := verifyDoneUncommittedForComplete(flow.repo.g, flow.session.cwd); err != nil {
		return 0, err
	}
	aheadCount := doneCommitsAhead(flow.repo.g, flow.repo.baseRef, flow.repo.defaultBranch, flow.repo.branch)
	sourceFlags, err := loadDoneSourceFlags(flow.session.cwd, flow.work.issueID)
	if err != nil {
		return 0, err
	}
	flow.work.sourceIssueForNoMerge = sourceFlags.issue
	flow.work.sourceBD = sourceFlags.bd
	if aheadCount == 0 {
		if err := handleDoneZeroCommit(flow, sourceFlags.noMerge); err != nil {
			return 0, err
		}
		return doneCompletedNotify, nil
	}
	if sourceFlags.reviewOnly {
		return 0, fmt.Errorf("cannot complete review-only issue %s with commits ahead of %s; add a fresh review evidence comment and complete without code changes", flow.work.issueID, flow.repo.baseRef)
	}
	aheadCount, err = rebaseDoneIfContaminated(flow, aheadCount)
	if err != nil {
		return 0, err
	}
	if stripped := stripOverlayInstructionFiles(flow.repo.g, flow.repo.defaultBranch, flow.repo.baseRef); stripped {
		aheadCount, _ = git.CommitsAhead(flow.repo.g, flow.repo.baseRef, "HEAD")
	}
	_ = aheadCount
	return doneCompletedContinue, nil
}

func submitDoneCompleted(flow *doneFlow) (doneCompletedStage, error) {
	flow.work.convoyInfo = resolveDoneConvoy(flow.work.sourceIssueForNoMerge, flow.work.issueID)
	if doneSkipPushForLocalStrategy(flow.work.convoyInfo, flow.work.sourceIssueForNoMerge) {
		printDoneLocalStrategy(flow.repo.branch, flow.work.issueID)
		return doneCompletedNotify, nil
	}
	if flow.work.convoyInfo != nil && flow.work.convoyInfo.MergeStrategy == "direct" {
		return applyDoneDirectMerge(flow, flow.work.convoyInfo.ID, false)
	}
	if flow.work.issueID == "" {
		return 0, fmt.Errorf("cannot determine source issue from branch '%s'; use --issue to specify", flow.repo.branch)
	}
	warnLocalDoneBeadsDir(flow.session.cwd)
	flow.work.bd = beads.NewWithBeadsDir(flow.session.cwd, beads.ResolveBeadsDir(flow.session.cwd))
	flow.work.convoyInfo = resolveDoneConvoy(flow.work.sourceIssueForNoMerge, flow.work.issueID)
	if flow.work.convoyInfo != nil && flow.work.convoyInfo.MergeStrategy == "direct" {
		return applyDoneDirectMerge(flow, flow.work.convoyInfo.ID, true)
	}
	if stage := resumeDonePushCheckpoint(flow); stage != doneCompletedContinue {
		return stage, nil
	}
	return finishDoneFeaturePush(flow)
}

func warnLocalDoneBeadsDir(cwd string) {
	resolvedBeads := beads.ResolveBeadsDir(cwd)
	if !beads.IsLocalBeadsDir(cwd, resolvedBeads) {
		return
	}
	fmt.Fprintf(os.Stderr, "WARNING: beads resolved to local dir %s (no shared-beads redirect)\n", resolvedBeads)
	fmt.Fprintf(os.Stderr, "  MR beads written here will be invisible to the Refinery — run 'gt polecat repair' to fix\n")
}

func applyDoneDirectMerge(flow *doneFlow, convoyID string, late bool) (doneCompletedStage, error) {
	direct, err := handleDoneDirectMerge(flow, convoyID, late)
	if err != nil {
		return 0, err
	}
	flow.work.pushFailed = direct.pushFailed
	flow.work.doneErrors = append(flow.work.doneErrors, direct.doneErrors...)
	return doneCompletedNotify, nil
}

func resumeDonePushCheckpoint(flow *doneFlow) doneCompletedStage {
	if flow.work.checkpoints[CheckpointPushed] == "" {
		return doneCompletedContinue
	}
	if pushCheckpointMatchesBranch(flow.work.checkpoints, flow.repo.branch) {
		fmt.Printf("%s Branch already pushed (resumed from checkpoint)\n", style.Bold.Render("✓"))
		return doneCompletedAfterPush
	}
	fmt.Printf("→ Discarding stale push checkpoint (was for branch %s, now on %s)\n",
		flow.work.checkpoints[CheckpointPushed], flow.repo.branch)
	return doneCompletedContinue
}

func finishDoneFeaturePush(flow *doneFlow) (doneCompletedStage, error) {
	pushed, err := pushDoneFeatureBranch(flow.repo.g, flow.session.townRoot, flow.session.rigName, flow.repo.branch, flow.repo.baseRef, flow.work.issueID, flow.session.agentBeadID, flow.session.cwd, flow.work.sourceBD)
	if err != nil {
		return 0, err
	}
	flow.work.pushFailed = pushed.pushFailed
	flow.work.doneErrors = append(flow.work.doneErrors, pushed.doneErrors...)
	if pushed.afterPush {
		return doneCompletedAfterPush, nil
	}
	return doneCompletedNotify, nil
}

func verifyDoneCompletedBranch(branch, defaultBranch string) error {
	if branch == defaultBranch || branch == "master" {
		return fmt.Errorf("cannot submit %s/master branch to merge queue", defaultBranch)
	}
	return nil
}

func verifyDoneUncommittedForComplete(g *git.Git, cwd string) error {
	workStatus, err := git.CheckUncommittedWork(g)
	if err != nil {
		return fmt.Errorf("checking git status: %w", err)
	}
	if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingSafetyNet(cwd) {
		return fmt.Errorf("cannot complete: uncommitted changes would be lost\nCommit your changes first, or use --status DEFERRED to exit without completing\nUncommitted: %s", workStatus.String())
	}
	return nil
}

func doneCommitsAhead(g *git.Git, baseRef, defaultBranch, branch string) int {
	aheadCount, err := git.CommitsAhead(g, baseRef, "HEAD")
	if err == nil {
		return aheadCount
	}
	aheadCount, err = git.CommitsAhead(g, defaultBranch, branch)
	if err != nil {
		style.PrintWarning("could not check commits ahead of %s: %v", defaultBranch, err)
		return 1
	}
	return aheadCount
}

type doneSourceFlags struct {
	issue      *beads.Issue
	bd         *beads.Beads
	noMerge    bool
	reviewOnly bool
}

func loadDoneSourceFlags(cwd, issueID string) (doneSourceFlags, error) {
	if issueID == "" {
		return doneSourceFlags{}, nil
	}
	sourceInfo, sourceErr := resolveSubmitSourceIssue(cwd, issueID)
	if sourceErr != nil {
		return doneSourceFlags{}, fmt.Errorf("source issue validation failed: %w", sourceErr)
	}
	flags := doneSourceFlags{issue: sourceInfo.Issue, bd: sourceInfo.BD}
	if af := beads.ParseAttachmentFields(sourceInfo.Issue); af != nil {
		flags.noMerge = af.NoMerge || af.ReviewOnly
		flags.reviewOnly = af.ReviewOnly
	}
	return flags, nil
}

func handleDoneZeroCommit(flow *doneFlow, isNoMergeTask bool) error {
	g := flow.repo.g
	if os.Getenv("GT_POLECAT") != "" && doneState().cleanupStatus != "clean" && !isNoMergeTask {
		if !doneBranchAlreadyPushedWithWork(g, flow.repo.branch, flow.repo.defaultBranch) {
			return fmt.Errorf("cannot complete: no commits on branch ahead of %s\n"+
				"Polecats must have at least 1 commit to submit.\n"+
				"If the bug was already fixed upstream: gt done --status DEFERRED\n"+
				"If you're blocked: gt done --status ESCALATED",
				flow.repo.baseRef)
		}
	}
	fmt.Printf("%s Branch has no commits ahead of %s\n", style.Bold.Render("→"), flow.repo.baseRef)
	fmt.Printf("  Work was likely already merged or report-only.\n")
	fmt.Printf("  Skipping MR creation - completing without merge request.\n\n")
	return closeDoneNoMRWork(flow, isNoMergeTask)
}

func doneBranchAlreadyPushedWithWork(g *git.Git, branch, defaultBranch string) bool {
	if branch == defaultBranch {
		return false
	}
	pushed, unpushed, pushErr := git.BranchPushedToRemote(g, branch, "origin")
	return pushErr == nil && pushed && unpushed == 0
}

func closeDoneNoMRWork(flow *doneFlow, isNoMergeTask bool) error {
	issueID := flow.work.issueID
	if issueID == "" {
		return nil
	}
	bd := flow.work.sourceBD
	if bd == nil {
		bd = beads.New(flow.session.cwd)
	}
	if skipReason, fatal := doneNoMRSourceCloseSkipReason(bd, issueID, flow.work.sourceIssueForNoMerge); skipReason != "" {
		style.PrintWarning("%s", skipReason)
		fmt.Printf("  The bead will remain open for witness/mayor review.\n")
		notifyDoneCloseSkipped(flow.session.townRoot, flow.session.rigName, flow.session.sender, issueID, skipReason)
		if fatal {
			return fmt.Errorf("cannot complete review-only/no-MR work: %s", skipReason)
		}
		return nil
	}
	closeReason, err := doneNoMRCloseReason(flow, isNoMergeTask, bd)
	if err != nil {
		return err
	}
	if closeErr := forceCloseIssueWithRetry(bd.ForceCloseWithReason, issueID, closeReason, "Issue %s closed (no MR needed)"); closeErr != nil {
		style.PrintWarning("could not close issue %s after 3 attempts: %v (issue may be left HOOKED)", issueID, closeErr)
	}
	return nil
}

func doneNoMRCloseReason(flow *doneFlow, isNoMergeTask bool, bd *beads.Beads) (string, error) {
	closeReason := "Completed with no code changes (already fixed or already merged)"
	noMRCommitSHA, _ := git.Rev(flow.repo.g, "HEAD")
	cwd := flow.session.cwd
	issueID := flow.work.issueID
	defaultBranch := flow.repo.defaultBranch
	if doneState().skipVerify {
		noteVerifiedPushSkipped(bd, cwd, issueID, defaultBranch, noMRCommitSHA, "--skip-verify on no-MR close")
		if noMRCommitSHA != "" {
			closeReason = fmt.Sprintf("%s\nskip_verify: true\ntarget_branch: %s\ncommit_sha: %s", closeReason, defaultBranch, noMRCommitSHA)
		}
		return closeReason, nil
	}
	if isNoMergeTask {
		return closeReason, nil
	}
	if git.ForkBackedRemote(flow.repo.g, "origin") {
		return "", fmt.Errorf("cannot close no-MR code bead in fork/upstream mode: %s has no commits ahead of %s; use the fork PR flow instead", flow.repo.branch, flow.repo.baseRef)
	}
	if verifyErr := git.VerifyPushedCommitReachableFromPushTarget(flow.repo.g, "origin", defaultBranch, noMRCommitSHA); verifyErr != nil {
		noteVerifiedPushFailure(bd, cwd, issueID, defaultBranch, noMRCommitSHA, verifyErr)
		return "", fmt.Errorf("cannot close no-MR code bead: %w", verifyErr)
	}
	if noMRCommitSHA != "" {
		closeReason = fmt.Sprintf("%s\ntarget_branch: %s\ncommit_sha: %s", closeReason, defaultBranch, noMRCommitSHA)
	}
	return closeReason, nil
}

func rebaseDoneIfContaminated(flow *doneFlow, aheadCount int) (int, error) {
	contaminationBase, fetchRemote := doneContaminationFetchTarget(flow.repo.defaultBranch, flow.repo.baseRef)
	if fetchErr := git.Fetch(flow.repo.g, fetchRemote); fetchErr != nil {
		style.PrintWarning("could not fetch %s before contamination check: %v (proceeding with local refs)", fetchRemote, fetchErr)
	}
	contam, err := git.CheckBranchContamination(flow.repo.g, contaminationBase)
	if err != nil || contam.Behind <= 0 {
		return aheadCount, nil
	}
	return applyDoneContaminationRebase(flow, contaminationBase, fetchRemote, contam.Behind, aheadCount)
}

func doneContaminationFetchTarget(defaultBranch, baseRef string) (string, string) {
	contaminationBase := baseRef
	if doneState().target != "" && doneState().target != defaultBranch {
		contaminationBase = doneContaminationBaseRef(defaultBranch, doneState().target)
	}
	fetchRemote := git.RemoteForRef(contaminationBase)
	if fetchRemote == "" {
		fetchRemote = "origin"
	}
	return contaminationBase, fetchRemote
}

func applyDoneContaminationRebase(flow *doneFlow, contaminationBase, fetchRemote string, behind, aheadCount int) (int, error) {
	const warnThreshold = 50
	const blockThreshold = 200
	if behind >= blockThreshold {
		return 0, fmt.Errorf("branch contamination: %d commits behind %s (threshold: %d)\n"+
			"The branch is severely stale and will include unrelated changes in the PR.\n"+
			"Fix: git fetch %s && git rebase %s",
			behind, contaminationBase, blockThreshold, fetchRemote, contaminationBase)
	}
	if behind >= warnThreshold {
		style.PrintWarning("branch is %d commits behind %s — consider rebasing to avoid PR contamination", behind, contaminationBase)
	}
	alreadyPushed := flow.work.checkpoints[CheckpointPushed] == flow.repo.branch
	rebased, skipReason, rebaseErr := autoRebaseOnTarget(gitRebaseAdapter{flow.repo.g}, contaminationBase, behind, doneState().preVerified, alreadyPushed)
	if rebaseErr != nil {
		return 0, rebaseErr
	}
	if rebased {
		fmt.Printf("%s Branch rebased onto %s\n", style.Bold.Render("✓"), contaminationBase)
		aheadCount, _ = git.CommitsAhead(flow.repo.g, flow.repo.baseRef, "HEAD")
	} else if skipReason != "" {
		style.PrintWarning("branch is %d commits behind %s but %s; skipping auto-rebase", behind, contaminationBase, skipReason)
	}
	return aheadCount, nil
}

func resolveDoneConvoy(sourceIssue *beads.Issue, issueID string) *ConvoyInfo {
	if info := getConvoyInfoFromSourceIssue(sourceIssue); info != nil {
		return info
	}
	return getConvoyInfoForIssue(issueID)
}

func printDoneLocalStrategy(branch, issueID string) {
	fmt.Printf("%s Local merge strategy: skipping push and merge queue\n", style.Bold.Render("→"))
	fmt.Printf("  Branch: %s\n", branch)
	if issueID != "" {
		fmt.Printf("  Issue: %s\n", issueID)
	}
	fmt.Println()
	fmt.Printf("%s\n", style.Dim.Render("Work stays on local feature branch."))
}

type doneDirectResult struct {
	pushFailed bool
	doneErrors []string
}

func handleDoneDirectMerge(flow *doneFlow, convoyID string, late bool) (doneDirectResult, error) {
	msgs := doneDirectMergeMessages(flow.repo.defaultBranch, late)
	fmt.Printf("%s %s: pushing to %s\n", style.Bold.Render("→"), msgs.label, flow.repo.defaultBranch)
	if late && convoyID != "" {
		fmt.Printf("  Convoy: %s\n", convoyID)
	}
	directBd := flow.work.sourceBD
	if directBd == nil {
		directBd = flow.work.bd
	}
	if directBd == nil {
		directBd = beads.New(flow.session.cwd)
	}
	if skipReason := doneDirectMergeSkipReason(directBd, flow.work.issueID, flow.work.sourceIssueForNoMerge, flow.repo.defaultBranch); skipReason != "" {
		style.PrintWarning("%s", skipReason)
		notifyDoneCloseSkipped(flow.session.townRoot, flow.session.rigName, flow.session.sender, flow.work.issueID, skipReason)
		return doneDirectResult{}, fmt.Errorf("cannot complete direct-merge work: %s", skipReason)
	}
	return pushAndCloseDoneDirect(flow, directBd, msgs)
}

type doneDirectMessages struct {
	label, verifyReason, verifyFailSuffix, closeReason string
}

func doneDirectMergeMessages(defaultBranch string, late bool) doneDirectMessages {
	msgs := doneDirectMessages{
		label:            "Direct merge strategy",
		verifyReason:     "--skip-verify on direct merge",
		verifyFailSuffix: "Direct merge pushed but remote verification failed. Source bead will remain in progress.",
		closeReason:      fmt.Sprintf("Direct merge to %s (convoy strategy)", defaultBranch),
	}
	if !late {
		return msgs
	}
	msgs.label = "Late-detected direct merge strategy"
	msgs.verifyReason = "--skip-verify on late direct merge"
	msgs.verifyFailSuffix = "Late direct merge pushed but remote verification failed. Source bead will remain in progress."
	msgs.closeReason = fmt.Sprintf("Direct merge to %s (convoy strategy, late detection)", defaultBranch)
	return msgs
}

func pushAndCloseDoneDirect(flow *doneFlow, directBd *beads.Beads, msgs doneDirectMessages) (doneDirectResult, error) {
	pushSubmoduleChanges(flow.repo.g, flow.repo.baseRef)
	directRefspec := flow.repo.branch + ":" + flow.repo.defaultBranch
	if directPushErr := git.Push(flow.repo.g, "origin", directRefspec, false); directPushErr != nil {
		errMsg := fmt.Sprintf("direct push to %s failed: %v", flow.repo.defaultBranch, directPushErr)
		style.PrintWarning("%s", errMsg)
		return doneDirectResult{pushFailed: true, doneErrors: []string{errMsg}}, nil
	}
	if result, failed := verifyDoneDirectPush(flow, directBd, msgs); failed {
		return result, nil
	}
	fmt.Printf("%s Branch pushed directly to %s\n", style.Bold.Render("✓"), flow.repo.defaultBranch)
	doneState().cleanupStatus = cleanupStatusAfterSuccessfulPush(doneState().cleanupStatus)
	if err := closeDoneDirectIssue(flow, directBd, msgs.closeReason); err != nil {
		return doneDirectResult{}, err
	}
	return doneDirectResult{}, nil
}

func verifyDoneDirectPush(flow *doneFlow, directBd *beads.Beads, msgs doneDirectMessages) (doneDirectResult, bool) {
	directCommitSHA, _ := git.Rev(flow.repo.g, "HEAD")
	if doneState().skipVerify {
		noteVerifiedPushSkipped(directBd, flow.session.cwd, flow.work.issueID, flow.repo.defaultBranch, directCommitSHA, msgs.verifyReason)
		return doneDirectResult{}, false
	}
	verifyErr := git.VerifyPushedCommitReachableFromPushTarget(flow.repo.g, "origin", flow.repo.defaultBranch, directCommitSHA)
	if verifyErr == nil {
		return doneDirectResult{}, false
	}
	errMsg := verifyErr.Error()
	noteVerifiedPushFailure(directBd, flow.session.cwd, flow.work.issueID, flow.repo.defaultBranch, directCommitSHA, verifyErr)
	style.PrintWarning("%s\n%s", errMsg, msgs.verifyFailSuffix)
	return doneDirectResult{pushFailed: true, doneErrors: []string{errMsg}}, true
}

func closeDoneDirectIssue(flow *doneFlow, directBd *beads.Beads, closeReason string) error {
	issueID := flow.work.issueID
	if issueID == "" {
		return nil
	}
	if skipReason, fatal := doneSourceCloseSkipReason(directBd, issueID, flow.work.sourceIssueForNoMerge); skipReason != "" {
		style.PrintWarning("%s", skipReason)
		notifyDoneCloseSkipped(flow.session.townRoot, flow.session.rigName, flow.session.sender, issueID, skipReason)
		if fatal {
			return fmt.Errorf("cannot complete direct-merge work: %s", skipReason)
		}
		return nil
	}
	if closeErr := forceCloseIssueWithRetry(directBd.ForceCloseWithReason, issueID, closeReason, "Issue %s closed (direct merge)"); closeErr != nil {
		style.PrintWarning("could not close issue %s after 3 attempts: %v", issueID, closeErr)
	}
	return nil
}

type donePushStage struct {
	afterPush  bool
	pushFailed bool
	doneErrors []string
}

func pushDoneFeatureBranch(g *git.Git, townRoot, rigName, branch, baseRef, issueID, agentBeadID, cwd string, sourceBD *beads.Beads) (donePushStage, error) {
	pushSubmoduleChanges(g, baseRef)
	fmt.Printf("Pushing branch to remote...\n")
	refspec := branch + ":" + branch
	pushedCommitSHA, _ := git.Rev(g, "HEAD")
	pushErr := git.Push(g, "origin", refspec, false)
	if pushErr != nil {
		pushErr = retryDonePushFromBareRepo(townRoot, rigName, refspec, pushErr)
	}
	if pushErr != nil {
		errMsg := fmt.Sprintf("push failed for branch '%s': %v", branch, pushErr)
		if doneTreatPushAsLocalFallback(pushErr) {
			style.PrintWarning("%s\nOrigin is not writable. Keeping work on the local branch; this is not an agent failure.", errMsg)
			fmt.Printf("%s Local fallback: skipping merge queue. Use --push-url or --merge=local for third-party remotes.\n", style.Bold.Render("→"))
			return donePushStage{}, nil
		}
		style.PrintWarning("%s\nCommits exist locally but failed to push. Witness will be notified.", errMsg)
		return donePushStage{pushFailed: true, doneErrors: []string{errMsg}}, nil
	}
	if pushedCommitSHA == "" {
		pushedCommitSHA, _ = git.Rev(g, "HEAD")
	}
	if doneState().skipVerify {
		noteVerifiedPushSkipped(sourceBD, cwd, issueID, branch, pushedCommitSHA, "--skip-verify on branch push")
	} else if verifyErr := verifyPushedCommitWithBareFallback(g, townRoot, rigName, branch, pushedCommitSHA); verifyErr != nil {
		errMsg := verifyErr.Error()
		noteVerifiedPushFailure(sourceBD, cwd, issueID, branch, pushedCommitSHA, verifyErr)
		style.PrintWarning("%s\nCommits exist locally but verified push failed. Witness will be notified.", errMsg)
		return donePushStage{pushFailed: true, doneErrors: []string{errMsg}}, nil
	}
	fmt.Printf("%s Branch pushed to origin\n", style.Bold.Render("✓"))
	doneState().cleanupStatus = cleanupStatusAfterSuccessfulPush(doneState().cleanupStatus)
	if agentBeadID != "" {
		cpBd := beads.New(cwd).ForAgentBead()
		writeDoneCheckpoint(cpBd, agentBeadID, CheckpointPushed, branch)
	}
	return donePushStage{afterPush: true}, nil
}

func retryDonePushFromBareRepo(townRoot, rigName, refspec string, pushErr error) error {
	style.PrintWarning("primary push failed: %v — trying bare repo fallback...", pushErr)
	bareRepoPath := filepath.Join(townRoot, rigName, ".repo.git")
	if _, statErr := os.Stat(bareRepoPath); statErr != nil {
		return pushErr
	}
	bareGit := git.NewGitWithDir(bareRepoPath, "")
	if fallbackErr := git.Push(bareGit, "origin", refspec, false); fallbackErr != nil {
		style.PrintWarning("bare repo push also failed: %v", fallbackErr)
		return fallbackErr
	}
	fmt.Printf("%s Branch pushed via bare repo fallback\n", style.Bold.Render("✓"))
	return nil
}
