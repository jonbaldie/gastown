package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/channelevents"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type awaitEventOptions struct {
	channel              string
	timeout              string
	backoffBase          string
	backoffMult          int
	backoffMax           string
	quiet                bool
	agentBead            string
	cleanup              bool
	contextCheckInterval string
	json                 bool
}

func awaitEventOptionsFromCommand(cmd *cobra.Command) awaitEventOptions {
	return awaitEventOptions{
		channel:              commandStringFlag(cmd, "channel"),
		timeout:              commandStringFlag(cmd, "timeout"),
		backoffBase:          commandStringFlag(cmd, "backoff-base"),
		backoffMult:          commandIntFlag(cmd, "backoff-mult"),
		backoffMax:           commandStringFlag(cmd, "backoff-max"),
		quiet:                commandBoolFlag(cmd, "quiet"),
		agentBead:            commandStringFlag(cmd, "agent-bead"),
		cleanup:              commandBoolFlag(cmd, "cleanup"),
		contextCheckInterval: commandStringFlag(cmd, "context-check-interval"),
		json:                 commandBoolFlag(cmd, "json"),
	}
}

var moleculeAwaitEventCmd = &cobra.Command{
	Use:   "await-event",
	Short: "Wait for a file-based event on a named channel",
	Long: `Wait for event files to appear in ~/gt/events/<channel>/, with optional backoff.

Unlike await-signal (which subscribes to the generic beads activity feed),
await-event watches a dedicated event channel directory for .event files.
Events are emitted via "gt mol step emit-event" or programmatically.

Channels are single-consumer: only one process should watch a given channel
at a time. If multiple consumers watch the same channel with --cleanup,
events may be deleted before all consumers read them.

EVENT FORMAT:
Events are JSON files in ~/gt/events/<channel>/*.event:
  {"type": "...", "channel": "...", "timestamp": "...", "payload": {...}}

BEHAVIOR:
1. Check for already-pending events (return immediately if found)
2. If none, poll the directory until a new .event file appears or timeout
3. On wake, return all pending event file paths and contents
4. With --cleanup, delete processed event files automatically

BACKOFF MODE:
Same as await-signal: base * multiplier^idle_cycles, capped at max.
Idle cycles and backoff-until timestamp tracked on agent bead labels.
If killed and restarted, backoff resumes from the stored backoff-until.

CONTEXT-YIELD:
When --context-check-interval is set, await-event returns early with reason
"context-yield" after the specified wall-clock interval, even if no event
arrived and the backoff timeout has not expired. This allows patrol agents
to assess context usage between waits, preventing unbounded accumulation
during long idle periods.

Output when yielding:
  CONTEXT: check
  EFFORT: full

After context-check, call await-event again with the same parameters if
context is acceptable, or hand off the session if context is high.

EXIT CODES:
  0 - Event(s) found, timeout, or context-yield
  1 - Error

EXAMPLES:
  # Wait for refinery events with 10min timeout
  gt mol step await-event --channel refinery --timeout 10m

  # Backoff mode with agent bead tracking
  gt mol step await-event --channel refinery --agent-bead VAS-refinery \
    --backoff-base 60s --backoff-mult 2 --backoff-max 10m

  # Auto-cleanup processed events
  gt mol step await-event --channel refinery --cleanup

  # Yield every 5m for context check during long idle waits
  gt mol step await-event --channel refinery --agent-bead VAS-refinery \
    --backoff-base 60s --backoff-mult 2 --backoff-max 15m --cleanup \
    --context-check-interval 5m`,
	RunE: runMoleculeAwaitEvent,
}

// AwaitEventResult is the result of an await-event operation.
type AwaitEventResult struct {
	Reason      string        `json:"reason"`                // "event" or "timeout"
	Elapsed     time.Duration `json:"elapsed"`               // how long we waited
	Events      []EventFile   `json:"events,omitempty"`      // event files found
	IdleCycles  int           `json:"idle_cycles,omitempty"` // current idle cycle count
	EffortLevel string        `json:"effort_level"`          // "full" or "abbreviated"
}

