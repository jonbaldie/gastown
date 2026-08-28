package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/checkpoint"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/workspace"
)

// hookInput represents the JSON input from LLM runtime hooks.
// Claude Code sends this on stdin for SessionStart hooks.
type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Source         string `json:"source"` // startup, resume, clear, compact
	HookEventName  string `json:"hook_event_name"`
}

// readHookSessionID reads session ID from available sources in hook mode.
//
// Priority (env vars first so non-Claude runtimes skip the stdin read entirely):
//  1. GT_SESSION_ID / runtime session env var — set by the hook command
//  2. Stdin JSON (Claude Code format)         — Claude sends {"session_id":…,"source":…}
//  3. Persisted .runtime/session_id           — written by a prior SessionStart hook
//  4. Auto-generate UUID
//
// Source is resolved from GT_HOOK_SOURCE env, stdin JSON, or empty.
// Non-Claude runtimes can use their preset's session ID environment variable
// (selected through GT_AGENT or GT_SESSION_ID_ENV), or set GT_SESSION_ID
// directly. Either path avoids the stdin delay. Example:
//
//	SessionStart: "export GT_SESSION_ID=$(uuidgen) GT_HOOK_SOURCE=startup && gt prime --hook"
//	PreCompress:  "export GT_HOOK_SOURCE=compact && gt prime --hook"
func readHookSessionID() (sessionID, source string) {
	primeStructuredSessionStartOutput = false
	// Source can come from env (any runtime) or stdin JSON (Claude only).
	// Check env first so it's available even when stdin provides the session ID.
	source = os.Getenv("GT_HOOK_SOURCE")

	// 1. Environment variables (fast path — skips stdin read entirely)
	if id := os.Getenv("GT_SESSION_ID"); id != "" {
		return id, source
	}
	if id := runtime.SessionIDFromEnv(); id != "" {
		return id, source
	}
	// 2. Try reading stdin JSON (Claude Code format).
	//    Checked before persisted file so a fresh Claude session always wins
	//    over a potentially stale .runtime/session_id from a previous session.
	if input := readStdinJSON(); input != nil {
		primeStructuredSessionStartOutput = input.HookEventName == "SessionStart"
		if input.SessionID != "" {
			// Stdin source overrides env source when both are present
			if input.Source != "" {
				source = input.Source
			}
			return input.SessionID, source
		}
	}

	// 3. Persisted session ID from a prior hook invocation (e.g., PreCompress
	//    reusing the session ID that SessionStart wrote to .runtime/session_id)
	if id := ReadPersistedSessionID(); id != "" {
		return id, source
	}

	// 4. Auto-generate
	return uuid.New().String(), source
}

// stdinReadTimeout is how long readStdinJSON waits for data before giving up.
// This is a safety net for runtimes that pipe stdin without sending data AND
// don't set GT_SESSION_ID. Claude Code sends JSON on the first tick, so 500ms
// is generous. Non-Claude runtimes should set GT_SESSION_ID to skip this entirely.
const stdinReadTimeout = 500 * time.Millisecond

// readStdinJSON attempts to read and parse JSON from stdin.
// Returns nil if stdin is empty, not a pipe, invalid JSON, or no data
// arrives within stdinReadTimeout.
func readStdinJSON() *hookInput {
	// Check if stdin has data (non-blocking)
	stat, err := os.Stdin.Stat()
	if err != nil {
		return nil
	}

	// Only read if stdin is a pipe or has data
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		// stdin is a terminal, not a pipe - no data to read
		return nil
	}

	// Read with timeout: some LLM runtimes pipe stdin without sending data,
	// which would block ReadString forever. Use a goroutine + timer so we
	// fall through to env-var / auto-generate paths after stdinReadTimeout.
	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		ch <- readResult{line, err}
	}()

	var line string
	select {
	case r := <-ch:
		if r.err != nil && r.line == "" {
			return nil
		}
		line = r.line
	case <-time.After(stdinReadTimeout):
		// The goroutine above is still blocked on ReadString and will leak.
		// This is intentional — gt prime is a short-lived CLI command that
		// exits shortly after, so the goroutine is cleaned up by process exit.
		return nil
	}

	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	var input hookInput
	if err := json.Unmarshal([]byte(line), &input); err != nil {
		return nil
	}

	return &input
}

// persistSessionID writes the session ID to .runtime/session_id
// This allows subsequent gt prime calls to find the session ID.
func persistSessionID(dir, sessionID string) {
	runtimeDir := filepath.Join(dir, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return // Non-fatal
	}

	sessionFile := filepath.Join(runtimeDir, "session_id")
	content := fmt.Sprintf("%s\n%s\n", sessionID, time.Now().Format(time.RFC3339))
	_ = os.WriteFile(sessionFile, []byte(content), 0644) // Non-fatal
}

// ReadPersistedSessionID reads a previously persisted session ID.
// Checks cwd first, then town root.
// Returns empty string if not found.
func ReadPersistedSessionID() string {
	// Try cwd first
	cwd, err := os.Getwd()
	if err == nil {
		if id := readSessionFile(cwd); id != "" {
			return id
		}
	}

	// Try town root
	townRoot, err := workspace.FindFromCwd()
	if err == nil && townRoot != "" {
		if id := readSessionFile(townRoot); id != "" {
			return id
		}
	}

	return ""
}

