package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/sling"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/worker"
)

type townSlingRun struct {
	ctx              context.Context
	intent           sling.Intent
	townRoot         string
	beadsDir         string
	result           *SlingResult
	info             *beadInfo
	dest             slingDestination
	originalStatus   string
	originalAssignee string
	formulaName      string
	convoyID         string
	beadToHook       string
	work             townSlingWork
	dog              townSlingDogState
}

type townSlingWork struct {
	vars               []string
	formulaCooked      bool
	rollbackConvoyID   string
	formulaVars        string
	actor              string
	targetPane         string
	freshlySpawned     bool
	explicitForce      bool
	attachedMoleculeID string
}

type townSlingDogState struct {
	description      string
	previousCleared  bool
	dispatchComplete bool
}

func runTownSling(ctx context.Context, intent sling.Intent) (*SlingResult, error) {
	r, unlock, err := beginTownSling(ctx, intent)
	if err != nil {
		if r != nil {
			return r.result, err
		}
		return nil, err
	}
	defer unlock()
	return executeTownSling(r)
}

func executeTownSling(r *townSlingRun) (*SlingResult, error) {
	done, err := checkTownSlingReady(r)
	if done || err != nil {
		return r.result, err
	}
	if err := attachTownSlingDestination(r); err != nil {
		return r.result, err
	}
	if r.dest.Admission != nil {
		defer r.dest.Admission.Release()
	}
	defer rollbackTownSlingDog(r)
	return continueTownSling(r)
}

func continueTownSling(r *townSlingRun) (*SlingResult, error) {
	if err := prepareTownSlingWork(r); err != nil {
		return r.result, err
	}
	if r.intent.DryRun {
		printSlingDryRun(r.intent, r.info, r.formulaName, r.dest.Agent, r.dest.Pane, r.convoyID)
		r.result.Success = true
		return r.result, nil
	}
	return completeTownSling(r)
}

func completeTownSling(r *townSlingRun) (*SlingResult, error) {
	fieldUpdates, err := cookAndAttachTownSling(r)
	if err != nil {
		return r.result, err
	}
	if err := hookTownSling(r, fieldUpdates); err != nil {
		return r.result, err
	}
	return finishTownSling(r)
}

func beginTownSling(ctx context.Context, intent sling.Intent) (*townSlingRun, func(), error) {
	townRoot := intent.TownRoot
	if townRoot == "" {
		var err error
		townRoot, err = findTownRoot()
		if err != nil {
			return nil, nil, err
		}
	}
	releaseLock, err := tryAcquireSlingBeadLock(townRoot, intent.BeadID)
	if err != nil {
		return &townSlingRun{result: &SlingResult{BeadID: intent.BeadID, ErrMsg: err.Error()}}, nil, err
	}
	beadsDir := intent.BeadsDir
	if beadsDir == "" {
		beadsDir = filepath.Join(townRoot, ".beads")
	}
	intent.BeadID = followMovedBead(intent.BeadID, townRoot)
	return &townSlingRun{
		ctx:      ctx,
		intent:   intent,
		townRoot: townRoot,
		beadsDir: beadsDir,
		result:   &SlingResult{BeadID: intent.BeadID},
	}, releaseLock, nil
}

