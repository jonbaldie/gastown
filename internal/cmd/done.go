package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/instructions"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/telemetry"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/worker"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

func newDoneCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "done",
		GroupID:     GroupWork,
		Annotations: map[string]string{AnnotationPolecatSafe: "true"},
		Short:       "Signal work ready for merge queue",
		Long: `Signal that your work is complete and ready for the merge queue.

This is a convenience command for polecats that:
1. Submits the current branch to the merge queue
2. Auto-detects issue ID from branch name
3. Notifies the Witness with the exit outcome
4. Exits the polecat session after durable handoff
   (Witness/refinery cleanup owns the retired sandbox)

Exit statuses:
  COMPLETED      - Work done, MR submitted (default)
  ESCALATED      - Hit blocker, needs human intervention
  DEFERRED       - Work paused, issue still open

Examples:
  gt done                              # Submit branch, notify COMPLETED, exit session
  gt done --pre-verified               # Submit with pre-verification fast-path
  gt done --target feat/my-branch      # Explicit MR target branch
  gt done --pre-verified --target feat/contract-review  # Pre-verified with explicit target
  gt done --issue gt-abc               # Explicit issue ID
  gt done --skip-verify                # Audit-only escape hatch for non-code closes
  gt done --status ESCALATED           # Signal blocker, skip MR
  gt done --status DEFERRED            # Pause work, skip MR`,
		RunE:         runDone,
		SilenceUsage: true, // Don't print usage on operational errors (confuses agents)
	}
	state := doneState()
	cmd.Flags().StringVar(&state.issue, "issue", "", "Source issue ID (default: parse from branch name)")
	cmd.Flags().IntVarP(&state.priority, "priority", "p", -1, "Override priority (0-4, default: inherit from issue)")
	cmd.Flags().StringVar(&state.status, "status", ExitCompleted, "Exit status: COMPLETED, ESCALATED, or DEFERRED")
	cmd.Flags().StringVar(&state.cleanupStatus, "cleanup-status", "", "Git cleanup status: clean, uncommitted, unpushed, stash, unknown (ZFC: agent-observed)")
	cmd.Flags().BoolVar(&state.resume, "resume", false, "Resume from last checkpoint (auto-detected, for Witness recovery)")
	cmd.Flags().BoolVar(&state.preVerified, "pre-verified", false, "Mark MR as pre-verified (polecat ran gates after rebasing onto target)")
	cmd.Flags().StringVar(&state.target, "target", "", "Explicit MR target branch (overrides formula_vars and auto-detection)")
	cmd.Flags().BoolVar(&state.skipVerify, "skip-verify", false, "Skip verified-push checks for audit/test-only completion (recorded on bead)")
	return cmd
}

type doneCommandState struct {
	issue         string
	priority      int
	status        string
	cleanupStatus string
	resume        bool
	preVerified   bool
	target        string
	skipVerify    bool
}

var doneCommandStateInstance = sync.OnceValue(func() *doneCommandState {
	return &doneCommandState{priority: -1, status: ExitCompleted}
})

func doneState() *doneCommandState {
	return doneCommandStateInstance()
}

// Valid exit types for gt done
const (
	ExitCompleted = "COMPLETED"
	ExitEscalated = "ESCALATED"
	ExitDeferred  = "DEFERRED"
)

func doneContaminationBaseRef(defaultBranch, explicitTarget string) string {
	targetBranch := defaultBranch
	if explicitTarget != "" {
		targetBranch = strings.TrimSpace(explicitTarget)
		if strings.HasPrefix(targetBranch, "origin/") || strings.HasPrefix(targetBranch, "upstream/") {
			return targetBranch
		}
	}

	return "origin/" + targetBranch
}

func shouldUpdateAgentStateOnDone(pushFailed, mrFailed bool) bool {
	return !pushFailed && !mrFailed
}

func shouldRetirePolecatSessionAfterDone(exitType, mergeStrategy string, pushFailed, mrFailed bool) bool {
	if exitType != ExitCompleted || pushFailed || mrFailed {
		return false
	}
	return mergeStrategy != "local"
}

type doneSessionKiller interface {
	KillSessionWithProcessesExcluding(_ string, _ []string) error
}

type donePolecatWorktree struct {
	townRoot    string
	cwd         string
	rigName     string
	polecatName string
	actor       string
}

var newDoneSessionKiller = func() doneSessionKiller {
	return tmux.NewTmux()
}

var updateAgentStateOnDoneFn = updateAgentStateOnDone

func updateAgentStateAfterSubmission(cwd, townRoot, exitType, issueID string, pushFailed, mrFailed bool) error {
	if !shouldUpdateAgentStateOnDone(pushFailed, mrFailed) {
		style.PrintWarning("skipping agent cleanup because push or MR submission failed")
		return nil
	}
	return updateAgentStateOnDoneFn(cwd, townRoot, exitType, issueID)
}

func resolveDonePolecatWorktree() (donePolecatWorktree, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return donePolecatWorktree{}, fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory unavailable: %w", err)
	}
	return resolveDonePolecatWorktreeAt(cwd)
}

func resolveDonePolecatWorktreeAt(cwd string) (donePolecatWorktree, error) {
	absCwd, err := validateDoneWorktreeCwd(cwd)
	if err != nil {
		return donePolecatWorktree{}, err
	}
	townRoot, err := doneTownRootForWorktree(absCwd)
	if err != nil {
		return donePolecatWorktree{}, err
	}
	actorRig, actorName, err := resolveDonePolecatIdentity()
	if err != nil {
		return donePolecatWorktree{}, err
	}
	gitRoot, err := doneGitRootInWorktree(absCwd)
	if err != nil {
		return donePolecatWorktree{}, err
	}
	return matchDoneAssignedWorktree(townRoot, actorRig, actorName, gitRoot)
}

func validateDoneWorktreeCwd(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory unavailable")
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolving current directory: %w", err)
	}
	info, err := os.Stat(absCwd)
	if err != nil {
		return "", fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("gt done must be run from the assigned polecat worktree: current path is not a directory: %s", absCwd)
	}
	return absCwd, nil
}

func doneTownRootForWorktree(absCwd string) (string, error) {
	townRoot, err := workspace.FindOrError(absCwd)
	if err != nil {
		return "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if err := doneValidateSessionTownRoot(townRoot); err != nil {
		return "", err
	}
	return townRoot, nil
}

func resolveDonePolecatIdentity() (string, string, error) {
	actorRig, actorName, err := donePolecatActorIdentity(os.Getenv("BD_ACTOR"))
	if err != nil {
		return "", "", err
	}
	roleRig, roleName, err := donePolecatEnvIdentity(os.Getenv("GT_ROLE"), os.Getenv("GT_RIG"), os.Getenv("GT_POLECAT"))
	if err != nil {
		return "", "", err
	}
	if actorRig != roleRig || actorName != roleName {
		return "", "", fmt.Errorf("gt done identity mismatch: BD_ACTOR=%s/polecats/%s but GT_ROLE/GT_RIG/GT_POLECAT resolve to %s/polecats/%s", actorRig, actorName, roleRig, roleName)
	}
	if err := doneRejectGitEnvOverrides(); err != nil {
		return "", "", err
	}
	return actorRig, actorName, nil
}

func doneGitRootInWorktree(absCwd string) (string, error) {
	gitRoot, err := doneGitTopLevel(absCwd)
	if err != nil {
		return "", fmt.Errorf("gt done must be run from the assigned polecat git worktree: %w", err)
	}
	gitRoot = doneCanonicalPath(gitRoot)
	canonicalCwd := doneCanonicalPath(absCwd)
	if !donePathWithin(gitRoot, canonicalCwd) {
		return "", fmt.Errorf("gt done must be run from the assigned polecat worktree: current directory %s is outside git root %s", canonicalCwd, gitRoot)
	}
	return gitRoot, nil
}

func matchDoneAssignedWorktree(townRoot, actorRig, actorName, gitRoot string) (donePolecatWorktree, error) {
	candidates, err := donePolecatWorktreeCandidates(townRoot, actorRig, actorName)
	if err != nil {
		return donePolecatWorktree{}, err
	}
	for _, candidate := range candidates {
		if gitRoot == doneCanonicalPath(candidate) {
			return donePolecatWorktree{
				townRoot:    townRoot,
				cwd:         gitRoot,
				rigName:     actorRig,
				polecatName: actorName,
				actor:       fmt.Sprintf("%s/polecats/%s", actorRig, actorName),
			}, nil
		}
	}
	return donePolecatWorktree{}, fmt.Errorf("gt done must be run from assigned polecat worktree %s; current git root is %s", strings.Join(candidates, " or "), gitRoot)
}

func donePolecatWorktreeCandidates(townRoot, rigName, polecatName string) ([]string, error) {
	nested := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	info, err := os.Stat(nested)
	if err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("assigned polecat worktree path is not a directory: %s", nested)
		}
		return []string{nested}, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("checking assigned polecat worktree %s: %w", nested, err)
	}

	return []string{filepath.Join(townRoot, rigName, "polecats", polecatName)}, nil
}

func donePolecatActorIdentity(actor string) (string, string, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", "", fmt.Errorf("gt done requires BD_ACTOR to identify the assigned polecat")
	}
	parts := strings.Split(actor, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] != "polecats" || parts[2] == "" {
		return "", "", fmt.Errorf("gt done is for polecats only (BD_ACTOR=%s)", actor)
	}
	if err := doneValidateIdentitySegment("BD_ACTOR rig", parts[0]); err != nil {
		return "", "", err
	}
	if err := doneValidateIdentitySegment("BD_ACTOR polecat", parts[2]); err != nil {
		return "", "", err
	}
	return parts[0], parts[2], nil
}

func donePolecatEnvIdentity(gtRole, gtRig, gtPolecat string) (string, string, error) {
	gtRole = strings.TrimSpace(gtRole)
	gtRig = strings.TrimSpace(gtRig)
	gtPolecat = strings.TrimSpace(gtPolecat)
	if err := doneRequirePolecatEnv(gtRole, gtRig, gtPolecat); err != nil {
		return "", "", err
	}
	if err := doneValidatePolecatEnvSegments(gtRig, gtPolecat); err != nil {
		return "", "", err
	}

	roleRig, rolePolecat, err := donePolecatRoleIdentity(gtRole)
	if err != nil {
		return "", "", err
	}
	if err := doneCheckPolecatRoleIdentity(roleRig, rolePolecat, gtRig, gtPolecat); err != nil {
		return "", "", err
	}

	return gtRig, gtPolecat, nil
}

func doneRequirePolecatEnv(gtRole, gtRig, gtPolecat string) error {
	if gtRole == "" {
		return fmt.Errorf("gt done requires GT_ROLE to identify the assigned polecat")
	}
	if gtRig == "" {
		return fmt.Errorf("gt done requires GT_RIG to identify the assigned polecat")
	}
	if gtPolecat == "" {
		return fmt.Errorf("gt done requires GT_POLECAT to identify the assigned polecat")
	}
	return nil
}

func doneValidatePolecatEnvSegments(gtRig, gtPolecat string) error {
	if err := doneValidateIdentitySegment("GT_RIG", gtRig); err != nil {
		return err
	}
	return doneValidateIdentitySegment("GT_POLECAT", gtPolecat)
}

func doneCheckPolecatRoleIdentity(roleRig, rolePolecat, gtRig, gtPolecat string) error {
	if roleRig != "" && roleRig != gtRig {
		return fmt.Errorf("gt done identity mismatch: GT_ROLE rig %s != GT_RIG %s", roleRig, gtRig)
	}
	if rolePolecat != "" && rolePolecat != gtPolecat {
		return fmt.Errorf("gt done identity mismatch: GT_ROLE polecat %s != GT_POLECAT %s", rolePolecat, gtPolecat)
	}
	return nil
}

func donePolecatRoleIdentity(gtRole string) (string, string, error) {
	if gtRole == string(RolePolecat) {
		return "", "", nil
	}
	parts := strings.Split(gtRole, "/")
	switch len(parts) {
	case 2:
		return donePolecatRoleIdentityScoped(gtRole)
	case 3:
		return donePolecatRoleIdentityPath(gtRole, parts)
	default:
		return "", "", fmt.Errorf("gt done is for polecats only (GT_ROLE=%s)", gtRole)
	}
}

