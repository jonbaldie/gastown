package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/style"
)

// doneStage is the next labeled section of gt done after a helper returns.
type doneStage int

const (
	doneStageNotify doneStage = iota
	doneStageAfterMR
)

type doneSession struct {
	exitType    string
	townRoot    string
	cwd         string
	rigName     string
	polecatName string
	sender      string
	worker      string
	agentBeadID string
}

type doneGitState struct {
	g             *git.Git
	branch        string
	defaultBranch string
	baseRef       string
	target        string
}

type doneWorkState struct {
	issueID               string
	mrID                  string
	pushFailed            bool
	mrFailed              bool
	doneErrors            []string
	checkpoints           map[DoneCheckpoint]string
	convoyInfo            *ConvoyInfo
	sourceIssueForNoMerge *beads.Issue
	sourceBD              *beads.Beads
	bd                    *beads.Beads
	priority              int
}

// doneFlow is the shared state for the gt done checkpoint stages.
type doneFlow struct {
	session doneSession
	repo    doneGitState
	work    doneWorkState
}

func runDoneAfterPushThenFinish(f *doneFlow) error {
	stage, err := runDoneAfterPush(f)
	if err != nil {
		return err
	}
	if stage == doneStageAfterMR {
		printDoneAfterMR(f)
	}
	return finishDone(f)
}

func printDoneAfterMR(f *doneFlow) {
	fmt.Printf("  Source: %s\n", f.repo.branch)
	fmt.Printf("  Target: %s\n", f.repo.target)
	fmt.Printf("  Issue: %s\n", f.work.issueID)
	if f.session.worker != "" {
		fmt.Printf("  Worker: %s\n", f.session.worker)
	}
	fmt.Printf("  Priority: P%d\n", f.work.priority)
	fmt.Println()
	fmt.Printf("%s\n", style.Dim.Render("The Refinery will process your merge request."))
}

func printDoneNonCompleted(exitType, issueID, branch string) {
	fmt.Printf("%s Signaling %s\n", style.Bold.Render("→"), exitType)
	if issueID != "" {
		fmt.Printf("  Issue: %s\n", issueID)
	}
	fmt.Printf("  Branch: %s\n", branch)
}

func finishDone(f *doneFlow) error {
	if shouldNudgeRefinery(f.session.exitType, f.work.mrID) {
		nudgeRefinery(f.session.rigName, "MERGE_READY received - check inbox for pending work")
	}

	fmt.Printf("\nNotifying Witness...\n")
	writeDoneCompletionMetadata(f)
	writeDoneWitnessCheckpoint(f)
	logDoneEvents(f)

	if err := updateAgentStateAfterSubmission(f.session.cwd, f.session.townRoot, f.session.exitType, f.work.issueID, f.work.pushFailed, f.work.mrFailed); err != nil {
		return err
	}

	nudgeWitness(f.session.rigName, fmt.Sprintf("POLECAT_DONE %s exit=%s", f.session.polecatName, f.session.exitType))
	fmt.Printf("%s Witness notified of %s (via nudge)\n", style.Bold.Render("✓"), f.session.exitType)

	return retireOrPreserveDoneSession(f)
}

func writeDoneCompletionMetadata(f *doneFlow) {
	if f.session.agentBeadID == "" {
		return
	}
	completionBd := beads.New(f.session.cwd).ForAgentBead()
	meta := &beads.CompletionMetadata{
		ExitType:       f.session.exitType,
		MRID:           f.work.mrID,
		Branch:         f.repo.branch,
		HookBead:       f.work.issueID,
		MRFailed:       f.work.mrFailed,
		PushFailed:     f.work.pushFailed,
		CompletionTime: time.Now().UTC().Format(time.RFC3339),
	}
	if err := completionBd.UpdateAgentCompletion(f.session.agentBeadID, meta); err != nil {
		style.PrintWarning("could not write completion metadata to agent bead: %v", err)
	}
}

func writeDoneWitnessCheckpoint(f *doneFlow) {
	if f.session.agentBeadID == "" {
		return
	}
	cpBd := beads.New(f.session.cwd).ForAgentBead()
	writeDoneCheckpoint(cpBd, f.session.agentBeadID, CheckpointWitnessNotified, "ok")
}

