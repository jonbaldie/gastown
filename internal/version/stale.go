// Package version provides version information and staleness checking for gt.
package version

import (
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"

	"github.com/jonbaldie/gastown/internal/util"
)

// StaleBinaryInfo contains information about binary staleness.
type StaleBinaryInfo struct {
	IsStale       bool   // True if binary commit is behind the build-branch ref
	IsForward     bool   // True if the compare commit is a descendant of binary commit (safe to rebuild)
	OnMainBranch  bool   // True if the resolved source worktree is on a build branch
	BinaryCommit  string // Commit hash the binary was built from
	RepoCommit    string // Commit of the ref the binary was compared against (CompareRef)
	CompareRef    string // The ref staleness was computed against (e.g. "main", "origin/main")
	CommitsBehind int    // Number of commits binary is behind (0 if unknown)
	Skipped       bool   // True if staleness could not be determined safely
	SkipReason    string // Human-readable reason the check was skipped
	Error         error  // Any error encountered during check
}

type buildBranchRef struct {
	ref     string
	display string
	commit  string
}

// resolveCommitHash gets the commit hash from an optional build override or
// the VCS metadata embedded in the binary.
func resolveCommitHash(commitOverride string) string {
	if commitOverride != "" {
		return commitOverride
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}

	return ""
}

// Describe returns a one-line, human-readable staleness summary for a stale
// binary, using subject as the leading noun so callers can vary it
// ("Binary" for gt doctor, "gt binary" for the startup warning):
//
//	"Binary is 3 commits behind main (built from abc123…, main at def456…)"
//	"gt binary is stale (built from abc123…, origin/main at def456…)"
//
// It is only meaningful when i.IsStale; callers gate on that. A zero
// CommitsBehind (count unknown) falls back to the "is stale" wording.
func (i *StaleBinaryInfo) Describe(subject string) string {
	if i.CommitsBehind > 0 {
		return fmt.Sprintf("%s is %d commits behind %s (built from %s, %s at %s)",
			subject, i.CommitsBehind, i.CompareRef,
			ShortCommit(i.BinaryCommit), i.CompareRef, ShortCommit(i.RepoCommit))
	}
	return fmt.Sprintf("%s is stale (built from %s, %s at %s)",
		subject, ShortCommit(i.BinaryCommit), i.CompareRef, ShortCommit(i.RepoCommit))
}

// ShortCommit returns first 12 characters of a hash.
func ShortCommit(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// commitsMatch compares two commit hashes, handling different lengths.
// Returns true if one is a prefix of the other (minimum 7 chars to avoid false positives).
func commitsMatch(a, b string) bool {
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	// Need at least 7 chars for a reasonable comparison
	if minLen < 7 {
		return false
	}
	return strings.HasPrefix(a, b[:minLen]) || strings.HasPrefix(b, a[:minLen])
}

// CheckStaleBinary compares the binary's embedded commit with a build-branch
// ref. An optional commit override should be supplied when a caller receives
// the build-time commit through its own linker-injected configuration. When
// omitted, the check reads VCS metadata embedded in the binary.
//
// It returns staleness info including whether the binary needs rebuilding.
// This check is designed to be fast and non-blocking - errors are captured but
// don't interrupt normal operation.
func CheckStaleBinary(repoDir string, commitOverride ...string) *StaleBinaryInfo {
	info := &StaleBinaryInfo{}
	configuredCommit := ""
	if len(commitOverride) > 0 {
		configuredCommit = commitOverride[0]
	}
	binaryCommit, done := prepareStaleBinary(repoDir, info, configuredCommit)
	if done {
		return info
	}

	compareCommit, done := resolveStaleComparison(repoDir, info, binaryCommit)
	if done {
		return info
	}
	info.RepoCommit = compareCommit
	assessStaleBinary(repoDir, info, binaryCommit, compareCommit)
	return info
}

func prepareStaleBinary(repoDir string, info *StaleBinaryInfo, commitOverride string) (string, bool) {
	info.BinaryCommit = resolveCommitHash(commitOverride)
	if info.BinaryCommit == "" {
		info.Error = fmt.Errorf("cannot determine binary commit (dev build?)")
		return "", true
	}
	if !isGitRepo(repoDir) {
		info.Error = fmt.Errorf("source repo %q is not a git worktree", repoDir)
		return "", true
	}
	binaryCommit, err := resolveGitCommit(repoDir, info.BinaryCommit)
	if err != nil {
		info.Skipped = true
		info.SkipReason = "binary commit not found in source repo; cannot compare staleness"
		return "", true
	}
	return binaryCommit, false
}

func resolveStaleComparison(repoDir string, info *StaleBinaryInfo, binaryCommit string) (string, bool) {
	branch := currentBranch(repoDir)
	info.OnMainBranch = isBuildBranch(branch)
	if info.OnMainBranch {
		info.CompareRef = branch
		compareCommit, err := resolveGitCommit(repoDir, "HEAD")
		if err != nil {
			info.Error = fmt.Errorf("cannot resolve build branch HEAD: %w", err)
			return "", true
		}
		return compareCommit, false
	}

	ref, ok := resolveBuildBranchRef(repoDir, binaryCommit)
	if !ok {
		info.Skipped = true
		info.SkipReason = "source worktree not on a build branch and no build-branch ref found to compare against"
		return "", true
	}
	info.CompareRef = ref.display
	return ref.commit, false
}

func currentBranch(repoDir string) string {
	branchCmd := exec.Command("git", "symbolic-ref", "--short", "HEAD")
	branchCmd.Dir = repoDir
	util.SetDetachedProcessGroup(branchCmd)
	branchOutput, err := branchCmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(branchOutput))
}

