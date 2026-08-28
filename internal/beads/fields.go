// Package beads provides field parsing utilities for structured issue descriptions.
package beads

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func trimBlankLines(lines []string) []string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[start:end]
}

// Note: AgentFields, ParseAgentFields, FormatAgentDescription, and CreateAgentBead are in beads.go

// AttachmentFields holds the attachment info for pinned beads.
// These fields track which molecule is attached to a handoff/pinned bead.
type AttachmentFields struct {
	AttachedMolecule string   // Root issue ID of the attached molecule
	AttachedFormula  string   // Formula name (e.g., "mol-polecat-work") for inline step display
	AttachedAt       string   // ISO 8601 timestamp when attached
	AttachedArgs     string   // Natural language args passed via gt sling --args (no-tmux mode)
	AttachedVars     []string // Formula variables passed via gt sling --var
	DispatchedBy     string   // Agent ID that dispatched this work (for completion notification)
	NoMerge          bool     // If true, gt done skips merge queue (for upstream PRs/human review)
	ReviewOnly       bool     // If true, assignee must evaluate and report back — no merge/commit/push
	Mode             string   // Execution mode: "" (normal) or "ralph" (Ralph Wiggum loop)
	ConvoyID         string   // Convoy bead ID tracking this issue (e.g., "hq-cv-abc")
	MergeStrategy    string   // Convoy merge strategy: "direct", "mr", "local", or "" (default = mr)
	ConvoyOwned      bool     // If true, convoy has gt:owned label (caller-managed lifecycle)
	FormulaVars      string   // Newline-separated key=value pairs for formula template substitution
}

// ParseAttachmentFields extracts attachment fields from an issue's description.
// Fields are expected as "key: value" lines. Returns nil if no attachment fields found.
func ParseAttachmentFields(issue *Issue) *AttachmentFields {
	if issue == nil || issue.Description == "" {
		return nil
	}

	fields := &AttachmentFields{}
	hasFields := false
	var formulaVars []string

	for _, line := range strings.Split(issue.Description, "\n") {
		key, value, ok := attachmentFieldLine(line)
		if !ok || value == "" {
			continue
		}
		found, parsedFormulaVars := setAttachmentField(fields, key, value)
		hasFields = hasFields || found
		formulaVars = append(formulaVars, parsedFormulaVars...)
	}
	if len(formulaVars) > 0 {
		fields.FormulaVars = strings.Join(formulaVars, "\n")
	}

	if !hasFields {
		return nil
	}
	return fields
}

// FormatAttachmentFields formats AttachmentFields as a string suitable for an issue description.
// Only non-empty fields are included.
func FormatAttachmentFields(fields *AttachmentFields) string {
	if fields == nil {
		return ""
	}

	var lines []string
	for _, field := range formattedAttachmentTextFields(fields) {
		if field.value != "" {
			lines = append(lines, field.key+": "+field.value)
		}
	}
	if fields.NoMerge {
		lines = append(lines, "no_merge: true")
	}
	if fields.ReviewOnly {
		lines = append(lines, "review_only: true")
	}
	if fields.ConvoyOwned {
		lines = append(lines, "convoy_owned: true")
	}
	if fields.FormulaVars != "" {
		if formatted := formatFormulaVars(fields.FormulaVars); formatted != "" {
			lines = append(lines, "formula_vars: "+formatted)
		}
	}

	return strings.Join(lines, "\n")
}

type attachmentTextField struct{ key, value string }

func formattedAttachmentTextFields(fields *AttachmentFields) []attachmentTextField {
	return []attachmentTextField{
		{"attached_molecule", fields.AttachedMolecule},
		{"attached_formula", fields.AttachedFormula},
		{"attached_at", fields.AttachedAt},
		{"attached_args", fields.AttachedArgs},
		{"attached_vars", formatAttachedVars(fields.AttachedVars)},
		{"dispatched_by", fields.DispatchedBy},
		{"mode", fields.Mode},
		{"convoy_id", fields.ConvoyID},
		{"merge_strategy", fields.MergeStrategy},
	}
}

func attachmentFieldLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	colonIdx := strings.Index(line, ":")
	if line == "" || colonIdx == -1 {
		return "", "", false
	}
	return canonicalAttachmentField(strings.ToLower(strings.TrimSpace(line[:colonIdx]))), strings.TrimSpace(line[colonIdx+1:]), true
}

