package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type mailSendOptions struct {
	content  mailSendContentOptions
	delivery mailSendDeliveryOptions
	routing  mailSendRoutingOptions
	stdin    bool
}

type mailSendContentOptions struct {
	subject  string
	body     string
	typeName string
	replyTo  string
}

type mailSendDeliveryOptions struct {
	priority  int
	urgent    bool
	pinned    bool
	wisp      bool
	permanent bool
	notify    bool
	noNotify  bool
	cc        []string
}

type mailSendRoutingOptions struct {
	to   string
	from string
	self bool
}

func mailSendOptionsFromCommand(cmd *cobra.Command) mailSendOptions {
	return mailSendOptions{
		content: mailSendContentOptions{
			subject:  commandStringFlag(cmd, "subject"),
			body:     commandStringAliasFlag(cmd, "message", "body"),
			typeName: commandStringFlag(cmd, "type"),
			replyTo:  commandStringFlag(cmd, "reply-to"),
		},
		delivery: mailSendDeliveryOptions{
			priority:  commandIntFlag(cmd, "priority"),
			urgent:    commandBoolFlag(cmd, "urgent"),
			pinned:    commandBoolFlag(cmd, "pinned"),
			wisp:      commandBoolFlag(cmd, "wisp"),
			permanent: commandBoolFlag(cmd, "permanent"),
			notify:    commandBoolFlag(cmd, "notify"),
			noNotify:  commandBoolFlag(cmd, "no-notify"),
			cc:        commandStringArrayFlag(cmd, "cc"),
		},
		routing: mailSendRoutingOptions{
			to:   commandStringFlag(cmd, "to"),
			from: commandStringFlag(cmd, "from"),
			self: commandBoolFlag(cmd, "self"),
		},
		stdin: commandBoolFlag(cmd, "stdin"),
	}
}

func runMailSend(cmd *cobra.Command, args []string) error {
	opts := mailSendOptionsFromCommand(cmd)
	if err := readMailStdin(&opts); err != nil {
		return err
	}

	to, err := resolveMailRecipient(args, opts)
	if err != nil {
		return err
	}

	// All mail uses town beads (two-level architecture)
	workDir, err := findMailWorkDir()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Determine sender (--from overrides auto-detection, for relay/bridge use)
	from := opts.routing.from
	if from == "" {
		from = detectSender()
	}

	// If subject looks like a reply ("Re: ...") but the user didn't pass
	// --reply-to, try to infer the original message from the sender's inbox.
	// When exactly one unambiguous match exists, populate mailReplyTo so the
	// existing thread-lookup + ClearReplyReminders flow below works as designed.
	// hq-k382x: without this, every "gt mail send <addr> -s 'Re: ...'" leaves
	// the queued reply-reminder in place.
	maybeInferMailReply(workDir, from, to, &opts)

	msg := buildMailSendMessage(workDir, from, to, opts)

	// Use address resolver for new address types
	townRoot, _ := workspace.FindFromCwd()
	resolver := mail.NewResolver(beads.New(townRoot), townRoot)

	recipients, err := resolver.Resolve(to)
	if err != nil {
		// Validation errors are definitive — do not fall back to legacy routing,
		// which would silently deliver to a dead inbox.
		// See: https://github.com/steveyegge/gastown/issues/2038
		if errors.Is(err, mail.ErrUnknownRecipient) {
			return err
		}
		return sendLegacyMail(workDir, from, to, msg, opts.content.subject)
	}
	return sendResolvedMail(workDir, from, to, msg, recipients, opts.content.subject)
}

// readMailStdin replaces the message body with stdin content when requested.
func readMailStdin(opts *mailSendOptions) error {
	if !opts.stdin {
		return nil
	}
	if opts.content.body != "" {
		return fmt.Errorf("cannot use --stdin with --message/-m")
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	opts.content.body = strings.TrimRight(string(data), "\n")
	return nil
}

func resolveMailRecipient(args []string, opts mailSendOptions) (string, error) {
	if opts.routing.self {
		return resolveSelfMailRecipient()
	}
	if opts.routing.to != "" {
		return opts.routing.to, nil
	}
	if len(args) > 0 {
		return args[0], nil
	}
	return "", fmt.Errorf("address required (use positional arg, --to, or --self)")
}

func resolveSelfMailRecipient() (string, error) {
	// Auto-detect identity from cwd.
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting current directory: %w", err)
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return "", fmt.Errorf("not in a Gas Town workspace")
	}
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return "", fmt.Errorf("detecting role: %w", err)
	}
	ctx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	to := buildAgentIdentity(ctx)
	if to == "" {
		return "", fmt.Errorf("cannot determine identity (role: %s)", ctx.Role)
	}
	return to, nil
}