func logDoneEvents(f *doneFlow) {
	if err := LogDone(f.session.townRoot, f.session.sender, f.work.issueID); err != nil {
		style.PrintWarning("could not log done event: %v", err)
	}
	if err := events.LogFeed(events.TypeDone, f.session.sender, events.DonePayload(f.work.issueID, f.repo.branch)); err != nil {
		style.PrintWarning("could not log feed event: %v", err)
	}
}

func retireOrPreserveDoneSession(f *doneFlow) error {
	isPolecat, retirePolecat := decideDonePolecatRetirement(f)
	fmt.Println()
	if !isPolecat {
		fmt.Printf("%s Session exiting\n", style.Bold.Render("→"))
		fmt.Printf("  Witness will handle cleanup.\n")
	}
	if !retirePolecat {
		return nil
	}
	fmt.Printf("%s Terminating polecat session\n", style.Bold.Render("→"))
	reportWorkerDone(f.session.townRoot, f.session.rigName, f.session.polecatName)
	if err := retirePolecatSessionAfterDone(f.session.rigName, f.session.polecatName, os.Getpid()); err != nil {
		style.PrintWarning("could not terminate polecat session: %v", err)
	}
	return nil
}

func decideDonePolecatRetirement(f *doneFlow) (isPolecat, retirePolecat bool) {
	roleInfo, err := GetRoleWithContext(f.session.cwd, f.session.townRoot)
	if err != nil || roleInfo.Role != RolePolecat {
		return false, false
	}
	if f.work.pushFailed || f.work.mrFailed {
		fmt.Printf("%s Work needs recovery (push or MR failed) — session preserved\n", style.Bold.Render("⚠"))
	}
	fillDoneConvoyInfo(f)
	mergeStrategy := ""
	if f.work.convoyInfo != nil {
		mergeStrategy = f.work.convoyInfo.MergeStrategy
	}
	retirePolecat = shouldRetirePolecatSessionAfterDone(f.session.exitType, mergeStrategy, f.work.pushFailed, f.work.mrFailed)
	if retirePolecat {
		fmt.Printf("%s Polecat session retiring after durable handoff\n", style.Bold.Render("✓"))
	} else {
		fmt.Printf("%s Session preserved for recovery or local review\n", style.Bold.Render("→"))
	}
	return true, retirePolecat
}

func fillDoneConvoyInfo(f *doneFlow) {
	if f.session.exitType != ExitCompleted || f.work.issueID == "" || f.work.convoyInfo != nil {
		return
	}
	f.work.convoyInfo = getConvoyInfoFromSourceIssue(f.work.sourceIssueForNoMerge)
	if f.work.convoyInfo == nil {
		f.work.convoyInfo = getConvoyInfoForIssue(f.work.issueID)
	}
}

func runDoneAfterPush(f *doneFlow) (doneStage, error) {
	handled, err := handleNoMergeAfterPush(f)
	if err != nil {
		return 0, err
	}
	if handled {
		return doneStageNotify, nil
	}
	resolveDoneMRTarget(f)
	return resumeOrCreateDoneMR(f)
}

func handleNoMergeAfterPush(f *doneFlow) (bool, error) {
	attachmentFields := beads.ParseAttachmentFields(f.work.sourceIssueForNoMerge)
	if attachmentFields == nil || !attachmentFields.NoMerge {
		return false, nil
	}

	fmt.Printf("%s No-merge mode: skipping merge queue\n", style.Bold.Render("→"))
	fmt.Printf("  Branch: %s\n", f.repo.branch)
	fmt.Printf("  Issue: %s\n", f.work.issueID)
	fmt.Println()

	prURL := maybeCreateNoMergePR(f)
	notifyNoMergeDispatcher(f, attachmentFields.DispatchedBy, prURL)
	if err := closeNoMergeWork(f, attachmentFields, prURL); err != nil {
		return true, err
	}
	return true, nil
}

