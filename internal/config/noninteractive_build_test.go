package config

import (
	"strings"
	"testing"
)

// TestBuildNonInteractiveCommand verifies that the NonInteractive config
// (Subcommand, OutputFlag, PromptFlag) is consumed to build a correct
// non-interactive (headless / one-shot) command. This is the regression test
// for gt-5p5x: previously NonInteractive was dead config — set on 9 presets
// but never read by any builder.
func TestBuildNonInteractiveCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rc      *RuntimeConfig
		prompt  string
		wantSub []string // substrings that MUST appear in the command
		wantNot []string // substrings that must NOT appear
	}{
		{
			name: "opencode run --format json positional prompt",
			rc: &RuntimeConfig{
				Command: "opencode",
				Args:    []string{},
				PromptMode: "arg",
				NonInteractive: &NonInteractiveConfig{
					Subcommand: "run",
					OutputFlag: "--format json",
				},
			},
			prompt:  "say hello",
			wantSub: []string{"opencode", "run", "--format", "json", "say hello"},
			wantNot: []string{"--prompt"},
		},
		{
			name: "gemini --output-format json -p",
			rc: &RuntimeConfig{
				Command:    "gemini",
				Args:       []string{},
				PromptMode: "arg",
				NonInteractive: &NonInteractiveConfig{
					PromptFlag: "-p",
					OutputFlag: "--output-format json",
				},
			},
			prompt:  "do thing",
			wantSub: []string{"gemini", "--output-format", "json", "-p", "do thing"},
			wantNot: []string{"run", "exec"},
		},
		{
			name: "codex exec --json positional prompt",
			rc: &RuntimeConfig{
				Command:    "codex",
				Args:       []string{},
				PromptMode: "arg",
				NonInteractive: &NonInteractiveConfig{
					Subcommand: "exec",
					OutputFlag: "--json",
				},
			},
			prompt:  "review this",
			wantSub: []string{"codex", "exec", "--json", "review this"},
			wantNot: []string{"--prompt", "-p"},
		},
		{
			name: "nil NonInteractive falls back to BuildArgsWithPrompt",
			rc: &RuntimeConfig{
				Command:    "claude",
				Args:       []string{"--dangerously-skip-permissions"},
				PromptMode: "arg",
			},
			prompt:  "hi",
			wantSub: []string{"claude", "--dangerously-skip-permissions", "hi"},
			wantNot: []string{"run", "--format"},
		},
		{
			name: "copilot -p only (no output flag)",
			rc: &RuntimeConfig{
				Command:    "copilot",
				Args:       []string{},
				PromptMode: "arg",
				NonInteractive: &NonInteractiveConfig{
					PromptFlag: "-p",
				},
			},
			prompt:  "test",
			wantSub: []string{"copilot", "-p", "test"},
			wantNot: []string{"--format", "run"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.rc.BuildNonInteractiveCommand(tt.prompt)
			for _, sub := range tt.wantSub {
				if !strings.Contains(got, sub) {
					t.Errorf("BuildNonInteractiveCommand(%q) = %q; missing %q", tt.prompt, got, sub)
				}
			}
			for _, not := range tt.wantNot {
				if strings.Contains(got, not) {
					t.Errorf("BuildNonInteractiveCommand(%q) = %q; unexpected %q", tt.prompt, got, not)
				}
			}
		})
	}
}

