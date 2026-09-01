package deacon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/dog"
	"github.com/jonbaldie/gastown/internal/util"
)

// Default parameters for feed-stranded rate limiting.
// Configurable via operational.deacon.max_feeds_per_cycle and
// operational.deacon.feed_cooldown in settings/config.json.
const (
	// DefaultMaxFeedsPerCycle is the maximum number of convoys to feed in one invocation.
	// Prevents spawning too many dogs at once.
	DefaultMaxFeedsPerCycle = 3

	// DefaultFeedCooldown is the minimum time between feeding the same convoy.
	// Prevents re-dispatching a dog before the previous one finishes.
	DefaultFeedCooldown = 10 * time.Minute
)

// FeedStrandedState tracks feeding attempts per convoy.
// Persisted to deacon/feed-stranded-state.json.
type FeedStrandedState struct {
	// Convoys maps convoy ID to their feed tracking state.
	Convoys map[string]*ConvoyFeedState `json:"convoys"`

	// LastUpdated is when this state was last written.
	LastUpdated time.Time `json:"last_updated"`
}

// ConvoyFeedState tracks the feed history for a single convoy.
type ConvoyFeedState struct {
	// ConvoyID is the convoy identifier.
	ConvoyID string `json:"convoy_id"`

	// FeedCount is total number of feed dispatches for this convoy.
	FeedCount int `json:"feed_count"`

	// LastFeedTime is when the last feed was dispatched.
	LastFeedTime time.Time `json:"last_feed_time,omitempty"`
}

// StrandedConvoy holds info about a stranded convoy from `gt convoy stranded --json`.
type StrandedConvoy struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	TrackedCount int      `json:"tracked_count"`
	ReadyCount   int      `json:"ready_count"`
	ReadyIssues  []string `json:"ready_issues"`
}

// FeedResult describes the outcome of a feed-stranded invocation.
type FeedResult struct {
	// Fed is the number of convoys dispatched to dogs for feeding.
	Fed int `json:"fed"`

	// Closed is the number of empty convoys auto-closed.
	Closed int `json:"closed"`

	// Skipped is the number of convoys skipped (cooldown).
	Skipped int `json:"skipped"`

	// NeedsAttention is the number of convoys with tracked issues but no ready
	// issues. These require agent judgment — Go surfaces the raw data but does
	// not classify or act on them.
	NeedsAttention int `json:"needs_attention"`

	// Errors is the number of convoys that failed to process.
	Errors int `json:"errors"`

	// Details has per-convoy results.
	Details []FeedConvoyResult `json:"details"`
}

// FeedConvoyResult describes the outcome for a single convoy.
type FeedConvoyResult struct {
	ConvoyID     string `json:"convoy_id"`
	Action       string `json:"action"` // "fed", "closed", "cooldown", "error", "limit", "needs_attention", "rejected", "blocked"
	Message      string `json:"message"`
	TrackedCount int    `json:"tracked_count,omitempty"` // Raw data for agent inspection
	ReadyCount   int    `json:"ready_count,omitempty"`   // Raw data for agent inspection
}

// FeedStrandedStateFile returns the path to the feed-stranded state file.
func FeedStrandedStateFile(townRoot string) string {
	return filepath.Join(townRoot, "deacon", "feed-stranded-state.json")
}

// LoadFeedStrandedState loads the feed-stranded state from disk.
// Returns empty state if file doesn't exist.
func LoadFeedStrandedState(townRoot string) (*FeedStrandedState, error) {
	stateFile := FeedStrandedStateFile(townRoot)

	data, err := os.ReadFile(stateFile) //nolint:gosec // G304: path is constructed from trusted townRoot
	if err != nil {
		if os.IsNotExist(err) {
			return &FeedStrandedState{
				Convoys: make(map[string]*ConvoyFeedState),
			}, nil
		}
		return nil, fmt.Errorf("reading feed-stranded state: %w", err)
	}

	var state FeedStrandedState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing feed-stranded state: %w", err)
	}

	if state.Convoys == nil {
		state.Convoys = make(map[string]*ConvoyFeedState)
	}

	return &state, nil
}

// SaveFeedStrandedState saves the feed-stranded state to disk.
func SaveFeedStrandedState(townRoot string, state *FeedStrandedState) error {
	stateFile := FeedStrandedStateFile(townRoot)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		return fmt.Errorf("creating deacon directory: %w", err)
	}

	state.LastUpdated = time.Now().UTC()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling feed-stranded state: %w", err)
	}

	return os.WriteFile(stateFile, data, 0600)
}