// EventFile represents a single event file.
type EventFile struct {
	Path    string          `json:"path"`
	Content json.RawMessage `json:"content"`
}

func init() {
	moleculeAwaitEventCmd.Flags().String("channel", "",
		"Event channel name (required, e.g., 'refinery')")
	moleculeAwaitEventCmd.Flags().String("timeout", "60s",
		"Maximum time to wait for event (e.g., 30s, 5m, 10m)")
	moleculeAwaitEventCmd.Flags().String("backoff-base", "",
		"Base interval for exponential backoff (e.g., 60s)")
	moleculeAwaitEventCmd.Flags().Int("backoff-mult", 2,
		"Multiplier for exponential backoff (default: 2)")
	moleculeAwaitEventCmd.Flags().String("backoff-max", "",
		"Maximum interval cap for backoff (e.g., 10m)")
	moleculeAwaitEventCmd.Flags().String("agent-bead", "",
		"Agent bead ID for tracking idle cycles")
	moleculeAwaitEventCmd.Flags().Bool("quiet", false,
		"Suppress output (for scripting)")
	moleculeAwaitEventCmd.Flags().Bool("cleanup", false,
		"Delete event files after reading them")
	moleculeAwaitEventCmd.Flags().String("context-check-interval", "",
		"Yield after this wall-clock interval so the caller can assess context (e.g., 5m). Returns reason 'context-yield'.")
	moleculeAwaitEventCmd.Flags().BoolVar(&moleculeJSON, "json", false,
		"Output as JSON")
	_ = moleculeAwaitEventCmd.MarkFlagRequired("channel")

	moleculeStepCmd.AddCommand(moleculeAwaitEventCmd)
}

func runMoleculeAwaitEvent(cmd *cobra.Command, _ []string) error {
	opts := awaitEventOptionsFromCommand(cmd)
	state, err := prepareAwaitEvent(opts)
	if err != nil {
		return err
	}

	result, err := waitForAwaitEvent(state)
	if err != nil {
		return err
	}
	updateAwaitEventTracking(state, result)
	cleanupAwaitEvent(state, result)
	result.EffortLevel = effortLevelForAwaitResult(result.Reason, result.IdleCycles)
	return outputAwaitEvent(opts, result)
}

type awaitEventState struct {
	opts                 awaitEventOptions
	townRoot             string
	eventDir             string
	beadsDir             string
	idleCycles           int
	backoffUntil         time.Time
	timeout              time.Duration
	contextCheckInterval time.Duration
	resumed              bool
}

func prepareAwaitEvent(opts awaitEventOptions) (*awaitEventState, error) {
	if err := validateAwaitEventChannel(opts.channel); err != nil {
		return nil, err
	}
	townRoot := resolveAwaitEventTownRoot()
	eventDir := filepath.Join(townRoot, "events", opts.channel)
	if err := os.MkdirAll(eventDir, 0755); err != nil {
		return nil, fmt.Errorf("creating event directory: %w", err)
	}

	beadsDir, idleCycles, backoffUntil := loadAwaitEventTracking(opts)
	fullTimeout, err := calculateEventTimeout(opts, idleCycles)
	if err != nil {
		return nil, fmt.Errorf("invalid timeout configuration: %w", err)
	}
	contextCheckInterval, err := parseAwaitEventContextInterval(opts.contextCheckInterval)
	if err != nil {
		return nil, err
	}
	timeout, resumed := resumeAwaitEventWindow(opts, fullTimeout, backoffUntil)
	persistAwaitEventWindow(opts, beadsDir, timeout, resumed)
	printAwaitEventStart(opts, timeout, idleCycles)

	return &awaitEventState{
		opts: opts, townRoot: townRoot, eventDir: eventDir, beadsDir: beadsDir,
		idleCycles: idleCycles, backoffUntil: backoffUntil, timeout: timeout,
		contextCheckInterval: contextCheckInterval, resumed: resumed,
	}, nil
}