func assessStaleBinary(repoDir string, info *StaleBinaryInfo, binaryCommit, compareCommit string) {
	if commitsMatch(info.BinaryCommit, info.RepoCommit) {
		return
	}
	if onlyBeadsChanges(repoDir, binaryCommit, compareCommit) {
		return
	}

	info.IsStale = true
	info.IsForward = isAncestor(repoDir, binaryCommit, compareCommit)
	info.CommitsBehind = countCommitsBehind(repoDir, binaryCommit, compareCommit)
}

func countCommitsBehind(repoDir, binaryCommit, compareCommit string) int {
	countCmd := exec.Command("git", "rev-list", "--count", binaryCommit+".."+compareCommit)
	countCmd.Dir = repoDir
	util.SetDetachedProcessGroup(countCmd)
	countOutput, err := countCmd.Output()
	if err != nil {
		return 0
	}
	var commitsBehind int
	count, parseErr := fmt.Sscanf(strings.TrimSpace(string(countOutput)), "%d", &commitsBehind)
	if parseErr != nil || count != 1 {
		return 0
	}
	return commitsBehind
}

// resolveBuildBranchRef finds a build-branch ref to compare the binary against
// when the resolved source worktree is parked on a non-build branch (the normal
// state for $GT_ROOT/gastown/mayor/rig). Without this, staleness would be
// computed against unmerged feature work (GH#4034).
//
// Candidate refs are fully qualified to avoid branch/tag shadowing. Among refs
// that contain the binary commit, choose the freshest descendant; only use the
// candidate order below to break truly diverged ties.
func resolveBuildBranchRef(repoDir, binaryCommit string) (buildBranchRef, bool) {
	usable := usableBuildBranchRefs(repoDir, binaryCommit)
	return freshestBuildBranchRef(repoDir, usable)
}

func usableBuildBranchRefs(repoDir, binaryCommit string) []buildBranchRef {
	var usable []buildBranchRef
	for _, candidate := range buildBranchCandidates(repoDir) {
		commit, err := resolveGitCommit(repoDir, candidate.ref)
		if err != nil || !isAncestor(repoDir, binaryCommit, commit) {
			continue
		}
		candidate.commit = commit
		usable = append(usable, candidate)
	}
	return usable
}

func freshestBuildBranchRef(repoDir string, usable []buildBranchRef) (buildBranchRef, bool) {
	if len(usable) == 0 {
		return buildBranchRef{}, false
	}

	frontier := make([]buildBranchRef, 0, len(usable))
	for _, candidate := range usable {
		if !hasNewerBuildBranchRef(repoDir, candidate, usable) {
			frontier = append(frontier, candidate)
		}
	}
	return frontier[0], true
}

func hasNewerBuildBranchRef(repoDir string, candidate buildBranchRef, usable []buildBranchRef) bool {
	for _, other := range usable {
		if candidate.commit == other.commit {
			continue
		}
		if isAncestor(repoDir, candidate.commit, other.commit) {
			return true
		}
	}
	return false
}

func buildBranchCandidates(repoDir string) []buildBranchRef {
	candidates := make([]buildBranchRef, 0, 10)
	for _, pattern := range []string{
		"refs/heads/carry/",
		"refs/remotes/upstream/carry/",
		"refs/remotes/origin/carry/",
	} {
		if ref, ok := singleBranchRef(repoDir, pattern); ok {
			candidates = append(candidates, ref)
		}
	}
	candidates = append(candidates,
		buildBranchRef{ref: "refs/remotes/upstream/main", display: "upstream/main"},
		buildBranchRef{ref: "refs/remotes/upstream/master", display: "upstream/master"},
		buildBranchRef{ref: "refs/remotes/origin/main", display: "origin/main"},
		buildBranchRef{ref: "refs/remotes/origin/master", display: "origin/master"},
		buildBranchRef{ref: "refs/heads/main", display: "main"},
		buildBranchRef{ref: "refs/heads/master", display: "master"},
	)
	return candidates
}

