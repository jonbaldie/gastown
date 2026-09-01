// Package nudge provides non-destructive nudge delivery for Gas Town agents.
//
// The nudge queue allows messages to be delivered cooperatively: instead of
// sending text directly to a tmux session (which cancels in-flight tool calls),
// nudges are written to a queue directory and picked up by the agent's
// UserPromptSubmit hook at the next natural turn boundary.
//
// Queue location: <townRoot>/.runtime/nudge_queue/<session>/
// Each nudge is a JSON file named by timestamp for FIFO ordering.
package nudge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
)

// Priority levels for nudge delivery.
const (
	// PriorityNormal is the default — delivered at next turn boundary.
	PriorityNormal = "normal"
	// PriorityUrgent means the agent should handle this promptly.
	PriorityUrgent = "urgent"
	// PrioritySystem waits for the end of a turn and does not start a new one.
	PrioritySystem = "system"
)

// Operational limits and defaults.
// These are compiled-in fallbacks. Configurable via operational.nudge
// in settings/config.json (ZFC pattern).
const (
	// DefaultNormalTTL is the time-to-live for normal-priority nudges.
	DefaultNormalTTL = 30 * time.Minute

	// DefaultUrgentTTL is the time-to-live for urgent-priority nudges.
	DefaultUrgentTTL = 2 * time.Hour

	// MaxQueueDepth is the maximum number of pending nudges per session.
	MaxQueueDepth = 50

	// staleClaimThreshold is how long a .claimed file must be untouched
	// before Drain considers it orphaned (from a crashed drainer) and removes it.
	staleClaimThreshold = 5 * time.Minute
)

// nudgeConfig loads nudge-specific thresholds from town settings.
func nudgeConfig(townRoot string) *config.NudgeThresholds {
	return config.LoadOperationalConfig(townRoot).GetNudgeConfig()
}