func checkTownSlingReady(r *townSlingRun) (bool, error) {
	if live, liveErr := worker.LiveRunFromStore(r.townRoot, r.intent.BeadID); liveErr == nil && live != nil {
		r.result.ErrMsg = "live run"
		return true, fmt.Errorf("%w: bead %s already has live run %s", worker.ErrLiveRun, r.intent.BeadID, live.RunID)
	}
	if err := rejectParkedTownSling(r); err != nil {
		return true, err
	}
	info, err := getBeadInfoFromTownRoot(r.townRoot, r.intent.BeadID)
	if err != nil {
		r.result.ErrMsg = err.Error()
		return true, fmt.Errorf("could not get bead info: %w", err)
	}
	r.info = info
	r.work.explicitForce = r.intent.Force
	sameTarget, formulaRefresh := slingSameTarget(r.intent, info, r.townRoot)
	status := evaluateSlingStatus(info, r.intent.BeadID, r.work.explicitForce, r.intent.Merge, sameTarget, formulaRefresh)
	if status.Err != nil {
		r.result.ErrMsg = status.ErrMsg
		return true, status.Err
	}
	if status.NoOp {
		r.result.Success = true
		r.result.NoOp = true
		if name, ok := polecatNameFromAssignee(r.intent.RigName, info.Assignee); ok {
			r.result.PolecatName = name
		}
		return true, nil
	}
	r.intent.Force = status.Force
	r.intent.Merge = status.Merge
	r.originalStatus = info.Status
	r.originalAssignee = info.Assignee
	return false, nil
}

func rejectParkedTownSling(r *townSlingRun) error {
	if r.intent.RigName == "" {
		return nil
	}
	blocked, reason := IsRigParkedOrDocked(r.townRoot, r.intent.RigName)
	if !blocked {
		return nil
	}
	r.result.ErrMsg = "rig " + reason
	undoCmd := "gt rig unpark"
	if reason == "docked" {
		undoCmd = "gt rig undock"
	}
	return fmt.Errorf("cannot sling to %s rig %q\n%s %s", reason, r.intent.RigName, undoCmd, r.intent.RigName)
}

func attachTownSlingDestination(r *townSlingRun) error {
	if err := r.ctx.Err(); err != nil {
		r.result.ErrMsg = err.Error()
		return err
	}
	dest, movedID, err := resolveSlingDestination(r.ctx, r.intent, r.townRoot, r.work.explicitForce)
	if err != nil {
		r.result.ErrMsg = err.Error()
		return err
	}
	if err := applyMovedTownSlingBead(r, movedID); err != nil {
		return err
	}
	r.dest = dest
	r.result.SpawnInfo = dest.SpawnInfo
	if dest.SpawnInfo != nil {
		r.result.PolecatName = dest.SpawnInfo.PolecatName
	}
	r.work.targetPane = dest.Pane
	r.work.vars = append([]string(nil), r.intent.Vars...)
	r.formulaName = r.intent.Formula
	r.dog.description = r.info.Description
	r.dog.dispatchComplete = dest.DelayedDog == nil
	return nil
}

func applyMovedTownSlingBead(r *townSlingRun, movedID string) error {
	if movedID == "" || movedID == r.intent.BeadID {
		return nil
	}
	r.intent.BeadID = movedID
	r.result.BeadID = movedID
	info, err := getBeadInfoFromTownRoot(r.townRoot, movedID)
	if err != nil {
		r.result.ErrMsg = err.Error()
		return fmt.Errorf("could not get moved bead info: %w", err)
	}
	r.info = info
	return nil
}

func rollbackTownSlingDog(r *townSlingRun) {
	if r.dog.dispatchComplete {
		return
	}
	rollbackStatus, rollbackAssignee := townSlingDogRollbackIdentity(r)
	restored := rollbackFailedDogDispatch(r.dest.DelayedDog, r.townRoot, r.intent.BeadID, r.dest.WorkDir, r.dog.description, rollbackStatus, rollbackAssignee, r.result.ConvoyID, r.info)
	if !restored && !dogFormulaSourceStillOriginal(r.townRoot, r.intent.BeadID, r.info) {
		return
	}
	if r.result.AttachedMolecule != "" {
		cleanupRolledBackDogMolecule(r.result.AttachedMolecule, r.intent.BeadID, r.townRoot)
	}
}

func townSlingDogRollbackIdentity(r *townSlingRun) (string, string) {
	if !shouldOpenTownSlingDogRollback(r) {
		return r.originalStatus, r.originalAssignee
	}
	return "open", ""
}

