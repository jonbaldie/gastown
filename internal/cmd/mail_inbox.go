package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

// getMailbox returns the mailbox for the given address.
func getMailbox(address string) (*mail.Mailbox, error) {
	// All mail uses town beads (two-level architecture)
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
	return mailbox, nil
}

func runMailInbox(cmd *cobra.Command, args []string) error {
	showAll := commandBoolFlag(cmd, "all")
	unreadOnly := commandBoolFlag(cmd, "unread")
	if showAll && unreadOnly {
		return errors.New("--all and --unread are mutually exclusive")
	}

	address := mailInboxAddress(cmd, args)

	mailbox, err := getMailbox(address)
	if err != nil {
		return err
	}

	// Load the inbox once. Count() and ListUnread() both call List(), so using
	// them here doubles the bd/Dolt reads on the hot patrol path.
	snapshot, err := loadInboxSnapshot(mailbox, unreadOnly)
	if err != nil {
		return fmt.Errorf("listing messages: %w", err)
	}

	if commandBoolFlag(cmd, "json") {
		return encodeMailJSON(snapshot.messages)
	}
	printInbox(address, snapshot.messages, snapshot.total, snapshot.unread)
	return nil
}

func mailInboxAddress(cmd *cobra.Command, args []string) string {
	if identity := commandStringAliasFlag(cmd, "identity", "address"); identity != "" {
		return identity
	}
	if len(args) > 0 {
		return args[0]
	}
	return detectSender()
}

func encodeMailJSON(value any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func printInbox(address string, messages []*mail.Message, total, unread int) {
	fmt.Printf("%s Inbox: %s (%d messages, %d unread)\n\n",
		style.Bold.Render("📬"), address, total, unread)

	if len(messages) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(no messages)"))
		return
	}

	for i, msg := range messages {
		printInboxMessage(i+1, msg)
	}
}

func printInboxMessage(index int, msg *mail.Message) {
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
	indexStr := style.Dim.Render(fmt.Sprintf("%d.", index))
	fmt.Printf("  %s %s %s%s%s%s\n", indexStr, readMarker, msg.Subject, typeMarker, priorityMarker, wispMarker)
	fmt.Printf("      %s from %s\n", style.Dim.Render(msg.ID), msg.From)
	fmt.Printf("      %s\n", style.Dim.Render(msg.Timestamp.Local().Format("2006-01-02 15:04")))
}

type inboxLister interface {
	List() ([]*mail.Message, error)
}

type loadInboxSnapshotResult struct {
	messages []*mail.Message
	total    int
	unread   int
}

func loadInboxSnapshot(mailbox inboxLister, unreadOnly bool) (loadInboxSnapshotResult, error) {
	allMessages, err := mailbox.List()
	if err != nil {
		return loadInboxSnapshotResult{}, err
	}
	if allMessages == nil {
		allMessages = make([]*mail.Message, 0)
	}

	total, unread := countInboxMessages(allMessages)
	if unreadOnly {
		return loadInboxSnapshotResult{messages: filterUnreadMessages(allMessages), total: total, unread: unread}, nil
	}
	return loadInboxSnapshotResult{messages: allMessages, total: total, unread: unread}, nil
}

func countInboxMessages(messages []*mail.Message) (total, unread int) {
	total = len(messages)
	for _, msg := range messages {
		if msg != nil && !msg.Read {
			unread++
		}
	}
	return total, unread
}

func filterUnreadMessages(messages []*mail.Message) []*mail.Message {
	unreadMessages := make([]*mail.Message, 0)
	for _, msg := range messages {
		if msg != nil && !msg.Read {
			unreadMessages = append(unreadMessages, msg)
		}
	}
	return unreadMessages
}