func donePolecatRoleIdentityScoped(gtRole string) (string, string, error) {
	role, roleRig, rolePolecat := parseRoleString(gtRole)
	if role != RolePolecat || roleRig == "" || rolePolecat == "" {
		return "", "", fmt.Errorf("gt done is for polecats only (GT_ROLE=%s)", gtRole)
	}
	if err := doneValidateIdentitySegment("GT_ROLE rig", roleRig); err != nil {
		return "", "", err
	}
	if err := doneValidateIdentitySegment("GT_ROLE polecat", rolePolecat); err != nil {
		return "", "", err
	}
	return roleRig, rolePolecat, nil
}

func donePolecatRoleIdentityPath(gtRole string, parts []string) (string, string, error) {
	if parts[1] != "polecats" || parts[0] == "" || parts[2] == "" {
		return "", "", fmt.Errorf("gt done is for polecats only (GT_ROLE=%s)", gtRole)
	}
	if err := doneValidateIdentitySegment("GT_ROLE rig", parts[0]); err != nil {
		return "", "", err
	}
	if err := doneValidateIdentitySegment("GT_ROLE polecat", parts[2]); err != nil {
		return "", "", err
	}
	return parts[0], parts[2], nil
}

func doneValidateIdentitySegment(name, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("gt done invalid %s: %q is not a single path segment", name, value)
	}
	return nil
}

func doneValidateSessionTownRoot(townRoot string) error {
	current := doneCanonicalPath(townRoot)
	for _, envName := range []string{"GT_TOWN_ROOT", "GT_ROOT"} {
		envRoot := strings.TrimSpace(os.Getenv(envName))
		if envRoot == "" {
			continue
		}
		if doneCanonicalPath(envRoot) != current {
			return fmt.Errorf("gt done town root mismatch: %s=%s but current workspace is %s", envName, doneCanonicalPath(envRoot), current)
		}
	}
	return nil
}

func donePathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func doneRejectGitEnvOverrides() error {
	for _, envName := range []string{
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_INDEX_FILE",
		"GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NAMESPACE",
	} {
		if strings.TrimSpace(os.Getenv(envName)) != "" {
			return fmt.Errorf("gt done requires an unambiguous git worktree; unset %s", envName)
		}
	}
	return nil
}

func doneGitTopLevel(cwd string) (string, error) {
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolving git root for %s: %s", cwd, strings.TrimSpace(string(output)))
	}
	gitRoot := strings.TrimSpace(string(output))
	if gitRoot == "" {
		return "", fmt.Errorf("git root for %s is empty", cwd)
	}
	return gitRoot, nil
}

func doneCanonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

func polecatSessionRetirementTarget(rigName, polecatName string, pid int) (string, []string, bool) {
	if rigName == "" || polecatName == "" || pid <= 0 {
		return "", nil, false
	}
	return session.PolecatSessionName(session.PrefixFor(rigName), polecatName), []string{fmt.Sprintf("%d", pid)}, true
}

func retirePolecatSessionAfterDone(rigName, polecatName string, pid int) error {
	sessionName, excludePIDs, ok := polecatSessionRetirementTarget(rigName, polecatName, pid)
	if !ok {
		return nil
	}
	return newDoneSessionKiller().KillSessionWithProcessesExcluding(sessionName, excludePIDs)
}

func cleanupStatusAfterSuccessfulPush(status string) string {
	if status == "unpushed" || status == "has_unpushed" {
		return "clean"
	}
	return status
}

func cleanupStatusFromWorkState(workStatus *git.UncommittedWorkStatus, workDir string, branchPushed bool, unpushedCount int, branchPushedErr error) string {
	if workStatus == nil {
		return "unknown"
	}
	if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingSafetyNet(workDir) {
		return "uncommitted"
	}
	if workStatus.StashCount > 0 {
		return "stash"
	}
	if branchPushedErr != nil || !branchPushed || unpushedCount > 0 {
		return "unpushed"
	}
	return "clean"
}

var reviewEvidencePrefixes = []string{
	"report:",
	"findings:",
	"review:",
	"evidence:",
	"verdict:",
	"decision:",
	"pr-sheriff-evidence",
	"pr sheriff evidence",
}

var generatedCommentPrefixes = []string{
	"verified_push_",
	"mr created:",
}

func doneSourceCloseSkipReason(bd *beads.Beads, issueID string, issue *beads.Issue) (string, bool) {
	currentHead, _ := currentReviewEvidenceHead()
	return doneSourceCloseSkipReasonForHead(bd, issueID, issue, currentHead)
}

func doneNoMRSourceCloseSkipReason(bd *beads.Beads, issueID string, issue *beads.Issue) (string, bool) {
	return doneSourceCloseSkipReason(bd, issueID, issue)
}

func doneSkipPushForLocalStrategy(convoyInfo *ConvoyInfo, sourceIssue *beads.Issue) bool {
	if convoyInfo != nil && beads.IsLocalMergeStrategy(convoyInfo.MergeStrategy) {
		return true
	}
	if sourceIssue == nil {
		return false
	}
	if beads.HasLocalMergeStrategy(beads.ParseAttachmentFields(sourceIssue)) {
		return true
	}
	return beads.IssueTextImpliesLocalMerge(sourceIssue.Title + "\n" + sourceIssue.Description)
}

// doneTreatPushAsLocalFallback reports whether a failed origin push is a
// read-only / third-party remote rather than an agent crash. Those failures
// must stay local and must not set PushFailed (HIGH mayor escalation).
func doneTreatPushAsLocalFallback(err error) bool {
	return git.IsNonWritableRemoteError(err)
}

func doneDirectMergeSkipReason(bd *beads.Beads, issueID string, issue *beads.Issue, targetBranch string) string {
	if strings.TrimSpace(issueID) == "" {
		return "source issue is required for direct merge"
	}
	issue, skipReason, _ := loadDoneSourceIssue(bd, issueID, issue)
	if skipReason != "" {
		return skipReason
	}
	if err := validateConcreteSourceIssue(issueID, issue); err != nil {
		return err.Error()
	}
	if attachment := beads.ParseAttachmentFields(issue); attachment != nil {
		switch {
		case attachment.NoMerge:
			return fmt.Sprintf("source_issue %s has no_merge=true", issueID)
		case attachment.ReviewOnly:
			return fmt.Sprintf("review-only issue %s cannot be direct-merged to %s", issueID, targetBranch)
		case strings.EqualFold(strings.TrimSpace(attachment.MergeStrategy), "local"):
			return fmt.Sprintf("source_issue %s has merge_strategy=local", issueID)
		}
	}
	if unchecked := beads.HasUncheckedCriteria(issue); unchecked > 0 {
		return fmt.Sprintf("issue %s has %d unchecked acceptance criteria — skipping direct merge", issueID, unchecked)
	}
	return ""
}

func doneSourceCloseSkipReasonForHead(bd *beads.Beads, issueID string, issue *beads.Issue, currentHead string) (string, bool) {
	issue, skipReason, fatal := loadDoneSourceIssue(bd, issueID, issue)
	if skipReason != "" {
		return skipReason, fatal
	}
	if err := validateConcreteSourceIssue(issueID, issue); err != nil {
		return err.Error(), true
	}
	// merge_strategy=local skips push and the merge queue. It does not skip close.
	// A successful local gt done must close the work bead so convoy progress lands.
	if skipReason, fatal := doneReviewOnlyCloseSkipReasonForHead(bd, issueID, issue, currentHead); skipReason != "" {
		return skipReason, fatal
	}
	if unchecked := beads.HasUncheckedCriteria(issue); unchecked > 0 {
		return fmt.Sprintf("issue %s has %d unchecked acceptance criteria — skipping close", issueID, unchecked), false
	}
	return "", false
}

func sourceUsesMergeQueue(issue *beads.Issue) bool {
	if issue == nil {
		return false
	}
	fields := beads.ParseAttachmentFields(issue)
	if fields == nil {
		return false
	}
	// ResolveMergeStrategy intentionally defaults absent metadata to "mr" for
	// submission, but that default cannot prove a submission happened.
	return strings.EqualFold(strings.TrimSpace(fields.MergeStrategy), "mr")
}

func doneReviewOnlyCloseSkipReason(bd *beads.Beads, issueID string, issue *beads.Issue) (string, bool) {
	issue, skipReason, fatal := loadDoneSourceIssue(bd, issueID, issue)
	if skipReason != "" {
		return skipReason, fatal
	}
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil || !attachment.ReviewOnly {
		return "", false
	}
	currentHead, err := currentReviewEvidenceHead()
	if err != nil {
		return fmt.Sprintf("could not verify review evidence for %s: %v", issueID, err), true
	}
	return doneReviewOnlyCloseSkipReasonForHead(bd, issueID, issue, currentHead)
}

func doneReviewOnlyCloseSkipReasonForHead(bd *beads.Beads, issueID string, issue *beads.Issue, currentHead string) (string, bool) {
	issue, skipReason, fatal := loadDoneSourceIssue(bd, issueID, issue)
	if skipReason != "" {
		return skipReason, fatal
	}
	attachment := beads.ParseAttachmentFields(issue)
	if attachment == nil || !attachment.ReviewOnly {
		return "", false
	}
	assignmentAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(attachment.AttachedAt))
	if err != nil {
		return fmt.Sprintf("review-only issue %s has no fresh assignment timestamp — re-sling it or add evidence after a fresh assignment", issueID), true
	}
	if strings.TrimSpace(issue.Assignee) == "" {
		return fmt.Sprintf("review-only issue %s has no assignee for evidence author validation", issueID), true
	}
	currentHead = strings.TrimSpace(currentHead)
	if currentHead == "" {
		return fmt.Sprintf("review-only issue %s has no current HEAD for evidence validation", issueID), true
	}
	hasEvidence, err := hasFreshReviewReportEvidence(bd, issueID, issue, assignmentAt, issue.Assignee, currentHead)
	if err != nil {
		return fmt.Sprintf("could not verify review evidence for %s: %v", issueID, err), true
	}
	if !hasEvidence {
		return fmt.Sprintf("review-only issue %s has no fresh review evidence comment for assignee %s and head %s", issueID, strings.TrimSpace(issue.Assignee), currentHead), true
	}
	return "", false
}

func loadDoneSourceIssue(bd *beads.Beads, issueID string, issue *beads.Issue) (*beads.Issue, string, bool) {
	if issueID == "" {
		return nil, "", false
	}
	if issue != nil {
		return issue, "", false
	}
	if bd == nil {
		return nil, fmt.Sprintf("could not inspect issue %s close eligibility", issueID), true
	}
	loaded, err := bd.Show(issueID)
	if err != nil {
		return nil, fmt.Sprintf("could not inspect issue %s close eligibility: %v", issueID, err), true
	}
	return loaded, "", false
}

func currentReviewEvidenceHead() (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolving current HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func hasFreshReviewReportEvidence(bd *beads.Beads, issueID string, issue *beads.Issue, assignmentAt time.Time, assignee, currentHead string) (bool, error) {
	if issue != nil {
		if hasFreshReviewEvidenceComment(issue.Comments, assignmentAt, assignee, currentHead) {
			return true, nil
		}
	}
	if bd == nil || issueID == "" {
		return false, nil
	}
	comments, err := bd.Comments(issueID)
	if err != nil {
		return false, err
	}
	if hasFreshReviewEvidenceComment(comments, assignmentAt, assignee, currentHead) {
		return true, nil
	}
	return false, nil
}

func hasFreshReviewEvidenceComment(comments []beads.Comment, assignmentAt time.Time, assignee, currentHead string) bool {
	assignee = strings.TrimSpace(assignee)
	currentHead = strings.TrimSpace(currentHead)
	if assignee == "" || currentHead == "" {
		return false
	}
	for _, comment := range comments {
		if reviewCommentMatchesEvidence(comment, assignmentAt, assignee, currentHead) {
			return true
		}
	}
	return false
}

func reviewCommentMatchesEvidence(comment beads.Comment, assignmentAt time.Time, assignee, currentHead string) bool {
	createdAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(comment.CreatedAt))
	if err != nil || !createdAt.After(assignmentAt) {
		return false
	}
	if strings.TrimSpace(comment.Author) != assignee {
		return false
	}
	if isGeneratedReviewComment(comment.Text) || !isReviewEvidenceText(comment.Text) {
		return false
	}
	return reviewEvidenceHeadSHA(comment.Text) == currentHead
}

