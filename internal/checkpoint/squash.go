package checkpoint

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/jonbaldie/gastown/internal/util"
)

// WIPCommitPrefix is the commit message prefix used by checkpoint_dog auto-commits.
const WIPCommitPrefix = "WIP: checkpoint (auto)"

// CountWIPCommits returns the number of WIP checkpoint commits between
// the merge-base of baseRef and HEAD.
func CountWIPCommits(workDir, baseRef string) (int, error) {
	mergeBase, err := gitOutput(workDir, "merge-base", baseRef, "HEAD")
	if err != nil {
		return 0, fmt.Errorf("finding merge-base: %w", err)
	}

	// List commit subjects from merge-base..HEAD
	logOut, err := gitOutput(workDir, "log", "--format=%s", mergeBase+"..HEAD")
	if err != nil {
		return 0, fmt.Errorf("listing commits: %w", err)
	}

	if logOut == "" {
		return 0, nil
	}

	count := 0
	for _, line := range strings.Split(logOut, "\n") {
		if strings.HasPrefix(line, WIPCommitPrefix) {
			count++
		}
	}
	return count, nil
}

// SquashWIPCommits collapses all commits from merge-base..HEAD into a single
// commit, preserving non-WIP commit messages in the body. Returns the number
// of WIP commits that were squashed.
//
// This is safe because Refinery squash-merges polecat branches anyway —
// individual commit history on polecat branches is not preserved.
func SquashWIPCommits(workDir, baseRef string) (int, error) {
	mergeBase, subjects, err := commitsSinceBase(workDir, baseRef)
	if err != nil {
		return 0, err
	}
	if len(subjects) == 0 {
		return 0, nil // No commits to squash
	}
	wipCount, nonWIPSubjects := splitWIPCommitSubjects(subjects)
	if wipCount == 0 {
		return 0, nil // No WIP commits to squash
	}
	if _, err := gitOutput(workDir, "reset", "--soft", mergeBase); err != nil {
		return 0, fmt.Errorf("soft reset: %w", err)
	}
	if _, err := gitOutput(workDir, "commit", "-m", squashedCommitMessage(nonWIPSubjects)); err != nil {
		return 0, fmt.Errorf("squash commit: %w", err)
	}
	return wipCount, nil
}

func commitsSinceBase(workDir, baseRef string) (string, []string, error) {
	mergeBase, err := gitOutput(workDir, "merge-base", baseRef, "HEAD")
	if err != nil {
		return "", nil, fmt.Errorf("finding merge-base: %w", err)
	}
	logOut, err := gitOutput(workDir, "log", "--format=%s", mergeBase+"..HEAD")
	if err != nil {
		return "", nil, fmt.Errorf("listing commits: %w", err)
	}
	if logOut == "" {
		return mergeBase, nil, nil
	}
	return mergeBase, strings.Split(logOut, "\n"), nil
}

func splitWIPCommitSubjects(subjects []string) (int, []string) {
	wipCount := 0
	nonWIPSubjects := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		if strings.HasPrefix(subject, WIPCommitPrefix) {
			wipCount++
		} else if subject != "" {
			nonWIPSubjects = append(nonWIPSubjects, subject)
		}
	}
	return wipCount, nonWIPSubjects
}

func squashedCommitMessage(nonWIPSubjects []string) string {
	if len(nonWIPSubjects) == 0 {
		return "squashed WIP checkpoint commits"
	}
	var message strings.Builder
	message.WriteString(nonWIPSubjects[0])
	if len(nonWIPSubjects) > 1 {
		for _, subject := range nonWIPSubjects[1:] {
			message.WriteString("\n- ")
			message.WriteString(subject)
		}
	}
	return message.String()
}

// gitOutput runs a git command and returns trimmed stdout.
func gitOutput(workDir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	util.SetDetachedProcessGroup(cmd)

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return "", fmt.Errorf("%s: %s", err, stderr)
			}
		}
		return "", err
	}

	return strings.TrimSpace(string(out)), nil
}
