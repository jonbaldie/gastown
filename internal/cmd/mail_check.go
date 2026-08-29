package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/gastown/internal/estop"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/nudge"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

func runMailCheck(cmd *cobra.Command, _ []string) error {
	inject := mailBoolFlag(cmd, "inject")
	jsonOutput := mailBoolFlag(cmd, "json")
	identity := mailStringAliasFlag(cmd, "identity", "address")
	address := mailCheckAddress(identity)
	workDir, mailbox, messages, unread, handled, err := loadMailCheckInbox(address, inject)
	if handled {
		return nil
	}
	if err != nil {
		return err
	}

	if jsonOutput {
		return writeMailCheckJSON(address, unread)
	}

	if inject {
		return injectMailCheck(workDir, address, mailbox, messages, unread)
	}

	return finishNormalMailCheck(unread)
}

func mailCheckAddress(identity string) string {
	if identity != "" {
		return identity
	}
	return detectSender()
}

func loadMailCheckInbox(address string, inject bool) (string, *mail.Mailbox, []*mail.Message, int, bool, error) {
	workDir, err := findMailWorkDir()
	if err != nil {
		if inject {
			fmt.Fprintf(os.Stderr, "gt mail check: workspace lookup failed: %v\n", err)
			return "", nil, nil, 0, true, nil
		}
		return "", nil, nil, 0, false, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	mailbox, err := mail.NewRouter(workDir).GetMailbox(address)
	if err != nil {
		if inject {
			fmt.Fprintf(os.Stderr, "gt mail check: mailbox error for %s: %v\n", address, err)
			return "", nil, nil, 0, true, nil
		}
		return "", nil, nil, 0, false, fmt.Errorf("getting mailbox: %w", err)
	}

	// Load the inbox once. The inject path needs unread messages later, and
	// calling Count() followed by ListUnread() doubles bd/Dolt reads.
	messages, _, unread, err := loadInboxSnapshot(mailbox, false)
	if err != nil {
		if inject {
			fmt.Fprintf(os.Stderr, "gt mail check: inbox load error for %s: %v\n", address, err)
			return "", nil, nil, 0, true, nil
		}
		return "", nil, nil, 0, false, fmt.Errorf("loading inbox: %w", err)
	}
	return workDir, mailbox, messages, unread, false, nil
}

func writeMailCheckJSON(address string, unread int) error {
	result := map[string]interface{}{
		"address": address,
		"unread":  unread,
		"has_new": unread > 0,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

func injectMailCheck(workDir, address string, mailbox *mail.Mailbox, messages []*mail.Message, unread int) error {
	printMailCheckEStop()
	if unread > 0 {
		messages = filterUnreadMessages(messages)
		fmt.Print(formatInjectOutput(messages))
		// Ack after output so message is delivered before being marked acked.
		if ackErr := mailbox.AcknowledgeDeliveries(address, messages); ackErr != nil {
			fmt.Fprintf(os.Stderr, "gt mail check: delivery ack update failed for %s: %v\n", address, ackErr)
		}
	}
	drainMailCheckNudges(workDir)
	return nil
}

func printMailCheckEStop() {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return
	}
	rigName := os.Getenv("GT_RIG")
	if !estop.IsActive(townRoot) && (rigName == "" || !estop.IsRigActive(townRoot, rigName)) {
		return
	}
	fmt.Print("<system-reminder>\n")
	fmt.Print("EMERGENCY STOP ACTIVE. All work is paused.\n")
	fmt.Print("Do NOT start new tasks or tool calls. Checkpoint your current state\n")
	fmt.Print("(save progress notes) and wait for the overseer to run 'gt thaw'.\n")
	fmt.Print("This is a system-level pause — it may be due to infrastructure failure,\n")
	fmt.Print("maintenance, or the operator traveling.\n")
	fmt.Print("</system-reminder>\n")
}

func drainMailCheckNudges(workDir string) {
	sessionName := tmux.CurrentSessionName()
	if sessionName == "" {
		return
	}
	queuedNudges, err := nudge.Drain(workDir, sessionName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gt mail check: nudge queue drain error: %v\n", err)
		return
	}
	if len(queuedNudges) > 0 {
		fmt.Print(nudge.FormatForInjection(queuedNudges))
	}
}

func finishNormalMailCheck(unread int) error {
	if unread > 0 {
		fmt.Printf("%s %d unread message(s)\n", style.Bold.Render("📬"), unread)
		return NewSilentExit(0)
	}
	fmt.Println("No new mail")
	return NewSilentExit(1)
}

// formatInjectOutput builds the system-reminder text for inject mode.
// It separates messages into three tiers (urgent, high, normal/low) and
// formats them with priority-appropriate framing for the agent.
func formatInjectOutput(messages []*mail.Message) string {
	urgent, high, normal := categorizeMailCheckMessages(messages)
	if len(urgent) > 0 {
		return formatUrgentMailCheckOutput(urgent, high, normal)
	}
	if len(high) > 0 {
		return formatHighMailCheckOutput(high, normal)
	}
	return formatNormalMailCheckOutput(normal)
}

func categorizeMailCheckMessages(messages []*mail.Message) (urgent, high, normal []*mail.Message) {
	for _, msg := range messages {
		switch msg.Priority {
		case mail.PriorityUrgent:
			urgent = append(urgent, msg)
		case mail.PriorityHigh:
			high = append(high, msg)
		default:
			normal = append(normal, msg)
		}
	}
	return urgent, high, normal
}

func formatUrgentMailCheckOutput(urgent, high, normal []*mail.Message) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	fmt.Fprintf(&b, "URGENT: %d urgent message(s) require immediate attention.\n\n", len(urgent))
	writeMailCheckMessages(&b, urgent)
	if len(high) > 0 {
		fmt.Fprintf(&b, "\nAlso %d high-priority message(s) — process before going idle:\n", len(high))
		writeMailCheckMessages(&b, high)
	}
	if len(normal) > 0 {
		fmt.Fprintf(&b, "\n(Plus %d additional message(s) — check after current task.)\n", len(normal))
	}
	b.WriteString("\nRun 'gt mail read <id>' to read urgent messages.\n")
	b.WriteString("</system-reminder>\n")
	return b.String()
}

func formatHighMailCheckOutput(high, normal []*mail.Message) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	fmt.Fprintf(&b, "You have %d high-priority message(s) in your inbox.\n\n", len(high))
	writeMailCheckMessages(&b, high)
	if len(normal) > 0 {
		fmt.Fprintf(&b, "\n(Plus %d additional message(s).)\n", len(normal))
	}
	b.WriteString("\nContinue your current task. When it completes, process these messages\n")
	b.WriteString("before going idle: 'gt mail inbox'\n")
	b.WriteString("</system-reminder>\n")
	return b.String()
}

func formatNormalMailCheckOutput(normal []*mail.Message) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	fmt.Fprintf(&b, "You have %d unread message(s) in your inbox.\n\n", len(normal))
	writeMailCheckMessages(&b, normal)
	b.WriteString("\nContinue your current task. When it completes, check these messages\n")
	b.WriteString("before going idle: 'gt mail inbox'\n")
	b.WriteString("</system-reminder>\n")
	return b.String()
}

func writeMailCheckMessages(b *strings.Builder, messages []*mail.Message) {
	for _, msg := range messages {
		fmt.Fprintf(b, "- %s from %s: %s\n", msg.ID, msg.From, msg.Subject)
	}
}
