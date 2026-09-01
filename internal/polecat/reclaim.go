package polecat

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/session"
)

func brokenIdleReclaimDispositionBlocker(d WorkstateDisposition) string {
	if d.Reason != "git-check-failed" {
		return fmt.Sprintf("workstate=%s reason=%s", d.Verdict, d.Reason)
	}
	// Structural reclaim already proved the worktree is damaged. A single
	// git-check-failed blocker is the expected Workstate for that clone,
	// including the detailed git error InspectWorkstate now records.
	if len(d.Blockers) != 1 {
		return fmt.Sprintf("workstate blockers=%s", strings.Join(d.Blockers, ","))
	}
	return ""
}

func brokenIdleReclaimAgentBlocker(fields *beads.AgentFields) string {
	if fields == nil {
		return "agent_fields=<missing>"
	}
	if blocker := agentStateBlocker(fields.AgentState); blocker != "" {
		return blocker
	}
	if blocker := cleanupStatusBlocker(fields.CleanupStatus); blocker != "" {
		return blocker
	}
	return agentDetailsBlocker(fields)
}

func agentDetailsBlocker(fields *beads.AgentFields) string {
	if blocker := nonEmptyAgentFieldBlocker("hook_bead", fields.HookBead); blocker != "" {
		return blocker
	}
	if blocker := nonEmptyAgentFieldBlocker("active_mr", fields.ActiveMR); blocker != "" {
		return blocker
	}
	if blocker := flagAgentFieldBlocker("push_failed", fields.PushFailed); blocker != "" {
		return blocker
	}
	if blocker := flagAgentFieldBlocker("mr_failed", fields.MRFailed); blocker != "" {
		return blocker
	}
	if strings.TrimSpace(fields.Branch) == "" {
		return "branch=<missing>"
	}
	return ""
}

func agentStateBlocker(raw string) string {
	state := beads.AgentState(raw)
	if state == beads.AgentStateIdle {
		return ""
	}
	if state == "" {
		return "agent_state=<missing>"
	}
	return "agent_state=" + string(state)
}

func cleanupStatusBlocker(raw string) string {
	status := CleanupStatus(raw)
	if status == CleanupClean {
		return ""
	}
	if status == "" {
		return "cleanup_status=<missing>"
	}
	return "cleanup_status=" + string(status)
}

func nonEmptyAgentFieldBlocker(label, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return label + "=" + value
}

func flagAgentFieldBlocker(label string, set bool) string {
	if set {
		return label + "=true"
	}
	return ""
}

func brokenIdleReclaimMRBlocker(branch string, mr *beads.Issue, err error) string {
	if strings.TrimSpace(branch) == "" {
		return "branch=<missing>"
	}
	if err != nil {
		return fmt.Sprintf("checking MR for branch %s: %v", branch, err)
	}
	if mr != nil {
		return fmt.Sprintf("branch %s has open MR %s status=%s", branch, mr.ID, mr.Status)
	}
	return ""
}

func (m *Manager) brokenIdleReclaimSessionBlocker(name string) string {
	if m.tmux == nil {
		return "session_state=unverified"
	}
	sessionName := session.PolecatSessionName(session.PrefixFor(m.rig.Name), name)
	running, err := m.tmux.HasSession(sessionName)
	if err != nil {
		return fmt.Sprintf("session_state=lookup_error: %v", err)
	}
	if running {
		return "session_state=running"
	}
	return ""
}
