package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonbaldie/gastown/internal/nudge"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/tmux/tmuxtest"
	"github.com/jonbaldie/gastown/internal/worker"
)

func TestShouldFallBackFromWorkerNudge(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "unknown worker run falls back to live session",
			err:  worker.ErrUnknownState,
			want: true,
		},
		{
			name: "unavailable worker falls back to live session",
			err:  worker.ErrServerDown,
			want: true,
		},
		{
			name: "serialized unknown worker run falls back to live session",
			err:  errors.New("worker: unknown state: hq-mayor"),
			want: true,
		},
		{
			name: "worker delivery failure remains visible",
			err:  errors.New("delivery rejected"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldFallBackFromWorkerNudge(tt.err); got != tt.want {
				t.Fatalf("shouldFallBackFromWorkerNudge(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestDeliverNudgeFallsBackToTmuxWhenWorkerHasNoRun(t *testing.T) {
	realTmux := tmuxtest.RealTmuxOrSkip(t)
	townRoot, err := os.MkdirTemp("", "gt-nudge-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(townRoot) })
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(townRoot)
	t.Setenv("GT_TEST_NUDGE_LOG", "")

	server, err := worker.Listen(townRoot, worker.NewTmuxAdapter())
	if err != nil {
		t.Fatalf("starting worker: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	socket := tmuxtest.SocketName(t, "nudge-worker-fallback-")
	const sessionName = "hq-mayor"
	tmuxtest.StartSession(t, realTmux, socket, sessionName, "exec cat")

	origMode := nudgeModeFlag
	origPriority := nudgePriorityFlag
	t.Cleanup(func() {
		nudgeModeFlag = origMode
		nudgePriorityFlag = origPriority
	})
	nudgeModeFlag = NudgeModeImmediate
	nudgePriorityFlag = nudge.PriorityUrgent

	const message = "worker-fallback-delivered"
	tm := tmux.NewTmuxWithSocketAndBinary(socket, realTmux)
	if err := deliverNudge(tm, sessionName, message, "test"); err != nil {
		t.Fatalf("deliverNudge() = %v, want Worker unknown-state fallback to tmux", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command(realTmux, "-L", socket, "capture-pane", "-p", "-t", sessionName).Output()
		if err != nil {
			t.Fatalf("capturing tmux pane: %v", err)
		}
		if strings.Contains(string(out), message) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("nudge was not delivered to the live tmux session after Worker reported unknown state")
}
