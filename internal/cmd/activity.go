package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type activityOptions struct {
	actor, rig, polecat, target, reason, message, status, issue, to string
	count                                                           int
}

var activityCmd = &cobra.Command{
	Use:     "activity",
	GroupID: GroupDiag,
	Short:   "Emit and view activity events",
	Long: `Emit and view activity events for the Gas Town activity feed.

Events are written to ~/gt/.events.jsonl and can be viewed with 'gt feed'.

Subcommands:
  emit    Emit an activity event`,
}

var activityEmitCmd = &cobra.Command{
	Use:   "emit <event-type>",
	Short: "Emit an activity event",
	Long: `Emit an activity event to the Gas Town activity feed.

Supported event types for witness patrol:
  patrol_started   - When witness begins patrol cycle
  polecat_checked  - When witness checks a polecat
  polecat_nudged   - When witness nudges a stuck polecat
  escalation_sent  - When witness escalates to Mayor/Deacon
  patrol_complete  - When patrol cycle finishes

Supported event types for refinery:
  merge_started    - When refinery starts a merge
  merge_complete   - When merge succeeds
  merge_failed     - When merge fails
  queue_processed  - When refinery finishes processing queue

Common options:
  --actor    Who is emitting the event (e.g., greenplace/witness)
  --rig      Which rig the event is about
  --message  Human-readable message

Examples:
  gt activity emit patrol_started --rig greenplace --count 3
  gt activity emit polecat_checked --rig greenplace --polecat Toast --status working --issue gp-xyz
  gt activity emit polecat_nudged --rig greenplace --polecat Toast --reason "idle for 10 minutes"
  gt activity emit escalation_sent --rig greenplace --target Toast --to mayor --reason "unresponsive"
  gt activity emit patrol_complete --rig greenplace --count 3 --message "All polecats healthy"`,
	Args: cobra.ExactArgs(1),
}

func init() {
	opts := &activityOptions{}
	activityEmitCmd.RunE = func(_ *cobra.Command, args []string) error { return runActivityEmit(opts, args) }
	// Emit command flags
	activityEmitCmd.Flags().StringVar(&opts.actor, "actor", "", "Actor emitting the event (auto-detected if not set)")
	activityEmitCmd.Flags().StringVar(&opts.rig, "rig", "", "Rig the event is about")
	activityEmitCmd.Flags().StringVar(&opts.polecat, "polecat", "", "Polecat involved (for polecat_checked, polecat_nudged)")
	activityEmitCmd.Flags().StringVar(&opts.target, "target", "", "Target of the action (for escalation)")
	activityEmitCmd.Flags().StringVar(&opts.reason, "reason", "", "Reason for the action")
	activityEmitCmd.Flags().StringVar(&opts.message, "message", "", "Human-readable message")
	activityEmitCmd.Flags().StringVar(&opts.status, "status", "", "Status (for polecat_checked: working, idle, stuck)")
	activityEmitCmd.Flags().StringVar(&opts.issue, "issue", "", "Issue ID (for polecat_checked)")
	activityEmitCmd.Flags().StringVar(&opts.to, "to", "", "Escalation target (for escalation_sent: mayor, deacon)")
	activityEmitCmd.Flags().IntVar(&opts.count, "count", 0, "Polecat count (for patrol events)")

	activityCmd.AddCommand(activityEmitCmd)
	rootCmd.AddCommand(activityCmd)
}

func runActivityEmit(opts *activityOptions, args []string) error {
	eventType := args[0]

	// Validate we're in a Gas Town workspace
	_, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Auto-detect actor if not provided
	actor := opts.actor
	if actor == "" {
		actor = detectActor()
	}

	// Build payload based on event type
	var payload map[string]interface{}

	switch eventType {
	case events.TypePatrolStarted, events.TypePatrolComplete:
		if opts.rig == "" {
			return fmt.Errorf("--rig is required for %s events", eventType)
		}
		payload = events.PatrolPayload(opts.rig, opts.count, opts.message)

	case events.TypePolecatChecked:
		if opts.rig == "" || opts.polecat == "" {
			return fmt.Errorf("--rig and --polecat are required for polecat_checked events")
		}
		if opts.status == "" {
			opts.status = "checked"
		}
		payload = events.PolecatCheckPayload(opts.rig, opts.polecat, opts.status, opts.issue)

	case events.TypePolecatNudged:
		if opts.rig == "" || opts.polecat == "" {
			return fmt.Errorf("--rig and --polecat are required for polecat_nudged events")
		}
		payload = events.NudgePayload(opts.rig, opts.polecat, opts.reason)

	case events.TypeEscalationSent:
		if opts.rig == "" || opts.target == "" || opts.to == "" {
			return fmt.Errorf("--rig, --target, and --to are required for escalation_sent events")
		}
		payload = events.EscalationPayload(opts.rig, opts.target, opts.to, opts.reason)

	case events.TypeMergeStarted, events.TypeMerged, events.TypeMergeFailed, events.TypeMergeSkipped:
		// Refinery events - flexible payload
		payload = make(map[string]interface{})
		if opts.rig != "" {
			payload["rig"] = opts.rig
		}
		if opts.message != "" {
			payload["message"] = opts.message
		}
		if opts.target != "" {
			payload["branch"] = opts.target
		}
		if opts.reason != "" {
			payload["reason"] = opts.reason
		}

	default:
		// Generic event - use whatever flags are provided
		payload = make(map[string]interface{})
		if opts.rig != "" {
			payload["rig"] = opts.rig
		}
		if opts.polecat != "" {
			payload["polecat"] = opts.polecat
		}
		if opts.target != "" {
			payload["target"] = opts.target
		}
		if opts.reason != "" {
			payload["reason"] = opts.reason
		}
		if opts.message != "" {
			payload["message"] = opts.message
		}
		if opts.status != "" {
			payload["status"] = opts.status
		}
		if opts.issue != "" {
			payload["issue"] = opts.issue
		}
		if opts.to != "" {
			payload["to"] = opts.to
		}
		if opts.count > 0 {
			payload["count"] = opts.count
		}
	}

	// Emit the event
	if err := events.LogFeed(eventType, actor, payload); err != nil {
		return fmt.Errorf("emitting event: %w", err)
	}

	// Print confirmation
	payloadJSON, _ := json.Marshal(payload)
	fmt.Printf("%s Emitted %s event\n", style.Success.Render("✓"), style.Bold.Render(eventType))
	fmt.Printf("  Actor:   %s\n", actor)
	fmt.Printf("  Payload: %s\n", string(payloadJSON))

	return nil
}

// Note: detectActor is defined in sling.go and reused here