func resolveGitCommit(repoDir, rev string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	cmd.Dir = repoDir
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// isAncestor reports whether ancestor is an ancestor of ref (a commit is its
// own ancestor) in repoDir.
func isAncestor(repoDir, ancestor, ref string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, ref)
	cmd.Dir = repoDir
	util.SetDetachedProcessGroup(cmd)
	return cmd.Run() == nil
}

// singleBranchRef returns the sole matching branch/ref, if exactly one exists.
// Multiple matches are ambiguous and yield false.
func singleBranchRef(repoDir, pattern string) (buildBranchRef, bool) {
	cmd := exec.Command("git", "for-each-ref", "--format=%(refname)", pattern)
	cmd.Dir = repoDir
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return buildBranchRef{}, false
	}
	refs := strings.Fields(strings.TrimSpace(string(out)))
	if len(refs) != 1 {
		return buildBranchRef{}, false
	}
	display := strings.TrimPrefix(refs[0], "refs/heads/")
	display = strings.TrimPrefix(display, "refs/remotes/")
	return buildBranchRef{ref: refs[0], display: display}, true
}

// GetRepoRoot returns the git repository root for the gt source code.
// The canonical source is the gastown repo itself ($GT_ROOT/gastown).
// Crew rigs also contain cmd/gt/main.go but have different HEADs,
// so we prefer the gastown repo over CWD-based git toplevel detection.
func GetRepoRoot() (string, error) {
	// Check if GT_ROOT environment variable is set (agents always have this)
	if gtRoot := os.Getenv("GT_ROOT"); gtRoot != "" {
		candidates := []string{
			gtRoot + "/gastown",
			gtRoot + "/gastown/mayor/rig",
		}
		for _, candidate := range candidates {
			if hasGtSource(candidate) {
				return candidate, nil
			}
		}
	}

	// Try common development paths relative to home
	home := os.Getenv("HOME")
	if home != "" {
		candidates := []string{
			home + "/gt/gastown",
			home + "/gt/gastown/mayor/rig",
			home + "/gastown",
			home + "/gastown/mayor/rig",
			home + "/src/gastown",
			home + "/src/gastown/mayor/rig",
		}
		for _, candidate := range candidates {
			if hasGtSource(candidate) {
				return candidate, nil
			}
		}
	}

	// Fall back to current directory's git repo (may be a crew rig)
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	util.SetDetachedProcessGroup(cmd)
	if output, err := cmd.Output(); err == nil {
		root := strings.TrimSpace(string(output))
		if hasGtSource(root) {
			return root, nil
		}
	}

	return "", fmt.Errorf("cannot locate gt source repository")
}

// isGitRepo checks if a directory is a git repository.
func isGitRepo(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(output)) == "true"
}

// hasGtSource checks if a directory contains the gt source code.
// We look for cmd/gt/main.go as the definitive marker.
func hasGtSource(dir string) bool {
	_, err := os.Stat(dir + "/cmd/gt/main.go")
	return err == nil
}

// onlyBeadsChanges checks whether all commits between binaryCommit and
// compareRef exclusively modify files under .beads/. Returns true if the diff
// contains no changes outside .beads/, meaning the binary is functionally
// up-to-date. Used to suppress false-positive stale warnings from bd backup
// commits. (GH#2596)
func onlyBeadsChanges(repoDir, binaryCommit, compareRef string) bool {
	// Get files changed between binary commit and the build ref, excluding
	// .beads/. If this produces no output, all changes are within .beads/
	cmd := exec.Command("git", "diff", "--name-only", binaryCommit+".."+compareRef, "--", ".", ":!.beads")
	cmd.Dir = repoDir
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	if err != nil {
		// Can't determine — be conservative, assume stale
		return false
	}
	return strings.TrimSpace(string(output)) == ""
}

// isBuildBranch returns true if the given branch is safe for automated rebuilds.
// Accepted branches:
//   - main, master: upstream default branches
//   - carry/*: fork operational branches (e.g., carry/operational)
//
// This prevents automated rebuilds from random feature, fix, or polecat branches
// which could cause downgrades or crash loops.
func isBuildBranch(branch string) bool {
	switch branch {
	case "main", "master":
		return true
	}
	return strings.HasPrefix(branch, "carry/")
}
