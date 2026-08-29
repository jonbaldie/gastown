package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
)

// polecatTarget represents a polecat to operate on.
type polecatTarget struct {
	rigName     string
	polecatName string
	mgr         *polecat.Manager
	r           *rig.Rig
}

// resolvePolecatTargets builds a list of polecats from command args.
// If useAll is true, the first arg is treated as a rig name and all polecats in it are returned.
// Otherwise, args are parsed as rig/polecat addresses.
func resolvePolecatTargets(args []string, useAll bool) ([]polecatTarget, error) {
	if useAll {
		return resolveAllPolecatTargets(args)
	}
	return resolveExplicitPolecatTargets(args)
}

func resolveAllPolecatTargets(args []string) ([]polecatTarget, error) {
	// --all flag: first arg is just the rig name.
	rigName := args[0]
	if _, _, err := parseAddress(rigName); err == nil {
		return nil, fmt.Errorf("with --all, provide just the rig name (e.g., 'gt polecat <cmd> %s --all')", strings.Split(rigName, "/")[0])
	}

	mgr, r, err := getPolecatManager(rigName)
	if err != nil {
		return nil, err
	}
	polecats, err := mgr.List()
	if err != nil {
		return nil, fmt.Errorf("listing polecats: %w", err)
	}

	targets := make([]polecatTarget, 0, len(polecats))
	for _, p := range polecats {
		targets = append(targets, polecatTarget{
			rigName:     rigName,
			polecatName: p.Name,
			mgr:         mgr,
			r:           r,
		})
	}
	return targets, nil
}

func resolveExplicitPolecatTargets(args []string) ([]polecatTarget, error) {
	var targets []polecatTarget
	for _, arg := range args {
		if !strings.Contains(arg, "/") {
			return nil, fmt.Errorf("invalid address '%s': must be in 'rig/polecat' format (e.g., 'gastown/Toast')", arg)
		}

		rigName, polecatName, err := parseAddress(arg)
		if err != nil {
			return nil, fmt.Errorf("invalid address '%s': %w", arg, err)
		}

		mgr, r, err := getPolecatManager(rigName)
		if err != nil {
			return nil, err
		}

		targets = append(targets, polecatTarget{
			rigName:     rigName,
			polecatName: polecatName,
			mgr:         mgr,
			r:           r,
		})
	}

	return targets, nil
}

// SafetyCheckResult holds the result of safety checks for a polecat.
type SafetyCheckResult struct {
	Polecat       string
	Blocked       bool
	Reasons       []string
	CleanupStatus polecat.CleanupStatus
	HookBead      string
	HookStale     bool // true if hooked bead is closed
	ActiveMR      string
	OpenMR        string
	GitState      *GitState
}

type polecatSafetyInput struct {
	bd        *beads.Beads
	state     polecat.State
	issue     string
	clonePath string
	gitState  *GitState
	fields    *beads.AgentFields
	agentErr  error
}

// checkPolecatSafety performs safety checks before destructive operations.
// Returns nil if the polecat is safe to operate on, or a SafetyCheckResult with reasons if blocked.
func checkPolecatSafety(target polecatTarget) *SafetyCheckResult {
	result := &SafetyCheckResult{
		Polecat: fmt.Sprintf("%s/%s", target.rigName, target.polecatName),
	}
	input := loadPolecatSafetyInput(target)
	result.GitState = input.gitState
	if input.agentErr == nil && input.fields != nil {
		result.CleanupStatus = polecat.CleanupStatus(input.fields.CleanupStatus)
		result.HookBead = input.fields.HookBead
		result.ActiveMR = input.fields.ActiveMR
	}

	d := polecat.InspectWorkstate(target.polecatName, input.bd, input.clonePath, input.state, input.issue)
	if !d.Reusable && !d.SafeToNuke {
		result.Reasons = append(result.Reasons, d.Blockers...)
		if len(result.Reasons) == 0 && d.Reason != "" && d.Reason != "reusable" {
			result.Reasons = append(result.Reasons, d.Reason)
		}
	}
	result.Blocked = len(result.Reasons) > 0
	return result
}

