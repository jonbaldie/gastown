package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var trailCmd = &cobra.Command{
	Use:     "trail",
	Aliases: []string{"recent", "recap"},
	GroupID: GroupWork,
	Short:   "Show recent agent activity",
	Long: `Show recent activity in the workspace.

Without a subcommand, shows recent commits from agents.

Subcommands:
  commits    Recent git commits from agents
  beads      Recent beads (work items)
  hooks      Recent hook activity

Flags:
  --since    Show activity since this time (e.g., "1h", "24h", "7d")
  --limit    Maximum number of items to show (default: 20)
  --json     Output as JSON
  --all      Include all activity (not just agents)

Examples:
  gt trail                     # Recent commits (default)
  gt trail commits             # Same as above
  gt trail commits --since 1h  # Last hour
  gt trail beads               # Recent beads
  gt trail hooks               # Recent hook activity
  gt recent                    # Alias for gt trail
  gt recap --since 24h         # Activity from last 24 hours`,
	RunE: runTrailCommits, // Default to commits
}

var trailCommitsCmd = &cobra.Command{
	Use:   "commits",
	Short: "Show recent commits from agents",
	Long: `Show recent git commits made by agents.

By default, filters to commits from agents (using the configured
email domain). Use --all to include all commits.

Examples:
  gt trail commits              # Recent agent commits
  gt trail commits --since 1h   # Last hour of commits
  gt trail commits --all        # All commits (including non-agents)
  gt trail commits --json       # JSON output`,
	RunE: runTrailCommits,
}

var trailBeadsCmd = &cobra.Command{
	Use:   "beads",
	Short: "Show recent beads",
	Long: `Show recently created or modified beads (work items).

Examples:
  gt trail beads              # Recent beads
  gt trail beads --since 24h  # Last 24 hours of beads
  gt trail beads --json       # JSON output`,
	RunE: runTrailBeads,
}

var trailHooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Show recent hook activity",
	Long: `Show recent hook activity (agents taking or dropping hooks).

Examples:
  gt trail hooks              # Recent hook activity
  gt trail hooks --since 1h   # Last hour of hook activity
  gt trail hooks --json       # JSON output`,
	RunE: runTrailHooks,
}

func init() {
	// Add flags to trail command
	trailCmd.PersistentFlags().String("since", "", "Show activity since this time (e.g., 1h, 24h, 7d)")
	trailCmd.PersistentFlags().Int("limit", 20, "Maximum number of items to show")
	trailCmd.PersistentFlags().Bool("json", false, "Output as JSON")
	trailCmd.PersistentFlags().Bool("all", false, "Include all activity (not just agents)")

	// Add subcommands
	trailCmd.AddCommand(trailCommitsCmd)
	trailCmd.AddCommand(trailBeadsCmd)
	trailCmd.AddCommand(trailHooksCmd)

	// Register with root
	rootCmd.AddCommand(trailCmd)
}

// CommitEntry represents a git commit for output.
type CommitEntry struct {
	Hash      string    `json:"hash"`
	ShortHash string    `json:"short_hash"`
	Author    string    `json:"author"`
	Email     string    `json:"email"`
	Date      time.Time `json:"date"`
	DateRel   string    `json:"date_relative"`
	Subject   string    `json:"subject"`
	IsAgent   bool      `json:"is_agent"`
}

func runTrailCommits(cmd *cobra.Command, _ []string) error {
	sinceText := commandStringFlag(cmd, "since")
	limit := commandIntFlag(cmd, "limit")
	jsonOutput := commandBoolFlag(cmd, "json")
	includeAll := commandBoolFlag(cmd, "all")
	domain := trailAgentEmailDomain()

	gitArgs, err := trailCommitsArgs(sinceText, limit)
	if err != nil {
		return err
	}

	gitCmd := exec.Command("git", gitArgs...)
	output, err := gitCmd.Output()
	if err != nil {
		return fmt.Errorf("running git log: %w", err)
	}

	commits := parseTrailCommits(string(output), domain, includeAll, limit)

	if jsonOutput {
		return outputTrailJSON(commits)
	}

	if len(commits) == 0 {
		fmt.Println("No commits found")
		return nil
	}

	fmt.Printf("%s\n\n", style.Bold.Render("Recent Commits"))
	printTrailCommits(commits)

	return nil
}

func trailAgentEmailDomain() string {
	domain := DefaultAgentEmailDomain
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return domain
	}
	settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err == nil && settings.AgentEmailDomain != "" {
		return settings.AgentEmailDomain
	}
	return domain
}

func trailCommitsArgs(sinceText string, limit int) ([]string, error) {
	args := []string{"log", "--format=%H|%h|%an|%ae|%aI|%ar|%s", fmt.Sprintf("-n%d", limit*2)}
	if sinceText == "" {
		return args, nil
	}
	duration, err := parseDuration(sinceText)
	if err != nil {
		return nil, fmt.Errorf("invalid --since value: %w", err)
	}
	since := time.Now().Add(-duration)
	return append(args, fmt.Sprintf("--since=%s", since.Format(time.RFC3339))), nil
}

