package doctor

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/gastown/internal/lock"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// IdentityCollisionCheck checks for agent identity collisions and stale locks.
type IdentityCollisionCheck struct {
	BaseCheck
}

// NewIdentityCollisionCheck creates a new identity collision check.
func NewIdentityCollisionCheck() *IdentityCollisionCheck {
	return &IdentityCollisionCheck{
		BaseCheck: BaseCheck{
			CheckName:        "identity-collision",
			CheckDescription: "Check for agent identity collisions and stale locks",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

func (c *IdentityCollisionCheck) CanFix() bool {
	return true // Can fix stale locks
}

func (c *IdentityCollisionCheck) Run(ctx *CheckContext) *CheckResult {
	// Find all locks
	locks, err := lock.FindAllLocks(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("could not scan for locks: %v", err),
		}
	}

	if len(locks) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "no worker locks found",
		}
	}

	sessionSet := activeIdentitySessions()
	staleLocks, orphanedLocks, healthyLocks := classifyIdentityLocks(locks, sessionSet)
	return identityCheckResult(c.Name(), staleLocks, orphanedLocks, healthyLocks)
}

func activeIdentitySessions() map[string]bool {
	// Build a set containing both session names AND session IDs because locks
	// may store either format.
	t := tmux.NewTmux()
	sessionSet := make(map[string]bool)
	sessions, _ := t.ListSessions()
	for _, session := range sessions {
		sessionSet[session] = true
	}
	// Lock files may contain session_id in formats like "%55" or "$55".
	sessionIDs, _ := t.ListSessionIDs()
	for _, id := range sessionIDs {
		addIdentitySessionID(sessionSet, id)
	}
	return sessionSet
}

func addIdentitySessionID(sessionSet map[string]bool, id string) {
	sessionSet[id] = true
	if len(id) == 0 {
		return
	}
	switch id[0] {
	case '$':
		sessionSet["%"+id[1:]] = true
	case '%':
		sessionSet["$"+id[1:]] = true
	}
}

func classifyIdentityLocks(locks map[string]*lock.LockInfo, sessionSet map[string]bool) ([]string, []string, int) {
	var staleLocks []string
	var orphanedLocks []string
	healthyLocks := 0
	for workerDir, info := range locks {
		sessionExists := info.SessionID != "" && sessionSet[info.SessionID]
		if info.IsStale() {
			if sessionExists {
				healthyLocks++
				continue
			}
			staleLocks = append(staleLocks, fmt.Sprintf("%s (dead PID %d)", workerDir, info.PID))
			continue
		}
		if info.SessionID != "" && !sessionSet[info.SessionID] {
			orphanedLocks = append(orphanedLocks, fmt.Sprintf("%s (PID %d, missing session %s)", workerDir, info.PID, info.SessionID))
			continue
		}
		healthyLocks++
	}
	return staleLocks, orphanedLocks, healthyLocks
}

func identityCheckResult(name string, staleLocks, orphanedLocks []string, healthyLocks int) *CheckResult {
	if len(staleLocks) == 0 && len(orphanedLocks) == 0 {
		return &CheckResult{
			Name:    name,
			Status:  StatusOK,
			Message: fmt.Sprintf("%d worker lock(s), all healthy", healthyLocks),
		}
	}
	result := &CheckResult{Name: name}
	addStaleLockDetails(result, staleLocks)
	addOrphanedLockDetails(result, orphanedLocks)
	return result
}

func addStaleLockDetails(result *CheckResult, staleLocks []string) {
	if len(staleLocks) == 0 {
		return
	}
	result.Status = StatusWarning
	result.Message = fmt.Sprintf("%d stale lock(s) found", len(staleLocks))
	result.Details = append(result.Details, "Stale locks (dead PIDs):")
	for _, stale := range staleLocks {
		result.Details = append(result.Details, "  "+stale)
	}
	result.FixHint = "Run 'gt doctor --fix' or 'gt agents fix' to clean up"
}

func addOrphanedLockDetails(result *CheckResult, orphanedLocks []string) {
	if len(orphanedLocks) == 0 {
		return
	}
	result.Status = StatusWarning
	if result.Message != "" {
		result.Message += ", "
	}
	result.Message += fmt.Sprintf("%d orphaned lock(s)", len(orphanedLocks))
	result.Details = append(result.Details, "Orphaned locks (missing sessions):")
	for _, orphaned := range orphanedLocks {
		result.Details = append(result.Details, "  "+orphaned)
	}
	if !strings.Contains(result.FixHint, "doctor") {
		result.FixHint = "Run 'gt doctor --fix' to clean up stale locks"
	}
}

func (c *IdentityCollisionCheck) Fix(ctx *CheckContext) error {
	cleaned, err := lock.CleanStaleLocks(ctx.TownRoot)
	if err != nil {
		return fmt.Errorf("cleaning stale locks: %w", err)
	}

	if cleaned > 0 {
		fmt.Printf("  Cleaned %d stale lock(s)\n", cleaned)
	}

	return nil
}
