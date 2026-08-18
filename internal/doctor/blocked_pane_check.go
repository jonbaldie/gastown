package doctor

import (
	"fmt"

	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// BlockedPaneCheck detects agent panes stuck on an interactive dialog
// such as a /model picker. The agent process is alive, so zombie checks
// miss it, but the pane cannot accept work.
type BlockedPaneCheck struct {
	BaseCheck
}

// NewBlockedPaneCheck creates a blocked-pane check.
func NewBlockedPaneCheck() *BlockedPaneCheck {
	return &BlockedPaneCheck{
		BaseCheck: BaseCheck{
			CheckName:        "blocked-panes",
			CheckDescription: "Detect agent panes blocked on interactive dialogs",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run captures known Gas Town panes and reports blocking dialogs.
func (c *BlockedPaneCheck) Run(ctx *CheckContext) *CheckResult {
	t := tmux.NewTmux()
	sessions, err := t.ListSessions()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not list tmux sessions",
			Details: []string{err.Error()},
		}
	}

	var blocked []string
	var checked int
	for _, sess := range sessions {
		if sess == "" || !session.IsKnownSession(sess) {
			continue
		}
		checked++
		content, capErr := t.CapturePane(sess, 80)
		if capErr != nil {
			continue
		}
		if name, ok := tmux.ContainsBlockingPane(content); ok {
			blocked = append(blocked, fmt.Sprintf("%s: %s", sess, name))
		}
	}

	if len(blocked) == 0 {
		msg := "No blocked agent panes"
		if checked > 0 {
			msg = fmt.Sprintf("All %d Gas Town panes are interactive", checked)
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: msg,
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Found %d blocked pane(s)", len(blocked)),
		Details: blocked,
		FixHint: "Dismiss the dialog in the pane (Esc or choose a model), or restart the session",
	}
}