func canonicalAttachmentField(key string) string {
	return map[string]string{
		"attached_molecule": "attached_molecule", "attached-molecule": "attached_molecule", "attachedmolecule": "attached_molecule",
		"attached_formula": "attached_formula", "attached-formula": "attached_formula", "attachedformula": "attached_formula",
		"attached_at": "attached_at", "attached-at": "attached_at", "attachedat": "attached_at",
		"attached_args": "attached_args", "attached-args": "attached_args", "attachedargs": "attached_args",
		"attached_vars": "attached_vars", "attached-vars": "attached_vars", "attachedvars": "attached_vars",
		"dispatched_by": "dispatched_by", "dispatched-by": "dispatched_by", "dispatchedby": "dispatched_by",
		"no_merge": "no_merge", "no-merge": "no_merge", "nomerge": "no_merge",
		"review_only": "review_only", "review-only": "review_only", "reviewonly": "review_only",
		"mode":      "mode",
		"convoy_id": "convoy_id", "convoy-id": "convoy_id", "convoyid": "convoy_id", "convoy": "convoy_id",
		"merge_strategy": "merge_strategy", "merge-strategy": "merge_strategy", "mergestrategy": "merge_strategy",
		"convoy_owned": "convoy_owned", "convoy-owned": "convoy_owned", "convoyowned": "convoy_owned",
		"formula_vars": "formula_vars", "formula-vars": "formula_vars", "formulavars": "formula_vars",
	}[key]
}

func setAttachmentField(fields *AttachmentFields, key, value string) (bool, []string) {
	if destination, ok := attachmentStringFields(fields)[key]; ok {
		*destination = value
		return true, nil
	}
	if key == "attached_vars" {
		fields.AttachedVars = parseAttachedVars(value)
		return true, nil
	}
	if key == "formula_vars" {
		return true, splitFormulaVars(parseFormulaVars(value))
	}
	if destination, ok := attachmentBooleanFields(fields)[key]; ok {
		*destination = strings.EqualFold(value, "true")
		return true, nil
	}
	return false, nil
}

func attachmentStringFields(fields *AttachmentFields) map[string]*string {
	return map[string]*string{
		"attached_molecule": &fields.AttachedMolecule, "attached_formula": &fields.AttachedFormula,
		"attached_at": &fields.AttachedAt, "attached_args": &fields.AttachedArgs,
		"dispatched_by": &fields.DispatchedBy, "mode": &fields.Mode,
		"convoy_id": &fields.ConvoyID, "merge_strategy": &fields.MergeStrategy,
	}
}

func attachmentBooleanFields(fields *AttachmentFields) map[string]*bool {
	return map[string]*bool{"no_merge": &fields.NoMerge, "review_only": &fields.ReviewOnly, "convoy_owned": &fields.ConvoyOwned}
}

// SetAttachmentFields updates an issue's description with the given attachment fields.
// Existing attachment field lines are replaced; other content is preserved.
// Returns the new description string.
func SetAttachmentFields(issue *Issue, fields *AttachmentFields) string {
	// Known attachment field keys (lowercase)
	attachmentKeys := map[string]bool{
		"attached_molecule": true,
		"attached-molecule": true,
		"attachedmolecule":  true,
		"attached_formula":  true,
		"attached-formula":  true,
		"attachedformula":   true,
		"attached_at":       true,
		"attached-at":       true,
		"attachedat":        true,
		"attached_args":     true,
		"attached-args":     true,
		"attachedargs":      true,
		"attached_vars":     true,
		"attached-vars":     true,
		"attachedvars":      true,
		"dispatched_by":     true,
		"dispatched-by":     true,
		"dispatchedby":      true,
		"no_merge":          true,
		"no-merge":          true,
		"nomerge":           true,
		"review_only":       true,
		"review-only":       true,
		"reviewonly":        true,
		"mode":              true,
		"convoy_id":         true,
		"convoy-id":         true,
		"convoyid":          true,
		"convoy":            true,
		"merge_strategy":    true,
		"merge-strategy":    true,
		"mergestrategy":     true,
		"convoy_owned":      true,
		"convoy-owned":      true,
		"convoyowned":       true,
		"formula_vars":      true,
		"formula-vars":      true,
		"formulavars":       true,
	}

	// Collect non-attachment lines from existing description
	var otherLines []string
	if issue != nil && issue.Description != "" {
		for _, line := range strings.Split(issue.Description, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				// Preserve blank lines in content
				otherLines = append(otherLines, line)
				continue
			}

			// Check if this is an attachment field line
			colonIdx := strings.Index(trimmed, ":")
			if colonIdx == -1 {
				otherLines = append(otherLines, line)
				continue
			}

			key := strings.ToLower(strings.TrimSpace(trimmed[:colonIdx]))
			if !attachmentKeys[key] {
				otherLines = append(otherLines, line)
			}
			// Skip attachment field lines - they'll be replaced
		}
	}

	// Build new description: attachment fields first, then other content
	formatted := FormatAttachmentFields(fields)

	otherLines = trimBlankLines(otherLines)

	if formatted == "" {
		return strings.Join(otherLines, "\n")
	}
	if len(otherLines) == 0 {
		return formatted
	}

	return formatted + "\n\n" + strings.Join(otherLines, "\n")
}

