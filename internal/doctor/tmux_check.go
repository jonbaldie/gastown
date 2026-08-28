package doctor

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// LinkedPaneCheck detects tmux sessions that share panes,
// which can cause crosstalk (messages sent to one session appearing in another).
type LinkedPaneCheck struct {
	FixableCheck
	linkedSessions []string // Sessions with linked panes, cached for Fix
}

// NewLinkedPaneCheck creates a new linked pane check.
func NewLinkedPaneCheck() *LinkedPaneCheck {
	return &LinkedPaneCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "linked-panes",
				CheckDescription: "Detect tmux sessions sharing panes (causes crosstalk)",
				CheckCategory:    CategoryInfrastructure,
			},
		},
	}
}

// Run checks for linked panes across Gas Town tmux sessions.
func (c *LinkedPaneCheck) Run(_ *CheckContext) *CheckResult {
	t := tmux.NewTmux()

	sessions, err := t.ListSessions()
	if err != nil {
		return linkedPaneListError(c, err)
	}

	gtSessions := knownSessions(sessions)

	if len(gtSessions) < 2 {
		return linkedPaneNotEnoughResult(c)
	}

	paneToSessions := c.collectPaneSessions(gtSessions)
	conflicts, linkedSessionSet := linkedPaneConflicts(paneToSessions)
	c.cacheLinkedSessions(linkedSessionSet)
	return c.linkedPaneResult(gtSessions, conflicts)
}

func linkedPaneListError(c *LinkedPaneCheck, err error) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: "Could not list tmux sessions",
		Details: []string{err.Error()},
	}
}

func linkedPaneNotEnoughResult(c *LinkedPaneCheck) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "Not enough sessions to check for linking",
	}
}

func knownSessions(sessions []string) []string {
	var known []string
	for _, name := range sessions {
		if session.IsKnownSession(name) {
			known = append(known, name)
		}
	}
	return known
}

func (c *LinkedPaneCheck) collectPaneSessions(sessions []string) map[string][]string {
	paneToSessions := make(map[string][]string)
	for _, name := range sessions {
		panes, err := c.getSessionPanes(name)
		if err != nil {
			continue
		}
		for _, pane := range panes {
			paneToSessions[pane] = append(paneToSessions[pane], name)
		}
	}
	return paneToSessions
}

func linkedPaneConflicts(paneToSessions map[string][]string) ([]string, map[string]bool) {
	var conflicts []string
	linkedSessionSet := make(map[string]bool)
	for pane, sessions := range paneToSessions {
		if len(sessions) <= 1 {
			continue
		}
		conflicts = append(conflicts, fmt.Sprintf("Pane %s shared by: %s", pane, strings.Join(sessions, ", ")))
		for _, name := range sessions {
			linkedSessionSet[name] = true
		}
	}
	return conflicts, linkedSessionSet
}

func (c *LinkedPaneCheck) cacheLinkedSessions(linkedSessionSet map[string]bool) {
	mayorSession := session.MayorSessionName()
	c.linkedSessions = nil
	for name := range linkedSessionSet {
		if mayorSession == "" || name != mayorSession {
			c.linkedSessions = append(c.linkedSessions, name)
		}
	}
}

func (c *LinkedPaneCheck) linkedPaneResult(sessions, conflicts []string) *CheckResult {
	if len(conflicts) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d Gas Town sessions have independent panes", len(sessions)),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("Found %d linked pane(s) causing crosstalk!", len(conflicts)),
		Details: conflicts,
		FixHint: "Run 'gt doctor --fix' to kill linked sessions (daemon will recreate)",
	}
}

// Fix kills sessions with linked panes (except mayor session).
// The daemon will recreate them with independent panes.
func (c *LinkedPaneCheck) Fix(_ *CheckContext) error {
	if len(c.linkedSessions) == 0 {
		return nil
	}

	t := tmux.NewTmux()
	var lastErr error

	for _, session := range c.linkedSessions {
		// Use KillSessionWithProcesses to ensure all descendant processes are killed.
		if err := t.KillSessionWithProcesses(session); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

// getSessionPanes returns all pane IDs for a session.
func (c *LinkedPaneCheck) getSessionPanes(session string) ([]string, error) {
	// Get pane IDs using tmux list-panes with format
	// Using #{pane_id} which gives us the unique pane identifier like %123
	// Note: -s flag lists all panes in all windows of this session (not -a which is global)
	out, err := tmux.BuildCommand("list-panes", "-t", session, "-s", "-F", "#{pane_id}").Output()
	if err != nil {
		return nil, err
	}

	var panes []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			panes = append(panes, line)
		}
	}

	return panes, nil
}
