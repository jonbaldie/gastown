package polecat

import "strings"

const (
	WorkstateVerdictWorking       = "WORKING"
	WorkstateVerdictSafeToNuke    = "SAFE_TO_NUKE"
	WorkstateVerdictPendingMR     = "PENDING_MR"
	WorkstateVerdictNeedsRecovery = "NEEDS_RECOVERY"
	WorkstateVerdictNeedsMQSubmit = "NEEDS_MQ_SUBMIT"
)

// WorkstateInput contains the lifecycle, git, and merge-queue facts needed to
// classify a polecat consistently across list, recovery, witness, and capacity.
type WorkstateInput struct {
	State                          State
	HookBead                       string
	CleanupStatus                  CleanupStatus
	IgnoreCleanupStatus            bool
	PartialSpawnWithoutDurableHook bool
	ActiveWorkBlocker              string
	ActiveWorkCountsTowardCapacity bool
	ActiveMR                       string
	ActiveMRBlocker                string
	AssignedBeadTerminal           bool
	WorkstateGitFacts
	WorkstateMQFacts
}

// WorkstateGitFacts contains the live branch and working-tree evidence used
// when deciding whether a polecat can be reused.
type WorkstateGitFacts struct {
	Branch               string
	GitDirty             bool
	GitDirtyReason       string
	StashCount           int
	UnpushedCommits      int
	GitCheckFailed       bool
	GitCheckFailedReason string
}

// WorkstateMQFacts contains merge-request and merge-queue evidence used when
// deciding whether a polecat has work that still needs attention.
type WorkstateMQFacts struct {
	PushFailed         bool
	MRFailed           bool
	MQCheckRequired    bool
	HasSubmittableWork bool
	MQNotRequired      bool
	MRSubmitted        bool
	MQLookupFailed     bool
}

// WorkstateDisposition is the canonical polecat lifecycle decision. InspectWorkstate
// gathers Beads and git facts and DecideWorkstate classifies them. Callers must
// not gather those facts themselves.
type WorkstateDisposition struct {
	Verdict              string   `json:"verdict"`
	Reason               string   `json:"reason,omitempty"`
	Reusable             bool     `json:"reusable"`
	SafeToNuke           bool     `json:"safe_to_nuke"`
	NeedsRecovery        bool     `json:"needs_recovery"`
	NeedsMQSubmit        bool     `json:"needs_mq_submit"`
	MQStatus             string   `json:"mq_status,omitempty"`
	CountsTowardCapacity bool     `json:"counts_toward_capacity"`
	ReuseStatus          string   `json:"reuse_status,omitempty"`
	Blockers             []string `json:"blockers,omitempty"`
}

// DecideWorkstate returns the canonical disposition for a polecat.
func DecideWorkstate(in WorkstateInput) WorkstateDisposition {
	if d, ok := decideWorkstateEarly(in); ok {
		return d
	}
	acc := collectWorkstateBlockers(in)
	if len(acc.d.Blockers) > 0 {
		return finalizeBlockedWorkstate(acc)
	}
	d, done := applyMQWorkstate(acc.d, in)
	if done {
		return d
	}
	return finalizeReusableWorkstate(d, in)
}

func decideWorkstateEarly(in WorkstateInput) (WorkstateDisposition, bool) {
	if in.ActiveMRBlocker != "" && !in.PushFailed && !in.MRFailed && in.State == StateDone {
		return WorkstateDisposition{
			Verdict:     WorkstateVerdictPendingMR,
			Reason:      "active-mr-open",
			ReuseStatus: "idle-pr-open",
			Blockers:    []string{in.ActiveMRBlocker},
		}, true
	}
	// StateDone (agent_state=done, seen before a polecat's own idle transition
	// lands) falls through to the real predicate checks below instead of
	// bailing out here — otherwise a merged/clean polecat gets NEEDS_RECOVERY
	// with no blockers, disagreeing with git-state for no reason (gt-check-recovery-bug).
	if in.State != StateIdle && in.State != StateDone {
		return decideBusyWorkstate(in), true
	}
	return WorkstateDisposition{}, false
}

func decideBusyWorkstate(in WorkstateInput) WorkstateDisposition {
	verdict := WorkstateVerdictNeedsRecovery
	needsRecovery := true
	if in.State == StateWorking {
		verdict = WorkstateVerdictWorking
		needsRecovery = false
	}
	d := WorkstateDisposition{
		Verdict:              verdict,
		Reason:               "not-idle",
		NeedsRecovery:        needsRecovery,
		CountsTowardCapacity: true,
	}
	if in.ActiveWorkBlocker != "" {
		d.Blockers = append(d.Blockers, in.ActiveWorkBlocker)
	}
	return d
}

type workstateAccumulator struct {
	d               WorkstateDisposition
	capacityBlocked bool
	activeMRBlocks  bool
}

func (a *workstateAccumulator) block(reason, blocker string, countsTowardCapacity bool) {
	if a.d.Reason == "" {
		a.d.Reason = reason
	}
	if blocker != "" {
		a.d.Blockers = append(a.d.Blockers, blocker)
	}
	a.capacityBlocked = a.capacityBlocked || countsTowardCapacity
}

