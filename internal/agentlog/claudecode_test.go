package agentlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClaudeProjectDirFor(t *testing.T) {
	// The project hash replaces '/' with '-', so the leading slash becomes '-'.
	// e.g., /some/work/dir → $HOME/.claude/projects/-some-work-dir
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("getting home dir: %v", err)
	}

	input := "/some/work/dir"
	wantSuffix := "-some-work-dir"
	wantDir := filepath.Join(home, claudeProjectsDir, wantSuffix)

	got, err := claudeProjectDirFor(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantDir {
		t.Errorf("claudeProjectDirFor(%q) = %q, want %q", input, got, wantDir)
	}
}

func TestParseClaudeCodeLine_Text(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello world"}]},"timestamp":"2026-02-23T10:00:00Z"}`
	events := parseClaudeCodeLine(line, "hq-mayor", "claudecode", "test-uuid")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != "text" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "text")
	}
	if ev.Role != "assistant" {
		t.Errorf("Role = %q, want %q", ev.Role, "assistant")
	}
	if ev.Content != "Hello world" {
		t.Errorf("Content = %q, want %q", ev.Content, "Hello world")
	}
	if ev.SessionID != "hq-mayor" {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, "hq-mayor")
	}
	if ev.AgentType != "claudecode" {
		t.Errorf("AgentType = %q, want %q", ev.AgentType, "claudecode")
	}
}

func TestParseClaudeCodeLine_ToolUse(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`
	events := parseClaudeCodeLine(line, "s1", "claudecode", "test-uuid")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != "tool_use" {
		t.Errorf("EventType = %q, want %q", ev.EventType, "tool_use")
	}
	if ev.Content == "" {
		t.Error("Content should not be empty for tool_use")
	}
	// Content should contain the tool name
	if len(ev.Content) < 4 || ev.Content[:4] != "Bash" {
		t.Errorf("Content %q should start with tool name 'Bash'", ev.Content)
	}
}

func TestParseClaudeCodeLine_UsageUsesConversationMetadata(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[],"usage":{"input_tokens":12,"output_tokens":34,"cache_read_input_tokens":56,"cache_creation_input_tokens":78}},"timestamp":"2026-02-23T10:00:00Z"}`
	events := parseClaudeCodeLine(line, "hq-mayor", "claudecode", "test-uuid")
	if len(events) != 1 {
		t.Fatalf("expected 1 usage event, got %d", len(events))
	}
	event := events[0]
	if event.AgentType != "claudecode" || event.SessionID != "hq-mayor" || event.NativeSessionID != "test-uuid" {
		t.Errorf("usage event metadata = %#v", event)
	}
	if event.EventType != "usage" || event.InputTokens != 12 || event.OutputTokens != 34 || event.CacheReadTokens != 56 || event.CacheCreationTokens != 78 {
		t.Errorf("usage event = %#v", event)
	}
}

func TestParseClaudeCodeLine_SkipsUnknownTypes(t *testing.T) {
	line := `{"type":"summary","content":"some summary"}`
	events := parseClaudeCodeLine(line, "s1", "claudecode", "test-uuid")
	if len(events) != 0 {
		t.Errorf("expected 0 events for summary type, got %d", len(events))
	}
}

func TestParseClaudeCodeLine_InvalidJSON(t *testing.T) {
	events := parseClaudeCodeLine("not json", "s1", "claudecode", "test-uuid")
	if len(events) != 0 {
		t.Errorf("expected 0 events for invalid JSON, got %d", len(events))
	}
}

