package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

func runMailThread(cmd *cobra.Command, args []string) error {
	threadID := args[0]

	workDir, err := findMailWorkDir()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	address := detectSender()
	mailbox, err := mail.NewRouter(workDir).GetMailbox(address)
	if err != nil {
		return fmt.Errorf("getting mailbox: %w", err)
	}

	messages, err := mailbox.ListByThread(threadID)
	if err != nil {
		return fmt.Errorf("getting thread: %w", err)
	}

	if commandBoolFlag(cmd, "json") {
		return writeMailThreadJSON(messages)
	}
	return renderMailThread(threadID, messages)
}

func writeMailThreadJSON(messages []*mail.Message) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(messages)
}

func renderMailThread(threadID string, messages []*mail.Message) error {
	fmt.Printf("%s Thread: %s (%d messages)\n\n",
		style.Bold.Render("🧵"), threadID, len(messages))

	if len(messages) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(no messages in thread)"))
		return nil
	}

	for i, msg := range messages {
		renderMailThreadMessage(i, msg)
	}

	return nil
}

func renderMailThreadMessage(index int, msg *mail.Message) {
	typeMarker := ""
	if msg.Type != "" && msg.Type != mail.TypeNotification {
		typeMarker = fmt.Sprintf(" [%s]", msg.Type)
	}
	priorityMarker := ""
	if msg.Priority == mail.PriorityHigh || msg.Priority == mail.PriorityUrgent {
		priorityMarker = " " + style.Bold.Render("!")
	}

	if index > 0 {
		fmt.Printf("  %s\n", style.Dim.Render("│"))
	}
	fmt.Printf("  %s %s%s%s\n", style.Bold.Render("●"), msg.Subject, typeMarker, priorityMarker)
	fmt.Printf("    %s from %s to %s\n",
		style.Dim.Render(msg.ID),
		msg.From, msg.To)
	fmt.Printf("    %s\n",
		style.Dim.Render(msg.Timestamp.Local().Format("2006-01-02 15:04")))

	if msg.Body != "" {
		fmt.Printf("    %s\n", msg.Body)
	}
}

func runMailReply(cmd *cobra.Command, args []string) error {
	messageBody, err := resolveMailReplyBody(args, commandStringAliasFlag(cmd, "message", "body"))
	if err != nil {
		return err
	}

	msgID := args[0]
	router, from, original, err := loadMailReply(msgID)
	if err != nil {
		return err
	}
	reply := buildMailReply(msgID, from, original, messageBody, commandStringFlag(cmd, "subject"))
	return sendMailReply(router, from, original, reply)
}

func resolveMailReplyBody(args []string, flagBody string) (string, error) {
	messageBody := flagBody
	if len(args) > 1 {
		messageBody = args[1]
	}
	if messageBody == "" {
		return "", fmt.Errorf("message body required: provide as second argument or use -m flag")
	}
	return messageBody, nil
}

func loadMailReply(msgID string) (*mail.Router, string, *mail.Message, error) {
	workDir, err := findMailWorkDir()
	if err != nil {
		return nil, "", nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	from := detectSender()
	router := mail.NewRouter(workDir)
	mailbox, err := router.GetMailbox(from)
	if err != nil {
		return nil, "", nil, fmt.Errorf("getting mailbox: %w", err)
	}
	original, err := mailbox.Get(msgID)
	if err != nil {
		return nil, "", nil, fmt.Errorf("getting message: %w", err)
	}
	return router, from, original, nil
}

func buildMailReply(msgID, from string, original *mail.Message, messageBody, subject string) *mail.Message {
	if subject == "" {
		if strings.HasPrefix(original.Subject, "Re: ") {
			subject = original.Subject
		} else {
			subject = "Re: " + original.Subject
		}
	}
	reply := &mail.Message{
		From:     from,
		To:       original.From,
		Subject:  subject,
		Body:     messageBody,
		Type:     mail.TypeReply,
		Priority: mail.PriorityNormal,
		ReplyTo:  msgID,
		ThreadID: original.ThreadID,
	}
	if reply.ThreadID == "" {
		reply.ThreadID = generateThreadID()
	}
	return reply
}

func sendMailReply(router *mail.Router, from string, original, reply *mail.Message) error {
	defer router.WaitPendingNotifications()
	if err := router.Send(reply); err != nil {
		return fmt.Errorf("sending reply: %w", err)
	}
	if err := router.ClearReplyReminders(from, reply.ThreadID); err != nil {
		style.PrintWarning("could not clear satisfied reply reminders: %v", err)
	}

	fmt.Printf("%s Reply sent to %s\n", style.Bold.Render("✓"), original.From)
	fmt.Printf("  Subject: %s\n", reply.Subject)
	if original.ThreadID != "" {
		fmt.Printf("  Thread: %s\n", style.Dim.Render(original.ThreadID))
	}

	return nil
}
