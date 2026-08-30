package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/worker"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var seanceCmd = &cobra.Command{
	Use:     "seance",
	GroupID: GroupDiag,
	Short:   "Talk to your predecessor sessions",
	Long: `Seance lets you literally talk to predecessor sessions.

"Where did you put the stuff you left for me?" - The #1 handoff question.

Instead of parsing logs, seance spawns a Claude subprocess that resumes
a predecessor session with full context. You can ask questions directly:
  - "Why did you make this decision?"
  - "Where were you stuck?"
  - "What did you try that didn't work?"

DISCOVERY:
  gt seance                     # List recent sessions from events
  gt seance --role crew         # Filter by role type
  gt seance --rig gastown       # Filter by rig
  gt seance --recent 10         # Last N sessions

THE SEANCE (talk to predecessor):
  gt seance --talk <session-id>              # Interactive conversation
  gt seance --talk <id> -p "Where is X?"     # One-shot question

The --talk flag spawns: claude --fork-session --resume <id>
This loads the predecessor's full context without modifying their session.

Sessions are discovered from:
  1. Events emitted by SessionStart hooks (~/gt/.events.jsonl)
  2. The [GAS TOWN] beacon makes sessions searchable in /resume`,
	RunE: runSeance,
}

func init() {
	seanceCmd.Flags().String("role", "", "Filter by role (crew, polecat, witness, etc.)")
	seanceCmd.Flags().String("rig", "", "Filter by rig name")
	seanceCmd.Flags().IntP("recent", "n", 20, "Number of recent sessions to show")
	seanceCmd.Flags().StringP("talk", "t", "", "Session ID to commune with")
	seanceCmd.Flags().StringP("prompt", "p", "", "One-shot prompt (with --talk)")
	seanceCmd.Flags().Bool("json", false, "Output as JSON")

	rootCmd.AddCommand(seanceCmd)
}

// sessionEvent represents a session_start event from our event stream.
type sessionEvent struct {
	Timestamp string                 `json:"ts"`
	Type      string                 `json:"type"`
	Actor     string                 `json:"actor"`
	Payload   map[string]interface{} `json:"payload"`
}

type seanceOptions struct {
	role   string
	rig    string
	recent int
	talk   string
	prompt string
	json   bool
}

func runSeance(cmd *cobra.Command, _ []string) error {
	opts := seanceOptions{
		role:   commandStringFlag(cmd, "role"),
		rig:    commandStringFlag(cmd, "rig"),
		recent: commandIntFlag(cmd, "recent"),
		talk:   commandStringFlag(cmd, "talk"),
		prompt: commandStringFlag(cmd, "prompt"),
		json:   commandBoolFlag(cmd, "json"),
	}

	// If --talk is provided, spawn a seance
	if opts.talk != "" {
		return runSeanceTalk(opts.talk, opts.prompt)
	}

	// Otherwise, list discoverable sessions
	return runSeanceList(opts)
}

func runSeanceList(opts seanceOptions) error {
	sessions, err := loadSeanceListSessions()
	if err != nil {
		return err
	}
	filtered := filterSeanceSessions(sessions, opts)
	if opts.recent > 0 && len(filtered) > opts.recent {
		filtered = filtered[:opts.recent]
	}
	return printSeanceList(filtered, opts.json)
}

func loadSeanceListSessions() ([]sessionEvent, error) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return nil, fmt.Errorf("not in a Gas Town workspace")
	}
	sessions, err := discoverSessions(townRoot)
	if err != nil {
		return nil, fmt.Errorf("discovering sessions: %w", err)
	}
	return sessions, nil
}

