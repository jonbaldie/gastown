package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/telemetry"
	"github.com/jonbaldie/gastown/internal/workspace"
)

type formulaSlingState struct {
	formulaSlingRequest
	formulaSlingTarget
	progress formulaSlingProgress
}

type formulaSlingRequest struct {
	ctx            context.Context
	formulaName    string
	townRoot       string
	townBeadsDir   string
	formulaWorkDir string
	mode           string
}

type formulaSlingTarget struct {
	targetAgent    string
	targetPane     string
	resolved       *ResolvedTarget
	delayedDogInfo *DogDispatchInfo
	isSelfSling    bool
	admission      *polecatAdmissionHandle
	poolUnlock     func()
}

type formulaSlingProgress struct {
	wispRootID          string
	dispatchBeadID      string
	delayedDogComplete  bool
	formulaWorkComplete bool
}

func prepareFormulaSlingState(ctx context.Context, args []string) (*formulaSlingState, error) {
	formulaName := args[0]
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return nil, fmt.Errorf("finding town root: %w", err)
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")
	target := slingFormulaTarget(args)
	admission, err := acquireFormulaSlingAdmission(townRoot, target, formulaName)
	if err != nil {
		return nil, err
	}
	poolUnlock, err := acquireFormulaDogPoolLock(townRoot, target, formulaName)
	if err != nil {
		if admission != nil {
			admission.Release()
		}
		return nil, err
	}
	resolved, err := resolveFormulaSlingTarget(townRoot, target, formulaName, admission != nil)
	if err != nil {
		releaseFormulaSlingResources(admission, poolUnlock)
		return nil, err
	}
	return newFormulaSlingState(ctx, formulaName, townRoot, townBeadsDir, resolved, admission, poolUnlock), nil
}

func acquireFormulaSlingAdmission(townRoot, target, formulaName string) (*polecatAdmissionHandle, error) {
	if slingDryRun || target == "" {
		return nil, nil
	}
	rigName, isRig := IsRigName(target)
	if !isRig {
		return nil, nil
	}
	admission, _, err := acquirePolecatAdmissionFn(townRoot, rigName, formulaName, "formula")
	return admission, err
}

func acquireFormulaDogPoolLock(townRoot, target, formulaName string) (func(), error) {
	if slingDryRun || !isFormulaDogPoolTarget(target) {
		return nil, nil
	}
	poolUnlock, err := tryAcquireSlingAssigneeLock(townRoot, "deacon/dogs")
	if err != nil {
		return nil, fmt.Errorf("serializing dog-pool formula sling for %s: %w", formulaName, err)
	}
	return poolUnlock, nil
}

func resolveFormulaSlingTarget(townRoot, target, formulaName string, skipAdmission bool) (*ResolvedTarget, error) {
	return resolveTarget(target, ResolveTargetOptions{
		DryRun:               slingDryRun,
		Force:                slingForce,
		Create:               slingCreate,
		Account:              slingAccount,
		Agent:                slingAgent,
		NoBoot:               slingNoBoot,
		WorkDesc:             formulaName,
		TownRoot:             townRoot,
		SkipPolecatAdmission: skipAdmission,
	})
}

func releaseFormulaSlingResources(admission *polecatAdmissionHandle, poolUnlock func()) {
	if poolUnlock != nil {
		poolUnlock()
	}
	if admission != nil {
		admission.Release()
	}
}

func newFormulaSlingState(ctx context.Context, formulaName, townRoot, townBeadsDir string, resolved *ResolvedTarget, admission *polecatAdmissionHandle, poolUnlock func()) *formulaSlingState {
	state := &formulaSlingState{
		formulaSlingRequest: formulaSlingRequest{
			ctx:            ctx,
			formulaName:    formulaName,
			townRoot:       townRoot,
			townBeadsDir:   townBeadsDir,
			formulaWorkDir: resolved.WorkDir,
		},
		formulaSlingTarget: formulaSlingTarget{
			targetAgent:    resolved.Agent,
			targetPane:     resolved.Pane,
			resolved:       resolved,
			delayedDogInfo: resolved.DelayedDogInfo,
			isSelfSling:    resolved.IsSelfSling,
			admission:      admission,
		},
		progress: formulaSlingProgress{},
	}
	if state.formulaWorkDir == "" {
		state.formulaWorkDir = townRoot
	}
	state.poolUnlock = poolUnlock
	fmt.Printf("%s Slinging formula %s to %s...\n", style.Bold.Render("🎯"), formulaName, state.targetAgent)
	return state
}