func runMailRead(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("message ID or index required\n\nRun 'gt mail inbox' to list messages and their IDs")
	}
	msgRef := args[0]

	// Determine which inbox
	address := detectSender()

	mailbox, err := getMailbox(address)
	if err != nil {
		return err
	}

	msgID, err := resolveMailMessageID(mailbox, msgRef)
	if err != nil {
		return err
	}

	msg, err := mailbox.Get(msgID)
	if err != nil {
		return fmt.Errorf("getting message: %w", err)
	}

	if err := mailbox.MarkReadOnly(msgID); err != nil {
		style.PrintWarning("could not mark message as read: %v", err)
	}

	if commandBoolFlag(cmd, "json") {
		if err := encodeMailJSON(msg); err != nil {
			return err
		}
		ackMailRead(mailbox, address, msg)
		return nil
	}
	printMailMessage(msg)
	ackMailRead(mailbox, address, msg)
	return nil
}

func resolveMailMessageID(mailbox *mail.Mailbox, msgRef string) (string, error) {
	idx, err := strconv.Atoi(msgRef)
	if err != nil || idx <= 0 {
		return msgRef, nil
	}
	messages, err := mailbox.List()
	if err != nil {
		return "", fmt.Errorf("listing messages: %w", err)
	}
	if idx > len(messages) {
		return "", fmt.Errorf("index %d out of range (inbox has %d messages)", idx, len(messages))
	}
	return messages[idx-1].ID, nil
}

func printMailMessage(msg *mail.Message) {
	priorityStr := ""
	if msg.Priority == mail.PriorityUrgent {
		priorityStr = " " + style.Bold.Render("[URGENT]")
	} else if msg.Priority == mail.PriorityHigh {
		priorityStr = " " + style.Bold.Render("[HIGH PRIORITY]")
	}

	typeStr := ""
	if msg.Type != "" && msg.Type != mail.TypeNotification {
		typeStr = fmt.Sprintf(" [%s]", msg.Type)
	}

	fmt.Printf("%s %s%s%s\n\n", style.Bold.Render("Subject:"), msg.Subject, typeStr, priorityStr)
	fmt.Printf("From: %s\n", msg.From)
	fmt.Printf("To: %s\n", msg.To)
	fmt.Printf("Date: %s\n", msg.Timestamp.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("ID: %s\n", style.Dim.Render(msg.ID))

	printMailMessageMetadata(msg)
}

func printMailMessageMetadata(msg *mail.Message) {
	if msg.ThreadID != "" {
		fmt.Printf("Thread: %s\n", style.Dim.Render(msg.ThreadID))
	}
	if msg.ReplyTo != "" {
		fmt.Printf("Reply-To: %s\n", style.Dim.Render(msg.ReplyTo))
	}
	if msg.Body != "" {
		fmt.Printf("\n%s\n", msg.Body)
	}
}

func ackMailRead(mailbox *mail.Mailbox, address string, msg *mail.Message) {
	if err := mailbox.AcknowledgeDeliveries(address, []*mail.Message{msg}); err != nil {
		fmt.Fprintf(os.Stderr, "gt mail read: delivery ack failed: %v\n", err)
	}
}

func runMailPeek(_ *cobra.Command, _ []string) error {
	// Determine which inbox
	address := detectSender()

	mailbox, err := getMailbox(address)
	if err != nil {
		return NewSilentExit(1) // Silent exit - can't access mailbox
	}

	// Get unread messages
	messages, err := mailbox.ListUnread()
	if err != nil || len(messages) == 0 {
		return NewSilentExit(1) // Silent exit - no unread
	}

	// Show first unread message
	msg := messages[0]

	fmt.Printf("📬 %s%s\n", msg.Subject, mailPeekPriority(msg.Priority))
	fmt.Printf("From: %s\n", msg.From)
	fmt.Printf("ID: %s\n\n", msg.ID)

	printMailPeekBody(msg.Body)
	printMailPeekCount(len(messages))

	return nil
}

func mailPeekPriority(priority mail.Priority) string {
	switch priority {
	case mail.PriorityUrgent:
		return " [URGENT]"
	case mail.PriorityHigh:
		return " [!]"
	default:
		return ""
	}
}

func printMailPeekBody(body string) {
	if body == "" {
		return
	}
	if len(body) > 500 {
		body = body[:500] + "\n..."
	}
	fmt.Print(body)
	if !strings.HasSuffix(body, "\n") {
		fmt.Println()
	}
}

func printMailPeekCount(count int) {
	if count > 1 {
		fmt.Printf("\n%s\n", style.Dim.Render(fmt.Sprintf("(+%d more unread)", count-1)))
	}
}

func runMailDelete(_ *cobra.Command, args []string) error {
	// Determine which inbox
	address := detectSender()

	mailbox, err := getMailbox(address)
	if err != nil {
		return err
	}

	// Delete all specified messages
	deleted := 0
	var errors []string
	for _, msgID := range args {
		if err := mailbox.Delete(msgID); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", msgID, err))
		} else {
			deleted++
		}
	}

	// Report results
	if len(errors) > 0 {
		fmt.Printf("%s Deleted %d/%d messages\n",
			style.Bold.Render("⚠"), deleted, len(args))
		for _, e := range errors {
			fmt.Printf("  Error: %s\n", e)
		}
		return fmt.Errorf("failed to delete %d messages", len(errors))
	}

	if len(args) == 1 {
		fmt.Printf("%s Message deleted\n", style.Bold.Render("✓"))
	} else {
		fmt.Printf("%s Deleted %d messages\n", style.Bold.Render("✓"), deleted)
	}
	return nil
}

