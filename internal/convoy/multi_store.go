// Package convoy — multi-store resolution for cross-database convoy tracking.
package convoy

import (
	"context"
	"strings"

	beadsdk "github.com/jonbaldie/beads"
	"github.com/jonbaldie/gastown/internal/beads"
)

// StoreResolver resolves beads issues across multiple stores using prefix-based
// routing. In multi-rig Gas Town setups, each rig has its own Dolt database.
// Convoys live in the HQ store but may track issues in rig stores (e.g., ds-*
// in dashboard). Without cross-store resolution, convoy tracking sees 0/0 for
// cross-database dependencies. See GH #2624.
type StoreResolver struct {
	// stores maps store names ("hq", "dashboard", etc.) to beads stores.
	stores map[string]beadsdk.Storage

	// townRoot is the path to the town root, used for prefix → rig name lookup.
	townRoot string
}

// NewStoreResolver creates a resolver from the daemon's store map.
// If stores is nil or empty, all resolution methods fall through gracefully.
func NewStoreResolver(townRoot string, stores map[string]beadsdk.Storage) *StoreResolver {
	return &StoreResolver{
		stores:   stores,
		townRoot: townRoot,
	}
}

// ResolveIssues fetches fresh issue data for the given IDs, looking up each
// issue in the appropriate store based on its prefix. Issues found in any store
// are returned in the result map. Issues not found in any store are omitted.
func (r *StoreResolver) ResolveIssues(ctx context.Context, ids []string) map[string]*beadsdk.Issue {
	result := make(map[string]*beadsdk.Issue, len(ids))
	if len(r.stores) == 0 || len(ids) == 0 {
		return result
	}

	for storeName, storeIDs := range r.groupIssueIDsByStore(ids) {
		r.resolveStoreIssues(ctx, result, storeName, storeIDs)
	}
	return result
}

// groupIssueIDsByStore groups IDs by their prefix-routed target store.
func (r *StoreResolver) groupIssueIDsByStore(ids []string) map[string][]string {
	byStore := make(map[string][]string)
	for _, id := range ids {
		if storeName := r.storeForID(id); storeName != "" {
			byStore[storeName] = append(byStore[storeName], id)
		}
	}
	return byStore
}

func (r *StoreResolver) resolveStoreIssues(ctx context.Context, result map[string]*beadsdk.Issue, storeName string, ids []string) {
	store := r.stores[storeName]
	if store == nil {
		return
	}
	issues, err := store.GetIssuesByIDs(ctx, ids)
	if err != nil {
		return
	}
	for _, issue := range issues {
		if issue != nil {
			result[issue.ID] = issue
		}
	}
}

// ResolveDepsWithMetadata fetches dependency metadata for an issue, trying
// the appropriate store for that issue's prefix. Returns nil on any error.
func (r *StoreResolver) ResolveDepsWithMetadata(ctx context.Context, issueID string) []*beadsdk.IssueWithDependencyMetadata {
	if len(r.stores) == 0 {
		return nil
	}

	storeName := r.storeForID(issueID)
	if storeName == "" {
		return nil
	}
	store := r.stores[storeName]
	if store == nil {
		return nil
	}

	deps, err := store.GetDependenciesWithMetadata(ctx, issueID)
	if err != nil {
		return nil
	}
	return deps
}

// storeForID returns the store name for a given issue ID based on prefix routing.
// Returns "hq" for town-level prefixes, rig name for rig prefixes, or "" if unknown.
func (r *StoreResolver) storeForID(id string) string {
	// Strip external: wrapper if present
	if strings.HasPrefix(id, "external:") {
		parts := strings.SplitN(id, ":", 3)
		if len(parts) == 3 {
			id = parts[2]
		}
	}

	prefix := beads.ExtractPrefix(id)
	if prefix == "" {
		return ""
	}

	rigName := beads.GetRigNameForPrefix(r.townRoot, prefix)
	if rigName == "" {
		// Town-level prefix (e.g., "hq-") or unknown → use hq store
		return "hq"
	}
	return rigName
}
