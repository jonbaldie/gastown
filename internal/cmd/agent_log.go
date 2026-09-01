package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jonbaldie/gastown/internal/agentlog"
	"github.com/jonbaldie/gastown/internal/telemetry"
	"github.com/spf13/cobra"
)

type agentLogOptions struct {
	session   string
	workDir   string
	agentType string
	since     string
	runID     string
}

func init() {
	rootCmd.AddCommand(newAgentLogCommand())
}

func newAgentLogCommand() *cobra.Command {
	opts := &agentLogOptions{agentType: "claudecode"}
	cmd := &cobra.Command{
		Use:    "agent-log",
		Short:  "Stream agent conversation events to OTLP log endpoint (invoked by session lifecycle)",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runAgentLog(opts)
		},
	}
	cmd.Flags().StringVar(&opts.session, "session", "", "Gas Town tmux session name (used as log tag)")
	cmd.Flags().StringVar(&opts.workDir, "work-dir", "", "Agent working directory (used to locate conversation log files)")
	cmd.Flags().StringVar(&opts.agentType, "agent", opts.agentType, "Agent type (claudecode, opencode)")
	cmd.Flags().StringVar(&opts.since, "since", "", "Only watch JSONL files modified at or after this RFC3339 timestamp (filters out pre-existing Claude sessions)")
	cmd.Flags().StringVar(&opts.runID, "run-id", "", "GASTA run identifier (GT_RUN); injected into every agent.event for waterfall correlation")
	_ = cmd.MarkFlagRequired("session")
	_ = cmd.MarkFlagRequired("work-dir")
	return cmd
}

func runAgentLog(opts *agentLogOptions) error {
	ctx := agentLogContext(opts)
	provider := initAgentLogTelemetry(ctx)
	defer shutdownAgentLogTelemetry(provider)

	since, err := parseAgentLogSince(opts.since)
	if err != nil {
		return err
	}

	adapter, err := newAgentLogAdapter(opts.agentType)
	if err != nil {
		return err
	}

	ch, err := adapter.Watch(ctx, opts.session, opts.workDir, since)
	if err != nil {
		return fmt.Errorf("starting watcher: %w", err)
	}

	for ev := range ch {
		recordAgentLogEvent(ctx, ev)
	}
	return nil
}

func agentLogContext(opts *agentLogOptions) context.Context {
	ctx := context.Background()
	runID := opts.runID
	if runID == "" {
		runID = os.Getenv("GT_RUN")
	}
	if runID != "" {
		ctx = telemetry.WithRunID(ctx, runID)
	}
	return ctx
}

func initAgentLogTelemetry(ctx context.Context) *telemetry.Provider {
	provider, err := telemetry.Init(ctx, "gastown", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: telemetry init failed: %v\n", err)
	}
	return provider
}

func shutdownAgentLogTelemetry(provider *telemetry.Provider) {
	if provider == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = provider.Shutdown(shutdownCtx)
}

func parseAgentLogSince(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	since, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing --since %q: %w", raw, err)
	}
	return since, nil
}

func newAgentLogAdapter(agentType string) (agentlog.AgentAdapter, error) {
	adapter := agentlog.NewAdapter(agentType)
	if adapter == nil {
		return nil, fmt.Errorf("unknown agent type %q; supported: claudecode, opencode", agentType)
	}
	return adapter, nil
}

func recordAgentLogEvent(ctx context.Context, ev agentlog.AgentEvent) {
	if ev.EventType == "usage" {
		telemetry.RecordAgentTokenUsage(ctx, ev.SessionID, ev.NativeSessionID,
			ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens, ev.CacheCreationTokens)
		return
	}
	telemetry.RecordAgentEvent(ctx, ev.SessionID, ev.AgentType, ev.EventType, ev.Role, ev.Content, ev.NativeSessionID, ev.Timestamp)
}