func parseTrailCommits(output, domain string, includeAll bool, limit int) []CommitEntry {
	var commits []CommitEntry
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		commit, ok := parseTrailCommit(line, domain, includeAll)
		if !ok {
			continue
		}
		commits = append(commits, commit)
		if len(commits) >= limit {
			break
		}
	}
	return commits
}

func parseTrailCommit(line, domain string, includeAll bool) (CommitEntry, bool) {
	if line == "" {
		return CommitEntry{}, false
	}
	parts := strings.SplitN(line, "|", 7)
	if len(parts) < 7 {
		return CommitEntry{}, false
	}
	isAgent := strings.HasSuffix(parts[3], "@"+domain)
	if !includeAll && !isAgent {
		return CommitEntry{}, false
	}
	date, _ := time.Parse(time.RFC3339, parts[4])
	return CommitEntry{
		Hash:      parts[0],
		ShortHash: parts[1],
		Author:    parts[2],
		Email:     parts[3],
		Date:      date,
		DateRel:   parts[5],
		Subject:   parts[6],
		IsAgent:   isAgent,
	}, true
}

func outputTrailJSON(value interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func printTrailCommits(commits []CommitEntry) {
	fmt.Printf("%s\n\n", style.Bold.Render("Recent Commits"))
	for _, commit := range commits {
		author := commit.Author
		if commit.IsAgent {
			author = style.Bold.Render(commit.Author)
		}
		fmt.Printf("%s %s\n", style.Dim.Render(commit.ShortHash), commit.Subject)
		fmt.Printf("    %s %s\n", author, style.Dim.Render(commit.DateRel))
	}
}

// BeadEntry represents a bead for output.
type BeadEntry struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Agent     string    `json:"agent,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdateRel string    `json:"updated_relative"`
}

// HookEntry represents a hook/unhook event for output.
type HookEntry struct {
	Type      string    `json:"type"`
	Actor     string    `json:"actor"`
	Bead      string    `json:"bead,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	TimeRel   string    `json:"time_relative"`
}

func runTrailBeads(cmd *cobra.Command, _ []string) error {
	sinceText := commandStringFlag(cmd, "since")
	limit := commandIntFlag(cmd, "limit")
	jsonOutput := commandBoolFlag(cmd, "json")
	beadsDir, err := findBeadsDir()
	if err != nil {
		return fmt.Errorf("finding beads: %w", err)
	}

	beadsArgs, err := trailBeadsArgs(sinceText, limit)
	if err != nil {
		return err
	}

	beadsCmd := exec.Command("beads", beadsArgs...)
	beadsCmd.Dir = beadsDir
	beadsCmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir+"/.beads")
	output, err := beadsCmd.Output()
	if err != nil {
		// Fallback: beads might not support all these flags
		// Try a simpler approach
		return runTrailBeadsSimple(beadsDir, limit)
	}

	entries := parseTrailBeads(string(output))

	if jsonOutput {
		return outputTrailJSON(entries)
	}

	if len(entries) == 0 {
		fmt.Println("No beads found")
		return nil
	}

	printTrailBeads(entries)

	return nil
}

func trailBeadsArgs(sinceText string, limit int) ([]string, error) {
	args := []string{
		"query",
		"--format", "{{.ID}}|{{.Title}}|{{.Status}}|{{.Agent}}|{{.UpdatedAt}}",
		"--limit", fmt.Sprintf("%d", limit),
		"--sort", "-updated_at",
	}
	if sinceText == "" {
		return args, nil
	}
	duration, err := parseDuration(sinceText)
	if err != nil {
		return nil, fmt.Errorf("invalid --since value: %w", err)
	}
	since := time.Now().Add(-duration)
	return append(args, "--since", since.Format(time.RFC3339)), nil
}

func parseTrailBeads(output string) []BeadEntry {
	var entries []BeadEntry
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		entry, ok := parseTrailBead(line)
		if ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

func parseTrailBead(line string) (BeadEntry, bool) {
	if line == "" {
		return BeadEntry{}, false
	}
	parts := strings.SplitN(line, "|", 5)
	if len(parts) < 5 {
		return BeadEntry{}, false
	}
	updatedAt, _ := time.Parse(time.RFC3339, parts[4])
	return BeadEntry{
		ID:        parts[0],
		Title:     parts[1],
		Status:    parts[2],
		Agent:     parts[3],
		UpdatedAt: updatedAt,
		UpdateRel: relativeTime(updatedAt),
	}, true
}

func printTrailBeads(entries []BeadEntry) {
	fmt.Printf("%s\n\n", style.Bold.Render("Recent Beads"))
	for _, entry := range entries {
		statusColor := trailBeadStatusStyle(entry.Status)
		fmt.Printf("%s %s\n", style.Bold.Render(entry.ID), entry.Title)
		fmt.Printf("    %s %s", statusColor.Render(entry.Status), style.Dim.Render(entry.UpdateRel))
		if entry.Agent != "" {
			fmt.Printf(" by %s", entry.Agent)
		}
		fmt.Println()
	}
}

func trailBeadStatusStyle(status string) lipgloss.Style {
	switch status {
	case "open":
		return style.Success
	case "in_progress":
		return style.Warning
	case "done", "merged":
		return style.Info
	default:
		return style.Dim
	}
}

func runTrailBeadsSimple(beadsDir string, limit int) error {
	// Simple fallback using beads list
	beadsCmd := exec.Command("beads", "list", "--limit", fmt.Sprintf("%d", limit))
	beadsCmd.Dir = beadsDir
	beadsCmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir+"/.beads")
	beadsCmd.Stdout = os.Stdout
	beadsCmd.Stderr = os.Stderr
	return beadsCmd.Run()
}

