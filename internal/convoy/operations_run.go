package convoy

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	beadsdk "github.com/jonbaldie/beads"
	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/util"
)

type convoyLogger func(format string, args ...interface{})

type convoyIssueCheck struct {
	ctx         context.Context
	store       beadsdk.Storage
	townRoot    string
	issueID     string
	caller      string
	logger      convoyLogger
	gtPath      string
	isRigParked func(string) bool
	resolver    *StoreResolver
}

func newConvoyIssueCheck(ctx context.Context, store beadsdk.Storage, townRoot, issueID, caller string, logger convoyLogger, gtPath string, isRigParked func(string) bool, resolver []*StoreResolver) *convoyIssueCheck {
	if logger == nil {
		logger = func(format string, args ...interface{}) {}
	}
	if isRigParked == nil {
		isRigParked = func(string) bool { return false }
	}
	c := &convoyIssueCheck{
		ctx:         ctx,
		store:       store,
		townRoot:    townRoot,
		issueID:     issueID,
		caller:      caller,
		logger:      logger,
		gtPath:      gtPath,
		isRigParked: isRigParked,
	}
	if len(resolver) > 0 {
		c.resolver = resolver[0]
	}
	return c
}

func checkOneTrackingConvoy(c *convoyIssueCheck, convoyID string) {
	if isConvoyClosed(c.ctx, c.store, convoyID) {
		c.logger("%s: convoy %s already closed, skipping", c.caller, convoyID)
		return
	}
	if isConvoyStaged(c.ctx, c.store, convoyID) {
		c.logger("%s: convoy %s is staged (not yet launched), skipping", c.caller, convoyID)
		return
	}
	c.logger("%s: checking convoy %s", c.caller, convoyID)
	if err := runConvoyCheck(c.ctx, c.townRoot, convoyID, c.gtPath); err != nil {
		c.logger("%s: convoy %s check failed: %s", c.caller, convoyID, util.FirstLine(err.Error()))
	}
	if !isConvoyClosed(c.ctx, c.store, convoyID) {
		feedNextReadyIssue(c.ctx, c.store, c.townRoot, convoyID, c.caller, c.logger, c.gtPath, c.isRigParked, c.resolver)
	}
}

func loadIssueDeps(ctx context.Context, store beadsdk.Storage, issueID string, resolver *StoreResolver) []*beadsdk.IssueWithDependencyMetadata {
	if store == nil {
		return nil
	}
	if resolver != nil {
		deps := resolver.ResolveDepsWithMetadata(ctx, issueID)
		if len(deps) > 0 {
			return deps
		}
	}
	deps, err := store.GetDependenciesWithMetadata(ctx, issueID)
	if err != nil {
		return nil
	}
	return deps
}

func scanBlockingDeps(deps []*beadsdk.IssueWithDependencyMetadata, hasResolver bool) (ids, types []string, blocked bool) {
	for _, d := range deps {
		depType := string(d.DependencyType)
		if !blockingDepTypes[depType] {
			continue
		}
		status := string(d.Status)
		if status == "tombstone" {
			continue
		}
		if status == "closed" {
			if depType == "merge-blocks" && !strings.HasPrefix(d.CloseReason, "Merged in ") {
				return nil, nil, true
			}
			continue
		}
		if !hasResolver {
			return nil, nil, true
		}
		ids = append(ids, extractIssueID(d.ID))
		types = append(types, depType)
	}
	return ids, types, false
}

func staleBlockersStillOpen(ctx context.Context, resolver *StoreResolver, staleIDs, staleTypes []string) bool {
	if len(staleIDs) == 0 {
		return false
	}
	freshMap := resolver.ResolveIssues(ctx, staleIDs)
	for i, id := range staleIDs {
		if staleIDStillBlocks(freshMap, id, staleTypes[i]) {
			return true
		}
	}
	return false
}

func staleIDStillBlocks(freshMap map[string]*beadsdk.Issue, id, depType string) bool {
	fresh, ok := freshMap[id]
	if !ok {
		return true
	}
	freshStatus := string(fresh.Status)
	if freshStatus == "tombstone" {
		return false
	}
	if freshStatus != "closed" {
		return true
	}
	return depType == "merge-blocks" && !strings.HasPrefix(fresh.CloseReason, "Merged in ")
}