func readSessionFile(dir string) string {
	sessionFile := filepath.Join(dir, ".runtime", "session_id")
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return ""
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0])
	}
	return ""
}

// resolveSessionIDForPrime finds the session ID from available sources.
// Priority: GT_SESSION_ID env, CLAUDE_SESSION_ID env, persisted file, fallback.
func resolveSessionIDForPrime(actor string) string {
	// 1. Try runtime's session ID lookup (checks GT_SESSION_ID_ENV, then CLAUDE_SESSION_ID)
	if id := runtime.SessionIDFromEnv(); id != "" {
		return id
	}

	// 2. Persisted session file (from gt prime --hook)
	if id := ReadPersistedSessionID(); id != "" {
		return id
	}

	// 3. Fallback to generated identifier
	return fmt.Sprintf("%s-%d", actor, os.Getpid())
}

// emitSessionEvent emits a session_start event for seance discovery.
// The event is written to ~/gt/.events.jsonl and can be queried via gt seance.
// Session ID resolution order: GT_SESSION_ID, CLAUDE_SESSION_ID, persisted file, fallback.
func emitSessionEvent(ctx RoleContext) {
	if ctx.Role == RoleUnknown {
		return
	}

	// Get agent identity for the actor field
	actor := getAgentIdentity(ctx)
	if actor == "" {
		return
	}

	// Get session ID from multiple sources
	sessionID := resolveSessionIDForPrime(actor)

	// Determine topic from hook state or default
	topic := ""
	if ctx.Role == RoleWitness || ctx.Role == RoleRefinery || ctx.Role == RoleDeacon {
		topic = "patrol"
	}

	// Emit the event
	payload := events.SessionPayload(sessionID, actor, topic, ctx.WorkDir)
	_ = events.LogFeed(events.TypeSessionStart, actor, payload)
}

// outputSessionMetadata prints a structured metadata line for seance discovery.
// Format: [GAS TOWN] role:<role> pid:<pid> session:<session_id>
// This enables gt seance to discover sessions from gt prime output.
func outputSessionMetadata(ctx RoleContext) {
	if ctx.Role == RoleUnknown {
		return
	}

	// Get agent identity for the role field
	actor := getAgentIdentity(ctx)
	if actor == "" {
		return
	}

	// Get session ID from multiple sources
	sessionID := resolveSessionIDForPrime(actor)

	// Output structured metadata line
	fmt.Println(formatSessionMetadataLine(actor, sessionID))
}

// formatSessionMetadataLine keeps the bracketed "[GAS TOWN]" banner for normal
// human-facing output, but removes the leading brackets during structured
// SessionStart hooks because Codex will see '[' and try to parse the line as
// JSON instead of treating it as plain text session metadata.
func formatSessionMetadataLine(actor, sessionID string) string {
	if primeStructuredSessionStartOutput {
		return fmt.Sprintf("GAS TOWN role:%s pid:%d session:%s", actor, os.Getpid(), sessionID)
	}
	return fmt.Sprintf("[GAS TOWN] role:%s pid:%d session:%s", actor, os.Getpid(), sessionID)
}

// --- Session state detection (merged from prime_state.go) ---

// SessionState represents the detected session state for observability.
type SessionState struct {
	State         string `json:"state"`                    // normal, post-handoff, crash-recovery, autonomous
	Role          Role   `json:"role"`                     // detected role
	PrevSession   string `json:"prev_session,omitempty"`   // for post-handoff
	CheckpointAge string `json:"checkpoint_age,omitempty"` // for crash-recovery
	HookedBead    string `json:"hooked_bead,omitempty"`    // for autonomous
}

// detectSessionState returns the current session state without side effects.
func detectSessionState(ctx RoleContext) SessionState {
	if state, ok := postHandoffSessionState(ctx); ok {
		return state
	}
	if state, ok := crashRecoverySessionState(ctx); ok {
		return state
	}
	if state, ok := autonomousSessionState(ctx); ok {
		return state
	}
	return SessionState{State: "normal", Role: ctx.Role}
}

func postHandoffSessionState(ctx RoleContext) (SessionState, bool) {
	markerPath := filepath.Join(ctx.WorkDir, constants.DirRuntime, constants.FileHandoffMarker)
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return SessionState{}, false
	}
	return SessionState{
		State:       "post-handoff",
		Role:        ctx.Role,
		PrevSession: strings.TrimSpace(string(data)),
	}, true
}

func crashRecoverySessionState(ctx RoleContext) (SessionState, bool) {
	if !isWorkerRole(ctx.Role) {
		return SessionState{}, false
	}
	cp, err := checkpoint.Read(ctx.WorkDir)
	if err != nil || cp == nil {
		return SessionState{}, false
	}
	if checkpoint.IsStale(cp, 24*time.Hour) {
		return SessionState{}, false
	}
	return SessionState{
		State:         "crash-recovery",
		Role:          ctx.Role,
		CheckpointAge: checkpoint.Age(cp).Round(time.Minute).String(),
	}, true
}

