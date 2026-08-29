package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type awaitSignalOptions struct {
	timeout     string
	backoffBase string
	backoffMult int
	backoffMax  string
	quiet       bool
	agentBead   string
	json        bool
}

func awaitSignalOptionsFromCommand(cmd *cobra.Command) awaitSignalOptions {
	return awaitSignalOptions{
		timeout:     commandStringFlag(cmd, "timeout"),
		backoffBase: commandStringFlag(cmd, "backoff-base"),
		backoffMult: commandIntFlag(cmd, "backoff-mult"),
		backoffMax:  commandStringFlag(cmd, "backoff-max"),
		quiet:       commandBoolFlag(cmd, "quiet"),
		agentBead:   commandStringFlag(cmd, "agent-bead"),
		json:        commandBoolFlag(cmd, "json"),
	}
}

var moleculeAwaitSignalCmd = &cobra.Command{
	Use:   "await-signal",
	Short: "Wait for activity feed signal with timeout",
	Long: `Wait for any activity on the events feed, with optional backoff.

This command is the primary wake mechanism for patrol agents. It tails
~/gt/.events.jsonl and returns immediately when a new event is appended
(indicating Gas Town activity such as slings, nudges, mail, spawns, etc.).

If no activity occurs within the timeout, the command returns with exit code 0
but sets the AWAIT_SIGNAL_REASON environment variable to "timeout".

The timeout can be specified directly or via backoff configuration for
exponential wait patterns.

BACKOFF MODE:
When backoff parameters are provided, the effective timeout is calculated as:
  min(base * multiplier^idle_cycles, max)

The idle_cycles value is read from the agent bead's "idle" label, enabling
exponential backoff that persists across invocations. When a signal is
received, the caller should reset idle:0 on the agent bead.

EXIT CODES:
  0 - Signal received or timeout (check output for which)
  1 - Error opening events file

EXAMPLES:
  # Simple wait with 60s timeout (canonical form)
  gt mol step await-signal --timeout 60s

  # Short form (alias)
  gt mol await-signal --timeout 60s

  # Backoff mode with agent bead tracking:
  gt mol await-signal --agent-bead gt-gastown-witness \
    --backoff-base 30s --backoff-mult 2 --backoff-max 15m

  # On timeout, the agent bead's idle:N label is auto-incremented
  # On signal, caller should reset: gt agents state gt-gastown-witness --set idle=0

  # Quiet mode (no output, for scripting)
  gt mol await-signal --timeout 30s --quiet`,
	RunE: runMoleculeAwaitSignal,
}

// moleculeAwaitSignalShortcutCmd is a separate command instance that allows
// "gt mol await-signal" in addition to the canonical "gt mol step await-signal".
// A separate instance is required because cobra does not support a single
// command having two parents (AddCommand overwrites the parent pointer).
var moleculeAwaitSignalShortcutCmd = &cobra.Command{
	Use:   "await-signal",
	Short: "Wait for activity feed signal with timeout (alias: gt mol step await-signal)",
	Long:  moleculeAwaitSignalCmd.Long,
	RunE:  runMoleculeAwaitSignal,
}

// AwaitSignalResult is the result of an await-signal operation.
type AwaitSignalResult struct {
	Reason      string        `json:"reason"`                // "signal" or "timeout"
	Elapsed     time.Duration `json:"elapsed"`               // how long we waited
	Signal      string        `json:"signal,omitempty"`      // the line that woke us (if signal)
	IdleCycles  int           `json:"idle_cycles,omitempty"` // current idle cycle count (after update)
	EffortLevel string        `json:"effort_level"`          // "full" or "abbreviated"
}