// ConvoyFields holds the structured fields for a convoy bead.
// These fields are stored as key: value lines in the issue description.
type ConvoyFields struct {
	Owner                string // Convoy owner address (e.g., "mayor/")
	Notify               string // Additional notification address
	Molecule             string // Associated molecule/swarm ID
	Merge                string // Merge strategy
	BaseBranch           string // Target branch for polecats (e.g., "feat/extraction-review")
	Watchers             string // Comma-separated mail notification addresses (added via gt convoy watch)
	NudgeWatchers        string // Comma-separated nudge notification addresses (added via gt convoy watch --nudge)
	CompletionNotifiedAt string // RFC3339 timestamp when completion notifications were claimed/sent
}

type optionalDescriptionField struct{ key, value string }

// ParseConvoyFields extracts convoy fields from an issue's description.
// Returns nil if no convoy fields found.
func ParseConvoyFields(issue *Issue) *ConvoyFields {
	if issue == nil || issue.Description == "" {
		return nil
	}

	fields := &ConvoyFields{}
	for _, line := range strings.Split(issue.Description, "\n") {
		setConvoyField(fields, line)
	}
	if !hasConvoyFields(fields) {
		return nil
	}
	return fields
}

func setConvoyField(fields *ConvoyFields, line string) {
	key, value, ok := agentDescriptionFieldLine(line)
	if !ok || value == "" {
		return
	}
	if destination, ok := convoyFieldDestinations(fields)[convoyFieldKey(key)]; ok {
		*destination = value
	}
}

func convoyFieldKey(key string) string {
	key = strings.ReplaceAll(key, "-", "")
	key = strings.ReplaceAll(key, "_", "")
	return strings.ReplaceAll(key, " ", "")
}

func convoyFieldDestinations(fields *ConvoyFields) map[string]*string {
	return map[string]*string{
		"owner": &fields.Owner, "notify": &fields.Notify, "molecule": &fields.Molecule,
		"merge": &fields.Merge, "basebranch": &fields.BaseBranch, "watchers": &fields.Watchers,
		"nudgewatchers": &fields.NudgeWatchers, "completionnotifiedat": &fields.CompletionNotifiedAt,
	}
}

func hasConvoyFields(fields *ConvoyFields) bool {
	return fields.Owner != "" || fields.Notify != "" || fields.Molecule != "" || fields.Merge != "" ||
		fields.BaseBranch != "" || fields.Watchers != "" || fields.NudgeWatchers != "" || fields.CompletionNotifiedAt != ""
}

// NotificationAddresses returns deduplicated mail notification addresses from convoy fields.
// Includes Owner, Notify, and all Watchers addresses.
func (f *ConvoyFields) NotificationAddresses() []string {
	if f == nil {
		return nil
	}
	seen := make(map[string]bool)
	var addrs []string
	for _, addr := range []string{f.Owner, f.Notify} {
		if addr != "" && !seen[addr] {
			addrs = append(addrs, addr)
			seen[addr] = true
		}
	}
	for _, addr := range splitWatchers(f.Watchers) {
		if addr != "" && !seen[addr] {
			addrs = append(addrs, addr)
			seen[addr] = true
		}
	}
	return addrs
}

