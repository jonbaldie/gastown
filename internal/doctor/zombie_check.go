package doctor

import (
	"fmt"

	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// ZombieSessionCheck detects tmux sessions that are valid Gas Town sessions
// but have no Claude/node process running inside (zombies).
// These occur when Claude exits or crashes but the tmux session remains.
type ZombieSessionCheck struct {
	FixableCheck
	zombieSessions []string // Cached during Run for use in Fix
}

// NewZombieSessionCheck creates a new zombie session check.
func NewZombieSessionCheck() *ZombieSessionCheck {
	return &ZombieSessionCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "zombie-sessions",
				CheckDescription: "Detect tmux sessions with dead Claude processes",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// Run checks for zombie Gas Town sessions (tmux alive but Claude dead).
func (c *ZombieSessionCheck) Run(_ *CheckContext) *CheckResult {
	t := tmux.NewTmux()

	sessions, err := t.ListSessions()
	if err != nil {
		return zombieSessionListError(c, err)
	}

	if len(sessions) == 0 {
		return noZombieSessionsResult(c)
	}

	zombies, healthyCount := classifyZombieSessions(t, sessions)

	c.zombieSessions = zombies
	return c.zombieResult(zombies, healthyCount)
}

func zombieSessionListError(c *ZombieSessionCheck, err error) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: "Could not list tmux sessions",
		Details: []string{err.Error()},
	}
}

func noZombieSessionsResult(c *ZombieSessionCheck) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "No tmux sessions found",
	}
}

func classifyZombieSessions(t *tmux.Tmux, sessions []string) ([]string, int) {
	var zombies []string
	healthyCount := 0
	for _, sess := range sessions {
		if !shouldInspectZombieSession(sess) {
			continue
		}
		if t.IsAgentAlive(sess) {
			healthyCount++
			continue
		}
		zombies = append(zombies, sess)
	}
	return zombies, healthyCount
}

func shouldInspectZombieSession(sess string) bool {
	return sess != "" && session.IsKnownSession(sess) && !isCrewSession(sess)
}

func (c *ZombieSessionCheck) zombieResult(zombies []string, healthyCount int) *CheckResult {
	if len(zombies) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: zombieOKMessage(healthyCount),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Found %d zombie session(s)", len(zombies)),
		Details: zombieDetails(zombies),
		FixHint: "Run 'gt doctor --fix' to kill zombie sessions",
	}
}

func zombieOKMessage(healthyCount int) string {
	if healthyCount > 0 {
		return fmt.Sprintf("All %d Gas Town sessions have running Claude processes", healthyCount)
	}
	return "No zombie sessions found"
}

func zombieDetails(zombies []string) []string {
	details := make([]string, len(zombies))
	for i, name := range zombies {
		details[i] = fmt.Sprintf("Zombie: %s (tmux alive, Claude dead)", name)
	}
	return details
}

// Fix kills all zombie sessions (tmux sessions with no Claude running).
// Crew sessions are never auto-killed as they are human-managed.
func (c *ZombieSessionCheck) Fix(_ *CheckContext) error {
	if len(c.zombieSessions) == 0 {
		return nil
	}

	t := tmux.NewTmux()
	var lastErr error

	for _, sess := range c.zombieSessions {
		// SAFEGUARD: Never auto-kill crew sessions (double-check)
		if isCrewSession(sess) {
			continue
		}

		// TOCTOU guard: re-verify Claude is still dead in this session.
		// Between Run() identifying zombies and Fix() killing them,
		// a Claude process may have started (e.g., session was restarted).
		if t.IsAgentAlive(sess) {
			continue
		}

		// Log pre-death event for audit trail
		_ = events.LogFeed(events.TypeSessionDeath, sess,
			events.SessionDeathPayload(sess, "unknown", "zombie cleanup", "gt doctor"))

		// Use KillSessionWithProcesses to ensure all descendant processes are killed.
		if err := t.KillSessionWithProcesses(sess); err != nil {
			lastErr = err
		}
	}

	return lastErr
}
