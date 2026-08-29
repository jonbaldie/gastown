package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/formula"
	"github.com/jonbaldie/gastown/internal/style"
)

const workflowTargetField = "workflow_target"

type workflowFormulaRun struct {
	f           *formula.Formula
	formulaName string
	targetRig   string
	opts        formulaRunOptions
	townBeads   string
	rigPrefix   string
	rigBeadsDir string
	workflowID  string
	setVars     map[string]interface{}
}

func executeWorkflowFormula(f *formula.Formula, formulaName, targetRig string, opts formulaRunOptions) error {
	fmt.Printf("%s Executing workflow formula: %s\n\n",
		style.Bold.Render("📋"), formulaName)
	w, err := beginWorkflowFormulaRun(f, formulaName, targetRig, opts)
	if err != nil {
		return err
	}
	if err := createWorkflowRootBead(w); err != nil {
		return err
	}
	stepBeads := createWorkflowStepBeads(w)
	slingCount, interactiveCount := dispatchWorkflowSteps(w, stepBeads)
	printWorkflowDispatched(w, slingCount, interactiveCount)
	return nil
}

func beginWorkflowFormulaRun(f *formula.Formula, formulaName, targetRig string, opts formulaRunOptions) (*workflowFormulaRun, error) {
	if len(f.Steps) == 0 {
		return nil, fmt.Errorf("workflow formula '%s' has no steps", formulaName)
	}
	townBeads, rigPrefix, rigBeadsDir, err := resolveConvoyBeads(targetRig)
	if err != nil {
		return nil, err
	}
	return &workflowFormulaRun{
		f:           f,
		formulaName: formulaName,
		targetRig:   targetRig,
		opts:        opts,
		townBeads:   townBeads,
		rigPrefix:   rigPrefix,
		rigBeadsDir: rigBeadsDir,
		setVars:     parseSetVars(opts.set),
	}, nil
}

func createWorkflowRootBead(w *workflowFormulaRun) error {
	w.workflowID = fmt.Sprintf("hq-wf-%s", generateFormulaShortID())
	workflowTitle := fmt.Sprintf("%s: %s (%d steps)", w.formulaName,
		truncate(w.f.Description, 50), len(w.f.Steps))
	description := fmt.Sprintf("Workflow: %s\n\nSteps: %d\nRig: %s",
		w.formulaName, len(w.f.Steps), w.targetRig)
	if beads.IsFlagLikeTitle(workflowTitle) {
		return fmt.Errorf("refusing to create workflow: title %q looks like a CLI flag", workflowTitle)
	}
	createArgs := []string{
		"create",
		"--type=task",
		"--id=" + w.workflowID,
		"--title=" + workflowTitle,
		"--description=" + description,
		"--labels=gt:convoy,gt:workflow",
	}
	if beads.NeedsForceForID(w.workflowID) {
		createArgs = append(createArgs, "--force")
	}
	if err := BdCmd(createArgs...).
		WithAutoCommit().
		Dir(w.townBeads).
		Stderr(os.Stderr).
		Run(); err != nil {
		return fmt.Errorf("creating workflow bead: %w", err)
	}
	fmt.Printf("%s Created workflow: %s\n", style.Bold.Render("✓"), w.workflowID)
	return nil
}

func createWorkflowStepBeads(w *workflowFormulaRun) map[string]string {
	stepBeads := make(map[string]string)
	for _, step := range w.f.Steps {
		if beadID, ok := createWorkflowStepBead(w, step, stepBeads); ok {
			stepBeads[step.ID] = beadID
		}
	}
	return stepBeads
}

func createWorkflowStepBead(w *workflowFormulaRun, step formula.Step, stepBeads map[string]string) (string, bool) {
	stepBeadID := fmt.Sprintf("%s-wfs-%s", w.rigPrefix, generateFormulaShortID())
	stepDescription := workflowStepDescription(step, substituteFormulaVars(step.Description, w.setVars))
	stepArgs := []string{
		"create",
		"--type=task",
		"--id=" + stepBeadID,
		"--title=" + step.Title,
		"--body-file=-",
	}
	if beads.NeedsForceForID(stepBeadID) {
		stepArgs = append(stepArgs, "--force")
	}
	if err := BdCmd(stepArgs...).
		Stdin(strings.NewReader(stepDescription)).
		WithAutoCommit().
		Dir(w.rigBeadsDir).
		Stderr(os.Stderr).
		Run(); err != nil {
		fmt.Printf("%s Failed to create step bead for %s: %v\n",
			style.Dim.Render("Warning:"), step.ID, err)
		return "", false
	}
	_ = addTrackingRelationFn(w.townBeads, w.workflowID, stepBeadID)
	wireWorkflowStepDeps(w, step, stepBeadID, stepBeads)
	needsStr := ""
	if len(step.Needs) > 0 {
		needsStr = fmt.Sprintf(" (needs: %s)", strings.Join(step.Needs, ", "))
	}
	fmt.Printf("  %s %s: %s%s\n", style.Dim.Render("○"), step.ID, stepBeadID, needsStr)
	return stepBeadID, true
}

