package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type escalateOptions struct {
	severity    string
	reason      string
	source      string
	relatedBead string
	fingerprint string
	json        bool
	dryRun      bool
	stdin       bool
}

func escalateOptionsFromCommand(cmd *cobra.Command) escalateOptions {
	return escalateOptions{
		severity:    commandStringFlag(cmd, "severity"),
		reason:      commandStringFlag(cmd, "reason"),
		source:      commandStringFlag(cmd, "source"),
		relatedBead: commandStringFlag(cmd, "related"),
		fingerprint: commandStringFlag(cmd, "fingerprint"),
		json:        commandBoolFlag(cmd, "json"),
		dryRun:      commandBoolFlag(cmd, "dry-run"),
		stdin:       commandBoolFlag(cmd, "stdin"),
	}
}

func escalationFingerprintLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("escalation-fp:%x", sum[:6])
}

type deliveryStatus struct {
	Target            string `json:"target,omitempty"`
	Channel           string `json:"channel"`
	Created           bool   `json:"created,omitempty"`
	Persisted         bool   `json:"persisted,omitempty"`
	RuntimeNotified   bool   `json:"runtime_notified,omitempty"`
	Annotated         bool   `json:"annotated,omitempty"`
	Severity          string `json:"severity,omitempty"`
	Error             string `json:"error,omitempty"`
	Warning           string `json:"warning,omitempty"`
	NotificationRoute string `json:"notification_route,omitempty"`
}

func runEscalateList(cmd *cobra.Command, _ []string) error {
	issues, phantomCount, err := loadEscalateListIssues(commandBoolFlag(cmd, "all"))
	if err != nil {
		return err
	}
	return printEscalateList(commandBoolFlag(cmd, "json"), issues, phantomCount)
}

func loadEscalateListIssues(listAll bool) ([]*beads.Issue, int, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, 0, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	issues, err := fetchEscalateListIssues(bd, listAll)
	if err != nil {
		return nil, 0, err
	}
	return filterLiveEscalateIssues(bd, issues)
}

func fetchEscalateListIssues(bd *beads.Beads, listAll bool) ([]*beads.Issue, error) {
	if !listAll {
		issues, err := bd.ListEscalations()
		if err != nil {
			return nil, fmt.Errorf("listing escalations: %w", err)
		}
		return issues, nil
	}
	out, err := bd.Run("list", "--label=gt:escalation", "--status=all", "--json")
	if err != nil {
		return nil, fmt.Errorf("listing escalations: %w", err)
	}
	var issues []*beads.Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing escalations: %w", err)
	}
	return issues, nil
}

func filterLiveEscalateIssues(bd *beads.Beads, issues []*beads.Issue) ([]*beads.Issue, int, error) {
	var live []*beads.Issue
	var phantomCount int
	for _, issue := range issues {
		if _, err := bd.Show(issue.ID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				phantomCount++
				fmt.Fprintf(os.Stderr, "warning: skipping unresolvable escalation %s (not found in live Dolt)\n", issue.ID)
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: could not verify escalation %s: %v\n", issue.ID, err)
		}
		live = append(live, issue)
	}
	return live, phantomCount, nil
}