func filterSeanceSessions(sessions []sessionEvent, opts seanceOptions) []sessionEvent {
	var filtered []sessionEvent
	for _, s := range sessions {
		if seanceSessionMatches(s, opts) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func seanceSessionMatches(s sessionEvent, opts seanceOptions) bool {
	actor := strings.ToLower(s.Actor)
	if opts.role != "" && !strings.Contains(actor, strings.ToLower(opts.role)) {
		return false
	}
	if opts.rig != "" && !strings.Contains(actor, strings.ToLower(opts.rig)) {
		return false
	}
	return true
}

func printSeanceList(filtered []sessionEvent, jsonOutput bool) error {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(filtered)
	}
	if len(filtered) == 0 {
		fmt.Println("No session events found.")
		fmt.Println(style.Dim.Render("Sessions are discovered from ~/gt/.events.jsonl"))
		fmt.Println(style.Dim.Render("Ensure SessionStart hooks emit session_start events"))
		return nil
	}
	printSeanceListTable(filtered)
	return nil
}

func printSeanceListTable(filtered []sessionEvent) {
	fmt.Printf("%s\n\n", style.Bold.Render("Discoverable Sessions"))
	idWidth, roleWidth, timeWidth, topicWidth := 12, 26, 16, 28
	fmt.Printf("%-*s  %-*s  %-*s  %-*s\n",
		idWidth, "SESSION_ID",
		roleWidth, "ROLE",
		timeWidth, "STARTED",
		topicWidth, "TOPIC")
	fmt.Printf("%s\n", strings.Repeat("─", idWidth+roleWidth+timeWidth+topicWidth+6))
	for _, s := range filtered {
		printSeanceListRow(s, idWidth, roleWidth, timeWidth, topicWidth)
	}
	fmt.Printf("\n%s\n", style.Bold.Render("Talk to a predecessor:"))
	fmt.Printf("  gt seance --talk <session-id>\n")
	fmt.Printf("  gt seance --talk <session-id> -p \"Where did you put X?\"\n")
}

func printSeanceListRow(s sessionEvent, idWidth, roleWidth, timeWidth, topicWidth int) {
	sessionID := truncateSeanceCol(getPayloadString(s.Payload, "session_id"), idWidth)
	role := truncateSeanceCol(s.Actor, roleWidth)
	topic := getPayloadString(s.Payload, "topic")
	if topic == "" {
		topic = "-"
	}
	fmt.Printf("%-*s  %-*s  %-*s  %-*s\n",
		idWidth, sessionID,
		roleWidth, role,
		timeWidth, formatEventTime(s.Timestamp),
		topicWidth, truncateSeanceCol(topic, topicWidth))
}

func truncateSeanceCol(value string, width int) string {
	if len(value) > width {
		return value[:width-1] + "…"
	}
	return value
}

// resolveSeanceCommand finds the command for an agent that supports --fork-session.
// Returns the resolved command path, or error if no agent supports fork session.
func resolveSeanceCommand() (string, error) {
	for _, name := range config.ListAgentPresets() {
		preset := config.GetAgentPresetByName(name)
		if preset != nil && preset.SupportsForkSession {
			// Use RuntimeConfigFromPreset to resolve the actual command path
			rc := config.RuntimeConfigFromPreset(preset.Name)
			return rc.Command, nil
		}
	}
	return "", fmt.Errorf("no agent supports fork session (seance requires --fork-session)")
}

func runSeanceTalk(sessionID, prompt string) error {
	agentCmd, err := resolveSeanceCommand()
	if err != nil {
		return err
	}
	cleanupOrphanedSessionSymlinks()
	townRoot, _ := workspace.FindFromCwd()
	sessionID, err = resolveSeanceTalkSessionID(townRoot, sessionID)
	if err != nil {
		return err
	}
	fmt.Printf("%s Summoning session %s...\n\n", style.Bold.Render("🔮"), sessionID)
	cleanup, err := symlinkSessionToCurrentAccount(townRoot, sessionID)
	if err != nil {
		fmt.Printf("%s\n", style.Dim.Render("Note: "+err.Error()))
	}
	if cleanup != nil {
		defer cleanup()
	}
	return runSeanceTalkCommand(agentCmd, sessionID, prompt)
}

func resolveSeanceTalkSessionID(townRoot, sessionID string) (string, error) {
	if len(sessionID) >= 36 {
		return sessionID, nil
	}
	sessionID = strings.TrimSuffix(sessionID, "…")
	sessionID = strings.TrimSuffix(sessionID, "...")
	if townRoot == "" {
		return sessionID, nil
	}
	resolved, err := resolveSessionPrefix(townRoot, sessionID)
	if err != nil {
		return "", fmt.Errorf("resolving session ID: %w", err)
	}
	return resolved, nil
}

func runSeanceTalkCommand(agentCmd, sessionID, prompt string) error {
	args := []string{"--fork-session", "--resume", sessionID}
	env := clearClaudeCodeEnv(os.Environ())
	if prompt != "" {
		return runSeanceTalkOneShot(agentCmd, args, env, prompt)
	}
	return runSeanceTalkInteractive(agentCmd, args, env)
}

func runSeanceTalkOneShot(agentCmd string, args, env []string, prompt string) error {
	args = append(args, "-p", prompt)
	cmd := exec.Command(agentCmd, args...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("seance failed: %w", err)
	}
	return nil
}

func runSeanceTalkInteractive(agentCmd string, args, env []string) error {
	cmd := exec.Command(agentCmd, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	fmt.Printf("%s\n", style.Dim.Render("You are now talking to your predecessor. Ask them anything."))
	fmt.Printf("%s\n\n", style.Dim.Render("Exit with /exit or Ctrl+C"))
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 0 || exitErr.ExitCode() == 130 {
				return nil
			}
		}
		return fmt.Errorf("seance ended: %w", err)
	}
	return nil
}