func shouldOpenTownSlingDogRollback(r *townSlingRun) bool {
	if !r.intent.Force || (r.originalStatus != "hooked" && r.originalStatus != "in_progress") {
		return false
	}
	parts := strings.Split(r.originalAssignee, "/")
	return r.dog.previousCleared || (len(parts) >= 3 && parts[1] == "polecats")
}

func prepareTownSlingWork(r *townSlingRun) error {
	appendTownSlingVars(r)
	printTownSlingStart(r)
	if err := r.ctx.Err(); err != nil {
		r.result.ErrMsg = err.Error()
		return err
	}
	if err := forceClearOldHook(r.ctx, r.intent, r.info, r.dest.Agent, r.townRoot); err != nil {
		r.result.ErrMsg = err.Error()
		return err
	}
	autoApplyTownSlingFormula(r)
	if err := burnStaleTownSlingMolecules(r); err != nil {
		return err
	}
	convoyID, createdConvoy := resolveAttemptConvoy(r.intent, r.info)
	r.convoyID = convoyID
	r.result.ConvoyID = convoyID
	if createdConvoy {
		r.work.rollbackConvoyID = convoyID
	}
	return nil
}

func appendTownSlingVars(r *townSlingRun) {
	if r.dest.SpawnInfo != nil && r.dest.SpawnInfo.BaseBranch != "" && r.dest.SpawnInfo.BaseBranch != "main" {
		r.work.vars = append(r.work.vars, fmt.Sprintf("base_branch=%s", r.dest.SpawnInfo.BaseBranch))
	}
	if r.intent.ResumeBranch != "" {
		r.work.vars = append(r.work.vars, fmt.Sprintf("resume_branch=%s", r.intent.ResumeBranch))
	}
}

func printTownSlingStart(r *townSlingRun) {
	if r.intent.RigName != "" {
		return
	}
	if r.formulaName != "" {
		fmt.Printf("%s Slinging formula %s on %s to %s...\n", style.Bold.Render("🎯"), r.formulaName, r.intent.BeadID, r.dest.Agent)
		return
	}
	fmt.Printf("%s Slinging %s to %s...\n", style.Bold.Render("🎯"), r.intent.BeadID, r.dest.Agent)
}

func autoApplyTownSlingFormula(r *townSlingRun) {
	if r.formulaName != "" || r.intent.HookRawBead || !strings.Contains(r.dest.Agent, "/polecats/") {
		return
	}
	targetRig := r.intent.RigName
	if targetRig == "" {
		if parts := strings.SplitN(r.dest.Agent, "/", 2); len(parts) >= 1 {
			targetRig = parts[0]
		}
	}
	r.formulaName = resolveFormula(slingState().formula, false, r.townRoot, targetRig)
	if slingState().formula != "" {
		fmt.Printf("  Applying %s for polecat work...\n", r.formulaName)
	} else if r.intent.RigName == "" {
		fmt.Printf("  Auto-applying %s for polecat work...\n", r.formulaName)
	}
	r.intent.Formula = r.formulaName
}

func burnStaleTownSlingMolecules(r *townSlingRun) error {
	if r.intent.Formula == "" {
		return nil
	}
	existingMolecules, err := collectExistingMoleculesForBead(r.info, r.intent.BeadID, r.townRoot)
	if err != nil {
		r.result.ErrMsg = fmt.Sprintf("molecule check failed: %v", err)
		return fmt.Errorf("checking existing molecule bonds: %w", err)
	}
	if len(existingMolecules) == 0 {
		return nil
	}
	if !townSlingMoleculesStale(r) {
		r.result.ErrMsg = "has existing molecule(s)"
		return fmt.Errorf("bead %s has existing molecule(s) (use --force)", r.intent.BeadID)
	}
	if r.intent.DryRun {
		fmt.Printf("  Would burn %d stale molecule(s): %s\n",
			len(existingMolecules), strings.Join(existingMolecules, ", "))
		return nil
	}
	fmt.Printf("  %s Burning %d stale molecule(s): %s\n",
		style.Warning.Render("⚠"), len(existingMolecules), strings.Join(existingMolecules, ", "))
	if err := burnExistingMolecules(existingMolecules, r.intent.BeadID, r.townRoot); err != nil {
		r.result.ErrMsg = fmt.Sprintf("burn failed: %v", err)
		return fmt.Errorf("burning stale molecules: %w", err)
	}
	cleaned, err := getBeadInfoFromTownRoot(r.townRoot, r.intent.BeadID)
	if err != nil {
		r.result.ErrMsg = err.Error()
		return fmt.Errorf("reading bead after burning stale molecules: %w", err)
	}
	r.info.Description = cleaned.Description
	return nil
}