// GetConvoyState returns the feed state for a convoy, creating if needed.
func (s *FeedStrandedState) GetConvoyState(convoyID string) *ConvoyFeedState {
	if s.Convoys == nil {
		s.Convoys = make(map[string]*ConvoyFeedState)
	}

	state, ok := s.Convoys[convoyID]
	if !ok {
		state = &ConvoyFeedState{ConvoyID: convoyID}
		s.Convoys[convoyID] = state
	}
	return state
}

// IsInCooldown returns true if the convoy was recently fed.
func (s *ConvoyFeedState) IsInCooldown(cooldown time.Duration) bool {
	if s.LastFeedTime.IsZero() {
		return false
	}
	return time.Since(s.LastFeedTime) < cooldown
}

// CooldownRemaining returns how long until cooldown expires.
func (s *ConvoyFeedState) CooldownRemaining(cooldown time.Duration) time.Duration {
	if s.LastFeedTime.IsZero() {
		return 0
	}
	remaining := cooldown - time.Since(s.LastFeedTime)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// RecordFeed records a feed dispatch for the convoy.
func (s *ConvoyFeedState) RecordFeed() {
	s.FeedCount++
	s.LastFeedTime = time.Now().UTC()
}

// FindStrandedConvoys runs `gt convoy stranded --json` and parses the output.
func FindStrandedConvoys(townRoot string) ([]StrandedConvoy, error) {
	cmd := exec.Command("gt", "convoy", "stranded", "--json")
	cmd.Dir = townRoot
	cmd.Env = deaconReadOnlyRoutingEnv(townRoot)
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running gt convoy stranded: %w", err)
	}

	var stranded []StrandedConvoy
	if err := json.Unmarshal(output, &stranded); err != nil {
		return nil, fmt.Errorf("parsing stranded convoys: %w", err)
	}

	return stranded, nil
}

var findStrandedConvoys = FindStrandedConvoys

// FeedStranded detects stranded convoys and takes mechanical actions where safe.
// Empty convoys (0 tracked) are auto-closed. Feedable convoys get a dog dispatched.
// Convoys with tracked-but-not-ready issues are surfaced as "needs_attention" with
// raw data (tracked_count, ready_count) for the deacon agent to inspect and decide.
// Rate limits by maxPerCycle and per-convoy cooldown.
func FeedStranded(townRoot string, maxPerCycle int, cooldown time.Duration) *FeedResult {
	result := &FeedResult{}
	if err := dog.RequireDispatchAllowed(townRoot); err != nil {
		appendFeedError(result, "", "blocked", fmt.Sprintf("guardian blocked dog dispatch: %v", err))
		return result
	}
	if maxPerCycle <= 0 {
		maxPerCycle = DefaultMaxFeedsPerCycle
	}
	if cooldown <= 0 {
		cooldown = DefaultFeedCooldown
	}
	stranded, state, ok := loadFeedStrandedWork(townRoot, result)
	if !ok {
		return result
	}
	fedCount := 0
	for _, convoy := range stranded {
		fedCount += processStrandedConvoy(townRoot, convoy, state, result, fedCount, maxPerCycle, cooldown)
	}
	if err := SaveFeedStrandedState(townRoot, state); err != nil {
		result.Details = append(result.Details, FeedConvoyResult{
			Action:  "error",
			Message: fmt.Sprintf("warning: failed to save feed state: %v", err),
		})
	}
	return result
}

func loadFeedStrandedWork(townRoot string, result *FeedResult) ([]StrandedConvoy, *FeedStrandedState, bool) {
	stranded, err := findStrandedConvoys(townRoot)
	if err != nil {
		appendFeedError(result, "", "error", fmt.Sprintf("failed to find stranded convoys: %v", err))
		return nil, nil, false
	}
	if len(stranded) == 0 {
		return nil, nil, false
	}
	state, err := LoadFeedStrandedState(townRoot)
	if err != nil {
		appendFeedError(result, "", "error", fmt.Sprintf("failed to load feed state: %v", err))
		return nil, nil, false
	}
	return stranded, state, true
}

