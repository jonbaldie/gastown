package rig

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
)

func TestAddLocalRigFencesTownBeadWalkUp(t *testing.T) {
	town, cfg := setupTestTown(t)
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}

	repo := createLocalGitRepo(t, filepath.Join(t.TempDir(), "demo"))
	mgr := NewManager(town, cfg, git.NewGit(town))
	if _, err := mgr.AddLocalRig(context.Background(), "demo", repo); err != nil {
		t.Fatalf("AddLocalRig: %v", err)
	}

	configPath := filepath.Join(town, "demo", ".beads", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading rig beads fence: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "prefix: de") && !strings.Contains(text, "issue-prefix: de") {
		t.Fatalf("rig beads fence missing prefix de:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(repo, ".beads")); !os.IsNotExist(err) {
		t.Fatal("AddLocalRig wrote .beads into the source repository")
	}
	if _, ok := beads.ResolveRepoAliasBeadsDir(town, "demo"); !ok {
		t.Fatal("sling cannot resolve the local rig Beads database")
	}
}

func createLocalGitRepo(t *testing.T, repoDir string) string {
	t.Helper()
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	cmds := [][]string{
		{"git", "init", "--initial-branch=main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test User"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args[1:], err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# demo\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{{"git", "add", "."}, {"git", "commit", "-m", "init"}} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args[1:], err, out)
		}
	}
	if resolved, err := filepath.EvalSymlinks(repoDir); err == nil {
		return resolved
	}
	return repoDir
}