// NudgeNotificationAddresses returns deduplicated nudge addresses from convoy fields.
func (f *ConvoyFields) NudgeNotificationAddresses() []string {
	if f == nil {
		return nil
	}
	return notificationAddresses(*f.watcherField(true))
}

func (f *ConvoyFields) watcherField(nudge bool) *string {
	if nudge {
		return &f.NudgeWatchers
	}
	return &f.Watchers
}

func notificationAddresses(watchers string) []string {
	seen := make(map[string]bool)
	var addrs []string
	for _, addr := range splitWatchers(watchers) {
		if addr != "" && !seen[addr] {
			addrs = append(addrs, addr)
			seen[addr] = true
		}
	}
	return addrs
}

// AddWatcher adds a mail watcher address to the comma-separated Watchers field.
// Returns true if the address was added (false if already present).
func (f *ConvoyFields) AddWatcher(addr string) bool {
	return addWatcher(f.watcherField(false), addr)
}

// AddNudgeWatcher adds a nudge watcher address to the comma-separated NudgeWatchers field.
// Returns true if the address was added (false if already present).
func (f *ConvoyFields) AddNudgeWatcher(addr string) bool {
	return addWatcher(f.watcherField(true), addr)
}

func addWatcher(watchers *string, addr string) bool {
	existing := splitWatchers(*watchers)
	for _, w := range existing {
		if w == addr {
			return false
		}
	}
	existing = append(existing, addr)
	*watchers = strings.Join(existing, ",")
	return true
}

// RemoveWatcher removes a mail watcher address. Returns true if it was present.
func (f *ConvoyFields) RemoveWatcher(addr string) bool {
	return removeWatcher(f.watcherField(false), addr)
}

// RemoveNudgeWatcher removes a nudge watcher address. Returns true if it was present.
func (f *ConvoyFields) RemoveNudgeWatcher(addr string) bool {
	return removeWatcher(f.watcherField(true), addr)
}

func removeWatcher(watchers *string, addr string) bool {
	existing := splitWatchers(*watchers)
	var remaining []string
	found := false
	for _, w := range existing {
		if w == addr {
			found = true
		} else {
			remaining = append(remaining, w)
		}
	}
	if found {
		*watchers = strings.Join(remaining, ",")
	}
	return found
}

// splitWatchers splits a comma-separated watcher string into trimmed, non-empty addresses.
func splitWatchers(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// FormatConvoyFields formats ConvoyFields as a string suitable for an issue description.
// Only non-empty fields are included.
func FormatConvoyFields(fields *ConvoyFields) string {
	if fields == nil {
		return ""
	}

	return formatOptionalDescriptionFields([]optionalDescriptionField{
		{"Owner", fields.Owner}, {"Notify", fields.Notify}, {"Merge", fields.Merge}, {"Molecule", fields.Molecule},
		{"base_branch", fields.BaseBranch}, {"Watchers", fields.Watchers}, {"nudge_watchers", fields.NudgeWatchers},
		{"completion_notified_at", fields.CompletionNotifiedAt},
	})
}

func formatOptionalDescriptionFields(fields []optionalDescriptionField) string {
	var lines []string
	for _, field := range fields {
		if field.value != "" {
			lines = append(lines, field.key+": "+field.value)
		}
	}
	return strings.Join(lines, "\n")
}

func formatAttachedVars(vars []string) string {
	if len(vars) == 0 {
		return ""
	}
	encoded, err := json.Marshal(vars)
	if err != nil {
		return strings.Join(vars, ", ")
	}
	return string(encoded)
}

func parseAttachedVars(raw string) []string {
	if raw == "" {
		return nil
	}
	var vars []string
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &vars); err == nil {
			return vars
		}
	}
	return []string{raw}
}

func formatFormulaVars(raw string) string {
	return formatAttachedVars(splitFormulaVars(raw))
}

func parseFormulaVars(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "[") {
		if vars := parseAttachedVars(raw); len(vars) > 0 {
			return strings.Join(vars, "\n")
		}
		return ""
	}
	return strings.Join(splitFormulaVars(raw), "\n")
}

func splitFormulaVars(raw string) []string {
	if raw == "" {
		return nil
	}
	vars := strings.Split(raw, "\n")
	out := vars[:0]
	for _, variable := range vars {
		variable = strings.TrimSpace(variable)
		if variable != "" {
			out = append(out, variable)
		}
	}
	return out
}