func init() {
	state := moleculeState()
	moleculeAwaitSignalCmd.Flags().String("timeout", "60s",
		"Maximum time to wait for signal (e.g., 30s, 5m)")
	moleculeAwaitSignalCmd.Flags().String("backoff-base", "",
		"Base interval for exponential backoff (e.g., 30s)")
	moleculeAwaitSignalCmd.Flags().Int("backoff-mult", 2,
		"Multiplier for exponential backoff (default: 2)")
	moleculeAwaitSignalCmd.Flags().String("backoff-max", "",
		"Maximum interval cap for backoff (e.g., 10m)")
	moleculeAwaitSignalCmd.Flags().String("agent-bead", "",
		"Agent bead ID for tracking idle cycles (reads/writes idle:N label)")
	moleculeAwaitSignalCmd.Flags().Bool("quiet", false,
		"Suppress output (for scripting)")
	moleculeAwaitSignalCmd.Flags().BoolVar(&state.json, "json", false,
		"Output as JSON")

	moleculeStepCmd.AddCommand(moleculeAwaitSignalCmd)

	// Register shortcut flags on the shortcut command.
	moleculeAwaitSignalShortcutCmd.Flags().String("timeout", "60s",
		"Maximum time to wait for signal (e.g., 30s, 5m)")
	moleculeAwaitSignalShortcutCmd.Flags().String("backoff-base", "",
		"Base interval for exponential backoff (e.g., 30s)")
	moleculeAwaitSignalShortcutCmd.Flags().Int("backoff-mult", 2,
		"Multiplier for exponential backoff (default: 2)")
	moleculeAwaitSignalShortcutCmd.Flags().String("backoff-max", "",
		"Maximum interval cap for backoff (e.g., 10m)")
	moleculeAwaitSignalShortcutCmd.Flags().String("agent-bead", "",
		"Agent bead ID for tracking idle cycles (reads/writes idle:N label)")
	moleculeAwaitSignalShortcutCmd.Flags().Bool("quiet", false,
		"Suppress output (for scripting)")
	moleculeAwaitSignalShortcutCmd.Flags().BoolVar(&state.json, "json", false,
		"Output as JSON")

	// alias: gt mol await-signal (in addition to gt mol step await-signal)
	moleculeCmd.AddCommand(moleculeAwaitSignalShortcutCmd)
}

type moleculeAwaitRun struct {
	opts         awaitSignalOptions
	beadsDir     string
	townRoot     string
	idleCycles   int
	backoffUntil time.Time
	timeout      time.Duration
	resumed      bool
}

func runMoleculeAwaitSignal(cmd *cobra.Command, _ []string) error {
	r, err := beginMoleculeAwait(cmd)
	if err != nil {
		return err
	}
	if err := prepareMoleculeAwaitTimeout(r); err != nil {
		return err
	}
	printMoleculeAwaitStart(r)
	result, err := waitMoleculeAwait(r)
	if err != nil {
		return err
	}
	applyMoleculeAwaitResult(r, result)
	return printMoleculeAwaitResult(r.opts, result)
}

func beginMoleculeAwait(cmd *cobra.Command) (*moleculeAwaitRun, error) {
	beadsDir, err := resolveAgentTrackingBeadsDir()
	if err != nil {
		return nil, fmt.Errorf("not in a beads workspace: %w", err)
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	opts := awaitSignalOptionsFromCommand(cmd)
	idleCycles, backoffUntil := loadMoleculeAwaitState(opts, beadsDir)
	return &moleculeAwaitRun{
		opts:         opts,
		beadsDir:     beadsDir,
		townRoot:     townRoot,
		idleCycles:   idleCycles,
		backoffUntil: backoffUntil,
	}, nil
}

func loadMoleculeAwaitState(opts awaitSignalOptions, beadsDir string) (int, time.Time) {
	if opts.agentBead == "" {
		return 0, time.Time{}
	}
	labels, err := getAgentLabels(opts.agentBead, beadsDir)
	if err != nil {
		if !opts.quiet {
			fmt.Printf("%s Could not read agent bead (starting at idle=0): %v\n",
				style.Dim.Render("⚠"), err)
		}
		return 0, time.Time{}
	}
	return parseMoleculeAwaitIdle(labels), parseMoleculeAwaitBackoffUntil(labels)
}

func parseMoleculeAwaitIdle(labels map[string]string) int {
	idleStr, ok := labels["idle"]
	if !ok {
		return 0
	}
	n, err := parseIntSimple(idleStr)
	if err != nil {
		return 0
	}
	return n
}

func parseMoleculeAwaitBackoffUntil(labels map[string]string) time.Time {
	untilStr, ok := labels["backoff-until"]
	if !ok {
		return time.Time{}
	}
	ts, err := parseIntSimple(untilStr)
	if err != nil || ts <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(ts), 0)
}

