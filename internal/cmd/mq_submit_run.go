package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type mqSubmitRun struct {
	opts          mqSubmitOptions
	townRoot      string
	rigName       string
	cwd           string
	g             *git.Git
	branch        string
	defaultBranch string
	issueID       string
	worker        string
	target        string
	bd            *beads.Beads
	sourceBD      *beads.Beads
	sourceIssue   *beads.Issue
	priority      int
}

func runMqSubmit(cmd *cobra.Command, _ []string) error {
	r, err := beginMqSubmit(cmd)
	if err != nil {
		return err
	}
	if err := resolveMqSubmitSource(r); err != nil {
		return err
	}
	if err := resolveMqSubmitTarget(r); err != nil {
		return err
	}
	if err := checkMqSubmitDeps(r); err != nil {
		return err
	}
	mrIssue, err := registerMqSubmit(r)
	if err != nil {
		return err
	}
	printMqSubmitSuccess(r, mrIssue)
	return maybeMqSubmitCleanup(r)
}

func beginMqSubmit(cmd *cobra.Command) (*mqSubmitRun, error) {
	opts, err := readMQSubmitOptions(cmd)
	if err != nil {
		return nil, err
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigName, _, err := findCurrentRig(townRoot)
	if err != nil {
		return nil, err
	}
	cwd, err := resolveMqSubmitCwd(townRoot, rigName)
	if err != nil {
		return nil, err
	}
	g := git.NewGit(cwd)
	branch, defaultBranch, err := resolveMqSubmitBranch(g, townRoot, rigName, opts.branch)
	if err != nil {
		return nil, err
	}
	return &mqSubmitRun{
		opts:          opts,
		townRoot:      townRoot,
		rigName:       rigName,
		cwd:           cwd,
		g:             g,
		branch:        branch,
		defaultBranch: defaultBranch,
	}, nil
}

func resolveMqSubmitCwd(townRoot, rigName string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting current directory: %w", err)
	}
	if cwd != townRoot {
		return cwd, nil
	}
	if polecatCwd, ok := mqSubmitPolecatCwd(townRoot, rigName); ok {
		return polecatCwd, nil
	}
	if crewName := os.Getenv("GT_CREW"); crewName != "" && rigName != "" {
		crewClone := filepath.Join(townRoot, rigName, "crew", crewName)
		if _, err := os.Stat(crewClone); err == nil {
			return crewClone, nil
		}
	}
	return cwd, nil
}

func mqSubmitPolecatCwd(townRoot, rigName string) (string, bool) {
	if !mqSubmitIsPolecat() {
		return "", false
	}
	polecatName := os.Getenv("GT_POLECAT")
	if polecatName == "" || rigName == "" {
		return "", false
	}
	polecatClone := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if _, err := os.Stat(polecatClone); err == nil {
		return polecatClone, true
	}
	polecatClone = filepath.Join(townRoot, rigName, "polecats", polecatName)
	if _, err := os.Stat(filepath.Join(polecatClone, ".git")); err == nil {
		return polecatClone, true
	}
	return "", false
}

func mqSubmitIsPolecat() bool {
	if role := os.Getenv("GT_ROLE"); role != "" {
		parsedRole, _, _ := parseRoleString(role)
		return parsedRole == RolePolecat
	}
	return os.Getenv("GT_POLECAT") != ""
}

func resolveMqSubmitBranch(g *git.Git, townRoot, rigName, requested string) (string, string, error) {
	branch := requested
	if branch == "" {
		var err error
		branch, err = g.CurrentBranch()
		if err != nil {
			return "", "", fmt.Errorf("getting current branch: %w", err)
		}
	}
	defaultBranch := "main"
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	if branch == defaultBranch || branch == "master" {
		return "", "", fmt.Errorf("cannot submit %s/master branch to merge queue", defaultBranch)
	}
	return branch, defaultBranch, nil
}

func resolveMqSubmitSource(r *mqSubmitRun) error {
	info := parseBranchName(r.branch)
	r.issueID = r.opts.issue
	if r.issueID == "" {
		r.issueID = info.Issue
	}
	r.worker = info.Worker
	if r.issueID == "" {
		return fmt.Errorf("cannot determine source issue from branch '%s'; use --issue to specify", r.branch)
	}
	r.bd = beads.New(r.cwd)
	sourceInfo, err := resolveSubmitSourceIssue(r.cwd, r.issueID)
	if err != nil {
		return fmt.Errorf("source issue validation failed: %w", err)
	}
	r.sourceBD = sourceInfo.BD
	r.sourceIssue = sourceInfo.Issue
	if r.opts.priority >= 0 {
		r.priority = r.opts.priority
	} else {
		r.priority = r.sourceIssue.Priority
	}
	return nil
}

func resolveMqSubmitTarget(r *mqSubmitRun) error {
	r.target = r.defaultBranch
	if r.opts.epic != "" {
		r.target = resolveIntegrationBranchName(r.sourceBD, filepath.Join(r.townRoot, r.rigName), r.opts.epic)
		return nil
	}
	if af := beads.ParseAttachmentFields(r.sourceIssue); af != nil {
		if bb := extractFormulaVar(af.FormulaVars, "base_branch"); bb != "" && bb != r.defaultBranch {
			r.target = bb
			fmt.Printf("  Target branch override: %s (from formula_vars)\n", r.target)
		}
	}
	if r.target != r.defaultBranch {
		return nil
	}
	if !mqSubmitRefineryEnabled(r.townRoot, r.rigName) {
		return nil
	}
	autoTarget, err := beads.DetectIntegrationBranch(r.sourceBD, r.g, r.issueID)
	if err != nil {
		fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("(note: %v)", err)))
		return nil
	}
	if autoTarget != "" {
		r.target = autoTarget
	}
	return nil
}

