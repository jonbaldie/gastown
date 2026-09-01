package refinery

import (
	"fmt"
	"io"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
)

type mergedWorkBeadCloseRequest struct {
	MRID        string
	Branch      string
	Target      string
	SourceIssue string
	AgentBead   string
	MergeCommit string
}

type mergedWorkBeadCloseResult struct {
	WorkBeadID string
	Closed     bool
	NotFound   bool
}

type workBeadCloser interface {
	Show(_ string) (*beads.Issue, error)
	ForceCloseWithReason(_ string, _ ...string) error
}

type issueReader interface {
	Show(_ string) (*beads.Issue, error)
}

type mergedWorkBeadLog func(format string, args ...interface{})

func newMergedWorkBeadLog(out io.Writer) mergedWorkBeadLog {
	return func(format string, args ...interface{}) {
		if out != nil {
			_, _ = fmt.Fprintf(out, format, args...)
		}
	}
}

func closeMergedWorkBead(work workBeadCloser, agent issueReader, out io.Writer, req mergedWorkBeadCloseRequest) mergedWorkBeadCloseResult {
	logf := newMergedWorkBeadLog(out)
	workBeadID := resolveMergedWorkBead(agent, req)
	result := mergedWorkBeadCloseResult{WorkBeadID: workBeadID}
	if workBeadID == "" {
		logf("[Refinery] Note: merged MR %s has no resolvable work bead to close\n", req.MRID)
		result.NotFound = true
		return result
	}
	if work == nil {
		logf("[Refinery] Warning: no beads client available to close work bead %s\n", workBeadID)
		result.NotFound = true
		return result
	}
	return closeResolvedWorkBead(work, logf, req, result)
}

func closeResolvedWorkBead(work workBeadCloser, logf mergedWorkBeadLog, req mergedWorkBeadCloseRequest, result mergedWorkBeadCloseResult) mergedWorkBeadCloseResult {
	issue, err := work.Show(result.WorkBeadID)
	if err != nil || issue == nil {
		logf("[Refinery] Warning: failed to fetch work bead %s: %v\n", result.WorkBeadID, err)
		result.NotFound = true
		return result
	}
	if skip := inspectWorkBeadForClose(issue, result.WorkBeadID, logf); skip != nil {
		return *skip
	}
	return forceCloseMergedWorkBead(work, logf, req, result)
}

func inspectWorkBeadForClose(issue *beads.Issue, workBeadID string, logf mergedWorkBeadLog) *mergedWorkBeadCloseResult {
	if reason := beads.ConcreteWorkIssueRejectReason(issue); reason != "" {
		logf("[Refinery] Warning: refusing to close non-concrete work bead %s (%s)\n", workBeadID, reason)
		return &mergedWorkBeadCloseResult{WorkBeadID: workBeadID, NotFound: true}
	}
	if beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal() {
		logf("[Refinery] Work bead already closed: %s\n", workBeadID)
		return &mergedWorkBeadCloseResult{WorkBeadID: workBeadID, Closed: true}
	}
	if reason := refineryMergedWorkBeadCloseBlockReason(issue); reason != "" {
		logf("[Refinery] Warning: refusing to close non-mergeable work bead %s (%s)\n", workBeadID, reason)
		return &mergedWorkBeadCloseResult{WorkBeadID: workBeadID, NotFound: true}
	}
	return nil
}

func forceCloseMergedWorkBead(work workBeadCloser, logf mergedWorkBeadLog, req mergedWorkBeadCloseRequest, result mergedWorkBeadCloseResult) mergedWorkBeadCloseResult {
	if err := work.ForceCloseWithReason(mergedWorkBeadCloseReason(req), result.WorkBeadID); err != nil {
		return handleMergedWorkBeadCloseError(work, logf, result, err)
	}
	logf("[Refinery] Closed work bead: %s\n", result.WorkBeadID)
	result.Closed = true
	return result
}

func mergedWorkBeadCloseReason(req mergedWorkBeadCloseRequest) string {
	closeReason := fmt.Sprintf("Merged in %s", req.MRID)
	if req.MergeCommit != "" {
		return fmt.Sprintf("%s\ntarget_branch: %s\ncommit_sha: %s", closeReason, req.Target, req.MergeCommit)
	}
	return closeReason
}

func handleMergedWorkBeadCloseError(work workBeadCloser, logf mergedWorkBeadLog, result mergedWorkBeadCloseResult, err error) mergedWorkBeadCloseResult {
	if alreadyClosedMergedWorkBead(work, result.WorkBeadID) {
		logf("[Refinery] Work bead already closed: %s\n", result.WorkBeadID)
		result.Closed = true
		return result
	}
	logf("[Refinery] Warning: failed to close work bead %s: %v\n", result.WorkBeadID, err)
	result.NotFound = true
	return result
}

func alreadyClosedMergedWorkBead(work workBeadCloser, workBeadID string) bool {
	issue, err := work.Show(workBeadID)
	if err != nil || issue == nil {
		return false
	}
	return beads.ConcreteWorkIssueRejectReason(issue) == "" &&
		beads.IssueStatus(strings.TrimSpace(issue.Status)).IsTerminal()
}

func resolveMergedWorkBead(agent issueReader, req mergedWorkBeadCloseRequest) string {
	if sourceIssue := cleanWorkBeadID(req.SourceIssue); sourceIssue != "" {
		return sourceIssue
	}
	if !canResolveWorkBeadFromAgent(agent, req) {
		return ""
	}
	return workBeadFromMatchingAgent(agent, req)
}

func canResolveWorkBeadFromAgent(agent issueReader, req mergedWorkBeadCloseRequest) bool {
	return agent != nil && cleanWorkBeadID(req.AgentBead) != "" && cleanWorkBeadID(req.MRID) != ""
}

func workBeadFromMatchingAgent(agent issueReader, req mergedWorkBeadCloseRequest) string {
	agentIssue, err := agent.Show(req.AgentBead)
	if err != nil || !beads.IsAgentBead(agentIssue) {
		return ""
	}
	fields := beads.ParseAgentFields(agentIssue.Description)
	if fields == nil || !agentFieldsMatchMergedMR(fields, req) {
		return ""
	}
	return cleanWorkBeadID(fields.LastSourceIssue)
}

func agentFieldsMatchMergedMR(fields *beads.AgentFields, req mergedWorkBeadCloseRequest) bool {
	if fields.ActiveMR != req.MRID && fields.MRID != req.MRID {
		return false
	}
	agentBranch := strings.TrimSpace(fields.Branch)
	requestBranch := strings.TrimSpace(req.Branch)
	if agentBranch == "" || strings.EqualFold(agentBranch, "null") {
		return false
	}
	return requestBranch != "" && !strings.EqualFold(requestBranch, "null") && agentBranch == requestBranch
}

func cleanWorkBeadID(id string) string {
	id = strings.TrimSpace(id)
	if strings.EqualFold(id, "null") {
		return ""
	}
	return id
}

func refineryMergedWorkBeadCloseBlockReason(issue *beads.Issue) string {
	if fields := beads.ParseAttachmentFields(issue); fields != nil {
		switch {
		case fields.NoMerge:
			return "no_merge"
		case fields.ReviewOnly:
			return "review_only"
		case strings.EqualFold(strings.TrimSpace(fields.MergeStrategy), "local"):
			return "merge_strategy:local"
		}
	}
	return ""
}