func townSlingMoleculesStale(r *townSlingRun) bool {
	if r.intent.Force || isOrphanMolecule(r.info) {
		return true
	}
	if r.info.Assignee == "" {
		return r.info.Status == "open" || r.info.Status == "in_progress"
	}
	return isHookedAgentDeadFn(r.info.Assignee)
}

func (r *townSlingRun) compensate(rollbackBeadID, reason string) {
	compensateSlingAttempt(slingCompensation{
		reason:        reason,
		spawnInfo:     r.dest.SpawnInfo,
		beadID:        rollbackBeadID,
		hookWorkDir:   r.dest.WorkDir,
		createdConvoy: r.work.rollbackConvoyID,
		townRoot:      r.townRoot,
		originalInfo:  r.info,
		force:         r.intent.Force,
	})
}

func cookAndAttachTownSling(r *townSlingRun) (beadFieldUpdates, error) {
	r.work.formulaCooked = r.intent.SkipCook
	if err := cookTownSlingFormula(r); err != nil {
		return beadFieldUpdates{}, err
	}
	r.beadToHook = r.intent.BeadID
	r.work.formulaVars = strings.Join(r.work.vars, "\n")
	if err := instantiateTownSlingFormula(r); err != nil {
		return beadFieldUpdates{}, err
	}
	r.result.AttachedMolecule = r.work.attachedMoleculeID
	r.work.actor = detectActor()
	return newSlingDispatchFieldUpdates(r.work.actor, r.intent, r.work.vars, r.work.formulaVars, r.convoyID, r.work.attachedMoleculeID), nil
}

func cookTownSlingFormula(r *townSlingRun) error {
	if r.intent.Formula == "" || r.work.formulaCooked {
		return nil
	}
	workDir := beads.ResolveHookDir(r.townRoot, r.intent.BeadID, r.dest.WorkDir)
	if err := CookFormula(r.intent.Formula, workDir, r.townRoot); err != nil {
		if r.intent.FormulaFailFatal {
			r.compensate(r.intent.BeadID, "Formula cook failed")
			r.result.ErrMsg = fmt.Sprintf("cook failed: %v", err)
			return fmt.Errorf("cooking formula %s: %w", r.intent.Formula, err)
		}
		fmt.Printf("  %s Could not cook formula %s: %v\n", style.Dim.Render("Warning:"), r.intent.Formula, err)
		return nil
	}
	r.work.formulaCooked = true
	return nil
}