func loadPolecatSafetyInput(target polecatTarget) polecatSafetyInput {
	input := polecatSafetyInput{
		bd:        beads.New(target.r.Path),
		state:     polecat.StateIdle,
		clonePath: polecat.ClonePathFor(target.r.Path, target.rigName, target.polecatName),
	}
	polecatInfo, infoErr := target.mgr.Get(target.polecatName)
	if infoErr == nil && polecatInfo != nil {
		input.state = polecatInfo.State
		input.issue = polecatInfo.Issue
		input.clonePath = polecatInfo.ClonePath
		if gitState, gitErr := getGitState(polecatInfo.ClonePath); gitErr == nil {
			input.gitState = gitState
		}
	}
	_, input.fields, input.agentErr = input.bd.GetAgentBead(polecatBeadIDForRig(target.r, target.rigName, target.polecatName))
	if input.agentErr == nil && input.fields != nil && input.issue == "" {
		input.issue = input.fields.LastSourceIssue
		if input.issue == "" {
			input.issue = input.fields.HookBead
		}
	}
	return input
}

func rigPrefix(r *rig.Rig) string {
	townRoot := filepath.Dir(r.Path)
	return beads.GetPrefixForRig(townRoot, r.Name)
}

func polecatBeadIDForRig(r *rig.Rig, rigName, polecatName string) string {
	return beads.PolecatBeadIDWithPrefix(rigPrefix(r), rigName, polecatName)
}

// displaySafetyCheckBlocked prints blocked polecats and guidance.
func displaySafetyCheckBlocked(blocked []*SafetyCheckResult) {
	displaySafetyCheckBlockedTo(os.Stderr, blocked)
}