// TestBuildNonInteractiveArgsWithPrompt verifies the exec-style arg vector.
func TestBuildNonInteractiveArgsWithPrompt(t *testing.T) {
	t.Parallel()

	rc := &RuntimeConfig{
		Command:    "opencode",
		Args:       []string{"-m", "ollama-cloud/gpt-oss:120b"},
		PromptMode: "arg",
		NonInteractive: &NonInteractiveConfig{
			Subcommand: "run",
			OutputFlag: "--format json",
		},
	}
	got := rc.BuildNonInteractiveArgsWithPrompt("hello")
	want := []string{"opencode", "-m", "ollama-cloud/gpt-oss:120b", "run", "--format", "json", "hello"}
	if len(got) != len(want) {
		t.Fatalf("got %d args %v, want %d args %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestApplyRuntimeNonInteractiveDefaults verifies that resolving a preset
// copies NonInteractive into the RuntimeConfig.
func TestApplyRuntimeNonInteractiveDefaults(t *testing.T) {
	t.Parallel()

	preset, ok := builtinPresets[AgentOpenCode]
	if !ok {
		t.Fatal("opencode preset missing")
	}
	if preset.NonInteractive == nil {
		t.Fatal("opencode preset has no NonInteractive config")
	}

	rc := &RuntimeConfig{Command: "opencode", PromptMode: "arg"}
	applyRuntimeNonInteractiveDefaults(rc, preset)

	if rc.NonInteractive == nil {
		t.Fatal("NonInteractive not copied from preset to RuntimeConfig")
	}
	if rc.NonInteractive.Subcommand != "run" {
		t.Errorf("Subcommand = %q, want %q", rc.NonInteractive.Subcommand, "run")
	}
	if rc.NonInteractive.OutputFlag != "--format json" {
		t.Errorf("OutputFlag = %q, want %q", rc.NonInteractive.OutputFlag, "--format json")
	}
	// opencode uses a positional prompt (no PromptFlag) for the "run" subcommand
	if rc.NonInteractive.PromptFlag != "" {
		t.Errorf("PromptFlag = %q, want empty (positional prompt for opencode run)", rc.NonInteractive.PromptFlag)
	}

	// Full resolution: applyRuntimePresetDefaults should also copy it.
	rc2 := &RuntimeConfig{Command: "opencode"}
	applyRuntimePresetDefaults(rc2, preset)
	if rc2.NonInteractive == nil {
		t.Fatal("NonInteractive not copied via applyRuntimePresetDefaults")
	}
	if rc2.NonInteractive.Subcommand != "run" {
		t.Errorf("via applyRuntimePresetDefaults: Subcommand = %q, want %q", rc2.NonInteractive.Subcommand, "run")
	}
}

// TestNonInteractiveConfigClone verifies that normalizeRuntimeConfig clones
// the NonInteractive field (not shared with the original).
func TestNonInteractiveConfigClone(t *testing.T) {
	t.Parallel()

	rc := &RuntimeConfig{
		Command:    "opencode",
		PromptMode: "arg",
		NonInteractive: &NonInteractiveConfig{
			Subcommand: "run",
			OutputFlag: "--format json",
		},
	}
	resolved := normalizeRuntimeConfig(rc)
	resolved.NonInteractive.Subcommand = "exec"

	if rc.NonInteractive.Subcommand != "run" {
		t.Errorf("normalizeRuntimeConfig did not clone NonInteractive: original mutated to %q", rc.NonInteractive.Subcommand)
	}
}

// TestRuntimeConfigFromPresetCarriesNonInteractive verifies that the public
// RuntimeConfigFromPreset path (used by seance, sling, etc.) copies
// NonInteractive from the builtin preset so BuildNonInteractiveCommand works.
func TestRuntimeConfigFromPresetCarriesNonInteractive(t *testing.T) {
	t.Parallel()

	rc := RuntimeConfigFromPreset(AgentOpenCode)
	if rc.NonInteractive == nil {
		t.Fatal("RuntimeConfigFromPreset(opencode) returned nil NonInteractive — builder will fall back to interactive mode")
	}
	if rc.NonInteractive.Subcommand != "run" {
		t.Errorf("Subcommand = %q, want %q", rc.NonInteractive.Subcommand, "run")
	}
	if rc.NonInteractive.PromptFlag != "" {
		t.Errorf("PromptFlag = %q, want empty (positional for opencode run)", rc.NonInteractive.PromptFlag)
	}
	if rc.NonInteractive.OutputFlag != "--format json" {
		t.Errorf("OutputFlag = %q, want %q", rc.NonInteractive.OutputFlag, "--format json")
	}

	// The full non-interactive command should include subcommand, output flag,
	// and positional prompt — but NOT --prompt (that's for interactive mode).
	cmd := rc.BuildNonInteractiveCommand("hello world")
	for _, want := range []string{"opencode", "run", "--format", "json", "hello world"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("BuildNonInteractiveCommand via RuntimeConfigFromPreset = %q; missing %q", cmd, want)
		}
	}
	if strings.Contains(cmd, "--prompt") {
		t.Errorf("BuildNonInteractiveCommand via RuntimeConfigFromPreset = %q; should NOT contain --prompt (run subcommand uses positional prompt)", cmd)
	}
}