func runMailArchive(cmd *cobra.Command, args []string) error {
	stale := commandBoolFlag(cmd, "stale")
	dryRun := commandBoolFlag(cmd, "dry-run")
	address := detectSender()

	mailbox, err := getMailbox(address)
	if err != nil {
		return err
	}

	if stale {
		if len(args) > 0 {
			return errors.New("--stale cannot be combined with message IDs")
		}
		return runMailArchiveStale(mailbox, address, dryRun)
	}
	return archiveExplicitMessages(mailbox, args, dryRun)
}

func archiveExplicitMessages(mailbox *mail.Mailbox, args []string, dryRun bool) error {
	if len(args) == 0 {
		return errors.New("message ID required unless using --stale")
	}
	if dryRun {
		printArchiveDryRun(args)
		return nil
	}

	return reportArchiveResult(archiveMessageIDs(mailbox, args), len(args), "")
}

func printArchiveDryRun(args []string) {
	fmt.Printf("%s Would archive %d message(s)\n", style.Dim.Render("(dry-run)"), len(args))
	for _, msgID := range args {
		fmt.Printf("  %s\n", style.Dim.Render(msgID))
	}
}

type archiveResult struct {
	archived int
	gcd      int
	errors   []string
}

func archiveMessageIDs(mailbox *mail.Mailbox, ids []string) archiveResult {
	var result archiveResult
	for _, msgID := range ids {
		err := mailbox.Delete(msgID)
		switch {
		case err == nil:
			result.archived++
		case errors.Is(err, mail.ErrMessageNotFound):
			result.gcd++
			fmt.Printf("  %s %s: underlying bead already gone (GC'd), entry cleared\n",
				style.Dim.Render("note"), msgID)
		default:
			result.errors = append(result.errors, fmt.Sprintf("%s: %v", msgID, err))
		}
	}
	return result
}

func reportArchiveResult(result archiveResult, requested int, qualifier string) error {
	total := result.archived + result.gcd
	noun := qualifier
	if noun != "" {
		noun += " "
	}
	if len(result.errors) > 0 {
		fmt.Printf("%s Archived %d/%d %smessages\n", style.Bold.Render("⚠"), total, requested, noun)
		for _, errMsg := range result.errors {
			fmt.Printf("  Error: %s\n", errMsg)
		}
		return fmt.Errorf("failed to archive %d %smessages", len(result.errors), noun)
	}
	if total == 1 {
		fmt.Printf("%s %smessage archived\n", style.Bold.Render("✓"), capitalizeArchiveQualifier(qualifier))
		return nil
	}
	fmt.Printf("%s Archived %d %smessages\n", style.Bold.Render("✓"), total, noun)
	return nil
}