func prepareMoleculeAwaitTimeout(r *moleculeAwaitRun) error {
	fullTimeout, err := calculateEffectiveTimeout(r.opts, r.idleCycles)
	if err != nil {
		return fmt.Errorf("invalid timeout configuration: %w", err)
	}
	now := time.Now()
	r.timeout = resumeMoleculeAwaitTimeout(r, fullTimeout, now)
	persistMoleculeAwaitBackoff(r, now)
	return nil
}

func resumeMoleculeAwaitTimeout(r *moleculeAwaitRun, fullTimeout time.Duration, now time.Time) time.Duration {
	if r.opts.agentBead == "" || r.backoffUntil.IsZero() || !r.backoffUntil.After(now) {
		return fullTimeout
	}
	remaining := r.backoffUntil.Sub(now)
	if remaining > fullTimeout {
		return fullTimeout
	}
	r.resumed = true
	return remaining
}

func persistMoleculeAwaitBackoff(r *moleculeAwaitRun, now time.Time) {
	if r.opts.agentBead == "" || r.resumed {
		return
	}
	if err := setAgentBackoffUntil(r.opts.agentBead, r.beadsDir, now.Add(r.timeout)); err != nil && !r.opts.quiet {
		fmt.Printf("%s Failed to persist backoff window: %v\n",
			style.Dim.Render("⚠"), err)
	}
}

func printMoleculeAwaitStart(r *moleculeAwaitRun) {
	if r.opts.quiet || r.opts.json {
		return
	}
	if r.resumed {
		fmt.Printf("%s Resuming backoff (remaining: %v, idle: %d)...\n",
			style.Dim.Render("⏳"), r.timeout.Round(time.Second), r.idleCycles)
		return
	}
	if r.opts.agentBead != "" {
		fmt.Printf("%s Awaiting signal (timeout: %v, idle: %d)...\n",
			style.Dim.Render("⏳"), r.timeout, r.idleCycles)
		return
	}
	fmt.Printf("%s Awaiting signal (timeout: %v)...\n", style.Dim.Render("⏳"), r.timeout)
}

func waitMoleculeAwait(r *moleculeAwaitRun) (*AwaitSignalResult, error) {
	startTime := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	result, err := waitForActivitySignal(ctx, r.townRoot)
	if err != nil {
		return nil, fmt.Errorf("feed subscription failed: %w", err)
	}
	result.Elapsed = time.Since(startTime)
	return result, nil
}

func applyMoleculeAwaitResult(r *moleculeAwaitRun, result *AwaitSignalResult) {
	if r.opts.agentBead == "" {
		setMoleculeAwaitEffort(result)
		return
	}
	if result.Reason == "timeout" {
		applyMoleculeAwaitTimeout(r, result)
	} else if result.Reason == "signal" {
		applyMoleculeAwaitSignal(r, result)
	}
	setMoleculeAwaitEffort(result)
}

func applyMoleculeAwaitTimeout(r *moleculeAwaitRun, result *AwaitSignalResult) {
	newIdleCycles := r.idleCycles + 1
	if err := setAgentIdleCycles(r.opts.agentBead, r.beadsDir, newIdleCycles); err != nil {
		if !r.opts.quiet {
			fmt.Printf("%s Failed to update agent bead idle count: %v\n",
				style.Dim.Render("⚠"), err)
		}
	} else {
		result.IdleCycles = newIdleCycles
	}
	warnMoleculeAwaitHeartbeat(r)
	_ = clearAgentBackoffUntil(r.opts.agentBead, r.beadsDir)
}

func applyMoleculeAwaitSignal(r *moleculeAwaitRun, result *AwaitSignalResult) {
	warnMoleculeAwaitHeartbeat(r)
	result.IdleCycles = r.idleCycles
	_ = clearAgentBackoffUntil(r.opts.agentBead, r.beadsDir)
}

func warnMoleculeAwaitHeartbeat(r *moleculeAwaitRun) {
	if err := updateAgentHeartbeat(r.opts.agentBead, r.beadsDir); err != nil && !r.opts.quiet {
		fmt.Printf("%s Failed to update agent heartbeat: %v\n",
			style.Dim.Render("⚠"), err)
	}
}