func isGeneratedReviewComment(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(strings.TrimSpace(line))
		if lower == "" {
			continue
		}
		for _, prefix := range generatedCommentPrefixes {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
}

func reviewEvidenceHeadSHA(text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		for _, key := range []string{"head_sha", "target_head_sha", "head"} {
			for _, sep := range []string{":", "="} {
				prefix := key + sep
				if strings.HasPrefix(lower, prefix) {
					return strings.TrimSpace(trimmed[len(prefix):])
				}
			}
		}
	}
	return ""
}

func isReviewEvidenceText(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		generated := false
		for _, prefix := range generatedCommentPrefixes {
			if strings.HasPrefix(lower, prefix) {
				generated = true
				break
			}
		}
		if generated {
			continue
		}
		for _, prefix := range reviewEvidencePrefixes {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
}

func init() {
	rootCmd.AddCommand(newDoneCommand())
}

func runDone(_ *cobra.Command, _ []string) (retErr error) {
	defer func() { telemetry.RecordDone(context.Background(), strings.ToUpper(doneState().status), retErr) }()
	// Guard: Only polecats should call gt done
	// Crew, deacons, witnesses etc. don't use gt done - they persist across tasks.
	// Polecat sessions end with gt done — the session is cleaned up, but the
	// polecat's persistent identity (agent bead, CV chain) survives across assignments.
	actor := os.Getenv("BD_ACTOR")
	if actor != "" && !isPolecatActor(actor) {
		return fmt.Errorf("gt done is for polecats only (you are %s)\nPolecat sessions end with gt done — the session is cleaned up, but identity persists.\nOther roles persist across tasks and don't use gt done.", actor)
	}

	// Validate exit status
	exitType := strings.ToUpper(doneState().status)
	if exitType != ExitCompleted && exitType != ExitEscalated && exitType != ExitDeferred {
		return fmt.Errorf("invalid exit status '%s': must be COMPLETED, ESCALATED, or DEFERRED", doneState().status)
	}

	// Clean completions retire the live polecat session after durable handoff.
	// Failed, deferred, escalated, and local-review paths preserve the session for recovery.

	worktree, err := resolveDonePolecatWorktree()
	if err != nil {
		return err
	}
	townRoot := worktree.townRoot
	cwd := worktree.cwd
	rigName := worktree.rigName
	polecatName := worktree.polecatName
	sender := worktree.actor

	g := git.NewGit(cwd)

	branch, err := g.CurrentBranch()
	if err != nil {
		return fmt.Errorf("getting current branch: %w", err)
	}

	// Auto-detect cleanup status if not explicitly provided
	// This prevents premature polecat cleanup by ensuring witness knows git state
	if doneState().cleanupStatus == "" {
		workStatus, err := g.CheckUncommittedWork()
		if err != nil {
			style.PrintWarning("could not auto-detect cleanup status: %v", err)
		} else {
			// CheckUncommittedWork.UnpushedCommits doesn't work for branches
			// without upstream tracking (common for polecats). Use the more
			// robust BranchPushedToRemote which compares against origin/main.
			pushed, unpushedCount, pushErr := g.BranchPushedToRemote(branch, "origin")
			if pushErr != nil {
				style.PrintWarning("could not check if branch is pushed: %v", pushErr)
			}
			doneState().cleanupStatus = cleanupStatusFromWorkState(workStatus, cwd, pushed, unpushedCount, pushErr)
		}
	}

	// SAFETY NET (gt-pvx, stash recovery): If we detected stashes belonging to
	// this branch, auto-pop them so the existing uncommitted-work auto-commit
	// path (below) catches the contents and saves them as a normal commit.
	//
	// Background: agents have been observed running `git stash` to clear the
	// working tree before rebase/checkout, then dying before `git stash pop`.
	// The stash entries become orphaned in .git/refs/stash, surviving for
	// indefinite periods and silently leaking work. By popping them on the way
	// out of `gt done`, the recovery flow turns "lost" stashes into a
	// committed safety-net snapshot.
	//
	// Pop happens oldest-first so the most recent state ends up on top of the
	// working tree (matches what a user would do manually). If any pop has
	// conflicts, we stop and let the agent/user resolve — surfacing the
	// conflict is better than silently dropping the stash.
	if doneState().cleanupStatus == "stash" {
		entries, err := g.StashListForBranch()
		if err != nil {
			style.PrintWarning("auto-pop: could not list stashes: %v — orphaned stashes may remain", err)
		} else if len(entries) > 0 {
			fmt.Printf("\n%s %d stash(es) detected on this branch — auto-popping (gt-pvx safety net)\n",
				style.Bold.Render("⚠"), len(entries))
			// Pop oldest first: iterate in reverse so newest lands on top.
			popFailed := false
			for i := len(entries) - 1; i >= 0; i-- {
				e := entries[i]
				fmt.Printf("  popping %s — %s\n", e.Ref, e.Message)
				if popErr := g.StashPop(e.Ref); popErr != nil {
					style.PrintWarning("auto-pop %s failed (likely conflict): %v", e.Ref, popErr)
					style.PrintWarning("stopping pop chain — resolve conflict manually then re-run gt done")
					popFailed = true
					break
				}
				// After each pop, stash refs shift; re-fetch the list before next pop.
				entries, err = g.StashListForBranch()
				if err != nil || len(entries) == 0 {
					break
				}
			}
			if !popFailed {
				// Re-evaluate cleanup status: pops likely produced uncommitted changes
				// that the next block will auto-commit. Worst case, status was already
				// uncommitted and the next block runs anyway.
				if workStatus, wsErr := g.CheckUncommittedWork(); wsErr == nil && workStatus.HasUncommittedChanges {
					doneState().cleanupStatus = "uncommitted"
					fmt.Printf("%s Stash content moved to working tree — will auto-commit below.\n",
						style.Bold.Render("✓"))
				} else {
					// Pops succeeded but produced nothing dirty (e.g. stashes were
					// already merged). Recompute status normally.
					doneState().cleanupStatus = ""
				}
			}
		}
	}

	// SAFETY NET: Auto-commit uncommitted work before ANY exit path (gt-pvx).
	// Polecats have been observed running gt done without committing their
	// implementation work (1000s of lines lost). This happened because:
	// 1. The agent skips the "commit changes" formula step
	// 2. The COMPLETED check blocks, but the agent retries with --status DEFERRED
	//    which skips all checks
	// 3. The agent's session dies after the error, before it can commit
	//
	// Auto-commit ensures work is NEVER lost regardless of exit type or agent behavior.
	// The commit message is clearly marked as an auto-save so reviewers know.
	if doneState().cleanupStatus == "uncommitted" {
		// Re-check to get file details (cleanup detection already confirmed uncommitted changes)
		workStatus, err := g.CheckUncommittedWork()
		if err == nil && workStatus.HasUncommittedChanges && !workStatus.CleanExcludingSafetyNet(cwd) {
			if len(workStatus.UnmergedFiles) > 0 {
				return fmt.Errorf("cannot auto-save unmerged conflicts: %s\nResolve conflicts first, or use --status DEFERRED to exit without completing", strings.Join(workStatus.UnmergedFiles, ", "))
			}

			fmt.Printf("\n%s Uncommitted changes detected — auto-saving to prevent work loss\n", style.Bold.Render("⚠"))
			fmt.Printf("  Files: %s\n\n", workStatus.String())

			// Stage recoverable source changes only. Do not use git add -A:
			// that command commits untracked binaries that gitignore does not name.
			if addErr := g.StageSafetyNet(); addErr != nil {
				style.PrintWarning("auto-commit: git add failed: %v — uncommitted work may be at risk", addErr)
			} else {
				// Unstage Gas Town overlay files if a tracked overlay was modified.
				_ = g.ResetFiles("CLAUDE.local.md")
				_ = g.ResetFiles("AGENTS.local.md")
				for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
					if data, readErr := os.ReadFile(filepath.Join(cwd, name)); readErr == nil {
						if instructions.IsGasTownOverlay(string(data)) {
							_ = g.ResetFiles(name)
						}
					}
				}
				// Build a descriptive commit message
				autoMsg := "fix: auto-save uncommitted implementation work (gt-pvx safety net)"
				if issueFromBranch := parseBranchName(branch).Issue; issueFromBranch != "" {
					autoMsg = fmt.Sprintf("fix: auto-save uncommitted implementation work (%s, gt-pvx safety net)", issueFromBranch)
				}
				staged, stagedErr := g.HasStagedChanges()
				if stagedErr != nil {
					style.PrintWarning("auto-commit: checking staged changes failed: %v — uncommitted work may be at risk", stagedErr)
				} else if !staged {
					fmt.Printf("  No source changes to auto-save (binaries and runtime artifacts stay uncommitted).\n\n")
				} else if commitErr := g.Commit(autoMsg); commitErr != nil {
					style.PrintWarning("auto-commit: git commit failed: %v — uncommitted work may be at risk", commitErr)
				} else {
					fmt.Printf("%s Auto-committed uncommitted work (safety net)\n", style.Bold.Render("✓"))
					fmt.Printf("  The agent should have committed before running gt done.\n")
					fmt.Printf("  This auto-save prevents work loss.\n\n")
					doneState().cleanupStatus = "unpushed" // Update status — changes are now committed but not pushed
				}
			}
		}
	}

	// Parse branch info
	info := parseBranchName(branch)

	// Override with explicit flags
	issueID := doneState().issue
	if issueID == "" {
		issueID = info.Issue
	}
	worker := info.Worker

	// Get agent bead ID for cross-referencing
	var agentBeadID string
	if roleInfo, err := GetRoleWithContext(cwd, townRoot); err == nil {
		if actor := roleInfo.ActorString(); actor != "" {
			sender = actor
		}
		ctx := RoleContext{
			Role:     roleInfo.Role,
			Rig:      roleInfo.Rig,
			Polecat:  roleInfo.Polecat,
			TownRoot: townRoot,
			WorkDir:  cwd,
		}
		agentBeadID = getAgentBeadID(ctx)

		// Recreate the agent bead if it's missing (hq-xu4p). Done-intent
		// labels, checkpoints, and active_mr all write to it; when it's gone
		// every write fails 'issue not found' and witness zombie detection +
		// done-resume silently degrade. Best-effort: a failed recreate just
		// leaves the existing warnings.
		ensureAgentBeadExists(beads.New(cwd).ForAgentBead(), agentBeadID, ctx)

		// Completion now exits the live polecat session after durable handoff.
		// The agent bead keeps lifecycle metadata for witness/refinery cleanup.
	}
	var assignedIssueIDs []string
	loadAssignedIssueIDs := func() []string {
		if assignedIssueIDs == nil && sender != "" {
			assignedIssueIDs = findAssignedBeadsForAgent(cwd, sender)
		}
		return assignedIssueIDs
	}

	// If issue ID not set by flag or branch name, query for hooked beads
	// assigned to this agent. This replaces reading agent_bead.hook_bead
	// (hq-l6mm5: direct bead tracking instead of agent bead slot).
	if issueID == "" && sender != "" {
		if hookIssue, ambiguous := selectAssignedIssue("", loadAssignedIssueIDs()); hookIssue != "" {
			issueID = hookIssue
		} else if ambiguous {
			return fmt.Errorf("multiple active assignments found for %s; cannot infer issue from hook. Use --issue to disambiguate", sender)
		}
	}

	// Stale-branch guard (hq-l0fj): a redispatched polecat that reuses its
	// previous work branch carries the OLD bead-id in the branch name, which
	// would mis-attribute this MR (close credit goes to a closed bead; the
	// real issue stays open and hooked). When the branch-derived id differs
	// from the hooked bead, trust the hook. An explicit --issue flag still
	// wins, and subtask branches of the hooked bead (e.g. gt-abc.1 under
	// hooked gt-abc) are left alone.
	if doneState().issue == "" && info.Issue != "" && sender != "" {
		if hookIssue, ambiguous := selectAssignedIssue(info.Issue, loadAssignedIssueIDs()); isStaleBranchIssue(info.Issue, hookIssue) {
			style.PrintWarning("branch %q embeds issue %s but your hooked bead is %s — submitting for %s (stale branch reuse?)", branch, info.Issue, hookIssue, hookIssue)
			fmt.Printf("  Fresh branches must be named polecat/<name>/<bead-id>+<suffix> for the bead you are working.\n")
			fmt.Printf("  Use --issue to override if the branch-derived id is actually correct.\n\n")
			issueID = hookIssue
		} else if ambiguous {
			return fmt.Errorf("branch %q embeds issue %s but %s has multiple active assignments; use --issue to disambiguate", branch, info.Issue, sender)
		}
	}

	// Write done-intent label EARLY, before push/MR operations.
	// If gt done crashes after this point, the Witness can detect the intent
	// and auto-nuke the zombie polecat.
	//
	// Also read existing checkpoints for resume capability (gt-aufru).
	// If gt done was interrupted (SIGTERM, context exhaustion, SIGKILL),
	// checkpoints indicate which stages completed. On re-invocation, we
	// skip those stages to avoid repeating work or hitting errors.
	checkpoints := map[DoneCheckpoint]string{}
	if agentBeadID != "" {
		// Agent bead lives in town DB despite rig prefix — bypass routing.
		bd := beads.New(cwd).ForAgentBead()
		setDoneIntentLabel(bd, agentBeadID, exitType)
		checkpoints = readDoneCheckpoints(bd, agentBeadID)
		if len(checkpoints) > 0 {
			fmt.Printf("%s Resuming gt done from checkpoint (previous run was interrupted)\n", style.Bold.Render("→"))
		}
	}

	// Write heartbeat state="exiting" (gt-3vr5: heartbeat v2).
	// Tells the witness we're in the gt done flow — trust the agent until
	// heartbeat goes stale. No timer-based inference needed.
	// Parallel to done-intent label for backwards compat during migration.
	if sessionName := os.Getenv("GT_SESSION"); sessionName != "" && townRoot != "" {
		polecat.TouchSessionHeartbeatWithState(townRoot, sessionName, polecat.HeartbeatExiting, "gt done", issueID)
	}

	// Get configured default branch for this rig
	defaultBranch := "main" // fallback
	if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	baseRef := g.CleanBaseRef("origin", defaultBranch, doneState().target)

	// For COMPLETED, we need an issue ID and branch must not be the default branch
	var mrID string
	var pushFailed bool
	var mrFailed bool
	var doneErrors []string
	var convoyInfo *ConvoyInfo // Populated if issue is tracked by a convoy
	var sourceIssueForNoMerge *beads.Issue
	var sourceBD *beads.Beads
	flow := &doneFlow{
		session: doneSession{
			exitType:    exitType,
			townRoot:    townRoot,
			cwd:         cwd,
			rigName:     rigName,
			polecatName: polecatName,
			sender:      sender,
			worker:      worker,
			agentBeadID: agentBeadID,
		},
		repo: doneGitState{
			g:             g,
			branch:        branch,
			defaultBranch: defaultBranch,
			baseRef:       baseRef,
		},
		work: doneWorkState{
			issueID:     issueID,
			checkpoints: checkpoints,
		},
	}
	notifyWitnessNow := func() error {
		flow.work.issueID = issueID
		flow.session.sender = sender
		flow.work.mrID = mrID
		flow.work.pushFailed = pushFailed
		flow.work.mrFailed = mrFailed
		flow.work.doneErrors = doneErrors
		flow.work.convoyInfo = convoyInfo
		flow.work.sourceIssueForNoMerge = sourceIssueForNoMerge
		flow.work.sourceBD = sourceBD
		return finishDone(flow)
	}
	if exitType == ExitCompleted {
		if branch == defaultBranch || branch == "master" {
			return fmt.Errorf("cannot submit %s/master branch to merge queue", defaultBranch)
		}

		// CRITICAL: Verify work exists before completing (hq-xthqf)
		// Polecats calling gt done without commits results in lost work.
		// We MUST check for:
		// 1. Working directory availability (can't verify git state without it)
		// 2. Uncommitted changes (work that would be lost)
		// 3. Unique commits compared to origin (ensures branch was pushed with actual work)

		// Block if there are uncommitted changes (would be lost on completion).
		// Runtime artifacts (.claude/, .opencode/, .beads/, .runtime/, __pycache__/) are
		// excluded — these are toolchain-managed and normally gitignored.
		// Without this filter, gt done fails on virtually every polecat because
		// Cursor creates .claude/ at runtime in every workspace.
		workStatus, err := g.CheckUncommittedWork()
		if err != nil {
			return fmt.Errorf("checking git status: %w", err)
		}
		if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingSafetyNet(cwd) {
			return fmt.Errorf("cannot complete: uncommitted changes would be lost\nCommit your changes first, or use --status DEFERRED to exit without completing\nUncommitted: %s", workStatus.String())
		}

		// Check if branch has commits ahead of the clean target base. In fork-backed
		// rigs this is upstream/main, not the fork's origin/main.
		aheadCount, err := g.CommitsAhead(baseRef, "HEAD")
		if err != nil {
			// Fallback to local branch comparison if origin not available
			aheadCount, err = g.CommitsAhead(defaultBranch, branch)
			if err != nil {
				// Can't determine - assume work exists and continue
				style.PrintWarning("could not check commits ahead of %s: %v", defaultBranch, err)
				aheadCount = 1
			}
		}

		// Check no_merge or review_only flags on the hooked bead. When set,
		// this is a non-code task (email, research, analysis, PRD review)
		// where zero commits is expected.
		// Must be checked before the zero-commit guard below (GH#2496, gt-kvf).
		isNoMergeTask := false
		reviewOnlySource := false
		if issueID != "" {
			sourceInfo, sourceErr := resolveSubmitSourceIssue(cwd, issueID)
			if sourceErr != nil {
				return fmt.Errorf("source issue validation failed: %w", sourceErr)
			}
			sourceIssueForNoMerge = sourceInfo.Issue
			sourceBD = sourceInfo.BD
			if af := beads.ParseAttachmentFields(sourceIssueForNoMerge); af != nil {
				if af.NoMerge || af.ReviewOnly {
					isNoMergeTask = true
				}
				reviewOnlySource = af.ReviewOnly
			}
		}

		// If no commits ahead, work was likely already merged or is a legitimate
		// report-only completion. Fork-backed rigs must not infer success from fork main.
		// For polecats, zero commits usually means the polecat sleepwalked through
		// implementation without writing code (gastown#1484, beads#emma).
		// The --cleanup-status=clean escape is preserved for legitimate report-only
		// tasks (audits, reviews) that the formula explicitly directs to use it.
		// no_merge/review_only tasks (GH#2496, gt-kvf) also bypass: non-code work has no commits by design.
		// IMPORTANT: The error message must NOT mention --cleanup-status=clean.
		// LLM agents read error messages and self-bypass (the original bug).
		if aheadCount == 0 {
			if os.Getenv("GT_POLECAT") != "" && doneState().cleanupStatus != "clean" && !isNoMergeTask {
				// Before failing, check whether commits exist on the remote feature branch.
				// After a polecat pushes to origin/<feature-branch> and submits an MR,
				// if master advances (e.g., other MRs land), the feature branch is no
				// longer ahead of origin/master — but the work WAS committed and pushed.
				// In that case, treat as "MR already submitted" and fall through. (GH#wd7)
				branchPushedWithWork := false
				if branch != defaultBranch {
					pushed, unpushed, pushErr := g.BranchPushedToRemote(branch, "origin")
					branchPushedWithWork = pushErr == nil && pushed && unpushed == 0
				}
				if !branchPushedWithWork {
					return fmt.Errorf("cannot complete: no commits on branch ahead of %s\n"+
						"Polecats must have at least 1 commit to submit.\n"+
						"If the bug was already fixed upstream: gt done --status DEFERRED\n"+
						"If you're blocked: gt done --status ESCALATED",
						baseRef)
				}
			}

			// Non-polecat (crew/mayor), polecat with --cleanup-status=clean
			// (report-only tasks like audits/reviews), or no_merge polecat
			// (non-code tasks like email/research per GH#2496):
			// zero commits is valid.
			fmt.Printf("%s Branch has no commits ahead of %s\n", style.Bold.Render("→"), baseRef)
			fmt.Printf("  Work was likely already merged or report-only.\n")
			fmt.Printf("  Skipping MR creation - completing without merge request.\n\n")

			// G15 fix: Close the base issue when completing with no MR.
			// Without this, no-op polecats (bug already fixed) leave issues stuck
			// in HOOKED state with assignee pointing to the nuked polecat.
			// Normally the Refinery closes after merge, but with no MR, nothing
			// would ever close the issue.
			if issueID != "" {
				bd := sourceBD
				if bd == nil {
					bd = beads.New(cwd)
				}

				skipClose := false
				if skipReason, fatal := doneNoMRSourceCloseSkipReason(bd, issueID, sourceIssueForNoMerge); skipReason != "" {
					style.PrintWarning("%s", skipReason)
					fmt.Printf("  The bead will remain open for witness/mayor review.\n")
					notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
					if fatal {
						return fmt.Errorf("cannot complete review-only/no-MR work: %s", skipReason)
					}
					skipClose = true
				}

				if !skipClose {
					closeReason := "Completed with no code changes (already fixed or already merged)"
					noMRCommitSHA, _ := g.Rev("HEAD")
					if doneState().skipVerify {
						noteVerifiedPushSkipped(bd, cwd, issueID, defaultBranch, noMRCommitSHA, "--skip-verify on no-MR close")
						if noMRCommitSHA != "" {
							closeReason = fmt.Sprintf("%s\nskip_verify: true\ntarget_branch: %s\ncommit_sha: %s", closeReason, defaultBranch, noMRCommitSHA)
						}
					} else if !isNoMergeTask {
						if g.ForkBackedRemote("origin") {
							return fmt.Errorf("cannot close no-MR code bead in fork/upstream mode: %s has no commits ahead of %s; use the fork PR flow instead", branch, baseRef)
						}
						if verifyErr := g.VerifyPushedCommitReachableFromPushTarget("origin", defaultBranch, noMRCommitSHA); verifyErr != nil {
							noteVerifiedPushFailure(bd, cwd, issueID, defaultBranch, noMRCommitSHA, verifyErr)
							return fmt.Errorf("cannot close no-MR code bead: %w", verifyErr)
						}
						if noMRCommitSHA != "" {
							closeReason = fmt.Sprintf("%s\ntarget_branch: %s\ncommit_sha: %s", closeReason, defaultBranch, noMRCommitSHA)
						}
					}
					// G15 fix: Force-close bypasses molecule dependency checks.
					// The polecat is about to be nuked — open wisps should not block closure.
					// Retry with backoff handles transient dolt lock contention (A2).
					var closeErr error
					for attempt := 1; attempt <= 3; attempt++ {
						closeErr = bd.ForceCloseWithReason(closeReason, issueID)
						if closeErr == nil {
							fmt.Printf("%s Issue %s closed (no MR needed)\n", style.Bold.Render("✓"), issueID)
							break
						}
						if attempt < 3 {
							style.PrintWarning("close attempt %d/3 failed: %v (retrying in %ds)", attempt, closeErr, attempt*2)
							time.Sleep(time.Duration(attempt*2) * time.Second)
						}
					}
					if closeErr != nil {
						style.PrintWarning("could not close issue %s after 3 attempts: %v (issue may be left HOOKED)", issueID, closeErr)
					}
				}
			}

			// Skip straight to witness notification (no MR needed)
			return notifyWitnessNow()
		}

		if reviewOnlySource {
			return fmt.Errorf("cannot complete review-only issue %s with commits ahead of %s; add a fresh review evidence comment and complete without code changes", issueID, baseRef)
		}

		// Branch contamination preflight: check if branch is significantly behind
		// the effective target branch, which indicates the branch may contain stale merge-base
		// artifacts that will pollute the PR diff. (GH#2220)
		//
		// gh#3400: Refresh remote tracking refs first so contamination check (and
		// the auto-rebase below) sees the current clean base. In fork-backed rigs,
		// that base is upstream/main, not the fork's origin/main.
		contaminationBase := baseRef
		if doneState().target != "" && doneState().target != defaultBranch {
			contaminationBase = doneContaminationBaseRef(defaultBranch, doneState().target)
		}
		fetchRemote := git.RemoteForRef(contaminationBase)
		if fetchRemote == "" {
			fetchRemote = "origin"
		}
		if fetchErr := g.Fetch(fetchRemote); fetchErr != nil {
			style.PrintWarning("could not fetch %s before contamination check: %v (proceeding with local refs)", fetchRemote, fetchErr)
		}
		contam, err := g.CheckBranchContamination(contaminationBase)
		if err == nil && contam.Behind > 0 {
			const warnThreshold = 50
			const blockThreshold = 200
			if contam.Behind >= blockThreshold {
				return fmt.Errorf("branch contamination: %d commits behind %s (threshold: %d)\n"+
					"The branch is severely stale and will include unrelated changes in the PR.\n"+
					"Fix: git fetch %s && git rebase %s",
					contam.Behind, contaminationBase, blockThreshold, fetchRemote, contaminationBase)
			} else if contam.Behind >= warnThreshold {
				style.PrintWarning("branch is %d commits behind %s — consider rebasing to avoid PR contamination", contam.Behind, contaminationBase)
			}

			// gh#3400: Auto-rebase the polecat branch onto the latest target before
			// push, so the resulting MR/PR has a current base.
			alreadyPushed := checkpoints[CheckpointPushed] == branch
			rebased, skipReason, rebaseErr := autoRebaseOnTarget(g, contaminationBase, contam.Behind, doneState().preVerified, alreadyPushed)
			if rebaseErr != nil {
				return rebaseErr
			}
			if rebased {
				fmt.Printf("%s Branch rebased onto %s\n", style.Bold.Render("✓"), contaminationBase)
				// Recompute commits ahead since rebase rewrote history.
				aheadCount, _ = g.CommitsAhead(baseRef, "HEAD")
			} else if skipReason != "" {
				style.PrintWarning("branch is %d commits behind %s but %s; skipping auto-rebase", contam.Behind, contaminationBase, skipReason)
			}
		}

		// Strip Gas Town overlay from the instruction pair before push.
		if stripped := stripOverlayInstructionFiles(g, defaultBranch, baseRef); stripped {
			// Recalculate commits ahead since we added a cleanup commit
			aheadCount, _ = g.CommitsAhead(baseRef, "HEAD")
		}

		// Determine merge strategy from convoy (gt-myofa.3)
		// Convoys can override the default MR-based workflow:
		//   direct: push commits straight to target branch, bypass refinery
		//   mr:     default — create merge-request bead, refinery merges
		//   local:  keep on feature branch, no push, no MR (for human review/upstream PRs)
		//
		// Primary: read convoy info from the issue's attachment fields (gt-7b6wf fix).
		// gt sling stores convoy_id and merge_strategy on the issue when dispatching,
		// which avoids unreliable cross-rig dep resolution at gt done time.
		// Fallback: dep-based lookup via getConvoyInfoForIssue (for issues dispatched
		// before this fix, or where attachment fields weren't set).
		convoyInfo = getConvoyInfoFromSourceIssue(sourceIssueForNoMerge)
		if convoyInfo == nil {
			convoyInfo = getConvoyInfoForIssue(issueID)
		}

		// Handle "local" strategy: skip push and MR entirely.
		// Check the current convoy and the issue itself so a re-dispatch without
		// --merge local cannot push a branch that must stay local.
		if doneSkipPushForLocalStrategy(convoyInfo, sourceIssueForNoMerge) {
			fmt.Printf("%s Local merge strategy: skipping push and merge queue\n", style.Bold.Render("→"))
			fmt.Printf("  Branch: %s\n", branch)
			if issueID != "" {
				fmt.Printf("  Issue: %s\n", issueID)
			}
			fmt.Println()
			fmt.Printf("%s\n", style.Dim.Render("Work stays on local feature branch."))
			return notifyWitnessNow()
		}

		// Handle "direct" strategy: push to target branch, skip MR
		if convoyInfo != nil && convoyInfo.MergeStrategy == "direct" {
			fmt.Printf("%s Direct merge strategy: pushing to %s\n", style.Bold.Render("→"), defaultBranch)
			directBd := sourceBD
			if directBd == nil {
				directBd = beads.New(cwd)
			}
			if skipReason := doneDirectMergeSkipReason(directBd, issueID, sourceIssueForNoMerge, defaultBranch); skipReason != "" {
				style.PrintWarning("%s", skipReason)
				notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
				return fmt.Errorf("cannot complete direct-merge work: %s", skipReason)
			}
			// Push submodule changes before direct push (gt-dzs)
			pushSubmoduleChanges(g, baseRef)
			directRefspec := branch + ":" + defaultBranch
			directPushErr := g.Push("origin", directRefspec, false)
			if directPushErr != nil {
				pushFailed = true
				errMsg := fmt.Sprintf("direct push to %s failed: %v", defaultBranch, directPushErr)
				doneErrors = append(doneErrors, errMsg)
				style.PrintWarning("%s", errMsg)
				return notifyWitnessNow()
			}
			directCommitSHA, _ := g.Rev("HEAD")
			if doneState().skipVerify {
				noteVerifiedPushSkipped(directBd, cwd, issueID, defaultBranch, directCommitSHA, "--skip-verify on direct merge")
			} else if verifyErr := g.VerifyPushedCommitReachableFromPushTarget("origin", defaultBranch, directCommitSHA); verifyErr != nil {
				pushFailed = true
				errMsg := verifyErr.Error()
				doneErrors = append(doneErrors, errMsg)
				noteVerifiedPushFailure(directBd, cwd, issueID, defaultBranch, directCommitSHA, verifyErr)
				style.PrintWarning("%s\nDirect merge pushed but remote verification failed. Source bead will remain in progress.", errMsg)
				return notifyWitnessNow()
			}
			fmt.Printf("%s Branch pushed directly to %s\n", style.Bold.Render("✓"), defaultBranch)
			doneState().cleanupStatus = cleanupStatusAfterSuccessfulPush(doneState().cleanupStatus)

			// Close the base issue — no MR/refinery will close it
			if issueID != "" {
				if skipReason, fatal := doneSourceCloseSkipReason(directBd, issueID, sourceIssueForNoMerge); skipReason != "" {
					style.PrintWarning("%s", skipReason)
					notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
					if fatal {
						return fmt.Errorf("cannot complete direct-merge work: %s", skipReason)
					}
				} else {
					closeReason := fmt.Sprintf("Direct merge to %s (convoy strategy)", defaultBranch)
					var closeErr error
					for attempt := 1; attempt <= 3; attempt++ {
						closeErr = directBd.ForceCloseWithReason(closeReason, issueID)
						if closeErr == nil {
							fmt.Printf("%s Issue %s closed (direct merge)\n", style.Bold.Render("✓"), issueID)
							break
						}
						if attempt < 3 {
							style.PrintWarning("close attempt %d/3 failed: %v (retrying in %ds)", attempt, closeErr, attempt*2)
							time.Sleep(time.Duration(attempt*2) * time.Second)
						}
					}
					if closeErr != nil {
						style.PrintWarning("could not close issue %s after 3 attempts: %v", issueID, closeErr)
					}
				}
			}

			return notifyWitnessNow()
		}

		// Default: "mr" strategy (or no convoy) — push branch, create MR bead

		if issueID == "" {
			return fmt.Errorf("cannot determine source issue from branch '%s'; use --issue to specify", branch)
		}

		// Initialize beads and validate the source before any remote mutation.
		// Without a redirect, MR beads are invisible to the Refinery.
		resolvedBeads := beads.ResolveBeadsDir(cwd)
		if beads.IsLocalBeadsDir(cwd, resolvedBeads) {
			fmt.Fprintf(os.Stderr, "WARNING: beads resolved to local dir %s (no shared-beads redirect)\n", resolvedBeads)
			fmt.Fprintf(os.Stderr, "  MR beads written here will be invisible to the Refinery — run 'gt polecat repair' to fix\n")
		}
		bd := beads.NewWithBeadsDir(cwd, resolvedBeads)

		// Fallback: check if issue belongs to a direct-merge convoy that the
		// primary check missed — e.g., issues dispatched before the attachment-field
		// fix, or where dep-based lookup failed at that point. This must happen
		// before the generic branch/submodule push because direct mode has no MR or
		// refinery recheck.
		convoyInfo = getConvoyInfoFromSourceIssue(sourceIssueForNoMerge)
		if convoyInfo == nil {
			convoyInfo = getConvoyInfoForIssue(issueID)
		}
		if convoyInfo != nil && convoyInfo.MergeStrategy == "direct" {
			fmt.Printf("%s Late-detected direct merge strategy: pushing to %s\n", style.Bold.Render("→"), defaultBranch)
			fmt.Printf("  Convoy: %s\n", convoyInfo.ID)
			directBd := sourceBD
			if directBd == nil {
				directBd = bd
			}
			if skipReason := doneDirectMergeSkipReason(directBd, issueID, sourceIssueForNoMerge, defaultBranch); skipReason != "" {
				style.PrintWarning("%s", skipReason)
				notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
				return fmt.Errorf("cannot complete direct-merge work: %s", skipReason)
			}

			pushSubmoduleChanges(g, baseRef)
			directRefspec := branch + ":" + defaultBranch
			directPushErr := g.Push("origin", directRefspec, false)
			if directPushErr != nil {
				pushFailed = true
				errMsg := fmt.Sprintf("direct push to %s failed: %v", defaultBranch, directPushErr)
				doneErrors = append(doneErrors, errMsg)
				style.PrintWarning("%s", errMsg)
				return notifyWitnessNow()
			}
			directCommitSHA, _ := g.Rev("HEAD")
			if doneState().skipVerify {
				noteVerifiedPushSkipped(directBd, cwd, issueID, defaultBranch, directCommitSHA, "--skip-verify on late direct merge")
			} else if verifyErr := g.VerifyPushedCommitReachableFromPushTarget("origin", defaultBranch, directCommitSHA); verifyErr != nil {
				pushFailed = true
				errMsg := verifyErr.Error()
				doneErrors = append(doneErrors, errMsg)
				noteVerifiedPushFailure(directBd, cwd, issueID, defaultBranch, directCommitSHA, verifyErr)
				style.PrintWarning("%s\nLate direct merge pushed but remote verification failed. Source bead will remain in progress.", errMsg)
				return notifyWitnessNow()
			}
			fmt.Printf("%s Branch pushed directly to %s\n", style.Bold.Render("✓"), defaultBranch)
			doneState().cleanupStatus = cleanupStatusAfterSuccessfulPush(doneState().cleanupStatus)

			if skipReason, fatal := doneSourceCloseSkipReason(directBd, issueID, sourceIssueForNoMerge); skipReason != "" {
				style.PrintWarning("%s", skipReason)
				notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, skipReason)
				if fatal {
					return fmt.Errorf("cannot complete direct-merge work: %s", skipReason)
				}
			} else {
				var closeErr error
				for attempt := 1; attempt <= 3; attempt++ {
					closeErr = directBd.ForceCloseWithReason(
						fmt.Sprintf("Direct merge to %s (convoy strategy, late detection)", defaultBranch), issueID)
					if closeErr == nil {
						fmt.Printf("%s Issue %s closed (direct merge)\n", style.Bold.Render("✓"), issueID)
						break
					}
					if attempt < 3 {
						style.PrintWarning("close attempt %d/3 failed: %v (retrying in %ds)", attempt, closeErr, attempt*2)
						time.Sleep(time.Duration(attempt*2) * time.Second)
					}
				}
				if closeErr != nil {
					style.PrintWarning("could not close issue %s after 3 attempts: %v", issueID, closeErr)
				}
			}

			return notifyWitnessNow()
		}

		afterPushNow := func() error {
			flow.work.issueID = issueID
			flow.session.sender = sender
			flow.work.mrID = mrID
			flow.work.pushFailed = pushFailed
			flow.work.mrFailed = mrFailed
			flow.work.doneErrors = doneErrors
			flow.work.convoyInfo = convoyInfo
			flow.work.sourceIssueForNoMerge = sourceIssueForNoMerge
			flow.work.sourceBD = sourceBD
			flow.work.bd = bd
			return runDoneAfterPushThenFinish(flow)
		}

		// Resume: skip push if already completed in a previous run (gt-aufru).
		// Validate checkpoint branch matches current branch (ge-sbo: stale checkpoint
		// on polecat reassignment causes new work to skip push for old branch).
		if checkpoints[CheckpointPushed] != "" {
			if pushCheckpointMatchesBranch(checkpoints, branch) {
				fmt.Printf("%s Branch already pushed (resumed from checkpoint)\n", style.Bold.Render("✓"))
				return afterPushNow()
			}
			// Stale checkpoint from a previous assignment — discard and push normally.
			fmt.Printf("→ Discarding stale push checkpoint (was for branch %s, now on %s)\n",
				checkpoints[CheckpointPushed], branch)
		}

		// CRITICAL: Push branch BEFORE creating MR bead (hq-6dk53, hq-a4ksk)
		// The MR bead triggers Refinery to process this branch. If the branch
		// isn't pushed yet, Refinery finds nothing to merge. The worktree gets
		// nuked at the end of gt done, so the commits are lost forever.
		//
		// Auto-push submodule changes BEFORE parent push (gt-dzs).
		// If the parent repo's submodule pointer references commits that don't
		// exist on the submodule's remote, the Refinery MR will be broken.
		// Detect modified submodules and push each one first.
		pushSubmoduleChanges(g, baseRef)

		// Use explicit refspec (branch:branch) to create the remote branch.
		// Without refspec, git push follows the tracking config — polecat branches
		// track origin/main, so a bare push sends commits to main directly,
		// bypassing the MR/refinery flow (G20 root cause).
		fmt.Printf("Pushing branch to remote...\n")
		refspec := branch + ":" + branch
		pushedCommitSHA, _ := g.Rev("HEAD")
		pushErr := g.Push("origin", refspec, false)
		if pushErr != nil {
			// Primary push failed — try fallback from the bare repo (GH #1348).
			// When polecat sessions are reused or worktrees are stale, the worktree's
			// git context may be broken. But the branch always exists in the bare repo
			// (.repo.git) because worktree commits share the same object database.
			style.PrintWarning("primary push failed: %v — trying bare repo fallback...", pushErr)
			rigPath := filepath.Join(townRoot, rigName)
			bareRepoPath := filepath.Join(rigPath, ".repo.git")
			if _, statErr := os.Stat(bareRepoPath); statErr == nil {
				bareGit := git.NewGitWithDir(bareRepoPath, "")
				pushErr = bareGit.Push("origin", refspec, false)
				if pushErr != nil {
					style.PrintWarning("bare repo push also failed: %v", pushErr)
				} else {
					fmt.Printf("%s Branch pushed via bare repo fallback\n", style.Bold.Render("✓"))
				}
			}
		}

		if pushErr != nil {
			// All push attempts failed
			errMsg := fmt.Sprintf("push failed for branch '%s': %v", branch, pushErr)
			if doneTreatPushAsLocalFallback(pushErr) {
				style.PrintWarning("%s\nOrigin is not writable. Keeping work on the local branch; this is not an agent failure.", errMsg)
				fmt.Printf("%s Local fallback: skipping merge queue. Use --push-url or --merge=local for third-party remotes.\n", style.Bold.Render("→"))
				return notifyWitnessNow()
			}
			pushFailed = true
			doneErrors = append(doneErrors, errMsg)
			style.PrintWarning("%s\nCommits exist locally but failed to push. Witness will be notified.", errMsg)
			return notifyWitnessNow()
		}

		// Verify the pushed branch tip is the exact local commit before creating
		// any MR bead. Branch-exists checks are insufficient: a stale remote
		// branch can exist while the new commit never reached origin.
		if pushedCommitSHA == "" {
			pushedCommitSHA, _ = g.Rev("HEAD")
		}
		if doneState().skipVerify {
			noteVerifiedPushSkipped(sourceBD, cwd, issueID, branch, pushedCommitSHA, "--skip-verify on branch push")
		} else if verifyErr := verifyPushedCommitWithBareFallback(g, townRoot, rigName, branch, pushedCommitSHA); verifyErr != nil {
			pushFailed = true
			errMsg := verifyErr.Error()
			doneErrors = append(doneErrors, errMsg)
			noteVerifiedPushFailure(sourceBD, cwd, issueID, branch, pushedCommitSHA, verifyErr)
			style.PrintWarning("%s\nCommits exist locally but verified push failed. Witness will be notified.", errMsg)
			return notifyWitnessNow()
		}
		fmt.Printf("%s Branch pushed to origin\n", style.Bold.Render("✓"))

		// Fix cleanup_status after successful push (gt-wcr).
		// Status was detected before push, so "unpushed" is now stale.
		doneState().cleanupStatus = cleanupStatusAfterSuccessfulPush(doneState().cleanupStatus)

		// Write push checkpoint for resume (gt-aufru)
		if agentBeadID != "" {
			// Agent bead lives in town DB despite rig prefix — bypass routing.
			cpBd := beads.New(cwd).ForAgentBead()
			writeDoneCheckpoint(cpBd, agentBeadID, CheckpointPushed, branch)
		}

		return afterPushNow()
	} else {
		printDoneNonCompleted(exitType, issueID, branch)
	}

	return notifyWitnessNow()
}

