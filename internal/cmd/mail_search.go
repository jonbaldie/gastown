package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

// runMailSearch searches for messages matching a pattern.
func runMailSearch(cmd *cobra.Command, args []string) error {
	query := args[0]
	address := detectSender()
	messages, err := searchMailMessages(query, address,
		mailStringFlag(cmd, "from"),
		mailBoolFlag(cmd, "subject"),
		mailBoolFlag(cmd, "body"))
	if err != nil {
		return err
	}

	if mailBoolFlag(cmd, "json") {
		return printMailSearchJSON(messages)
	}
	printMailSearchResults(address, messages)
	return nil
}

func searchMailMessages(query, address, fromFilter string, subjectOnly, bodyOnly bool) ([]*mail.Message, error) {
	// Get workspace for mail operations
	workDir, err := findMailWorkDir()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Get mailbox
	router := mail.NewRouter(workDir)
	mailbox, err := router.GetMailbox(address)
	if err != nil {
		return nil, fmt.Errorf("getting mailbox: %w", err)
	}

	// Build search options
	opts := mail.SearchOptions{
		Query:       query,
		FromFilter:  fromFilter,
		SubjectOnly: subjectOnly,
		BodyOnly:    bodyOnly,
	}

	// Execute search
	messages, err := mailbox.Search(opts)
	if err != nil {
		return nil, fmt.Errorf("searching messages: %w", err)
	}
	return messages, nil
}

func printMailSearchJSON(messages []*mail.Message) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(messages)
}

func printMailSearchResults(address string, messages []*mail.Message) {
	fmt.Printf("%s Search results for %s: %d message(s)\n\n",
		style.Bold.Render("🔍"), address, len(messages))

	if len(messages) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(no matches)"))
		return
	}

	for _, msg := range messages {
		printMailSearchMessage(msg)
	}
}

func printMailSearchMessage(msg *mail.Message) {
	readMarker := "●"
	if msg.Read {
		readMarker = "○"
	}
	typeMarker := ""
	if msg.Type != "" && msg.Type != mail.TypeNotification {
		typeMarker = fmt.Sprintf(" [%s]", msg.Type)
	}
	priorityMarker := ""
	if msg.Priority == mail.PriorityHigh || msg.Priority == mail.PriorityUrgent {
		priorityMarker = " " + style.Bold.Render("!")
	}
	wispMarker := ""
	if msg.Wisp {
		wispMarker = " " + style.Dim.Render("(wisp)")
	}

	fmt.Printf("  %s %s%s%s%s\n", readMarker, msg.Subject, typeMarker, priorityMarker, wispMarker)
	fmt.Printf("    %s from %s\n",
		style.Dim.Render(msg.ID),
		msg.From)
	fmt.Printf("    %s\n",
		style.Dim.Render(msg.Timestamp.Local().Format("2006-01-02 15:04")))
}