func maybeCreateNoMergePR(f *doneFlow) string {
	noMergeSettingsPath := filepath.Join(f.session.townRoot, f.session.rigName, "settings", "config.json")
	noMergeSettings, noMergeSettingsErr := config.LoadRigSettings(noMergeSettingsPath)
	if noMergeSettingsErr != nil || noMergeSettings.MergeQueue == nil || noMergeSettings.MergeQueue.MergeStrategy != "pr" {
		fmt.Printf("%s\n", style.Dim.Render("Work stays on feature branch for human review."))
		return ""
	}

	issueTitle := f.work.sourceIssueForNoMerge.Title
	prTitle := fmt.Sprintf("%s (%s)", issueTitle, f.work.issueID)
	if issueTitle == "" {
		prTitle = f.work.issueID
	}
	ghCmd := exec.CommandContext(context.Background(), "gh", "pr", "create",
		"--base", f.repo.defaultBranch,
		"--head", f.repo.branch,
		"--title", prTitle,
		"--body", noMergePRBody(f),
	)
	ghCmd.Dir = f.session.cwd
	prOutput, prErr := ghCmd.Output()
	if prErr != nil {
		style.PrintWarning("could not create GitHub PR: %v", prErr)
		return ""
	}
	prURL := strings.TrimSpace(string(prOutput))
	fmt.Printf("%s GitHub PR created: %s\n", style.Bold.Render("✓"), prURL)
	return prURL
}

func noMergePRBody(f *doneFlow) string {
	var prBodyBuilder strings.Builder
	prBodyBuilder.WriteString("## Summary\n\n")
	if f.work.sourceIssueForNoMerge.Description != "" {
		descLines := strings.Split(f.work.sourceIssueForNoMerge.Description, "\n")
		var cleanDesc []string
		for _, line := range descLines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "attached_") || strings.HasPrefix(trimmed, "dispatched_by:") || strings.HasPrefix(trimmed, "formula_vars:") {
				continue
			}
			cleanDesc = append(cleanDesc, line)
		}
		desc := strings.TrimSpace(strings.Join(cleanDesc, "\n"))
		if desc != "" {
			prBodyBuilder.WriteString(desc)
			prBodyBuilder.WriteString("\n\n")
		}
	}
	if diffStat, diffErr := git.DiffStat(f.repo.g, f.repo.baseRef+"..."+f.repo.branch); diffErr == nil && diffStat != "" {
		prBodyBuilder.WriteString("## Changes\n\n```\n")
		prBodyBuilder.WriteString(diffStat)
		prBodyBuilder.WriteString("```\n\n")
	}
	prBodyBuilder.WriteString("---\n")
	prBodyBuilder.WriteString(fmt.Sprintf("*Polecat: %s | Issue: %s*\n", f.session.worker, f.work.issueID))
	return prBodyBuilder.String()
}

func notifyNoMergeDispatcher(f *doneFlow, dispatcher, prURL string) {
	if dispatcher == "" {
		return
	}
	townRouter := mail.NewRouter(f.session.townRoot)
	defer mail.WaitPendingNotifications(townRouter)
	reviewBody := fmt.Sprintf("Branch: %s\nIssue: %s\nReady for review.", f.repo.branch, f.work.issueID)
	if prURL != "" {
		reviewBody = fmt.Sprintf("Branch: %s\nIssue: %s\nPR: %s\nReady for review.", f.repo.branch, f.work.issueID, prURL)
	}
	reviewMsg := &mail.Message{
		To:      dispatcher,
		From:    detectSender(),
		Subject: fmt.Sprintf("READY_FOR_REVIEW: %s", f.work.issueID),
		Body:    reviewBody,
	}
	if err := townRouter.Send(reviewMsg); err != nil {
		style.PrintWarning("could not notify dispatcher: %v", err)
		return
	}
	fmt.Printf("%s Dispatcher notified: READY_FOR_REVIEW\n", style.Bold.Render("✓"))
}

func closeNoMergeWork(f *doneFlow, attachmentFields *beads.AttachmentFields, prURL string) error {
	if f.work.issueID == "" {
		return nil
	}
	noMergeBd := f.work.sourceBD
	if noMergeBd == nil {
		noMergeBd = f.work.bd
	}
	canCloseIssue, err := prepareNoMergeClose(f, noMergeBd, attachmentFields)
	if err != nil {
		return err
	}
	if !canCloseIssue {
		return nil
	}
	closeReason := "No-merge work completed; merge queue skipped"
	if prURL != "" {
		closeReason = fmt.Sprintf("%s\npr_url: %s", closeReason, prURL)
	}
	if closeErr := forceCloseIssueWithRetry(
		noMergeBd.ForceCloseWithReason,
		f.work.issueID,
		closeReason,
		"Issue %s closed (no-merge)",
	); closeErr != nil {
		style.PrintWarning("could not close issue %s after 3 attempts: %v (issue may be left HOOKED)", f.work.issueID, closeErr)
	}
	return nil
}

