package cmd

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

// branchInfo holds parsed branch information.
type branchInfo struct {
	Branch string // Full branch name
	Issue  string // Issue ID extracted from branch
	Worker string // Worker name (polecat name)
}

type mqSubmitOptions struct {
	branch    string
	issue     string
	epic      string
	priority  int
	noCleanup bool
	skipDeps  bool
	resubmit  bool
}

// issuePattern matches issue IDs in branch names (e.g., "gt-xyz" or "gt-abc.1")
var issuePattern = regexp.MustCompile(`([a-z]+-[a-z0-9]+(?:\.[0-9]+)?)`)

// parseBranchName extracts issue ID and worker from a branch name.
// Supports formats:
//   - polecat/<worker>/<issue>[+|@]<suffix>  → issue=<issue>, worker=<worker>
//   - polecat/<worker>/<issue>  → issue=<issue>, worker=<worker>
//   - polecat/<worker>-<suffix>  → issue="", worker=<worker>
//   - <issue>                   → issue=<issue>, worker=""
func parseBranchName(branch string) branchInfo {
	info := branchInfo{Branch: branch}

	if meta, ok := polecat.ParseBranchName(branch); ok {
		info.Worker = meta.Polecat
		info.Issue = meta.Issue
		return info
	}
	if strings.HasPrefix(branch, "polecat/") {
		return info
	}

	// Try to find an issue ID pattern in the branch name
	// Common patterns: prefix-xxx, prefix-xxx.n (subtask)
	if matches := issuePattern.FindStringSubmatch(branch); len(matches) > 1 {
		info.Issue = matches[1]
	}

	return info
}

func readMQSubmitOptions(cmd *cobra.Command) (mqSubmitOptions, error) {
	opts := mqSubmitOptions{priority: -1}
	if cmd == nil {
		return opts, nil
	}
	if err := readMQSubmitIdentityFlags(cmd, &opts); err != nil {
		return opts, err
	}
	if err := readMQSubmitPolicyFlags(cmd, &opts); err != nil {
		return opts, err
	}
	return opts, nil
}

func readMQSubmitIdentityFlags(cmd *cobra.Command, opts *mqSubmitOptions) error {
	var err error
	if opts.branch, err = cmd.Flags().GetString("branch"); err != nil {
		return fmt.Errorf("reading --branch: %w", err)
	}
	if opts.issue, err = cmd.Flags().GetString("issue"); err != nil {
		return fmt.Errorf("reading --issue: %w", err)
	}
	if opts.epic, err = cmd.Flags().GetString("epic"); err != nil {
		return fmt.Errorf("reading --epic: %w", err)
	}
	if opts.priority, err = cmd.Flags().GetInt("priority"); err != nil {
		return fmt.Errorf("reading --priority: %w", err)
	}
	return nil
}

func readMQSubmitPolicyFlags(cmd *cobra.Command, opts *mqSubmitOptions) error {
	var err error
	if opts.noCleanup, err = cmd.Flags().GetBool("no-cleanup"); err != nil {
		return fmt.Errorf("reading --no-cleanup: %w", err)
	}
	if opts.skipDeps, err = cmd.Flags().GetBool("skip-deps"); err != nil {
		return fmt.Errorf("reading --skip-deps: %w", err)
	}
	if opts.resubmit, err = cmd.Flags().GetBool("resubmit"); err != nil {
		return fmt.Errorf("reading --resubmit: %w", err)
	}
	return nil
}

func resolveMQSubmitCommitSHA(g *git.Git, branch string) (string, error) {
	return git.Rev(g, fmt.Sprintf("refs/heads/%s^{commit}", branch))
}

func verifyMQSubmitPushedBranch(g *git.Git, branch, commitSHA string) error {
	if commitSHA != "" {
		if err := git.VerifyPushedCommit(g, "origin", branch, commitSHA); err != nil {
			return fmt.Errorf("%w\n\nHint: run 'git push origin %s' first (or 'gt done'), then re-run 'gt mq submit'", err, branch)
		}
		return nil
	}

	exists, err := git.PushRemoteBranchExists(g, "origin", branch)
	if err != nil {
		return fmt.Errorf("verify branch on origin: %w\n\nHint: run 'git push origin %s' first (or 'gt done'), then re-run 'gt mq submit'", err, branch)
	}
	if !exists {
		return fmt.Errorf("branch %q not found on origin\n\nHint: run 'git push origin %s' first (or 'gt done'), then re-run 'gt mq submit'", branch, branch)
	}
	return nil
}