// SetConvoyFields updates an issue's description with the given convoy fields.
// Existing convoy field lines are replaced; other content is preserved.
// Returns the new description string.
func SetConvoyFields(issue *Issue, fields *ConvoyFields) string {
	if issue == nil {
		return FormatConvoyFields(fields)
	}

	// Known convoy field keys (lowercase)
	convoyKeys := map[string]bool{
		"owner":                  true,
		"notify":                 true,
		"merge":                  true,
		"molecule":               true,
		"base_branch":            true,
		"base-branch":            true,
		"basebranch":             true,
		"watchers":               true,
		"nudge_watchers":         true,
		"nudge-watchers":         true,
		"nudgewatchers":          true,
		"completion_notified_at": true,
		"completion-notified-at": true,
		"completionnotifiedat":   true,
	}

	// Collect non-convoy lines from existing description
	var otherLines []string
	if issue.Description != "" {
		for _, line := range strings.Split(issue.Description, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				otherLines = append(otherLines, line)
				continue
			}

			colonIdx := strings.Index(trimmed, ":")
			if colonIdx == -1 {
				otherLines = append(otherLines, line)
				continue
			}

			key := strings.ToLower(strings.TrimSpace(trimmed[:colonIdx]))
			if !convoyKeys[key] {
				otherLines = append(otherLines, line)
			}
		}
	}

	// Build new description: other content first, then convoy fields
	formatted := FormatConvoyFields(fields)

	otherLines = trimBlankLines(otherLines)

	if len(otherLines) == 0 {
		return formatted
	}
	if formatted == "" {
		return strings.Join(otherLines, "\n")
	}

	return strings.Join(otherLines, "\n") + "\n" + formatted
}

// MRFields holds the structured fields for a merge-request issue.
// These fields are stored as key: value lines in the issue description.
type MRFields struct {
	Branch      string // Source branch name (e.g., "polecat/Nux/gt-xyz")
	Target      string // Target branch (e.g., "main" or "integration/gt-epic")
	SourceIssue string // The work item being merged (e.g., "gt-xyz")
	Worker      string // Who did the work
	Rig         string // Which rig
	CommitSHA   string // HEAD commit SHA at submission time (GH#3032: dedup key)
	PRURL       string // Recorded pull request URL, if one exists for this MR
	PRNumber    int    // Recorded pull request number, scoped to the target repo
	MergeCommit string // SHA of merge commit (set on close)
	CloseReason string // Reason for closing: merged, rejected, conflict, superseded
	AgentBead   string // Agent bead ID that created this MR (for traceability)
	MRConflictFields
	MRConvoyFields
	MRPreVerificationFields
}

// MRConflictFields records conflict-resolution metadata used for priority scoring.
type MRConflictFields struct {
	RetryCount      int    // Number of conflict-resolution cycles
	LastConflictSHA string // SHA of main when conflict occurred
	ConflictTaskID  string // Link to conflict-resolution task (if any)
}

// MRConvoyFields records convoy metadata used to prevent starvation in priority scoring.
type MRConvoyFields struct {
	ConvoyID        string // Parent convoy ID if part of a convoy
	ConvoyCreatedAt string // Convoy creation time (ISO 8601) for starvation prevention
}

// MRPreVerificationFields records verification completed after rebasing onto the target.
// It lets the Refinery fast-path a merge without rerunning its gates.
type MRPreVerificationFields struct {
	PreVerified     bool   // Polecat ran full gates after rebasing onto target
	PreVerifiedAt   string // ISO 8601 timestamp when verification completed
	PreVerifiedBase string // Target branch SHA at verification time
}

// ParseMRFields extracts structured merge-request fields from an issue's description.
// Fields are expected as "key: value" lines, with optional prose text mixed in.
// Returns nil if no MR fields are found.
func ParseMRFields(issue *Issue) *MRFields {
	if issue == nil || issue.Description == "" {
		return nil
	}

	fields := &MRFields{}
	for _, line := range strings.Split(issue.Description, "\n") {
		setMRField(fields, line)
	}
	if !hasMRFields(fields) {
		return nil
	}
	return fields
}

