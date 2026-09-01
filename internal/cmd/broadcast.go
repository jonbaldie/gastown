package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

func init() {
	cmd := newBroadcastCommand()
	cmd.Flags().String("rig", "", "Only broadcast to workers in this rig")
	cmd.Flags().Bool("all", false, "Include all agents (mayor, witness, etc.), not just workers")
	cmd.Flags().Bool("dry-run", false, "Show what would be sent without sending")
	rootCmd.AddCommand(cmd)
}

func newBroadcastCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "broadcast <message>",
		GroupID: GroupComm,
		Short:   "Send a nudge message to all workers",
		Long: `Broadcasts a message to all active workers (polecats and crew).

By default, only workers (polecats and crew) receive the message.
Use --all to include infrastructure agents (mayor, deacon, witness, refinery).

The message is sent as a nudge to each worker's Claude Code session.

Examples:
  gt broadcast "Check your mail"
  gt broadcast --rig greenplace "New priority work available"
  gt broadcast --all "System maintenance in 5 minutes"
  gt broadcast --dry-run "Test message"`,
		Args: cobra.ExactArgs(1),
		RunE: runBroadcast,
	}
}

type broadcastOptions struct {
	rig    string
	all    bool
	dryRun bool
}

func broadcastOptionsFromCommand(cmd *cobra.Command) (broadcastOptions, error) {
	rig, err := cmd.Flags().GetString("rig")
	if err != nil {
		return broadcastOptions{}, err
	}
	all, err := cmd.Flags().GetBool("all")
	if err != nil {
		return broadcastOptions{}, err
	}
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return broadcastOptions{}, err
	}
	return broadcastOptions{rig: rig, all: all, dryRun: dryRun}, nil
}

func runBroadcast(cmd *cobra.Command, args []string) error {
	opts, err := broadcastOptionsFromCommand(cmd)
	if err != nil {
		return err
	}

	message := args[0]

	if message == "" {
		return fmt.Errorf("message cannot be empty")
	}

	// Get all agent sessions (including polecats)
	agents, err := getAgentSessions(true)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	// Get sender identity to exclude self
	sender := os.Getenv("BD_ACTOR")
	targets := selectBroadcastTargets(agents, opts, sender)
	if len(targets) == 0 {
		return reportNoBroadcastTargets(opts.rig)
	}

	if opts.dryRun {
		printBroadcastDryRun(targets, message)
		return nil
	}

	result := sendBroadcast(targets, message)
	return reportBroadcast(result)
}

func selectBroadcastTargets(agents []*AgentSession, opts broadcastOptions, sender string) []*AgentSession {
	var targets []*AgentSession
	for _, agent := range agents {
		if opts.rig != "" && agent.Rig != opts.rig {
			continue
		}
		if !opts.all && agent.Type != AgentCrew && agent.Type != AgentPolecat {
			continue
		}
		if sender != "" && formatAgentName(agent) == sender {
			continue
		}
		targets = append(targets, agent)
	}
	return targets
}

func reportNoBroadcastTargets(rig string) error {
	fmt.Println("No workers running to broadcast to.")
	if rig != "" {
		fmt.Printf("  (filtered by rig: %s)\n", rig)
	}
	return nil
}

func printBroadcastDryRun(targets []*AgentSession, message string) {
	fmt.Printf("Would broadcast to %d agent(s):\n\n", len(targets))
	for _, agent := range targets {
		fmt.Printf("  %s %s\n", AgentTypeIcons[agent.Type], formatAgentName(agent))
	}
	fmt.Printf("\nMessage: %s\n", message)
}

type broadcastResult struct {
	succeeded int
	failed    int
	skipped   int
	failures  []string
}

func sendBroadcast(targets []*AgentSession, message string) broadcastResult {
	t := tmux.NewTmux()
	townRoot, _ := workspace.FindFromCwd()
	result := broadcastResult{}

	fmt.Printf("Broadcasting to %d agent(s)...\n\n", len(targets))
	for i, agent := range targets {
		result.record(deliverBroadcast(t, townRoot, agent, message))
		if i < len(targets)-1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return result
}

type broadcastDelivery struct {
	succeeded bool
	failed    bool
	skipped   bool
	failure   string
}

func deliverBroadcast(t *tmux.Tmux, townRoot string, agent *AgentSession, message string) broadcastDelivery {
	agentName := formatAgentName(agent)
	if townRoot != "" {
		if shouldSend, level, _ := shouldNudgeTarget(townRoot, agentName, false); !shouldSend {
			fmt.Printf("  %s %s %s (DND: %s)\n", style.Dim.Render("○"), AgentTypeIcons[agent.Type], agentName, level)
			return broadcastDelivery{skipped: true}
		}
	}

	if err := t.NudgeSession(agent.Name, message); err != nil {
		fmt.Printf("  %s %s %s\n", style.ErrorPrefix, AgentTypeIcons[agent.Type], agentName)
		return broadcastDelivery{failed: true, failure: fmt.Sprintf("%s: %v", agentName, err)}
	}
	fmt.Printf("  %s %s %s\n", style.SuccessPrefix, AgentTypeIcons[agent.Type], agentName)
	return broadcastDelivery{succeeded: true}
}

func (r *broadcastResult) record(delivery broadcastDelivery) {
	if delivery.succeeded {
		r.succeeded++
	}
	if delivery.failed {
		r.failed++
		r.failures = append(r.failures, delivery.failure)
	}
	if delivery.skipped {
		r.skipped++
	}
}

func reportBroadcast(result broadcastResult) error {
	fmt.Println()
	if result.failed > 0 {
		summary := fmt.Sprintf("Broadcast complete: %d succeeded, %d failed", result.succeeded, result.failed)
		if result.skipped > 0 {
			summary += fmt.Sprintf(", %d skipped (DND)", result.skipped)
		}
		fmt.Printf("%s %s\n", style.WarningPrefix, summary)
		for _, failure := range result.failures {
			fmt.Printf("  %s\n", style.Dim.Render(failure))
		}
		return fmt.Errorf("%d nudge(s) failed", result.failed)
	}

	summary := fmt.Sprintf("Broadcast complete: %d agent(s) nudged", result.succeeded)
	if result.skipped > 0 {
		summary += fmt.Sprintf(", %d skipped (DND)", result.skipped)
	}
	fmt.Printf("%s %s\n", style.SuccessPrefix, summary)
	return nil
}

// formatAgentName returns a display name for an agent.
func formatAgentName(agent *AgentSession) string {
	switch agent.Type {
	case AgentMayor:
		return "mayor"
	case AgentDeacon:
		return "deacon"
	case AgentWitness:
		return fmt.Sprintf("%s/witness", agent.Rig)
	case AgentRefinery:
		return fmt.Sprintf("%s/refinery", agent.Rig)
	case AgentCrew:
		return fmt.Sprintf("%s/crew/%s", agent.Rig, agent.AgentName)
	case AgentPolecat:
		return fmt.Sprintf("%s/%s", agent.Rig, agent.AgentName)
	}
	return agent.Name
}