func printEscalateList(listJSON bool, issues []*beads.Issue, phantomCount int) error {
	if listJSON {
		out, _ := json.MarshalIndent(issues, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	if len(issues) == 0 {
		printEscalateListEmpty(phantomCount)
		return nil
	}
	fmt.Printf("Escalations (%d):\n\n", len(issues))
	for _, issue := range issues {
		printEscalateListItem(issue)
	}
	return nil
}

func printEscalateListEmpty(phantomCount int) {
	if phantomCount == 0 {
		fmt.Println("No escalations found")
		return
	}
	fmt.Printf("No escalations found (%d phantom entr%s skipped — bead IDs no longer exist in live Dolt)\n",
		phantomCount, map[bool]string{true: "y", false: "ies"}[phantomCount == 1])
}

func printEscalateListItem(issue *beads.Issue) {
	fields := beads.ParseEscalationFields(issue.Description)
	status := issue.Status
	if beads.HasLabel(issue, "acked") {
		status = "acked"
	}
	fmt.Printf("  %s %s [%s] %s\n", severityEmoji(fields.Severity), issue.ID, status, issue.Title)
	fmt.Printf("     Severity: %s | From: %s | %s\n",
		fields.Severity, fields.EscalatedBy, formatRelativeTime(issue.CreatedAt))
	if fields.AckedBy != "" {
		fmt.Printf("     Acked by: %s\n", fields.AckedBy)
	}
	fmt.Println()
}

func runEscalateAck(_ *cobra.Command, args []string) error {
	escalationID := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Detect who is acknowledging
	ackedBy := detectSender()
	if ackedBy == "" {
		ackedBy = "unknown"
	}

	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	if err := bd.AckEscalation(escalationID, ackedBy); err != nil {
		return fmt.Errorf("acknowledging escalation: %w", err)
	}

	// Log to activity feed
	_ = events.LogFeed(events.TypeEscalationAcked, ackedBy, map[string]interface{}{
		"escalation_id": escalationID,
		"acked_by":      ackedBy,
	})

	fmt.Printf("%s Escalation acknowledged: %s\n", style.Bold.Render("✓"), escalationID)
	return nil
}

func runEscalateClose(cmd *cobra.Command, args []string) error {
	closeReason := commandStringFlag(cmd, "reason")
	escalationID := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Detect who is closing
	closedBy := detectSender()
	if closedBy == "" {
		closedBy = "unknown"
	}

	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	if err := bd.CloseEscalation(escalationID, closedBy, closeReason); err != nil {
		return fmt.Errorf("closing escalation: %w", err)
	}

	// Log to activity feed
	_ = events.LogFeed(events.TypeEscalationClosed, closedBy, map[string]interface{}{
		"escalation_id": escalationID,
		"closed_by":     closedBy,
		"reason":        closeReason,
	})

	fmt.Printf("%s Escalation closed: %s\n", style.Bold.Render("✓"), escalationID)
	fmt.Printf("  Reason: %s\n", closeReason)
	return nil
}

func runEscalateStale(cmd *cobra.Command, _ []string) error {
	staleJSON := commandBoolFlag(cmd, "json")
	dryRun := commandBoolFlag(cmd, "dry-run")
	townRoot, escalationConfig, threshold, maxReescalations, stale, err := loadEscalateStale()
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		printEscalateStaleEmpty(staleJSON, threshold)
		return nil
	}
	reescalatedBy := detectSender()
	if reescalatedBy == "" {
		reescalatedBy = "system"
	}
	if dryRun {
		printEscalateStaleDryRun(stale, threshold, maxReescalations)
		return nil
	}
	results := reescalateStaleIssues(townRoot, escalationConfig, stale, reescalatedBy, maxReescalations)
	return printEscalateStaleResults(staleJSON, results)
}

func loadEscalateStale() (string, *config.EscalationConfig, time.Duration, int, []*beads.Issue, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", nil, 0, 0, nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	escalationConfig, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return "", nil, 0, 0, nil, fmt.Errorf("loading escalation config: %w", err)
	}
	threshold := escalationConfig.GetStaleThreshold()
	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	stale, err := bd.ListStaleEscalations(threshold)
	if err != nil {
		return "", nil, 0, 0, nil, fmt.Errorf("listing stale escalations: %w", err)
	}
	return townRoot, escalationConfig, threshold, escalationConfig.GetMaxReescalations(), stale, nil
}

func printEscalateStaleEmpty(staleJSON bool, threshold time.Duration) {
	if staleJSON {
		fmt.Println("[]")
		return
	}
	fmt.Printf("No stale escalations (threshold: %s)\n", threshold)
}

func printEscalateStaleDryRun(stale []*beads.Issue, threshold time.Duration, maxReescalations int) {
	fmt.Printf("Would re-escalate %d stale escalations (threshold: %s):\n\n", len(stale), threshold)
	for _, issue := range stale {
		printEscalateStaleDryRunItem(issue, maxReescalations)
	}
}

func printEscalateStaleDryRunItem(issue *beads.Issue, maxReescalations int) {
	fields := beads.ParseEscalationFields(issue.Description)
	willSkip := fields.Severity == "critical" || (maxReescalations > 0 && fields.ReescalationCount >= maxReescalations)
	emoji := severityEmoji(fields.Severity)
	if !willSkip {
		fmt.Printf("  %s %s %s\n", emoji, issue.ID, issue.Title)
		fmt.Printf("     %s → %s (reescalation %d/%d)\n",
			fields.Severity, getNextSeverity(fields.Severity), fields.ReescalationCount+1, maxReescalations)
		fmt.Println()
		return
	}
	fmt.Printf("  %s %s [SKIP] %s\n", emoji, issue.ID, issue.Title)
	if fields.Severity == "critical" {
		fmt.Printf("     Already at critical severity\n")
	} else {
		fmt.Printf("     Already at max reescalations (%d)\n", maxReescalations)
	}
	fmt.Println()
}

