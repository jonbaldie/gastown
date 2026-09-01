package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// runMailClaim claims the oldest unclaimed message from a work queue.
// If a queue name is provided, claims from that specific queue.
// If no queue name is provided, claims from any queue the caller is eligible for.
func runMailClaim(_ *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	caller := detectSender()
	beadsDir := beads.ResolveBeadsDir(townRoot)
	bd := beads.NewWithBeadsDir(townRoot, beadsDir)

	queue, err := selectClaimQueue(bd, args, caller)
	if err != nil {
		return err
	}
	if queue.name == "" {
		fmt.Printf("%s No queues available for claiming (caller: %s)\n", style.Dim.Render("○"), caller)
		return nil
	}

	messages, err := listUnclaimedQueueMessages(beadsDir, queue.name)
	if err != nil {
		return fmt.Errorf("listing queue messages: %w", err)
	}

	if len(messages) == 0 {
		fmt.Printf("%s No messages to claim in queue %s\n", style.Dim.Render("○"), queue.name)
		return nil
	}

	claimed, err := claimFirstQueueMessage(townRoot, beadsDir, caller, messages)
	if err != nil {
		return err
	}

	if claimed == nil {
		fmt.Printf("%s No messages to claim in queue %s (all contested)\n", style.Dim.Render("○"), queue.name)
		return nil
	}

	printClaimedQueueMessage(queue.name, claimed)

	return nil
}

type claimQueueSelection struct {
	name   string
	fields *beads.QueueFields
}

func selectClaimQueue(bd *beads.Beads, args []string, caller string) (claimQueueSelection, error) {
	if len(args) > 0 {
		return selectNamedClaimQueue(bd, args[0], caller)
	}
	issues, fields, err := bd.FindEligibleQueues(caller)
	if err != nil {
		return claimQueueSelection{}, fmt.Errorf("finding eligible queues: %w", err)
	}
	if len(issues) == 0 {
		return claimQueueSelection{}, nil
	}
	name := fields[0].Name
	if name == "" {
		name = issues[0].ID
	}
	return claimQueueSelection{name: name, fields: fields[0]}, nil
}

func selectNamedClaimQueue(bd *beads.Beads, name, caller string) (claimQueueSelection, error) {
	fields, err := findNamedClaimQueue(bd, name)
	if err != nil {
		return claimQueueSelection{}, err
	}
	if !beads.MatchClaimPattern(fields.ClaimPattern, caller) {
		return claimQueueSelection{}, fmt.Errorf("not eligible to claim from queue %s (caller: %s, pattern: %s)", name, caller, fields.ClaimPattern)
	}
	return claimQueueSelection{name: name, fields: fields}, nil
}

func findNamedClaimQueue(bd *beads.Beads, name string) (*beads.QueueFields, error) {
	for _, townLevel := range []bool{true, false} {
		issue, fields, err := bd.GetQueueBead(beads.QueueBeadID(name, townLevel))
		if err != nil {
			return nil, fmt.Errorf("looking up queue: %w", err)
		}
		if issue != nil {
			return fields, nil
		}
	}
	return nil, fmt.Errorf("unknown queue: %s", name)
}

func claimFirstQueueMessage(townRoot, beadsDir, caller string, messages []queueMessage) (*queueMessage, error) {
	for i := range messages {
		claimed, err := claimQueueCandidate(townRoot, beadsDir, caller, &messages[i])
		if err != nil {
			return nil, err
		}
		if claimed != nil {
			return claimed, nil
		}
	}
	return nil, nil
}

func claimQueueCandidate(townRoot, beadsDir, caller string, candidate *queueMessage) (*queueMessage, error) {
	if err := claimQueueMessage(beadsDir, candidate.ID, caller); err != nil {
		return nil, fmt.Errorf("claiming message: %w", err)
	}
	info, err := getQueueMessageInfo(beadsDir, candidate.ID)
	if err != nil {
		return nil, fmt.Errorf("verifying claim: %w", err)
	}
	if info.ClaimedBy == caller {
		ackClaimedQueueMessage(townRoot, beadsDir, caller, candidate.ID)
		return candidate, nil
	}
	if err := releaseQueueMessage(beadsDir, candidate.ID, caller); err != nil {
		style.PrintWarning("could not release stale claim on %s: %v", candidate.ID, err)
	}
	return nil, nil
}

func ackClaimedQueueMessage(townRoot, beadsDir, caller, messageID string) {
	if err := mail.AcknowledgeDeliveryBead(townRoot, beadsDir, messageID, mail.AddressToIdentity(caller)); err != nil {
		fmt.Fprintf(os.Stderr, "gt mail claim: delivery ack failed for %s: %v\n", messageID, err)
	}
}

