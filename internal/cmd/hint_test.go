package cmd

import (
	"strings"
	"testing"
)

func TestHintCommandRegisteredAndExempt(t *testing.T) {
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "hint" {
			found = true
			if cmd.GroupID != GroupWorkspace {
				t.Fatalf("hint GroupID = %q, want %q", cmd.GroupID, GroupWorkspace)
			}
			break
		}
	}
	if !found {
		t.Fatal("hint command not registered with rootCmd")
	}
	if !beadsExemptCommands["hint"] {
		t.Fatal("hint should be in beadsExemptCommands")
	}
	if !branchCheckExemptCommands["hint"] {
		t.Fatal("hint should be in branchCheckExemptCommands")
	}
}

func TestHintTextCoversGettingStarted(t *testing.T) {
	text := hintText()
	for _, want := range []string{
		"gt install ~/my-town",
		"gt config default-agent claude",
		`gt config agent set opencode "opencode -m ollama-cloud/gpt-oss:120b" --provider opencode`,
		"gt config default-agent opencode",
		"gt config mix default=opencode mayor=claude",
		"gt config role set mayor claude",
		"gt config mix",
		"gt up",
		"gt status",
		"gt rig add",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("hint text missing %q\n%s", want, text)
		}
	}
	if strings.Contains(text, "CGO_ENABLED") {
		t.Error("hint text must not teach a compiler flag")
	}
}