func setMoleculeAwaitEffort(result *AwaitSignalResult) {
	if result.Reason == "signal" || result.IdleCycles == 0 {
		result.EffortLevel = "full"
		return
	}
	result.EffortLevel = "abbreviated"
}

func printMoleculeAwaitResult(opts awaitSignalOptions, result *AwaitSignalResult) error {
	if opts.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	if opts.quiet {
		return nil
	}
	printMoleculeAwaitReason(opts, result)
	if result.EffortLevel == "abbreviated" {
		fmt.Printf("\n%s Run ABBREVIATED patrol: quick checks only, skip optional steps.\n",
			style.Bold.Render("EFFORT: reduced"))
		return nil
	}
	fmt.Printf("\n%s Run full patrol.\n", style.Bold.Render("EFFORT: full"))
	return nil
}

func printMoleculeAwaitReason(opts awaitSignalOptions, result *AwaitSignalResult) {
	switch result.Reason {
	case "signal":
		fmt.Printf("%s Signal received after %v\n",
			style.Bold.Render("✓"), result.Elapsed.Round(time.Millisecond))
		if result.Signal == "" {
			return
		}
		sig := result.Signal
		if len(sig) > 80 {
			sig = sig[:77] + "..."
		}
		fmt.Printf("  %s\n", style.Dim.Render(sig))
	case "timeout":
		if opts.agentBead != "" {
			fmt.Printf("%s Timeout after %v (idle cycle: %d)\n",
				style.Dim.Render("⏱"), result.Elapsed.Round(time.Millisecond), result.IdleCycles)
			return
		}
		fmt.Printf("%s Timeout after %v (no activity)\n",
			style.Dim.Render("⏱"), result.Elapsed.Round(time.Millisecond))
	}
}

// calculateEffectiveTimeout determines the timeout based on flags.
// If backoff parameters are provided, uses exponential backoff formula:
//
//	min(base * multiplier^idleCycles, max)
//
// Otherwise uses the simple --timeout value.
func calculateEffectiveTimeout(opts awaitSignalOptions, idleCycles int) (time.Duration, error) {
	if opts.backoffBase == "" {
		return time.ParseDuration(opts.timeout)
	}
	return calculateBackoffTimeout(opts, idleCycles)
}

func calculateBackoffTimeout(opts awaitSignalOptions, idleCycles int) (time.Duration, error) {
	base, err := time.ParseDuration(opts.backoffBase)
	if err != nil {
		return 0, fmt.Errorf("invalid backoff-base: %w", err)
	}
	maxDur, err := parseBackoffMax(opts.backoffMax)
	if err != nil {
		return 0, err
	}
	timeout := base
	for i := 0; i < idleCycles; i++ {
		if maxDur > 0 && timeout >= maxDur {
			return maxDur, nil
		}
		timeout *= time.Duration(opts.backoffMult)
	}
	if maxDur > 0 && timeout > maxDur {
		return maxDur, nil
	}
	return timeout, nil
}

func parseBackoffMax(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	maxDur, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid backoff-max: %w", err)
	}
	return maxDur, nil
}

// waitForActivitySignal tails the events file for new activity.
// townRoot is the Gas Town workspace root; the events file is at
// <townRoot>/.events.jsonl. Returns immediately when a new event line is
// appended, or when context is canceled.
func waitForActivitySignal(ctx context.Context, townRoot string) (*AwaitSignalResult, error) {
	return waitForEventsFile(ctx, filepath.Join(townRoot, events.EventsFile))
}

// waitForEventsFile tails the events file for new lines.
// This replaces the former bd activity --follow subprocess approach.
func waitForEventsFile(ctx context.Context, eventsPath string) (*AwaitSignalResult, error) {

	f, err := os.OpenFile(eventsPath, os.O_RDONLY|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening events file %s: %w", eventsPath, err)
	}
	defer f.Close()

	// Seek to end — we only want new events, not historical ones
	if _, err := f.Seek(0, 2); err != nil {
		return nil, fmt.Errorf("seeking to end of events file: %w", err)
	}

	// Poll for new lines using bufio.Reader (not Scanner, which doesn't
	// resume after EOF). Reader.ReadString properly retries the underlying
	// file reader, picking up appended data between polls.
	reader := bufio.NewReader(f)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return &AwaitSignalResult{Reason: "timeout"}, nil
		case <-ticker.C:
			result, err := readEventsFileSignal(reader)
			if result != nil || err != nil {
				return result, err
			}
		}
	}
}