func instantiateTownSlingFormula(r *townSlingRun) error {
	if r.intent.Formula == "" || !r.work.formulaCooked {
		return nil
	}
	injectTownSlingFormulaVars(r)
	skipCook := r.intent.RigName != ""
	formulaResult, err := InstantiateFormulaOnBead(r.ctx, r.intent.Formula, r.intent.BeadID, r.info.Title, r.dest.WorkDir, r.townRoot, skipCook, r.work.vars)
	if err != nil {
		if r.intent.FormulaFailFatal {
			r.compensate(r.intent.BeadID, "Formula instantiation failed")
			r.result.ErrMsg = fmt.Sprintf("formula failed: %v", err)
			return fmt.Errorf("instantiating formula %s: %w", r.intent.Formula, err)
		}
		fmt.Printf("  %s Could not apply formula: %v (hooking raw bead)\n", style.Dim.Render("Warning:"), err)
		return nil
	}
	fmt.Printf("  %s Formula %s applied\n", style.Bold.Render("✓"), r.intent.Formula)
	r.beadToHook = formulaResult.BeadToHook
	r.work.attachedMoleculeID = formulaResult.WispRootID
	if len(formulaResult.FormulaVars) > 0 {
		r.work.vars = formulaResult.FormulaVars
		r.work.formulaVars = strings.Join(r.work.vars, "\n")
	}
	return nil
}

func injectTownSlingFormulaVars(r *townSlingRun) {
	rigName := r.intent.RigName
	if rigName == "" {
		if parts := strings.SplitN(r.dest.Agent, "/", 2); len(parts) >= 1 {
			rigName = parts[0]
		}
	}
	if rigName != "" {
		r.work.vars = append(loadRigCommandVars(r.townRoot, rigName), r.work.vars...)
	}
	if r.intent.RigName != "" {
		if priorVars := lookupPriorAttempt(r.beadsDir, r.intent.BeadID); len(priorVars) > 0 {
			r.work.vars = append(r.work.vars, priorVars...)
			fmt.Printf("  %s Prior attempt found — context injected for polecat\n", style.Dim.Render("↻"))
		}
	}
	r.work.formulaVars = strings.Join(r.work.vars, "\n")
}

func hookTownSling(r *townSlingRun, fieldUpdates beadFieldUpdates) error {
	assigneeUnlock, assigneeLockErr := tryAcquireSlingAssigneeLockFn(r.townRoot, r.dest.Agent)
	if assigneeLockErr != nil {
		r.compensate(r.intent.BeadID, "Assignee lock failed")
		r.result.ErrMsg = "assignee lock failed"
		return fmt.Errorf("serializing hook write for %s: %w", r.dest.Agent, assigneeLockErr)
	}
	defer assigneeUnlock()
	if err := storeTownSlingFieldsBeforeHook(r, fieldUpdates); err != nil {
		return err
	}
	if err := writeTownSlingHook(r); err != nil {
		return err
	}
	return afterTownSlingHook(r, fieldUpdates)
}

func writeTownSlingHook(r *townSlingRun) error {
	if err := r.ctx.Err(); err != nil {
		r.compensate(r.beadToHook, "Sling canceled")
		r.result.ErrMsg = err.Error()
		return err
	}
	hookDir := beads.ResolveHookDir(r.townRoot, r.beadToHook, r.dest.WorkDir)
	if err := hookBeadWithRetryWithTownRootFn(r.beadToHook, r.dest.Agent, hookDir, r.townRoot); err != nil {
		r.compensate(r.beadToHook, "Hook failed")
		r.result.ErrMsg = "hook failed"
		return fmt.Errorf("failed to hook bead: %w", err)
	}
	return nil
}

func afterTownSlingHook(r *townSlingRun, fieldUpdates beadFieldUpdates) error {
	nudgeTownSlingMayor(r)
	printTownSlingHooked(r)
	if err := events.LogFeed(events.TypeSling, r.work.actor, events.SlingPayload(r.beadToHook, r.dest.Agent)); err != nil {
		fmt.Printf("%s Could not record sling event: %v\n", style.Dim.Render("Warning:"), err)
	}
	if err := updateTownSlingAgentHook(r); err != nil {
		return err
	}
	if err := storeTownSlingFieldsAfterHook(r, fieldUpdates); err != nil {
		return err
	}
	if r.intent.Mode != "" {
		updateAgentMode(r.dest.Agent, r.intent.Mode, r.dest.WorkDir, r.beadsDir)
	}
	return clearPreviousTownSlingDog(r)
}

