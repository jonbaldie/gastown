// Package convoy provides convoy tracking operations: finding tracking convoys,
// checking completion, feeding ready issues, and dispatching via gt sling.
package convoy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	beadsdk "github.com/jonbaldie/beads"
	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/util"
)

// CheckConvoysForIssue finds any convoys tracking the given issue and triggers
// convoy completion checks. If the convoy is not complete, it reactively feeds
// the next ready issue to keep the convoy progressing without waiting for
// polling-based patrol cycles.
//
// The check is idempotent - running it multiple times for the same issue is safe.
// The underlying `gt convoy check` handles already-closed convoys gracefully.
//
// Parameters:
//   - ctx: context for storage operations
//   - store: beads storage for dependency/issue queries (nil skips convoy checks)
//   - townRoot: path to the town root directory
//   - issueID: the issue ID that was just closed
//   - caller: identifier for logging (e.g., "Convoy")
//   - logger: optional logger function (can be nil)
//   - gtPath: resolved path to the gt binary (e.g. from exec.LookPath or daemon config)
//   - resolver: optional StoreResolver for cross-database issue resolution (nil falls back to subprocess)
//
// Returns the convoy IDs that were checked (may be empty if issue isn't tracked).
func CheckConvoysForIssue(ctx context.Context, store beadsdk.Storage, townRoot, issueID, caller string, logger func(format string, args ...interface{}), gtPath string, isRigParked func(string) bool, resolver ...*StoreResolver) []string {
	if store == nil {
		return nil
	}
	c := newConvoyIssueCheck(ctx, store, townRoot, issueID, caller, logger, gtPath, isRigParked, resolver)
	convoyIDs := getTrackingConvoys(ctx, store, issueID, c.logger)
	if len(convoyIDs) == 0 {
		return nil
	}
	c.logger("%s: %s tracked by %d convoy(s): %v", caller, issueID, len(convoyIDs), convoyIDs)
	for _, convoyID := range convoyIDs {
		checkOneTrackingConvoy(c, convoyID)
	}
	return convoyIDs
}

// getTrackingConvoys returns convoy IDs that track the given issue.
// Uses SDK GetDependentsWithMetadata filtered by type "tracks".
func getTrackingConvoys(ctx context.Context, store beadsdk.Storage, issueID string, logger func(format string, args ...interface{})) []string {
	dependents, err := store.GetDependentsWithMetadata(ctx, issueID)
	if err != nil {
		if logger != nil {
			logger("Convoy: getTrackingConvoys(%s) store error: %v", issueID, err)
		}
		return nil
	}

	convoyIDs := make([]string, 0)
	for _, d := range dependents {
		if string(d.DependencyType) == "tracks" {
			convoyIDs = append(convoyIDs, d.ID)
		}
	}
	return convoyIDs
}

// isConvoyClosed checks if a convoy is already closed.
func isConvoyClosed(ctx context.Context, store beadsdk.Storage, convoyID string) bool {
	issue, err := store.GetIssue(ctx, convoyID)
	if err != nil || issue == nil {
		return false
	}
	return string(issue.Status) == "closed"
}

// isConvoyStaged checks if a convoy is in a staged state (not yet launched).
// Staged convoys have statuses like "staged_ready" or "staged_warnings".
// They should not be fed until they are launched (transitioned to "open").
func isConvoyStaged(ctx context.Context, store beadsdk.Storage, convoyID string) bool {
	issue, err := store.GetIssue(ctx, convoyID)
	if err != nil || issue == nil {
		return false // fail-open: if we can't read, assume not staged
	}
	return strings.HasPrefix(string(issue.Status), "staged_")
}

// runConvoyCheck runs `gt convoy check <convoy-id>` to check a specific convoy.
// This is idempotent and handles already-closed convoys gracefully.
// The context parameter enables cancellation on daemon shutdown.
// gtPath is the resolved path to the gt binary.
func runConvoyCheck(ctx context.Context, townRoot, convoyID, gtPath string) error {
	cmd := exec.CommandContext(ctx, gtPath, "convoy", "check", convoyID)
	cmd.Dir = townRoot
	util.SetProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}

	return nil
}

// trackedIssue holds basic info about an issue tracked by a convoy.
type trackedIssue struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Assignee  string `json:"assignee"`
	Priority  int    `json:"priority"`
	IssueType string `json:"issue_type"`
}

