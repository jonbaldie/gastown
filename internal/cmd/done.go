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
	exitType, err := validateDoneRequest()
	if err != nil {
		return err
	}
	flow, err := setupDoneFlow(exitType)
	if err != nil {
		return err
	}
	if exitType != ExitCompleted {
		printDoneNonCompleted(exitType, flow.work.issueID, flow.repo.branch)
		return finishDone(flow)
	}
	stage, err := runDoneCompleted(flow)
	if err != nil {
		return err
	}
	if stage == doneCompletedAfterPush {
		return runDoneAfterPushThenFinish(flow)
	}
	return finishDone(flow)
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
