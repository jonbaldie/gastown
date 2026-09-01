package doctor

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// ThemeCheck verifies tmux sessions have correct themes applied.
type ThemeCheck struct {
	FixableCheck
}

// NewThemeCheck creates a new theme check.
func NewThemeCheck() *ThemeCheck {
	return &ThemeCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "themes",
				CheckDescription: "Check tmux session theme configuration",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks if tmux sessions have themes applied correctly.
func (c *ThemeCheck) Run(_ *CheckContext) *CheckResult {
	t := tmux.NewTmux()

	// List all sessions
	sessions, err := t.ListSessions()
	if err != nil {
		// No tmux server or error - not a problem, just skip
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No tmux sessions running",
		}
	}

	gtSessions := knownGasTownSessions(sessions)

	if len(gtSessions) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No Gas Town sessions running",
		}
	}

	needsUpdate := outdatedThemeSessions(gtSessions)

	if len(needsUpdate) > 0 {
		details := make([]string, len(needsUpdate))
		for i, s := range needsUpdate {
			details[i] = fmt.Sprintf("Needs update: %s", s)
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d session(s) have outdated theme format", len(needsUpdate)),
			Details: details,
			FixHint: "Run 'gt theme apply --all' or 'gt doctor --fix'",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("%d session(s) have correct themes", len(gtSessions)),
	}
}

func knownGasTownSessions(sessions []string) []string {
	var known []string
	for _, name := range sessions {
		if session.IsKnownSession(name) {
			known = append(known, name)
		}
	}
	return known
}

func outdatedThemeSessions(sessions []string) []string {
	var outdated []string
	for _, name := range sessions {
		statusLeft, err := getSessionStatusLeft(name)
		if err != nil {
			continue
		}
		if hasLegacyThemeFormat(statusLeft) {
			outdated = append(outdated, name)
		}
	}
	return outdated
}

func hasLegacyThemeFormat(statusLeft string) bool {
	return strings.Contains(statusLeft, "[") && strings.Contains(statusLeft, "]")
}

// Fix applies themes to all sessions.
func (c *ThemeCheck) Fix(ctx *CheckContext) error {
	cmd := exec.Command("gt", "theme", "apply", "--all")
	cmd.Dir = ctx.TownRoot
	return cmd.Run()
}

// getSessionStatusLeft retrieves the status-left setting for a tmux session.
func getSessionStatusLeft(session string) (string, error) {
	cmd := tmux.BuildCommand("show-options", "-t", session, "status-left")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	// Parse: status-left "value"
	line := strings.TrimSpace(string(output))
	if idx := strings.Index(line, "\""); idx != -1 {
		end := strings.LastIndex(line, "\"")
		if end > idx {
			return line[idx+1 : end], nil
		}
	}
	return line, nil
}
