package rig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initGitWorktree(t *testing.T, dir string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init", "-q", dir},
		{"git", "-C", dir, "config", "user.email", "test@example.com"},
		{"git", "-C", dir, "config", "user.name", "test"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func TestProvision_WritesLocalExcludeNotGitignore(t *testing.T) {
	town := t.TempDir()
	rigDir := filepath.Join(town, "rigs", "wyvern")
	workDir := filepath.Join(rigDir, "polecats", "toast")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	initGitWorktree(t, workDir)

	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := Provision(rigDir, workDir, "polecat"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workDir, ".gitignore")); err == nil {
		t.Fatal("Provision must not write a tracked .gitignore")
	}

	exclude := filepath.Join(workDir, ".git", "info", "exclude")
	data, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatalf("expected local exclude: %v", err)
	}
	if !strings.Contains(string(data), ".runtime/") {
		t.Fatalf("exclude missing .runtime/: %s", data)
	}
}

func TestProvision_RunsSetupHooksAndCopiesOverlay(t *testing.T) {
	town := t.TempDir()
	rigDir := filepath.Join(town, "rigs", "wyvern")
	workDir := filepath.Join(rigDir, "crew", "toast")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	initGitWorktree(t, workDir)

	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}

	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "from-overlay"), []byte("copied"), 0644); err != nil {
		t.Fatal(err)
	}

	hooksDir := filepath.Join(rigDir, ".runtime", "setup-hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hooksDir, "01-mark.sh")
	script := "#!/bin/sh\ntouch hook-ran\n"
	if err := os.WriteFile(hook, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	if err := Provision(rigDir, workDir, "crew"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(workDir, "from-overlay"))
	if err != nil {
		t.Fatalf("overlay file missing: %v", err)
	}
	if string(got) != "copied" {
		t.Fatalf("overlay content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(workDir, "hook-ran")); err != nil {
		t.Fatalf("setup hook did not run: %v", err)
	}
}

func TestProvision_MissingBeadsStillCompletes(t *testing.T) {
	town := t.TempDir()
	rigDir := filepath.Join(town, "wyvern")
	workDir := filepath.Join(rigDir, "polecats", "toast")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	initGitWorktree(t, workDir)

	if err := Provision(rigDir, workDir, "polecat"); err != nil {
		t.Fatalf("Provision without beads: %v", err)
	}
	exclude := filepath.Join(workDir, ".git", "info", "exclude")
	if _, err := os.Stat(exclude); err != nil {
		t.Fatalf("expected local exclude after missing-beads provision: %v", err)
	}
}

func TestProvision_RigAsTownRedirectsPrimeMD(t *testing.T) {
	rigDir := t.TempDir()
	mayorBeads := filepath.Join(rigDir, "mayor", "rig", ".beads")
	workDir := filepath.Join(rigDir, "polecats", "toast", filepath.Base(rigDir))
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mayorBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	initGitWorktree(t, workDir)

	if err := Provision(rigDir, workDir, "polecat"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workDir, ".beads", "PRIME.md")); err == nil {
		t.Fatal("PRIME.md must not be created in the worktree")
	}
	if _, err := os.Stat(filepath.Join(workDir, ".beads", "redirect")); err != nil {
		t.Fatalf("worktree redirect missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mayorBeads, "PRIME.md")); err != nil {
		t.Fatalf("PRIME.md missing at mayor/rig/.beads: %v", err)
	}
}

func TestProvision_NestedTownRedirectsPrimeMD(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(`{"name":"test-town"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(town, "wyvern")
	mayorBeads := filepath.Join(rigDir, "mayor", "rig", ".beads")
	workDir := filepath.Join(rigDir, "polecats", "toast", "wyvern")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mayorBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	initGitWorktree(t, workDir)

	if err := Provision(rigDir, workDir, "polecat"); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workDir, ".beads", "PRIME.md")); err == nil {
		t.Fatal("PRIME.md must not be created in the worktree")
	}
	if _, err := os.Stat(filepath.Join(workDir, ".beads", "redirect")); err != nil {
		t.Fatalf("worktree redirect missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mayorBeads, "PRIME.md")); err != nil {
		t.Fatalf("PRIME.md missing at mayor/rig/.beads: %v", err)
	}
}