func slingFormulaTarget(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return ""
}

func isFormulaDogPoolTarget(target string) bool {
	dogName, isDog := IsDogTarget(target)
	return isDog && dogName == ""
}

func dryRunFormulaSling(s *formulaSlingState) error {
	existing, err := findHookedFormulaSingletonFn(s.formulaWorkDir, s.targetAgent, s.formulaName)
	if err != nil {
		return fmt.Errorf("checking existing hooked formulas for %s: %w", s.targetAgent, err)
	}
	if existing != nil && !slingForce && isDurableFormulaDispatch(existing) {
		fmt.Printf("Would reuse existing formula %s on %s via %s\n", s.formulaName, s.targetAgent, existing.ID)
		return nil
	}
	fmt.Printf("Would cook formula: %s\n", s.formulaName)
	fmt.Printf("Would create wisp and pin to: %s\n", s.targetAgent)
	for _, v := range slingVars {
		fmt.Printf("  --var %s\n", v)
	}
	fmt.Printf("Would nudge pane: %s\n", s.targetPane)
	return nil
}

func acquireFormulaAssigneeLock(s *formulaSlingState) (func(), error) {
	assigneeUnlock, err := tryAcquireSlingAssigneeLock(s.townRoot, s.targetAgent)
	if err == nil {
		return assigneeUnlock, nil
	}
	lockErr := fmt.Errorf("serializing formula sling for %s: %w", s.targetAgent, err)
	if s.delayedDogInfo == nil {
		return nil, lockErr
	}
	if clearErr := s.delayedDogInfo.ClearWorkIfMatches(); clearErr != nil {
		return nil, errors.Join(lockErr, fmt.Errorf("clearing failed dog assignment: %w", clearErr))
	}
	return nil, lockErr
}

func formulaSlingMode() string {
	if slingRalph {
		return "ralph"
	}
	return ""
}

func prepareExistingFormulaSling(s *formulaSlingState, existing *beads.Issue) (bool, *polecatAdmissionHandle, error) {
	if shouldReuseExistingFormula(existing, s.delayedDogInfo, slingForce) {
		return true, nil, reuseExistingFormulaSling(s, existing)
	}
	if s.delayedDogInfo != nil && !s.delayedDogInfo.ownsWork && !s.delayedDogInfo.WorksOnHook(existing) {
		return true, nil, fmt.Errorf("dog formula reuse became stale before hook verification; retry dispatch")
	}
	cleaned, err := cleanupOrMigrateExistingFormula(s, existing)
	if err != nil {
		return true, nil, err
	}
	if !cleaned {
		if handled, err := migrateExistingFormulaSling(s, existing); handled {
			return true, nil, err
		}
	}
	admission, err := acquireAdditionalFormulaAdmission(s)
	if err != nil {
		return true, nil, err
	}
	return false, admission, nil
}

func migrateExistingFormulaSling(s *formulaSlingState, existing *beads.Issue) (bool, error) {
	if existing == nil || slingForce || !isLegacyFormulaWisp(existing) {
		return false, nil
	}
	dispatchBeadID, wispRootID, err := migrateLegacyFormulaDispatch(existing, s.formulaName, s.formulaWorkDir, s.townRoot, s.targetAgent, s.mode)
	if err != nil {
		rollbackFormulaSling(s, dispatchBeadID)
		return true, err
	}
	s.progress.dispatchBeadID = dispatchBeadID
	s.progress.wispRootID = wispRootID
	err = finishFormulaSling(s.resolved, s.delayedDogInfo, formulaSlingFinishState{
		delayedDogComplete:  &s.progress.delayedDogComplete,
		formulaWorkComplete: &s.progress.formulaWorkComplete,
		targetPane:          &s.targetPane,
	}, s.townBeadsDir, s.formulaName, s.progress.dispatchBeadID, s.targetAgent, s.isSelfSling, s.mode)
	return true, err
}

