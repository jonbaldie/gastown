package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/nudge"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type nudgePollerConfig struct {
	townRoot           string
	sessionName        string
	pollInterval       time.Duration
	idleTimeout        time.Duration
	tmux               *tmux.Tmux
	nudgeOpts          tmux.NudgeOpts
	hasPromptDetection bool
}

func init() {
	rootCmd.AddCommand(nudgePollerCmd)
	nudgePollerCmd.Flags().String("interval", nudge.DefaultPollInterval, "Poll interval (e.g., 10s, 30s)")
	nudgePollerCmd.Flags().String("idle-timeout", nudge.DefaultIdleTimeout, "How long to wait for agent idle before skipping")
}

var nudgePollerCmd = &cobra.Command{
	Use:    "nudge-poller <session>",
	Short:  "Background nudge queue poller for non-Claude agents",
	Hidden: true, // Internal command — launched by crew manager, not by users.
	Long: `Polls the nudge queue for a tmux session and drains it when the agent
is idle. This is the background equivalent of Claude's UserPromptSubmit hook
drain — it ensures queued nudges are delivered to agents that lack
turn-boundary hooks (Gemini, Codex, Cursor, etc.).

This command runs as a long-lived background process. It exits when:
  - The target tmux session dies
  - It receives SIGTERM (from StopPoller or session teardown)
  - The poll loop encounters an unrecoverable error

Normally launched automatically by 'gt crew start' for non-Claude agents.
Not intended for direct user invocation.`,
	Args: cobra.ExactArgs(1),
	RunE: runNudgePoller,
}

func runNudgePoller(cmd *cobra.Command, args []string) error {
	sessionName := args[0]
	intervalFlag := commandStringFlag(cmd, "interval")
	idleTimeoutFlag := commandStringFlag(cmd, "idle-timeout")
	cfg, err := loadNudgePollerConfig(sessionName, intervalFlag, idleTimeoutFlag)
	if err != nil {
		return err
	}
	return runNudgePollerLoop(cfg)
}

func loadNudgePollerConfig(sessionName, intervalFlag, idleTimeoutFlag string) (nudgePollerConfig, error) {
	cfg := nudgePollerConfig{sessionName: sessionName}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nudgePollerConfig{}, fmt.Errorf("cannot find town root: %w", err)
	}
	cfg.townRoot = townRoot

	pollInterval, err := time.ParseDuration(intervalFlag)
	if err != nil {
		return nudgePollerConfig{}, fmt.Errorf("invalid --interval: %w", err)
	}
	cfg.pollInterval = pollInterval

	idleTimeout, err := time.ParseDuration(idleTimeoutFlag)
	if err != nil {
		return nudgePollerConfig{}, fmt.Errorf("invalid --idle-timeout: %w", err)
	}
	cfg.idleTimeout = idleTimeout

	cfg.tmux = tmux.NewTmux()

	// Verify session exists before starting the loop.
	if exists, _ := cfg.tmux.HasSession(sessionName); !exists {
		return nudgePollerConfig{}, fmt.Errorf("session %q not found", sessionName)
	}

	// Resolve nudge options once at startup: if the target agent uses Escape
	// as cancel (e.g., Gemini CLI), skip the Escape keystroke during delivery
	// to avoid canceling in-flight generation. (GH#gt-wasn)
	if name, err := cfg.tmux.GetEnvironment(sessionName, "GT_AGENT"); err == nil && name != "" {
		if preset := config.GetAgentPresetByName(name); preset != nil {
			cfg.hasPromptDetection = preset.ReadyPromptPrefix != ""
			if preset.EscapeCancelsRequest {
				cfg.nudgeOpts.SkipEscape = true
			}
		}
	}
	return cfg, nil
}

func runNudgePollerLoop(cfg nudgePollerConfig) error {
	// Set up signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			return nil // graceful shutdown

		case <-ticker.C:
			if !pollNudgeQueue(cfg) {
				return nil // session gone, exit
			}
		}
	}
}

func pollNudgeQueue(cfg nudgePollerConfig) bool {
	if exists, _ := cfg.tmux.HasSession(cfg.sessionName); !exists {
		return false
	}

	if n, _ := nudge.Pending(cfg.townRoot, cfg.sessionName); n == 0 {
		return true
	}

	// For runtimes with prompt detection, defer delivery until the session
	// is actually idle. Runtimes without prompt detection preserve the old
	// best-effort behavior and drain on the poll interval.
	waitErr := cfg.tmux.WaitForIdle(cfg.sessionName, cfg.idleTimeout)
	if shouldSkipDrainUntilIdle(cfg.hasPromptDetection, waitErr) {
		return true
	}

	drained, err := nudge.Drain(cfg.townRoot, cfg.sessionName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nudge-poller: drain error for %s: %v\n", cfg.sessionName, err)
		return true
	}
	if len(drained) == 0 {
		return true // someone else drained it
	}

	formatted := nudge.FormatForInjection(drained)
	if err := cfg.tmux.NudgeSessionWithOpts(cfg.sessionName, formatted, cfg.nudgeOpts); err != nil {
		fmt.Fprintf(os.Stderr, "nudge-poller: injection error for %s: %v\n", cfg.sessionName, err)
		requeueDrainedNudges(cfg.townRoot, cfg.sessionName, "nudge-poller", drained)
	}
	return true
}

func shouldSkipDrainUntilIdle(hasPromptDetection bool, waitErr error) bool {
	return hasPromptDetection && waitErr != nil
}