func processStrandedConvoy(townRoot string, convoy StrandedConvoy, state *FeedStrandedState, result *FeedResult, fedCount, maxPerCycle int, cooldown time.Duration) int {
	if convoy.ReadyCount == 0 {
		handleUnreadyStrandedConvoy(townRoot, convoy, result)
		return 0
	}
	if skipStrandedFeed(convoy, state, result, fedCount, maxPerCycle, cooldown) {
		return 0
	}
	if !admitAndDispatchFeed(townRoot, convoy, result) {
		return 0
	}
	state.GetConvoyState(convoy.ID).RecordFeed()
	result.Fed++
	result.Details = append(result.Details, FeedConvoyResult{
		ConvoyID: convoy.ID,
		Action:   "fed",
		Message:  fmt.Sprintf("dispatched dog to feed (%d ready issues)", convoy.ReadyCount),
	})
	return 1
}

func handleUnreadyStrandedConvoy(townRoot string, convoy StrandedConvoy, result *FeedResult) {
	if convoy.TrackedCount > 0 {
		result.NeedsAttention++
		result.Details = append(result.Details, FeedConvoyResult{
			ConvoyID:     convoy.ID,
			Action:       "needs_attention",
			Message:      fmt.Sprintf("%d tracked issues, 0 ready — requires agent review", convoy.TrackedCount),
			TrackedCount: convoy.TrackedCount,
			ReadyCount:   0,
		})
		return
	}
	if err := dog.RequireActivationAllowed(townRoot); err != nil {
		appendFeedError(result, convoy.ID, "blocked", fmt.Sprintf("guardian blocked empty-convoy close: %v", err))
		return
	}
	if err := closeEmptyConvoy(townRoot, convoy.ID); err != nil {
		appendFeedError(result, convoy.ID, "error", fmt.Sprintf("failed to auto-close empty convoy: %v", err))
		return
	}
	result.Closed++
	result.Details = append(result.Details, FeedConvoyResult{
		ConvoyID: convoy.ID,
		Action:   "closed",
		Message:  "auto-closed empty convoy (0 tracked issues)",
	})
}

func skipStrandedFeed(convoy StrandedConvoy, state *FeedStrandedState, result *FeedResult, fedCount, maxPerCycle int, cooldown time.Duration) bool {
	if fedCount >= maxPerCycle {
		result.Details = append(result.Details, FeedConvoyResult{
			ConvoyID: convoy.ID,
			Action:   "limit",
			Message:  fmt.Sprintf("skipped: per-cycle limit reached (%d/%d)", fedCount, maxPerCycle),
		})
		return true
	}
	convoyState := state.GetConvoyState(convoy.ID)
	if !convoyState.IsInCooldown(cooldown) {
		return false
	}
	result.Skipped++
	result.Details = append(result.Details, FeedConvoyResult{
		ConvoyID: convoy.ID,
		Action:   "cooldown",
		Message:  fmt.Sprintf("in cooldown (remaining: %s)", convoyState.CooldownRemaining(cooldown).Round(time.Second)),
	})
	return true
}

func admitAndDispatchFeed(townRoot string, convoy StrandedConvoy, result *FeedResult) bool {
	ok, reason, err := admitConvoyFeed(townRoot, convoy.ID)
	if err != nil {
		appendFeedError(result, convoy.ID, "error", fmt.Sprintf("failed to admit convoy feed: %v", err))
		return false
	}
	if !ok {
		result.Details = append(result.Details, FeedConvoyResult{
			ConvoyID: convoy.ID,
			Action:   "rejected",
			Message:  reason,
		})
		return false
	}
	if err := dispatchFeedDog(townRoot, convoy.ID); err != nil {
		appendFeedError(result, convoy.ID, "error", fmt.Sprintf("failed to dispatch feed dog: %v", err))
		return false
	}
	return true
}

func appendFeedError(result *FeedResult, convoyID, action, message string) {
	result.Errors++
	result.Details = append(result.Details, FeedConvoyResult{
		ConvoyID: convoyID,
		Action:   action,
		Message:  message,
	})
}