func convoyBaseBranch(ctx context.Context, store beadsdk.Storage, convoyID string) string {
	convoy, err := store.GetIssue(ctx, convoyID)
	if err != nil || convoy == nil {
		return ""
	}
	if cf := beads.ParseConvoyFields(&beads.Issue{Description: convoy.Description}); cf != nil {
		return cf.BaseBranch
	}
	return ""
}

func sortTrackedIssues(tracked []trackedIssue) {
	sort.Slice(tracked, func(i, j int) bool {
		if tracked[i].Priority != tracked[j].Priority {
			return tracked[i].Priority < tracked[j].Priority
		}
		return tracked[i].ID < tracked[j].ID
	})
}

type convoyFeedRun struct {
	ctx         context.Context
	store       beadsdk.Storage
	townRoot    string
	convoyID    string
	caller      string
	logger      convoyLogger
	gtPath      string
	isRigParked func(string) bool
	resolver    *StoreResolver
	baseBranch  string
}

func tryFeedReadyIssues(r *convoyFeedRun, tracked []trackedIssue) bool {
	for _, issue := range tracked {
		if feedReadyIssue(r, issue) {
			return true
		}
	}
	return false
}

func feedReadyIssue(r *convoyFeedRun, issue trackedIssue) bool {
	if issue.Status != "open" || issue.Assignee != "" {
		return false
	}
	if !IsSlingableType(issue.IssueType) {
		r.logger("%s: convoy %s: %s has non-slingable type %q, skipping", r.caller, r.convoyID, issue.ID, issue.IssueType)
		return false
	}
	if isIssueBlocked(r.ctx, r.store, issue.ID, r.resolver) {
		r.logger("%s: convoy %s: %s is blocked, skipping", r.caller, r.convoyID, issue.ID)
		return false
	}
	return dispatchReadyIssue(r, issue)
}

func dispatchReadyIssue(r *convoyFeedRun, issue trackedIssue) bool {
	rig := rigForIssue(r.townRoot, issue.ID)
	if rig == "" {
		r.logger("%s: convoy %s: cannot determine rig for issue %s, skipping", r.caller, r.convoyID, issue.ID)
		return false
	}
	if r.isRigParked(rig) {
		r.logger("%s: convoy %s: rig %s is parked, skipping %s", r.caller, r.convoyID, rig, issue.ID)
		return false
	}
	r.logger("%s: convoy %s: feeding next ready issue %s to %s", r.caller, r.convoyID, issue.ID, rig)
	if err := dispatchIssue(r.ctx, r.townRoot, issue.ID, rig, r.gtPath, r.baseBranch); err != nil {
		r.logger("%s: convoy %s: dispatch %s failed: %s", r.caller, r.convoyID, issue.ID, util.FirstLine(err.Error()))
		return false
	}
	return true
}

type trackedDepMeta struct {
	status    string
	assignee  string
	priority  int
	issueType string
}

func collectTrackedIDs(ctx context.Context, store beadsdk.Storage, convoyID string) ([]string, map[string]trackedDepMeta) {
	deps, err := store.GetDependenciesWithMetadata(ctx, convoyID)
	if err != nil || len(deps) == 0 {
		return nil, nil
	}
	var ids []string
	metaByID := make(map[string]trackedDepMeta)
	for _, d := range deps {
		if string(d.DependencyType) != "tracks" {
			continue
		}
		id := extractIssueID(d.ID)
		ids = append(ids, id)
		metaByID[id] = trackedDepMeta{
			status:    string(d.Status),
			assignee:  d.Assignee,
			priority:  d.Priority,
			issueType: string(d.IssueType),
		}
	}
	return ids, metaByID
}

func refreshTrackedIssueMap(ctx context.Context, store beadsdk.Storage, townRoot string, ids []string, resolver *StoreResolver) map[string]*beadsdk.Issue {
	freshIssues, err := store.GetIssuesByIDs(ctx, ids)
	if err != nil {
		freshIssues = nil
	}
	freshMap := make(map[string]*beadsdk.Issue)
	for _, iss := range freshIssues {
		if iss != nil {
			freshMap[iss.ID] = iss
		}
	}
	fillMissingTrackedIssues(ctx, townRoot, ids, freshMap, resolver)
	return freshMap
}