func acquireAdditionalFormulaAdmission(s *formulaSlingState) (*polecatAdmissionHandle, error) {
	if s.admission != nil || !strings.Contains(s.targetAgent, "/polecats/") {
		return nil, nil
	}
	parts := strings.Split(s.targetAgent, "/")
	if len(parts) < 3 {
		return nil, nil
	}
	admission, _, err := acquirePolecatAdmissionFn(s.townRoot, parts[0], s.formulaName, "formula")
	return admission, err
}

func cleanupOrMigrateExistingFormula(s *formulaSlingState, existing *beads.Issue) (bool, error) {
	if existing == nil || slingForce {
		return false, nil
	}
	if s.delayedDogInfo != nil && s.delayedDogInfo.ownsWork {
		if err := cleanupStaleDogFormulaWispFn(existing.ID, s.formulaWorkDir); err != nil {
			return true, fmt.Errorf("cleaning stale dog formula wisp %s: %w", existing.ID, err)
		}
		return true, nil
	}
	return false, nil
}

func reuseExistingFormulaSling(s *formulaSlingState, existing *beads.Issue) error {
	if err := updateExistingFormulaMode(s, existing); err != nil {
		return err
	}
	fmt.Printf("%s Formula %s already hooked to %s via %s, no-op\n",
		style.Dim.Render("○"), s.formulaName, s.targetAgent, existing.ID)
	if s.delayedDogInfo == nil {
		return nil
	}
	if _, err := s.delayedDogInfo.CompleteFormulaStartup(existing.ID); err != nil {
		return fmt.Errorf("completing existing dog formula dispatch: %w", err)
	}
	if os.Getenv("GT_TEST_NO_NUDGE") == "" {
		if err := nudgeFormulaDog(s.delayedDogInfo, formulaSlingPrompt(s.formulaName)); err != nil {
			return err
		}
	}
	s.progress.delayedDogComplete = true
	return nil
}

func updateExistingFormulaMode(s *formulaSlingState, existing *beads.Issue) error {
	existingMode := ""
	if fields := beads.ParseAttachmentFields(existing); fields != nil {
		existingMode = fields.Mode
	}
	if existingMode == s.mode {
		return nil
	}
	if err := storeFieldsInBeadFromTownRoot(s.townRoot, existing.ID, beadFieldUpdates{Mode: &s.mode}); err != nil {
		return fmt.Errorf("updating existing formula mode: %w", err)
	}
	if s.mode != "" || existingMode != "" {
		updateAgentMode(s.targetAgent, s.mode, "", s.townBeadsDir)
	}
	return nil
}

func rollbackFormulaSling(s *formulaSlingState, beadID string) {
	if s.resolved.NewPolecatInfo == nil {
		return
	}
	fmt.Printf("%s Rolling back spawned polecat %s...\n", style.Warning.Render("⚠"), s.resolved.NewPolecatInfo.PolecatName)
	rollbackSlingArtifactsFn(s.resolved.NewPolecatInfo, beadID, s.formulaWorkDir, "")
}

func executeFormulaSling(s *formulaSlingState) error {
	if err := cookFormulaSling(s); err != nil {
		return err
	}
	if err := createFormulaSlingWisp(s); err != nil {
		return err
	}
	if err := persistFormulaSlingDispatch(s); err != nil {
		return err
	}
	return finishFormulaSling(s.resolved, s.delayedDogInfo, formulaSlingFinishState{
		delayedDogComplete:  &s.progress.delayedDogComplete,
		formulaWorkComplete: &s.progress.formulaWorkComplete,
		targetPane:          &s.targetPane,
	}, s.townBeadsDir, s.formulaName, s.progress.dispatchBeadID, s.targetAgent, s.isSelfSling, s.mode)
}

