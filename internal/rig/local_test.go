package rig

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/git"
)

func TestAddLocalRigCreatesBeadsDatabase(t *testing.T) {
	root, rigsConfig := setupLocalRigTown(t)
	script := `#!/usr/bin/env bash
set -e
if [[ "$1" == "init" ]]; then
  mkdir -p .beads
  exit 0
fi
exit 0
`
	binDir := writeFakeBD(t, script, "@echo off\r\nexit /b 0\r\n")

	repo := createLocalGitRepo(t, filepath.Join(t.TempDir(), "proj"))
	manager := NewManager(root, rigsConfig, git.NewGit(root))
	if _, err := manager.AddLocalRig(context.Background(), "proj", repo); err != nil {
		t.Fatalf("AddLocalRig: %v", err)
	}
	t.Setenv("PATH", binDir)
	if err := manager.InitLocalRigBeads("proj"); err != nil {
		t.Fatalf("InitLocalRigBeads: %v", err)
	}

	rigBeads := filepath.Join(root, "proj", ".beads")
	if _, err := os.Stat(rigBeads); err != nil {
		t.Fatalf("local rig has no Beads database: %v", err)
	}
	if _, ok := beads.ResolveRepoAliasBeadsDir(root, "proj"); !ok {
		t.Fatal("sling cannot resolve the local rig Beads database")
	}
}

func TestInitLocalRigBeadsRejectsEmptyName(t *testing.T) {
	root, rigsConfig := setupLocalRigTown(t)
	manager := NewManager(root, rigsConfig, git.NewGit(root))
	if err := manager.InitLocalRigBeads("  "); err == nil {
		t.Fatal("InitLocalRigBeads accepted an empty name")
	}
}

func setupLocalRigTown(t *testing.T) (string, *config.RigsConfig) {
	t.Helper()
	root, rigsConfig := setupTestTown(t)
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := config.SaveRigsConfig(filepath.Join(root, "mayor", "rigs.json"), rigsConfig); err != nil {
		t.Fatalf("save rigs.json: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir town beads: %v", err)
	}
	return root, rigsConfig
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
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# proj\n"), 0644); err != nil {
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
