package cmd

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
)

func TestConfigRoleSetAndUnset(t *testing.T) {
	townRoot := setupTestTownForConfig(t)
	settingsPath := config.TownSettingsPath(townRoot)
	settings := config.NewTownSettings()
	settings.Agents = map[string]*config.RuntimeConfig{
		"pi-luna": {Provider: "pi", Command: "pi", Args: []string{"--model", "openai-codex/gpt-5.6-luna"}},
	}
	if err := config.SaveTownSettings(settingsPath, settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalWD) })
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if err := runConfigRoleSet(&cobra.Command{}, []string{"witness", "pi-luna", "low"}); err != nil {
		t.Fatalf("set role: %v", err)
	}

	got, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if got.RoleAgents["witness"] != "pi-luna" {
		t.Fatalf("role agent = %q, want pi-luna", got.RoleAgents["witness"])
	}
	if got.RoleEffort["witness"] != "low" {
		t.Fatalf("role effort = %q, want low", got.RoleEffort["witness"])
	}

	if err := runConfigRoleUnset(&cobra.Command{}, []string{"witness"}); err != nil {
		t.Fatalf("unset role: %v", err)
	}
	got, err = config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if _, ok := got.RoleAgents["witness"]; ok {
		t.Fatal("role agent was not removed")
	}
	if _, ok := got.RoleEffort["witness"]; ok {
		t.Fatal("role effort was not removed")
	}
}

func TestConfigRoleSetValidation(t *testing.T) {
	townRoot := setupTestTownForConfig(t)
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalWD) })
	if err := os.Chdir(townRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{name: "role", args: []string{"inventor", "pi", "low"}},
		{name: "agent", args: []string{"witness", "missing-agent", "low"}},
		{name: "effort", args: []string{"witness", "pi", "extreme"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := runConfigRoleSet(&cobra.Command{}, tt.args); err == nil {
				t.Fatalf("runConfigRoleSet(%v) succeeded, want error", tt.args)
			}
		})
	}
}
