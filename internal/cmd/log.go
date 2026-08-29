package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/townlog"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var logCmd = &cobra.Command{
	Use:     "log",
	GroupID: GroupDiag,
	Short:   "View town activity log",
	Long: `View the centralized log of Gas Town agent lifecycle events.

Events logged include:
  spawn   - new agent created
  wake    - agent resumed
  nudge   - message injected into agent
  handoff - agent handed off to fresh session
  done    - agent finished work
  crash   - agent exited unexpectedly
  kill    - agent killed intentionally

Examples:
  gt log                     # Show last 20 events
  gt log -n 50               # Show last 50 events
  gt log --type spawn        # Show only spawn events
  gt log --agent greenplace/    # Show events for gastown rig
  gt log --since 1h          # Show events from last hour
  gt log -f                  # Follow log (like tail -f)`,
	RunE: runLog,
}

var logCrashCmd = &cobra.Command{
	Use:   "crash",
	Short: "Record a crash event (called by tmux pane-died hook)",
	Long: `Record a crash event to the town log.

This command is called automatically by tmux when a pane exits unexpectedly.
It's not typically run manually.

The exit code determines if this was a crash or expected exit:
  - Exit code 0: Expected exit (logged as 'done' if no other done was recorded)
  - Exit code non-zero: Crash (logged as 'crash')

Examples:
  gt log crash --agent greenplace/Toast --session gt-greenplace-Toast --exit-code 1`,
	RunE: runLogCrash,
}

func init() {
	logCmd.Flags().IntP("tail", "n", 20, "Number of events to show")
	logCmd.Flags().StringP("type", "t", "", "Filter by event type (spawn,wake,nudge,handoff,done,crash,kill)")
	logCmd.Flags().StringP("agent", "a", "", "Filter by agent prefix (e.g., gastown/, greenplace/crew/max)")
	logCmd.Flags().String("since", "", "Show events since duration (e.g., 1h, 30m, 24h)")
	logCmd.Flags().BoolP("follow", "f", false, "Follow log output (like tail -f)")
	logCmd.Flags().Bool("acp", false, "View ACP debug logs (requires GT_ACP_DEBUG=1)")

	// crash subcommand flags
	logCrashCmd.Flags().String("agent", "", "Agent ID (e.g., greenplace/Toast)")
	logCrashCmd.Flags().String("session", "", "Tmux session name")
	logCrashCmd.Flags().Int("exit-code", -1, "Exit code from pane")
	_ = logCrashCmd.MarkFlagRequired("agent")

	logCmd.AddCommand(logCrashCmd)
	rootCmd.AddCommand(logCmd)
}

type logOptions struct {
	tail     int
	typeName string
	agent    string
	since    string
	follow   bool
	acp      bool
}

func runLog(cmd *cobra.Command, _ []string) error {
	opts := logOptions{
		tail:     commandIntFlag(cmd, "tail"),
		typeName: commandStringFlag(cmd, "type"),
		agent:    commandStringFlag(cmd, "agent"),
		since:    commandStringFlag(cmd, "since"),
		follow:   commandBoolFlag(cmd, "follow"),
		acp:      commandBoolFlag(cmd, "acp"),
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Handle --acp flag to view ACP debug logs
	if opts.acp {
		return viewACPLogs(townRoot, opts.tail, opts.follow)
	}

	logPath := fmt.Sprintf("%s/logs/town.log", townRoot)

	// If following, use tail -f
	if opts.follow {
		return followLog(logPath)
	}
	return printLogEvents(townRoot, logPath, opts)
}

func printLogEvents(townRoot, logPath string, opts logOptions) error {
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		fmt.Printf("%s No log file yet (no events recorded)\n", style.Dim.Render("○"))
		return nil
	}

	// Read events
	events, err := townlog.ReadEvents(townRoot)
	if err != nil {
		return fmt.Errorf("reading events: %w", err)
	}

	if len(events) == 0 {
		fmt.Printf("%s No events in log\n", style.Dim.Render("○"))
		return nil
	}

	events, err = filterLogEvents(events, opts)
	if err != nil {
		return err
	}

	if len(events) == 0 {
		fmt.Printf("%s No events match filter\n", style.Dim.Render("○"))
		return nil
	}

	// Print events
	for _, e := range events {
		printEvent(e)
	}

	return nil
}

func filterLogEvents(events []townlog.Event, opts logOptions) ([]townlog.Event, error) {
	filter := townlog.Filter{Type: townlog.EventType(opts.typeName), Agent: opts.agent}
	if opts.since != "" {
		duration, err := time.ParseDuration(opts.since)
		if err != nil {
			return nil, fmt.Errorf("invalid --since duration: %w", err)
		}
		filter.Since = time.Now().Add(-duration)
	}
	events = townlog.FilterEvents(events, filter)
	if opts.tail > 0 && len(events) > opts.tail {
		events = events[len(events)-opts.tail:]
	}
	return events, nil
}