// closeEmptyConvoy runs `gt convoy check <id>` to auto-close an empty convoy.
func closeEmptyConvoy(townRoot, convoyID string) error {
	cmd := exec.Command("gt", "convoy", "check", convoyID)
	cmd.Dir = townRoot
	cmd.Env = deaconMutationRoutingEnv(townRoot)
	util.SetDetachedProcessGroup(cmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// dispatchFeedDog dispatches a dog to feed a stranded convoy via gt sling.
var dispatchFeedDog = defaultDispatchFeedDog

func defaultDispatchFeedDog(townRoot, convoyID string) error {
	cmd := exec.Command("gt", "sling", constants.MolConvoyFeed, "deacon/dogs",
		"--var", fmt.Sprintf("convoy=%s", convoyID))
	cmd.Dir = townRoot
	cmd.Env = deaconMutationRoutingEnv(townRoot)
	util.SetDetachedProcessGroup(cmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ConvoyChild is a tracked convoy issue used for feed admission.
type ConvoyChild struct {
	ID       string
	Status   string
	Assignee string
	Blocked  bool
}

var listConvoyChildren = defaultListConvoyChildren

// AdmitConvoyFeed rejects a convoy when any tracked child is blocked, hooked,
// or assigned. Readiness cannot be inferred from a shallow ready-count.
func AdmitConvoyFeed(children []ConvoyChild) (ok bool, reason string) {
	for _, child := range children {
		if child.Blocked {
			return false, fmt.Sprintf("tracked child %s is blocked", child.ID)
		}
		if child.Status == "hooked" || child.Status == "in_progress" {
			return false, fmt.Sprintf("tracked child %s is %s", child.ID, child.Status)
		}
		if strings.TrimSpace(child.Assignee) != "" {
			return false, fmt.Sprintf("tracked child %s is assigned to %s", child.ID, child.Assignee)
		}
	}
	return true, ""
}

func admitConvoyFeed(townRoot, convoyID string) (bool, string, error) {
	children, err := listConvoyChildren(townRoot, convoyID)
	if err != nil {
		return false, "", err
	}
	ok, reason := AdmitConvoyFeed(children)
	return ok, reason, nil
}

func defaultListConvoyChildren(townRoot, convoyID string) ([]ConvoyChild, error) {
	issues, err := showConvoyDependencies(townRoot, convoyID)
	if err != nil {
		return nil, err
	}
	var children []ConvoyChild
	for _, dep := range issues {
		child, skip, err := loadTrackedConvoyChild(townRoot, dep)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		children = append(children, child)
	}
	return children, nil
}

func showConvoyDependencies(townRoot, convoyID string) ([]beads.IssueDep, error) {
	cmd := beads.Command(townRoot, townBeadsDir(townRoot), beads.ReadOnlyRouting, "show", convoyID, "--json")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("showing convoy %s: %w", convoyID, err)
	}
	var issues []struct {
		Dependencies []beads.IssueDep `json:"dependencies"`
	}
	if err := json.Unmarshal(output, &issues); err != nil || len(issues) == 0 {
		return nil, fmt.Errorf("parsing convoy %s: %w", convoyID, err)
	}
	return issues[0].Dependencies, nil
}

func loadTrackedConvoyChild(townRoot string, dep beads.IssueDep) (ConvoyChild, bool, error) {
	if dep.DependencyType != "" && dep.DependencyType != "tracks" {
		return ConvoyChild{}, true, nil
	}
	show := beads.Command(townRoot, townBeadsDir(townRoot), beads.ReadOnlyRouting, "show", dep.ID, "--json")
	out, showErr := show.Output()
	if showErr != nil {
		return ConvoyChild{}, false, fmt.Errorf("showing tracked child %s: %w", dep.ID, showErr)
	}
	var details []struct {
		Status   string `json:"status"`
		Assignee string `json:"assignee"`
		Blocked  bool   `json:"blocked"`
	}
	if err := json.Unmarshal(out, &details); err != nil || len(details) == 0 {
		return ConvoyChild{}, false, fmt.Errorf("parsing tracked child %s: %w", dep.ID, err)
	}
	return ConvoyChild{
		ID:       dep.ID,
		Status:   details[0].Status,
		Assignee: details[0].Assignee,
		Blocked:  details[0].Blocked,
	}, false, nil
}

// PruneFeedStrandedState removes entries for convoys that are no longer open.
// Call periodically to prevent unbounded state growth.
func PruneFeedStrandedState(townRoot string) (int, error) {
	state, err := LoadFeedStrandedState(townRoot)
	if err != nil {
		return 0, err
	}

	pruned := 0
	for convoyID := range state.Convoys {
		status := getConvoyStatus(townRoot, convoyID)
		if status == "closed" || status == "" {
			delete(state.Convoys, convoyID)
			pruned++
		}
	}

	if pruned > 0 {
		if err := SaveFeedStrandedState(townRoot, state); err != nil {
			return pruned, err
		}
	}

	return pruned, nil
}

// getConvoyStatus returns the current status of a convoy bead.
func getConvoyStatus(townRoot, convoyID string) string {
	cmd := beads.Command(townRoot, townBeadsDir(townRoot), beads.ReadOnlyRouting, "show", convoyID, "--json")

	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	var issues []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &issues); err != nil || len(issues) == 0 {
		return ""
	}
	return issues[0].Status
}