func maybeInferMailReply(workDir, from, to string, opts *mailSendOptions) {
	if opts.content.replyTo != "" || !hasReplyPrefix(opts.content.subject) {
		return
	}
	if inferred := inferReplyTo(workDir, from, to, opts.content.subject); inferred != "" {
		opts.content.replyTo = inferred
	}
}

func buildMailSendMessage(workDir, from, to string, opts mailSendOptions) *mail.Message {
	msg := mail.NewMessage(from, to, opts.content.subject, opts.content.body)
	if opts.delivery.urgent {
		msg.Priority = mail.PriorityUrgent
	} else {
		msg.Priority = mail.PriorityFromInt(opts.delivery.priority)
	}
	if opts.delivery.notify && msg.Priority == mail.PriorityNormal {
		msg.Priority = mail.PriorityHigh
	}
	msg.Type = mail.ParseMessageType(opts.content.typeName)
	msg.Pinned = opts.delivery.pinned
	msg.Wisp = opts.delivery.wisp && !opts.delivery.permanent
	msg.CC = opts.delivery.cc
	if opts.delivery.noNotify {
		msg.SuppressNotify = true
	}
	applyMailReply(workDir, from, opts.content.replyTo, msg)
	if msg.ThreadID == "" {
		msg.ThreadID = generateThreadID()
	}
	return msg
}

func applyMailReply(workDir, from, replyTo string, msg *mail.Message) {
	if replyTo == "" {
		return
	}
	msg.ReplyTo = replyTo
	if msg.Type == mail.TypeNotification {
		msg.Type = mail.TypeReply
	}
	router := mail.NewRouter(workDir)
	mailbox, err := router.GetMailbox(from)
	if err != nil {
		style.PrintWarning("could not open mailbox for thread lookup: %v", err)
		return
	}
	original, err := mailbox.Get(replyTo)
	if err != nil {
		style.PrintWarning("could not find original message %s for threading (new thread will be created)", replyTo)
		return
	}
	msg.ThreadID = original.ThreadID
}

func sendLegacyMail(workDir, from, to string, msg *mail.Message, subject string) error {
	// Fall back to legacy routing for infrastructure errors (beads down, etc.).
	router := mail.NewRouter(workDir)
	defer mail.WaitPendingNotifications(router)
	if err := router.Send(msg); err != nil {
		return fmt.Errorf("sending message: %w", err)
	}
	_ = events.LogFeed(events.TypeMail, from, events.MailPayload(to, subject))
	fmt.Printf("%s Message sent to %s\n", style.Bold.Render("✓"), to)
	fmt.Printf("  Subject: %s\n", subject)
	return nil
}

func sendResolvedMail(workDir, from, to string, msg *mail.Message, recipients []mail.Recipient, subject string) error {
	router := mail.NewRouter(workDir)
	defer mail.WaitPendingNotifications(router)
	recipientAddrs, sendErrs := sendMailRecipients(router, msg, recipients)
	if err := reportMailSendErrors(recipientAddrs, sendErrs); err != nil {
		return err
	}
	clearMailReplyReminders(router, from, msg.ThreadID, msg.ReplyTo)

	// Log mail event to activity feed
	_ = events.LogFeed(events.TypeMail, from, events.MailPayload(to, subject))

	printMailSendSummary(to, msg, recipientAddrs, subject)
	return nil
}

func reportMailSendErrors(recipientAddrs, sendErrs []string) error {
	if len(sendErrs) == 0 {
		return nil
	}
	if len(recipientAddrs) == 0 {
		return fmt.Errorf("all sends failed: %s", strings.Join(sendErrs, "; "))
	}
	fmt.Fprintf(os.Stderr, "⚠ Some deliveries failed: %s\n", strings.Join(sendErrs, "; "))
	return nil
}

func clearMailReplyReminders(router *mail.Router, from, threadID, replyTo string) {
	if replyTo == "" {
		return
	}
	if err := router.ClearReplyReminders(from, threadID); err != nil {
		style.PrintWarning("could not clear satisfied reply reminders: %v", err)
	}
}