func prepareNoMergeClose(f *doneFlow, noMergeBd *beads.Beads, attachmentFields *beads.AttachmentFields) (bool, error) {
	if skipReason, fatal := doneSourceCloseSkipReason(noMergeBd, f.work.issueID, f.work.sourceIssueForNoMerge); skipReason != "" {
		style.PrintWarning("%s", skipReason)
		notifyDoneCloseSkipped(f.session.townRoot, f.session.rigName, f.session.sender, f.work.issueID, skipReason)
		if fatal {
			return false, fmt.Errorf("cannot complete review-only/no-merge work: %s", skipReason)
		}
		return false, nil
	}
	if attachmentFields.AttachedMolecule == "" {
		return true, nil
	}
	if n := closeDescendants(noMergeBd, attachmentFields.AttachedMolecule); n > 0 {
		fmt.Fprintf(os.Stderr, "Closed %d molecule step(s) for %s\n", n, attachmentFields.AttachedMolecule)
	}
	if closeErr := forceCloseIssueWithRetry(
		noMergeBd.ForceCloseWithReason,
		attachmentFields.AttachedMolecule,
		"done",
		"Attached molecule %s closed",
	); closeErr != nil && !errors.Is(closeErr, beads.ErrNotFound) {
		style.PrintWarning("could not close attached molecule %s after 3 attempts: %v", attachmentFields.AttachedMolecule, closeErr)
		return false, nil
	}
	return true, nil
}

func resolveDoneMRTarget(f *doneFlow) {
	f.repo.target = f.repo.defaultBranch
	if applyExplicitDoneTarget(f) {
		assignDoneMRPriority(f)
		return
	}
	applyFormulaDoneTarget(f)
	applyIntegrationDoneTarget(f)
	assignDoneMRPriority(f)
}

func applyExplicitDoneTarget(f *doneFlow) bool {
	if doneState().target == "" {
		return false
	}
	f.repo.target = doneState().target
	fmt.Printf("  Target branch: %s (from --target flag)\n", f.repo.target)
	return true
}

func applyFormulaDoneTarget(f *doneFlow) {
	if f.repo.target != f.repo.defaultBranch || f.work.sourceIssueForNoMerge == nil {
		return
	}
	af := beads.ParseAttachmentFields(f.work.sourceIssueForNoMerge)
	if af == nil {
		return
	}
	bb := extractFormulaVar(af.FormulaVars, "base_branch")
	if bb == "" || bb == f.repo.defaultBranch {
		return
	}
	f.repo.target = bb
	fmt.Printf("  Target branch override: %s (from formula_vars)\n", f.repo.target)
}

func applyIntegrationDoneTarget(f *doneFlow) {
	if f.repo.target != f.repo.defaultBranch {
		return
	}
	refineryEnabled := true
	settingsPath := filepath.Join(f.session.townRoot, f.session.rigName, "settings", "config.json")
	if settings, err := config.LoadRigSettings(settingsPath); err == nil && settings.MergeQueue != nil {
		refineryEnabled = config.IsRefineryIntegrationEnabled(settings.MergeQueue)
	}
	if !refineryEnabled {
		return
	}
	autoTarget, err := beads.DetectIntegrationBranch(f.work.sourceBD, git.Checker{Git: f.repo.g}, f.work.issueID)
	if err == nil && autoTarget != "" {
		f.repo.target = autoTarget
	}
}

func assignDoneMRPriority(f *doneFlow) {
	if doneState().priority >= 0 {
		f.work.priority = doneState().priority
		return
	}
	f.work.priority = f.work.sourceIssueForNoMerge.Priority
}

func resumeOrCreateDoneMR(f *doneFlow) (doneStage, error) {
	resumed, notify, err := tryResumeDoneMRCheckpoint(f)
	if err != nil {
		return 0, err
	}
	if notify {
		return doneStageNotify, nil
	}
	if resumed {
		return doneStageAfterMR, nil
	}

	findOrCreateDoneMR(f)
	if f.work.mrFailed {
		return doneStageNotify, nil
	}
	if f.work.mrID != "" && f.session.agentBeadID != "" {
		cpBd := beads.New(f.session.cwd).ForAgentBead()
		writeDoneCheckpoint(cpBd, f.session.agentBeadID, CheckpointMRCreated, f.work.mrID)
	}
	return doneStageAfterMR, nil
}