func reescalateStaleIssues(townRoot string, escalationConfig *config.EscalationConfig, stale []*beads.Issue, reescalatedBy string, maxReescalations int) []*beads.ReescalationResult {
	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	router := mail.NewRouter(townRoot)
	defer mail.WaitPendingNotifications(router)
	var results []*beads.ReescalationResult
	for _, issue := range stale {
		result, err := bd.ReescalateEscalation(issue.ID, reescalatedBy, maxReescalations)
		if err != nil {
			style.PrintWarning("failed to reescalate %s: %v", issue.ID, err)
			continue
		}
		results = append(results, result)
		if !result.Skipped {
			notifyReescalation(router, escalationConfig, result, reescalatedBy)
		}
	}
	return results
}

func notifyReescalation(router *mail.Router, escalationConfig *config.EscalationConfig, result *beads.ReescalationResult, reescalatedBy string) {
	actions := escalationConfig.GetRouteForSeverity(result.NewSeverity)
	targets := extractMailTargetsFromActions(actions)
	for _, target := range targets {
		msg := &mail.Message{
			From:     reescalatedBy,
			To:       target,
			Subject:  fmt.Sprintf("[%s→%s] Re-escalated: %s", strings.ToUpper(result.OldSeverity), strings.ToUpper(result.NewSeverity), result.Title),
			Body:     formatReescalationMailBody(result, reescalatedBy),
			Type:     mail.TypeTask,
			Priority: escalateMailPriority(result.NewSeverity),
		}
		if err := router.Send(msg); err != nil {
			style.PrintWarning("failed to send reescalation to %s: %v", target, err)
		}
	}
	_ = events.LogFeed(events.TypeEscalationSent, reescalatedBy, map[string]interface{}{
		"escalation_id":    result.ID,
		"reescalated":      true,
		"old_severity":     result.OldSeverity,
		"new_severity":     result.NewSeverity,
		"reescalation_num": result.ReescalationNum,
		"targets":          strings.Join(targets, ","),
	})
}