// checkMoleculeStepDeps verifies that all prerequisite molecule steps are closed
// before allowing submission to the merge queue. Returns an error listing
// incomplete steps if any prerequisites are not yet done.
func checkMoleculeStepDeps(bd *beads.Beads, sourceIssue *beads.Issue) error {
	// Check if issue has an attached molecule
	fields := beads.ParseAttachmentFields(sourceIssue)
	if fields == nil || fields.AttachedMolecule == "" {
		return nil // No molecule attached — no enforcement needed
	}

	moleculeID := fields.AttachedMolecule

	// List all molecule steps (children of the molecule)
	children, err := bd.List(beads.ListOptions{
		Parent:   moleculeID,
		Status:   "all",
		Priority: -1,
	})
	if err != nil {
		// If we can't list steps, warn but don't block submission
		style.PrintWarning("could not check molecule steps for %s: %v", moleculeID, err)
		return nil
	}

	return validateMoleculePrereqs(children)
}

// validateMoleculePrereqs checks that all molecule steps that are prerequisites
// of the submit step are closed. Returns an error listing incomplete steps.
// Extracted for testability — accepts step data directly.
func validateMoleculePrereqs(children []*beads.Issue) error {
	if len(children) == 0 {
		return nil
	}
	incompleteSteps := incompleteMoleculePrereqs(children, moleculeSubmitSequence(children))
	if len(incompleteSteps) == 0 {
		return nil
	}
	sortStepsBySequence(incompleteSteps)
	var sb strings.Builder
	sb.WriteString("molecule step dependencies not met — incomplete prerequisite steps:\n")
	for _, step := range incompleteSteps {
		sb.WriteString(fmt.Sprintf("  ✗ %s: %s [%s]\n", step.ID, step.Title, step.Status))
	}
	sb.WriteString("\nComplete these steps before submitting, or use --skip-deps to override.")
	return fmt.Errorf("%s", sb.String())
}

func moleculeSubmitSequence(children []*beads.Issue) int {
	submitSeq := 999999
	for _, child := range children {
		if strings.Contains(strings.ToLower(child.Title), "submit") {
			return extractStepSequence(child.ID)
		}
	}
	return submitSeq
}

func incompleteMoleculePrereqs(children []*beads.Issue, submitSeq int) []*beads.Issue {
	var incompleteSteps []*beads.Issue
	for _, child := range children {
		if extractStepSequence(child.ID) >= submitSeq {
			continue
		}
		if child.Status != "closed" {
			incompleteSteps = append(incompleteSteps, child)
		}
	}
	return incompleteSteps
}

// polecatCleanup sends a lifecycle shutdown request to the witness and waits for termination.
// This is called after a polecat successfully submits an MR.
func polecatCleanup(rigName, worker, townRoot string) error {
	// Send lifecycle request to witness
	manager := rigName + "/witness"
	subject := fmt.Sprintf("LIFECYCLE: polecat-%s requesting shutdown", worker)
	body := fmt.Sprintf(`Lifecycle request from polecat %s.

Action: shutdown
Reason: MR submitted to merge queue
Time: %s

Please verify state and execute lifecycle action.
`, worker, time.Now().Format(time.RFC3339))

	// Send via gt mail
	cmd := exec.Command("gt", "mail", "send", manager,
		"-s", subject,
		"-m", body,
	)
	cmd.Dir = townRoot

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sending lifecycle request: %w: %s", err, string(out))
	}
	fmt.Printf("%s Sent shutdown request to %s\n", style.Bold.Render("✓"), manager)

	// Wait for retirement with periodic status
	fmt.Println()
	fmt.Printf("%s Waiting for retirement...\n", style.Dim.Render("◌"))
	fmt.Println(style.Dim.Render("(Witness will terminate this session)"))

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Timeout after 5 minutes to prevent indefinite blocking
	const maxCleanupWait = 5 * time.Minute
	timeout := time.After(maxCleanupWait)

	waitStart := time.Now()
	for {
		select {
		case <-ticker.C:
			elapsed := time.Since(waitStart).Round(time.Second)
			fmt.Printf("%s Still waiting (%v elapsed)...\n", style.Dim.Render("◌"), elapsed)
			if elapsed >= 2*time.Minute {
				fmt.Println(style.Dim.Render("  Hint: If witness isn't responding, you may need to:"))
				fmt.Println(style.Dim.Render("  - Check if witness is running: gt rig status"))
				fmt.Println(style.Dim.Render("  - Use Ctrl+C to abort and manually exit"))
			}
		case <-timeout:
			fmt.Printf("%s Timeout waiting for polecat retirement\n", style.WarningPrefix)
			fmt.Println(style.Dim.Render("  The polecat may have already terminated, or witness is unresponsive."))
			fmt.Println(style.Dim.Render("  You can verify with: gt polecat status"))
			return nil // Don't fail the MR submission just because cleanup timed out
		}
	}
}