func printClaimedQueueMessage(queueName string, claimed *queueMessage) {
	fmt.Printf("%s Claimed message from queue %s\n", style.Bold.Render("✓"), queueName)
	fmt.Printf("  ID: %s\n", claimed.ID)
	fmt.Printf("  Subject: %s\n", claimed.Title)
	if preview := queueMessagePreview(claimed.Description); preview != "" {
		fmt.Printf("  Preview: %s\n", style.Dim.Render(preview))
	}
	fmt.Printf("  From: %s\n", claimed.From)
	fmt.Printf("  Created: %s\n", claimed.Created.Local().Format("2006-01-02 15:04"))
}

func queueMessagePreview(description string) string {
	if description == "" {
		return ""
	}
	preview := strings.SplitN(description, "\n", 2)[0]
	if len(preview) > 80 {
		return preview[:77] + "..."
	}
	return preview
}

// queueMessage represents a message in a queue.
type queueMessage struct {
	ID          string
	Title       string
	Description string
	From        string
	Created     time.Time
	Priority    int
	ClaimedBy   string
	ClaimedAt   *time.Time
}

// listUnclaimedQueueMessages lists unclaimed messages in a queue.
// Unclaimed messages have queue:<name> label but no claimed-by label.
func listUnclaimedQueueMessages(beadsDir, queueName string) ([]queueMessage, error) {
	// Use bd list to find messages with queue:<name> label and status=open
	args := []string{"list",
		"--label", "queue:" + queueName,
		"--status", "open",
		"--label", "gt:message",
		"--json",
		"--limit", "0",
	}

	stdout, err := runQueueListCommand(beadsDir, args)
	if err != nil {
		return nil, err
	}

	// Parse JSON output
	var issues []struct {
		ID          string    `json:"id"`
		Title       string    `json:"title"`
		Description string    `json:"description"`
		Labels      []string  `json:"labels"`
		CreatedAt   time.Time `json:"created_at"`
		Priority    int       `json:"priority"`
	}

	if err := json.Unmarshal(stdout, &issues); err != nil {
		// If no messages, bd might output empty or error
		if strings.TrimSpace(string(stdout)) == "" || strings.TrimSpace(string(stdout)) == "[]" {
			return nil, nil
		}
		return nil, fmt.Errorf("parsing bd output: %w", err)
	}

	return queueMessagesFromIssues(issues), nil
}

func runQueueListCommand(beadsDir string, args []string) ([]byte, error) {
	cmd := beads.Spawn(args...)
	cmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func queueMessagesFromIssues(issues []struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Labels      []string  `json:"labels"`
	CreatedAt   time.Time `json:"created_at"`
	Priority    int       `json:"priority"`
}) []queueMessage {
	var messages []queueMessage
	for _, issue := range issues {
		msg := queueMessage{
			ID:          issue.ID,
			Title:       issue.Title,
			Description: issue.Description,
			Created:     issue.CreatedAt,
			Priority:    issue.Priority,
		}

		applyQueueMessageLabels(&msg, issue.Labels)
		// Only include unclaimed messages - check both ClaimedBy and ClaimedAt
		// to handle orphaned claimed-at labels from interrupted releases
		if msg.ClaimedBy == "" && msg.ClaimedAt == nil {
			messages = append(messages, msg)
		}
	}

	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Created.Before(messages[j].Created)
	})

	return messages
}

func applyQueueMessageLabels(msg *queueMessage, labels []string) {
	for _, label := range labels {
		switch {
		case strings.HasPrefix(label, "from:"):
			msg.From = strings.TrimPrefix(label, "from:")
		case strings.HasPrefix(label, "claimed-by:"):
			msg.ClaimedBy = strings.TrimPrefix(label, "claimed-by:")
		case strings.HasPrefix(label, "claimed-at:"):
			if timestamp, err := time.Parse(time.RFC3339, strings.TrimPrefix(label, "claimed-at:")); err == nil {
				msg.ClaimedAt = &timestamp
			}
		}
	}
}

// claimQueueMessage claims a message by adding claimed-by and claimed-at labels.
func claimQueueMessage(beadsDir, messageID, claimant string) error {
	now := time.Now().UTC().Format(time.RFC3339)

	args := []string{"label", "add", messageID,
		"claimed-by:" + claimant,
		"claimed-at:" + now,
	}

	cmd := beads.Spawn(args...)
	cmd.Env = append(os.Environ(),
		"BEADS_DIR="+beadsDir,
		"BD_ACTOR="+claimant,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
		return err
	}

	return nil
}