func wireWorkflowStepDeps(w *workflowFormulaRun, step formula.Step, stepBeadID string, stepBeads map[string]string) {
	for _, needID := range step.Needs {
		depBeadID, ok := stepBeads[needID]
		if !ok {
			fmt.Printf("%s Step '%s' needs '%s' but it has no bead (ordering issue?)\n",
				style.Dim.Render("Warning:"), step.ID, needID)
			continue
		}
		_ = BdCmd("dep", "add", stepBeadID, depBeadID).
			WithAutoCommit().
			Dir(w.rigBeadsDir).
			Run()
	}
}

func dispatchWorkflowSteps(w *workflowFormulaRun, stepBeads map[string]string) (slingCount, interactiveCount int) {
	fmt.Printf("\n%s Dispatching ready steps...\n\n", style.Bold.Render("→"))
	hasInteractive := workflowHasInteractive(w.f)
	for _, step := range w.f.Steps {
		if len(step.Needs) > 0 {
			continue
		}
		stepBeadID, ok := stepBeads[step.ID]
		if !ok {
			continue
		}
		if step.Interactive || hasInteractive {
			hookWorkflowInteractiveStep(w, step, stepBeadID)
			interactiveCount++
			continue
		}
		if slingWorkflowStep(w, step, stepBeadID) {
			slingCount++
		}
	}
	return slingCount, interactiveCount
}

func workflowHasInteractive(f *formula.Formula) bool {
	for _, step := range f.Steps {
		if step.Interactive {
			return true
		}
	}
	return false
}

func hookWorkflowInteractiveStep(w *workflowFormulaRun, step formula.Step, stepBeadID string) {
	_ = BdCmd("update", stepBeadID, "--status=hooked").
		WithAutoCommit().
		Dir(w.rigBeadsDir).
		Run()
	fmt.Printf("  %s %s: %s (interactive — hooked to current session)\n",
		style.Bold.Render("⇨"), step.ID, stepBeadID)
	fmt.Printf("    %s\n", step.Title)
	fmt.Printf("    When done: bd close %s\n\n", stepBeadID)
}

func slingWorkflowStep(w *workflowFormulaRun, step formula.Step, stepBeadID string) bool {
	stepAgent := w.opts.agent
	if stepAgent == "" {
		stepAgent = w.f.Agent
	}
	stepTarget := workflowStepTarget(step, w.targetRig)
	stepDescription := workflowStepDescription(step, substituteFormulaVars(step.Description, w.setVars))
	slingArgs := buildWorkflowStepSlingArgs(stepBeadID, stepTarget, stepDescription, step.Title, stepAgent)
	slingCmd := exec.Command("gt", slingArgs...)
	slingCmd.Stdout = os.Stdout
	slingCmd.Stderr = os.Stderr
	if err := slingCmd.Run(); err != nil {
		fmt.Printf("%s Failed to sling step %s: %v\n",
			style.Dim.Render("Warning:"), step.ID, err)
		_ = BdCmd("comments", "add", stepBeadID, fmt.Sprintf("Failed to sling: %v", err)).
			Dir(w.townBeads).
			Run()
		return false
	}
	return true
}

func printWorkflowDispatched(w *workflowFormulaRun, slingCount, interactiveCount int) {
	blockedCount := len(w.f.Steps) - slingCount - interactiveCount
	fmt.Printf("\n%s Workflow dispatched!\n", style.Bold.Render("✓"))
	fmt.Printf("  Workflow: %s\n", w.workflowID)
	if interactiveCount > 0 {
		fmt.Printf("  Steps:    %d total, %d interactive (current session), %d dispatched, %d awaiting dependencies\n",
			len(w.f.Steps), interactiveCount, slingCount, blockedCount)
		fmt.Printf("\n  This workflow has interactive steps. Work through them sequentially:\n")
		fmt.Printf("    bd mol current <molecule-id>   — find current step\n")
		fmt.Printf("    bd close <step-id>             — advance to next step\n")
	} else {
		fmt.Printf("  Steps:    %d total, %d dispatched, %d awaiting dependencies\n",
			len(w.f.Steps), slingCount, blockedCount)
	}
	fmt.Printf("\n  Track progress: gt convoy status %s\n", w.workflowID)
}

func workflowStepDescription(step formula.Step, description string) string {
	target := strings.TrimSpace(step.Target)
	if target == "" {
		return description
	}
	return fmt.Sprintf("%s: %s\n\n%s", workflowTargetField, target, description)
}

func workflowStepTarget(step formula.Step, targetRig string) string {
	target := strings.TrimSpace(step.Target)
	if target == "" || target == "rig" {
		return targetRig
	}
	return target
}

func buildWorkflowStepSlingArgs(beadID, targetRig, description, title, agent string) []string {
	args := []string{
		"sling", beadID, targetRig,
		"-a", description,
		"-s", title,
		"--no-convoy",
	}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	return args
}
