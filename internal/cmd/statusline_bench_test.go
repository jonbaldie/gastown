package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jonbaldie/gastown/internal/tmux"
)

var statusLineBenchSocketCounter atomic.Int64

// hasTmuxForBench reports whether a tmux binary is available.
func hasTmuxForBench() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// setupStatusLineBenchSessions spins up a real, isolated tmux server (its
// own socket, so it can't collide with a user's session) with n long-lived
// sessions, each printing content a real agent pane would show. This
// exercises the real `tmux capture-pane` subprocess path that
// isSessionWorking relies on in production.
func setupStatusLineBenchSessions(b *testing.B, n int) (*tmux.Tmux, []string) {
	b.Helper()
	if !hasTmuxForBench() {
		b.Skip("tmux not installed")
	}

	socket := fmt.Sprintf("gt-bench-statusline-%d-%d", os.Getpid(), statusLineBenchSocketCounter.Add(1))
	t := tmux.NewTmuxWithSocket(socket)

	sessions := make([]string, n)
	for i := range n {
		name := fmt.Sprintf("bench-sess-%d", i)
		sessions[i] = name
		// Half the panes show the "working" indicator, half don't — mirrors
		// a real town with a mix of busy and idle agents.
		cmd := "printf 'idle prompt\\n> \\n'; sleep 300"
		if i%2 == 0 {
			cmd = "printf 'working ✻ Pondering...\\n'; sleep 300"
		}
		if err := t.NewSessionWithCommand(name, "", cmd); err != nil {
			b.Fatalf("NewSessionWithCommand(%s): %v", name, err)
		}
	}

	b.Cleanup(func() {
		_ = t.KillServer()
	})

	return t, sessions
}

// checkWorkingSerial mirrors the pre-fix behavior: one tmux capture-pane
// subprocess call at a time.
func checkWorkingSerial(t *tmux.Tmux, sessions []string) int {
	working := 0
	for _, s := range sessions {
		if isSessionWorking(t, s) {
			working++
		}
	}
	return working
}

// checkWorkingConcurrent mirrors the post-fix behavior: bounded fan-out
// across sessions.
func checkWorkingConcurrent(t *tmux.Tmux, sessions []string) int {
	sem := make(chan struct{}, maxConcurrentWorkingChecks)
	var wg sync.WaitGroup
	var mu sync.Mutex
	working := 0
	for _, s := range sessions {
		wg.Add(1)
		sem <- struct{}{}
		go func(s string) {
			defer wg.Done()
			defer func() { <-sem }()
			if isSessionWorking(t, s) {
				mu.Lock()
				working++
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	return working
}

func BenchmarkStatusLineWorkingChecksSerial(b *testing.B) {
	t, sessions := setupStatusLineBenchSessions(b, 16)
	b.ResetTimer()
	for range b.N {
		checkWorkingSerial(t, sessions)
	}
}

func BenchmarkStatusLineWorkingChecksConcurrent(b *testing.B) {
	t, sessions := setupStatusLineBenchSessions(b, 16)
	b.ResetTimer()
	for range b.N {
		checkWorkingConcurrent(t, sessions)
	}
}

// TestStatusLineWorkingChecksConcurrentMatchesSerial proves the concurrent
// fan-out (production code) counts the same working sessions as the
// original serial loop, for the same real tmux panes.
func TestStatusLineWorkingChecksConcurrentMatchesSerial(t *testing.T) {
	if !hasTmuxForBench() {
		t.Skip("tmux not installed")
	}
	socket := fmt.Sprintf("gt-test-statusline-%d-%d", os.Getpid(), statusLineBenchSocketCounter.Add(1))
	tm := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = tm.KillServer() })

	const n = 12
	sessions := make([]string, n)
	for i := range n {
		name := fmt.Sprintf("test-sess-%d", i)
		sessions[i] = name
		cmd := "printf 'idle prompt\\n> \\n'; sleep 300"
		if i%3 == 0 {
			cmd = "printf 'working ✻ Pondering...\\n'; sleep 300"
		}
		if err := tm.NewSessionWithCommand(name, "", cmd); err != nil {
			t.Fatalf("NewSessionWithCommand(%s): %v", name, err)
		}
	}

	serialCount := checkWorkingSerial(tm, sessions)
	concurrentCount := checkWorkingConcurrent(tm, sessions)

	if serialCount == 0 {
		t.Fatal("expected at least one working session in fixture")
	}
	if serialCount != concurrentCount {
		t.Errorf("concurrent count %d != serial count %d", concurrentCount, serialCount)
	}
}