// runMailRelease releases a claimed queue message back to its queue.
func runMailRelease(_ *cobra.Command, args []string) error {
	messageID := args[0]

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	beadsDir := beads.ResolveBeadsDir(townRoot)

	// Get caller identity
	caller := detectSender()

	// Get message details to verify ownership and find queue
	msgInfo, err := getQueueMessageInfo(beadsDir, messageID)
	if err != nil {
		return fmt.Errorf("getting message: %w", err)
	}

	// Verify message exists and is a queue message
	if msgInfo.QueueName == "" {
		return fmt.Errorf("message %s is not a queue message (no queue label)", messageID)
	}

	// Verify caller is the one who claimed it
	if msgInfo.ClaimedBy == "" {
		return fmt.Errorf("message %s is not claimed", messageID)
	}
	if msgInfo.ClaimedBy != caller {
		return fmt.Errorf("message %s was claimed by %s, not %s", messageID, msgInfo.ClaimedBy, caller)
	}

	// Release the message: remove claimed-by and claimed-at labels
	if err := releaseQueueMessage(beadsDir, messageID, caller); err != nil {
		return fmt.Errorf("releasing message: %w", err)
	}

	fmt.Printf("%s Released message back to queue %s\n", style.Bold.Render("✓"), msgInfo.QueueName)
	fmt.Printf("  ID: %s\n", messageID)
	fmt.Printf("  Subject: %s\n", msgInfo.Title)

	return nil
}

// queueMessageInfo holds details about a queue message.
type queueMessageInfo struct {
	ID        string
	Title     string
	QueueName string
	ClaimedBy string
	ClaimedAt *time.Time
	Status    string
}

// getQueueMessageInfo retrieves information about a queue message.
func getQueueMessageInfo(beadsDir, messageID string) (*queueMessageInfo, error) {
	stdout, err := runQueueShowCommand(beadsDir, messageID)
	if err != nil {
		return nil, err
	}

	// Parse JSON output - bd show --json returns an array
	var issues []struct {
		ID     string   `json:"id"`
		Title  string   `json:"title"`
		Labels []string `json:"labels"`
		Status string   `json:"status"`
	}

	if err := json.Unmarshal(stdout, &issues); err != nil {
		return nil, fmt.Errorf("parsing message: %w", err)
	}

	if len(issues) == 0 {
		return nil, fmt.Errorf("message not found: %s", messageID)
	}

	return queueMessageInfoFromIssue(issues[0]), nil
}

func runQueueShowCommand(beadsDir, messageID string) ([]byte, error) {
	cmd := beads.Spawn("show", messageID, "--json")
	cmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if strings.Contains(errMsg, "not found") {
			return nil, fmt.Errorf("message not found: %s", messageID)
		}
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func queueMessageInfoFromIssue(issue struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Labels []string `json:"labels"`
	Status string   `json:"status"`
}) *queueMessageInfo {
	info := &queueMessageInfo{ID: issue.ID, Title: issue.Title, Status: issue.Status}
	for _, label := range issue.Labels {
		applyQueueInfoLabel(info, label)
	}
	return info
}

func applyQueueInfoLabel(info *queueMessageInfo, label string) {
	switch {
	case strings.HasPrefix(label, "queue:"):
		info.QueueName = strings.TrimPrefix(label, "queue:")
	case strings.HasPrefix(label, "claimed-by:"):
		info.ClaimedBy = strings.TrimPrefix(label, "claimed-by:")
	case strings.HasPrefix(label, "claimed-at:"):
		if timestamp, err := time.Parse(time.RFC3339, strings.TrimPrefix(label, "claimed-at:")); err == nil {
			info.ClaimedAt = &timestamp
		}
	}
}

// releaseQueueMessage releases a claimed message by removing claim labels.
// Both claimed-by and claimed-at are removed in a single bd command to prevent
// orphaned labels if the process crashes between separate removal steps.
func releaseQueueMessage(beadsDir, messageID, actor string) error {
	// Get current message info to find the exact claim labels
	info, err := getQueueMessageInfo(beadsDir, messageID)
	if err != nil {
		return err
	}

	// Collect labels to remove in a single atomic operation
	var labelsToRemove []string
	if info.ClaimedBy != "" {
		labelsToRemove = append(labelsToRemove, "claimed-by:"+info.ClaimedBy)
	}
	if info.ClaimedAt != nil {
		labelsToRemove = append(labelsToRemove, "claimed-at:"+info.ClaimedAt.Format(time.RFC3339))
	}

	if len(labelsToRemove) == 0 {
		return nil
	}

	// Remove all claim labels in a single bd command
	args := append([]string{"label", "remove", messageID}, labelsToRemove...)
	cmd := beads.Spawn(args...)
	cmd.Env = append(os.Environ(),
		"BEADS_DIR="+beadsDir,
		"BD_ACTOR="+actor,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" && !strings.Contains(errMsg, "does not have label") {
			return fmt.Errorf("%s", errMsg)
		}
	}

	return nil
}