func validateAwaitEventChannel(channel string) error {
	if !channelevents.ValidChannelName.MatchString(channel) {
		return fmt.Errorf("invalid channel name %q: must match [a-zA-Z0-9_-]", channel)
	}
	return nil
}

func resolveAwaitEventTownRoot() string {
	townRoot, err := workspace.FindFromCwd()
	if err == nil && townRoot != "" {
		return townRoot
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "gt")
}

func loadAwaitEventTracking(opts awaitEventOptions) (string, int, time.Time) {
	if opts.agentBead == "" {
		return "", 0, time.Time{}
	}
	beadsDir, err := resolveAgentTrackingBeadsDir()
	if err != nil {
		return "", 0, time.Time{}
	}
	labels, err := getAgentLabels(opts.agentBead, beadsDir)
	if err != nil {
		if !opts.quiet {
			fmt.Printf("%s Could not read agent bead (starting at idle=0): %v\n", style.Dim.Render("⚠"), err)
		}
		return beadsDir, 0, time.Time{}
	}
	idleCycles, backoffUntil := parseAwaitEventTrackingLabels(labels)
	return beadsDir, idleCycles, backoffUntil
}

func parseAwaitEventTrackingLabels(labels map[string]string) (int, time.Time) {
	var idleCycles int
	if idleStr, ok := labels["idle"]; ok {
		if n, err := parseIntSimple(idleStr); err == nil {
			idleCycles = n
		}
	}
	var backoffUntil time.Time
	if untilStr, ok := labels["backoff-until"]; ok {
		if ts, err := parseIntSimple(untilStr); err == nil && ts > 0 {
			backoffUntil = time.Unix(int64(ts), 0)
		}
	}
	return idleCycles, backoffUntil
}

func parseAwaitEventContextInterval(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid context-check-interval: %w", err)
	}
	return interval, nil
}

func resumeAwaitEventWindow(opts awaitEventOptions, fullTimeout time.Duration, backoffUntil time.Time) (time.Duration, bool) {
	now := time.Now()
	if opts.agentBead == "" || !backoffUntil.After(now) {
		return fullTimeout, false
	}
	remaining := backoffUntil.Sub(now)
	if remaining > fullTimeout {
		return fullTimeout, false
	}
	if !opts.quiet && !opts.json {
		fmt.Printf("%s Resuming backoff window (%v remaining)\n", style.Dim.Render("↻"), remaining.Round(time.Second))
	}
	return remaining, true
}

func persistAwaitEventWindow(opts awaitEventOptions, beadsDir string, timeout time.Duration, resumed bool) {
	if opts.agentBead != "" && beadsDir != "" && !resumed {
		_ = setAgentBackoffUntil(opts.agentBead, beadsDir, time.Now().Add(timeout))
	}
}

func printAwaitEventStart(opts awaitEventOptions, timeout time.Duration, idleCycles int) {
	if !opts.quiet && !opts.json {
		fmt.Printf("%s Awaiting event on channel %q (timeout: %v, idle: %d)...\n", style.Dim.Render("⏳"), opts.channel, timeout, idleCycles)
	}
}

func waitForAwaitEvent(state *awaitEventState) (*AwaitEventResult, error) {
	startTime := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), state.timeout)
	defer cancel()
	result, err := waitForEventFiles(ctx, state.eventDir, state.contextCheckInterval)
	if err != nil {
		return nil, fmt.Errorf("event watch failed: %w", err)
	}
	result.Elapsed = time.Since(startTime)
	return result, nil
}

func updateAwaitEventTracking(state *awaitEventState, result *AwaitEventResult) {
	if state.opts.agentBead == "" || state.beadsDir == "" {
		return
	}
	_ = updateAgentHeartbeat(state.opts.agentBead, state.beadsDir)
	updateAwaitEventIdle(state, result)
	if result.Reason == "event" || result.Reason == "timeout" {
		_ = clearAgentBackoffUntil(state.opts.agentBead, state.beadsDir)
	}
}