// clearClaudeCodeEnv returns a copy of the environment with Claude Code
// nesting-detection variables removed. This prevents the nested-session
// guard from blocking seance subprocesses.
func clearClaudeCodeEnv(environ []string) []string {
	var filtered []string
	for _, e := range environ {
		if strings.HasPrefix(e, "CLAUDECODE=") ||
			strings.HasPrefix(e, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

// discoverSessions reads session_start events from our event stream.
func discoverSessions(townRoot string) ([]sessionEvent, error) {
	sessions, err := readSessionStartEvents(townRoot)
	if err != nil {
		return nil, err
	}
	sessions = append(sessions, workerSessionEvents(townRoot)...)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Timestamp > sessions[j].Timestamp
	})
	return sessions, nil
}

func readSessionStartEvents(townRoot string) ([]sessionEvent, error) {
	file, err := os.Open(filepath.Join(townRoot, events.EventsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	var sessions []sessionEvent
	for scanner.Scan() {
		var event sessionEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Type == events.TypeSessionStart {
			sessions = append(sessions, event)
		}
	}
	return sessions, scanner.Err()
}

func workerSessionEvents(townRoot string) []sessionEvent {
	wev, err := worker.ReadEvents(townRoot)
	if err != nil {
		return nil
	}
	var sessions []sessionEvent
	for _, ev := range wev {
		if ev.Type != worker.EventStarted && ev.Type != worker.EventStopped {
			continue
		}
		sessions = append(sessions, sessionEvent{
			Timestamp: ev.Timestamp.UTC().Format(time.RFC3339),
			Type:      events.TypeSessionStart,
			Actor:     ev.SessionID,
			Payload: map[string]interface{}{
				"session_id": ev.SessionID,
				"run_id":     ev.RunID,
				"bead_id":    ev.BeadID,
				"topic":      ev.Type,
			},
		})
	}
	return sessions
}

// resolveSessionPrefix resolves a truncated session ID prefix to the full UUID
// by searching session_start events. Returns an error if zero or multiple matches.
func resolveSessionPrefix(townRoot, prefix string) (string, error) {
	sessions, err := discoverSessions(townRoot)
	if err != nil {
		return "", fmt.Errorf("searching sessions: %w", err)
	}

	var matches []string
	seen := make(map[string]bool)
	for _, s := range sessions {
		id := getPayloadString(s.Payload, "session_id")
		if strings.HasPrefix(id, prefix) && !seen[id] {
			matches = append(matches, id)
			seen[id] = true
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session found matching prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		display := matches
		if len(display) > 3 {
			display = display[:3]
		}
		return "", fmt.Errorf("ambiguous prefix %q matches %d sessions: %s",
			prefix, len(matches), strings.Join(display, ", "))
	}
}

func getPayloadString(payload map[string]interface{}, key string) string {
	if v, ok := payload[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func formatEventTime(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Local().Format("2006-01-02 15:04")
}

// sessionsIndex represents the structure of sessions-index.json files.
// We use json.RawMessage for entries to preserve all fields when copying.
type sessionsIndex struct {
	Version int               `json:"version"`
	Entries []json.RawMessage `json:"entries"`
}

// sessionsIndexEntry is a minimal struct to extract just the sessionId from an entry.
type sessionsIndexEntry struct {
	SessionID string `json:"sessionId"`
}

// sessionLocation contains the location info for a session.
type sessionLocation struct {
	configDir  string // The account's config directory
	projectDir string // The project directory name (e.g., "-Users-jv-gt-gastown-crew-propane")
}

// sessionsIndexLockTimeout is how long to wait for the index lock.
const sessionsIndexLockTimeout = 5 * time.Second

// lockSessionsIndex acquires an exclusive lock on the sessions index file.
// Returns the lock (caller must unlock) or error if lock cannot be acquired.
// The lock file is created adjacent to the index file with a .lock suffix.
func lockSessionsIndex(indexPath string) (*flock.Flock, error) {
	lockPath := indexPath + ".lock"

	// Ensure the directory exists
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}

	lock := flock.New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), sessionsIndexLockTimeout)
	defer cancel()

	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquiring lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("timeout waiting for sessions index lock")
	}

	return lock, nil
}

// findSessionLocation searches all account config directories for a session.
// Returns the config directory and project directory that contain the session.
func findSessionLocation(townRoot, sessionID string) *sessionLocation {
	if townRoot == "" {
		return nil
	}
	if loc := findSessionLocationInAccounts(townRoot, sessionID); loc != nil {
		return loc
	}
	return findSessionLocationInClaudeHome(sessionID)
}

func findSessionLocationInAccounts(townRoot, sessionID string) *sessionLocation {
	cfg, err := config.LoadAccountsConfig(constants.MayorAccountsPath(townRoot))
	if err != nil {
		return nil
	}
	for _, acct := range cfg.Accounts {
		if acct.ConfigDir == "" {
			continue
		}
		if loc := findSessionInConfigDir(expandSeanceHomePath(acct.ConfigDir), sessionID); loc != nil {
			return loc
		}
	}
	return nil
}

func expandSeanceHomePath(configDir string) string {
	if !strings.HasPrefix(configDir, "~/") {
		return configDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, configDir[2:])
}

func findSessionInConfigDir(configDir, sessionID string) *sessionLocation {
	projectsDir := filepath.Join(configDir, "projects")
	if _, err := os.Stat(projectsDir); os.IsNotExist(err) {
		return nil
	}
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if sessionIndexContainsID(filepath.Join(projectsDir, entry.Name(), "sessions-index.json"), sessionID) {
			return &sessionLocation{configDir: configDir, projectDir: entry.Name()}
		}
	}
	return nil
}

func sessionIndexContainsID(indexPath, sessionID string) bool {
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		return false
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return false
	}
	var index sessionsIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return false
	}
	return sessionsIndexHasID(index, sessionID)
}