func mqSubmitRefineryEnabled(townRoot, rigName string) bool {
	settingsPath := filepath.Join(townRoot, rigName, "settings", "config.json")
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil || settings.MergeQueue == nil {
		return true
	}
	return config.IsRefineryIntegrationEnabled(settings.MergeQueue)
}

func checkMqSubmitDeps(r *mqSubmitRun) error {
	if r.opts.skipDeps || r.opts.resubmit || r.sourceIssue == nil {
		return nil
	}
	return checkMoleculeStepDeps(r.sourceBD, r.sourceIssue)
}

func registerMqSubmit(r *mqSubmitRun) (*beads.Issue, error) {
	commitSHA, shaErr := resolveMQSubmitCommitSHA(r.g, r.branch)
	if shaErr != nil {
		style.PrintWarning("could not resolve submitted branch SHA: %v (falling back to branch-only dedup)", shaErr)
	}
	if err := verifyMQSubmitPushedBranch(r.g, r.branch, commitSHA); err != nil {
		return nil, err
	}
	existingMR, err := findExistingMqSubmit(r.bd, r.branch, commitSHA)
	if err != nil {
		style.PrintWarning("could not check for existing MR: %v", err)
	}
	if existingMR != nil {
		if err := validateMergeRequestSource(existingMR, r.issueID, r.sourceIssue); err != nil {
			return nil, fmt.Errorf("existing merge request validation failed: %w", err)
		}
		fmt.Printf("%s MR already exists (idempotent)\n", style.Bold.Render("✓"))
		return existingMR, nil
	}
	return createMqSubmit(r, commitSHA)
}

func findExistingMqSubmit(bd *beads.Beads, branch, commitSHA string) (*beads.Issue, error) {
	if commitSHA != "" {
		return bd.FindMRForBranchAndSHA(branch, commitSHA)
	}
	return bd.FindMRForBranch(branch)
}

func createMqSubmit(r *mqSubmitRun, commitSHA string) (*beads.Issue, error) {
	mrIssue, err := r.bd.Create(beads.CreateOptions{
		Title:       fmt.Sprintf("Merge: %s", r.issueID),
		Labels:      []string{"gt:merge-request"},
		Priority:    r.priority,
		Description: mqSubmitDescription(r, commitSHA),
		Ephemeral:   true,
		Rig:         r.rigName,
	})
	if err != nil {
		return nil, fmt.Errorf("creating merge request bead: %w", err)
	}
	if prefixErr := beads.ValidateRigPrefix(r.townRoot, r.rigName, mrIssue.ID); prefixErr != nil {
		style.PrintWarning("MR bead prefix mismatch: %v\nThe refinery may not find this MR — check 'gt mq list %s'", prefixErr, r.rigName)
	}
	nudgeRefinery(r.rigName, "MERGE_READY received - check inbox for pending work")
	backlinkMqSubmit(r, mrIssue.ID)
	supersedeOldMqSubmit(r, mrIssue.ID)
	return mrIssue, nil
}

func mqSubmitDescription(r *mqSubmitRun, commitSHA string) string {
	description := fmt.Sprintf("branch: %s\ntarget: %s\nsource_issue: %s\nrig: %s",
		r.branch, r.target, r.issueID, r.rigName)
	if commitSHA != "" {
		description += fmt.Sprintf("\ncommit_sha: %s", commitSHA)
	}
	if r.worker != "" {
		description += fmt.Sprintf("\nworker: %s", r.worker)
	}
	return description
}

func backlinkMqSubmit(r *mqSubmitRun, mrID string) {
	if r.issueID == "" {
		return
	}
	comment := fmt.Sprintf("MR created: %s", mrID)
	if err := r.sourceBD.AddComment(r.issueID, comment); err != nil {
		style.PrintWarning("could not back-link source issue %s to MR %s: %v", r.issueID, mrID, err)
	}
}

func supersedeOldMqSubmit(r *mqSubmitRun, mrID string) {
	if r.issueID == "" {
		return
	}
	oldMRs, err := r.bd.FindOpenMRsForIssue(r.issueID)
	if err != nil {
		return
	}
	for _, old := range oldMRs {
		if old.ID == mrID {
			continue
		}
		reason := fmt.Sprintf("superseded by %s", mrID)
		if err := r.bd.CloseWithReason(reason, old.ID); err != nil {
			style.PrintWarning("could not supersede old MR %s: %v", old.ID, err)
			continue
		}
		fmt.Printf("  %s Superseded old MR: %s\n", style.Dim.Render("○"), old.ID)
	}
}

func printMqSubmitSuccess(r *mqSubmitRun, mrIssue *beads.Issue) {
	fmt.Printf("%s Submitted to merge queue\n", style.Bold.Render("✓"))
	fmt.Printf("  MR ID: %s\n", style.Bold.Render(mrIssue.ID))
	fmt.Printf("  Source: %s\n", r.branch)
	fmt.Printf("  Target: %s\n", r.target)
	fmt.Printf("  Issue: %s\n", r.issueID)
	if r.worker != "" {
		fmt.Printf("  Worker: %s\n", r.worker)
	}
	fmt.Printf("  Priority: P%d\n", r.priority)
}

func maybeMqSubmitCleanup(r *mqSubmitRun) error {
	if r.worker == "" || r.opts.noCleanup {
		return nil
	}
	fmt.Println()
	fmt.Printf("%s Auto-cleanup: polecat work submitted\n", style.Bold.Render("✓"))
	if err := polecatCleanup(r.rigName, r.worker, r.townRoot); err != nil {
		style.PrintWarning("Could not auto-cleanup: %v", err)
		fmt.Println(style.Dim.Render("  You may need to run 'gt handoff --shutdown' manually"))
	}
	return nil
}