func updateAwaitEventIdle(state *awaitEventState, result *AwaitEventResult) {
	switch result.Reason {
	case "timeout":
		newIdle := state.idleCycles + 1
		if err := setAgentIdleCycles(state.opts.agentBead, state.beadsDir, newIdle); err != nil {
			if !state.opts.quiet {
				fmt.Printf("%s Failed to update idle count: %v\n", style.Dim.Render("⚠"), err)
			}
			return
		}
		result.IdleCycles = newIdle
	case "event":
		if state.idleCycles > 0 {
			_ = setAgentIdleCycles(state.opts.agentBead, state.beadsDir, 0)
		}
		result.IdleCycles = 0
	}
}

func cleanupAwaitEvent(state *awaitEventState, result *AwaitEventResult) {
	if !state.opts.cleanup || result.Reason != "event" {
		return
	}
	for _, event := range result.Events {
		_ = os.Remove(event.Path)
	}
}

func outputAwaitEvent(opts awaitEventOptions, result *AwaitEventResult) error {
	if opts.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	if opts.quiet {
		return nil
	}
	printAwaitEventResult(result)
	printAwaitEventEffort(result.EffortLevel)
	return nil
}

func printAwaitEventResult(result *AwaitEventResult) {
	switch result.Reason {
	case "event":
		fmt.Printf("%s %d event(s) received after %v\n", style.Bold.Render("✓"), len(result.Events), result.Elapsed.Round(time.Millisecond))
		for _, event := range result.Events {
			printAwaitEventType(event)
		}
	case "timeout":
		fmt.Printf("%s Timeout after %v (idle cycle: %d)\n", style.Dim.Render("⏱"), result.Elapsed.Round(time.Millisecond), result.IdleCycles)
	case "context-yield":
		fmt.Printf("%s Context-check interval reached after %v\n", style.Dim.Render("↺"), result.Elapsed.Round(time.Millisecond))
		fmt.Printf("\n%s Assess context usage before re-entering event wait.\n", style.Bold.Render("CONTEXT: check"))
		fmt.Printf("If context is OK, call await-event again. If context is high, hand off.\n")
	}
}

func printAwaitEventType(event EventFile) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(event.Content, &parsed); err == nil {
		if eventType, ok := parsed["type"].(string); ok {
			fmt.Printf("  %s %s\n", style.Dim.Render("→"), eventType)
		}
	}
}

func printAwaitEventEffort(effort string) {
	if effort == "abbreviated" {
		fmt.Printf("\n%s Run ABBREVIATED patrol: quick checks only, skip optional steps.\n", style.Bold.Render("EFFORT: reduced"))
		return
	}
	fmt.Printf("\n%s Run full patrol.\n", style.Bold.Render("EFFORT: full"))
}

func effortLevelForAwaitResult(reason string, idleCycles int) string {
	if reason == "event" || reason == "context-yield" || idleCycles == 0 {
		return "full"
	}
	return "abbreviated"
}

// calculateEventTimeout mirrors calculateEffectiveTimeout for await-event.
func calculateEventTimeout(opts awaitEventOptions, idleCycles int) (time.Duration, error) {
	if opts.backoffBase == "" {
		return time.ParseDuration(opts.timeout)
	}
	base, err := time.ParseDuration(opts.backoffBase)
	if err != nil {
		return 0, fmt.Errorf("invalid backoff-base: %w", err)
	}
	maxDur, err := parseAwaitEventBackoffMax(opts.backoffMax)
	if err != nil {
		return 0, err
	}
	return applyAwaitEventBackoff(base, maxDur, opts.backoffMult, idleCycles), nil
}

func parseAwaitEventBackoffMax(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	maxDur, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid backoff-max: %w", err)
	}
	return maxDur, nil
}

func applyAwaitEventBackoff(base, maxDur time.Duration, multiplier, idleCycles int) time.Duration {
	timeout := base
	for i := 0; i < idleCycles; i++ {
		if maxDur > 0 && timeout >= maxDur {
			return maxDur
		}
		timeout *= time.Duration(multiplier)
	}
	if maxDur > 0 && timeout > maxDur {
		return maxDur
	}
	return timeout
}

