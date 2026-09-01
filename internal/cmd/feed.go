package cmd

import (
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/tui/feed"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func init() {
	rootCmd.AddCommand(feedCmd)

	feedCmd.Flags().BoolP("follow", "f", false, "Stream events in real-time (default when no other flags)")
	feedCmd.Flags().Bool("no-follow", false, "Show events once and exit")
	feedCmd.Flags().IntP("limit", "n", 100, "Maximum number of events to show")
	feedCmd.Flags().String("since", "", "Show events since duration (e.g., 5m, 1h, 30s)")
	feedCmd.Flags().String("mol", "", "Filter by molecule/issue ID prefix")
	feedCmd.Flags().String("type", "", "Filter by event type (create, update, delete, comment)")
	feedCmd.Flags().String("rig", "", "Filter events by rig name")
	feedCmd.Flags().BoolP("window", "w", false, "Open in dedicated tmux window (creates 'feed' window)")
	feedCmd.Flags().Bool("plain", false, "Use plain text output (bd activity) instead of TUI")
	feedCmd.Flags().BoolP("problems", "p", false, "Start in problems view (shows stuck agents)")
}

var feedCmd = &cobra.Command{
	Use:     "feed",
	GroupID: GroupDiag,
	Short:   "Show real-time activity feed of gt events",
	Long: `Display a real-time feed of issue changes and agent activity.

By default, launches an interactive TUI dashboard with:
  - Agent tree (top): Shows all agents organized by role with latest activity
  - Convoy panel (middle): Shows in-progress and recently landed convoys
  - Event stream (bottom): Chronological feed you can scroll through
  - Vim-style navigation: j/k to scroll, tab to switch panels, 1/2/3 for panels, q to quit

Problems View (--problems/-p):
  A problem-first view that surfaces agents needing attention:
  - Detects stuck agents via structured beads data (hook state, timestamps)
  - Shows GUPP violations (hooked work + 30m no progress)
  - Keyboard actions: Enter=attach, n=nudge, h=handoff
  - Press 'p' to toggle between activity and problems view

The feed combines multiple event sources:
  - GT events: Agent activity like patrol, sling, handoff (from .events.jsonl)
  - Beads activity: Issue creates, updates, completions (from bd activity, when available)
  - Convoy status: In-progress and recently-landed convoys (refreshes every 10s)

Use --plain for simple text output (reads .events.jsonl directly).

Tmux Integration:
  Use --window to open the feed in a dedicated tmux window named 'feed'.
  This creates a persistent window you can cycle to with C-b n/p.

Event symbols:
  +  created/bonded    - New issue or molecule created
  →  in_progress       - Work started on an issue
  ✓  completed         - Issue closed or step completed
  ✗  failed            - Step or issue failed
  ⊘  deleted           - Issue removed
  🦉  patrol_started   - Witness began patrol cycle
  ⚡  polecat_nudged   - Worker was nudged
  🎯  sling            - Work was slung to worker
  🤝  handoff          - Session handed off

Agent state symbols (problems view):
  🔥  GUPP violation   - Hooked work + 30m no progress (critical)
  ⚠   STALLED          - Hooked work + 15m no progress
  ●   Working          - Actively producing output
  ○   Idle             - No hooked work
  💀  Zombie           - Dead/crashed session

MQ (Merge Queue) event symbols:
  ⚙  merge_started   - Refinery began processing an MR
  ✓  merged          - MR successfully merged (green)
  ✗  merge_failed    - Merge failed (conflict, tests, etc.) (red)
  ⊘  merge_skipped   - MR skipped (already merged, etc.)

Examples:
  gt feed                       # Launch TUI dashboard
  gt feed --problems            # Start in problems view
  gt feed -p                    # Short flag for problems view
  gt feed --plain               # Plain text output (bd activity)
  gt feed --window              # Open in dedicated tmux window
  gt feed --since 1h            # Events from last hour
  gt feed --rig greenplace      # Use gastown rig's beads`,
	RunE: runFeed,
}

type feedOptions struct {
	follow   bool
	limit    int
	since    string
	mol      string
	typeName string
	rig      string
	noFollow bool
	window   bool
	plain    bool
	problems bool
}

