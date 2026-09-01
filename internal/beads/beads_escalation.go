// Package beads provides escalation bead management.
package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EscalationFields holds structured fields for escalation beads.
// These are stored as "key: value" lines in the description.
type EscalationFields struct {
	Severity          string // critical, high, medium, low
	Reason            string // Why this was escalated
	Source            string // Source identifier (e.g., plugin:rebuild-gt, patrol:deacon)
	EscalatedBy       string // Agent address that escalated (e.g., "gastown/Toast")
	EscalatedAt       string // ISO 8601 timestamp
	AckedBy           string // Agent that acknowledged (empty if not acked)
	AckedAt           string // When acknowledged (empty if not acked)
	ClosedBy          string // Agent that closed (empty if not closed)
	ClosedReason      string // Resolution reason (empty if not closed)
	RelatedBead       string // Optional: related bead ID (task, bug, etc.)
	OriginalSeverity  string // Original severity before any re-escalation
	ReescalationCount int    // Number of times this has been re-escalated
	LastReescalatedAt string // When last re-escalated (empty if never)
	LastReescalatedBy string // Who last re-escalated (empty if never)
	Fingerprint       string // Stable duplicate-suppression label
}

// FormatEscalationDescription creates a description string from escalation fields.
func FormatEscalationDescription(title string, fields *EscalationFields) string {
	if fields == nil {
		return title
	}

	lines := []string{title, "",
		formatEscalationField("severity", fields.Severity),
		formatEscalationField("reason", fields.Reason),
		formatNullableEscalationField("source", fields.Source),
		formatEscalationField("escalated_by", fields.EscalatedBy),
		formatEscalationField("escalated_at", fields.EscalatedAt),
		formatNullableEscalationField("acked_by", fields.AckedBy),
		formatNullableEscalationField("acked_at", fields.AckedAt),
		formatNullableEscalationField("closed_by", fields.ClosedBy),
		formatNullableEscalationField("closed_reason", fields.ClosedReason),
		formatNullableEscalationField("related_bead", fields.RelatedBead),
		formatNullableEscalationField("original_severity", fields.OriginalSeverity),
		fmt.Sprintf("reescalation_count: %d", fields.ReescalationCount),
		formatNullableEscalationField("last_reescalated_at", fields.LastReescalatedAt),
		formatNullableEscalationField("last_reescalated_by", fields.LastReescalatedBy),
		formatNullableEscalationField("fingerprint", fields.Fingerprint),
	}
	return strings.Join(lines, "\n")
}

func formatEscalationField(key, value string) string {
	return fmt.Sprintf("%s: %s", key, value)
}

func formatNullableEscalationField(key, value string) string {
	if value == "" {
		value = "null"
	}
	return formatEscalationField(key, value)
}

// ParseEscalationFields extracts escalation fields from an issue's description.
func ParseEscalationFields(description string) *EscalationFields {
	fields := &EscalationFields{}
	for _, line := range strings.Split(description, "\n") {
		setEscalationField(fields, line)
	}
	return fields
}

func setEscalationField(fields *EscalationFields, line string) {
	key, value, ok := escalationFieldLine(line)
	if !ok {
		return
	}
	if key == "reescalation_count" {
		if count, err := strconv.Atoi(value); err == nil {
			fields.ReescalationCount = count
		}
		return
	}
	if destination, ok := escalationStringFields(fields)[key]; ok {
		*destination = value
	}
}

func escalationFieldLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	colonIdx := strings.Index(line, ":")
	if line == "" || colonIdx == -1 {
		return "", "", false
	}
	key := strings.ToLower(strings.TrimSpace(line[:colonIdx]))
	value := strings.TrimSpace(line[colonIdx+1:])
	if value == "null" {
		value = ""
	}
	return key, value, true
}

func escalationStringFields(fields *EscalationFields) map[string]*string {
	return map[string]*string{
		"severity":            &fields.Severity,
		"reason":              &fields.Reason,
		"source":              &fields.Source,
		"escalated_by":        &fields.EscalatedBy,
		"escalated_at":        &fields.EscalatedAt,
		"acked_by":            &fields.AckedBy,
		"acked_at":            &fields.AckedAt,
		"closed_by":           &fields.ClosedBy,
		"closed_reason":       &fields.ClosedReason,
		"related_bead":        &fields.RelatedBead,
		"original_severity":   &fields.OriginalSeverity,
		"last_reescalated_at": &fields.LastReescalatedAt,
		"last_reescalated_by": &fields.LastReescalatedBy,
		"fingerprint":         &fields.Fingerprint,
	}
}