func readEventsFileSignal(reader *bufio.Reader) (*AwaitSignalResult, error) {
	line, err := reader.ReadString('\n')
	if err == nil && line != "" {
		return &AwaitSignalResult{
			Reason: "signal",
			Signal: strings.TrimRight(line, "\n"),
		}, nil
	}
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("reading events file: %w", err)
	}
	return nil, nil
}

// parseIntSimple parses a string to int without using strconv.
func parseIntSimple(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty string")
	}
	n := 0
	for _, char := range s {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("invalid integer: %s", s)
		}
		n = n*10 + int(char-'0')
	}
	return n, nil
}

// updateAgentHeartbeat records a heartbeat timestamp on an agent bead via a
// heartbeat:EPOCH label. This proves the agent is alive during long idle periods.
//
// bd agent heartbeat was never shipped (steveyegge/beads#2828). We use the same
// read-modify-write label pattern as setAgentIdleCycles instead.
func updateAgentHeartbeat(agentBead, beadsDir string) error {
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	var newLabels []string
	for _, label := range allLabels {
		if len(label) > 10 && label[:10] == "heartbeat:" {
			continue // Replace existing heartbeat label
		}
		newLabels = append(newLabels, label)
	}
	newLabels = append(newLabels, fmt.Sprintf("heartbeat:%d", time.Now().Unix()))

	args := []string{"update", agentBead}
	for _, label := range newLabels {
		args = append(args, "--set-labels="+label)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.MutationPinned, args...)
	return cmd.Run()
}

// setAgentIdleCycles sets the idle:N label on an agent bead.
// Uses read-modify-write pattern to update only the idle label.
func setAgentIdleCycles(agentBead, beadsDir string, cycles int) error {
	// Read all current labels
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	// Build new label list: keep non-idle labels, add new idle value
	var newLabels []string
	for _, label := range allLabels {
		// Skip any existing idle:* label
		if len(label) > 5 && label[:5] == "idle:" {
			continue
		}
		newLabels = append(newLabels, label)
	}

	// Add new idle value
	newLabels = append(newLabels, fmt.Sprintf("idle:%d", cycles))

	// Use bd update with --set-labels to replace all labels
	args := []string{"update", agentBead}
	for _, label := range newLabels {
		args = append(args, "--set-labels="+label)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.MutationPinned, args...)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setting idle label: %w", err)
	}

	return nil
}

// setAgentBackoffUntil persists a backoff-until:TIMESTAMP label on the agent bead.
// This allows interrupted await-signal invocations to resume with remaining time
// instead of restarting the full backoff period.
func setAgentBackoffUntil(agentBead, beadsDir string, until time.Time) error {
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	var newLabels []string
	for _, label := range allLabels {
		if len(label) > 14 && label[:14] == "backoff-until:" {
			continue // Strip existing backoff-until
		}
		newLabels = append(newLabels, label)
	}
	newLabels = append(newLabels, fmt.Sprintf("backoff-until:%d", until.Unix()))

	args := []string{"update", agentBead}
	for _, label := range newLabels {
		args = append(args, "--set-labels="+label)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.MutationPinned, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setting backoff-until label: %w", err)
	}
	return nil
}

// clearAgentBackoffUntil removes the backoff-until label from the agent bead.
// Called when await-signal completes normally (timeout or signal received).
func clearAgentBackoffUntil(agentBead, beadsDir string) error {
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	var newLabels []string
	found := false
	for _, label := range allLabels {
		if len(label) > 14 && label[:14] == "backoff-until:" {
			found = true
			continue // Strip backoff-until
		}
		newLabels = append(newLabels, label)
	}

	if !found {
		return nil // Nothing to clear
	}

	args := []string{"update", agentBead}
	if len(newLabels) == 0 {
		args = append(args, "--set-labels=")
	} else {
		for _, label := range newLabels {
			args = append(args, "--set-labels="+label)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.MutationPinned, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clearing backoff-until label: %w", err)
	}
	return nil
}
