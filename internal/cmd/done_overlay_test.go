package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/instructions"
)

func TestStripOverlayInstructionFiles_RemovesAgentsOverlay(t *testing.T) {
	dir := initOverlayRepo(t)
	overlay := polecatOverlayText()
	writeAndCommit(t, dir, "feature", map[string]string{
		"AGENTS.md": overlay,
		"CLAUDE.md": overlay,
	})

	g := git.NewGit(dir)
	if !stripOverlayInstructionFiles(g, "main", "main") {
		t.Fatal("expected overlay pair to be stripped")
	}

	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := g.ShowFile("HEAD", name); err == nil {
			t.Errorf("%s still present on HEAD after strip", name)
		}
	}
}

func TestStripOverlayInstructionFiles_RemovesLocalPair(t *testing.T) {
	dir := initOverlayRepo(t)
	writeAndCommit(t, dir, "feature", map[string]string{
		"AGENTS.local.md": polecatOverlayText(),
		"CLAUDE.local.md": polecatOverlayText(),
	})

	g := git.NewGit(dir)
	if !stripOverlayInstructionFiles(g, "main", "main") {
		t.Fatal("expected local overlay pair to be stripped")
	}
	for _, name := range []string{"AGENTS.local.md", "CLAUDE.local.md"} {
		if _, err := g.ShowFile("HEAD", name); err == nil {
			t.Errorf("%s still present on HEAD after strip", name)
		}
	}
}

func TestStripOverlayInstructionFiles_RestoresConstitution(t *testing.T) {
	dir := initOverlayRepo(t)
	constitution := "# Project\nKeep this file.\n"
	runOverlayGit(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(constitution), 0644); err != nil {
		t.Fatal(err)
	}
	runOverlayGit(t, dir, "add", "CLAUDE.md")
	runOverlayGit(t, dir, "commit", "-m", "add constitution")

	writeAndCommit(t, dir, "feature", map[string]string{
		"CLAUDE.md": polecatOverlayText(),
	})

	g := git.NewGit(dir)
	if !stripOverlayInstructionFiles(g, "main", "main") {
		t.Fatal("expected constitution restore")
	}
	got, err := g.ShowFile("HEAD", "CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Keep this file") {
		t.Fatalf("constitution not restored: %q", got)
	}
	if instructions.IsGasTownOverlay(got) {
		t.Fatal("restored CLAUDE.md still looks like a Gas Town overlay")
	}
}

func TestStripOverlayInstructionFiles_LeavesConstitutionAlone(t *testing.T) {
	dir := initOverlayRepo(t)
	constitution := "# Project\nKeep this file.\n"
	runOverlayGit(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(constitution), 0644); err != nil {
		t.Fatal(err)
	}
	runOverlayGit(t, dir, "add", "CLAUDE.md")
	runOverlayGit(t, dir, "commit", "-m", "add constitution")

	writeAndCommit(t, dir, "feature", map[string]string{
		"README.md": "# still working\n",
	})

	g := git.NewGit(dir)
	if stripOverlayInstructionFiles(g, "main", "main") {
		t.Fatal("constitution-only branch should not create a strip commit")
	}
	got, err := g.ShowFile("HEAD", "CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != constitution && !strings.Contains(got, "Keep this file") {
		t.Fatalf("constitution changed: %q", got)
	}
}

func polecatOverlayText() string {
	return "# Polecat Context\n\n## 🚨 THE " + instructions.LifecycleMarker + " 🚨\n\ngt done\n"
}

func initOverlayRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runOverlayGit(t, dir, "init", "-b", "main")
	runOverlayGit(t, dir, "config", "user.name", "test")
	runOverlayGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runOverlayGit(t, dir, "add", "README.md")
	runOverlayGit(t, dir, "commit", "-m", "init")
	return dir
}

func writeAndCommit(t *testing.T, dir, branch string, files map[string]string) {
	t.Helper()
	runOverlayGit(t, dir, "checkout", "-B", branch)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		runOverlayGit(t, dir, "add", name)
	}
	runOverlayGit(t, dir, "commit", "-m", "add overlay")
}

func runOverlayGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
