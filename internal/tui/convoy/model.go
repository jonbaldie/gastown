package convoy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"sync"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/util"
)

// convoyIDPattern validates convoy IDs.
var convoyIDPattern = regexp.MustCompile(`^hq-[a-zA-Z0-9-]+$`)

// IssueItem represents a tracked issue within a convoy.
type IssueItem struct {
	ID     string
	Title  string
	Status string
}

// ConvoyItem represents a convoy with its tracked issues.
type ConvoyItem struct {
	ID       string
	Title    string
	Status   string
	Issues   []IssueItem
	Progress string // e.g., "2/5"
	Expanded bool
}

// Model is the bubbletea model for the convoy TUI.
type Model struct {
	convoyState
	convoyLoader
	convoyViewState

	// mu protects all fields read by View() from concurrent access:
	// convoys, cursor, err, showHelp, help, width, height.
	// Write lock is held during Update mutations; read lock during View/render.
	mu sync.RWMutex
}

type convoyState struct {
	convoys []ConvoyItem
	cursor  int // Current selection index in flattened view
	err     error
}

type convoyLoader struct {
	townBeads string // Path to town beads directory
}

type convoyViewState struct {
	// UI state
	keys     KeyMap
	help     help.Model
	showHelp bool
	width    int
	height   int
}

// New creates a new convoy TUI model.
func New(townBeads string) *Model {
	return &Model{
		convoyState: convoyState{
			convoys: make([]ConvoyItem, 0),
		},
		convoyLoader: convoyLoader{
			townBeads: townBeads,
		},
		convoyViewState: convoyViewState{
			keys: DefaultKeyMap(),
			help: help.New(),
		},
	}
}

// Init initializes the model.
func (m *Model) Init() tea.Cmd {
	return m.fetchConvoys
}

// fetchConvoysMsg is the result of fetching convoys.
type fetchConvoysMsg struct {
	convoys []ConvoyItem
	err     error
}

// fetchConvoys fetches convoy data from beads.
func (l *convoyLoader) fetchConvoys() tea.Msg {
	convoys, err := loadConvoys(l.townBeads)
	return fetchConvoysMsg{convoys: convoys, err: err}
}