func tryResumeDoneMRCheckpoint(f *doneFlow) (resumed bool, notify bool, err error) {
	if f.work.checkpoints[CheckpointMRCreated] == "" {
		return false, false, nil
	}
	cpMRID := f.work.checkpoints[CheckpointMRCreated]
	cpMR, cpErr := f.work.bd.Show(cpMRID)
	if cpErr != nil || cpMR == nil {
		return false, false, nil
	}
	branchPrefix := "branch: " + f.repo.branch + "\n"
	if !strings.HasPrefix(cpMR.Description, branchPrefix) {
		fmt.Printf("→ Discarding stale MR checkpoint %s (was for different branch)\n", cpMRID)
		return false, false, nil
	}
	if err := validateMergeRequestSource(cpMR, f.work.issueID, f.work.sourceIssueForNoMerge); err != nil {
		f.work.mrFailed = true
		errMsg := fmt.Sprintf("checkpoint MR validation failed: %v", err)
		f.work.doneErrors = append(f.work.doneErrors, errMsg)
		style.PrintWarning("%s\nBranch is pushed but MR bead not trusted. Witness will be notified.", errMsg)
		return false, true, nil
	}
	f.work.mrID = cpMRID
	fmt.Printf("%s MR already created (resumed from checkpoint: %s)\n", style.Bold.Render("✓"), f.work.mrID)
	return true, false, nil
}

func findOrCreateDoneMR(f *doneFlow) {
	commitSHA, _ := git.Rev(f.repo.g, "HEAD")
	existingMR, err := findExistingDoneMR(f, commitSHA)
	if err != nil {
		style.PrintWarning("could not check for existing MR: %v", err)
	}
	if existingMR != nil {
		reuseExistingDoneMR(f, existingMR)
		return
	}
	createDoneMRBead(f, commitSHA)
}

func findExistingDoneMR(f *doneFlow, commitSHA string) (*beads.Issue, error) {
	if commitSHA != "" {
		return f.work.bd.FindMRForBranchAndSHA(f.repo.branch, commitSHA)
	}
	return f.work.bd.FindMRForBranch(f.repo.branch)
}

func reuseExistingDoneMR(f *doneFlow, existingMR *beads.Issue) {
	if err := validateMergeRequestSource(existingMR, f.work.issueID, f.work.sourceIssueForNoMerge); err != nil {
		f.work.mrFailed = true
		errMsg := fmt.Sprintf("existing MR validation failed: %v", err)
		f.work.doneErrors = append(f.work.doneErrors, errMsg)
		style.PrintWarning("%s\nBranch is pushed but existing MR bead not trusted. Witness will be notified.", errMsg)
		return
	}
	f.work.mrID = existingMR.ID
	fmt.Printf("%s MR already exists (idempotent)\n", style.Bold.Render("✓"))
	fmt.Printf("  MR ID: %s\n", style.Bold.Render(f.work.mrID))
}

func createDoneMRBead(f *doneFlow, commitSHA string) {
	mrIssue, err := f.work.bd.Create(beads.CreateOptions{
		Title:       fmt.Sprintf("Merge: %s", f.work.issueID),
		Labels:      []string{"gt:merge-request"},
		Priority:    f.work.priority,
		Description: doneMRDescription(f, commitSHA),
		Ephemeral:   true,
		Rig:         f.session.rigName,
	})
	if !acceptCreatedDoneMR(f, mrIssue, err) {
		return
	}
	if prefixErr := beads.ValidateRigPrefix(f.session.townRoot, f.session.rigName, f.work.mrID); prefixErr != nil {
		style.PrintWarning("MR bead prefix mismatch: %v\nThe refinery may not find this MR — check 'gt mq list %s'", prefixErr, f.session.rigName)
	}
	supersedeOlderDoneMRs(f)
	linkDoneMRBead(f)
	fmt.Printf("%s Work submitted to merge queue (verified)\n", style.Bold.Render("✓"))
	fmt.Printf("  MR ID: %s\n", style.Bold.Render(f.work.mrID))
}

