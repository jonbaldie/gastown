package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/worker"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type slingRunInput struct {
	cmd          *cobra.Command
	args         []string
	townRoot     string
	townBeadsDir string
	deferred     bool
}

func runSlingCommand(cmd *cobra.Command, ctx context.Context, args []string) error {
	if err := validateSlingInvocation(); err != nil {
		return err
	}
	return withSlingAutoCommit(func() error {
		input, err := prepareSlingInput(cmd, args)
		if err != nil {
			return err
		}
		if handled, err := dispatchSlingSpecial(ctx, input); handled {
			return err
		}
		return dispatchSlingSingle(ctx, input)
	})
}

func validateSlingInvocation() error {
	if err := validateSlingRole(); err != nil {
		return err
	}
	if err := validateSlingMerge(); err != nil {
		return err
	}
	return validateSlingResume()
}

func validateSlingRole() error {
	if role := os.Getenv("GT_ROLE"); role != "" {
		parsedRole, _, _ := parseRoleString(role)
		if parsedRole == RolePolecat {
			return fmt.Errorf("polecats cannot sling (use gt done for handoff)")
		}
	} else if polecatName := os.Getenv("GT_POLECAT"); polecatName != "" {
		return fmt.Errorf("polecats cannot sling (use gt done for handoff)")
	}
	return nil
}

func validateSlingMerge() error {
	if slingMerge != "" && slingMerge != "direct" && slingMerge != "mr" && slingMerge != "local" {
		return fmt.Errorf("invalid --merge value %q: must be direct, mr, or local", slingMerge)
	}
	return nil
}

func validateSlingResume() error {
	if slingResumeBranch != "" && slingResumePR != 0 {
		return fmt.Errorf("--branch and --pr are mutually exclusive")
	}
	if (slingResumeBranch != "" || slingResumePR != 0) && slingBaseBranch != "" {
		return fmt.Errorf("--base-branch cannot be combined with --branch or --pr (resume implies starting on the existing branch)")
	}
	if slingResumePR != 0 {
		resolved, err := resolvePRBranch(slingResumePR)
		if err != nil {
			return fmt.Errorf("resolving --pr %d: %w", slingResumePR, err)
		}
		slingResumeBranch = resolved
		fmt.Printf("%s --pr %d resolved to branch %s\n", style.Dim.Render("→"), slingResumePR, resolved)
	}
	return nil
}

func withSlingAutoCommit(run func() error) error {
	prevAutoCommit := os.Getenv("BD_DOLT_AUTO_COMMIT")
	os.Setenv("BD_DOLT_AUTO_COMMIT", "off")
	defer func() {
		if prevAutoCommit == "" {
			os.Unsetenv("BD_DOLT_AUTO_COMMIT")
		} else {
			os.Setenv("BD_DOLT_AUTO_COMMIT", prevAutoCommit)
		}
	}()
	return run()
}

func prepareSlingInput(cmd *cobra.Command, args []string) (slingRunInput, error) {
	if err := readSlingStdin(); err != nil {
		return slingRunInput{}, err
	}

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return slingRunInput{}, fmt.Errorf("finding town root: %w", err)
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if err := prepareSlingArgs(args); err != nil {
		return slingRunInput{}, err
	}
	deferred, err := shouldDeferDispatch()
	if err != nil {
		return slingRunInput{}, err
	}
	return slingRunInput{cmd: cmd, args: args, townRoot: townRoot, townBeadsDir: townBeadsDir, deferred: deferred}, nil
}