// followLog uses tail -f to follow the log file.
func followLog(logPath string) error {
	// Check if log file exists, create empty if not
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		// Create logs directory and empty file
		if err := os.MkdirAll(fmt.Sprintf("%s", logPath[:len(logPath)-len("town.log")-1]), 0755); err != nil {
			return fmt.Errorf("creating logs directory: %w", err)
		}
		if _, err := os.Create(logPath); err != nil {
			return fmt.Errorf("creating log file: %w", err)
		}
	}

	fmt.Printf("%s Following %s (Ctrl+C to stop)\n\n", style.Dim.Render("○"), logPath)

	tailCmd := exec.Command("tail", "-f", logPath)
	tailCmd.Stdout = os.Stdout
	tailCmd.Stderr = os.Stderr

	return tailCmd.Run()
}

// viewACPLogs displays the ACP debug log file.
func viewACPLogs(townRoot string, tail int, follow bool) error {
	logPath := fmt.Sprintf("%s/logs/acp.log", townRoot)

	// If following, use tail -f
	if follow {
		return followLog(logPath)
	}

	// Check if log file exists
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		fmt.Printf("%s No ACP log file. Set GT_ACP_DEBUG=1 to enable logging.\n", style.Dim.Render("○"))
		return nil
	}

	// Read the log file
	content, err := os.ReadFile(logPath)
	if err != nil {
		return fmt.Errorf("reading ACP log: %w", err)
	}

	lines := strings.Split(string(content), "\n")

	// Apply tail limit
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}

	// Print lines
	for _, line := range lines {
		if line != "" {
			fmt.Println(line)
		}
	}

	return nil
}

// printEvent prints a single event with styling.
func printEvent(e townlog.Event) {
	ts := e.Timestamp.Format("2006-01-02 15:04:05")
	fmt.Printf("%s %s %s %s\n", style.Dim.Render(ts), eventTypeLabel(e.Type), e.Agent, formatEventDetail(e))
}

func eventTypeLabel(eventType townlog.EventType) string {
	labels := map[townlog.EventType]string{
		townlog.EventSpawn:            style.Success.Render("[spawn]"),
		townlog.EventWake:             style.Bold.Render("[wake]"),
		townlog.EventNudge:            style.Dim.Render("[nudge]"),
		townlog.EventHandoff:          style.Bold.Render("[handoff]"),
		townlog.EventHandoffNoPersist: style.Error.Render("[handoff-NOPERSIST]"),
		townlog.EventDone:             style.Success.Render("[done]"),
		townlog.EventCrash:            style.Error.Render("[crash]"),
		townlog.EventKill:             style.Warning.Render("[kill]"),
		townlog.EventCallback:         style.Bold.Render("[callback]"),
		townlog.EventPatrolStarted:    style.Bold.Render("[patrol_started]"),
		townlog.EventPolecatChecked:   style.Dim.Render("[polecat_checked]"),
		townlog.EventPolecatNudged:    style.Warning.Render("[polecat_nudged]"),
		townlog.EventEscalationSent:   style.Error.Render("[escalation_sent]"),
		townlog.EventPatrolComplete:   style.Success.Render("[patrol_complete]"),
	}
	if label, ok := labels[eventType]; ok {
		return label
	}
	return fmt.Sprintf("[%s]", eventType)
}