func setMRField(fields *MRFields, line string) {
	key, value, ok := agentDescriptionFieldLine(line)
	if !ok || value == "" {
		return
	}
	key = convoyFieldKey(key)
	if key == "convoy" {
		key = "convoyid"
	}
	if destination, ok := mrStringFields(fields)[key]; ok {
		*destination = value
		return
	}
	if destination, ok := mrIntFields(fields)[key]; ok {
		if value, err := parseIntField(value); err == nil {
			*destination = value
		}
		return
	}
	if key == "preverified" {
		fields.PreVerified = strings.EqualFold(value, "true")
	}
}

func mrStringFields(fields *MRFields) map[string]*string {
	return map[string]*string{
		"branch": &fields.Branch, "target": &fields.Target, "sourceissue": &fields.SourceIssue, "worker": &fields.Worker, "rig": &fields.Rig, "commitsha": &fields.CommitSHA, "prurl": &fields.PRURL, "mergecommit": &fields.MergeCommit, "closereason": &fields.CloseReason, "agentbead": &fields.AgentBead, "lastconflictsha": &fields.LastConflictSHA, "conflicttaskid": &fields.ConflictTaskID, "convoyid": &fields.ConvoyID, "convoycreatedat": &fields.ConvoyCreatedAt, "preverifiedat": &fields.PreVerifiedAt, "preverifiedbase": &fields.PreVerifiedBase,
	}
}
func mrIntFields(fields *MRFields) map[string]*int {
	return map[string]*int{"prnumber": &fields.PRNumber, "retrycount": &fields.RetryCount}
}
func hasMRFields(fields *MRFields) bool { return FormatMRFields(fields) != "" }

// parseIntField parses an integer from a string, returning 0 on error.
func parseIntField(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// FormatMRFields formats MRFields as a string suitable for an issue description.
// Only non-empty fields are included.
func FormatMRFields(fields *MRFields) string {
	if fields == nil {
		return ""
	}

	lines := formatOptionalDescriptionFields([]optionalDescriptionField{
		{"branch", fields.Branch}, {"target", fields.Target}, {"source_issue", fields.SourceIssue}, {"worker", fields.Worker}, {"rig", fields.Rig}, {"commit_sha", fields.CommitSHA}, {"pr_url", fields.PRURL},
		{"pr_number", optionalPositiveInt(fields.PRNumber)}, {"merge_commit", fields.MergeCommit}, {"close_reason", fields.CloseReason}, {"agent_bead", fields.AgentBead}, {"retry_count", optionalPositiveInt(fields.RetryCount)}, {"last_conflict_sha", fields.LastConflictSHA}, {"conflict_task_id", fields.ConflictTaskID}, {"convoy_id", fields.ConvoyID}, {"convoy_created_at", fields.ConvoyCreatedAt}, {"pre_verified", optionalTrue(fields.PreVerified)}, {"pre_verified_at", fields.PreVerifiedAt}, {"pre_verified_base", fields.PreVerifiedBase},
	})
	return lines
}

func optionalPositiveInt(value int) string {
	if value <= 0 {
		return ""
	}
	return fmt.Sprint(value)
}

func optionalTrue(value bool) string {
	if !value {
		return ""
	}
	return "true"
}

// SetMRFields updates an issue's description with the given MR fields.
// Existing MR field lines are replaced; other content is preserved.
// Returns the new description string.
func SetMRFields(issue *Issue, fields *MRFields) string {
	if issue == nil {
		return FormatMRFields(fields)
	}

	// Known MR field keys (lowercase)
	mrKeys := map[string]bool{
		"branch":            true,
		"target":            true,
		"source_issue":      true,
		"source-issue":      true,
		"sourceissue":       true,
		"worker":            true,
		"rig":               true,
		"commit_sha":        true,
		"commit-sha":        true,
		"commitsha":         true,
		"pr_url":            true,
		"pr-url":            true,
		"prurl":             true,
		"pr_number":         true,
		"pr-number":         true,
		"prnumber":          true,
		"merge_commit":      true,
		"merge-commit":      true,
		"mergecommit":       true,
		"close_reason":      true,
		"close-reason":      true,
		"closereason":       true,
		"agent_bead":        true,
		"agent-bead":        true,
		"agentbead":         true,
		"retry_count":       true,
		"retry-count":       true,
		"retrycount":        true,
		"last_conflict_sha": true,
		"last-conflict-sha": true,
		"lastconflictsha":   true,
		"conflict_task_id":  true,
		"conflict-task-id":  true,
		"conflicttaskid":    true,
		"convoy_id":         true,
		"convoy-id":         true,
		"convoyid":          true,
		"convoy":            true,
		"convoy_created_at": true,
		"convoy-created-at": true,
		"convoycreatedat":   true,
		"pre_verified":      true,
		"pre-verified":      true,
		"preverified":       true,
		"pre_verified_at":   true,
		"pre-verified-at":   true,
		"preverifiedat":     true,
		"pre_verified_base": true,
		"pre-verified-base": true,
		"preverifiedbase":   true,
	}

	// Collect non-MR lines from existing description
	var otherLines []string
	if issue.Description != "" {
		for _, line := range strings.Split(issue.Description, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				// Preserve blank lines in content
				otherLines = append(otherLines, line)
				continue
			}

			// Check if this is an MR field line
			colonIdx := strings.Index(trimmed, ":")
			if colonIdx == -1 {
				otherLines = append(otherLines, line)
				continue
			}

			key := strings.ToLower(strings.TrimSpace(trimmed[:colonIdx]))
			if !mrKeys[key] {
				otherLines = append(otherLines, line)
			}
			// Skip MR field lines - they'll be replaced
		}
	}

	// Build new description: MR fields first, then other content
	formatted := FormatMRFields(fields)

	otherLines = trimBlankLines(otherLines)

	if formatted == "" {
		return strings.Join(otherLines, "\n")
	}
	if len(otherLines) == 0 {
		return formatted
	}

	return formatted + "\n\n" + strings.Join(otherLines, "\n")
}

