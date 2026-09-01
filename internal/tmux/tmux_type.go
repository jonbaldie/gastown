package tmux

import (
	"os"
	"sync"
)

// Tmux wraps tmux operations.
type Tmux struct {
	socketName   string // tmux socket name (-L flag), empty = default socket
	binary       string // optional tmux executable override; empty means "tmux"
	CapsOnce     sync.Once
	Caps         Capabilities
	CapsOverride *Capabilities
}

// NewTmux creates a new Tmux wrapper using the initialized town socket.
// Falls back to GT_TOWN_SOCKET env var (set by cross-socket tmux bindings).
// Empty socket means use the default tmux server.
func NewTmux() *Tmux {
	sock := GetDefaultSocket()
	if sock == "" {
		// GT_TOWN_SOCKET is embedded in tmux bindings created by EnsureBindingsOnSocket
		// so that "gt agents menu" / "gt feed" invoked from a personal terminal still
		// target the correct town server even when InitRegistry was not called.
		sock = os.Getenv("GT_TOWN_SOCKET")
	}
	return &Tmux{socketName: sock}
}

// NewTmuxWithSocket creates a Tmux wrapper that targets a named socket.
// This creates/connects to an isolated tmux server, separate from the user's
// default server. Primarily used in tests to prevent session name collisions
// and keystroke leaks (e.g. Escape from NudgeSession hitting the user's prefix table).
func NewTmuxWithSocket(socket string) *Tmux {
	return &Tmux{socketName: socket}
}

// NewTmuxWithSocketAndBinary is like NewTmuxWithSocket but invokes a specific
// tmux executable. Tests use this to wrap the host tmux without mutating PATH.
func NewTmuxWithSocketAndBinary(socket, binary string) *Tmux {
	return &Tmux{socketName: socket, binary: binary}
}
