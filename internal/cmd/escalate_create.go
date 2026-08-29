package cmd

import (
	"encoding/json"
	"fmt"
	"io"
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

type escalateCreateRequest struct {
	opts             escalateOptions
	description      string
	severity         string
	townRoot         string
	escalationConfig *config.EscalationConfig
	agentID          string
}

func runEscalate(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	req, err := prepareEscalateCreate(cmd, args)
	if err != nil {
		return err
	}
	if req.opts.dryRun {
		return printEscalateDryRun(req.opts, req.severity, req.description, req.escalationConfig)
	}
	return completeEscalateCreate(req)
}

func prepareEscalateCreate(cmd *cobra.Command, args []string) (*escalateCreateRequest, error) {
	opts, err := beginEscalateCreate(cmd)
	if err != nil {
		return nil, err
	}
	severity := strings.ToLower(opts.severity)
	if !config.IsValidSeverity(severity) {
		return nil, fmt.Errorf("invalid severity '%s': must be critical, high, medium, or low", opts.severity)
	}
	townRoot, escalationConfig, err := loadEscalateContext()
	if err != nil {
		return nil, err
	}
	agentID := detectSender()
	if agentID == "" {
		agentID = "unknown"
	}
	return &escalateCreateRequest{
		opts:             opts,
		description:      strings.Join(args, " "),
		severity:         severity,
		townRoot:         townRoot,
		escalationConfig: escalationConfig,
		agentID:          agentID,
	}, nil
}

func completeEscalateCreate(req *escalateCreateRequest) error {
	issue, fingerprintLabel, err := createEscalateBead(req.townRoot, req.opts, req.severity, req.description, req.agentID)
	if err != nil {
		return err
	}
	if issue == nil {
		return nil
	}
	actions := req.escalationConfig.GetRouteForSeverity(req.severity)
	targets := extractMailTargetsFromActions(actions)
	statuses := deliverEscalateMail(req.townRoot, req.agentID, issue, req.severity, req.description, req.opts, targets)
	statuses = append(statuses, executeExternalActions(actions, req.escalationConfig, issue.ID, req.severity, req.description, req.townRoot)...)
	logEscalateFeed(req.agentID, issue.ID, req.severity, req.description, req.opts.source, actions, targets)
	printEscalateResult(req.opts, issue.ID, req.severity, fingerprintLabel, targets, actions, statuses)
	return nil
}

func beginEscalateCreate(cmd *cobra.Command) (escalateOptions, error) {
	opts := escalateOptionsFromCommand(cmd)
	if opts.stdin {
		if opts.reason != "" {
			return opts, fmt.Errorf("cannot use --stdin with --reason/-r")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return opts, fmt.Errorf("reading stdin: %w", err)
		}
		opts.reason = strings.TrimRight(string(data), "\n")
	}
	return opts, nil
}

func loadEscalateContext() (string, *config.EscalationConfig, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	escalationConfig, err := config.LoadOrCreateEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		return "", nil, fmt.Errorf("loading escalation config: %w", err)
	}
	return townRoot, escalationConfig, nil
}

func printEscalateDryRun(opts escalateOptions, severity, description string, escalationConfig *config.EscalationConfig) error {
	actions := escalationConfig.GetRouteForSeverity(severity)
	targets := extractMailTargetsFromActions(actions)
	fmt.Printf("Would create escalation:\n")
	fmt.Printf("  Severity: %s\n", severity)
	fmt.Printf("  Description: %s\n", description)
	if opts.reason != "" {
		fmt.Printf("  Reason: %s\n", opts.reason)
	}
	if opts.source != "" {
		fmt.Printf("  Source: %s\n", opts.source)
	}
	if opts.fingerprint != "" {
		fmt.Printf("  Fingerprint: %s\n", escalationFingerprintLabel(opts.fingerprint))
	}
	fmt.Printf("  Actions: %s\n", strings.Join(actions, ", "))
	fmt.Printf("  Mail targets: %s\n", strings.Join(targets, ", "))
	return nil
}

func createEscalateBead(townRoot string, opts escalateOptions, severity, description, agentID string) (*beads.Issue, string, error) {
	bd := beads.New(beads.ResolveBeadsDir(townRoot))
	fingerprintLabel := escalationFingerprintLabel(opts.fingerprint)
	if fingerprintLabel != "" {
		matches, err := bd.ListEscalationsByFingerprint(fingerprintLabel)
		if err != nil {
			return nil, "", fmt.Errorf("checking escalation fingerprint: %w", err)
		}
		if len(matches) > 0 {
			printEscalateDuplicate(opts, matches[0], fingerprintLabel)
			return nil, fingerprintLabel, nil
		}
	}
	fields := &beads.EscalationFields{
		Severity:    severity,
		Reason:      opts.reason,
		Source:      opts.source,
		EscalatedBy: agentID,
		EscalatedAt: time.Now().Format(time.RFC3339),
		RelatedBead: opts.relatedBead,
		Fingerprint: fingerprintLabel,
	}
	issue, err := bd.CreateEscalationBead(description, fields)
	if err != nil {
		return nil, "", fmt.Errorf("creating escalation bead: %w", err)
	}
	return issue, fingerprintLabel, nil
}