// RoleConfig holds structured lifecycle configuration for role beads.
// These fields are stored as "key: value" lines in the role bead description.
// This enables agents to self-register their lifecycle configuration,
// replacing hardcoded identity string parsing in the daemon.
type RoleConfig struct {
	// SessionPattern defines how to derive tmux session name.
	// Supports placeholders: {rig}, {name}, {role}
	// Examples: "hq-mayor", "hq-deacon", "gt-{rig}-{role}", "gt-{rig}-{name}"
	SessionPattern string

	// WorkDirPattern defines the working directory relative to town root.
	// Supports placeholders: {town}, {rig}, {name}, {role}
	// Examples: "{town}", "{town}/{rig}", "{town}/{rig}/polecats/{name}"
	WorkDirPattern string

	// NeedsPreSync indicates whether workspace needs git sync before starting.
	// True for agents with persistent clones (refinery, crew, polecat).
	NeedsPreSync bool

	// StartCommand is the command to run after creating the session.
	// Default: "exec claude --dangerously-skip-permissions"
	StartCommand string

	// EnvVars are additional environment variables to set in the session.
	// Stored as "key=value" pairs.
	EnvVars map[string]string

	// Health check thresholds - per ZFC, agents control their own stuck detection.
	// These allow the Deacon's patrol config to be agent-defined rather than hardcoded.

	// PingTimeout is how long to wait for a health check response.
	// Format: duration string (e.g., "30s", "1m"). Default: 30s.
	PingTimeout string

	// ConsecutiveFailures is how many failed health checks before force-kill.
	// Default: 3.
	ConsecutiveFailures int

	// KillCooldown is the minimum time between force-kills of the same agent.
	// Format: duration string (e.g., "5m", "10m"). Default: 5m.
	KillCooldown string

	// StuckThreshold is how long a wisp can be in_progress before considered stuck.
	// Format: duration string (e.g., "1h", "30m"). Default: 1h.
	StuckThreshold string

	// WispTTLs maps wisp types to their TTL duration strings.
	// Stored as "wisp_ttl_<type>: <duration>" in the role bead description.
	// Examples: wisp_ttl_patrol: 48h, wisp_ttl_error: 336h, wisp_ttl_gc_report: 24h
	// These override rig config and hardcoded defaults for compaction policy.
	WispTTLs map[string]string
}

// ParseRoleConfig extracts RoleConfig from a role bead's description.
// Fields are expected as "key: value" lines. Returns nil if no config found.
func ParseRoleConfig(description string) *RoleConfig {
	config := &RoleConfig{
		EnvVars:  make(map[string]string),
		WispTTLs: make(map[string]string),
	}
	for _, line := range strings.Split(description, "\n") {
		setRoleConfigField(config, line)
	}
	if !hasRoleConfigFields(config) {
		return nil
	}
	return config
}