// Queue management commands (beads-native)

var mailQueueCmd = &cobra.Command{
	Use:   "queue",
	Short: "Manage mail queues",
	Long: `Manage beads-native mail queues.

Queues provide a way to distribute work to eligible workers.
Messages sent to a queue can be claimed by workers matching the claim pattern.

COMMANDS:
  create    Create a new queue
  show      Show queue details
  list      List all queues
  delete    Delete a queue

Examples:
  gt mail queue create work --claimers 'gastown/polecats/*'
  gt mail queue show work
  gt mail queue list
  gt mail queue delete work`,
	RunE: requireSubcommand,
}

var mailQueueCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new queue",
	Long: `Create a new beads-native mail queue.

The --claimers flag specifies a pattern for who can claim messages from this queue.
Patterns support wildcards: 'gastown/polecats/*' matches any polecat in gastown rig.

Examples:
  gt mail queue create work --claimers 'gastown/polecats/*'
  gt mail queue create dispatch --claimers 'gastown/crew/*'
  gt mail queue create urgent --claimers '*'`,
	Args: cobra.ExactArgs(1),
	RunE: runMailQueueCreate,
}

var mailQueueShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show queue details",
	Long: `Show details about a mail queue.

Displays the queue's claim pattern, status, and message counts.

Examples:
  gt mail queue show work
  gt mail queue show dispatch --json`,
	Args: cobra.ExactArgs(1),
	RunE: runMailQueueShow,
}

var mailQueueListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all queues",
	Long: `List all beads-native mail queues.

Shows queue names, claim patterns, and status.

Examples:
  gt mail queue list
  gt mail queue list --json`,
	RunE: runMailQueueList,
}

var mailQueueDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a queue",
	Long: `Delete a mail queue.

This permanently removes the queue bead. Messages in the queue are not affected.

Examples:
  gt mail queue delete work`,
	Args: cobra.ExactArgs(1),
	RunE: runMailQueueDelete,
}

func init() {
	// Queue create flags
	mailQueueCreateCmd.Flags().String("claimers", "", "Pattern for who can claim from this queue (required)")
	_ = mailQueueCreateCmd.MarkFlagRequired("claimers")

	// Queue show/list flags
	mailQueueShowCmd.Flags().Bool("json", false, "Output as JSON")
	mailQueueListCmd.Flags().Bool("json", false, "Output as JSON")

	// Add queue subcommands
	mailQueueCmd.AddCommand(mailQueueCreateCmd)
	mailQueueCmd.AddCommand(mailQueueShowCmd)
	mailQueueCmd.AddCommand(mailQueueListCmd)
	mailQueueCmd.AddCommand(mailQueueDeleteCmd)

	// Add queue command to mail
	getMailCommand().AddCommand(mailQueueCmd)
}