func printEscalateDuplicate(opts escalateOptions, existing *beads.Issue, fingerprintLabel string) {
	if opts.json {
		result := map[string]interface{}{
			"id":          existing.ID,
			"status":      "duplicate_suppressed",
			"fingerprint": fingerprintLabel,
		}
		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Printf("%s Duplicate escalation suppressed: %s\n", style.Bold.Render("✓"), existing.ID)
	fmt.Printf("  Fingerprint: %s\n", fingerprintLabel)
}

func deliverEscalateMail(townRoot, agentID string, issue *beads.Issue, severity, description string, opts escalateOptions, targets []string) []deliveryStatus {
	router := mail.NewRouter(townRoot)
	defer router.WaitPendingNotifications()
	statuses := []deliveryStatus{{Channel: "bead", Created: true, Severity: severity}}
	for _, target := range targets {
		statuses = append(statuses, sendEscalateMail(router, townRoot, agentID, issue, severity, description, opts, target))
	}
	return statuses
}

func sendEscalateMail(router *mail.Router, townRoot, agentID string, issue *beads.Issue, severity, description string, opts escalateOptions, target string) deliveryStatus {
	status := deliveryStatus{Target: target, Channel: "mail", Severity: severity, NotificationRoute: "mail+nudge"}
	msg := &mail.Message{
		From:    agentID,
		To:      target,
		Subject: fmt.Sprintf("[%s] %s", strings.ToUpper(severity), description),
		Body:    formatEscalationMailBody(issue.ID, severity, opts.reason, agentID, opts.relatedBead),
		Type:    mail.TypeEscalation,
		MessageConversation: mail.MessageConversation{
			ThreadID: issue.ID,
		},
		Priority: escalateMailPriority(severity),
	}
	if err := router.Send(msg); err != nil {
		status.Error = err.Error()
		style.PrintWarning("failed to send to %s: %v", target, err)
		return status
	}
	status.Persisted = true
	status.RuntimeNotified = true
	annotateEscalateMail(townRoot, issue.ID, severity, target, msg.Subject, &status)
	return status
}

func escalateMailPriority(severity string) mail.Priority {
	switch severity {
	case config.SeverityCritical:
		return mail.PriorityUrgent
	case config.SeverityHigh:
		return mail.PriorityHigh
	case config.SeverityMedium:
		return mail.PriorityNormal
	default:
		return mail.PriorityLow
	}
}

func annotateEscalateMail(townRoot, issueID, severity, target, subject string, status *deliveryStatus) {
	mailBeads := beads.New(beads.ResolveBeadsDir(townRoot))
	mailIssue, err := mailBeads.FindLatestIssueByTitleAndAssignee(subject, mail.AddressToIdentity(target))
	if err != nil {
		status.Warning = fmt.Sprintf("annotation lookup failed: %v", err)
		style.PrintWarning("failed to annotate escalation mail for %s: %v", target, err)
		return
	}
	addLabels := []string{
		fmt.Sprintf("severity:%s", severity),
		fmt.Sprintf("escalation:%s", issueID),
	}
	if err := mailBeads.Update(mailIssue.ID, beads.UpdateOptions{AddLabels: addLabels}); err != nil {
		status.Warning = fmt.Sprintf("annotation update failed: %v", err)
		style.PrintWarning("failed to annotate escalation mail labels for %s: %v", target, err)
		return
	}
	status.Annotated = true
}

func logEscalateFeed(agentID, issueID, severity, description, source string, actions, targets []string) {
	payload := events.EscalationPayload(issueID, agentID, strings.Join(targets, ","), description)
	payload["severity"] = severity
	payload["actions"] = strings.Join(actions, ",")
	if source != "" {
		payload["source"] = source
	}
	_ = events.LogFeed(events.TypeEscalationSent, agentID, payload)
}

func printEscalateResult(opts escalateOptions, issueID, severity, fingerprintLabel string, targets, actions []string, statuses []deliveryStatus) {
	if opts.json {
		printEscalateJSON(opts, issueID, severity, fingerprintLabel, targets, actions, statuses)
		return
	}
	fmt.Printf("%s Escalation created: %s\n", severityEmoji(severity), issueID)
	fmt.Printf("  Severity: %s\n", severity)
	if opts.source != "" {
		fmt.Printf("  Source: %s\n", opts.source)
	}
	if fingerprintLabel != "" {
		fmt.Printf("  Fingerprint: %s\n", fingerprintLabel)
	}
	fmt.Printf("  Routed to: %s\n", strings.Join(targets, ", "))
	for _, status := range statuses {
		if status.Error != "" {
			fmt.Printf("  Delivery issue [%s:%s]: %s\n", status.Channel, status.Target, status.Error)
		}
	}
}

func printEscalateJSON(opts escalateOptions, issueID, severity, fingerprintLabel string, targets, actions []string, statuses []deliveryStatus) {
	hasFailure := false
	for _, status := range statuses {
		if status.Error != "" {
			hasFailure = true
			break
		}
	}
	result := map[string]interface{}{
		"id":       issueID,
		"severity": severity,
		"actions":  actions,
		"targets":  targets,
		"delivery": statuses,
		"status":   map[bool]string{true: "partial_failure", false: "ok"}[hasFailure],
	}
	if opts.source != "" {
		result["source"] = opts.source
	}
	if fingerprintLabel != "" {
		result["fingerprint"] = fingerprintLabel
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(out))
}