func cookFormulaSling(s *formulaSlingState) error {
	fmt.Printf("  Cooking formula...\n")
	if err := BdCmd("cook", s.formulaName).
		Dir(s.formulaWorkDir).
		WithGTRoot(s.townRoot).
		Run(); err != nil {
		telemetry.RecordMolCook(s.ctx, s.formulaName, err)
		rollbackFormulaSling(s, "")
		return fmt.Errorf("cooking formula: %w", err)
	}
	telemetry.RecordMolCook(s.ctx, s.formulaName, nil)
	return nil
}

func createFormulaSlingWisp(s *formulaSlingState) error {
	fmt.Printf("  Creating wisp...\n")
	wispArgs := []string{"mol", "wisp", s.formulaName}
	for _, v := range slingVars {
		wispArgs = append(wispArgs, "--var", v)
	}
	wispArgs = append(wispArgs, "--json")
	wispOut, err := BdCmd(wispArgs...).
		Dir(s.formulaWorkDir).
		WithAutoCommit().
		WithGTRoot(s.townRoot).
		Output()
	if err != nil {
		rollbackFormulaSling(s, "")
		return fmt.Errorf("creating wisp: %w", err)
	}
	s.progress.wispRootID, err = parseWispIDFromJSON(wispOut)
	if err != nil {
		telemetry.RecordMolWisp(s.ctx, s.formulaName, "", "", err)
		rollbackFormulaSling(s, "")
		return fmt.Errorf("parsing wisp output: %w", err)
	}
	telemetry.RecordMolWisp(s.ctx, s.formulaName, s.progress.wispRootID, "", nil)
	fmt.Printf("%s Wisp created: %s\n", style.Bold.Render("✓"), s.progress.wispRootID)
	return nil
}

func persistFormulaSlingDispatch(s *formulaSlingState) error {
	dispatchBead, err := createFormulaDispatchBead(s.formulaName, s.formulaWorkDir)
	if err != nil {
		return err
	}
	s.progress.dispatchBeadID = dispatchBead.ID
	fmt.Printf("%s Durable dispatch bead created: %s\n", style.Bold.Render("✓"), s.progress.dispatchBeadID)
	if s.delayedDogInfo != nil {
		if err := s.delayedDogInfo.persistWorkSource(s.progress.dispatchBeadID); err != nil {
			return fmt.Errorf("recording dog formula source: %w", err)
		}
	}
	if err := persistAndHookFormulaDispatch(s.townRoot, s.formulaWorkDir, s.progress.dispatchBeadID, s.targetAgent, s.formulaName, s.progress.wispRootID, s.mode); err != nil {
		return err
	}
	return nil
}

func cleanupFormulaSlingFailure(s *formulaSlingState, err error) error {
	cleanupID := s.progress.dispatchBeadID
	if cleanupID == "" {
		cleanupID = s.progress.wispRootID
	}
	reason := "burned: formula sling failed"
	if s.delayedDogInfo != nil && !s.progress.delayedDogComplete {
		reason = "burned: dog formula sling failed"
		if err != nil || cleanupID != "" {
			err = cleanupDelayedDogFormulaFailure(err, s.delayedDogInfo, cleanupID, s.formulaWorkDir)
		}
		return errors.Join(err, rollbackIncompleteFormulaSling(s.progress.dispatchBeadID, s.progress.wispRootID, s.formulaWorkDir, reason))
	}
	if err != nil && !s.progress.formulaWorkComplete && cleanupID != "" {
		err = errors.Join(err, rollbackIncompleteFormulaSling(s.progress.dispatchBeadID, s.progress.wispRootID, s.formulaWorkDir, reason))
	}
	return err
}
