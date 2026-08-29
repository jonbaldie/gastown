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

	// Check if log file exists
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

	// Build filter
	filter := townlog.Filter{}

	if opts.typeName != "" {
		filter.Type = townlog.EventType(opts.typeName)
	}

	if opts.agent != "" {
		filter.Agent = opts.agent
	}

	if opts.since != "" {
		duration, err := time.ParseDuration(opts.since)
		if err != nil {
			return fmt.Errorf("invalid --since duration: %w", err)
		}
		filter.Since = time.Now().Add(-duration)
	}

	// Apply filter
	events = townlog.FilterEvents(events, filter)

	// Apply tail limit
	if opts.tail > 0 && len(events) > opts.tail {
		events = events[len(events)-opts.tail:]
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

	// Color-code event types
	var typeStr string
	switch e.Type {
	case townlog.EventSpawn:
		typeStr = style.Success.Render("[spawn]")
	case townlog.EventWake:
		typeStr = style.Bold.Render("[wake]")
	case townlog.EventNudge:
		typeStr = style.Dim.Render("[nudge]")
	case townlog.EventHandoff:
		typeStr = style.Bold.Render("[handoff]")
	case townlog.EventHandoffNoPersist:
		typeStr = style.Error.Render("[handoff-NOPERSIST]")
	case townlog.EventDone:
		typeStr = style.Success.Render("[done]")
	case townlog.EventCrash:
		typeStr = style.Error.Render("[crash]")
	case townlog.EventKill:
		typeStr = style.Warning.Render("[kill]")
	case townlog.EventCallback:
		typeStr = style.Bold.Render("[callback]")
	case townlog.EventPatrolStarted:
		typeStr = style.Bold.Render("[patrol_started]")
	case townlog.EventPolecatChecked:
		typeStr = style.Dim.Render("[polecat_checked]")
	case townlog.EventPolecatNudged:
		typeStr = style.Warning.Render("[polecat_nudged]")
	case townlog.EventEscalationSent:
		typeStr = style.Error.Render("[escalation_sent]")
	case townlog.EventPatrolComplete:
		typeStr = style.Success.Render("[patrol_complete]")
	default:
		typeStr = fmt.Sprintf("[%s]", e.Type)
	}

	detail := formatEventDetail(e)
	fmt.Printf("%s %s %s %s\n", style.Dim.Render(ts), typeStr, e.Agent, detail)
}

// formatEventDetail returns a human-readable detail string for an event.
func formatEventDetail(e townlog.Event) string {
	switch e.Type {
	case townlog.EventSpawn:
		if e.Context != "" {
			return fmt.Sprintf("spawned for %s", e.Context)
		}
		return "spawned"
	case townlog.EventWake:
		if e.Context != "" {
			return fmt.Sprintf("resumed (%s)", e.Context)
		}
		return "resumed"
	case townlog.EventNudge:
		if e.Context != "" {
			return fmt.Sprintf("nudged with %q", truncateStr(e.Context, 40))
		}
		return "nudged"
	case townlog.EventHandoff:
		if e.Context != "" {
			return fmt.Sprintf("handed off (%s)", e.Context)
		}
		return "handed off"
	case townlog.EventHandoffNoPersist:
		if e.Context != "" {
			return fmt.Sprintf("handoff FAILED (%s)", e.Context)
		}
		return "handoff FAILED (no persist)"
	case townlog.EventDone:
		if e.Context != "" {
			return fmt.Sprintf("completed %s", e.Context)
		}
		return "completed work"
	case townlog.EventCrash:
		if e.Context != "" {
			return fmt.Sprintf("exited unexpectedly (%s)", e.Context)
		}
		return "exited unexpectedly"
	case townlog.EventKill:
		if e.Context != "" {
			return fmt.Sprintf("killed (%s)", e.Context)
		}
		return "killed"
	case townlog.EventCallback:
		if e.Context != "" {
			return fmt.Sprintf("callback: %s", e.Context)
		}
		return "callback processed"
	case townlog.EventPatrolStarted:
		if e.Context != "" {
			return fmt.Sprintf("started patrol (%s)", e.Context)
		}
		return "started patrol"
	case townlog.EventPolecatChecked:
		if e.Context != "" {
			return fmt.Sprintf("checked %s", e.Context)
		}
		return "checked polecat"
	case townlog.EventPolecatNudged:
		if e.Context != "" {
			return fmt.Sprintf("nudged (%s)", e.Context)
		}
		return "nudged polecat"
	case townlog.EventEscalationSent:
		if e.Context != "" {
			return fmt.Sprintf("escalated (%s)", e.Context)
		}
		return "escalated"
	case townlog.EventPatrolComplete:
		if e.Context != "" {
			return fmt.Sprintf("patrol complete (%s)", e.Context)
		}
		return "patrol complete"
	default:
		if e.Context != "" {
			return fmt.Sprintf("%s (%s)", e.Type, e.Context)
		}
		return string(e.Type)
	}
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
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		// Try to find town root from conventional location
		// This is called from tmux hook which may not have proper cwd
		home := os.Getenv("HOME")
		defaultRoot := home + "/gt"
		if _, statErr := os.Stat(defaultRoot + "/mayor"); statErr == nil {
			townRoot = defaultRoot
		}
		if townRoot == "" {
			return fmt.Errorf("cannot find town root (tried cwd and ~/gt)")
		}
	}

	// Determine event type based on exit code
	var eventType townlog.EventType
	var context string

	if exitCode == 0 {
		// Exit code 0 = normal exit
		// Could be handoff, done, or user quit - we log as "done" if no prior done event
		// The Witness can analyze further if needed
		eventType = townlog.EventDone
		context = "exited normally"
	} else if exitCode == 130 {
		// Exit code 130 = Ctrl+C (SIGINT)
		// This is typically intentional user interrupt
		eventType = townlog.EventKill
		context = fmt.Sprintf("interrupted (exit %d)", exitCode)
	} else {
		// Non-zero exit = crash
		eventType = townlog.EventCrash
		context = fmt.Sprintf("exit code %d", exitCode)
		if session != "" {
			context += fmt.Sprintf(" (session: %s)", session)
		}
	}

	// Log the event
	logger := townlog.NewLogger(townRoot)
	if err := logger.Log(eventType, agent, context); err != nil {
		return fmt.Errorf("logging event: %w", err)
	}
	if eventType == townlog.EventCrash {
		logCrashFeedEvent(townRoot, agent, session, exitCode)
	}

	return nil
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