func capitalizeArchiveQualifier(qualifier string) string {
	if qualifier == "" {
		return "Message "
	}
	return strings.ToUpper(qualifier[:1]) + qualifier[1:] + " "
}

type staleMessage struct {
	Message *mail.Message
	Reason  string
}

func runMailArchiveStale(mailbox *mail.Mailbox, address string, dryRun bool) error {
	sessionStart, err := archiveSessionStart(address)
	if err != nil {
		return err
	}

	messages, err := mailbox.List()
	if err != nil {
		return fmt.Errorf("listing messages: %w", err)
	}

	staleMessages := staleMessagesForSession(messages, sessionStart)
	if dryRun {
		printStaleArchiveDryRun(staleMessages)
		return nil
	}

	if len(staleMessages) == 0 {
		fmt.Printf("%s No stale messages to archive\n", style.Success.Render("✓"))
		return nil
	}

	return reportArchiveResult(archiveMessageIDs(mailbox, staleMessageIDs(staleMessages)), len(staleMessages), "stale")
}

func archiveSessionStart(address string) (time.Time, error) {
	identity, err := session.ParseAddress(address)
	if err != nil {
		return time.Time{}, fmt.Errorf("determining session for %s: %w", address, err)
	}
	sessionName := identity.SessionName()
	if sessionName == "" {
		return time.Time{}, fmt.Errorf("could not determine session name for %s", address)
	}
	start, err := session.SessionCreatedAt(sessionName)
	if err != nil {
		return time.Time{}, fmt.Errorf("getting session start time for %s: %w", sessionName, err)
	}
	return start, nil
}

func printStaleArchiveDryRun(messages []staleMessage) {
	if len(messages) == 0 {
		fmt.Printf("%s No stale messages found\n", style.Success.Render("✓"))
		return
	}
	fmt.Printf("%s Would archive %d stale message(s):\n", style.Dim.Render("(dry-run)"), len(messages))
	for _, stale := range messages {
		fmt.Printf("  %s %s\n", style.Dim.Render(stale.Message.ID), stale.Message.Subject)
	}
}

func staleMessageIDs(messages []staleMessage) []string {
	ids := make([]string, 0, len(messages))
	for _, stale := range messages {
		ids = append(ids, stale.Message.ID)
	}
	return ids
}

func staleMessagesForSession(messages []*mail.Message, sessionStart time.Time) []staleMessage {
	var staleMessages []staleMessage
	for _, msg := range messages {
		stale, reason := session.StaleReasonForTimes(msg.Timestamp, sessionStart)
		if stale {
			staleMessages = append(staleMessages, staleMessage{Message: msg, Reason: reason})
		}
	}
	return staleMessages
}

func runMailMarkRead(cmd *cobra.Command, args []string) error {
	address := detectSender()

	mailbox, err := getMailbox(address)
	if err != nil {
		return err
	}

	// --all: mark all unread messages as read
	if commandBoolFlag(cmd, "all") {
		return markAllMailRead(mailbox, args)
	}

	if len(args) == 0 {
		return fmt.Errorf("message ID required (or use --all to mark all as read)")
	}

	marked, markErrors := markReadIDs(mailbox, args)
	return reportMarkedMessages(marked, markErrors, len(args), "read")
}

func markAllMailRead(mailbox *mail.Mailbox, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("--all cannot be combined with explicit message IDs")
	}
	messages, err := mailbox.ListUnread()
	if err != nil {
		return fmt.Errorf("listing unread messages: %w", err)
	}
	if len(messages) == 0 {
		fmt.Printf("%s No unread messages\n", style.Bold.Render("✓"))
		return nil
	}
	ids := make([]string, 0, len(messages))
	for _, msg := range messages {
		ids = append(ids, msg.ID)
	}
	marked := markReadIDsWithWarnings(mailbox, ids)
	fmt.Printf("%s Marked %d messages as read\n", style.Bold.Render("✓"), marked)
	return nil
}