// CreateEscalationBead creates an escalation bead for tracking escalations.
// The created_by field is populated from BD_ACTOR env var for provenance tracking.
func (b *Beads) CreateEscalationBead(title string, fields *EscalationFields) (*Issue, error) {
	// Guard against flag-like titles (gt-e0kx5: --help garbage beads)
	if IsFlagLikeTitle(title) {
		return nil, fmt.Errorf("refusing to create escalation bead: %w (got %q)", ErrFlagTitle, title)
	}

	description := FormatEscalationDescription(title, fields)

	// Pass description via stdin (--body-file=-) instead of --description=...
	// to avoid embedding newlines in a flag value. bd 1.0.3+ rejects newline-
	// containing flag values, which broke `gt escalate` for any escalation
	// with structured YAML metadata in the description.
	args := []string{"create", "--json",
		"--title=" + title,
		"--body-file=-",
		"--type=task",
		"--ephemeral",
		"--wisp-type=escalation",
		"--labels=gt:escalation",
	}

	// Add severity as a label for easy filtering
	if fields != nil && fields.Severity != "" {
		args = append(args, fmt.Sprintf("--labels=severity:%s", fields.Severity))
	}
	if fields != nil && fields.Fingerprint != "" {
		args = append(args, "--labels="+fields.Fingerprint)
	}

	// Default actor from BD_ACTOR env var for provenance tracking
	// Uses getActor() to respect isolated mode (tests)
	if actor := b.getActor(); actor != "" {
		args = append(args, "--actor="+actor)
	}

	out, err := b.runWithStdin([]byte(description), args...)
	if err != nil {
		return nil, err
	}

	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parsing bd create output: %w", err)
	}

	return &issue, nil
}

// AckEscalation acknowledges an escalation bead.
// Sets acked_by and acked_at fields, adds "acked" label.
func (b *Beads) AckEscalation(id, ackedBy string) error {
	target := b.forIssueID(id)
	// First get current issue to preserve other fields
	issue, err := target.Show(id)
	if err != nil {
		return err
	}

	// Verify it's an escalation
	if !HasLabel(issue, "gt:escalation") {
		return fmt.Errorf("issue %s is not an escalation bead (missing gt:escalation label)", id)
	}

	// Parse existing fields
	fields := ParseEscalationFields(issue.Description)
	fields.AckedBy = ackedBy
	fields.AckedAt = time.Now().Format(time.RFC3339)

	// Format new description
	description := FormatEscalationDescription(issue.Title, fields)

	return target.Update(id, UpdateOptions{
		Description: &description,
		AddLabels:   []string{"acked"},
	})
}

// CloseEscalation closes an escalation bead with a resolution reason.
// Sets closed_by and closed_reason fields, closes the issue.
func (b *Beads) CloseEscalation(id, closedBy, reason string) error {
	target := b.forIssueID(id)
	// First get current issue to preserve other fields
	issue, err := target.Show(id)
	if err != nil {
		return err
	}

	// Verify it's an escalation
	if !HasLabel(issue, "gt:escalation") {
		return fmt.Errorf("issue %s is not an escalation bead (missing gt:escalation label)", id)
	}

	// Parse existing fields
	fields := ParseEscalationFields(issue.Description)
	fields.ClosedBy = closedBy
	fields.ClosedReason = reason

	// Format new description
	description := FormatEscalationDescription(issue.Title, fields)

	// Update description first
	if err := target.Update(id, UpdateOptions{
		Description: &description,
		AddLabels:   []string{"resolved"},
	}); err != nil {
		return err
	}

	// Close the issue
	_, err = target.run("close", id, "--reason="+reason)
	return err
}

// GetEscalationBead retrieves an escalation bead by ID.
// Returns nil if not found.
func (b *Beads) GetEscalationBead(id string) (*Issue, *EscalationFields, error) {
	issue, err := b.forIssueID(id).Show(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	if !HasLabel(issue, "gt:escalation") {
		return nil, nil, fmt.Errorf("issue %s is not an escalation bead (missing gt:escalation label)", id)
	}

	fields := ParseEscalationFields(issue.Description)
	return issue, fields, nil
}

// ListEscalations returns all open escalation beads.
func (b *Beads) ListEscalations() ([]*Issue, error) {
	out, err := b.run("list", "--label=gt:escalation", "--status=open", "--json")
	if err != nil {
		return nil, err
	}

	var issues []*Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}

	return filterEscalationRecords(issues), nil
}