// pushSubmoduleChanges detects submodules modified between baseRef
// and HEAD, and pushes each submodule's new commit to its remote before the
// parent repo push. This prevents the parent's submodule pointer from
// referencing commits that don't exist on the submodule's remote (gt-dzs).
func pushSubmoduleChanges(g *git.Git, baseRef string) {
	subChanges, err := g.SubmoduleChanges(baseRef, "HEAD")
	if err != nil {
		// Non-fatal: repos without submodules return nil, nil.
		// Only warn if the error is real (not just "no submodules").
		style.PrintWarning("could not detect submodule changes: %v", err)
		return
	}
	for _, sc := range subChanges {
		if sc.NewSHA == "" {
			continue // Submodule removed, nothing to push
		}
		shortSHA := sc.NewSHA
		if len(shortSHA) > 8 {
			shortSHA = shortSHA[:8]
		}
		fmt.Printf("Pushing submodule %s (%s)...\n", sc.Path, shortSHA)
		if subPushErr := g.PushSubmoduleCommit(sc.Path, sc.NewSHA, "origin"); subPushErr != nil {
			style.PrintWarning("submodule push failed for %s: %v (parent push may fail)", sc.Path, subPushErr)
		} else {
			fmt.Printf("%s Submodule %s pushed\n", style.Bold.Render("✓"), sc.Path)
		}
	}
}

