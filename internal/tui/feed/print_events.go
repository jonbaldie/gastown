package feed

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PrintOptions controls filtering and behavior for PrintGtEvents.
type PrintOptions struct {
	Limit  int
	Follow bool
	Since  string          // duration string like "5m", "1h"
	Mol    string          // molecule/issue ID prefix filter
	Type   string          // event type filter
	Rig    string          // rig name filter (matches event's Rig field)
	Ctx    context.Context // optional: controls follow-mode lifecycle; nil uses signal.NotifyContext
}

// PrintGtEvents reads .events.jsonl and prints events to stdout.
// When opts.Follow is true, it tails the file for new events after printing
// the initial batch, polling every 200ms. Canceled via opts.Ctx or SIGINT.
func PrintGtEvents(townRoot string, opts PrintOptions) error {
	file, sinceTime, err := openGtEventsFile(townRoot, opts.Since)
	if err != nil {
		return err
	}
	defer file.Close()
	events, err := loadGtEvents(file, sinceTime, opts)
	if err != nil {
		return err
	}
	printLoadedGtEvents(events, opts.Follow)
	if !opts.Follow {
		return nil
	}
	return followGtEvents(file, sinceTime, opts)
}

func openGtEventsFile(townRoot, since string) (*os.File, time.Time, error) {
	eventsPath := filepath.Join(townRoot, ".events.jsonl")
	file, err := os.Open(eventsPath)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("no events file found at %s: %w", eventsPath, err)
	}
	sinceTime, err := parseEventsSince(since)
	if err != nil {
		file.Close()
		return nil, time.Time{}, err
	}
	return file, sinceTime, nil
}

func parseEventsSince(since string) (time.Time, error) {
	if since == "" {
		return time.Time{}, nil
	}
	dur, err := time.ParseDuration(since)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --since duration %q: %w", since, err)
	}
	return time.Now().Add(-dur), nil
}

func loadGtEvents(file *os.File, sinceTime time.Time, opts PrintOptions) ([]Event, error) {
	var events []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		appendMatchingEvent(&events, scanner.Text(), sinceTime, opts)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading events: %w", err)
	}
	return limitGtEvents(events, opts.Limit), nil
}

func appendMatchingEvent(events *[]Event, line string, sinceTime time.Time, opts PrintOptions) {
	event := parseGtEventLine(line)
	if event == nil || !matchesFilters(event, sinceTime, opts.Mol, opts.Type, opts.Rig) {
		return
	}
	*events = append(*events, *event)
}

func limitGtEvents(events []Event, limit int) []Event {
	sort.Slice(events, func(i, j int) bool {
		return events[i].Time.After(events[j].Time)
	})
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events
}

func printLoadedGtEvents(events []Event, follow bool) {
	if len(events) == 0 && !follow {
		fmt.Println("No events found in .events.jsonl")
		return
	}
	for _, event := range events {
		printEvent(event)
	}
}

func followGtEvents(file *os.File, sinceTime time.Time, opts PrintOptions) error {
	// Tail mode: poll for new lines using a fresh scanner each tick.
	// bufio.Scanner sets an internal 'done' flag after EOF and won't retry,
	// so we must create a new scanner each poll cycle while preserving the
	// file offset (os.File tracks position across scanner instances).
	ctx := opts.Ctx
	if ctx == nil {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scanNewGtEvents(file, sinceTime, opts)
		}
	}
}

func scanNewGtEvents(file *os.File, sinceTime time.Time, opts PrintOptions) {
	s := bufio.NewScanner(file)
	s.Buffer(make([]byte, 1024*1024), 1024*1024)
	for s.Scan() {
		event := parseGtEventLine(s.Text())
		if event != nil && matchesFilters(event, sinceTime, opts.Mol, opts.Type, opts.Rig) {
			printEvent(*event)
		}
	}
}

// matchesFilters checks whether an event passes the --since, --mol, --type, and --rig filters.
func matchesFilters(event *Event, sinceTime time.Time, mol, eventType, rig string) bool {
	if !matchesSinceFilter(event, sinceTime) {
		return false
	}
	if !matchesMolFilter(event, mol) {
		return false
	}
	if eventType != "" && event.Type != eventType {
		return false
	}
	if rig != "" && event.Rig != rig {
		return false
	}
	return true
}

func matchesSinceFilter(event *Event, sinceTime time.Time) bool {
	return sinceTime.IsZero() || !event.Time.Before(sinceTime)
}

func matchesMolFilter(event *Event, mol string) bool {
	if mol == "" {
		return true
	}
	return strings.Contains(event.Target, mol) || strings.Contains(event.Message, mol)
}

// printEvent formats and prints a single event line.
func printEvent(event Event) {
	symbol := typeSymbol(event.Type)
	ts := event.Time.Local().Format("15:04:05")
	actor := event.Actor
	if actor == "" {
		actor = "system"
	}
	fmt.Printf("[%s] %s %-25s %s\n", ts, symbol, actor, event.Message)
}

var eventTypeSymbols = map[string]string{
	"patrol_started":  "\U0001F989", // owl
	"patrol_complete": "\U0001F989", // owl
	"polecat_nudged":  "\u26A1",     // lightning
	"sling":           "\U0001F3AF", // target
	"handoff":         "\U0001F91D", // handshake
	"done":            "\u2713",     // checkmark
	"merged":          "\u2713",
	"merge_failed":    "\u2717", // x
	"create":          "+",
	"complete":        "\u2713",
	"fail":            "\u2717",
	"delete":          "\u2298", // circled minus
}

func typeSymbol(eventType string) string {
	if symbol, ok := eventTypeSymbols[eventType]; ok {
		return symbol
	}
	return "\u2192" // arrow
}
