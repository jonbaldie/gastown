package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

func TestNewBlockedPaneCheck(t *testing.T) {
	check := NewBlockedPaneCheck()
	if check.Name() != "blocked-panes" {
		t.Fatalf("name = %q", check.Name())
	}
	if check.Category() != CategoryInfrastructure {
		t.Fatalf("category = %q", check.Category())
	}
}

func TestBlockedPaneCheck_ReportsModelPicker(t *testing.T) {
	if testing.Short() {
		t.Skip("starts tmux sessions")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	socket := fmt.Sprintf("gt-blockpane-%d", time.Now().UnixNano())
	prev := tmux.GetDefaultSocket()
	tmux.SetDefaultSocket(socket)
	t.Cleanup(func() { tmux.SetDefaultSocket(prev) })

	old := session.DefaultRegistry()
	session.SetDefaultRegistry(session.NewPrefixRegistry())
	t.Cleanup(func() { session.SetDefaultRegistry(old) })

	tm := tmux.NewTmux()
	t.Cleanup(func() { _ = tm.KillServer() })

	picker := "Select model\n❯ Default (recommended)\n  Opus\n  Sonnet\n  Haiku\n\n  Enter to confirm · Esc to exit\n"
	script := filepath.Join(t.TempDir(), "picker.sh")
	body := "#!/bin/sh\nprintf '%s' " + strconv.Quote(picker) + "\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tm.NewSessionWithCommand("hq-mayor", "", script); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var content string
	for {
		var capErr error
		content, capErr = tm.CapturePane("hq-mayor", 80)
		if capErr == nil && strings.Contains(content, "Select model") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane never showed model picker: %v\n%s", capErr, content)
		}
		time.Sleep(20 * time.Millisecond)
	}

	result := NewBlockedPaneCheck().Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusWarning {
		t.Fatalf("status = %s (%s), want Warning for a /model picker", result.Status, result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "hq-mayor") || !strings.Contains(strings.ToLower(joined), "model") {
		t.Fatalf("details should name hq-mayor model picker, got %q (%s)", joined, result.Message)
	}
}