func TestDecodeConversationEntry(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "assistant", line: `{"type":"assistant","message":{"role":"assistant"}}`, want: true},
		{name: "user", line: `{"type":"user","message":{"role":"user"}}`, want: true},
		{name: "unsupported type", line: `{"type":"summary","message":{"role":"assistant"}}`},
		{name: "missing message", line: `{"type":"assistant"}`},
		{name: "invalid JSON", line: `{`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var entry ccEntry
			if got := decodeConversationEntry(tt.line, &entry); got != tt.want {
				t.Fatalf("decodeConversationEntry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEntryTimestamp(t *testing.T) {
	want := time.Date(2026, time.February, 23, 10, 0, 0, 0, time.UTC)
	if got := entryTimestamp("2026-02-23T10:00:00Z"); !got.Equal(want) {
		t.Fatalf("valid timestamp = %s, want %s", got, want)
	}

	before := time.Now()
	got := entryTimestamp("invalid")
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("invalid timestamp fallback = %s, want between %s and %s", got, before, after)
	}
}

func TestContentEventsPreservesMetadata(t *testing.T) {
	timestamp := time.Date(2026, time.February, 23, 10, 0, 0, 0, time.UTC)
	metadata := eventMetadata{
		agentType:       "claudecode",
		sessionID:       "session-1",
		nativeSessionID: "native-1",
		role:            "assistant",
		timestamp:       timestamp,
	}
	events := contentEvents([]ccContent{{Type: "text", Text: "hello"}}, metadata)
	if len(events) != 1 {
		t.Fatalf("contentEvents() returned %d events, want 1", len(events))
	}
	want := AgentEvent{
		AgentType:       "claudecode",
		SessionID:       "session-1",
		NativeSessionID: "native-1",
		EventType:       "text",
		Role:            "assistant",
		Content:         "hello",
		Timestamp:       timestamp,
	}
	if events[0] != want {
		t.Fatalf("content event = %#v, want %#v", events[0], want)
	}
}

func TestAppendUsageEventTokenSources(t *testing.T) {
	timestamp := time.Date(2026, time.February, 23, 10, 0, 0, 0, time.UTC)
	metadata := eventMetadata{
		agentType:       "claudecode",
		sessionID:       "session-1",
		nativeSessionID: "native-1",
		timestamp:       timestamp,
	}
	tests := []struct {
		name  string
		usage ccUsage
	}{
		{name: "input", usage: ccUsage{InputTokens: 1}},
		{name: "output", usage: ccUsage{OutputTokens: 1}},
		{name: "cache read", usage: ccUsage{CacheReadInputTokens: 1}},
		{name: "cache creation", usage: ccUsage{CacheCreationInputTokens: 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := ccEntry{Type: "assistant", Message: &ccMessage{Usage: &tt.usage}}
			events := appendUsageEvent(nil, entry, metadata)
			if len(events) != 1 {
				t.Fatalf("appendUsageEvent() returned %d events, want 1", len(events))
			}
			event := events[0]
			if event.AgentType != "claudecode" || event.SessionID != "session-1" || event.NativeSessionID != "native-1" || event.EventType != "usage" || event.Role != "assistant" || !event.Timestamp.Equal(timestamp) {
				t.Fatalf("usage event metadata = %#v", event)
			}
		})
	}

	zeroUsage := ccUsage{}
	if got := appendUsageEvent(nil, ccEntry{Type: "assistant", Message: &ccMessage{Usage: &zeroUsage}}, metadata); len(got) != 0 {
		t.Fatalf("zero usage produced events: %#v", got)
	}
	inputUsage := ccUsage{InputTokens: 1}
	if got := appendUsageEvent(nil, ccEntry{Type: "user", Message: &ccMessage{Usage: &inputUsage}}, metadata); len(got) != 0 {
		t.Fatalf("user usage produced events: %#v", got)
	}
}

func TestNewAdapter(t *testing.T) {
	tests := []struct {
		name      string
		agentType string
		wantNil   bool
		wantType  string
	}{
		{"claudecode", "claudecode", false, "claudecode"},
		{"empty defaults to claudecode", "", false, "claudecode"},
		{"opencode", "opencode", false, "opencode"},
		{"unknown", "kiro", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAdapter(tt.agentType)
			if tt.wantNil {
				if a != nil {
					t.Errorf("expected nil adapter for %q", tt.agentType)
				}
				return
			}
			if a == nil {
				t.Fatalf("expected non-nil adapter for %q", tt.agentType)
			}
			if a.AgentType() != tt.wantType {
				t.Errorf("AgentType() = %q, want %q", a.AgentType(), tt.wantType)
			}
		})
	}
}