// QueuedNudge represents a nudge message stored in the queue.
type QueuedNudge struct {
	Sender    string    `json:"sender"`
	Message   string    `json:"message"`
	Priority  string    `json:"priority"`
	Kind      string    `json:"kind,omitempty"`
	ThreadID  string    `json:"thread_id,omitempty"`
	Severity  string    `json:"severity,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// DeliverAfter, if non-zero, defers delivery until this time has passed.
	// Drain skips (but does not discard) the nudge until the deadline is met.
	DeliverAfter time.Time `json:"deliver_after,omitempty"`
}

// queueDir returns the nudge queue directory for a given session.
// Path: <townRoot>/.runtime/nudge_queue/<session>/
func queueDir(townRoot, session string) string {
	// Sanitize session name for filesystem safety
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_queue", safe)
}

// randomSuffix returns a short random hex string to disambiguate filenames
// when multiple processes enqueue within the same nanosecond.
func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Enqueue writes a nudge to the queue for the given session.
// The nudge will be picked up by the agent's hook at the next turn boundary.
// Returns an error if the queue is full (MaxQueueDepth reached).
func Enqueue(townRoot, session string, nudge QueuedNudge) error {
	dir := queueDir(townRoot, session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating nudge queue dir: %w", err)
	}

	// Check queue depth before writing to prevent runaway senders.
	maxDepth := nudgeConfig(townRoot).MaxQueueDepthV()
	pending, _ := Pending(townRoot, session)
	if pending >= maxDepth {
		return fmt.Errorf("nudge queue for %s is full (%d/%d pending)", session, pending, maxDepth)
	}

	if nudge.Timestamp.IsZero() {
		nudge.Timestamp = time.Now()
	}
	if nudge.Priority == "" {
		nudge.Priority = PriorityNormal
	}

	// Set expiry if not already specified by the caller.
	if nudge.ExpiresAt.IsZero() {
		switch nudge.Priority {
		case PriorityUrgent:
			nudge.ExpiresAt = nudge.Timestamp.Add(DefaultUrgentTTL)
		default:
			nudge.ExpiresAt = nudge.Timestamp.Add(DefaultNormalTTL)
		}
	}

	data, err := json.MarshalIndent(nudge, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling nudge: %w", err)
	}

	// Use nanosecond timestamp + random suffix for unique, ordered filenames.
	// The random suffix prevents collisions when multiple agents enqueue
	// nudges for the same session within the same nanosecond.
	filename := fmt.Sprintf("%d-%s.json", nudge.Timestamp.UnixNano(), randomSuffix())
	path := filepath.Join(dir, filename)

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing nudge to queue: %w", err)
	}

	return nil
}

// Requeue writes previously drained nudges back to the queue for later delivery.
// Existing timestamps are preserved so FIFO ordering remains stable relative to
// one another; only expired nudges are skipped.
func Requeue(townRoot, session string, nudges []QueuedNudge) error {
	for _, n := range nudges {
		if !n.ExpiresAt.IsZero() && time.Now().After(n.ExpiresAt) {
			continue
		}
		if err := Enqueue(townRoot, session, n); err != nil {
			return err
		}
	}
	return nil
}

// Drain reads and removes all queued nudges for a session, returning them
// in FIFO order. This is called by the hook to pick up pending nudges.
//
// Uses rename-then-process to prevent concurrent Drain calls from delivering
// the same nudge twice: each file is atomically renamed to a .claimed suffix
// before reading, so only one caller can claim each nudge.
//
// Expired nudges (past ExpiresAt) are silently discarded during drain.
// Orphaned .claimed files from crashed drainers are swept if older than 5 minutes.
func Drain(townRoot, session string) ([]QueuedNudge, error) {
	dir := queueDir(townRoot, session)
	entries, err := readQueueEntries(dir)
	if err != nil {
		return nil, err
	}
	requeueOrphanedClaims(dir, entries, townRoot)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	now := time.Now()
	var nudges []QueuedNudge
	for _, entry := range entries {
		if n, ok := drainQueueEntry(dir, entry, now); ok {
			nudges = append(nudges, n)
		}
	}
	return nudges, nil
}

func readQueueEntries(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading nudge queue: %w", err)
	}
	return entries, nil
}

func requeueOrphanedClaims(dir string, entries []os.DirEntry, townRoot string) {
	// Requeue orphaned .claimed files from crashed drainers.
	// A .claimed file older than staleClaimThreshold is certainly orphaned —
	// normal processing completes in milliseconds. We rename it back to .json
	// so it gets picked up on this or a future Drain call, rather than deleting
	// it (which would permanently drop the nudge).
	staleThreshold := nudgeConfig(townRoot).StaleClaimThresholdD()
	now := time.Now()
	for _, entry := range entries {
		requeueOrphanedClaim(dir, entry, now, staleThreshold)
	}
}

func requeueOrphanedClaim(dir string, entry os.DirEntry, now time.Time, staleThreshold time.Duration) {
	if !strings.Contains(entry.Name(), ".claimed") {
		return
	}
	info, err := entry.Info()
	if err != nil || now.Sub(info.ModTime()) <= staleThreshold {
		return
	}
	orphanPath := filepath.Join(dir, entry.Name())
	name := entry.Name()
	claimedIdx := strings.Index(name, ".claimed")
	restoredPath := filepath.Join(dir, name[:claimedIdx])
	if err := os.Rename(orphanPath, restoredPath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to requeue orphaned claim %s: %v\n", entry.Name(), err)
		_ = os.Remove(orphanPath)
	}
}

func drainQueueEntry(dir string, entry os.DirEntry, now time.Time) (QueuedNudge, bool) {
	if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
		return QueuedNudge{}, false
	}
	path := filepath.Join(dir, entry.Name())
	claimPath, ok := claimQueueFile(path)
	if !ok {
		return QueuedNudge{}, false
	}
	n, ok := readClaimedNudge(path, claimPath, entry.Name())
	if !ok {
		return QueuedNudge{}, false
	}
	if dropClaimedNudge(n, path, claimPath, entry.Name(), now) {
		return QueuedNudge{}, false
	}
	if rmErr := os.Remove(claimPath); rmErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove processed claim %s: %v\n", entry.Name(), rmErr)
	}
	return n, true
}

func claimQueueFile(path string) (string, bool) {
	// Atomically claim the file by renaming it. If another Drain call
	// is racing us, only one rename will succeed — the loser gets
	// ENOENT and moves on. This prevents double-delivery.
	//
	// Each drainer uses a unique claim suffix to avoid destination
	// collisions. On Windows, os.Rename to a shared destination is
	// not atomic — two goroutines can both "succeed" via
	// MOVEFILE_REPLACE_EXISTING, causing data loss. Unique suffixes
	// ensure each rename has a distinct target.
	claimPath := path + ".claimed." + randomSuffix()
	if err := os.Rename(path, claimPath); err != nil {
		return "", false
	}
	return claimPath, true
}

func readClaimedNudge(path, claimPath, name string) (QueuedNudge, bool) {
	data, err := os.ReadFile(claimPath)
	if err != nil {
		if !os.IsNotExist(err) {
			_ = os.Rename(claimPath, path) // best-effort unclaim; orphan sweep catches failures
		}
		return QueuedNudge{}, false
	}
	var n QueuedNudge
	if err := json.Unmarshal(data, &n); err != nil {
		if rmErr := os.Remove(claimPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove malformed claim %s: %v\n", name, rmErr)
		}
		return QueuedNudge{}, false
	}
	return n, true
}

func dropClaimedNudge(n QueuedNudge, path, claimPath, name string, now time.Time) bool {
	if !n.ExpiresAt.IsZero() && now.After(n.ExpiresAt) {
		if rmErr := os.Remove(claimPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove expired nudge %s: %v\n", name, rmErr)
		}
		return true
	}
	if !n.DeliverAfter.IsZero() && now.Before(n.DeliverAfter) {
		if renameErr := os.Rename(claimPath, path); renameErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to unclaim deferred nudge %s: %v\n", name, renameErr)
		}
		return true
	}
	return false
}

// Pending returns the count of queued nudges for a session without draining.
// This is an approximate count — it does not check expiry or read file contents.
func Pending(townRoot, session string) (int, error) {
	dir := queueDir(townRoot, session)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading nudge queue: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}

	return count, nil
}

// QueueLen returns the number of pending nudges for a session without draining.
// Returns 0 on error — callers use this for quick checks. Missing queue
// directories are expected (no nudges yet) and silenced; other filesystem
// errors are logged to stderr so they don't go unnoticed.
func QueueLen(townRoot, session string) int {
	n, err := Pending(townRoot, session)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: nudge queue check failed for %s: %v\n", session, err)
	}
	return n
}

// RemoveKindByThread deletes queued nudges for a session that match both the
// provided kind and thread ID. It only removes queued .json files, leaving any
// in-flight claimed files alone so concurrent drainers can finish safely.
func RemoveKindByThread(townRoot, session, kind, threadID string) (int, error) {
	if kind == "" || threadID == "" {
		return 0, nil
	}
	dir := queueDir(townRoot, session)
	entries, err := readQueueEntries(dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		n, err := removeQueuedNudgeIfMatch(dir, entry, kind, threadID)
		if err != nil {
			return removed, err
		}
		removed += n
	}
	return removed, nil
}

func removeQueuedNudgeIfMatch(dir string, entry os.DirEntry, kind, threadID string) (int, error) {
	if !isQueuedJSON(entry) {
		return 0, nil
	}
	path := filepath.Join(dir, entry.Name())
	n, ok, err := readQueuedNudge(path, entry.Name())
	if err != nil || !ok {
		return 0, err
	}
	if n.Kind != kind || n.ThreadID != threadID {
		return 0, nil
	}
	return removeQueuedFile(path, entry.Name())
}

func isQueuedJSON(entry os.DirEntry) bool {
	return !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json")
}

func readQueuedNudge(path, name string) (QueuedNudge, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return QueuedNudge{}, false, nil
		}
		return QueuedNudge{}, false, fmt.Errorf("reading queued nudge %s: %w", name, err)
	}
	var n QueuedNudge
	if err := json.Unmarshal(data, &n); err != nil {
		return QueuedNudge{}, false, nil
	}
	return n, true, nil
}

func removeQueuedFile(path, name string) (int, error) {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("removing queued nudge %s: %w", name, err)
	}
	return 1, nil
}

// FormatForInjection formats queued nudges as a system-reminder block
// suitable for Claude Code hook output.
func FormatForInjection(nudges []QueuedNudge) string {
	if len(nudges) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<system-reminder>\n")

	// Separate urgent from normal
	var urgent, normal []QueuedNudge
	for _, n := range nudges {
		if n.Priority == PriorityUrgent {
			urgent = append(urgent, n)
		} else {
			normal = append(normal, n)
		}
	}

	if len(urgent) > 0 {
		b.WriteString(fmt.Sprintf("QUEUED NUDGE (%d urgent):\n\n", len(urgent)))
		for _, n := range urgent {
			b.WriteString(fmt.Sprintf("  [URGENT from %s] %s\n", n.Sender, n.Message))
		}
		if len(normal) > 0 {
			b.WriteString(fmt.Sprintf("\nPlus %d non-urgent nudge(s):\n", len(normal)))
			for _, n := range normal {
				b.WriteString(fmt.Sprintf("  [from %s] %s\n", n.Sender, n.Message))
			}
		}
		b.WriteString("\nHandle urgent nudges before continuing current work.\n")
	} else {
		b.WriteString(fmt.Sprintf("QUEUED NUDGE (%d message(s)):\n\n", len(normal)))
		for _, n := range normal {
			b.WriteString(fmt.Sprintf("  [from %s] %s\n", n.Sender, n.Message))
		}
		b.WriteString("\nThis is a background notification. Continue current work unless the nudge is higher priority.\n")
	}

	b.WriteString("</system-reminder>\n")
	return b.String()
}
