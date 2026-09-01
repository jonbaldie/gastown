package refinery

import "github.com/jonbaldie/gastown/internal/git"

// PRProvider abstracts VCS-specific PR operations for the merge queue.
// Implementations exist for GitHub (default) and Bitbucket Cloud.
type PRProvider interface {
	// FindPullRequest returns the PR for the given ref, or nil if none exists.
	FindPullRequest(_, _ string, _ int, _ string) (*git.PullRequestInfo, error)

	// IsPRApproved checks whether a PR has at least one approving review.
	IsPRApproved(_ *git.PullRequestInfo) (bool, error)

	// MergePR merges a PR using the specified method (e.g., "squash", "merge", "rebase").
	// Returns the merge commit SHA on success (if available).
	MergePR(_ *git.PullRequestInfo, _ string) (string, error)
}
