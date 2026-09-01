package cmd

import (
	"github.com/spf13/cobra"
)

var mailHookCmd = &cobra.Command{
	Use:   "hook <mail-id>",
	Short: "Attach mail to your hook (alias for 'gt hook attach')",
	Long: `Attach a mail message to your hook.

This is an alias for 'gt hook attach <mail-id>'. It attaches the specified
mail message to your hook so you can work on it.

The hook is the "durability primitive" - work on your hook survives session
restarts, context compaction, and handoffs.

Examples:
  gt mail hook msg-abc123                    # Attach mail to your hook
  gt mail hook msg-abc123 -s "Fix the bug"   # With subject for handoff
  gt mail hook msg-abc123 --force            # Replace existing incomplete work

Related commands:
  gt hook <bead>     # Attach any bead to your hook
  gt hook status     # Show what's on your hook
  gt unsling         # Remove work from hook`,
	Args: cobra.ExactArgs(1),
	RunE: runMailHook,
}

func init() {
	mailHookCmd.Flags().StringP("subject", "s", "", "Subject for handoff mail (optional)")
	mailHookCmd.Flags().StringP("message", "m", "", "Message for handoff mail (optional)")
	mailHookCmd.Flags().BoolP("dry-run", "n", false, "Show what would be done")
	mailHookCmd.Flags().BoolP("force", "f", false, "Replace existing incomplete hooked bead")

	getMailCommand().AddCommand(mailHookCmd)
}

// runMailHook attaches mail to the hook - delegates to the hook command's logic
func runMailHook(cmd *cobra.Command, args []string) error {
	// Delegate to the hook command's run function
	return runHook(cmd, args)
}