func runTrailHooks(cmd *cobra.Command, _ []string) error {
	sinceText := commandStringFlag(cmd, "since")
	limit := commandIntFlag(cmd, "limit")
	jsonOutput := commandBoolFlag(cmd, "json")
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	since, err := trailSince(sinceText)
	if err != nil {
		return err
	}

	entries, err := readHookTrailEntries(filepath.Join(townRoot, events.EventsFile), since, limit)
	if err != nil {
		return err
	}

	if jsonOutput {
		return outputTrailJSON(entries)
	}

	if len(entries) == 0 {
		fmt.Println("No hook activity found")
		return nil
	}

	printTrailHooks(entries)

	return nil
}

func trailSince(sinceText string) (time.Time, error) {
	if sinceText == "" {
		return time.Time{}, nil
	}
	duration, err := parseDuration(sinceText)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since value: %w", err)
	}
	return time.Now().Add(-duration), nil
}

func printTrailHooks(entries []HookEntry) {
	fmt.Printf("%s\n\n", style.Bold.Render("Hook Activity"))
	for _, entry := range entries {
		action := "hooked"
		if entry.Type == events.TypeUnhook {
			action = "unhooked"
		}
		target := entry.Bead
		if target == "" {
			target = "a bead"
		}
		fmt.Printf("%s %s %s %s\n",
			style.Dim.Render(entry.Timestamp.Format("2006-01-02 15:04")),
			style.Bold.Render(entry.Actor), action, style.Bold.Render(target))
		if entry.TimeRel != "" {
			fmt.Printf("    %s\n", style.Dim.Render(entry.TimeRel))
		}
	}
}

func readHookTrailEntries(eventsPath string, since time.Time, limit int) ([]HookEntry, error) {
	if limit <= 0 {
		return []HookEntry{}, nil
	}

	data, err := os.ReadFile(eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading events file: %w", err)
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, nil
	}

	lines := strings.Split(trimmed, "\n")
	entryCap := len(lines)
	if limit < entryCap {
		entryCap = limit
	}
	entries := make([]HookEntry, 0, entryCap)
	for i := len(lines) - 1; i >= 0; i-- {
		entry, ok := parseHookTrailEntry(lines[i], since)
		if !ok {
			continue
		}
		entries = append(entries, entry)
		if len(entries) >= limit {
			break
		}
	}

	return entries, nil
}

func parseHookTrailEntry(line string, since time.Time) (HookEntry, bool) {
	line = strings.TrimSpace(line)
	event, ok := decodeHookTrailEvent(line)
	if !ok {
		return HookEntry{}, false
	}
	ts, ok := hookTrailTimestamp(event.Timestamp, since)
	if !ok {
		return HookEntry{}, false
	}
	return HookEntry{
		Type:      event.Type,
		Actor:     hookTrailActor(event.Actor),
		Bead:      hookTrailBead(event.Payload),
		Timestamp: ts,
		TimeRel:   relativeTime(ts),
	}, true
}

func decodeHookTrailEvent(line string) (events.Event, bool) {
	if line == "" {
		return events.Event{}, false
	}
	var event events.Event
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return events.Event{}, false
	}
	validType := event.Type == events.TypeHook || event.Type == events.TypeUnhook
	return event, validType
}

func hookTrailTimestamp(value string, since time.Time) (time.Time, bool) {
	ts, err := time.Parse(time.RFC3339, value)
	if err != nil || (!since.IsZero() && ts.Before(since)) {
		return time.Time{}, false
	}
	return ts, true
}

func hookTrailActor(actor string) string {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "unknown"
	}
	return actor
}

func hookTrailBead(payload map[string]interface{}) string {
	rawBead, ok := payload["bead"]
	if !ok || rawBead == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(rawBead))
}

func findBeadsDir() (string, error) {
	// Try local beads dir first
	dir, err := findLocalBeadsDir()
	if err == nil {
		return dir, nil
	}

	// Fall back to town root
	return findMailWorkDir()
}

func relativeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	diff := time.Since(t)
	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		mins := int(diff.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	case diff < 24*time.Hour:
		hours := int(diff.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	default:
		days := int(diff.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}