// formatEventDetail returns a human-readable detail string for an event.
func formatEventDetail(e townlog.Event) string {
	formats := map[townlog.EventType]struct {
		format string
		empty  string
	}{
		townlog.EventSpawn:            {"spawned for %s", "spawned"},
		townlog.EventWake:             {"resumed (%s)", "resumed"},
		townlog.EventNudge:            {"nudged with %q", "nudged"},
		townlog.EventHandoff:          {"handed off (%s)", "handed off"},
		townlog.EventHandoffNoPersist: {"handoff FAILED (%s)", "handoff FAILED (no persist)"},
		townlog.EventDone:             {"completed %s", "completed work"},
		townlog.EventCrash:            {"exited unexpectedly (%s)", "exited unexpectedly"},
		townlog.EventKill:             {"killed (%s)", "killed"},
		townlog.EventCallback:         {"callback: %s", "callback processed"},
		townlog.EventPatrolStarted:    {"started patrol (%s)", "started patrol"},
		townlog.EventPolecatChecked:   {"checked %s", "checked polecat"},
		townlog.EventPolecatNudged:    {"nudged (%s)", "nudged polecat"},
		townlog.EventEscalationSent:   {"escalated (%s)", "escalated"},
		townlog.EventPatrolComplete:   {"patrol complete (%s)", "patrol complete"},
	}
	spec, ok := formats[e.Type]
	if !ok {
		if e.Context == "" {
			return string(e.Type)
		}
		return fmt.Sprintf("%s (%s)", e.Type, e.Context)
	}
	if e.Context == "" {
		return spec.empty
	}
	context := e.Context
	if e.Type == townlog.EventNudge {
		context = truncateStr(context, 40)
	}
	return fmt.Sprintf(spec.format, context)
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// runLogCrash handles the "gt log crash" command from tmux pane-died hooks.
func runLogCrash(cmd *cobra.Command, _ []string) error {
	agent := commandStringFlag(cmd, "agent")
	session := commandStringFlag(cmd, "session")
	exitCode := commandIntFlag(cmd, "exit-code")
	townRoot, err := crashTownRoot()
	if err != nil {
		return err
	}

	eventType, eventContext := crashEvent(exitCode, session)

	logger := townlog.NewLogger(townRoot)
	if err := logger.Log(eventType, agent, eventContext); err != nil {
		return fmt.Errorf("logging event: %w", err)
	}
	if eventType == townlog.EventCrash {
		logCrashFeedEvent(townRoot, agent, session, exitCode)
	}

	return nil
}

func crashTownRoot() (string, error) {
	townRoot, err := workspace.FindFromCwd()
	if err == nil && townRoot != "" {
		return townRoot, nil
	}
	defaultRoot := os.Getenv("HOME") + "/gt"
	if _, statErr := os.Stat(defaultRoot + "/mayor"); statErr == nil {
		return defaultRoot, nil
	}
	if townRoot == "" {
		return "", fmt.Errorf("cannot find town root (tried cwd and ~/gt)")
	}
	return townRoot, nil
}

func crashEvent(exitCode int, session string) (townlog.EventType, string) {
	if exitCode == 0 {
		return townlog.EventDone, "exited normally"
	}
	if exitCode == 130 {
		return townlog.EventKill, fmt.Sprintf("interrupted (exit %d)", exitCode)
	}
	context := fmt.Sprintf("exit code %d", exitCode)
	if session != "" {
		context += fmt.Sprintf(" (session: %s)", session)
	}
	return townlog.EventCrash, context
}

func logCrashFeedEvent(townRoot, agent, session string, exitCode int) {
	if townRoot == "" {
		return
	}
	if session == "" {
		session = "unknown"
	}

	origDir, getwdErr := os.Getwd()
	if err := os.Chdir(townRoot); err != nil {
		return
	}
	if getwdErr == nil {
		defer func() { _ = os.Chdir(origDir) }()
	}

	reason := fmt.Sprintf("crashed with exit code %d", exitCode)
	payload := events.SessionDeathPayload(session, agent, reason, "gt log crash")
	payload["exit_code"] = exitCode
	_ = events.LogFeed(events.TypeSessionDeath, agent, payload)
}

// LogEvent is a helper that logs an event from anywhere in the codebase.
// It finds the town root and logs the event.
func LogEvent(eventType townlog.EventType, agent, context string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return err // Silently fail if not in a workspace
	}
	if townRoot == "" {
		return nil
	}

	logger := townlog.NewLogger(townRoot)
	return logger.Log(eventType, agent, context)
}

// LogEventWithRoot logs an event when the town root is already known.
func LogEventWithRoot(townRoot string, eventType townlog.EventType, agent, context string) error {
	logger := townlog.NewLogger(townRoot)
	return logger.Log(eventType, agent, context)
}

// Convenience functions for common events

// LogSpawn logs a spawn event.
func LogSpawn(townRoot, agent, issueID string) error {
	return LogEventWithRoot(townRoot, townlog.EventSpawn, agent, issueID)
}

// LogWake logs a wake event.
func LogWake(townRoot, agent, context string) error {
	return LogEventWithRoot(townRoot, townlog.EventWake, agent, context)
}

// LogNudge logs a nudge event.
func LogNudge(townRoot, agent, message string) error {
	return LogEventWithRoot(townRoot, townlog.EventNudge, agent, strings.TrimSpace(message))
}

// LogHandoff logs a handoff event.
func LogHandoff(townRoot, agent, context string) error {
	return LogEventWithRoot(townRoot, townlog.EventHandoff, agent, context)
}

// LogHandoffNoPersist logs a failed handoff where Dolt persistence failed.
// Creates a distinct marker in town.log so crash recovery can identify
// handoffs that were attempted but never persisted to Dolt.
func LogHandoffNoPersist(townRoot, agent, context string, persistErr error) error {
	msg := context
	if persistErr != nil {
		msg = fmt.Sprintf("%s — error: %v", context, persistErr)
	}
	return LogEventWithRoot(townRoot, townlog.EventHandoffNoPersist, agent, msg)
}

// LogDone logs a done event.
func LogDone(townRoot, agent, issueID string) error {
	return LogEventWithRoot(townRoot, townlog.EventDone, agent, issueID)
}

// LogCrash logs a crash event.
func LogCrash(townRoot, agent, reason string) error {
	return LogEventWithRoot(townRoot, townlog.EventCrash, agent, reason)
}

// LogKill logs a kill event.
func LogKill(townRoot, agent, reason string) error {
	return LogEventWithRoot(townRoot, townlog.EventKill, agent, reason)
}