func displaySafetyCheckBlockedTo(w io.Writer, blocked []*SafetyCheckResult) {
	fmt.Fprintf(w, "%s Cannot nuke the following polecats:\n\n", style.Error.Render("Error:"))
	var polecatList []string
	for _, b := range blocked {
		fmt.Fprintf(w, "  %s:\n", style.Bold.Render(b.Polecat))
		for _, r := range b.Reasons {
			fmt.Fprintf(w, "    - %s\n", r)
		}
		polecatList = append(polecatList, b.Polecat)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Safety checks failed. Resolve issues before nuking, or use --force.")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintln(w, "  1. Complete work: gt done (from polecat session)")
	fmt.Fprintln(w, "  2. Push changes: git push (from polecat worktree)")
	fmt.Fprintln(w, "  3. Escalate: gt mail send mayor/ -s \"RECOVERY_NEEDED\" -m \"...\"")
	fmt.Fprintf(w, "  4. Force nuke (LOSES WORK): gt polecat nuke --force %s\n", strings.Join(polecatList, " "))
	fmt.Fprintln(w)
}

func formatSafetyCheckBlockers(blocked []*SafetyCheckResult) string {
	parts := make([]string, 0, len(blocked))
	for _, b := range blocked {
		parts = append(parts, fmt.Sprintf("%s: %s", b.Polecat, strings.Join(b.Reasons, "; ")))
	}
	return strings.Join(parts, " | ")
}

// displayDryRunSafetyCheck shows safety check status for dry-run mode. It returns true when a normal nuke would refuse.
func displayDryRunSafetyCheck(target polecatTarget) bool {
	fmt.Printf("\n  Safety checks:\n")
	result := checkPolecatSafety(target)
	input := loadDryRunSafetyInput(target)
	displayDryRunCleanup(input)
	displayDryRunOpenMR(input)
	return result.Blocked
}

type dryRunSafetyInput struct {
	polecatInfo *polecat.Polecat
	infoErr     error
	bd          *beads.Beads
	agentIssue  *beads.Issue
	fields      *beads.AgentFields
	agentErr    error
}

func loadDryRunSafetyInput(target polecatTarget) dryRunSafetyInput {
	input := dryRunSafetyInput{bd: beads.New(target.r.Path)}
	input.polecatInfo, input.infoErr = target.mgr.Get(target.polecatName)
	agentBeadID := polecatBeadIDForRig(target.r, target.rigName, target.polecatName)
	input.agentIssue, input.fields, input.agentErr = input.bd.GetAgentBead(agentBeadID)
	return input
}

func displayDryRunCleanup(input dryRunSafetyInput) {
	if input.agentErr != nil || input.fields == nil {
		displayDryRunFallbackState(input)
		fmt.Printf("    - Hook: %s\n", style.Dim.Render("unknown (no agent bead)"))
		return
	}

	displayDryRunCleanupStatus(polecat.CleanupStatus(input.fields.CleanupStatus))
	displayDryRunHook(input.bd, input.agentIssue, input.fields)
	displayDryRunActiveMR(input)
}

func displayDryRunFallbackState(input dryRunSafetyInput) {
	if input.infoErr != nil || input.polecatInfo == nil {
		fmt.Printf("    - Git state: %s\n", style.Dim.Render("unknown (no polecat info)"))
		return
	}
	gitState, gitErr := getGitState(input.polecatInfo.ClonePath)
	if gitErr != nil {
		fmt.Printf("    - Git state: %s\n", style.Warning.Render("cannot check"))
	} else if gitState.Clean {
		fmt.Printf("    - Git state: %s\n", style.Success.Render("clean"))
	} else {
		fmt.Printf("    - Git state: %s\n", style.Error.Render("dirty"))
	}
}

func displayDryRunCleanupStatus(cleanupStatus polecat.CleanupStatus) {
	if cleanupStatus.IsSafe() {
		fmt.Printf("    - Cleanup status: %s\n", style.Success.Render(string(cleanupStatus)))
		return
	}
	if cleanupStatus.RequiresRecovery() {
		fmt.Printf("    - Cleanup status: %s\n", style.Error.Render(string(cleanupStatus)))
		return
	}
	statusText := string(cleanupStatus)
	if statusText == "" {
		statusText = "<missing>"
	}
	fmt.Printf("    - Cleanup status: %s\n", style.Warning.Render(statusText))
}

func displayDryRunHook(bd *beads.Beads, agentIssue *beads.Issue, fields *beads.AgentFields) {
	hookBead := agentIssue.HookBead
	if hookBead == "" {
		hookBead = fields.HookBead
	}
	if hookBead == "" {
		fmt.Printf("    - Hook: %s\n", style.Success.Render("empty"))
		return
	}
	hookedIssue, err := bd.Show(hookBead)
	if err == nil && hookedIssue != nil && hookedIssue.Status == "closed" {
		fmt.Printf("    - Hook: %s (%s, closed - stale)\n", style.Warning.Render("stale"), hookBead)
		return
	}
	fmt.Printf("    - Hook: %s (%s)\n", style.Error.Render("has work"), hookBead)
}

func displayDryRunActiveMR(input dryRunSafetyInput) {
	if input.fields.ActiveMR == "" {
		return
	}
	sourceHint := agentSourceIssueHint("", input.fields)
	gitSafe := input.infoErr == nil && input.polecatInfo != nil && activeMRGitSafeForWorktree(input.polecatInfo.ClonePath)
	if blocker := activeMRBlocker(input.bd, input.fields.ActiveMR, sourceHint, true, gitSafe); blocker != "" {
		fmt.Printf("    - Active MR: %s (%s)\n", style.Error.Render("blocked"), blocker)
		return
	}
	fmt.Printf("    - Active MR: %s (%s)\n", style.Success.Render("terminal"), input.fields.ActiveMR)
}

func displayDryRunOpenMR(input dryRunSafetyInput) {
	if input.infoErr != nil || input.polecatInfo == nil || input.polecatInfo.Branch == "" {
		fmt.Printf("    - Open MR: %s\n", style.Dim.Render("unknown (no branch info)"))
		return
	}
	mr, mrErr := input.bd.FindMRForBranch(input.polecatInfo.Branch)
	if mrErr == nil && mr != nil {
		fmt.Printf("    - Open MR: %s (%s)\n", style.Error.Render("yes"), mr.ID)
		return
	}
	fmt.Printf("    - Open MR: %s\n", style.Success.Render("none"))
}