func forceCloseIssueWithRetry(closeFn func(string, ...string) error, issueID, reason, successFormat string) error {
	return forceCloseIssueWithRetrySleep(closeFn, issueID, reason, successFormat, time.Sleep)
}

func forceCloseIssueWithRetrySleep(closeFn func(string, ...string) error, issueID, reason, successFormat string, sleep func(time.Duration)) error {
	var closeErr error
	for attempt := 1; attempt <= 3; attempt++ {
		closeErr = closeFn(reason, issueID)
		if closeErr == nil {
			fmt.Printf("%s "+successFormat+"\n", style.Bold.Render("✓"), issueID)
			return nil
		}
		if attempt < 3 {
			style.PrintWarning("close attempt %d/3 failed: %v (retrying in %ds)", attempt, closeErr, attempt*2)
			sleep(time.Duration(attempt*2) * time.Second)
		}
	}
	return closeErr
}

func notifyDoneCloseSkipped(townRoot, rigName, sender, issueID, reason string) {
	if townRoot == "" || rigName == "" || issueID == "" {
		return
	}
	if sender == "" {
		sender = fmt.Sprintf("%s/polecat", rigName)
	}

	router := mail.NewRouter(townRoot)
	defer router.WaitPendingNotifications()
	msg := &mail.Message{
		To:      fmt.Sprintf("%s/witness", rigName),
		From:    sender,
		Subject: fmt.Sprintf("DONE_CLOSE_SKIPPED: %s", issueID),
		Body: fmt.Sprintf("gt done skipped closing %s.\n\nReason: %s\n\nThe bead remains open for witness/mayor review.",
			issueID, reason),
	}
	if err := router.Send(msg); err != nil {
		style.PrintWarning("could not notify witness about skipped close: %v", err)
	} else {
		fmt.Printf("%s Witness notified: DONE_CLOSE_SKIPPED\n", style.Bold.Render("✓"))
	}
}