func findSessionLocationInClaudeHome(sessionID string) *sessionLocation {
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		return nil
	}
	claudeDir := filepath.Join(home, ".claude")
	resolved, evalErr := filepath.EvalSymlinks(claudeDir)
	if evalErr != nil {
		resolved = claudeDir
	}
	fallbackEntries, readErr := os.ReadDir(filepath.Join(resolved, "projects"))
	if readErr != nil {
		return nil
	}
	for _, entry := range fallbackEntries {
		if !entry.IsDir() {
			continue
		}
		sessionFile := filepath.Join(resolved, "projects", entry.Name(), sessionID+".jsonl")
		if _, statErr := os.Stat(sessionFile); statErr == nil {
			return &sessionLocation{configDir: resolved, projectDir: entry.Name()}
		}
	}
	return nil
}

// symlinkSessionToCurrentAccount finds a session in any account and symlinks
// it to the current account so Claude can access it.
// Returns a cleanup function to remove the symlink after use.
func symlinkSessionToCurrentAccount(townRoot, sessionID string) (cleanup func(), err error) {
	// Get current account's config directory (resolve ~/.claude symlink)
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("getting home directory: %w", err)
	}

	claudeDir := filepath.Join(home, ".claude")
	currentConfigDir, err := filepath.EvalSymlinks(claudeDir)
	if err != nil {
		// ~/.claude might not be a symlink, use it directly
		currentConfigDir = claudeDir
	}

	return symlinkSessionToConfigDir(townRoot, sessionID, currentConfigDir)
}

