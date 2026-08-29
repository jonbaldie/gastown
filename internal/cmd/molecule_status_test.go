package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jonbaldie/gastown/internal/beads"
)

func TestMoleculeCurrentPrefersLiveHookOverStaleHandoff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script bd stub not supported on Windows")
	}
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir Town beads: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	bdScript := `#!/bin/sh
while [ "$1" = "--allow-stale" ]; do shift; done
cmd="$1"; shift
case "$cmd" in
  list)
    if echo "$*" | grep -q 'status=pinned'; then
      echo '[{"id":"gt-handoff","title":"witness Handoff","status":"pinned","description":"attached_molecule: gt-stale"}]'
    elif echo "$*" | grep -q 'status=hooked.*assignee=gastown/witness'; then
      echo '[{"id":"gt-live-hook","title":"Live Hook","status":"hooked","assignee":"gastown/witness","description":"attached_molecule: gt-live"}]'
    else
      echo '[]'
    fi
    ;;
  query)
    if echo "$*" | grep -q 'status=hooked.*assignee=gastown/witness'; then
      echo '[{"id":"gt-live-hook","title":"Live Hook","status":"hooked","assignee":"gastown/witness","description":"attached_molecule: gt-live"}]'
    else
      echo '[]'
    fi
    ;;
  show)
    case "$1" in
      gt-live) echo '[{"id":"gt-live","title":"Live Molecule","status":"open"}]' ;;
      *) echo '[{"id":"gt-stale","title":"Stale Molecule","status":"open"}]' ;;
    esac
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BEADS_DIR", "")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir town root: %v", err)
	}

	previousJSON := moleculeState().json
	moleculeState().json = true
	t.Cleanup(func() { moleculeState().json = previousJSON })

	previousStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = previousStdout })

	err = runMoleculeCurrent(nil, []string{"gastown/witness"})
	_ = writer.Close()
	if err != nil {
		t.Fatalf("run molecule current: %v", err)
	}

	var output bytes.Buffer
	if _, err := io.Copy(&output, reader); err != nil {
		t.Fatalf("read molecule current output: %v", err)
	}
	var info MoleculeCurrentInfo
	if err := json.Unmarshal(output.Bytes(), &info); err != nil {
		t.Fatalf("parse molecule current output %q: %v", output.String(), err)
	}
	if info.MoleculeID != "gt-live" {
		t.Fatalf("current molecule = %q, want live Hook molecule %q (output: %s)", info.MoleculeID, "gt-live", output.String())
	}
	if info.Diagnosis == "" || !strings.Contains(info.Diagnosis, "gt-stale") || !strings.Contains(info.Diagnosis, "gt-live") {
		t.Fatalf("diagnosis = %q, want disagreement between stale Handoff gt-stale and live Hook gt-live", info.Diagnosis)
	}
}

func TestMoleculeCurrentDoesNotTreatHandoffAsLiveWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script bd stub not supported on Windows")
	}
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir Town beads: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	bdScript := `#!/bin/sh
while [ "$1" = "--allow-stale" ]; do shift; done
cmd="$1"; shift
case "$cmd" in
  list)
    if echo "$*" | grep -q 'status=pinned'; then
      echo '[{"id":"gt-handoff","title":"witness Handoff","status":"pinned","description":"attached_molecule: gt-resume"}]'
    else
      echo '[]'
    fi
    ;;
  query)
    echo '[]'
    ;;
  show)
    echo '[{"id":"gt-resume","title":"Resume Molecule","status":"open"}]'
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BEADS_DIR", "")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir town root: %v", err)
	}

	previousJSON := moleculeState().json
	moleculeState().json = true
	t.Cleanup(func() { moleculeState().json = previousJSON })

	previousStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = previousStdout })

	err = runMoleculeCurrent(nil, []string{"gastown/witness"})
	_ = writer.Close()
	if err != nil {
		t.Fatalf("run molecule current: %v", err)
	}

	var output bytes.Buffer
	if _, err := io.Copy(&output, reader); err != nil {
		t.Fatalf("read molecule current output: %v", err)
	}
	var info MoleculeCurrentInfo
	if err := json.Unmarshal(output.Bytes(), &info); err != nil {
		t.Fatalf("parse molecule current output %q: %v", output.String(), err)
	}
	if info.MoleculeID != "" || info.Status != "naked" {
		t.Fatalf("current molecule = %q, status = %q; stale Handoff must not become live work (output: %s)", info.MoleculeID, info.Status, output.String())
	}
	if !strings.Contains(info.Diagnosis, "gt-resume") || !strings.Contains(info.Diagnosis, "no live Hook") {
		t.Fatalf("diagnosis = %q, want stale Handoff diagnosis", info.Diagnosis)
	}
}

func TestOutputMoleculeStatus_StandaloneFormulaShowsVars(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir tempDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	status := MoleculeStatusInfo{
		HasWork:         true,
		PinnedBead:      &beads.Issue{ID: "gt-wisp-xyz", Title: "Standalone formula work"},
		AttachedFormula: "mol-release",
		AttachedVars:    []string{"version=1.2.3", "channel=stable"},
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputMoleculeStatus(status)

	w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = oldStdout
	output := buf.String()

	if !strings.Contains(output, "📐 Formula: mol-release") {
		t.Fatalf("expected formula in output, got:\n%s", output)
	}
	if !strings.Contains(output, "--var version=1.2.3") || !strings.Contains(output, "--var channel=stable") {
		t.Fatalf("expected formula vars in output, got:\n%s", output)
	}
}

func TestOutputMoleculeStatus_FormulaWispShowsWorkflowContext(t *testing.T) {
	status := MoleculeStatusInfo{
		HasWork:         true,
		PinnedBead:      &beads.Issue{ID: "tool-wisp-demo", Title: "demo-hello"},
		AttachedFormula: "demo-hello",
		Progress: &MoleculeProgressInfo{
			RootID:     "tool-wisp-demo",
			RootTitle:  "demo-hello",
			TotalSteps: 3,
			DoneSteps:  0,
			ReadySteps: []string{"tool-wisp-step-1"},
		},
		NextAction: "Show the workflow steps: gt prime or bd mol current tool-wisp-demo",
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputMoleculeStatus(status)

	w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = oldStdout
	output := buf.String()

	if !strings.Contains(output, "📐 Formula: demo-hello") {
		t.Fatalf("expected formula line in output, got:\n%s", output)
	}
	if strings.Contains(output, "No molecule attached") {
		t.Fatalf("formula wisp should not be rendered as naked work, got:\n%s", output)
	}
	if strings.Contains(output, "Attach a molecule to start work") {
		t.Fatalf("formula wisp should not suggest gt mol attach, got:\n%s", output)
	}
	if !strings.Contains(output, "Show the workflow steps: gt prime or bd mol current tool-wisp-demo") {
		t.Fatalf("expected workflow next action, got:\n%s", output)
	}
}
