package checkpoint

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitWIPCommitSubjects(t *testing.T) {
	subjects := []string{
		"newest real change",
		WIPCommitPrefix,
		WIPCommitPrefix + " after tests",
		"",
		"older real change",
		"WIP checkpoint (auto)",
	}

	count, nonWIP := splitWIPCommitSubjects(subjects)
	if count != 2 {
		t.Fatalf("WIP count = %d, want 2", count)
	}
	want := []string{"newest real change", "older real change", "WIP checkpoint (auto)"}
	if strings.Join(nonWIP, "|") != strings.Join(want, "|") {
		t.Fatalf("non-WIP subjects = %q, want %q", nonWIP, want)
	}
}

func TestSquashedCommitMessage(t *testing.T) {
	tests := []struct {
		name     string
		subjects []string
		want     string
	}{
		{"none", nil, "squashed WIP checkpoint commits"},
		{"one", []string{"implement feature"}, "implement feature"},
		{"many", []string{"newest", "middle", "oldest"}, "newest\n- middle\n- oldest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := squashedCommitMessage(tt.subjects); got != tt.want {
				t.Fatalf("squashedCommitMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGitOutputReportsStderr(t *testing.T) {
	_, err := gitOutput(t.TempDir(), "definitely-not-a-git-subcommand")
	if err == nil {
		t.Fatal("gitOutput() error = nil, want command failure")
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("gitOutput() returned raw ExitError instead of stderr context: %v", err)
	}
	if !strings.Contains(err.Error(), "definitely-not-a-git-subcommand") {
		t.Fatalf("gitOutput() error = %q, want git stderr", err)
	}
}

func TestCommitsSinceBasePropagatesMergeBaseError(t *testing.T) {
	dir := initTestRepo(t)
	mergeBase, subjects, err := commitsSinceBase(dir, "missing-base")
	if err == nil || !strings.Contains(err.Error(), "finding merge-base") {
		t.Fatalf("commitsSinceBase() error = %v, want finding merge-base error", err)
	}
	if mergeBase != "" || subjects != nil {
		t.Fatalf("error result = (%q, %v), want empty values", mergeBase, subjects)
	}
}

func TestCommitsSinceBaseReturnsMergeBaseWithNoCommits(t *testing.T) {
	dir := initTestRepo(t)
	mergeBase, subjects, err := commitsSinceBase(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if mergeBase == "" || subjects != nil {
		t.Fatalf("result = (%q, %v), want merge base and nil subjects", mergeBase, subjects)
	}
}

// initTestRepo creates a fresh git repo with an initial commit and returns its path.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args[1:], err, out)
		}
	}

	// Create initial commit on main
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "-A"},
		{"git", "commit", "-m", "initial commit"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args[1:], err, out)
		}
	}

	return dir
}

// createBranch creates a branch from current HEAD and switches to it.
func createBranch(t *testing.T, dir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "checkout", "-b", branch)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout -b %s failed: %v\n%s", branch, err, out)
	}
}

// addCommit adds a file and commits with the given message.
func addCommit(t *testing.T, dir, filename, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", filename},
		{"git", "commit", "-m", msg},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args[1:], err, out)
		}
	}
}

// getCommitSubjects returns the commit subjects on the branch since main.
func getCommitSubjects(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("git", "log", "--format=%s", "main..HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

func TestCountWIPCommits_NoWIP(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "add feature A")
	addCommit(t, dir, "b.go", "package b", "add feature B")

	count, err := CountWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 WIP commits, got %d", count)
	}
}

func TestCountWIPCommits_AllWIP(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)

	count, err := CountWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 WIP commits, got %d", count)
	}
}

func TestCountWIPCommits_Mixed(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "real work")
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)
	addCommit(t, dir, "c.go", "package c", "more real work")

	count, err := CountWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 WIP commit, got %d", count)
	}
}

func TestSquashWIPCommits_NoWIP(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "real work")

	wipCount, err := SquashWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 0 {
		t.Errorf("expected 0, got %d", wipCount)
	}

	// Verify commit is untouched
	subjects := getCommitSubjects(t, dir)
	if len(subjects) != 1 || subjects[0] != "real work" {
		t.Errorf("expected [real work], got %v", subjects)
	}
}

func TestSquashWIPCommits_AllWIP(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", WIPCommitPrefix)
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)

	wipCount, err := SquashWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 2 {
		t.Errorf("expected 2, got %d", wipCount)
	}

	// Verify squashed into single commit with generic message
	subjects := getCommitSubjects(t, dir)
	if len(subjects) != 1 {
		t.Errorf("expected 1 commit after squash, got %d: %v", len(subjects), subjects)
	}
	if len(subjects) > 0 && subjects[0] != "squashed WIP checkpoint commits" {
		t.Errorf("expected generic message, got %q", subjects[0])
	}

	// Verify files exist
	for _, f := range []string{"a.go", "b.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist after squash", f)
		}
	}
}

func TestSquashWIPCommits_Mixed(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")
	addCommit(t, dir, "a.go", "package a", "implement auth handler")
	addCommit(t, dir, "b.go", "package b", WIPCommitPrefix)
	addCommit(t, dir, "c.go", "package c", "add auth tests")
	addCommit(t, dir, "d.go", "package d", WIPCommitPrefix)

	wipCount, err := SquashWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 2 {
		t.Errorf("expected 2, got %d", wipCount)
	}

	// Verify squashed into single commit with non-WIP subjects preserved
	subjects := getCommitSubjects(t, dir)
	if len(subjects) != 1 {
		t.Errorf("expected 1 commit after squash, got %d: %v", len(subjects), subjects)
	}

	// Verify all files exist
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist after squash", f)
		}
	}
}

func TestSquashWIPCommits_NoCommits(t *testing.T) {
	dir := initTestRepo(t)
	createBranch(t, dir, "feature")

	wipCount, err := SquashWIPCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if wipCount != 0 {
		t.Errorf("expected 0 for no commits, got %d", wipCount)
	}
}