func collectWorkstateBlockers(in WorkstateInput) workstateAccumulator {
	acc := workstateAccumulator{d: WorkstateDisposition{Verdict: WorkstateVerdictSafeToNuke}}
	applyHookAndMRBlockers(&acc, in)
	applyCleanupBlocker(&acc, in)
	applyGitBlockers(&acc, in)
	acc.activeMRBlocks = in.ActiveMRBlocker != ""
	if acc.activeMRBlocks {
		acc.block("active-mr-open", in.ActiveMRBlocker, false)
	}
	return acc
}

func applyHookAndMRBlockers(acc *workstateAccumulator, in WorkstateInput) {
	if in.HookBead != "" && !in.PartialSpawnWithoutDurableHook {
		acc.block("hook-still-set", "has work on hook ("+in.HookBead+")", true)
	}
	if in.PushFailed {
		acc.block("push-failed", "push_failed=true", true)
	}
	if in.MRFailed {
		acc.block("mr-failed", "mr_failed=true", true)
	}
	if in.ActiveWorkBlocker != "" {
		acc.block("active-work", in.ActiveWorkBlocker, in.ActiveWorkCountsTowardCapacity)
	}
}

func applyCleanupBlocker(acc *workstateAccumulator, in WorkstateInput) {
	if in.IgnoreCleanupStatus || in.CleanupStatus.IsSafe() {
		return
	}
	reason := "cleanup-" + string(in.CleanupStatus)
	blocker := "cleanup_status=" + string(in.CleanupStatus)
	if in.CleanupStatus == "" {
		reason = "cleanup-unknown"
		blocker = "cleanup_status=<missing>"
	} else if in.CleanupStatus == CleanupUnknown {
		reason = "cleanup-unknown"
	}
	acc.block(reason, blocker, true)
}

func applyGitBlockers(acc *workstateAccumulator, in WorkstateInput) {
	if in.GitCheckFailed {
		blocker := in.GitCheckFailedReason
		if blocker == "" {
			blocker = "git_state=unknown"
		}
		acc.block("git-check-failed", blocker, true)
	}
	if in.GitDirty {
		blocker := in.GitDirtyReason
		if blocker == "" {
			blocker = "git_state=has_uncommitted"
		}
		acc.block("git-dirty", blocker, true)
	}
	if in.StashCount > 0 {
		acc.block("git-stash", "git_state=has_stash stash_count="+itoa(in.StashCount), true)
	}
	if in.UnpushedCommits > 0 {
		acc.block("git-unpushed", "git_state=has_unpushed unpushed_commits="+itoa(in.UnpushedCommits), true)
	}
}

func finalizeBlockedWorkstate(acc workstateAccumulator) WorkstateDisposition {
	d := acc.d
	if acc.activeMRBlocks && len(d.Blockers) == 1 {
		d.Verdict = WorkstateVerdictPendingMR
		d.ReuseStatus = "idle-pr-open"
		return d
	}
	d.Verdict = WorkstateVerdictNeedsRecovery
	d.NeedsRecovery = true
	d.CountsTowardCapacity = acc.capacityBlocked
	d.ReuseStatus = "idle-recovery-needed"
	return d
}

func applyMQWorkstate(d WorkstateDisposition, in WorkstateInput) (WorkstateDisposition, bool) {
	if !in.MQCheckRequired {
		return d, false
	}
	if in.MQLookupFailed {
		return mqLookupFailedWorkstate(d), true
	}
	if !in.HasSubmittableWork || in.MQNotRequired {
		d.MQStatus = "not_required"
		return d, false
	}
	if in.MRSubmitted {
		d.MQStatus = "submitted"
		return d, false
	}
	return mqNotSubmittedWorkstate(d), true
}

func mqLookupFailedWorkstate(d WorkstateDisposition) WorkstateDisposition {
	d.Verdict = WorkstateVerdictNeedsRecovery
	d.Reason = "mq-lookup-failed"
	d.NeedsRecovery = true
	d.MQStatus = "unknown"
	d.CountsTowardCapacity = true
	d.ReuseStatus = "idle-recovery-needed"
	d.Blockers = append(d.Blockers, "mq_status=unknown")
	return d
}

func mqNotSubmittedWorkstate(d WorkstateDisposition) WorkstateDisposition {
	d.Verdict = WorkstateVerdictNeedsMQSubmit
	d.Reason = "mq-not-submitted"
	d.NeedsRecovery = true
	d.NeedsMQSubmit = true
	d.MQStatus = "not_submitted"
	d.CountsTowardCapacity = true
	d.ReuseStatus = "idle-recovery-needed"
	d.Blockers = append(d.Blockers, "mq_status=not_submitted")
	return d
}

func finalizeReusableWorkstate(d WorkstateDisposition, in WorkstateInput) WorkstateDisposition {
	d.Reusable = true
	d.SafeToNuke = true
	d.Reason = "reusable"
	d.ReuseStatus = "idle-clean"
	if strings.HasPrefix(in.Branch, "polecat/") {
		d.ReuseStatus = "idle-preserved"
	}
	return d
}

// CanIgnoreStaleCleanupStatus returns true when a dirty persisted
// cleanup_status is older than the direct predicates proving no work is at risk.
// The status remains unsafe globally; callers must opt into this reconciliation
// path only after gathering live git, hook, work, and active-MR facts.
func CanIgnoreStaleCleanupStatus(status CleanupStatus, workTerminal, hookSafe, activeMRSafe, gitSafe bool) bool {
	if !workTerminal || !hookSafe || !activeMRSafe || !gitSafe {
		return false
	}
	switch status {
	case CleanupUncommitted, CleanupStash, CleanupUnpushed:
		return true
	default:
		return false
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