func acceptCreatedDoneMR(f *doneFlow, mrIssue *beads.Issue, err error) bool {
	if err != nil {
		errMsg := fmt.Sprintf("MR bead creation failed: %v", err)
		markDoneMRFailed(f, errMsg)
		style.PrintWarning("%s\nBranch is pushed but MR bead not created. Witness will be notified.", errMsg)
		return false
	}
	f.work.mrID = mrIssue.ID
	if f.work.mrID == "" {
		errMsg := "MR bead creation returned empty ID"
		markDoneMRFailed(f, errMsg)
		style.PrintWarning("%s\nBranch is pushed but MR bead has no ID. Witness will be notified.", errMsg)
		return false
	}
	verifiedMR, verifyErr := f.work.bd.Show(f.work.mrID)
	if verifyErr != nil || verifiedMR == nil {
		errMsg := fmt.Sprintf("MR bead created but verification read-back failed (id=%s): %v", f.work.mrID, verifyErr)
		markDoneMRFailed(f, errMsg)
		style.PrintWarning("%s\nBranch is pushed but MR bead not confirmed. Preserving worktree.", errMsg)
		return false
	}
	return true
}

func linkDoneMRBead(f *doneFlow) {
	if f.session.agentBeadID != "" {
		if err := f.work.bd.ForAgentBead().UpdateAgentActiveMR(f.session.agentBeadID, f.work.mrID); err != nil {
			style.PrintWarning("could not update agent bead with active_mr: %v", err)
		}
	}
	if f.work.issueID == "" {
		return
	}
	comment := fmt.Sprintf("MR created: %s", f.work.mrID)
	if err := f.work.sourceBD.AddComment(f.work.issueID, comment); err != nil {
		style.PrintWarning("could not back-link source issue %s to MR %s: %v", f.work.issueID, f.work.mrID, err)
	}
}

func doneMRDescription(f *doneFlow, commitSHA string) string {
	description := fmt.Sprintf("branch: %s\ntarget: %s\nsource_issue: %s\nrig: %s",
		f.repo.branch, f.repo.target, f.work.issueID, f.session.rigName)
	if commitSHA != "" {
		description += fmt.Sprintf("\ncommit_sha: %s", commitSHA)
	}
	if doneState().skipVerify {
		description += "\nskip_verify: true"
	}
	if f.session.worker != "" {
		description += fmt.Sprintf("\nworker: %s", f.session.worker)
	}
	if f.session.agentBeadID != "" {
		description += fmt.Sprintf("\nagent_bead: %s", f.session.agentBeadID)
	}
	description += "\nretry_count: 0"
	description += "\nlast_conflict_sha: null"
	description += "\nconflict_task_id: null"
	if !doneState().preVerified {
		return description
	}
	description += "\npre_verified: true"
	description += fmt.Sprintf("\npre_verified_at: %s", time.Now().UTC().Format(time.RFC3339))
	verifiedBaseRef := git.CleanBaseRef(f.repo.g, "origin", f.repo.defaultBranch, f.repo.target)
	if verifiedBase, baseErr := git.Rev(f.repo.g, verifiedBaseRef); baseErr == nil {
		description += fmt.Sprintf("\npre_verified_base: %s", verifiedBase)
		return description
	} else {
		style.PrintWarning("could not resolve %s for pre-verified base: %v (pre-verification data incomplete)", verifiedBaseRef, baseErr)
	}
	return description
}

func markDoneMRFailed(f *doneFlow, errMsg string) {
	f.work.mrFailed = true
	f.work.doneErrors = append(f.work.doneErrors, errMsg)
}

func supersedeOlderDoneMRs(f *doneFlow) {
	if f.work.issueID == "" {
		return
	}
	oldMRs, findErr := f.work.bd.FindOpenMRsForIssue(f.work.issueID)
	if findErr != nil {
		return
	}
	for _, old := range oldMRs {
		if old.ID == f.work.mrID {
			continue
		}
		reason := fmt.Sprintf("superseded by %s", f.work.mrID)
		if closeErr := f.work.bd.CloseWithReason(reason, old.ID); closeErr != nil {
			style.PrintWarning("could not supersede old MR %s: %v", old.ID, closeErr)
			continue
		}
		fmt.Printf("  %s Superseded old MR: %s\n", style.Dim.Render("○"), old.ID)
	}
}