func storeTownSlingFieldsBeforeHook(r *townSlingRun, fieldUpdates beadFieldUpdates) error {
	if r.work.attachedMoleculeID != "" || !slingFieldsRequireDurableWrite(fieldUpdates) {
		return nil
	}
	storedDescription, err := storeFieldsInBeadFromTownRootWithDescription(r.townRoot, r.beadToHook, fieldUpdates)
	if err != nil {
		r.compensate(r.beadToHook, "Raw sling metadata failed")
		r.result.ErrMsg = "raw sling metadata failed"
		return fmt.Errorf("storing raw sling metadata before hook: %w", err)
	}
	if r.dest.DelayedDog != nil {
		r.dog.description = storedDescription
	}
	return nil
}

func nudgeTownSlingMayor(r *townSlingRun) {
	if r.dest.Agent != "mayor/" {
		return
	}
	if err := nudgeMayorHook(r.townRoot, r.beadToHook); err != nil {
		fmt.Printf("%s Could not nudge Mayor after Hook: %v\n", style.Dim.Render("Warning:"), err)
	}
}

func printTownSlingHooked(r *townSlingRun) {
	if r.dest.SpawnInfo != nil {
		fmt.Printf("  %s Work attached to %s\n", style.Bold.Render("✓"), r.dest.SpawnInfo.PolecatName)
		return
	}
	fmt.Printf("%s Work attached to hook (status=hooked)\n", style.Bold.Render("✓"))
}

func updateTownSlingAgentHook(r *townSlingRun) error {
	if r.dest.HookSetAtomically {
		return nil
	}
	if err := updateAgentHookBead(r.dest.Agent, r.beadToHook, r.dest.WorkDir, r.beadsDir); err != nil {
		if r.dest.DelayedDog != nil {
			r.result.ErrMsg = err.Error()
			return fmt.Errorf("updating dog agent hook: %w", err)
		}
		fmt.Printf("%s Could not update agent hook: %v\n", style.Dim.Render("Warning:"), err)
	}
	return nil
}

func storeTownSlingFieldsAfterHook(r *townSlingRun, fieldUpdates beadFieldUpdates) error {
	storedDescription, storeErr := storeFieldsInBeadFromTownRootWithDescription(r.townRoot, r.beadToHook, fieldUpdates)
	if storeErr != nil {
		return reportTownSlingStoreErr(r, fieldUpdates, storeErr)
	}
	if r.dest.DelayedDog != nil {
		r.dog.description = storedDescription
	}
	printTownSlingStoredFlags(r)
	return nil
}

func reportTownSlingStoreErr(r *townSlingRun, fieldUpdates beadFieldUpdates, storeErr error) error {
	if r.dest.DelayedDog != nil && r.work.attachedMoleculeID != "" {
		r.result.ErrMsg = storeErr.Error()
		return fmt.Errorf("storing dog formula attachment metadata: %w", storeErr)
	}
	if slingFieldsRequireDurableWrite(fieldUpdates) {
		r.compensate(r.beadToHook, "Durable sling metadata failed")
		r.result.ErrMsg = "sling metadata failed"
		return fmt.Errorf("storing sling metadata: %w", storeErr)
	}
	fmt.Printf("  %s Could not store fields in bead: %v\n", style.Dim.Render("Warning:"), storeErr)
	return nil
}

func printTownSlingStoredFlags(r *townSlingRun) {
	if r.intent.Args != "" && r.intent.RigName == "" {
		fmt.Printf("%s Args stored in bead (durable)\n", style.Bold.Render("✓"))
	}
	if r.intent.NoMerge && r.intent.RigName == "" {
		fmt.Printf("%s No-merge mode enabled (work stays on feature branch)\n", style.Bold.Render("✓"))
	}
	if r.intent.ReviewOnly && r.intent.RigName == "" {
		fmt.Printf("%s Review-only mode: assignee must evaluate and report back, NOT merge/commit/push\n", style.Bold.Render("⚠"))
	}
}

