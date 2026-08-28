package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/townlog"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var auditCmd = &cobra.Command{
	Use:     "audit",
	GroupID: GroupDiag,
	Short:   "Query work history by actor",
	Long: `Query provenance data across git commits, beads, and events.

Shows a unified timeline of work performed by an actor including:
  - Git commits authored by the actor
  - Beads (issues) created by the actor
  - Beads closed by the actor (via assignee)
  - Town log events (spawn, done, handoff, etc.)
  - Activity feed events

Examples:
  gt audit --actor=greenplace/crew/joe       # Show all work by joe
  gt audit --actor=greenplace/polecats/toast # Show polecat toast's work
  gt audit --actor=mayor                  # Show mayor's activity
  gt audit --since=24h                    # Show all activity in last 24h
  gt audit --actor=joe --since=1h         # Combined filters
  gt audit --json                         # Output as JSON`,
	RunE: runAudit,
}

func init() {
	auditCmd.Flags().String("actor", "", "Filter by actor (agent address or partial match)")
	auditCmd.Flags().String("since", "", "Show events since duration (e.g., 1h, 24h, 7d)")
	auditCmd.Flags().IntP("limit", "n", 50, "Maximum number of entries to show")
	auditCmd.Flags().Bool("json", false, "Output as JSON")

	rootCmd.AddCommand(auditCmd)
}

// AuditEntry represents a single entry in the audit log.
type AuditEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"` // "git", "beads", "townlog", "events"
	Type      string    `json:"type"`   // "commit", "bead_created", "bead_closed", "spawn", etc.
	Actor     string    `json:"actor"`
	Summary   string    `json:"summary"`
	Details   string    `json:"details,omitempty"`
	ID        string    `json:"id,omitempty"` // commit hash, bead ID, etc.
}

type auditOptions struct {
	actor string
	since string
	limit int
	json  bool
}

func readAuditOptions(cmd *cobra.Command) auditOptions {
	if cmd == nil {
		return auditOptions{limit: 50}
	}
	return auditOptions{
		actor: auditStringFlag(cmd, "actor", ""),
		since: auditStringFlag(cmd, "since", ""),
		limit: auditIntFlag(cmd, "limit", 50),
		json:  auditBoolFlag(cmd, "json", false),
	}
}

func auditStringFlag(cmd *cobra.Command, name, fallback string) string {
	value, err := cmd.Flags().GetString(name)
	if err != nil {
		return fallback
	}
	return value
}

func auditIntFlag(cmd *cobra.Command, name string, fallback int) int {
	value, err := cmd.Flags().GetInt(name)
	if err != nil {
		return fallback
	}
	return value
}

func auditBoolFlag(cmd *cobra.Command, name string, fallback bool) bool {
	value, err := cmd.Flags().GetBool(name)
	if err != nil {
		return fallback
	}
	return value
}

func runAudit(cmd *cobra.Command, _ []string) error {
	opts := readAuditOptions(cmd)
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	sinceTime, err := auditSinceTime(opts.since)
	if err != nil {
		return err
	}

	allEntries := collectAuditEntries(townRoot, opts.actor, sinceTime)

	sort.Slice(allEntries, func(i, j int) bool {
		return allEntries[i].Timestamp.After(allEntries[j].Timestamp)
	})

	allEntries = limitAuditEntries(allEntries, opts.limit)

	if len(allEntries) == 0 {
		if opts.actor != "" {
			fmt.Printf("%s No activity found for actor %q\n", style.Dim.Render("○"), opts.actor)
		} else {
			fmt.Printf("%s No activity found\n", style.Dim.Render("○"))
		}
		return nil
	}

	// Output
	if opts.json {
		return outputAuditJSON(allEntries)
	}
	return outputAuditText(allEntries)
}

func auditSinceTime(since string) (time.Time, error) {
	if since == "" {
		return time.Time{}, nil
	}
	duration, err := parseDuration(since)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since duration: %w", err)
	}
	return time.Now().Add(-duration), nil
}

