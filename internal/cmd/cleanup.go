package cmd

import (
	"fmt"

	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/util"
	"github.com/spf13/cobra"
)

var cleanupCmd = &cobra.Command{
	Use:     "cleanup",
	GroupID: GroupWork,
	Short:   "Clean up orphaned Claude processes",
	Long: `Clean up orphaned Claude processes that survived session termination.

This command finds and kills Claude processes that are not associated with
any active Gas Town tmux session. These orphans can accumulate when:
- Polecat sessions are killed without proper cleanup
- Claude spawns subagent processes that outlive their parent
- Network or system issues interrupt normal shutdown

Uses aggressive tmux session verification to detect ALL orphaned processes,
not just those with PPID=1.

Examples:
  gt cleanup              # Clean up orphans with confirmation
  gt cleanup --dry-run    # Show what would be killed
  gt cleanup --force      # Kill without confirmation`,
	RunE: runCleanup,
}

func init() {
	cleanupCmd.Flags().Bool("dry-run", false, "Show what would be killed without killing")
	cleanupCmd.Flags().BoolP("force", "f", false, "Kill without confirmation")

	rootCmd.AddCommand(cleanupCmd)
}

type cleanupOptions struct {
	dryRun bool
	force  bool
}

func cleanupOptionsFromCommand(cmd *cobra.Command) (cleanupOptions, error) {
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return cleanupOptions{}, err
	}
	force, err := cmd.Flags().GetBool("force")
	if err != nil {
		return cleanupOptions{}, err
	}
	return cleanupOptions{dryRun: dryRun, force: force}, nil
}

func runCleanup(cmd *cobra.Command, _ []string) error {
	opts, err := cleanupOptionsFromCommand(cmd)
	if err != nil {
		return err
	}

	// Find orphaned processes using aggressive zombie detection
	zombies, err := util.FindZombieClaudeProcesses()
	if err != nil {
		return fmt.Errorf("finding orphaned processes: %w", err)
	}

	if len(zombies) == 0 {
		fmt.Printf("%s No orphaned Claude processes found\n", style.Bold.Render("✓"))
		return nil
	}

	printCleanupZombies(zombies)

	if opts.dryRun {
		fmt.Printf("%s Dry run - no processes killed\n", style.Dim.Render("ℹ"))
		return nil
	}

	// Confirm unless --force
	if !opts.force && !confirmCleanup(len(zombies)) {
		return nil
	}

	// Kill the processes using the standard cleanup function
	results, err := util.CleanupZombieClaudeProcesses()
	if err != nil {
		return fmt.Errorf("cleaning up processes: %w", err)
	}

	reportCleanupResults(results)
	return nil
}

func printCleanupZombies(zombies []util.ZombieProcess) {
	// Show what we found
	fmt.Printf("%s Found %d orphaned Claude process(es):\n\n", style.Warning.Render("⚠"), len(zombies))
	for _, z := range zombies {
		ageStr := formatProcessAgeCleanup(z.Age)
		fmt.Printf("  %s %s (age: %s, tty: %s)\n",
			style.Bold.Render(fmt.Sprintf("PID %d", z.PID)),
			z.Cmd,
			style.Dim.Render(ageStr),
			z.TTY)
	}
	fmt.Println()
}

func confirmCleanup(count int) bool {
	fmt.Printf("Kill these %d process(es)? [y/N] ", count)
	var response string
	_, _ = fmt.Scanln(&response)
	confirmed := response == "y" || response == "Y" || response == "yes" || response == "Yes"
	if !confirmed {
		fmt.Println("Aborted")
	}
	return confirmed
}

func reportCleanupResults(results []util.ZombieCleanupResult) {
	// Report results
	var killed, escalated int
	for _, r := range results {
		switch r.Signal {
		case "SIGTERM":
			fmt.Printf("  %s PID %d sent SIGTERM\n", style.Success.Render("✓"), r.Process.PID)
			killed++
		case "SIGKILL":
			fmt.Printf("  %s PID %d sent SIGKILL (didn't respond to SIGTERM)\n", style.Warning.Render("⚠"), r.Process.PID)
			killed++
		case "UNKILLABLE":
			fmt.Printf("  %s PID %d survived SIGKILL\n", style.Error.Render("✗"), r.Process.PID)
			escalated++
		}
	}

	fmt.Printf("\n%s Cleaned up %d process(es)", style.Bold.Render("✓"), killed)
	if escalated > 0 {
		fmt.Printf(", %d unkillable", escalated)
	}
	fmt.Println()
}

// formatProcessAgeCleanup formats seconds into a human-readable age string
func formatProcessAgeCleanup(seconds int) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm%ds", seconds/60, seconds%60)
	}
	hours := seconds / 3600
	mins := (seconds % 3600) / 60
	return fmt.Sprintf("%dh%dm", hours, mins)
}
