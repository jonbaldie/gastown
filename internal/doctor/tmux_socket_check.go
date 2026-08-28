package doctor

import (
	"fmt"
	"sort"

	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// socketSessionLister is the minimal interface needed to list and kill sessions
// on a specific tmux socket. Allows injecting mocks in tests.
type socketSessionLister interface {
	ListSessions() ([]string, error)
	KillSessionWithProcesses(_ string) error
}

// SocketSplitBrainCheck detects tmux sessions that exist on both the town
// socket (e.g., "gt-a1b2c3") and the "default" socket. This split-brain causes
// gt nudge and other session-discovery commands to fail because they only
// search the town socket.
type SocketSplitBrainCheck struct {
	FixableCheck
	staleSessions []string // Sessions on "default" that also exist on town socket

	townListerForTest    socketSessionLister // nil → real tmux on town socket
	defaultListerForTest socketSessionLister // nil → real tmux on "default" socket
	socketForTest        string              // override for tmux.GetDefaultSocket()
	useSocketForTest     bool                // distinguishes empty override from unset
}

// NewSocketSplitBrainCheck creates a new socket split-brain check.
func NewSocketSplitBrainCheck() *SocketSplitBrainCheck {
	return &SocketSplitBrainCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "socket-split-brain",
				CheckDescription: "Detect tmux sessions on wrong socket (causes nudge failures)",
				CheckCategory:    CategoryInfrastructure,
			},
		},
	}
}

// Run checks for Gas Town sessions on the "default" socket that duplicate
// sessions on the town socket.
func (c *SocketSplitBrainCheck) Run(_ *CheckContext) *CheckResult {
	townSocket := c.townSocket()
	if townSocket == "" || townSocket == "default" {
		return socketSplitBrainDefaultResult(c)
	}

	townLister, defaultLister := c.socketListers()

	townSessions, err := townLister.ListSessions()
	if err != nil {
		return socketSplitBrainTownErrorResult(c)
	}

	defaultSessions, err := defaultLister.ListSessions()
	if err != nil {
		return socketSplitBrainDefaultErrorResult(c)
	}

	duplicates, orphans := splitBrainSessions(townSessions, defaultSessions)

	c.staleSessions = append(duplicates, orphans...)
	sort.Strings(c.staleSessions)
	return c.socketSplitBrainResult(townSocket, duplicates, orphans)
}

func (c *SocketSplitBrainCheck) townSocket() string {
	if c.useSocketForTest {
		return c.socketForTest
	}
	return tmux.GetDefaultSocket()
}

func (c *SocketSplitBrainCheck) socketListers() (socketSessionLister, socketSessionLister) {
	townLister := socketSessionLister(tmux.NewTmux())
	if c.townListerForTest != nil {
		townLister = c.townListerForTest
	}
	defaultLister := socketSessionLister(tmux.NewTmuxWithSocket("default"))
	if c.defaultListerForTest != nil {
		defaultLister = c.defaultListerForTest
	}
	return townLister, defaultLister
}

func socketSplitBrainDefaultResult(c *SocketSplitBrainCheck) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "Town socket is default — no split-brain possible",
	}
}

func socketSplitBrainTownErrorResult(c *SocketSplitBrainCheck) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "Could not list town socket sessions (server may not be running)",
	}
}

func socketSplitBrainDefaultErrorResult(c *SocketSplitBrainCheck) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "No default socket server running — no split-brain",
	}
}

func splitBrainSessions(townSessions, defaultSessions []string) ([]string, []string) {
	townSet := make(map[string]bool, len(townSessions))
	for _, name := range townSessions {
		townSet[name] = true
	}
	var duplicates []string
	var orphans []string
	for _, name := range defaultSessions {
		if !session.IsKnownSession(name) {
			continue
		}
		if townSet[name] {
			duplicates = append(duplicates, name)
			continue
		}
		orphans = append(orphans, name)
	}
	return duplicates, orphans
}

func (c *SocketSplitBrainCheck) socketSplitBrainResult(townSocket string, duplicates, orphans []string) *CheckResult {
	if len(c.staleSessions) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("No split-brain: all Gas Town sessions on %q socket", townSocket),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("Found %d Gas Town session(s) on wrong socket — nudge/discovery will fail", len(c.staleSessions)),
		Details: socketSplitBrainDetails(townSocket, duplicates, orphans),
		FixHint: "Run 'gt doctor --fix' to kill stale sessions on wrong socket",
	}
}

func socketSplitBrainDetails(townSocket string, duplicates, orphans []string) []string {
	var details []string
	for _, name := range duplicates {
		details = append(details, fmt.Sprintf("DUPLICATE: %s exists on both %q and \"default\" sockets", name, townSocket))
	}
	for _, name := range orphans {
		details = append(details, fmt.Sprintf("ORPHAN: %s only on \"default\" socket (should be on %q)", name, townSocket))
	}
	return details
}

// Fix kills Gas Town sessions on the "default" socket that shouldn't be there.
func (c *SocketSplitBrainCheck) Fix(_ *CheckContext) error {
	if len(c.staleSessions) == 0 {
		return nil
	}

	var defaultLister socketSessionLister = tmux.NewTmuxWithSocket("default")
	if c.defaultListerForTest != nil {
		defaultLister = c.defaultListerForTest
	}
	var lastErr error

	for _, s := range c.staleSessions {
		if err := defaultLister.KillSessionWithProcesses(s); err != nil {
			lastErr = err
		}
	}

	return lastErr
}