func collectAuditEntries(townRoot, actor string, since time.Time) []AuditEntry {
	var entries []AuditEntry
	appendAuditSource(&entries, "git commits", func() ([]AuditEntry, error) {
		return collectGitCommits(townRoot, actor, since)
	})
	appendAuditSource(&entries, "beads", func() ([]AuditEntry, error) {
		return collectBeadsActivity(townRoot, actor, since)
	})
	appendAuditSource(&entries, "town log", func() ([]AuditEntry, error) {
		return collectTownlogEvents(townRoot, actor, since)
	})
	appendAuditSource(&entries, "events feed", func() ([]AuditEntry, error) {
		return collectFeedEvents(townRoot, actor, since)
	})
	return entries
}

func appendAuditSource(entries *[]AuditEntry, source string, collect func() ([]AuditEntry, error)) {
	collected, err := collect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not query %s: %v\n", source, err)
		return
	}
	*entries = append(*entries, collected...)
}

func limitAuditEntries(entries []AuditEntry, limit int) []AuditEntry {
	if limit > 0 && len(entries) > limit {
		return entries[:limit]
	}
	return entries
}

// parseDuration parses a duration string with support for days (d).
func parseDuration(s string) (time.Duration, error) {
	// Check for days suffix
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		var d int
		if _, err := fmt.Sscanf(days, "%d", &d); err != nil {
			return 0, fmt.Errorf("invalid days format: %s", s)
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// collectGitCommits queries git log for commits by the actor.
func collectGitCommits(townRoot, actor string, since time.Time) ([]AuditEntry, error) { //nolint:unparam // error return kept for future use
	var entries []AuditEntry

	// Build git log command
	args := []string{"log", "--format=%H|%aI|%an|%s", "--all"}

	if actor != "" {
		// Try to match actor in author name
		// Actor format might be "greenplace/crew/joe" - extract "joe" as the author name
		authorName := extractAuthorName(actor)
		args = append(args, "--author="+authorName)
	}

	if !since.IsZero() {
		args = append(args, "--since="+since.Format(time.RFC3339))
	}

	// Limit to reasonable number
	args = append(args, "-n", "100")

	cmd := exec.Command("git", args...)
	cmd.Dir = townRoot

	output, err := cmd.Output()
	if err != nil {
		// Git might fail if not a repo - not fatal
		return nil, nil
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}

		hash := parts[0]
		timestamp, _ := time.Parse(time.RFC3339, parts[1])
		author := parts[2]
		subject := parts[3]

		// If actor filter is set, also match on the full actor string in commit message
		if actor != "" && !matchesActor(author, actor) && !strings.Contains(subject, actor) {
			continue
		}

		entries = append(entries, AuditEntry{
			Timestamp: timestamp,
			Source:    "git",
			Type:      "commit",
			Actor:     author,
			Summary:   subject,
			ID:        shortHash(hash),
		})
	}

	return entries, nil
}

// extractAuthorName extracts the likely git author name from an actor address.
func extractAuthorName(actor string) string {
	// Actor format: "greenplace/crew/joe" -> "joe"
	// Or: "mayor" -> "mayor"
	parts := strings.Split(actor, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return actor
}

// matchesActor checks if a name matches the actor filter (partial match).
func matchesActor(name, actor string) bool {
	name = strings.ToLower(name)
	actor = strings.ToLower(actor)

	// Exact match
	if name == actor {
		return true
	}

	// Extract last component of actor for matching
	actorName := extractAuthorName(actor)
	if strings.Contains(name, actorName) {
		return true
	}

	// Check if actor appears in name
	if strings.Contains(name, actor) {
		return true
	}

	return false
}

// collectBeadsActivity queries beads for issues created or closed by the actor.
func collectBeadsActivity(townRoot, actor string, since time.Time) ([]AuditEntry, error) {
	var entries []AuditEntry

	// Find the gastown beads path (where gt- prefix issues live)
	gastownBeadsPath := filepath.Join(townRoot, "gastown", "mayor", "rig")
	b := beads.New(gastownBeadsPath)

	// List all issues to filter by created_by and assignee
	issues, err := b.List(beads.ListOptions{
		Status:   "all",
		Priority: -1,
	})
	if err != nil {
		return nil, err
	}

	for _, issue := range issues {
		appendCreatedBeadEntry(&entries, issue, actor, since)
		appendClosedBeadEntry(&entries, issue, actor, since)
	}

	return entries, nil
}

func appendCreatedBeadEntry(entries *[]AuditEntry, issue *beads.Issue, actor string, since time.Time) {
	if issue.CreatedBy == "" || (actor != "" && !matchesActor(issue.CreatedBy, actor)) {
		return
	}
	ts := parseBeadsTimestamp(issue.CreatedAt)
	if !since.IsZero() && ts.Before(since) {
		return
	}
	*entries = append(*entries, AuditEntry{
		Timestamp: ts,
		Source:    "beads",
		Type:      "bead_created",
		Actor:     issue.CreatedBy,
		Summary:   fmt.Sprintf("Created: %s", issue.Title),
		ID:        issue.ID,
		Details:   fmt.Sprintf("type=%s priority=%d", issue.Type, issue.Priority),
	})
}

func appendClosedBeadEntry(entries *[]AuditEntry, issue *beads.Issue, actor string, since time.Time) {
	if issue.Status != "closed" || issue.Assignee == "" || (actor != "" && !matchesActor(issue.Assignee, actor)) {
		return
	}
	ts := parseBeadsTimestamp(issue.ClosedAt)
	if ts.IsZero() {
		ts = parseBeadsTimestamp(issue.UpdatedAt)
	}
	if !since.IsZero() && ts.Before(since) {
		return
	}
	*entries = append(*entries, AuditEntry{
		Timestamp: ts,
		Source:    "beads",
		Type:      "bead_closed",
		Actor:     issue.Assignee,
		Summary:   fmt.Sprintf("Closed: %s", issue.Title),
		ID:        issue.ID,
	})
}

// parseBeadsTimestamp parses a beads timestamp string.
func parseBeadsTimestamp(s string) time.Time {
	// Try various formats
	formats := []string{
		time.RFC3339,
		"2006-01-02 15:04",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, format := range formats {
		if t, err := time.Parse(format, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// collectTownlogEvents queries the town log for agent lifecycle events.
func collectTownlogEvents(townRoot, actor string, since time.Time) ([]AuditEntry, error) {
	var entries []AuditEntry

	allEvents, err := townlog.ReadEvents(townRoot)
	if err != nil {
		return nil, err
	}

	for _, e := range allEvents {
		// Apply actor filter
		if actor != "" && !matchesActor(e.Agent, actor) {
			continue
		}

		// Apply since filter
		if !since.IsZero() && e.Timestamp.Before(since) {
			continue
		}

		entries = append(entries, AuditEntry{
			Timestamp: e.Timestamp,
			Source:    "townlog",
			Type:      string(e.Type),
			Actor:     e.Agent,
			Summary:   formatTownlogSummary(e),
		})
	}

	return entries, nil
}

// formatTownlogSummary creates a readable summary from a town log event.
func formatTownlogSummary(e townlog.Event) string {
	switch e.Type {
	case townlog.EventSpawn:
		return formatEventContext(e.Context, "Spawned for %s", "Spawned")
	case townlog.EventDone:
		return formatEventContext(e.Context, "Completed %s", "Completed work")
	case townlog.EventHandoff:
		return "Handed off session"
	case townlog.EventHandoffNoPersist:
		return "Handoff FAILED (Dolt persistence)"
	case townlog.EventCrash:
		return formatEventContext(e.Context, "Crashed: %s", "Crashed")
	case townlog.EventKill:
		return "Session killed"
	case townlog.EventNudge:
		return "Nudged"
	case townlog.EventWake:
		return "Resumed"
	default:
		return formatEventContext(e.Context, fmt.Sprintf("%s: %%s", e.Type), string(e.Type))
	}
}

func formatEventContext(context, format, fallback string) string {
	if context != "" {
		return fmt.Sprintf(format, context)
	}
	return fallback
}

// collectFeedEvents queries the activity feed for events.
func collectFeedEvents(townRoot, actor string, since time.Time) ([]AuditEntry, error) {
	var entries []AuditEntry

	eventsPath := filepath.Join(townRoot, events.EventsFile)
	file, err := os.Open(eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No events file yet
		}
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var e events.Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // Skip malformed lines
		}

		// Apply actor filter
		if actor != "" && !matchesActor(e.Actor, actor) {
			continue
		}

		// Parse timestamp
		ts, _ := time.Parse(time.RFC3339, e.Timestamp)

		// Apply since filter
		if !since.IsZero() && ts.Before(since) {
			continue
		}

		entries = append(entries, AuditEntry{
			Timestamp: ts,
			Source:    "events",
			Type:      e.Type,
			Actor:     e.Actor,
			Summary:   formatFeedSummary(e),
		})
	}

	return entries, nil
}

// formatFeedSummary creates a readable summary from a feed event.
func formatFeedSummary(e events.Event) string {
	switch e.Type {
	case events.TypeSling:
		return formatFeedPayload(e, "bead", "Slung %s", "Slung work")
	case events.TypeMerged:
		return formatFeedPayload(e, "branch", "Merged %s", "Merged work")
	case events.TypeMergeFailed:
		return formatFeedPayload(e, "reason", "Merge failed: %s", "Merge failed")
	case events.TypeHandoff:
		return "Handed off"
	case events.TypeDone:
		return formatFeedPayload(e, "bead", "Done %s", "Done")
	case events.TypeMail:
		return formatFeedPayload(e, "to", "Sent mail to %s", "Sent mail")
	default:
		return e.Type
	}
}

func formatFeedPayload(e events.Event, key, format, fallback string) string {
	value, ok := e.Payload[key].(string)
	if ok {
		return fmt.Sprintf(format, value)
	}
	return fallback
}

func outputAuditJSON(entries []AuditEntry) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(entries)
}

func outputAuditText(entries []AuditEntry) error {
	// Group by date for readability
	var currentDate string

	for _, e := range entries {
		date := e.Timestamp.Format("2006-01-02")
		if date != currentDate {
			if currentDate != "" {
				fmt.Println()
			}
			fmt.Printf("%s\n", style.Bold.Render("─── "+date+" ───────────────────────────────────────────"))
			currentDate = date
		}

		timeStr := e.Timestamp.Format("15:04:05")
		sourceStr := formatSource(e.Source)
		typeStr := formatType(e.Type)

		// Build the line
		var idPart string
		if e.ID != "" {
			idPart = style.Dim.Render(fmt.Sprintf(" [%s]", e.ID))
		}

		fmt.Printf("%s %s %s %s%s\n",
			style.Dim.Render(timeStr),
			sourceStr,
			typeStr,
			e.Summary,
			idPart,
		)

		if e.Actor != "" {
			fmt.Printf("         %s\n", style.Dim.Render("by "+e.Actor))
		}
	}

	return nil
}

func formatSource(source string) string {
	switch source {
	case "git":
		return style.Bold.Render("[git]")
	case "beads":
		return style.Success.Render("[beads]")
	case "townlog":
		return style.Dim.Render("[log]")
	case "events":
		return style.Warning.Render("[events]")
	default:
		return fmt.Sprintf("[%s]", source)
	}
}

func formatType(t string) string {
	switch t {
	case "commit":
		return style.Success.Render("commit")
	case "bead_created":
		return style.Success.Render("created")
	case "bead_closed":
		return style.Bold.Render("closed")
	case "spawn":
		return style.Success.Render("spawn")
	case "done":
		return style.Success.Render("done")
	default:
		return formatLifecycleType(t)
	}
}

func formatLifecycleType(t string) string {
	switch t {
	case "handoff":
		return style.Bold.Render("handoff")
	case "crash":
		return style.Error.Render("crash")
	case "kill":
		return style.Warning.Render("kill")
	case "merged":
		return style.Success.Render("merged")
	case "merge_failed":
		return style.Error.Render("merge_failed")
	default:
		return t
	}
}