func noteVerifiedPushFailure(sourceBD *beads.Beads, cwd, issueID, branch, commit string, verifyErr error) {
	if issueID == "" || cwd == "" {
		return
	}
	bd := sourceBD
	if bd == nil {
		bd, _, _ = routedIssueBeads(cwd, issueID)
	}
	inProgress := "in_progress"
	_ = bd.Update(issueID, beads.UpdateOptions{Status: &inProgress})
	msg := fmt.Sprintf("verified_push_failed: commit %s not verified on origin/%s: %v", commit, branch, verifyErr)
	_ = bd.AddComment(issueID, msg)
}

func noteVerifiedPushSkipped(sourceBD *beads.Beads, cwd, issueID, branch, commit, reason string) {
	if issueID == "" || cwd == "" {
		return
	}
	msg := fmt.Sprintf("verified_push_skipped: commit %s branch origin/%s reason=%s", commit, branch, reason)
	bd := sourceBD
	if bd == nil {
		bd, _, _ = routedIssueBeads(cwd, issueID)
	}
	_ = bd.AddComment(issueID, msg)
}

func verifyPushedCommitWithBareFallback(g *git.Git, townRoot, rigName, branch, commit string) error {
	verifyErr := g.VerifyPushedCommit("origin", branch, commit)
	if verifyErr == nil {
		return nil
	}

	bareRepoPath := filepath.Join(townRoot, rigName, ".repo.git")
	if _, statErr := os.Stat(bareRepoPath); statErr != nil {
		return verifyErr
	}
	bareGit := git.NewGitWithDir(bareRepoPath, "")
	tip, tipErr := bareGit.Rev("refs/heads/" + branch)
	if tipErr == nil && strings.TrimSpace(tip) == strings.TrimSpace(commit) {
		return nil
	}
	return verifyErr
}

// shouldNudgeRefinery reports whether a gt done invocation may wake the
// refinery. Only COMPLETED exits create an MR bead; DEFERRED and ESCALATED
// exits (polecats finishing operational tasks with no code changes) must
// never emit MQ_SUBMIT, or the refinery wakes from backoff to find an empty
// merge queue (gh#3885). The exitType check is defensive: it holds the
// invariant even if a future code path populates mrID outside COMPLETED.
func shouldNudgeRefinery(exitType, mrID string) bool {
	return exitType == ExitCompleted && mrID != ""
}

// setDoneIntentLabel writes a done-intent:<type>:<unix-ts> label on the agent bead
// EARLY in gt done, before push/MR. This allows the Witness to detect polecats that
// crashed mid-gt-done: if the session is dead but done-intent exists, the polecat was
// trying to exit and should be auto-nuked.
//
// Follows the existing idle:N / backoff-until:TIMESTAMP label pattern.
// Non-fatal: if this fails, gt done continues without the safety net.
func setDoneIntentLabel(bd *beads.Beads, agentBeadID, exitType string) {
	if agentBeadID == "" {
		return
	}
	label := doneIntentLabel(exitType, time.Now())
	if err := bd.Update(agentBeadID, beads.UpdateOptions{
		AddLabels: []string{label},
	}); err != nil {
		// Non-fatal: warn but continue
		fmt.Fprintf(os.Stderr, "Warning: couldn't set done-intent label on %s: %v\n", agentBeadID, err)
	}
}