func runFeed(cmd *cobra.Command, _ []string) error {
	opts := feedOptions{
		follow:   commandBoolFlag(cmd, "follow"),
		limit:    commandIntFlag(cmd, "limit"),
		since:    commandStringFlag(cmd, "since"),
		mol:      commandStringFlag(cmd, "mol"),
		typeName: commandStringFlag(cmd, "type"),
		rig:      commandStringFlag(cmd, "rig"),
		noFollow: commandBoolFlag(cmd, "no-follow"),
		window:   commandBoolFlag(cmd, "window"),
		plain:    commandBoolFlag(cmd, "plain"),
		problems: commandBoolFlag(cmd, "problems"),
	}

	// Must be in a Gas Town workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace (run from ~/gt or a rig directory)")
	}

	// Build feed arguments for window mode
	bdArgs := buildFeedArgs(opts)

	// Handle --window mode: --rig is forwarded as a CLI flag via buildFeedArgs
	if opts.window {
		workDir, err := currentFeedWorkDir()
		if err != nil {
			return err
		}
		return runFeedInWindow(workDir, bdArgs)
	}

	// Use TUI by default if running in a terminal and not --plain
	useTUI := !opts.plain && term.IsTerminal(int(os.Stdout.Fd()))

	if useTUI {
		// TUI mode: resolve --rig to a beads directory for BdActivitySource
		workDir, err := resolveFeedTUIWorkDir(townRoot, opts.rig)
		if err != nil {
			return err
		}
		return runFeedTUI(workDir, opts.problems)
	}

	// Plain mode: --rig is a pure event filter via PrintOptions.Rig
	return runFeedDirect(townRoot, opts)
}

func currentFeedWorkDir() (string, error) {
	workDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting current directory: %w", err)
	}
	return workDir, nil
}

func resolveFeedTUIWorkDir(townRoot, rigName string) (string, error) {
	workDir, err := currentFeedWorkDir()
	if err != nil {
		return "", err
	}
	if rigName == "" {
		return workDir, nil
	}
	candidates := []string{
		fmt.Sprintf("%s/%s/mayor/rig", townRoot, rigName),
		fmt.Sprintf("%s/%s", townRoot, rigName),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate + "/.beads"); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("rig '%s' not found or has no .beads directory", rigName)
}

// buildFeedArgs builds the feed CLI arguments for window mode.
func buildFeedArgs(opts feedOptions) []string {
	args := feedFollowArgs(opts)
	return append(args, feedFilterArgs(opts)...)
}

func feedFollowArgs(opts feedOptions) []string {
	if feedShouldFollow(opts) {
		return []string{"--follow"}
	}
	return nil
}

