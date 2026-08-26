package rig

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInspectTrackedBeadsImportCountsJSONLAndExecutableHooks(t *testing.T) {
	root := t.TempDir()
	beadsDir := filepath.Join(root, ".beads")
	hooksDir := filepath.Join(root, ".git", "hooks")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir beads: %v", err)
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	jsonl := `{"id":"gt-1","title":"one"}
{"id":"gt-2","title":"two"}

# comment
`
	if err := os.WriteFile(filepath.Join(beadsDir, "issues.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	hookPath := filepath.Join(hooksDir, "post-checkout")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-checkout.sample"), []byte("sample"), 0o755); err != nil {
		t.Fatalf("write sample hook: %v", err)
	}

	imp, err := InspectTrackedBeadsImport(root)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if imp.BeadCount != 2 {
		t.Fatalf("bead count = %d, want 2", imp.BeadCount)
	}
	if len(imp.JSONLFiles) != 1 || imp.JSONLFiles[0] != ".beads/issues.jsonl" {
		t.Fatalf("jsonl files = %v, want [.beads/issues.jsonl]", imp.JSONLFiles)
	}
	if runtime.GOOS != "windows" && (len(imp.ExecutableHooks) != 1 || imp.ExecutableHooks[0] != ".git/hooks/post-checkout") {
		t.Fatalf("executable hooks = %v, want [.git/hooks/post-checkout]", imp.ExecutableHooks)
	}
}

func TestTrackedBeadsImportRequiresConsent(t *testing.T) {
	imp := TrackedBeadsImport{
		Source:          "/tmp/mayor/rig",
		BeadCount:       3,
		JSONLFiles:      []string{".beads/issues.jsonl"},
		ExecutableHooks: []string{".git/hooks/post-checkout"},
	}
	err := RequireTrackedBeadsImportConsent(imp, false)
	if !errors.Is(err, ErrTrackedBeadsImportConsent) {
		t.Fatalf("err = %v, want ErrTrackedBeadsImportConsent", err)
	}
	if err == nil || !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "/tmp/mayor/rig") || !strings.Contains(err.Error(), ".beads/issues.jsonl") {
		t.Fatalf("consent error must report source and bead count, got %v", err)
	}
	if err := RequireTrackedBeadsImportConsent(imp, true); err != nil {
		t.Fatalf("explicit consent must allow import: %v", err)
	}
}

func TestTrackedBeadsImportAllowsNormalRig(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".beads", "config.yaml"), []byte("prefix: gt\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	imp, err := InspectTrackedBeadsImport(root)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if imp.RequiresConsent() {
		t.Fatalf("normal rig import = %+v, want no consent", imp)
	}
	if err := RequireTrackedBeadsImportConsent(imp, false); err != nil {
		t.Fatalf("normal rig must not require consent: %v", err)
	}
}

func TestInspectTrackedBeadsImportRequiresConsentForHookWithoutBeads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve POSIX executable bits")
	}
	root := t.TempDir()
	hooksDir := filepath.Join(root, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-commit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	imp, err := InspectTrackedBeadsImport(root)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !imp.RequiresConsent() || len(imp.ExecutableHooks) != 1 {
		t.Fatalf("hook-only import = %+v, want consent for the executable hook", imp)
	}
}