// runMailQueueCreate creates a new beads-native queue.
func runMailQueueCreate(cmd *cobra.Command, args []string) error {
	queueName := args[0]
	claimers := commandStringFlag(cmd, "claimers")

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Get caller identity for created_by
	caller := detectSender()

	// Create queue bead
	b := beads.NewWithBeadsDir(townRoot, beads.ResolveBeadsDir(townRoot))

	// Generate queue bead ID (town-level: hq-q-<name>)
	queueID := beads.QueueBeadID(queueName, true)

	// Check if queue already exists
	existing, _, err := b.GetQueueBead(queueID)
	if err != nil {
		return fmt.Errorf("checking for existing queue: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("queue %q already exists", queueName)
	}

	// Create queue fields
	fields := &beads.QueueFields{
		Name:         queueName,
		ClaimPattern: claimers,
		Status:       beads.QueueStatusActive,
		CreatedBy:    caller,
		CreatedAt:    time.Now().Format(time.RFC3339),
	}

	title := fmt.Sprintf("Queue: %s", queueName)
	_, err = b.CreateQueueBead(queueID, title, fields)
	if err != nil {
		return fmt.Errorf("creating queue: %w", err)
	}

	fmt.Printf("%s Created queue %s\n", style.Bold.Render("✓"), queueName)
	fmt.Printf("  ID: %s\n", queueID)
	fmt.Printf("  Claimers: %s\n", claimers)

	return nil
}

// runMailQueueShow shows details about a queue.
func runMailQueueShow(cmd *cobra.Command, args []string) error {
	queueName := args[0]

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Get queue bead
	b := beads.NewWithBeadsDir(townRoot, beads.ResolveBeadsDir(townRoot))

	queueID := beads.QueueBeadID(queueName, true)
	issue, fields, err := b.GetQueueBead(queueID)
	if err != nil {
		return fmt.Errorf("getting queue: %w", err)
	}
	if issue == nil {
		return fmt.Errorf("queue %q not found", queueName)
	}

	if commandBoolFlag(cmd, "json") {
		output := map[string]interface{}{
			"id":               issue.ID,
			"name":             fields.Name,
			"claim_pattern":    fields.ClaimPattern,
			"status":           fields.Status,
			"available_count":  fields.AvailableCount,
			"processing_count": fields.ProcessingCount,
			"completed_count":  fields.CompletedCount,
			"failed_count":     fields.FailedCount,
			"created_by":       fields.CreatedBy,
			"created_at":       fields.CreatedAt,
		}
		jsonBytes, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	// Human-readable output
	fmt.Printf("%s Queue: %s\n", style.Bold.Render("📬"), queueName)
	fmt.Printf("  ID: %s\n", issue.ID)
	fmt.Printf("  Claimers: %s\n", fields.ClaimPattern)
	fmt.Printf("  Status: %s\n", fields.Status)
	fmt.Printf("  Available: %d\n", fields.AvailableCount)
	fmt.Printf("  Processing: %d\n", fields.ProcessingCount)
	fmt.Printf("  Completed: %d\n", fields.CompletedCount)
	if fields.FailedCount > 0 {
		fmt.Printf("  Failed: %d\n", fields.FailedCount)
	}
	if fields.CreatedBy != "" {
		fmt.Printf("  Created by: %s\n", fields.CreatedBy)
	}
	if fields.CreatedAt != "" {
		fmt.Printf("  Created at: %s\n", fields.CreatedAt)
	}

	return nil
}

// runMailQueueList lists all queues.
func runMailQueueList(cmd *cobra.Command, _ []string) error {
	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// List queue beads
	b := beads.NewWithBeadsDir(townRoot, beads.ResolveBeadsDir(townRoot))

	queues, err := b.ListQueueBeads()
	if err != nil {
		return fmt.Errorf("listing queues: %w", err)
	}

	if len(queues) == 0 {
		fmt.Printf("%s No queues found\n", style.Dim.Render("○"))
		return nil
	}

	if commandBoolFlag(cmd, "json") {
		var output []map[string]interface{}
		for _, issue := range queues {
			fields := beads.ParseQueueFields(issue.Description)
			output = append(output, map[string]interface{}{
				"id":            issue.ID,
				"name":          fields.Name,
				"claim_pattern": fields.ClaimPattern,
				"status":        fields.Status,
			})
		}
		jsonBytes, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling JSON: %w", err)
		}
		fmt.Println(string(jsonBytes))
		return nil
	}

	// Human-readable output
	fmt.Printf("%s Queues (%d)\n\n", style.Bold.Render("📬"), len(queues))
	for _, issue := range queues {
		fields := beads.ParseQueueFields(issue.Description)
		fmt.Printf("  %s\n", style.Bold.Render(fields.Name))
		fmt.Printf("    Claimers: %s\n", fields.ClaimPattern)
		fmt.Printf("    Status: %s\n", fields.Status)
	}

	return nil
}

// runMailQueueDelete deletes a queue.
func runMailQueueDelete(_ *cobra.Command, args []string) error {
	queueName := args[0]

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Delete queue bead
	b := beads.NewWithBeadsDir(townRoot, beads.ResolveBeadsDir(townRoot))

	queueID := beads.QueueBeadID(queueName, true)

	// Verify queue exists
	issue, _, err := b.GetQueueBead(queueID)
	if err != nil {
		return fmt.Errorf("getting queue: %w", err)
	}
	if issue == nil {
		return fmt.Errorf("queue %q not found", queueName)
	}

	if err := b.DeleteQueueBead(queueID); err != nil {
		return fmt.Errorf("deleting queue: %w", err)
	}

	fmt.Printf("%s Deleted queue %s\n", style.Bold.Render("✓"), queueName)

	return nil
}