// loadConvoys loads convoy data from the beads directory.
func loadConvoys(townBeads string) ([]ConvoyItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), constants.BdSubprocessTimeout)
	defer cancel()

	// Get list of open issues and filter locally so legacy type=convoy beads remain visible.
	listArgs := []string{"list", "--json", "--limit=0"}
	listCmd := beads.SpawnContext(ctx, listArgs...)
	util.SetDetachedProcessGroup(listCmd)
	listCmd.Dir = townBeads
	var stdout bytes.Buffer
	listCmd.Stdout = &stdout

	if err := listCmd.Run(); err != nil {
		return nil, fmt.Errorf("listing convoys: %w", err)
	}

	var rawConvoys []struct {
		ID        string   `json:"id"`
		Title     string   `json:"title"`
		Status    string   `json:"status"`
		IssueType string   `json:"issue_type"`
		Labels    []string `json:"labels"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &rawConvoys); err != nil {
		return nil, fmt.Errorf("parsing convoy list: %w", err)
	}

	convoys := make([]ConvoyItem, 0, len(rawConvoys))
	for _, rc := range rawConvoys {
		if rc.IssueType != "convoy" && !tuiConvoyHasLabel(rc.Labels, "gt:convoy") {
			continue
		}
		issues, completed, total := loadTrackedIssues(townBeads, rc.ID)
		convoys = append(convoys, ConvoyItem{
			ID:       rc.ID,
			Title:    rc.Title,
			Status:   rc.Status,
			Issues:   issues,
			Progress: fmt.Sprintf("%d/%d", completed, total),
			Expanded: false,
		})
	}

	return convoys, nil
}

func tuiConvoyHasLabel(labels []string, target string) bool {
	for _, label := range labels {
		if label == target {
			return true
		}
	}
	return false
}

// loadTrackedIssues loads issues tracked by a convoy.
func loadTrackedIssues(townBeads, convoyID string) ([]IssueItem, int, int) {
	// Validate convoy ID for safety
	if !convoyIDPattern.MatchString(convoyID) {
		return nil, 0, 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), constants.BdSubprocessTimeout)
	defer cancel()

	// Query tracked issues using bd dep list (returns full issue details)
	cmd := beads.SpawnContext(ctx, "dep", "list", convoyID, "-t", "tracks", "--json")
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = townBeads
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return nil, 0, 0
	}

	var tracked []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &tracked); err != nil {
		return nil, 0, 0
	}

	// Extract raw issue IDs and refresh status via cross-rig lookup.
	// bd dep list returns status from the dependency record in HQ beads
	// which is never updated when cross-rig issues are closed in their rig.
	for i := range tracked {
		tracked[i].ID = beads.ExtractIssueID(tracked[i].ID)
	}
	freshStatus := refreshIssueStatus(ctx, tracked)

	issues := make([]IssueItem, 0, len(tracked))
	completed := 0
	for _, t := range tracked {
		status := t.Status
		if fresh, ok := freshStatus[t.ID]; ok {
			status = fresh
		}
		issues = append(issues, IssueItem{
			ID:     t.ID,
			Title:  t.Title,
			Status: status,
		})
		if status == "closed" {
			completed++
		}
	}

	// Sort by status (open first, then closed)
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Status == issues[j].Status {
			return issues[i].ID < issues[j].ID
		}
		return issues[i].Status != "closed" // open comes first
	})

	return issues, completed, len(issues)
}

// refreshIssueStatus does a batch bd show to get current status for tracked issues.
// Returns a map from issue ID to current status.
func refreshIssueStatus(ctx context.Context, tracked []struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}) map[string]string {
	if len(tracked) == 0 {
		return nil
	}

	args := []string{"show"}
	for _, t := range tracked {
		args = append(args, t.ID)
	}
	args = append(args, "--json")

	cmd := beads.SpawnContext(ctx, args...)
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return nil
	}

	var issues []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &issues); err != nil {
		return nil
	}

	result := make(map[string]string, len(issues))
	for _, issue := range issues {
		result[issue.ID] = issue.Status
	}
	return result
}

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.updateWindowSize(msg)
		return m, nil

	case fetchConvoysMsg:
		m.updateConvoys(msg)
		return m, nil

	case tea.KeyMsg:
		return m, m.updateKey(msg)
	}

	return m, nil
}

func (m *Model) updateWindowSize(msg tea.WindowSizeMsg) {
	m.mu.Lock()
	m.width = msg.Width
	m.height = msg.Height
	m.help.Width = msg.Width
	m.mu.Unlock()
}

func (m *Model) updateConvoys(msg fetchConvoysMsg) {
	m.mu.Lock()
	m.err = msg.err
	m.convoys = msg.convoys
	m.mu.Unlock()
}

func (m *Model) updateKey(msg tea.KeyMsg) tea.Cmd {
	handlers := []struct {
		matches bool
		handle  func() tea.Cmd
	}{
		{key.Matches(msg, m.keys.Quit), func() tea.Cmd { return tea.Quit }},
		{key.Matches(msg, m.keys.Help), m.toggleHelp},
		{key.Matches(msg, m.keys.Up), func() tea.Cmd {
			m.moveCursor(-1)
			return nil
		}},
		{key.Matches(msg, m.keys.Down), func() tea.Cmd {
			m.moveCursor(1)
			return nil
		}},
		{key.Matches(msg, m.keys.Top), func() tea.Cmd {
			m.setCursor(0)
			return nil
		}},
		{key.Matches(msg, m.keys.Bottom), m.moveCursorToBottom},
		{key.Matches(msg, m.keys.Toggle), m.toggleExpand},
		{isConvoyNumberKey(msg), func() tea.Cmd {
			m.jumpToConvoy(numberKeyIndex(msg))
			return nil
		}},
	}
	for _, handler := range handlers {
		if handler.matches {
			return handler.handle()
		}
	}
	return nil
}

func (m *Model) toggleHelp() tea.Cmd {
	m.mu.Lock()
	m.showHelp = !m.showHelp
	m.mu.Unlock()
	return nil
}

func (m *Model) moveCursor(delta int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if delta < 0 && m.cursor > 0 {
		m.cursor--
		return
	}
	if delta > 0 && m.cursor < m.maxCursorLocked() {
		m.cursor++
	}
}

func (m *Model) setCursor(cursor int) {
	m.mu.Lock()
	m.cursor = cursor
	m.mu.Unlock()
}

func (m *Model) moveCursorToBottom() tea.Cmd {
	m.mu.Lock()
	m.cursor = m.maxCursorLocked()
	m.mu.Unlock()
	return nil
}

func (m *Model) toggleExpand() tea.Cmd {
	m.mu.Lock()
	m.toggleExpandLocked()
	m.mu.Unlock()
	return nil
}

func isConvoyNumberKey(msg tea.KeyMsg) bool {
	value := msg.String()
	return value >= "1" && value <= "9"
}

func numberKeyIndex(msg tea.KeyMsg) int {
	return int(msg.String()[0] - '1')
}

func (m *Model) jumpToConvoy(convoyIndex int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if convoyIndex < len(m.convoys) {
		m.jumpToConvoyLocked(convoyIndex)
	}
}

// maxCursorLocked returns the maximum valid cursor position.
// Caller must hold m.mu (read or write).
func (s *convoyState) maxCursorLocked() int {
	count := 0
	for _, c := range s.convoys {
		count++ // convoy itself
		if c.Expanded {
			count += len(c.Issues)
		}
	}
	if count == 0 {
		return 0
	}
	return count - 1
}

// cursorToConvoyIndexLocked returns the convoy index and issue index for the current cursor.
// Returns (convoyIdx, issueIdx) where issueIdx is -1 if on a convoy row.
// Caller must hold m.mu (read or write).
func (s *convoyState) cursorToConvoyIndexLocked() (int, int) {
	pos := 0
	for ci, c := range s.convoys {
		if pos == s.cursor {
			return ci, -1
		}
		pos++
		if c.Expanded {
			for ii := range c.Issues {
				if pos == s.cursor {
					return ci, ii
				}
				pos++
			}
		}
	}
	return -1, -1
}

// toggleExpandLocked toggles expansion of the convoy at the current cursor.
// Caller must hold m.mu write lock.
func (s *convoyState) toggleExpandLocked() {
	ci, ii := s.cursorToConvoyIndexLocked()
	if ci >= 0 && ii == -1 {
		// On a convoy row, toggle it
		s.convoys[ci].Expanded = !s.convoys[ci].Expanded
	}
}

// jumpToConvoyLocked moves the cursor to a specific convoy by index.
// Caller must hold m.mu write lock.
func (s *convoyState) jumpToConvoyLocked(convoyIdx int) {
	if convoyIdx < 0 || convoyIdx >= len(s.convoys) {
		return
	}
	pos := 0
	for ci, c := range s.convoys {
		if ci == convoyIdx {
			s.cursor = pos
			return
		}
		pos++
		if c.Expanded {
			pos += len(c.Issues)
		}
	}
}

// View renders the model.
// Acquires read lock to safely access all View-visible fields.
func (m *Model) View() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.renderView()
}
