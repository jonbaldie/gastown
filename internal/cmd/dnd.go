package cmd

import (
	"fmt"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

var dndCmd = &cobra.Command{
	Use:     "dnd [on|off|status]",
	GroupID: GroupComm,
	Short:   "Toggle Do Not Disturb mode for notifications",
	Long: `Control notification level for the current agent.

Do Not Disturb (DND) mode mutes non-critical notifications,
allowing you to focus on work without interruption.

Subcommands:
  on      Enable DND mode (mute notifications)
  off     Disable DND mode (resume normal notifications)
  status  Show current notification level

Without arguments, toggles DND mode.

Related: gt notify - for fine-grained notification level control

Examples:
  gt dnd            # Toggle DND on/off
  gt dnd on         # Enable DND (mute notifications)
  gt dnd off        # Disable DND (resume notifications)
  gt dnd status     # Show current notification level`,
	Args: cobra.MaximumNArgs(1),
	RunE: runDnd,
}

func init() {
	rootCmd.AddCommand(dndCmd)
}

func runDnd(_ *cobra.Command, args []string) error {
	bd, agentBeadID, err := resolveNotifyTarget()
	if err != nil {
		return err
	}
	currentLevel := getNotificationLevel(bd, agentBeadID)
	action := dndAction(args, currentLevel)
	return runDndAction(bd, agentBeadID, currentLevel, action)
}

func dndAction(args []string, currentLevel string) string {
	if len(args) > 0 {
		return args[0]
	}
	// Toggle: if muted -> normal, else -> muted
	if currentLevel == beads.NotifyMuted {
		return "off"
	}
	return "on"
}

func runDndAction(bd *beads.Beads, agentBeadID, currentLevel, action string) error {
	switch action {
	case "on":
		if err := bd.UpdateAgentNotificationLevel(agentBeadID, beads.NotifyMuted); err != nil {
			return fmt.Errorf("enabling DND: %w", err)
		}
		fmt.Printf("%s DND enabled - notifications muted\n", style.SuccessPrefix)
		fmt.Printf("  Run %s to resume notifications\n", style.Bold.Render("gt dnd off"))

	case "off":
		if err := bd.UpdateAgentNotificationLevel(agentBeadID, beads.NotifyNormal); err != nil {
			return fmt.Errorf("disabling DND: %w", err)
		}
		fmt.Printf("%s DND disabled - notifications resumed\n", style.SuccessPrefix)

	case "status":
		showDndStatus(currentLevel)

	default:
		return fmt.Errorf("unknown action %q: use on, off, or status", action)
	}

	return nil
}

func showDndStatus(level string) {
	if level == "" {
		level = beads.NotifyNormal
	}

	icon := "🔔"
	description := "All important notifications"
	switch level {
	case beads.NotifyVerbose:
		icon = "🔊"
		description = "All notifications (verbose)"
	case beads.NotifyMuted:
		icon = "🔕"
		description = "Notifications muted (DND)"
	}

	fmt.Printf("%s Notification level: %s\n", icon, style.Bold.Render(level))
	fmt.Printf("  %s\n", style.Dim.Render(description))
}
