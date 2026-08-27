package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

func setupCmdTestRegistry(t *testing.T) {
	t.Helper()
	registry := session.NewPrefixRegistry()
	registry.Register("gt", "gastown")
	registry.Register("do", "coder_dotfiles")
	registry.Register("mr", "myrig")
	old := session.DefaultRegistry()
	session.SetDefaultRegistry(registry)
	t.Cleanup(func() { session.SetDefaultRegistry(old) })
}

func TestStatusLineAvoidsBeadsHotPath(t *testing.T) {
	if !beadsExemptCommands["status-line"] {
		t.Fatal("status-line must be exempt from bd version checks")
	}
	if !branchCheckExemptCommands["status-line"] {
		t.Fatal("status-line must be exempt from git branch/stale checks")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "statusline.go"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, forbidden := range []string{
		`internal/beads`,
		`internal/mail`,
		`beads.New`,
		`mail.New`,
		`getHookedWork`,
		`getMailPreview`,
		`ListUnread`,
		`getRefineryManager`,
		`.Queue()`,
	} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("status-line hot path must not contain %q", forbidden)
		}
	}
}

func TestRunStatusLineReadsSessionEnvironment(t *testing.T) {
	if !hasTmuxForBench() {
		t.Skip("tmux not installed")
	}

	socket := fmt.Sprintf("gt-test-statusline-env-%d-%d", os.Getpid(), statusLineBenchSocketCounter.Add(1))
	tm := tmux.NewTmuxWithSocket(socket)
	const sessionName = "statusline-env-worker"
	if err := tm.NewSessionWithCommand(sessionName, "", "sleep 300"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillServer() })

	oldSocket := tmux.GetDefaultSocket()
	oldSession := statusLineSession
	tmux.SetDefaultSocket(socket)
	statusLineSession = sessionName
	t.Cleanup(func() {
		tmux.SetDefaultSocket(oldSocket)
		statusLineSession = oldSession
	})

	// Conflicting process values prove the named session, not the fallback
	// environment, supplies the rendered identity and work item.
	t.Setenv("GT_POLECAT", "process-polecat")
	t.Setenv("GT_CREW", "process-crew")
	t.Setenv("GT_ISSUE", "process-issue")
	t.Setenv("GT_ROLE", "process-role")
	t.Setenv("GT_RIG", "process-rig")

	capture := func() string {
		t.Helper()
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		oldStdout := os.Stdout
		os.Stdout = writer
		runErr := runStatusLine(statusLineCmd, nil)
		_ = writer.Close()
		os.Stdout = oldStdout
		output, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if runErr != nil {
			t.Fatalf("runStatusLine: %v", runErr)
		}
		if readErr != nil {
			t.Fatalf("read stdout: %v", readErr)
		}
		return string(output)
	}

	setSessionEnv := func(values map[string]string) {
		t.Helper()
		for _, key := range []string{"GT_RIG", "GT_POLECAT", "GT_CREW", "GT_ISSUE", "GT_ROLE"} {
			if err := tm.SetEnvironment(sessionName, key, values[key]); err != nil {
				t.Fatalf("SetEnvironment(%s): %v", key, err)
			}
		}
	}

	setSessionEnv(map[string]string{"GT_POLECAT": "toast", "GT_ISSUE": "gt-fast"})
	if got, want := capture(), "😺 gt-fast |"; got != want {
		t.Fatalf("polecat status = %q, want %q", got, want)
	}

	setSessionEnv(map[string]string{"GT_CREW": "alice", "GT_ISSUE": "gt-steady"})
	if got, want := capture(), "👷 gt-steady |"; got != want {
		t.Fatalf("crew status = %q, want %q", got, want)
	}

	setSessionEnv(map[string]string{"GT_RIG": "gastown", "GT_ROLE": "refinery"})
	if got, want := capture(), "idle |"; got != want {
		t.Fatalf("refinery status = %q, want %q", got, want)
	}
}

func TestSchedulerRunAvoidsRootBeadsChecks(t *testing.T) {
	if !beadsExemptCommands["scheduler"] {
		t.Fatal("scheduler must be exempt from root bd version checks")
	}
	if !branchCheckExemptCommands["scheduler"] {
		t.Fatal("scheduler must be exempt from root git branch checks")
	}
	if !isCommandOrAncestorExempt(schedulerRunCmd, beadsExemptCommands) {
		t.Fatal("scheduler run must inherit bd exemption from scheduler parent")
	}
	if !isCommandOrAncestorExempt(schedulerRunCmd, branchCheckExemptCommands) {
		t.Fatal("scheduler run must inherit branch-check exemption from scheduler parent")
	}
}
