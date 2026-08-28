package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubTmux satisfies TmuxSession without a tmux server. Listen only stores the
// adapter, so no method is called by these tests.
type stubTmux struct{}

func (stubTmux) HasSession(string) (bool, error)                 { return false, nil }
func (stubTmux) NudgeSession(string, string) error               { return nil }
func (stubTmux) WaitForRuntimeReady(string, time.Duration) error { return nil }
func (stubTmux) WaitForIdle(string, time.Duration) error         { return nil }
func (stubTmux) IsAgentAlive(string) bool                        { return false }
func (stubTmux) KillSessionWithProcesses(string) error           { return nil }
func (stubTmux) CheckSessionHealth(string, time.Duration) string { return "" }

// deepTownRoot builds a town root long enough that the worker socket path
// exceeds the operating system limit for a Unix socket address, which is about
// 100 bytes on every supported platform.
func deepTownRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// SocketPath appends the runtime dir and the socket file name to the town
	// root, so pad the root until the whole path is well past the limit.
	for len(SocketPath(root)) < 160 {
		root = filepath.Join(root, strings.Repeat("d", 24))
	}
	if err := os.MkdirAll(filepath.Join(root, "worker"), runtimeMode); err != nil {
		t.Fatalf("creating deep town root: %v", err)
	}
	return root
}

// TestListenSucceedsWhenSocketPathIsTooLong locks down the degradation path.
//
// The socket path is derived from the town root, and a town installed under a
// deep directory produces a path the kernel cannot bind. Listen used to return
// that bind error, so the whole Worker was unavailable even though the loopback
// listener it binds a few lines later would have served the same routes. A
// nested worktree or a long home directory was enough to trigger it.
func TestListenSucceedsWhenSocketPathIsTooLong(t *testing.T) {
	root := deepTownRoot(t)
	store := newStore(root)
	if err := store.ensure(); err != nil {
		t.Fatalf("ensure store: %v", err)
	}

	w, err := Listen(root, stubTmux{})
	if err != nil {
		t.Fatalf("Listen with a %d byte socket path: %v", len(SocketPath(root)), err)
	}
	defer func() { _ = w.Close() }()

	if w.local.unixActive() {
		t.Fatalf("expected the Unix socket to be unavailable at %d bytes", len(SocketPath(root)))
	}

	// The returned Worker must be usable, not merely constructed.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.client.Ping(ctx); err != nil {
		t.Fatalf("ping over the loopback fallback: %v", err)
	}

	wantAddress := fmt.Sprintf("127.0.0.1:%d", w.local.port)
	if network, address := w.Endpoint(); network != "tcp" || address != wantAddress {
		t.Errorf("Endpoint() = %q %q, want %q %q", network, address, "tcp", wantAddress)
	}
}

// shortTownRoot returns a town root whose socket path fits in a Unix socket
// address.
//
// t.TempDir() cannot be used for this: on macOS it returns a path under
// /var/folders that is already close to the limit, so a test built on it skips
// on the very platform where the bug was found. /tmp is short on every system
// this runs on.
func shortTownRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "gtworker")
	if err != nil {
		t.Skipf("no short temp directory available: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if n := len(SocketPath(root)); n >= 100 {
		t.Fatalf("short town root gives a %d byte socket path, so the test proves nothing", n)
	}
	return root
}

// TestListenUsesUnixSocketWhenPathFits guards the fix against the opposite
// mistake: a short town root must still get the preferred transport.
func TestListenUsesUnixSocketWhenPathFits(t *testing.T) {
	root := shortTownRoot(t)
	store := newStore(root)
	if err := store.ensure(); err != nil {
		t.Fatalf("ensure store: %v", err)
	}

	w, err := Listen(root, stubTmux{})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = w.Close() }()

	if !w.local.unixActive() {
		t.Fatal("expected the Unix socket to be bound for a short town root")
	}
	if network, address := w.Endpoint(); network != "unix" || address != SocketPath(root) {
		t.Errorf("Endpoint() = %q %q, want %q %q", network, address, "unix", SocketPath(root))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.client.Ping(ctx); err != nil {
		t.Fatalf("ping over the Unix socket: %v", err)
	}
}