func isWorkerRole(role Role) bool {
	return role == RolePolecat || role == RoleCrew
}

func autonomousSessionState(ctx RoleContext) (SessionState, bool) {
	agentID := getAgentIdentity(ctx)
	if agentID == "" {
		return SessionState{}, false
	}

	beadsDir := sessionBeadsDir(ctx)
	b := beads.New(beadsDir)
	if hookBead, ok := resolveSessionAgentHookBead(ctx, agentID); ok {
		return autonomousState(ctx.Role, hookBead), true
	}
	if hookBead, ok := assignedHookBead(b, agentID); ok {
		return autonomousState(ctx.Role, hookBead), true
	}
	if hookBead, ok := townHookBead(ctx, agentID); ok {
		return autonomousState(ctx.Role, hookBead), true
	}
	return SessionState{}, false
}

func sessionBeadsDir(ctx RoleContext) string {
	if ctx.Rig == "" || ctx.TownRoot == "" {
		return ctx.WorkDir
	}
	rigDir := filepath.Join(ctx.TownRoot, ctx.Rig)
	if _, err := os.Stat(filepath.Join(rigDir, ".beads")); err == nil {
		return rigDir
	}
	return ctx.WorkDir
}

func resolveSessionAgentHookBead(ctx RoleContext, agentID string) (string, bool) {
	agentBeadID := buildAgentBeadID(agentID, ctx.Role, ctx.TownRoot)
	if agentBeadID == "" {
		return "", false
	}
	agentBeadDir := beads.ResolveHookDir(ctx.TownRoot, agentBeadID, ctx.WorkDir)
	agentBead, err := beads.New(agentBeadDir).Show(agentBeadID)
	if err != nil || agentBead == nil || agentBead.HookBead == "" {
		return "", false
	}
	hookBeadDir := beads.ResolveHookDir(ctx.TownRoot, agentBead.HookBead, ctx.WorkDir)
	hookBead, err := beads.New(hookBeadDir).Show(agentBead.HookBead)
	if err != nil || hookBead == nil || !isActiveHookStatus(hookBead.Status) {
		return "", false
	}
	return agentBead.HookBead, true
}

func isActiveHookStatus(status string) bool {
	return status == beads.StatusHooked || status == "in_progress"
}

func assignedHookBead(b *beads.Beads, agentID string) (string, bool) {
	hookedBeads, err := listAssignedActiveWork(b, agentID)
	if err != nil || len(hookedBeads) == 0 {
		return "", false
	}
	return hookedBeads[0].ID, true
}

func townHookBead(ctx RoleContext, agentID string) (string, bool) {
	if isTownLevelRole(agentID) || ctx.TownRoot == "" {
		return "", false
	}
	townB := beads.New(filepath.Join(ctx.TownRoot, ".beads"))
	return assignedHookBead(townB, agentID)
}

func autonomousState(role Role, hookBead string) SessionState {
	return SessionState{State: "autonomous", Role: role, HookedBead: hookBead}
}

// checkHandoffMarker checks for a handoff marker file and outputs a warning if found.
// This prevents the "handoff loop" bug where a new session sees /handoff in context
// and incorrectly runs it again. The marker tells the new session: "handoff is DONE,
// the /handoff you see in context was from YOUR PREDECESSOR, not a request for you."
//
// The marker format is: "session_id\nreason" (reason is optional, on second line).
// When present, the reason is stored in primeHandoffReason for compact/resume detection.
// This enables compaction-triggered handoff cycles to route through the lighter
// compact/resume path instead of full re-initialization. (GH#1965)
func checkHandoffMarker(workDir string) {
	markerPath := filepath.Join(workDir, constants.DirRuntime, constants.FileHandoffMarker)
	data, err := os.ReadFile(markerPath)
	if err != nil {
		// No marker = not post-handoff, normal startup
		return
	}

	// Parse marker: first line is session ID, optional second line is reason
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	prevSession := strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		primeHandoffReason = strings.TrimSpace(lines[1])
	}

	// Remove the marker FIRST so we don't warn twice
	_ = os.Remove(markerPath)

	// Output prominent warning
	outputHandoffWarning(prevSession)
}

// checkHandoffMarkerDryRun checks for handoff marker without removing it (for --dry-run).
func checkHandoffMarkerDryRun(workDir string) {
	markerPath := filepath.Join(workDir, constants.DirRuntime, constants.FileHandoffMarker)
	data, err := os.ReadFile(markerPath)
	if err != nil {
		// No marker = not post-handoff, normal startup
		explain(true, "Post-handoff: no handoff marker found")
		return
	}

	// Parse marker: first line is session ID, optional second line is reason
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	prevSession := strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		primeHandoffReason = strings.TrimSpace(lines[1])
	}

	explain(true, fmt.Sprintf("Post-handoff: marker found (predecessor: %s, reason: %s), marker NOT removed in dry-run", prevSession, primeHandoffReason))

	// Output the warning but don't remove marker
	outputHandoffWarning(prevSession)
}