// ListEscalationsByFingerprint returns open escalation beads matching a stable fingerprint label.
func (b *Beads) ListEscalationsByFingerprint(fingerprintLabel string) ([]*Issue, error) {
	if fingerprintLabel == "" {
		return nil, nil
	}
	out, err := b.run("list",
		"--label=gt:escalation",
		"--label="+fingerprintLabel,
		"--status=open",
		"--json",
	)
	if err != nil {
		return nil, err
	}

	var issues []*Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}

	return filterEscalationRecords(issues), nil
}

// ListEscalationsBySeverity returns open escalation beads filtered by severity.
func (b *Beads) ListEscalationsBySeverity(severity string) ([]*Issue, error) {
	out, err := b.run("list",
		"--label=gt:escalation",
		"--label=severity:"+severity,
		"--status=open",
		"--json",
	)
	if err != nil {
		return nil, err
	}

	var issues []*Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}

	return filterEscalationRecords(issues), nil
}

func filterEscalationRecords(issues []*Issue) []*Issue {
	filtered := issues[:0]
	for _, issue := range issues {
		if HasLabel(issue, "gt:message") {
			continue
		}
		filtered = append(filtered, issue)
	}
	return filtered
}

// ListStaleEscalations returns escalations older than the given threshold.
// threshold is a duration string like "1h" or "30m".
func (b *Beads) ListStaleEscalations(threshold time.Duration) ([]*Issue, error) {
	// Get all open escalations
	escalations, err := b.ListEscalations()
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-threshold)
	var stale []*Issue

	for _, issue := range escalations {
		// Skip acknowledged escalations
		if HasLabel(issue, "acked") {
			continue
		}

		// Check if older than threshold
		createdAt, err := time.Parse(time.RFC3339, issue.CreatedAt)
		if err != nil {
			continue // Skip if can't parse
		}

		if createdAt.Before(cutoff) {
			stale = append(stale, issue)
		}
	}

	return stale, nil
}

// ReescalationResult holds the result of a reescalation operation.
type ReescalationResult struct {
	ID              string
	Title           string
	OldSeverity     string
	NewSeverity     string
	ReescalationNum int
	Skipped         bool
	SkipReason      string
}

// ReescalateEscalation bumps the severity of an escalation and updates tracking fields.
// Returns the new severity if successful, or an error.
// reescalatedBy should be the identity of the agent/process doing the reescalation.
// maxReescalations limits how many times an escalation can be bumped (0 = unlimited).
func (b *Beads) ReescalateEscalation(id, reescalatedBy string, maxReescalations int) (*ReescalationResult, error) {
	// Get the escalation
	issue, fields, err := b.GetEscalationBead(id)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("escalation not found: %s", id)
	}

	result := &ReescalationResult{
		ID:          id,
		Title:       issue.Title,
		OldSeverity: fields.Severity,
	}

	// Check if already at max reescalations
	if maxReescalations > 0 && fields.ReescalationCount >= maxReescalations {
		result.Skipped = true
		result.SkipReason = fmt.Sprintf("already at max reescalations (%d)", maxReescalations)
		return result, nil
	}

	// Check if already at critical (can't bump further)
	if fields.Severity == "critical" {
		result.Skipped = true
		result.SkipReason = "already at critical severity"
		result.NewSeverity = "critical"
		return result, nil
	}

	// Save original severity on first reescalation
	if fields.OriginalSeverity == "" {
		fields.OriginalSeverity = fields.Severity
	}

	// Bump severity
	newSeverity := bumpSeverity(fields.Severity)
	fields.Severity = newSeverity
	fields.ReescalationCount++
	fields.LastReescalatedAt = time.Now().Format(time.RFC3339)
	fields.LastReescalatedBy = reescalatedBy

	result.NewSeverity = newSeverity
	result.ReescalationNum = fields.ReescalationCount

	// Format new description
	description := FormatEscalationDescription(issue.Title, fields)

	// Update the bead with new description and severity label
	if err := b.forIssueID(id).Update(id, UpdateOptions{
		Description:  &description,
		AddLabels:    []string{"reescalated", "severity:" + newSeverity},
		RemoveLabels: []string{"severity:" + result.OldSeverity},
	}); err != nil {
		return nil, fmt.Errorf("updating escalation: %w", err)
	}

	return result, nil
}

// bumpSeverity returns the next higher severity level.
// low -> medium -> high -> critical
func bumpSeverity(severity string) string {
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