func clearPreviousTownSlingDog(r *townSlingRun) error {
	if r.dest.DelayedDog == nil || !r.intent.Force || !strings.HasPrefix(r.originalAssignee, "deacon/dogs/") || r.originalAssignee == r.dest.Agent {
		return nil
	}
	if err := clearPreviousDogAssignment(r.townRoot, r.originalAssignee, r.intent.BeadID, r.info.Description); err != nil {
		r.result.ErrMsg = err.Error()
		return fmt.Errorf("clearing previous dog assignment: %w", err)
	}
	r.dog.previousCleared = true
	return nil
}

func finishTownSling(r *townSlingRun) (*SlingResult, error) {
	if err := startTownSlingSession(r); err != nil {
		return r.result, err
	}
	if err := completeTownSlingDispatch(r); err != nil {
		return r.result, err
	}
	if !r.intent.NoBoot && r.intent.RigName != "" {
		wakeRigAgents(r.intent.RigName)
	}
	r.result.Success = true
	return r.result, nil
}

func startTownSlingSession(r *townSlingRun) error {
	if r.dest.SpawnInfo == nil {
		return nil
	}
	r.work.freshlySpawned = true
	pane, err := r.dest.SpawnInfo.StartSession()
	if err != nil {
		fmt.Printf("  %s Could not start session: %v, cleaning up partial state...\n", style.Dim.Render("✗"), err)
		r.compensate(r.beadToHook, "Session failed")
		r.result.ErrMsg = fmt.Sprintf("session failed: %v", err)
		return fmt.Errorf("starting polecat session: %w", err)
	}
	r.work.targetPane = pane
	r.result.PolecatName = r.dest.SpawnInfo.PolecatName
	if r.intent.RigName != "" {
		fmt.Printf("  %s Session started for %s\n", style.Bold.Render("▶"), r.dest.SpawnInfo.PolecatName)
	}
	return nil
}

func completeTownSlingDispatch(r *townSlingRun) error {
	if r.dest.DelayedDog != nil {
		pane, err := completeBareDogDispatch(r.dest.DelayedDog, r.beadToHook, r.convoyID, r.work.attachedMoleculeID, r.intent.Subject, r.intent.Args)
		if err != nil {
			r.result.ErrMsg = err.Error()
			return err
		}
		r.work.targetPane = pane
		r.dog.dispatchComplete = true
		return nil
	}
	if r.work.freshlySpawned || r.dest.IsSelfSling || r.work.targetPane == "" || r.intent.RigName != "" {
		printTownSlingNoNudge(r)
		return nil
	}
	return nudgeTownSlingPane(r)
}

func printTownSlingNoNudge(r *townSlingRun) {
	if r.work.freshlySpawned {
		return
	}
	if r.dest.IsSelfSling {
		fmt.Printf("%s Self-sling: work hooked, will process on next turn\n", style.Dim.Render("○"))
		return
	}
	if r.work.targetPane == "" && r.intent.RigName == "" {
		fmt.Printf("%s No pane to nudge (agent will discover work via gt prime)\n", style.Dim.Render("○"))
	}
}

func nudgeTownSlingPane(r *townSlingRun) error {
	sessionName := getSessionFromPane(r.work.targetPane)
	if sessionName != "" {
		if err := ensureAgentReady(sessionName); err != nil {
			fmt.Printf("%s Could not verify agent ready: %v\n", style.Dim.Render("○"), err)
		}
	}
	if err := injectStartPrompt(r.work.targetPane, r.beadToHook, r.intent.Subject, r.intent.Args); err != nil {
		fmt.Printf("%s Could not nudge (no tmux?): %v\n", style.Dim.Render("○"), err)
		fmt.Printf("  Agent will discover work via gt prime / bd show\n")
		return nil
	}
	fmt.Printf("%s Start prompt sent\n", style.Bold.Render("▶"))
	return nil
}