// waitForEventFiles checks for pending events, then polls until events appear or timeout.
// Uses a polling loop instead of inotifywait for cross-platform compatibility.
//
// contextCheckAfter, when non-zero, causes an early return with reason "context-yield"
// after the given wall-clock duration. This allows the caller (a patrol agent) to
// assess context usage before re-entering the wait, preventing unbounded context
// accumulation during long idle periods.
func waitForEventFiles(ctx context.Context, eventDir string, contextCheckAfter time.Duration) (*AwaitEventResult, error) {
	events, err := readPendingEvents(eventDir)
	if err != nil {
		return nil, err
	}
	if len(events) > 0 {
		return &AwaitEventResult{
			Reason: "event",
			Events: events,
		}, nil
	}

	if result := eventWaitDeadlineResult(ctx); result != nil {
		return result, nil
	}

	contextYieldC, stopTimer := eventContextYieldTimer(contextCheckAfter)
	defer stopTimer()
	return pollEventFiles(ctx, eventDir, contextYieldC)
}

func eventWaitDeadlineResult(ctx context.Context) *AwaitEventResult {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 {
		return &AwaitEventResult{Reason: "timeout"}
	}
	return nil
}

func eventContextYieldTimer(after time.Duration) (<-chan time.Time, func()) {
	if after <= 0 {
		return nil, func() {}
	}
	timer := time.NewTimer(after)
	return timer.C, func() { timer.Stop() }
}

func pollEventFiles(ctx context.Context, eventDir string, contextYieldC <-chan time.Time) (*AwaitEventResult, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return finalizeEventWait(ctx, eventDir, "timeout"), nil
		case <-contextYieldC:
			return finalizeEventWait(ctx, eventDir, "context-yield"), nil
		case <-ticker.C:
			result, err := pollEventFilesOnce(ctx, eventDir)
			if err != nil {
				return nil, err
			}
			if result != nil {
				return result, nil
			}
		}
	}
}

func finalizeEventWait(ctx context.Context, eventDir, reason string) *AwaitEventResult {
	events := readPendingEventsBounded(ctx, eventDir, 500*time.Millisecond)
	if len(events) > 0 {
		return &AwaitEventResult{Reason: "event", Events: events}
	}
	return &AwaitEventResult{Reason: reason}
}

func pollEventFilesOnce(ctx context.Context, eventDir string) (*AwaitEventResult, error) {
	type readResult struct {
		events []EventFile
		err    error
	}
	ch := make(chan readResult, 1)
	go func() {
		events, err := readPendingEvents(eventDir)
		ch <- readResult{events: events, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, nil
	case result := <-ch:
		if result.err != nil {
			return nil, result.err
		}
		if len(result.events) == 0 {
			return nil, nil
		}
		return &AwaitEventResult{Reason: "event", Events: result.events}, nil
	}
}

// readPendingEventsBounded runs readPendingEvents in a goroutine and returns
// whatever it produces within the given budget, or nil if it doesn't finish.
// ctx is also honored — whichever deadline fires first wins.
func readPendingEventsBounded(ctx context.Context, dir string, budget time.Duration) []EventFile {
	ch := make(chan []EventFile, 1)
	go func() {
		events, _ := readPendingEvents(dir)
		ch <- events
	}()
	select {
	case events := <-ch:
		return events
	case <-time.After(budget):
		return nil
	case <-ctx.Done():
		// ctx already done — give the read a tiny grace window so we
		// don't drop events that were 1ms from arriving.
		select {
		case events := <-ch:
			return events
		case <-time.After(50 * time.Millisecond):
			return nil
		}
	}
}

// readPendingEvents reads all .event files from the directory.
func readPendingEvents(dir string) ([]EventFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var events []EventFile
	var paths []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".event") {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}

	sort.Strings(paths) // oldest first

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // skip unreadable files
		}
		events = append(events, EventFile{
			Path:    path,
			Content: json.RawMessage(data),
		})
	}

	return events, nil
}