// clearDoneIntentLabel removes any done-intent:* label from the agent bead.
// Called at the end of updateAgentStateOnDone on clean exit.
// Uses read-modify-write pattern (same as clearAgentBackoffUntil).
func clearDoneIntentLabel(bd *beads.Beads, agentBeadID string) {
	if agentBeadID == "" {
		return
	}
	issue, err := bd.Show(agentBeadID)
	if err != nil {
		return // Agent bead gone, nothing to clear
	}

	toRemove := labelsWithPrefix(issue.Labels, "done-intent:")
	if len(toRemove) == 0 {
		return // No done-intent label to clear
	}

	if err := bd.Update(agentBeadID, beads.UpdateOptions{
		RemoveLabels: toRemove,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't clear done-intent label on %s: %v\n", agentBeadID, err)
	}
}

func doneIntentLabel(exitType string, now time.Time) string {
	return fmt.Sprintf("done-intent:%s:%d", exitType, now.Unix())
}

// DoneCheckpoint represents a checkpoint stage in the gt done flow (gt-aufru).
// Checkpoints are stored as labels on the agent bead, enabling resume after
// process interruption (context exhaustion, SIGTERM, etc.).
type DoneCheckpoint string

const (
	CheckpointPushed          DoneCheckpoint = "pushed"
	CheckpointMRCreated       DoneCheckpoint = "mr-created"
	CheckpointWitnessNotified DoneCheckpoint = "witness-notified"
)

// writeDoneCheckpoint writes a checkpoint label on the agent bead.
// Format: done-cp:<stage>:<value>:<unix-ts>
// Non-fatal: if this fails, gt done continues without the checkpoint.
func writeDoneCheckpoint(bd *beads.Beads, agentBeadID string, cp DoneCheckpoint, value string) {
	if agentBeadID == "" {
		return
	}
	label := doneCheckpointLabel(cp, value, time.Now())
	if err := bd.Update(agentBeadID, beads.UpdateOptions{
		AddLabels: []string{label},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't write checkpoint %s on %s: %v\n", cp, agentBeadID, err)
	}
}

// readDoneCheckpoints reads all done-cp:* labels from the agent bead.
// Returns a map of checkpoint stage -> value. Empty map if none found.
func readDoneCheckpoints(bd *beads.Beads, agentBeadID string) map[DoneCheckpoint]string {
	checkpoints := make(map[DoneCheckpoint]string)
	if agentBeadID == "" {
		return checkpoints
	}
	issue, err := bd.Show(agentBeadID)
	if err != nil {
		return checkpoints
	}
	return parseDoneCheckpointLabels(issue.Labels)
}

// clearDoneCheckpoints removes all done-cp:* labels from the agent bead.
// Called on clean exit to prevent stale checkpoints from interfering with future runs.
func clearDoneCheckpoints(bd *beads.Beads, agentBeadID string) {
	if agentBeadID == "" {
		return
	}
	issue, err := bd.Show(agentBeadID)
	if err != nil {
		return
	}
	toRemove := labelsWithPrefix(issue.Labels, "done-cp:")
	if len(toRemove) == 0 {
		return
	}
	if err := bd.Update(agentBeadID, beads.UpdateOptions{
		RemoveLabels: toRemove,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't clear done checkpoints on %s: %v\n", agentBeadID, err)
	}
}

func doneCheckpointLabel(cp DoneCheckpoint, value string, now time.Time) string {
	return fmt.Sprintf("done-cp:%s:%s:%d", cp, value, now.Unix())
}

func pushCheckpointMatchesBranch(checkpoints map[DoneCheckpoint]string, branch string) bool {
	return checkpoints[CheckpointPushed] != "" && checkpoints[CheckpointPushed] == branch
}

func parseDoneCheckpointLabels(labels []string) map[DoneCheckpoint]string {
	checkpoints := make(map[DoneCheckpoint]string)
	for _, label := range labelsWithPrefix(labels, "done-cp:") {
		parts := strings.SplitN(label, ":", 4)
		if len(parts) >= 3 {
			checkpoints[DoneCheckpoint(parts[1])] = parts[2]
		}
	}
	return checkpoints
}

func labelsWithPrefix(labels []string, prefix string) []string {
	var matches []string
	for _, label := range labels {
		if strings.HasPrefix(label, prefix) {
			matches = append(matches, label)
		}
	}
	return matches
}

// updateAgentStateOnDone closes the hooked work bead and reports cleanup status.
// Uses issueID directly to find the hooked bead instead of reading the agent bead's
// hook_bead slot (hq-l6mm5: direct bead tracking).
//
// Clean completions use "done" to prevent dead completed sessions from
// re-entering the idle reuse pool before witness/refinery cleanup finishes.
// Escalated/deferred exits use "stuck" because they need recovery.
//
// Also self-reports cleanup_status for ZFC compliance (#10).
//
// BUG FIX (hq-3xaxy): This function must be resilient to working directory deletion.
// If the polecat's worktree is deleted before gt done finishes, we use env vars as fallback.
// All errors are warnings, not failures - gt done must complete even if bead ops fail.
func isFormulaCompletionBead(id string, issue *beads.Issue) bool {
	return strings.Contains(id, "-wfs-") || issue != nil && beads.HasLabel(issue, formulaDispatchLabel)
}

func shouldCloseHookedWorkOnDone(exitType, hookedBeadID, beadsPath string, bd *beads.Beads) bool {
	if hookedBeadID == "" {
		return false
	}
	if exitType != ExitDeferred {
		return true
	}
	if isFormulaCompletionBead(hookedBeadID, nil) {
		return true
	}
	hookBd, _, _ := routedIssueBeads(beadsPath, hookedBeadID)
	if hookBd == nil {
		hookBd = bd
	}
	hookedBead, err := hookBd.Show(hookedBeadID)
	if err != nil {
		style.PrintWarning("could not classify deferred work %s before completion: %v", hookedBeadID, err)
		return false
	}
	return isFormulaCompletionBead(hookedBeadID, hookedBead)
}

func closeHookedWorkOnDone(hookBd *beads.Beads, hookedBeadID, townRoot, rig string) error {
	hookedBead, err := hookBd.Show(hookedBeadID)
	if err != nil || !isClosableHookedBead(hookedBead.Status) {
		return nil
	}
	skip, err := skipCloseHookedWork(hookBd, hookedBead, hookedBeadID, townRoot, rig)
	if skip {
		return err
	}
	if !closeHookedMoleculeIfAttached(hookBd, hookedBead) {
		return nil
	}
	return closeHookedBeadIfReady(hookBd, hookedBead, hookedBeadID)
}

func skipCloseHookedWork(hookBd *beads.Beads, hookedBead *beads.Issue, hookedBeadID, townRoot, rig string) (bool, error) {
	if beads.HasLabel(hookedBead, "gt:rig") {
		fmt.Fprintf(os.Stderr, "Note: hooked bead %s is a rig identity bead (gt:rig) — skipping close\n", hookedBeadID)
		return true, nil
	}
	if sourceUsesMergeQueue(hookedBead) {
		reason := fmt.Sprintf("source issue %s is waiting for Refinery merge proof", hookedBeadID)
		noteHookedCloseSkipped(townRoot, rig, hookedBeadID, reason)
		return true, nil
	}
	skipReason, fatal := doneSourceCloseSkipReason(hookBd, hookedBeadID, hookedBead)
	if skipReason == "" {
		return false, nil
	}
	noteHookedCloseSkipped(townRoot, rig, hookedBeadID, skipReason)
	if fatal {
		return true, fmt.Errorf("cannot complete hooked work: %s", skipReason)
	}
	return true, nil
}

func noteHookedCloseSkipped(townRoot, rig, hookedBeadID, reason string) {
	style.PrintWarning("%s", reason)
	fmt.Fprintf(os.Stderr, "  The bead will remain open for witness/mayor review.\n")
	notifyDoneCloseSkipped(townRoot, rig, detectSender(), hookedBeadID, reason)
}

func closeHookedMoleculeIfAttached(hookBd *beads.Beads, hookedBead *beads.Issue) bool {
	attachment := beads.ParseAttachmentFields(hookedBead)
	if attachment == nil || attachment.AttachedMolecule == "" {
		return true
	}
	if n := closeDescendants(hookBd, attachment.AttachedMolecule); n > 0 {
		fmt.Fprintf(os.Stderr, "Closed %d molecule step(s) for %s\n", n, attachment.AttachedMolecule)
	}
	if closeErr := hookBd.ForceCloseWithReason("done", attachment.AttachedMolecule); closeErr != nil && !errors.Is(closeErr, beads.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "Warning: couldn't close attached molecule %s: %v\n", attachment.AttachedMolecule, closeErr)
		return false
	}
	return true
}

func closeHookedBeadIfReady(hookBd *beads.Beads, hookedBead *beads.Issue, hookedBeadID string) error {
	if unchecked := beads.HasUncheckedCriteria(hookedBead); unchecked > 0 {
		style.PrintWarning("hooked bead %s has %d unchecked acceptance criteria — skipping close", hookedBeadID, unchecked)
		fmt.Fprintf(os.Stderr, "  The bead will remain open for witness/mayor review.\n")
		return nil
	}
	if err := hookBd.Close(hookedBeadID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't close hooked bead %s: %v\n", hookedBeadID, err)
	}
	return nil
}

func updateAgentStateOnDone(cwd, townRoot, exitType, issueID string) error {
	roleInfo, ok := resolveDoneRoleInfo(cwd, townRoot)
	if !ok {
		return nil
	}
	ctx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	agentBeadID := getAgentBeadID(ctx)
	if agentBeadID == "" {
		style.PrintWarning("no agent bead ID found for %s/%s, skipping agent state update", ctx.Rig, ctx.Polecat)
		return nil
	}
	return applyDoneAgentState(townRoot, exitType, issueID, roleInfo, ctx, agentBeadID)
}

func resolveDoneRoleInfo(cwd, townRoot string) (RoleInfo, bool) {
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err == nil {
		return roleInfo, true
	}
	envRole := os.Getenv("GT_ROLE")
	envRig := os.Getenv("GT_RIG")
	envPolecat := os.Getenv("GT_POLECAT")
	if envRole == "" || envRig == "" {
		style.PrintWarning("could not determine role for agent state update (env: GT_ROLE=%q, GT_RIG=%q)", envRole, envRig)
		return RoleInfo{}, false
	}
	parsedRole, _, _ := parseRoleString(envRole)
	return RoleInfo{
		Role:     parsedRole,
		Rig:      envRig,
		Polecat:  envPolecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
		Source:   "env-fallback",
	}, true
}

func applyDoneAgentState(townRoot, exitType, issueID string, roleInfo RoleInfo, ctx RoleContext, agentBeadID string) error {
	// Use the rig directory so bd commands work if the polecat worktree is gone.
	beadsPath := doneBeadsPathForRole(ctx, townRoot)
	bd := beads.New(beadsPath)
	// agentBd bypasses prefix routing — agent beads (gt:agent label) live in
	// the town DB regardless of their ID prefix, but the rig-prefix routing
	// would otherwise misroute them to the rig DB and silently fail with
	// "issue not found". See beads.ForAgentBead docstring for details.
	agentBd := bd.ForAgentBead()

	// Find the hooked bead to close. Use issueID directly instead of reading
	// agent bead's hook_bead slot (hq-l6mm5: direct bead tracking).
	hookedBeadID := issueID
	if hookedBeadID == "" {
		// Fallback: query for hooked beads assigned to this agent
		agentID := roleInfo.ActorString()
		if found := findHookedBeadForAgent(bd, agentID); found != "" {
			hookedBeadID = found
		}
	}

	if err := closeHookedWorkIfNeeded(exitType, hookedBeadID, beadsPath, bd, townRoot, ctx.Rig); err != nil {
		return err
	}
	finalizeDoneAgentBead(agentBd, bd, agentBeadID, exitType)
	return nil
}

func doneBeadsPathForRole(ctx RoleContext, townRoot string) string {
	switch ctx.Role {
	case RoleMayor, RoleDeacon:
		return townRoot
	default:
		return filepath.Join(townRoot, ctx.Rig)
	}
}

func closeHookedWorkIfNeeded(exitType, hookedBeadID, beadsPath string, bd *beads.Beads, townRoot, rig string) error {
	if !shouldCloseHookedWorkOnDone(exitType, hookedBeadID, beadsPath, bd) {
		return nil
	}
	hookBd, _, _ := routedIssueBeads(beadsPath, hookedBeadID)
	if hookBd == nil {
		hookBd = bd
	}
	return closeHookedWorkOnDone(hookBd, hookedBeadID, townRoot, rig)
}

func finalizeDoneAgentBead(agentBd, bd *beads.Beads, agentBeadID, exitType string) {
	emptyHook := ""
	if err := agentBd.UpdateAgentDescriptionFields(agentBeadID, beads.AgentFieldUpdates{HookBead: &emptyHook}); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't clear hook_bead on %s: %v\n", agentBeadID, err)
	}
	purgeClosedEphemeralBeads(bd)
	agentState := string(beads.AgentStateDone)
	if exitType != ExitCompleted {
		agentState = "stuck"
	}
	if err := agentBd.UpdateAgentState(agentBeadID, agentState); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't set agent %s to %s: %v\n", agentBeadID, agentState, err)
	}
	updateDoneAgentCleanupStatus(agentBd, agentBeadID)
	clearDoneIntentLabel(agentBd, agentBeadID)
	clearDoneCheckpoints(agentBd, agentBeadID)
}

func updateDoneAgentCleanupStatus(agentBd *beads.Beads, agentBeadID string) {
	if doneState().cleanupStatus == "" {
		return
	}
	cleanupStatus := parseCleanupStatus(doneState().cleanupStatus)
	if cleanupStatus == polecat.CleanupUnknown {
		return
	}
	if err := agentBd.UpdateAgentCleanupStatus(agentBeadID, string(cleanupStatus)); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: couldn't update agent %s cleanup status: %v\n", agentBeadID, err)
	}
}