// slingableTypes are bead types that can be dispatched via gt sling.
// Only leaf work items are slingable — containers (epic) and non-work types
// (decision, message, event) are excluded. Unknown/empty types are treated
// as slingable (beads default to "task" when IssueType is empty).
var slingableTypes = map[string]bool{
	"task":    true,
	"bug":     true,
	"feature": true,
	"chore":   true,
	"":        true, // Empty type defaults to task
}

// IsSlingableType reports whether a bead type can be dispatched via gt sling.
// Exported for use by cmd/convoy.go stranded scan path.
func IsSlingableType(issueType string) bool {
	return slingableTypes[issueType]
}

// blockingDepTypes are dependency types that prevent an issue from being
// dispatched. parent-child is intentionally excluded — a child task is
// dispatchable even if its parent epic is open (consistent with molecule
// step behavior in internal/cmd/molecule_step.go).
var blockingDepTypes = map[string]bool{
	"blocks":             true,
	"conditional-blocks": true,
	"waits-for":          true,
	"merge-blocks":       true,
}

// isIssueBlocked checks if an issue has unclosed blocking dependencies.
// Returns true if any blocks, conditional-blocks, waits-for, or merge-blocks
// dependency targets an issue that is not closed/tombstone.
//
// For merge-blocks dependencies, "closed" alone is not sufficient — the
// blocker must have a CloseReason starting with "Merged in " to confirm
// that the code was actually integrated. This prevents dispatching work
// against un-merged code (see #1893).
//
// When a StoreResolver is provided, cross-database dependencies are resolved
// by querying the appropriate rig store for fresh status. Without a resolver,
// this falls back to the hq store's dependency metadata snapshot, which may
// be stale for cross-rig issues (see GH #2624).
func isIssueBlocked(ctx context.Context, store beadsdk.Storage, issueID string, resolver *StoreResolver) bool {
	deps := loadIssueDeps(ctx, store, issueID, resolver)
	if deps == nil {
		return false
	}
	staleIDs, staleTypes, blocked := scanBlockingDeps(deps, resolver != nil)
	if blocked {
		return true
	}
	return staleBlockersStillOpen(ctx, resolver, staleIDs, staleTypes)
}

// feedNextReadyIssue finds the next ready issue in a convoy and dispatches it
// via gt sling. A ready issue is one that is open, with no assignee, and not
// blocked by unclosed dependencies. This provides reactive (event-driven)
// convoy feeding instead of waiting for polling-based patrol cycles.
//
// Only one issue is dispatched per call. When that issue completes, the
// next close event triggers another feed cycle.
// gtPath is the resolved path to the gt binary.
func feedNextReadyIssue(ctx context.Context, store beadsdk.Storage, townRoot, convoyID, caller string, logger func(format string, args ...interface{}), gtPath string, isRigParked func(string) bool, resolver *StoreResolver) {
	tracked := getConvoyTrackedIssues(ctx, store, convoyID, townRoot, resolver)
	if len(tracked) == 0 {
		return
	}
	sortTrackedIssues(tracked)
	r := &convoyFeedRun{
		ctx: ctx, store: store, townRoot: townRoot, convoyID: convoyID, caller: caller,
		logger: logger, gtPath: gtPath, isRigParked: isRigParked, resolver: resolver,
		baseBranch: convoyBaseBranch(ctx, store, convoyID),
	}
	if !tryFeedReadyIssues(r, tracked) {
		logger("%s: convoy %s: no ready issues to feed", caller, convoyID)
	}
}

// getConvoyTrackedIssues returns issues tracked by a convoy with fresh status.
// Uses SDK GetDependenciesWithMetadata filtered by tracks, then GetIssuesByIDs for current status.
// When a StoreResolver is provided, cross-rig beads are resolved via direct store queries.
// Otherwise falls back to bd show subprocess via fetchCrossRigBeadStatus.
func getConvoyTrackedIssues(ctx context.Context, store beadsdk.Storage, convoyID, townRoot string, resolver *StoreResolver) []trackedIssue {
	ids, metaByID := collectTrackedIDs(ctx, store, convoyID)
	if len(ids) == 0 {
		return nil
	}
	freshMap := refreshTrackedIssueMap(ctx, store, townRoot, ids, resolver)
	result := trackedIssuesFromFreshOrMeta(ids, freshMap, metaByID)
	for _, t := range result {
		_, _, _, _, _ = t.ID, t.Status, t.Assignee, t.Priority, t.IssueType
	}
	return result
}