func fillMissingTrackedIssues(ctx context.Context, townRoot string, ids []string, freshMap map[string]*beadsdk.Issue, resolver *StoreResolver) {
	var missingIDs []string
	for _, id := range ids {
		if _, ok := freshMap[id]; !ok {
			missingIDs = append(missingIDs, id)
		}
	}
	if len(missingIDs) == 0 {
		return
	}
	if resolver != nil {
		for id, fresh := range resolver.ResolveIssues(ctx, missingIDs) {
			freshMap[id] = fresh
		}
		return
	}
	if townRoot == "" {
		return
	}
	for id, fresh := range fetchCrossRigBeadStatus(townRoot, missingIDs) {
		freshMap[id] = fresh
	}
}

func trackedIssuesFromFreshOrMeta(ids []string, freshMap map[string]*beadsdk.Issue, metaByID map[string]trackedDepMeta) []trackedIssue {
	result := make([]trackedIssue, 0, len(ids))
	for _, id := range ids {
		t := trackedIssue{ID: id}
		if fresh := freshMap[id]; fresh != nil {
			t.Status = string(fresh.Status)
			t.Assignee = fresh.Assignee
			t.Priority = fresh.Priority
			t.IssueType = string(fresh.IssueType)
		} else if meta, ok := metaByID[id]; ok {
			t.Status = meta.status
			t.Assignee = meta.assignee
			t.Priority = meta.priority
			t.IssueType = meta.issueType
		}
		result = append(result, t)
	}
	return result
}

func closedIssueStoreKey(townRoot, closedPrefix string) (closedRig, closedStoreKey string) {
	closedRig = beads.GetRigNameForPrefix(townRoot, closedPrefix)
	closedStoreKey = closedRig
	if closedStoreKey == "" {
		closedStoreKey = "hq"
	}
	return closedRig, closedStoreKey
}

type crossRigNotify struct {
	ctx            context.Context
	townRoot       string
	closedIssueID  string
	closedRig      string
	closedStoreKey string
	externalID     string
	logger         convoyLogger
	notifiedRigs   map[string]bool
}

func notifyStoreCrossRigDeps(n *crossRigNotify, storeName string, store beadsdk.Storage) {
	if storeName == n.closedStoreKey {
		return
	}
	dependents, err := store.GetDependentsWithMetadata(n.ctx, n.externalID)
	if err != nil || len(dependents) == 0 {
		return
	}
	for _, dep := range dependents {
		maybeNudgeCrossRigWitness(n, dep)
	}
}

func maybeNudgeCrossRigWitness(n *crossRigNotify, dep *beadsdk.IssueWithDependencyMetadata) {
	if dep == nil || !blockingDepTypes[string(dep.DependencyType)] {
		return
	}
	depID := extractIssueID(dep.ID)
	depPrefix := beads.ExtractPrefix(depID)
	if depPrefix == "" {
		return
	}
	depRig := beads.GetRigNameForPrefix(n.townRoot, depPrefix)
	if depRig == "" || depRig == n.closedRig || n.notifiedRigs[depRig] {
		return
	}
	n.notifiedRigs[depRig] = true
	n.logger("CrossRig: %s closed, unblocking %s (%s) — nudging %s/witness", n.closedIssueID, depID, depRig, depRig)
	runCrossRigNudge(n.townRoot, n.closedIssueID, depID, dep.Title, depRig, n.logger)
}

func runCrossRigNudge(townRoot, closedIssueID, depID, depTitle, depRig string, logger convoyLogger) {
	msg := fmt.Sprintf("Dependency resolved: %s — External dependency %s has closed. Unblocked: %s (%s). This issue may now proceed.",
		closedIssueID, closedIssueID, depID, depTitle)
	nudgeCmd := exec.Command("gt", "nudge", depRig+"/witness", "-m", msg)
	nudgeCmd.Dir = townRoot
	if err := nudgeCmd.Run(); err != nil {
		logger("CrossRig: nudge %s/witness failed: %v", depRig, err)
	}
}
