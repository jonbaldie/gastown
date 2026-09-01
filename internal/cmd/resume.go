package cmd

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

// Resume command checks for handoff messages.

var resumeCmd = &cobra.Command{
	Use:     "resume",
	GroupID: GroupWork,
	Short:   "Check for handoff messages",
	Long: `Check the inbox for handoff messages and display them for continuation.

The resume command checks for messages with "HANDOFF" in the subject
and displays them formatted for easy continuation.

Examples:
  gt resume    # Check inbox for handoff messages`,
	RunE: runResume,
}

func init() {
	rootCmd.AddCommand(resumeCmd)
}

func runResume(_ *cobra.Command, _ []string) error {
	return checkHandoffMessages()
}

type inboxMessage struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	From    string `json:"from"`
	Date    string `json:"date"`
	Body    string `json:"body"`
}

type resumeHandoffMessage struct {
	ID      string
	Subject string
	From    string
	Date    string
	Body    string
}

// checkHandoffMessages checks the inbox for handoff messages and displays them.
func checkHandoffMessages() error {
	output, err := fetchInboxJSON()
	if err != nil {
		return displayPlainInbox("checking inbox", true)
	}

	messages, err := decodeInboxMessages(output)
	if err != nil {
		return displayPlainInbox("fallback inbox check failed", false)
	}

	printHandoffMessages(messages)
	return nil
}

func fetchInboxJSON() ([]byte, error) {
	return exec.Command("gt", "mail", "inbox", "--json").Output()
}

func fetchInboxPlain() ([]byte, error) {
	return exec.Command("gt", "mail", "inbox").Output()
}

func decodeInboxMessages(output []byte) ([]inboxMessage, error) {
	var messages []inboxMessage
	if err := json.Unmarshal(output, &messages); err != nil {
		return nil, err
	}
	return messages, nil
}

func displayPlainInbox(errorPrefix string, includeReadHint bool) error {
	output, err := fetchInboxPlain()
	if err != nil {
		return fmt.Errorf("%s: %w", errorPrefix, err)
	}
	outputStr := string(output)
	if !containsHandoff(outputStr) {
		fmt.Printf("%s No handoff messages in inbox\n", style.Dim.Render("○"))
		if includeReadHint {
			fmt.Printf("  Handoff messages have 'HANDOFF' in the subject.\n")
		}
		return nil
	}

	fmt.Printf("%s Found handoff message(s):\n\n", style.Bold.Render("🤝"))
	fmt.Println(outputStr)
	if includeReadHint {
		fmt.Printf("\n%s Read with: gt mail read <id>\n", style.Bold.Render("→"))
	}
	return nil
}

func printHandoffMessages(messages []inboxMessage) {
	handoffs := findHandoffMessages(messages)
	if len(handoffs) == 0 {
		fmt.Printf("%s No handoff messages in inbox\n", style.Dim.Render("○"))
		fmt.Printf("  Handoff messages have 'HANDOFF' in the subject.\n")
		fmt.Printf("  Use 'gt handoff -s \"...\"' to create one when handing off.\n")
		return
	}

	fmt.Printf("%s Found %d handoff message(s):\n\n", style.Bold.Render("🤝"), len(handoffs))
	for i, msg := range handoffs {
		printHandoffMessage(i+1, msg)
	}

	if len(handoffs) == 1 {
		fmt.Printf("%s Read full message: gt mail read %s\n", style.Bold.Render("→"), handoffs[0].ID)
	} else {
		fmt.Printf("%s Read messages: gt mail read <id>\n", style.Bold.Render("→"))
	}
	fmt.Printf("%s Clear after reading: gt mail close <id>\n", style.Dim.Render("💡"))
}

func findHandoffMessages(messages []inboxMessage) []resumeHandoffMessage {
	var handoffs []resumeHandoffMessage
	for _, msg := range messages {
		if !containsHandoff(msg.Subject) {
			continue
		}
		handoffs = append(handoffs, resumeHandoffMessage{
			ID:      msg.ID,
			Subject: msg.Subject,
			From:    msg.From,
			Date:    msg.Date,
			Body:    msg.Body,
		})
	}
	return handoffs
}

func printHandoffMessage(number int, msg resumeHandoffMessage) {
	fmt.Printf("--- Handoff %d: %s ---\n", number, msg.ID)
	fmt.Printf("Subject: %s\n", msg.Subject)
	fmt.Printf("From: %s\n", msg.From)
	if msg.Date != "" {
		fmt.Printf("Date: %s\n", msg.Date)
	}
	if msg.Body != "" {
		fmt.Printf("\n%s\n", msg.Body)
	}
	fmt.Println()
}

// containsHandoff checks if a string contains "HANDOFF" (case-insensitive).
func containsHandoff(s string) bool {
	upper := strings.ToUpper(s)
	return strings.Contains(upper, "HANDOFF")
}