// extractIssueID strips the external:prefix:id wrapper from bead IDs.
func extractIssueID(id string) string {
	if strings.HasPrefix(id, "external:") {
		parts := strings.SplitN(id, ":", 3)
		if len(parts) == 3 {
			return parts[2]
		}
	}
	return id
}

// rigForIssue determines the rig name for an issue based on its ID prefix.
// Uses the beads routes to map prefixes to rigs.
func rigForIssue(townRoot, issueID string) string {
	prefix := beads.ExtractPrefix(issueID)
	if prefix == "" {
		return ""
	}
	return beads.GetRigNameForPrefix(townRoot, prefix)
}

// fetchCrossRigBeadStatus fetches fresh status for beads that live in other rigs.
// Groups IDs by prefix, resolves each prefix to its rig directory via routes,
// and runs `bd show --json <ids>` per rig. Pattern from batchFetchBeadInfoByIDs
// in capacity_dispatch.go.
func fetchCrossRigBeadStatus(townRoot string, ids []string) map[string]*beadsdk.Issue {
	result := make(map[string]*beadsdk.Issue)
	if len(ids) == 0 {
		return result
	}

	// Group IDs by prefix
	byPrefix := make(map[string][]string)
	for _, id := range ids {
		prefix := beads.ExtractPrefix(id)
		if prefix != "" {
			byPrefix[prefix] = append(byPrefix[prefix], id)
		}
	}

	for prefix, prefixIDs := range byPrefix {
		rigPath := beads.GetRigPathForPrefix(townRoot, prefix)
		if rigPath == "" {
			continue
		}

		args := append([]string{"show", "--json"}, prefixIDs...)
		cmd := beads.Spawn(args...)
		cmd.Dir = rigPath
		util.SetDetachedProcessGroup(cmd)
		out, err := cmd.Output()
		if err != nil {
			continue
		}

		var items []struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Assignee string `json:"assignee"`
			Priority int    `json:"priority"`
			Type     string `json:"issue_type"`
		}
		if err := json.Unmarshal(out, &items); err != nil {
			continue
		}
		for _, item := range items {
			result[item.ID] = &beadsdk.Issue{
				ID:        item.ID,
				Status:    beadsdk.Status(item.Status),
				Assignee:  item.Assignee,
				Priority:  item.Priority,
				IssueType: beadsdk.IssueType(item.Type),
			}
		}
	}

	return result
}

// FireCrossRigDepNotifications checks if any issues in other rigs were unblocked
// by the closure of closedIssueID. For each affected rig, sends a nudge to that
// rig's witness so it can react to the resolved dependency.
//
// Cross-rig deps are stored in the dependent issue's store as "external:<prefix>:<id>".
// To find them, this function queries each rig store using the external-wrapped form
// of the closed issue ID.
//
// This is best-effort: failures are silently logged. The closed issue's own store
// is skipped (same-rig deps don't need cross-rig notification).
func FireCrossRigDepNotifications(ctx context.Context, closedIssueID, townRoot string, stores map[string]beadsdk.Storage, logger func(format string, args ...interface{})) {
	if logger == nil {
		logger = func(format string, args ...interface{}) {}
	}
	if len(stores) == 0 || closedIssueID == "" || townRoot == "" {
		return
	}
	closedPrefix := beads.ExtractPrefix(closedIssueID)
	if closedPrefix == "" {
		return
	}
	closedRig, closedStoreKey := closedIssueStoreKey(townRoot, closedPrefix)
	n := &crossRigNotify{
		ctx: ctx, townRoot: townRoot, closedIssueID: closedIssueID,
		closedRig: closedRig, closedStoreKey: closedStoreKey,
		externalID: fmt.Sprintf("external:%s:%s", strings.TrimSuffix(closedPrefix, "-"), closedIssueID),
		logger:     logger, notifiedRigs: make(map[string]bool),
	}
	for storeName, store := range stores {
		notifyStoreCrossRigDeps(n, storeName, store)
	}
}

// dispatchIssue dispatches an issue to a rig via gt sling.
// The context parameter enables cancellation on daemon shutdown.
// gtPath is the resolved path to the gt binary.
func dispatchIssue(ctx context.Context, townRoot, issueID, rig, gtPath, baseBranch string) error {
	args := []string{"sling", issueID, rig, "--no-boot"}
	if baseBranch != "" {
		args = append(args, "--base-branch="+baseBranch)
	}
	cmd := exec.CommandContext(ctx, gtPath, args...)
	cmd.Dir = townRoot
	util.SetProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}

	return nil
}