func feedShouldFollow(opts feedOptions) bool {
	if opts.follow {
		return true
	}
	if opts.noFollow {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func feedFilterArgs(opts feedOptions) []string {
	var args []string
	if opts.limit != 100 {
		args = append(args, "--limit", fmt.Sprintf("%d", opts.limit))
	}

	if opts.since != "" {
		args = append(args, "--since", opts.since)
	}

	if opts.mol != "" {
		args = append(args, "--mol", opts.mol)
	}

	if opts.typeName != "" {
		args = append(args, "--type", opts.typeName)
	}

	if opts.rig != "" {
		args = append(args, "--rig", opts.rig)
	}

	return args
}

// runFeedDirect prints events from .events.jsonl to stdout.
// Supports --follow for tailing, and --since/--mol/--type for filtering.
// townRoot is the resolved workspace root (incorporates --rig if set).
func runFeedDirect(townRoot string, opts feedOptions) error {
	// Determine follow behavior:
	// - Explicit --follow: always follow
	// - Explicit --no-follow: never follow
	// - Non-TTY (pipe/script): no follow unless explicitly requested
	// - Default (TTY, no flags): follow
	shouldFollow := opts.follow
	if !shouldFollow && !opts.noFollow {
		shouldFollow = term.IsTerminal(int(os.Stdout.Fd()))
	}

	printOpts := feed.PrintOptions{
		Limit:  opts.limit,
		Follow: shouldFollow,
		Since:  opts.since,
		Mol:    opts.mol,
		Type:   opts.typeName,
		Rig:    opts.rig,
	}

	return feed.PrintGtEvents(townRoot, printOpts)
}

// runFeedTUI runs the interactive TUI feed.
func runFeedTUI(workDir string, problemsView bool) error {
	// Must be in a Gas Town workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	var sources []feed.EventSource

	// Create event source from bd activity (optional - bd may not have activity command)
	bdSource, err := feed.NewBdActivitySource(workDir)
	if err == nil {
		sources = append(sources, bdSource)
	}

	// Create MQ event source (optional - don't fail if not available)
	mqSource, err := feed.NewMQEventSourceFromWorkDir(workDir)
	if err == nil {
		sources = append(sources, mqSource)
	}

	// Create GT events source (optional - don't fail if not available)
	gtSource, err := feed.NewGtEventsSource(townRoot)
	if err == nil {
		sources = append(sources, gtSource)
	}

	if len(sources) == 0 {
		return fmt.Errorf("no event sources available (check that .events.jsonl exists in %s)", townRoot)
	}

	// Combine all sources
	multiSource := feed.NewMultiSource(sources...)
	defer func() { _ = multiSource.Close() }()

	// Create beads instance for agent health detection
	bd := beads.New(townRoot)

	// Create model and connect event source
	var m *feed.Model
	if problemsView {
		m = feed.NewModelWithProblemsView(bd)
	} else {
		m = feed.NewModel(bd)
	}
	m.SetEventChannel(multiSource.Events())
	m.SetTownRoot(townRoot)

	// Run the TUI
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("running TUI: %w", err)
	}

	return nil
}

// runFeedInWindow opens the feed in a dedicated tmux window.
func runFeedInWindow(workDir string, bdArgs []string) error {
	// Check if we're in tmux
	if !tmux.IsInsideTmux() {
		return fmt.Errorf("--window requires running inside tmux")
	}

	// Get current session from TMUX env var
	// Format: /tmp/tmux-501/default,12345,0 -> we need the session name
	tmuxEnv := os.Getenv("TMUX")
	if tmuxEnv == "" {
		return fmt.Errorf("TMUX environment variable not set")
	}

	t := tmux.NewTmux()

	// Get current session name
	sessionName, err := getCurrentTmuxSession()
	if err != nil {
		return fmt.Errorf("getting current session: %w", err)
	}

	feedWindowCmd := buildFeedWindowCommand(workDir, bdArgs)

	windowTarget := sessionName + ":feed"
	return openFeedWindow(t, sessionName, windowTarget, workDir, feedWindowCmd)
}

func buildFeedWindowCommand(workDir string, bdArgs []string) string {
	gtPath, err := os.Executable()
	if err != nil {
		gtPath = "gt"
	}
	base := fmt.Sprintf("cd \"%s\" && \"%s\" feed --plain --follow", workDir, gtPath)
	filteredArgs := filterFeedWindowArgs(bdArgs)
	if len(filteredArgs) == 0 {
		return base
	}
	return fmt.Sprintf("%s %s", base, strings.Join(filteredArgs, " "))
}

func filterFeedWindowArgs(bdArgs []string) []string {
	var filtered []string
	for _, arg := range bdArgs {
		if arg != "--follow" {
			filtered = append(filtered, arg)
		}
	}
	return filtered
}

func openFeedWindow(t *tmux.Tmux, sessionName, windowTarget, workDir, feedWindowCmd string) error {
	exists, err := windowExists(t, sessionName, "feed")
	if err != nil {
		return fmt.Errorf("checking for feed window: %w", err)
	}
	if exists {
		fmt.Printf("Switching to existing feed window...\n")
		return selectWindow(t, windowTarget)
	}
	fmt.Printf("Creating feed window in session %s...\n", sessionName)
	if err := createWindow(t, sessionName, "feed", workDir, feedWindowCmd); err != nil {
		return fmt.Errorf("creating feed window: %w", err)
	}
	return selectWindow(t, windowTarget)
}

// windowExists checks if a window with the given name exists in the session.
// Note: getCurrentTmuxSession is defined in handoff.go
func windowExists(_ *tmux.Tmux, session, windowName string) (bool, error) { // t unused: direct exec for simplicity
	cmd := tmux.BuildCommand("list-windows", "-t", session, "-F", "#{window_name}")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}

	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == windowName {
			return true, nil
		}
	}
	return false, nil
}

// createWindow creates a new tmux window with the given name and command.
func createWindow(_ *tmux.Tmux, session, windowName, workDir, command string) error { // t unused: direct exec for simplicity
	args := []string{"new-window", "-t", session, "-n", windowName, "-c", workDir, command}
	cmd := tmux.BuildCommand(args...)
	return cmd.Run()
}

// selectWindow switches to the specified window.
func selectWindow(_ *tmux.Tmux, target string) error { // t unused: direct exec for simplicity
	cmd := tmux.BuildCommand("select-window", "-t", target)
	return cmd.Run()
}