func isClosableHookedBead(status string) bool {
	return !beads.IssueStatus(status).IsTerminal()
}

// ensureAgentBeadExists recreates a missing agent bead so done-intent labels,
// checkpoints, and active_mr writes don't silently fail (hq-xu4p). Only
// rig-level agents are handled — town agents (mayor/deacon) are owned by
// gt doctor. Best-effort: failures are warned, never fatal.
func ensureAgentBeadExists(bd *beads.Beads, id string, ctx RoleContext) {
	if id == "" {
		return
	}
	if issue, err := bd.Show(id); err == nil && issue != nil && issue.Status != string(beads.StatusClosed) {
		return // exists and is active
	}

	fields := &beads.AgentFields{Rig: ctx.Rig, AgentState: "idle"}
	var title string
	switch ctx.Role {
	case RolePolecat:
		fields.RoleType = "polecat"
		title = fmt.Sprintf("Polecat worker %s in %s - autonomous worker with persistent identity.", ctx.Polecat, ctx.Rig)
	case RoleWitness:
		fields.RoleType = "witness"
		title = fmt.Sprintf("Witness for %s - monitors polecat health and progress.", ctx.Rig)
	case RoleRefinery:
		fields.RoleType = "refinery"
		title = fmt.Sprintf("Refinery for %s - processes merge queue.", ctx.Rig)
	default:
		return
	}

	if _, err := bd.CreateOrReopenAgentBead(id, title, fields); err != nil {
		style.PrintWarning("agent bead %s missing and recreate failed: %v", id, err)
	} else {
		fmt.Printf("%s Recreated/reopened missing agent bead: %s\n", style.Bold.Render("✓"), id)
	}
}

// isStaleBranchIssue reports whether a branch-derived issue id should be
// overridden by the agent's hooked bead (hq-l0fj stale-branch guard).
// True when both ids exist, they differ, and the branch id is not a subtask
// of the hooked bead (e.g. branch gt-abc.1 under hooked gt-abc is fine).
func isStaleBranchIssue(branchIssue, hookedIssue string) bool {
	if branchIssue == "" || hookedIssue == "" {
		return false
	}
	return branchIssue != hookedIssue && !strings.HasPrefix(branchIssue, hookedIssue+".")
}

// selectAssignedIssue returns the one authoritative assignment to use for
// done attribution. Ambiguous assignment state is deliberately not guessed.
func selectAssignedIssue(branchIssue string, assigned []string) (string, bool) {
	ids := uniqueAssignedIssueIDs(assigned)
	if len(ids) == 0 {
		return "", false
	}
	if branchIssueMatchesAssigned(branchIssue, ids) {
		return "", false
	}
	if len(ids) > 1 {
		return "", true
	}
	return ids[0], false
}

func uniqueAssignedIssueIDs(assigned []string) []string {
	unique := make(map[string]bool, len(assigned))
	for _, id := range assigned {
		if id != "" {
			unique[id] = true
		}
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func branchIssueMatchesAssigned(branchIssue string, ids []string) bool {
	if branchIssue == "" {
		return false
	}
	for _, id := range ids {
		if branchIssue == id || strings.HasPrefix(branchIssue, id+".") {
			return true
		}
	}
	return false
}

// findAssignedBeadsForAgent queries the same assignment locations as gt hook:
// the current rig, the target rig for rig agents, then town beads. The assigned
// work bead is authoritative; agent-bead hook slots are intentionally ignored.
func findAssignedBeadsForAgent(workDir, agentID string) []string {
	if agentID == "" {
		return nil
	}
	assigned := assignedIssueIDs(queryAssignedBeads(beads.New(workDir), agentID))
	if len(assigned) > 0 {
		return assigned
	}
	return findAssignedBeadsOutsideWorkDir(workDir, agentID)
}

func findAssignedBeadsOutsideWorkDir(workDir, agentID string) []string {
	townRoot, err := findTownRoot()
	if err != nil || townRoot == "" {
		return nil
	}
	if assigned := findAssignedBeadsInAgentRig(townRoot, workDir, agentID); len(assigned) > 0 {
		return assigned
	}
	if assigned := findAssignedBeadsInTown(townRoot, agentID); len(assigned) > 0 {
		return assigned
	}
	if isTownLevelRole(agentID) {
		return assignedIssueIDs(scanAllRigsForHookedBeads(townRoot, agentID))
	}
	return nil
}

func findAssignedBeadsInAgentRig(townRoot, workDir, agentID string) []string {
	parts := strings.Split(agentID, "/")
	if len(parts) == 0 {
		return nil
	}
	rigName := parts[0]
	if rigName == "" || rigName == "mayor" || rigName == "deacon" {
		return nil
	}
	rigWorkDir := filepath.Join(townRoot, rigName, "mayor", "rig")
	if rigWorkDir == workDir {
		return nil
	}
	return assignedIssueIDs(queryAssignedBeads(beads.New(rigWorkDir), agentID))
}

func findAssignedBeadsInTown(townRoot, agentID string) []string {
	townBeadsDir := filepath.Join(townRoot, ".beads")
	if _, err := os.Stat(townBeadsDir); err != nil {
		return nil
	}
	return assignedIssueIDs(queryAssignedBeads(beads.New(townBeadsDir), agentID))
}

func queryAssignedBeads(bd *beads.Beads, agentID string) []*beads.Issue {
	hooked, err := bd.List(beads.ListOptions{
		Status:   beads.StatusHooked,
		Assignee: agentID,
		Priority: -1,
	})
	if err == nil && len(hooked) > 0 {
		return hooked
	}
	inProgress, err := bd.List(beads.ListOptions{
		Status:   "in_progress",
		Assignee: agentID,
		Priority: -1,
	})
	if err == nil {
		return inProgress
	}
	return nil
}

func assignedIssueIDs(assigned []*beads.Issue) []string {
	ids := make([]string, 0, len(assigned))
	for _, issue := range assigned {
		if issue != nil && issue.ID != "" {
			ids = append(ids, issue.ID)
		}
	}
	return ids
}

// findHookedBeadForAgent queries for the agent's current assignment bead.
// This is the authoritative source for what work a polecat is doing, since the
// work bead itself tracks status and assignee (hq-l6mm5).
//
// Both hooked AND in_progress are checked (hq-xa4z): polecats routinely claim
// their assignment with `bd update --status=in_progress` when starting work,
// which made a hooked-only lookup blind to the active assignment — the stale-
// branch guard and the hook fallback silently no-op'd (same class of bug as
// gt-pftz in the close path). Hooked wins over in_progress when both exist.
// Returns empty string if no assignment bead is found.
func findHookedBeadForAgent(bd *beads.Beads, agentID string) string {
	issueID, _ := selectAssignedIssue("", assignedIssueIDs(queryAssignedBeads(bd, agentID)))
	return issueID
}

// parseCleanupStatus converts a string flag value to a CleanupStatus.
// ZFC: Agent observes git state and passes the appropriate status.
func parseCleanupStatus(s string) polecat.CleanupStatus {
	switch strings.ToLower(s) {
	case "clean":
		return polecat.CleanupClean
	case "uncommitted", "has_uncommitted":
		return polecat.CleanupUncommitted
	case "stash", "has_stash":
		return polecat.CleanupStash
	case "unpushed", "has_unpushed":
		return polecat.CleanupUnpushed
	default:
		return polecat.CleanupUnknown
	}
}

// isPolecatActor checks if a BD_ACTOR value represents a polecat.
// Polecat actors have format: rigname/polecats/polecatname
// Non-polecat actors have formats like: gastown/crew/name, rigname/witness, etc.
func isPolecatActor(actor string) bool {
	parts := strings.Split(strings.TrimSpace(actor), "/")
	return len(parts) == 3 && parts[0] != "" && parts[1] == "polecats" && parts[2] != ""
}

// stripOverlayInstructionFiles removes Gas Town overlay files from the branch
// before push. The overlay pair may be CLAUDE.md/AGENTS.md or the local pair.
func stripOverlayInstructionFiles(g *git.Git, defaultBranch, baseRef string) bool {
	changedFiles, err := g.DiffNameOnly(baseRef, "HEAD")
	if err != nil {
		return false
	}

	changed := map[string]bool{}
	for _, f := range changedFiles {
		changed[f] = true
	}

	needsCommit := false
	needsCommit = stripOverlayCanonical(g, defaultBranch, baseRef, "CLAUDE.md", changed["CLAUDE.md"]) || needsCommit
	needsCommit = stripOverlayCanonical(g, defaultBranch, baseRef, "AGENTS.md", changed["AGENTS.md"]) || needsCommit
	needsCommit = stripOverlayLocal(g, "CLAUDE.local.md", changed["CLAUDE.local.md"], false) || needsCommit
	needsCommit = stripOverlayLocal(g, "AGENTS.local.md", changed["AGENTS.local.md"], true) || needsCommit

	if !needsCommit {
		return false
	}

	if commitErr := g.Commit("chore: strip Gas Town overlay from instruction files"); commitErr != nil {
		style.PrintWarning("failed to create overlay cleanup commit: %v", commitErr)
		return false
	}

	fmt.Printf("%s Created cleanup commit to remove Gas Town overlay files\n",
		style.Bold.Render("✓"))
	return true
}

func stripOverlayCanonical(g *git.Git, defaultBranch, baseRef, name string, changed bool) bool {
	if !changed {
		return false
	}
	currentContent, showErr := g.ShowFile("HEAD", name)
	if showErr != nil || !instructions.IsGasTownOverlay(currentContent) {
		return false
	}
	if _, origErr := g.ShowFile(baseRef, name); origErr != nil {
		if rmErr := g.RmCached(name); rmErr == nil {
			fmt.Printf("%s Removed overlay %s (did not exist on %s)\n",
				style.Bold.Render("→"), name, defaultBranch)
			return true
		}
		return false
	}
	if coErr := g.CheckoutFileFromRef(baseRef, name); coErr == nil {
		if addErr := g.Add(name); addErr == nil {
			fmt.Printf("%s Restored original %s (stripped Gas Town overlay)\n",
				style.Bold.Render("→"), name)
			return true
		}
	}
	return false
}

func stripOverlayLocal(g *git.Git, name string, changed, requireOverlay bool) bool {
	if !changed {
		return false
	}
	if requireOverlay {
		currentContent, showErr := g.ShowFile("HEAD", name)
		if showErr != nil || !instructions.IsGasTownOverlay(currentContent) {
			return false
		}
	}
	if rmErr := g.RmCached(name); rmErr == nil {
		fmt.Printf("%s Removed %s from branch (Gas Town overlay)\n",
			style.Bold.Render("→"), name)
		return true
	}
	return false
}

// purgeClosedEphemeralBeads removes closed ephemeral beads (wisps) that accumulated
// during this and prior sessions. Polecat/witness sessions create mol-polecat-work
// steps, mol-witness-patrol cycles, etc. as wisps. These get closed during normal
// operation but are never deleted, accumulating hundreds of rows that pollute
// bd ready/list output. (hq-6161m)
//
// Best-effort: errors are logged but don't block gt done completion.
func purgeClosedEphemeralBeads(bd *beads.Beads) {
	out, err := bd.Run("purge", "--force", "--quiet")
	if err != nil {
		// Non-fatal: purge failure shouldn't block session completion
		fmt.Fprintf(os.Stderr, "Warning: wisp purge failed: %v\n", err)
		return
	}
	// bd purge --force --quiet outputs the count of purged beads
	outStr := strings.TrimSpace(string(out))
	if outStr != "" && outStr != "0" {
		fmt.Fprintf(os.Stderr, "Purged closed ephemeral beads: %s\n", outStr)
	}
}

func reportWorkerDone(townRoot, rigName, polecatName string) {
	if townRoot == "" || polecatName == "" {
		return
	}
	sessionID := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
	run, err := worker.RunBySession(townRoot, sessionID)
	if err != nil || run == nil {
		return
	}
	client, err := worker.DialAgent(townRoot)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = client.ReportLifecycle(ctx, worker.Lifecycle{
		Event: worker.EventStopping, RunID: run.RunID, SessionID: sessionID, Timestamp: time.Now().UTC(),
	})
	_ = client.ReportLifecycle(ctx, worker.Lifecycle{
		Event:     worker.EventStopped,
		RunID:     run.RunID,
		SessionID: sessionID,
		Timestamp: time.Now().UTC(),
		Metadata:  map[string]any{"exit_code": 0, "done": true},
	})
}