func symlinkSessionToConfigDir(townRoot, sessionID, targetConfigDir string) (cleanup func(), err error) {
	loc := findSessionLocation(townRoot, sessionID)
	if loc == nil {
		return nil, fmt.Errorf("session not found in any account")
	}
	cleanup, done, err := symlinkSessionInSameAccount(loc, sessionID, targetConfigDir)
	if done {
		return cleanup, err
	}
	return symlinkSessionAcrossAccounts(loc, sessionID, targetConfigDir)
}

func symlinkSessionInSameAccount(loc *sessionLocation, sessionID, targetConfigDir string) (func(), bool, error) {
	resolvedLocDir, _ := filepath.EvalSymlinks(loc.configDir)
	resolvedTargetDir, _ := filepath.EvalSymlinks(targetConfigDir)
	if resolvedLocDir != resolvedTargetDir {
		return nil, false, nil
	}
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		return nil, true, nil
	}
	cwdProjectDir := strings.ReplaceAll(cwd, "/", "-")
	if cwdProjectDir == loc.projectDir {
		return nil, true, nil
	}
	sourceFile := filepath.Join(targetConfigDir, "projects", loc.projectDir, sessionID+".jsonl")
	targetDir := filepath.Join(targetConfigDir, "projects", cwdProjectDir)
	if mkErr := os.MkdirAll(targetDir, 0755); mkErr != nil {
		return nil, true, nil
	}
	targetFile := filepath.Join(targetDir, sessionID+".jsonl")
	if _, lstatErr := os.Lstat(targetFile); lstatErr == nil {
		return nil, true, nil
	}
	if symlinkErr := os.Symlink(sourceFile, targetFile); symlinkErr != nil {
		return nil, true, nil
	}
	return func() { _ = os.Remove(targetFile) }, true, nil
}

func symlinkSessionAcrossAccounts(loc *sessionLocation, sessionID, targetConfigDir string) (func(), error) {
	sourceSessionFile := filepath.Join(loc.configDir, "projects", loc.projectDir, sessionID+".jsonl")
	if _, err := os.Stat(sourceSessionFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("session file not found: %s", sourceSessionFile)
	}
	currentProjectDir := filepath.Join(targetConfigDir, "projects", loc.projectDir)
	if err := os.MkdirAll(currentProjectDir, 0755); err != nil {
		return nil, fmt.Errorf("creating project directory: %w", err)
	}
	targetSessionFile := filepath.Join(currentProjectDir, sessionID+".jsonl")
	if done, err := prepareCrossAccountSessionTarget(sourceSessionFile, targetSessionFile); done {
		return nil, err
	}
	if err := os.Symlink(sourceSessionFile, targetSessionFile); err != nil {
		return nil, fmt.Errorf("creating symlink: %w", err)
	}
	indexModified, err := addSessionToTargetIndex(loc, sessionID, currentProjectDir, targetSessionFile)
	if err != nil {
		return nil, err
	}
	targetIndexPath := filepath.Join(currentProjectDir, "sessions-index.json")
	return seanceSessionCleanup(targetSessionFile, targetIndexPath, sessionID, indexModified), nil
}