func markReadIDs(mailbox *mail.Mailbox, ids []string) (int, []string) {
	marked, errors := 0, []string(nil)
	for _, msgID := range ids {
		if err := mailbox.MarkReadOnly(msgID); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", msgID, err))
		} else {
			marked++
		}
	}
	return marked, errors
}

func markReadIDsWithWarnings(mailbox *mail.Mailbox, ids []string) int {
	marked := 0
	for _, msgID := range ids {
		if err := mailbox.MarkReadOnly(msgID); err != nil {
			style.PrintWarning("could not mark %s as read: %v", msgID, err)
		} else {
			marked++
		}
	}
	return marked
}

func reportMarkedMessages(marked int, errors []string, requested int, state string) error {
	if len(errors) > 0 {
		fmt.Printf("%s Marked %d/%d messages as %s\n", style.Bold.Render("⚠"), marked, requested, state)
		for _, errMsg := range errors {
			fmt.Printf("  Error: %s\n", errMsg)
		}
		return fmt.Errorf("failed to mark %d messages", len(errors))
	}
	if requested == 1 {
		fmt.Printf("%s Message marked as %s\n", style.Bold.Render("✓"), state)
	} else {
		fmt.Printf("%s Marked %d messages as %s\n", style.Bold.Render("✓"), marked, state)
	}
	return nil
}

func runMailMarkUnread(_ *cobra.Command, args []string) error {
	// Determine which inbox
	address := detectSender()

	mailbox, err := getMailbox(address)
	if err != nil {
		return err
	}

	// Mark all specified messages as unread
	marked := 0
	var errors []string
	for _, msgID := range args {
		if err := mailbox.MarkUnreadOnly(msgID); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", msgID, err))
		} else {
			marked++
		}
	}

	// Report results
	if len(errors) > 0 {
		fmt.Printf("%s Marked %d/%d messages as unread\n",
			style.Bold.Render("⚠"), marked, len(args))
		for _, e := range errors {
			fmt.Printf("  Error: %s\n", e)
		}
		return fmt.Errorf("failed to mark %d messages", len(errors))
	}

	if len(args) == 1 {
		fmt.Printf("%s Message marked as unread\n", style.Bold.Render("✓"))
	} else {
		fmt.Printf("%s Marked %d messages as unread\n", style.Bold.Render("✓"), marked)
	}
	return nil
}

func runMailClear(_ *cobra.Command, args []string) error {
	address := mailClearAddress(args)

	mailbox, err := getMailbox(address)
	if err != nil {
		return err
	}

	// List all messages
	messages, err := mailbox.List()
	if err != nil {
		return fmt.Errorf("listing messages: %w", err)
	}

	if len(messages) == 0 {
		fmt.Printf("%s Inbox %s is already empty\n", style.Dim.Render("○"), address)
		return nil
	}

	deleted, deleteErrors := clearMailMessages(mailbox, messages)
	return reportClearedMessages(address, len(messages), deleted, deleteErrors)
}

func mailClearAddress(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return detectSender()
}

func clearMailMessages(mailbox *mail.Mailbox, messages []*mail.Message) (int, []string) {
	deleted := 0
	var errors []string
	for _, msg := range messages {
		err := mailbox.Delete(msg.ID)
		if err == nil {
			deleted++
			continue
		}
		if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
			continue
		}
		errors = append(errors, fmt.Sprintf("%s: %v", msg.ID, err))
	}
	return deleted, errors
}

func reportClearedMessages(address string, total, deleted int, errors []string) error {
	if len(errors) > 0 {
		fmt.Printf("%s Cleared %d/%d messages from %s\n", style.Bold.Render("⚠"), deleted, total, address)
		for _, errMsg := range errors {
			fmt.Printf("  Error: %s\n", errMsg)
		}
		return fmt.Errorf("failed to clear %d messages", len(errors))
	}
	fmt.Printf("%s Cleared %d messages from %s\n", style.Bold.Render("✓"), deleted, address)
	return nil
}
