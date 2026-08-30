package cmd

import "github.com/jonbaldie/gastown/internal/git"

type gitRebaseAdapter struct{ g *git.Git }

func (a gitRebaseAdapter) Rebase(base string) error {
	return git.Rebase(a.g, base)
}

func (a gitRebaseAdapter) AbortRebase() error {
	return git.AbortRebase(a.g)
}

type gitBranchVerifier struct{ g *git.Git }

func (a gitBranchVerifier) BranchExists(name string) (bool, error) {
	return git.BranchExists(a.g, name)
}

func (a gitBranchVerifier) RemoteTrackingBranchExists(remote, branch string) (bool, error) {
	return git.RemoteTrackingBranchExists(a.g, remote, branch)
}

type gitPostMergeAdapter struct{ g *git.Git }

func (a gitPostMergeAdapter) VerifyPushedCommitReachableFromPushTarget(remote, branch, commit string) error {
	return git.VerifyPushedCommitReachableFromPushTarget(a.g, remote, branch, commit)
}

func (a gitPostMergeAdapter) PushRemoteBranchTip(remote, branch string) (string, error) {
	return git.PushRemoteBranchTip(a.g, remote, branch)
}

func (a gitPostMergeAdapter) HasOpenPullRequest(ref git.PullRequestRef) bool {
	return git.HasOpenPullRequest(a.g, ref)
}

func (a gitPostMergeAdapter) Rev(ref string) (string, error) {
	return git.Rev(a.g, ref)
}

func (a gitPostMergeAdapter) DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error {
	return git.DeleteRemoteBranchIfAt(a.g, remote, branch, expectedHash)
}

func (a gitPostMergeAdapter) DeleteBranch(name string, force bool) error {
	return git.DeleteBranch(a.g, name, force)
}
