package dog

import (
	"fmt"
	"time"

	"github.com/jonbaldie/gastown/internal/tmux"
)

// sessionChecker abstracts the tmux health-check methods needed by the
// health checker.  Satisfied by *tmux.Tmux; mockable in tests.
type sessionChecker interface {
	CheckSessionHealth(_ string, _ time.Duration) tmux.ZombieStatus
	HasSession(_ string) (bool, error)
	KillSession(_ string) error
}

// DogHealthResult describes the health of a single dog.
type DogHealthResult struct {
	Name           string        `json:"name"`
	State          State         `json:"state"`
	SessionStatus  string        `json:"session_status"`          // from ZombieStatus.String()
	WorkDuration   time.Duration `json:"work_duration,omitempty"` // how long current work has been running
	NeedsAttention bool          `json:"needs_attention"`
	AutoCleared    bool          `json:"auto_cleared,omitempty"`
	Recommendation string        `json:"recommendation,omitempty"`
}

// HealthChecker performs health checks on dogs in the kennel.
type HealthChecker struct {
	mgr     *Manager
	checker sessionChecker
}

// NewHealthChecker creates a HealthChecker.
func NewHealthChecker(mgr *Manager, checker sessionChecker) *HealthChecker {
	return &HealthChecker{mgr: mgr, checker: checker}
}

// dogSessionName returns the tmux session name for a dog.
func dogSessionName(name string) string {
	return fmt.Sprintf("hq-dog-%s", name)
}

// Check performs a health check on a single dog.
func (hc *HealthChecker) Check(d *Dog, maxInactivity time.Duration, autoClear bool) DogHealthResult {
	result := DogHealthResult{
		Name:  d.Name,
		State: d.State,
	}

	// Compute work duration if working and WorkStartedAt is set.
	if d.State == StateWorking && !d.WorkStartedAt.IsZero() {
		result.WorkDuration = time.Since(d.WorkStartedAt)
	}
	session := dogSessionName(d.Name)
	switch d.State {
	case StateWorking:
		hc.checkWorkingDog(d, &result, session, maxInactivity, autoClear)
	case StateIdle:
		hc.checkIdleDog(&result, session, autoClear)
	}
	return result
}

func (hc *HealthChecker) checkWorkingDog(d *Dog, result *DogHealthResult, session string, maxInactivity time.Duration, autoClear bool) {
	status := hc.checker.CheckSessionHealth(session, maxInactivity)
	result.SessionStatus = status.String()
	switch status {
	case tmux.SessionDead:
		result.NeedsAttention, result.Recommendation = true, "zombie: session dead but state=working"
		hc.maybeClearStateOnly(d, result, autoClear, false, session, "zombie: source-backed work requires explicit recovery", "zombie auto-cleared (session dead)")
	case tmux.AgentDead:
		result.NeedsAttention, result.Recommendation = true, "zombie: agent dead in session"
		hc.maybeClearStateOnly(d, result, autoClear, true, session, "zombie: session killed; source-backed work requires explicit recovery", "zombie auto-cleared (agent dead, session killed)")
	case tmux.AgentHung:
		hc.checkHungDog(d, result, session, autoClear)
	}
}

func (hc *HealthChecker) checkHungDog(d *Dog, result *DogHealthResult, session string, autoClear bool) {
	result.NeedsAttention = true
	if !autoClear {
		result.Recommendation = "hung: agent alive but no tmux activity"
		return
	}
	hc.maybeClearStateOnly(d, result, true, true, session, "hung: session killed; source-backed work requires explicit recovery", "hung dog auto-cleared (idle prompt, session killed)")
}

func (hc *HealthChecker) checkIdleDog(result *DogHealthResult, session string, autoClear bool) {
	hasSession, _ := hc.checker.HasSession(session)
	if !hasSession {
		result.SessionStatus = "none"
		return
	}
	result.SessionStatus, result.NeedsAttention = "orphan", true
	if !autoClear {
		result.Recommendation = "orphan: dog idle but tmux session exists"
		return
	}
	_ = hc.checker.KillSession(session)
	result.AutoCleared, result.Recommendation = true, "orphan auto-cleared (session killed)"
}

func (hc *HealthChecker) maybeClearStateOnly(d *Dog, result *DogHealthResult, autoClear, killSession bool, session, blockedRec, clearedRec string) {
	if !autoClear {
		return
	}
	if killSession {
		_ = hc.checker.KillSession(session)
	}
	if !CanClearStateOnly(d.Work, d.WorkKind) {
		result.Recommendation = blockedRec
		return
	}
	cleared, err := hc.mgr.ClearWorkIfMatches(d.Name, d.Work, d.WorkStartedAt)
	if err != nil {
		result.Recommendation = "auto-clear failed: " + err.Error()
		return
	}
	if cleared {
		result.AutoCleared = true
		result.Recommendation = clearedRec
	}
}

// CheckAll performs health checks on all dogs.
func (hc *HealthChecker) CheckAll(maxInactivity time.Duration, autoClear bool) ([]DogHealthResult, error) {
	dogs, err := hc.mgr.List()
	if err != nil {
		return nil, fmt.Errorf("listing dogs: %w", err)
	}

	results := make([]DogHealthResult, 0, len(dogs))
	for _, d := range dogs {
		results = append(results, hc.Check(d, maxInactivity, autoClear))
	}
	return results, nil
}

// NeedsAttentionCount returns how many results need attention.
func NeedsAttentionCount(results []DogHealthResult) int {
	n := 0
	for _, r := range results {
		if r.NeedsAttention {
			n++
		}
	}
	return n
}
