package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

var mailDrainCmd = &cobra.Command{
	Use:   "drain",
	Short: "Bulk-archive stale protocol messages",
	Long: `Bulk-archive stale protocol and lifecycle messages from an inbox.

Drains messages matching common protocol patterns that accumulate in
agent inboxes (especially witness). These are messages that have been
processed or are no longer actionable.

DRAINABLE MESSAGE TYPES:
  POLECAT_DONE       Polecat completion notifications
  POLECAT_STARTED    Polecat startup notifications
  LIFECYCLE:*        Lifecycle events (shutdown, etc.)
  MERGED             Merge confirmations
  MERGE_READY        Merge ready notifications
  MERGE_FAILED       Merge failure notifications
  SWARM_START        Swarm initiation messages

NON-DRAINABLE (preserved):
  HELP:*             Help requests (need human attention)
  HANDOFF            Session handoff context

By default, only archives protocol messages older than 30 minutes.
Use --max-age to change the threshold, or --all to drain regardless of age.

Examples:
  gt mail drain                              # Drain own inbox (30m default)
  gt mail drain --identity gastown/witness   # Drain witness inbox
  gt mail drain --max-age 1h                 # Only drain messages >1h old
  gt mail drain --all                        # Drain all protocol messages
  gt mail drain --dry-run                    # Preview what would be drained`,
	RunE: runMailDrain,
}

func init() {
	mailDrainCmd.Flags().String("max-age", "30m", "Only drain messages older than this duration (e.g., 30m, 1h, 2h)")
	mailDrainCmd.Flags().BoolP("dry-run", "n", false, "Show what would be drained without archiving")
	mailDrainCmd.Flags().String("identity", "", "Target inbox identity (e.g., gastown/witness)")
	mailDrainCmd.Flags().Bool("all", false, "Drain all protocol messages regardless of age")
}

// drainableSubjects are protocol message subject prefixes that are safe to
// bulk-archive. These are routine notifications that don't require individual
// attention once the information is stale.
var drainableSubjects = []string{
	"CRASHED_POLECAT",
	"POLECAT_DONE",
	"POLECAT_STARTED",
	"LIFECYCLE:",
	"MERGED",
	"MERGE_READY",
	"MERGE_FAILED",
	"SWARM_START",
}

// isDrainableMessage checks if a message subject matches a drainable protocol pattern.
func isDrainableMessage(subject string) bool {
	for _, prefix := range drainableSubjects {
		if strings.HasPrefix(subject, prefix) {
			return true
		}
	}
	return false
}

func runMailDrain(cmd *cobra.Command, _ []string) error {
	maxAgeText := commandStringFlag(cmd, "max-age")
	maxAge, err := time.ParseDuration(maxAgeText)
	if err != nil {
		return fmt.Errorf("invalid --max-age %q: %w", maxAgeText, err)
	}

	address := commandStringFlag(cmd, "identity")
	if address == "" {
		address = detectSender()
	}

	mailbox, err := getMailbox(address)
	if err != nil {
		return err
	}

	messages, err := mailbox.List()
	if err != nil {
		return fmt.Errorf("listing messages: %w", err)
	}

	if len(messages) == 0 {
		fmt.Printf("%s Inbox %s is empty, nothing to drain\n", style.Success.Render("✓"), address)
		return nil
	}

	cutoff := time.Now().Add(-maxAge)
	candidates := drainCandidates(messages, cutoff, commandBoolFlag(cmd, "all"))

	if len(candidates) == 0 {
		fmt.Printf("%s No drainable messages in %s (%d messages total)\n",
			style.Success.Render("✓"), address, len(messages))
		return nil
	}

	if commandBoolFlag(cmd, "dry-run") {
		printDrainDryRun(address, len(messages), candidates)
		return nil
	}

	archived, archiveErrors := archiveDrainCandidates(mailbox, candidates)
	return reportDrain(address, len(messages), len(candidates), archived, archiveErrors, candidates)
}

type drainCandidate struct {
	Message *mail.Message
	Reason  string
}

func drainCandidates(messages []*mail.Message, cutoff time.Time, drainAll bool) []drainCandidate {
	var candidates []drainCandidate
	for _, msg := range messages {
		if candidate, ok := protocolDrainCandidate(msg, cutoff, drainAll); ok {
			candidates = append(candidates, candidate)
			continue
		}
		if candidate, ok := readWispDrainCandidate(msg, cutoff, drainAll); ok {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func protocolDrainCandidate(msg *mail.Message, cutoff time.Time, drainAll bool) (drainCandidate, bool) {
	if !isDrainableMessage(msg.Subject) || (!drainAll && msg.Timestamp.After(cutoff)) {
		return drainCandidate{}, false
	}
	reason := "protocol"
	if msg.Wisp {
		reason = "wisp+protocol"
	}
	return drainCandidate{Message: msg, Reason: reason}, true
}

func readWispDrainCandidate(msg *mail.Message, cutoff time.Time, drainAll bool) (drainCandidate, bool) {
	if isDrainableMessage(msg.Subject) || !msg.Wisp || !msg.Read || (!drainAll && msg.Timestamp.Before(cutoff) == false) {
		return drainCandidate{}, false
	}
	return drainCandidate{Message: msg, Reason: "read-wisp"}, true
}

func printDrainDryRun(address string, total int, candidates []drainCandidate) {
	fmt.Printf("%s Would drain %d/%d messages from %s:\n", style.Dim.Render("(dry-run)"), len(candidates), total, address)
	for _, candidate := range candidates {
		age := time.Since(candidate.Message.Timestamp).Truncate(time.Minute)
		fmt.Printf("  %s %s [%s] (age: %s)\n",
			style.Dim.Render(candidate.Message.ID), candidate.Message.Subject, candidate.Reason, age)
	}
}

func archiveDrainCandidates(mailbox *mail.Mailbox, candidates []drainCandidate) (int, []string) {
	archived := 0
	var archiveErrors []string
	for _, candidate := range candidates {
		if err := mailbox.Delete(candidate.Message.ID); err != nil {
			archiveErrors = append(archiveErrors, fmt.Sprintf("%s: %v", candidate.Message.ID, err))
			continue
		}
		archived++
	}
	return archived, archiveErrors
}

func reportDrain(address string, total, candidateCount, archived int, archiveErrors []string, candidates []drainCandidate) error {
	remaining := total - archived
	if len(archiveErrors) > 0 {
		fmt.Printf("%s Drained %d/%d messages from %s (%d remaining, %d errors)\n",
			style.Bold.Render("⚠"), archived, candidateCount, address, remaining, len(archiveErrors))
		for _, errMsg := range archiveErrors {
			fmt.Printf("  Error: %s\n", errMsg)
		}
		return fmt.Errorf("failed to drain %d messages", len(archiveErrors))
	}
	fmt.Printf("%s Drained %d messages from %s (%d remaining)\n", style.Bold.Render("✓"), archived, address, remaining)
	printDrainSummary(candidates)
	return nil
}

func printDrainSummary(candidates []drainCandidate) {
	typeCounts := make(map[string]int)
	for _, candidate := range candidates {
		typeCounts[candidate.Reason]++
	}
	for reason, count := range typeCounts {
		fmt.Printf("  %s: %d\n", reason, count)
	}
}