func prepareCrossAccountSessionTarget(sourceSessionFile, targetSessionFile string) (bool, error) {
	info, err := os.Lstat(targetSessionFile)
	if err != nil {
		return false, nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return true, nil
	}
	existing, _ := os.Readlink(targetSessionFile)
	if existing == sourceSessionFile {
		return true, nil
	}
	_ = os.Remove(targetSessionFile)
	return false, nil
}

func addSessionToTargetIndex(loc *sessionLocation, sessionID, currentProjectDir, targetSessionFile string) (bool, error) {
	sessionEntry, err := loadSourceSessionIndexEntry(loc, sessionID, targetSessionFile)
	if err != nil {
		return false, err
	}
	targetIndexPath := filepath.Join(currentProjectDir, "sessions-index.json")
	lock, err := lockSessionsIndex(targetIndexPath)
	if err != nil {
		_ = os.Remove(targetSessionFile)
		return false, fmt.Errorf("locking sessions index: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	targetIndex := loadOrInitSessionsIndex(targetIndexPath)
	if sessionsIndexHasID(targetIndex, sessionID) {
		return false, nil
	}
	targetIndex.Entries = append(targetIndex.Entries, sessionEntry)
	targetIndexData, err := json.MarshalIndent(targetIndex, "", "  ")
	if err != nil {
		_ = os.Remove(targetSessionFile)
		return false, fmt.Errorf("encoding target sessions index: %w", err)
	}
	if err := os.WriteFile(targetIndexPath, targetIndexData, 0600); err != nil {
		_ = os.Remove(targetSessionFile)
		return false, fmt.Errorf("writing target sessions index: %w", err)
	}
	return true, nil
}

func loadSourceSessionIndexEntry(loc *sessionLocation, sessionID, targetSessionFile string) (json.RawMessage, error) {
	sourceIndexPath := filepath.Join(loc.configDir, "projects", loc.projectDir, "sessions-index.json")
	sourceIndexData, err := os.ReadFile(sourceIndexPath)
	if err != nil {
		_ = os.Remove(targetSessionFile)
		return nil, fmt.Errorf("reading source sessions index: %w", err)
	}
	var sourceIndex sessionsIndex
	if err := json.Unmarshal(sourceIndexData, &sourceIndex); err != nil {
		_ = os.Remove(targetSessionFile)
		return nil, fmt.Errorf("parsing source sessions index: %w", err)
	}
	for _, rawEntry := range sourceIndex.Entries {
		var e sessionsIndexEntry
		if json.Unmarshal(rawEntry, &e) == nil && e.SessionID == sessionID {
			return rawEntry, nil
		}
	}
	_ = os.Remove(targetSessionFile)
	return nil, fmt.Errorf("session not found in source index")
}

func loadOrInitSessionsIndex(path string) sessionsIndex {
	var targetIndex sessionsIndex
	if targetIndexData, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(targetIndexData, &targetIndex)
		return targetIndex
	}
	targetIndex.Version = 1
	return targetIndex
}

func sessionsIndexHasID(idx sessionsIndex, sessionID string) bool {
	for _, rawEntry := range idx.Entries {
		var e sessionsIndexEntry
		if json.Unmarshal(rawEntry, &e) == nil && e.SessionID == sessionID {
			return true
		}
	}
	return false
}

func seanceSessionCleanup(targetSessionFile, targetIndexPath, sessionID string, indexModified bool) func() {
	return func() {
		_ = os.Remove(targetSessionFile)
		if !indexModified {
			return
		}
		cleanupLock, lockErr := lockSessionsIndex(targetIndexPath)
		if lockErr == nil {
			defer func() { _ = cleanupLock.Unlock() }()
		}
		removeSessionFromIndexFile(targetIndexPath, sessionID)
	}
}

func removeSessionFromIndexFile(targetIndexPath, sessionID string) {
	data, err := os.ReadFile(targetIndexPath)
	if err != nil {
		return
	}
	var idx sessionsIndex
	if json.Unmarshal(data, &idx) != nil {
		return
	}
	newEntries := make([]json.RawMessage, 0, len(idx.Entries))
	for _, rawEntry := range idx.Entries {
		var e sessionsIndexEntry
		if json.Unmarshal(rawEntry, &e) == nil && e.SessionID == sessionID {
			continue
		}
		newEntries = append(newEntries, rawEntry)
	}
	idx.Entries = newEntries
	newData, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(targetIndexPath, newData, 0600)
}

// cleanupOrphanedSessionSymlinks removes stale session symlinks from the current account.
// This handles cases where a previous seance was interrupted (e.g., SIGKILL) and
// couldn't run its cleanup function. Call this at the start of seance operations.
func cleanupOrphanedSessionSymlinks() {
	projectsDir, ok := currentClaudeProjectsDir()
	if !ok {
		return
	}
	projectEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}
	for _, projEntry := range projectEntries {
		if !projEntry.IsDir() {
			continue
		}
		cleanupOrphanedProjectSymlinks(filepath.Join(projectsDir, projEntry.Name()))
	}
}