func readSlingStdin() error {
	if !slingStdin {
		return nil
	}
	if slingMessage != "" && slingArgs != "" {
		return fmt.Errorf("cannot use --stdin when both --message and --args are already provided")
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	stdinContent := strings.TrimRight(string(data), "\n")
	if slingArgs == "" {
		slingArgs = stdinContent
	} else {
		slingMessage = stdinContent
	}
	return nil
}

func prepareSlingArgs(args []string) error {
	for i := range args {
		args[i] = normalizeSlingTarget(args[i])
	}
	if slingCrew != "" {
		if len(args) < 2 {
			return fmt.Errorf("--crew requires a rig target argument (e.g., gt sling <bead> <rig> --crew %s)", slingCrew)
		}
		target := args[len(args)-1]
		args[len(args)-1] = target + "/crew/" + slingCrew
	}
	if len(args) > 1 {
		if err := ValidateTarget(args[len(args)-1]); err != nil {
			return err
		}
	}
	if len(args) == 2 {
		redirected, err := applyWorkflowStepTargetOverride(args)
		if err != nil {
			return err
		}
		copy(args, redirected)
	}
	return nil
}

func dispatchSlingSpecial(ctx context.Context, input slingRunInput) (bool, error) {
	if handled, err := dispatchSlingBatch(input); handled {
		return true, err
	}
	if handled, err := dispatchSlingDeferredOn(input); handled {
		return true, err
	}
	if handled, err := dispatchSlingDeferredRig(ctx, input); handled {
		return true, err
	}
	if handled, err := dispatchSlingSingleID(input); handled {
		return true, err
	}
	if handled, err := dispatchSlingTwoBeadBatch(input); handled {
		return true, err
	}
	return false, nil
}

func dispatchSlingBatch(input slingRunInput) (bool, error) {
	if len(input.args) <= 2 {
		return false, nil
	}
	lastArg := input.args[len(input.args)-1]
	if rigName, isRig := IsRigName(lastArg); isRig {
		beadIDs := input.args[:len(input.args)-1]
		if input.deferred {
			if err := rejectBatchScheduleIDs(beadIDs); err != nil {
				return true, err
			}
			return true, runBatchSchedule(beadIDs, rigName, input.townRoot)
		}
		fmt.Printf("  %s the rig can be auto-resolved from bead prefixes. You can omit <%s>.\n",
			style.Dim.Render("Tip:"), rigName)
		return true, runBatchSling(beadIDs, rigName, input.townBeadsDir)
	}
	if !allBeadIDs(input.args) {
		return false, nil
	}
	rigName, err := resolveRigFromBeadIDs(input.args, filepath.Dir(input.townBeadsDir))
	if err != nil {
		return true, err
	}
	return true, runBatchSling(input.args, rigName, input.townBeadsDir)
}

func rejectBatchScheduleIDs(beadIDs []string) error {
	for _, id := range beadIDs {
		idType, err := detectSchedulerIDType(id)
		if err == nil && idType != "task" {
			return fmt.Errorf("%s '%s' cannot be batch-scheduled with an explicit rig\nUse: gt sling %s (children auto-resolve rigs)", idType, id, id)
		}
	}
	return nil
}

func dispatchSlingDeferredOn(input slingRunInput) (bool, error) {
	if !input.deferred || slingOnTarget == "" {
		return false, nil
	}
	if len(input.args) >= 2 {
		rigName, isRig := IsRigName(input.args[len(input.args)-1])
		if !isRig {
			return true, fmt.Errorf("'%s' is not a known rig\nUse: gt sling %s --on %s <rig>", input.args[len(input.args)-1], input.args[0], slingOnTarget)
		}
		formulaName := input.args[0]
		if slingHookRawBead {
			formulaName = ""
		}
		return true, scheduleBead(slingOnTarget, rigName, slingScheduleOptions(formulaName))
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return true, err
	}
	rigName := resolveRigForBead(townRoot, slingOnTarget)
	if rigName == "" {
		return true, fmt.Errorf("cannot resolve rig for bead %s\nSpecify explicitly: gt sling %s --on %s <rig>", slingOnTarget, input.args[0], slingOnTarget)
	}
	formulaName := input.args[0]
	if slingHookRawBead {
		formulaName = ""
	}
	return true, scheduleBead(slingOnTarget, rigName, slingScheduleOptions(formulaName))
}

func dispatchSlingDeferredRig(ctx context.Context, input slingRunInput) (bool, error) {
	if !input.deferred || len(input.args) != 2 {
		return false, nil
	}
	rigName, isRig := IsRigName(input.args[1])
	if isRig {
		return true, scheduleDeferredRigSling(ctx, input, rigName)
	}
	if _, isDog := IsDogTarget(input.args[1]); !isDog {
		return true, fmt.Errorf("deferred dispatch requires a rig target: gt sling %s <rig>\n'%s' is not a known rig", input.args[0], input.args[1])
	}
	return false, nil
}

func scheduleDeferredRigSling(ctx context.Context, input slingRunInput, rigName string) error {
	idType, err := detectSchedulerIDType(input.args[0])
	if err == nil && idType != "task" {
		return fmt.Errorf("%s cannot be scheduled with an explicit rig\nUse: gt sling %s (children auto-resolve rigs)", idType, input.args[0])
	}
	if verifyBeadExists(input.args[0]) != nil && isSlingFormulaForRig(input, rigName) {
		return runSlingFormula(ctx, input.args)
	}
	formula := resolveFormula(slingFormula, slingHookRawBead, input.townRoot, rigName)
	return scheduleBead(input.args[0], rigName, slingScheduleOptions(formula))
}

func isSlingFormulaForRig(input slingRunInput, rigName string) bool {
	formulaWorkDir := input.townRoot
	if rigBeadsDir, ok := beads.ResolveRepoAliasBeadsDir(input.townRoot, rigName); ok {
		formulaWorkDir = filepath.Dir(rigBeadsDir)
	}
	return verifyFormulaExists(input.args[0], formulaWorkDir, input.townRoot) == nil
}

func slingScheduleOptions(formula string) ScheduleOptions {
	return ScheduleOptions{
		ScheduleWork: ScheduleWork{
			Formula:      formula,
			Args:         slingArgs,
			Vars:         slingVars,
			Merge:        slingMerge,
			BaseBranch:   slingBaseBranch,
			ResumeBranch: slingResumeBranch,
		},
		NoConvoy:    slingNoConvoy,
		Owned:       slingOwned,
		DryRun:      slingDryRun,
		Force:       slingForce,
		NoMerge:     slingNoMerge,
		ReviewOnly:  slingReviewOnly,
		Account:     slingAccount,
		Agent:       slingAgent,
		HookRawBead: slingHookRawBead,
		Ralph:       slingRalph,
	}
}

func dispatchSlingSingleID(input slingRunInput) (bool, error) {
	if len(input.args) != 1 {
		return false, nil
	}
	idType, err := detectSchedulerIDType(input.args[0])
	if err != nil {
		if input.deferred {
			return true, fmt.Errorf("deferred dispatch requires a rig target: gt sling %s <rig>", input.args[0])
		}
		return false, nil
	}
	if idType == "task" {
		if input.deferred {
			return true, fmt.Errorf("deferred dispatch requires a rig target: gt sling %s <rig>", input.args[0])
		}
		return false, nil
	}
	formula := resolveFormula(slingFormula, slingHookRawBead, input.townRoot, "")
	if err := validateNoTaskOnlySchedulerFlags(input.cmd, idType); err != nil {
		return true, err
	}
	return executeSlingSchedulerID(input, idType, formula)
}

func executeSlingSchedulerID(input slingRunInput, idType, formula string) (bool, error) {
	switch idType {
	case "convoy":
		if input.deferred {
			return true, runConvoyScheduleByID(input.args[0], convoyScheduleOpts{Formula: formula, HookRawBead: slingHookRawBead, Force: slingForce, DryRun: slingDryRun})
		}
		return true, runConvoySlingByID(input.args[0], convoyScheduleOpts{Formula: formula, HookRawBead: slingHookRawBead, Force: slingForce, DryRun: slingDryRun, NoBoot: slingNoBoot})
	case "epic":
		if input.deferred {
			return true, runEpicScheduleByID(input.args[0], epicScheduleOpts{Formula: formula, HookRawBead: slingHookRawBead, Force: slingForce, DryRun: slingDryRun})
		}
		return true, runEpicSlingByID(input.args[0], epicScheduleOpts{Formula: formula, HookRawBead: slingHookRawBead, Force: slingForce, DryRun: slingDryRun, NoBoot: slingNoBoot})
	}
	return false, nil
}

func dispatchSlingTwoBeadBatch(input slingRunInput) (bool, error) {
	if len(input.args) != 2 || !allBeadIDs(input.args) {
		return false, nil
	}
	if _, isRig := IsRigName(input.args[1]); isRig {
		return false, nil
	}
	rigName, err := resolveRigFromBeadIDs(input.args, filepath.Dir(input.townBeadsDir))
	if err != nil {
		return true, err
	}
	return true, runBatchSling(input.args, rigName, input.townBeadsDir)
}

func dispatchSlingSingle(ctx context.Context, input slingRunInput) error {
	beadID, formulaName, formulaOnly, err := resolveSlingBeadOrFormula(input)
	if err != nil {
		return err
	}
	if formulaOnly {
		return runSlingFormula(ctx, input.args)
	}
	if err := worker.RefuseLiveBead(input.townRoot, beadID); err != nil {
		return err
	}
	if !slingDryRun && len(input.args) == 2 {
		if rigName, isRig := IsRigName(input.args[1]); isRig {
			formula := formulaName
			if formula == "" {
				formula = resolveFormula(slingFormula, slingHookRawBead, input.townRoot, rigName)
			}
			return runRigBeadSling(ctx, beadID, rigName, formula, input.townRoot, input.townBeadsDir)
		}
	}
	target := ""
	if len(input.args) > 1 {
		target = input.args[1]
	}
	intent := intentFromCLIFlags(beadID, "", formulaName, input.townRoot, input.townBeadsDir)
	intent.Target = target
	_, err = executeDeepSling(ctx, intent)
	return err
}

func resolveSlingBeadOrFormula(input slingRunInput) (beadID, formulaName string, formulaOnly bool, err error) {
	if slingOnTarget != "" {
		formulaName = input.args[0]
		beadID = slingOnTarget
		if err = verifyBeadExists(beadID); err != nil {
			return "", "", false, err
		}
		if err = verifyFormulaExists(formulaName, beads.ResolveHookDir(input.townRoot, beadID, ""), input.townRoot); err != nil {
			return "", "", false, err
		}
		return beadID, formulaName, false, nil
	}

	firstArg := input.args[0]
	if err = verifyBeadExists(firstArg); err == nil {
		return firstArg, "", false, nil
	}
	if formulaErr := verifyFormulaExists(firstArg, input.townRoot, input.townRoot); formulaErr == nil {
		return "", "", true, nil
	}
	if looksLikeBeadID(firstArg) {
		return firstArg, "", false, nil
	}
	return "", "", false, fmt.Errorf("'%s' is not a valid bead or formula", firstArg)
}