func printMailSendSummary(to string, msg *mail.Message, recipientAddrs []string, subject string) {
	fmt.Printf("%s Message sent to %s\n", style.Bold.Render("✓"), to)
	fmt.Printf("  Subject: %s\n", subject)
	if len(recipientAddrs) > 1 || (len(recipientAddrs) == 1 && recipientAddrs[0] != to) {
		fmt.Printf("  Recipients: %s\n", strings.Join(recipientAddrs, ", "))
	}
	if len(msg.CC) > 0 {
		fmt.Printf("  CC: %s\n", strings.Join(msg.CC, ", "))
	}
	if msg.Type != mail.TypeNotification {
		fmt.Printf("  Type: %s\n", msg.Type)
	}
}

func sendMailRecipients(router *mail.Router, msg *mail.Message, recipients []mail.Recipient) ([]string, []string) {
	var recipientAddrs []string
	var sendErrs []string
	for _, rec := range recipients {
		if err := sendMailRecipient(router, msg, rec); err != nil {
			sendErrs = append(sendErrs, fmt.Sprintf("%s: %v", mailRecipientErrorPrefix(rec), err))
			continue
		}
		recipientAddrs = append(recipientAddrs, rec.Address)
	}
	return recipientAddrs, sendErrs
}

func sendMailRecipient(router *mail.Router, msg *mail.Message, rec mail.Recipient) error {
	switch rec.Type {
	case mail.RecipientQueue, mail.RecipientChannel:
		// Queue messages are claimed by workers; channel messages are broadcast.
		msg.To = rec.Address
		return router.Send(msg)
	default:
		// Direct/agent messages fan out with a fresh ID for each recipient.
		msgCopy := *msg
		msgCopy.To = rec.Address
		msgCopy.ID = ""
		return router.Send(&msgCopy)
	}
}

func mailRecipientErrorPrefix(rec mail.Recipient) string {
	switch rec.Type {
	case mail.RecipientQueue:
		return "queue " + rec.Address
	case mail.RecipientChannel:
		return "channel " + rec.Address
	default:
		return rec.Address
	}
}

// generateThreadID creates a random thread ID for new message threads.
func generateThreadID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b) // crypto/rand.Read only fails on broken system
	return "thread-" + hex.EncodeToString(b)
}

// hasReplyPrefix reports whether subject begins with a "Re:" prefix
// (case-insensitive, tolerating arbitrary whitespace after the colon).
func hasReplyPrefix(subject string) bool {
	s := strings.TrimSpace(subject)
	if len(s) < 3 {
		return false
	}
	return strings.EqualFold(s[:3], "re:")
}

// normalizeReplySubject strips leading "Re: " prefixes (case-insensitive,
// possibly nested) and surrounding whitespace, so that two subjects with
// different reply nesting compare equal.
func normalizeReplySubject(subject string) string {
	s := strings.TrimSpace(subject)
	for hasReplyPrefix(s) {
		s = strings.TrimSpace(s[3:])
	}
	return strings.ToLower(s)
}

// normalizeAddress lowercases an address and trims a trailing slash so that
// "Mayor/" and "mayor" compare equal. Matches identityVariants behavior in
// mail.Mailbox without depending on its internals.
func normalizeAddress(addr string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(addr)), "/")
}

// inferReplyTo searches the sender's mailbox for a single unambiguous message
// FROM `to` whose subject (after stripping "Re:" prefixes) matches `subject`.
// Returns the matching message ID when exactly one match exists; returns "" on
// no-match, ambiguity, or any error. Best-effort — used only as a convenience
// to make `gt mail send <to> -s "Re: ..."` clear queued reply-reminders.
func inferReplyTo(workDir, from, to, subject string) string {
	router := mail.NewRouter(workDir)
	mailbox, err := router.GetMailbox(from)
	if err != nil {
		return ""
	}
	messages, err := mailbox.List()
	if err != nil {
		return ""
	}
	return pickReplyTo(messages, to, subject)
}

// pickReplyTo is the pure matching logic for inferReplyTo: given a list of
// candidate messages, returns the single matching message's ID, or "" if there
// is no match or more than one match. Pure to keep it unit-testable.
func pickReplyTo(messages []*mail.Message, to, subject string) string {
	wantSubject := normalizeReplySubject(subject)
	if wantSubject == "" {
		return ""
	}
	wantFrom := normalizeAddress(to)

	var matchID string
	matches := 0
	for _, m := range messages {
		if normalizeAddress(m.From) != wantFrom {
			continue
		}
		if normalizeReplySubject(m.Subject) != wantSubject {
			continue
		}
		matches++
		matchID = m.ID
		if matches > 1 {
			return ""
		}
	}
	if matches != 1 {
		return ""
	}
	return matchID
}