func printEscalateStaleResults(staleJSON bool, results []*beads.ReescalationResult) error {
	if staleJSON {
		out, _ := json.MarshalIndent(results, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	reescalated, skipped := countEscalateStaleResults(results)
	if reescalated == 0 && skipped > 0 {
		fmt.Printf("No escalations re-escalated (%d at max level)\n", skipped)
		return nil
	}
	fmt.Printf("🔄 Re-escalated %d stale escalations:\n\n", reescalated)
	for _, result := range results {
		if result.Skipped {
			continue
		}
		fmt.Printf("  %s %s: %s → %s (reescalation %d)\n",
			severityEmoji(result.NewSeverity), result.ID, result.OldSeverity, result.NewSeverity, result.ReescalationNum)
	}
	if skipped > 0 {
		fmt.Printf("\n  (%d skipped - at max level)\n", skipped)
	}
	return nil
}

func countEscalateStaleResults(results []*beads.ReescalationResult) (int, int) {
	var reescalated, skipped int
	for _, r := range results {
		if r.Skipped {
			skipped++
		} else {
			reescalated++
		}
	}
	return reescalated, skipped
}

func getNextSeverity(severity string) string {
	switch severity {
	case "low":
		return "medium"
	case "medium":
		return "high"
	case "high":
		return "critical"
	default:
		return "critical"
	}
}

func formatReescalationMailBody(result *beads.ReescalationResult, reescalatedBy string) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Escalation ID: %s", result.ID))
	lines = append(lines, fmt.Sprintf("Severity bumped: %s → %s", result.OldSeverity, result.NewSeverity))
	lines = append(lines, fmt.Sprintf("Reescalation #%d", result.ReescalationNum))
	lines = append(lines, fmt.Sprintf("Reescalated by: %s", reescalatedBy))
	lines = append(lines, "")
	lines = append(lines, "This escalation was not acknowledged within the stale threshold and has been automatically re-escalated to a higher severity.")
	lines = append(lines, "")
	lines = append(lines, "---")
	lines = append(lines, "To acknowledge: gt escalate ack "+result.ID)
	lines = append(lines, "To close: gt escalate close "+result.ID+" --reason \"resolution\"")
	return strings.Join(lines, "\n")
}

func runEscalateShow(cmd *cobra.Command, args []string) error {
	issue, fields, err := loadEscalateShow(args[0])
	if err != nil {
		return err
	}
	if commandBoolFlag(cmd, "json") {
		return printEscalateShowJSON(issue, fields)
	}
	printEscalateShowText(issue, fields)
	return nil
}

func loadEscalateShow(escalationID string) (*beads.Issue, *beads.EscalationFields, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	issue, fields, err := beads.New(beads.ResolveBeadsDir(townRoot)).GetEscalationBead(escalationID)
	if err != nil {
		return nil, nil, fmt.Errorf("getting escalation: %w", err)
	}
	if issue == nil {
		return nil, nil, fmt.Errorf("escalation not found: %s", escalationID)
	}
	return issue, fields, nil
}

func printEscalateShowJSON(issue *beads.Issue, fields *beads.EscalationFields) error {
	data := map[string]interface{}{
		"id":           issue.ID,
		"title":        issue.Title,
		"status":       issue.Status,
		"created_at":   issue.CreatedAt,
		"severity":     fields.Severity,
		"reason":       fields.Reason,
		"escalatedBy":  fields.EscalatedBy,
		"escalatedAt":  fields.EscalatedAt,
		"ackedBy":      fields.AckedBy,
		"ackedAt":      fields.AckedAt,
		"closedBy":     fields.ClosedBy,
		"closedReason": fields.ClosedReason,
		"relatedBead":  fields.RelatedBead,
	}
	out, _ := json.MarshalIndent(data, "", "  ")
	fmt.Println(string(out))
	return nil
}

func printEscalateShowText(issue *beads.Issue, fields *beads.EscalationFields) {
	fmt.Printf("%s Escalation: %s\n", severityEmoji(fields.Severity), issue.ID)
	fmt.Printf("  Title: %s\n", issue.Title)
	fmt.Printf("  Status: %s\n", issue.Status)
	fmt.Printf("  Severity: %s\n", fields.Severity)
	fmt.Printf("  Created: %s\n", formatRelativeTime(issue.CreatedAt))
	fmt.Printf("  Escalated by: %s\n", fields.EscalatedBy)
	printEscalateShowDetails(fields)
}

func printEscalateShowDetails(fields *beads.EscalationFields) {
	if fields.Reason != "" {
		fmt.Printf("  Reason: %s\n", fields.Reason)
	}
	if fields.AckedBy != "" {
		fmt.Printf("  Acknowledged by: %s at %s\n", fields.AckedBy, fields.AckedAt)
	}
	if fields.ClosedBy != "" {
		fmt.Printf("  Closed by: %s\n", fields.ClosedBy)
		fmt.Printf("  Resolution: %s\n", fields.ClosedReason)
	}
	if fields.RelatedBead != "" {
		fmt.Printf("  Related: %s\n", fields.RelatedBead)
	}
}

// Helper functions

// extractMailTargetsFromActions extracts mail targets from action strings.
// Action format: "mail:target" returns "target"
// E.g., ["bead", "mail:mayor", "email:human"] returns ["mayor"]
func extractMailTargetsFromActions(actions []string) []string {
	var targets []string
	for _, action := range actions {
		if strings.HasPrefix(action, "mail:") {
			target := strings.TrimPrefix(action, "mail:")
			if target != "" {
				targets = append(targets, target)
			}
		}
	}
	return targets
}

// executeExternalActions processes external notification actions (email:, sms:, slack, log).
func executeExternalActions(actions []string, cfg *config.EscalationConfig, beadID, severity, description, townRoot string) []deliveryStatus {
	var statuses []deliveryStatus
	for _, action := range actions {
		if status, ok := executeExternalAction(action, cfg, beadID, severity, description, townRoot); ok {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

func executeExternalAction(action string, cfg *config.EscalationConfig, beadID, severity, description, townRoot string) (deliveryStatus, bool) {
	switch {
	case strings.HasPrefix(action, "email:"):
		return executeEscalateEmailAction(action, cfg, beadID, severity, description), true
	case strings.HasPrefix(action, "sms:"):
		return executeEscalateSMSAction(action, cfg, beadID, severity, description), true
	case action == "slack":
		return executeEscalateSlackAction(cfg, beadID, severity, description), true
	case action == "log":
		return executeEscalateLogAction(townRoot, beadID, severity, description), true
	default:
		return deliveryStatus{}, false
	}
}

func executeEscalateEmailAction(action string, cfg *config.EscalationConfig, beadID, severity, description string) deliveryStatus {
	status := deliveryStatus{Channel: "email", Target: strings.TrimPrefix(action, "email:"), Severity: severity}
	switch {
	case cfg.Contacts.HumanEmail == "":
		status.Warning = "contacts.human_email not configured"
		style.PrintWarning("email action '%s' skipped: contacts.human_email not configured in settings/escalation.json", action)
	case cfg.Contacts.SMTPHost == "":
		status.Warning = "contacts.smtp_host not configured"
		style.PrintWarning("email action '%s' skipped: contacts.smtp_host not configured in settings/escalation.json", action)
	default:
		applyEscalateNotify(&status, sendEscalationEmail(cfg, beadID, severity, description), "email send failed: %v", "  📧 Email sent to %s\n", cfg.Contacts.HumanEmail)
	}
	return status
}

func executeEscalateSMSAction(action string, cfg *config.EscalationConfig, beadID, severity, description string) deliveryStatus {
	status := deliveryStatus{Channel: "sms", Target: strings.TrimPrefix(action, "sms:"), Severity: severity}
	switch {
	case cfg.Contacts.HumanSMS == "":
		status.Warning = "contacts.human_sms not configured"
		style.PrintWarning("sms action '%s' skipped: contacts.human_sms not configured in settings/escalation.json", action)
	case cfg.Contacts.SMSWebhook == "":
		status.Warning = "contacts.sms_webhook not configured"
		style.PrintWarning("sms action '%s' skipped: contacts.sms_webhook not configured in settings/escalation.json", action)
	default:
		applyEscalateNotify(&status, sendEscalationSMS(cfg, beadID, severity, description), "sms send failed: %v", "  📱 SMS sent to %s\n", cfg.Contacts.HumanSMS)
	}
	return status
}

func executeEscalateSlackAction(cfg *config.EscalationConfig, beadID, severity, description string) deliveryStatus {
	status := deliveryStatus{Channel: "slack", Target: "slack", Severity: severity}
	if cfg.Contacts.SlackWebhook == "" {
		status.Warning = "contacts.slack_webhook not configured"
		style.PrintWarning("slack action skipped: contacts.slack_webhook not configured in settings/escalation.json")
		return status
	}
	applyEscalateNotify(&status, sendEscalationSlack(cfg, beadID, severity, description), "slack post failed: %v", "  💬 Posted to Slack\n", "")
	return status
}

func executeEscalateLogAction(townRoot, beadID, severity, description string) deliveryStatus {
	status := deliveryStatus{Channel: "log", Target: "log", Severity: severity}
	applyEscalateNotify(&status, writeEscalationLog(townRoot, beadID, severity, description), "log write failed: %v", "  📝 Logged to escalation log\n", "")
	return status
}

func applyEscalateNotify(status *deliveryStatus, err error, failFmt, okFmt, okArg string) {
	if err != nil {
		status.Error = err.Error()
		style.PrintWarning(failFmt, err)
		return
	}
	status.RuntimeNotified = true
	if okArg == "" {
		fmt.Print(okFmt)
		return
	}
	fmt.Printf(okFmt, okArg)
}

// sendEscalationEmail sends an escalation notification via SMTP.
func sendEscalationEmail(cfg *config.EscalationConfig, beadID, severity, description string) error {
	host := cfg.Contacts.SMTPHost
	port := cfg.Contacts.SMTPPort
	if port == "" {
		port = "587"
	}
	from := cfg.Contacts.SMTPFrom
	if from == "" {
		from = "gastown@localhost"
	}
	to := cfg.Contacts.HumanEmail
	subject := fmt.Sprintf("[Gas Town %s] %s", strings.ToUpper(severity), description)

	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n"+
		"Gas Town Escalation\r\n"+
		"====================\r\n"+
		"Bead: %s\r\n"+
		"Severity: %s\r\n"+
		"Description: %s\r\n\r\n"+
		"Acknowledge: gt escalate ack %s\r\n",
		from, to, subject, beadID, strings.ToUpper(severity), description, beadID)

	addr := fmt.Sprintf("%s:%s", host, port)

	var auth smtp.Auth
	if cfg.Contacts.SMTPUser != "" {
		auth = smtp.PlainAuth("", cfg.Contacts.SMTPUser, cfg.Contacts.SMTPPass, host)
	}

	return smtp.SendMail(addr, auth, from, []string{to}, []byte(body))
}

// sendEscalationSlack posts an escalation notification to a Slack webhook.
func sendEscalationSlack(cfg *config.EscalationConfig, beadID, severity, description string) error {
	severityEmoji := map[string]string{
		"critical": "🔴",
		"high":     "🟠",
		"medium":   "🟡",
	}
	emoji := severityEmoji[severity]
	if emoji == "" {
		emoji = "⚪"
	}

	payload := map[string]string{
		"text": fmt.Sprintf("%s *[%s] Escalation %s*\n%s\n_Acknowledge: `gt escalate ack %s`_",
			emoji, strings.ToUpper(severity), beadID, description, beadID),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling slack payload: %w", err)
	}

	resp, err := http.Post(cfg.Contacts.SlackWebhook, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("posting to slack: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("slack webhook returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// sendEscalationSMS posts an escalation notification via SMS webhook (e.g. Twilio).
func sendEscalationSMS(cfg *config.EscalationConfig, beadID, severity, description string) error {
	payload := map[string]string{
		"to":   cfg.Contacts.HumanSMS,
		"body": fmt.Sprintf("[Gas Town %s] %s (bead: %s)", strings.ToUpper(severity), description, beadID),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling sms payload: %w", err)
	}

	resp, err := http.Post(cfg.Contacts.SMSWebhook, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("posting to sms webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sms webhook returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// writeEscalationLog appends an escalation entry to the log file.
func writeEscalationLog(townRoot, beadID, severity, description string) error {
	logDir := fmt.Sprintf("%s/logs", townRoot)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	logPath := fmt.Sprintf("%s/escalations.log", logDir)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	defer f.Close()

	entry := fmt.Sprintf("%s [%s] %s: %s\n", time.Now().Format(time.RFC3339), strings.ToUpper(severity), beadID, description)
	_, err = f.WriteString(entry)
	return err
}

func formatEscalationMailBody(beadID, severity, reason, from, related string) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Escalation ID: %s", beadID))
	lines = append(lines, fmt.Sprintf("Severity: %s", severity))
	lines = append(lines, fmt.Sprintf("From: %s", from))
	if reason != "" {
		lines = append(lines, "")
		lines = append(lines, "Reason:")
		lines = append(lines, reason)
	}
	if related != "" {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("Related: %s", related))
	}
	lines = append(lines, "")
	lines = append(lines, "---")
	lines = append(lines, "To acknowledge: gt escalate ack "+beadID)
	lines = append(lines, "To close: gt escalate close "+beadID+" --reason \"resolution\"")
	return strings.Join(lines, "\n")
}

func severityEmoji(severity string) string {
	switch severity {
	case config.SeverityCritical:
		return "🚨"
	case config.SeverityHigh:
		return "⚠️"
	case config.SeverityMedium:
		return "📢"
	case config.SeverityLow:
		return "ℹ️"
	default:
		return "📋"
	}
}

func formatRelativeTime(timestamp string) string {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return timestamp
	}

	duration := time.Since(t)
	if duration < time.Minute {
		return "just now"
	}
	if duration < time.Hour {
		mins := int(duration.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	}
	if duration < 24*time.Hour {
		hours := int(duration.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	}
	days := int(duration.Hours() / 24)
	if days == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", days)
}

// detectSender is defined in mail_send.go - we reuse it here
// If it's not accessible, we fall back to environment variables
func detectSenderFallback() string {
	// Try BD_ACTOR first (most common in agent context)
	if actor := os.Getenv("BD_ACTOR"); actor != "" {
		return actor
	}
	// Try GT_ROLE
	if role := os.Getenv("GT_ROLE"); role != "" {
		return role
	}
	return ""
}