func currentClaudeProjectsDir() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	claudeDir := filepath.Join(home, ".claude")
	currentConfigDir, err := filepath.EvalSymlinks(claudeDir)
	if err != nil {
		currentConfigDir = claudeDir
	}
	projectsDir := filepath.Join(currentConfigDir, "projects")
	if _, err := os.Stat(projectsDir); os.IsNotExist(err) {
		return "", false
	}
	return projectsDir, true
}

func cleanupOrphanedProjectSymlinks(projPath string) {
	orphanedSessionIDs := collectOrphanedSessionIDs(projPath)
	if len(orphanedSessionIDs) == 0 {
		return
	}
	removeOrphanedIndexEntries(filepath.Join(projPath, "sessions-index.json"), orphanedSessionIDs)
}

func collectOrphanedSessionIDs(projPath string) []string {
	files, err := os.ReadDir(projPath)
	if err != nil {
		return nil
	}
	var orphanedSessionIDs []string
	for _, f := range files {
		if sessionID, ok := orphanedSessionSymlinkID(filepath.Join(projPath, f.Name()), f.Name()); ok {
			orphanedSessionIDs = append(orphanedSessionIDs, sessionID)
		}
	}
	return orphanedSessionIDs
}

func orphanedSessionSymlinkID(filePath, name string) (string, bool) {
	if !strings.HasSuffix(name, ".jsonl") {
		return "", false
	}
	info, err := os.Lstat(filePath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	target, err := os.Readlink(filePath)
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		return "", false
	}
	_ = os.Remove(filePath)
	return strings.TrimSuffix(name, ".jsonl"), true
}

func removeOrphanedIndexEntries(indexPath string, orphanedSessionIDs []string) {
	lock, lockErr := lockSessionsIndex(indexPath)
	if lockErr != nil {
		return
	}
	defer func() { _ = lock.Unlock() }()
	index, ok := readSessionsIndexFile(indexPath)
	if !ok {
		return
	}
	newEntries := filterOrphanedIndexEntries(index.Entries, orphanedSessionIDs)
	if len(newEntries) == len(index.Entries) {
		return
	}
	index.Entries = newEntries
	writeSessionsIndexFile(indexPath, index)
}

func readSessionsIndexFile(indexPath string) (sessionsIndex, bool) {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return sessionsIndex{}, false
	}
	var index sessionsIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return sessionsIndex{}, false
	}
	return index, true
}

func filterOrphanedIndexEntries(entries []json.RawMessage, orphanedSessionIDs []string) []json.RawMessage {
	orphanedSet := make(map[string]bool, len(orphanedSessionIDs))
	for _, id := range orphanedSessionIDs {
		orphanedSet[id] = true
	}
	newEntries := make([]json.RawMessage, 0, len(entries))
	for _, rawEntry := range entries {
		var e sessionsIndexEntry
		if json.Unmarshal(rawEntry, &e) == nil && !orphanedSet[e.SessionID] {
			newEntries = append(newEntries, rawEntry)
		}
	}
	return newEntries
}

func writeSessionsIndexFile(indexPath string, index sessionsIndex) {
	newData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(indexPath, newData, 0600)
}