func setRoleConfigField(config *RoleConfig, line string) {
	key, value, ok := agentDescriptionFieldLine(line)
	if !ok || value == "" {
		return
	}
	if wispType, ok := ParseWispTTLKey(key); ok {
		config.WispTTLs[wispType] = value
		return
	}
	key = convoyFieldKey(key)
	if destination, ok := roleConfigStringFields(config)[key]; ok {
		*destination = value
		return
	}
	if key == "needspresync" {
		config.NeedsPreSync = strings.EqualFold(value, "true")
		return
	}
	if key == "consecutivefailures" {
		if value, err := parseIntField(value); err == nil {
			config.ConsecutiveFailures = value
		}
		return
	}
	if key == "envvar" {
		setRoleConfigEnvVar(config, value)
	}
}

func roleConfigStringFields(config *RoleConfig) map[string]*string {
	return map[string]*string{
		"sessionpattern": &config.SessionPattern, "workdirpattern": &config.WorkDirPattern, "startcommand": &config.StartCommand, "pingtimeout": &config.PingTimeout, "killcooldown": &config.KillCooldown, "stuckthreshold": &config.StuckThreshold,
	}
}
func setRoleConfigEnvVar(config *RoleConfig, value string) {
	if index := strings.Index(value, "="); index != -1 {
		config.EnvVars[strings.TrimSpace(value[:index])] = strings.TrimSpace(value[index+1:])
	}
}
func hasRoleConfigFields(config *RoleConfig) bool {
	for _, value := range []string{config.SessionPattern, config.WorkDirPattern, optionalTrue(config.NeedsPreSync), config.StartCommand, config.PingTimeout, optionalPositiveInt(config.ConsecutiveFailures), config.KillCooldown, config.StuckThreshold} {
		if value != "" {
			return true
		}
	}
	return len(config.EnvVars) > 0 || len(config.WispTTLs) > 0
}

// ParseWispTTLKey checks if a lowercase key matches the wisp_ttl_* pattern
// and returns the wisp type suffix. Supports underscore, hyphen, and camelCase variants.
// Examples: "wisp_ttl_patrol" → "patrol", "wisp-ttl-gc_report" → "gc_report"
func ParseWispTTLKey(key string) (string, bool) {
	for _, prefix := range []string{"wisp_ttl_", "wisp-ttl-", "wispttl"} {
		if strings.HasPrefix(key, prefix) {
			wispType := key[len(prefix):]
			if wispType != "" {
				return wispType, true
			}
		}
	}
	return "", false
}

// FormatRoleConfig formats RoleConfig as a string suitable for a role bead description.
// Only non-empty/non-default fields are included.
func FormatRoleConfig(config *RoleConfig) string {
	if config == nil {
		return ""
	}

	lines := strings.Split(formatOptionalDescriptionFields([]optionalDescriptionField{{"session_pattern", config.SessionPattern}, {"work_dir_pattern", config.WorkDirPattern}, {"needs_pre_sync", optionalTrue(config.NeedsPreSync)}, {"start_command", config.StartCommand}}), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	for k, v := range config.EnvVars {
		lines = append(lines, "env_var: "+k+"="+v)
	}
	// Sort wisp TTL keys for deterministic output
	wispTypes := make([]string, 0, len(config.WispTTLs))
	for k := range config.WispTTLs {
		wispTypes = append(wispTypes, k)
	}
	sort.Strings(wispTypes)
	for _, wt := range wispTypes {
		lines = append(lines, "wisp_ttl_"+wt+": "+config.WispTTLs[wt])
	}

	return strings.Join(lines, "\n")
}

// ExpandRolePattern expands placeholders in a pattern string.
// Supported placeholders: {town}, {rig}, {name}, {role}, {prefix}
func ExpandRolePattern(pattern, townRoot, rig, name, role, prefix string) string {
	result := pattern
	result = strings.ReplaceAll(result, "{town}", townRoot)
	result = strings.ReplaceAll(result, "{rig}", rig)
	result = strings.ReplaceAll(result, "{name}", name)
	result = strings.ReplaceAll(result, "{role}", role)
	result = strings.ReplaceAll(result, "{prefix}", prefix)
	return result
}
