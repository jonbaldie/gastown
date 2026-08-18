package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNames_IncludesStandingSkillDirectives(t *testing.T) {
	t.Parallel()

	names := Names()
	for _, name := range StandingNames {
		if !contains(names, name) {
			t.Fatalf("Names() missing %s: %v", name, names)
		}
	}
}

func TestProvisionFor_StandingSkillsAreModelInvocable(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	if err := ProvisionFor(workDir, "claude"); err != nil {
		t.Fatalf("ProvisionFor: %v", err)
	}

	assertStandingSkillsModelInvocable(t, workDir, "claude")
}

func TestProvisionFor_PatchesExistingStandingSkillsToModelInvocable(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	skill := filepath.Join(workDir, ".agents", "skills", "implement", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0755); err != nil {
		t.Fatal(err)
	}
	existing := "---\nname: implement\ndescription: custom standing skill\ndisable-model-invocation: true\n---\n\ncustom implement body\n"
	if err := os.WriteFile(skill, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ProvisionFor(workDir, "claude"); err != nil {
		t.Fatalf("ProvisionFor: %v", err)
	}

	got, err := os.ReadFile(skill)
	if err != nil {
		t.Fatal(err)
	}
	content := string(got)
	if strings.Contains(frontmatter(content), "disable-model-invocation") {
		t.Fatalf("existing standing skill still user-invoked:\n%s", content)
	}
	if !strings.Contains(content, "custom implement body") {
		t.Fatalf("rewrote custom standing skill body:\n%s", content)
	}
	if !strings.Contains(content, "description: custom standing skill") {
		t.Fatalf("dropped custom description:\n%s", content)
	}
}

func TestProvisionFor_LeavesNonStandingSkillsUserInvoked(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	if err := ProvisionFor(workDir, "claude"); err != nil {
		t.Fatalf("ProvisionFor: %v", err)
	}

	path := filepath.Join(workDir, ".agents", "skills", "grill-me", "SKILL.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frontmatter(string(got)), "disable-model-invocation: true") {
		t.Fatalf("non-standing skill should stay user-invoked:\n%s", got)
	}
}

func TestProvisionUserDir_StandingSkillsAreModelInvocable(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	if err := ProvisionUserDir(configDir); err != nil {
		t.Fatalf("ProvisionUserDir: %v", err)
	}

	for _, name := range StandingNames {
		path := filepath.Join(configDir, "skills", name, "SKILL.md")
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing user-dir skill %s: %v", name, err)
		}
		if strings.Contains(frontmatter(string(got)), "disable-model-invocation") {
			t.Errorf("%s still has disable-model-invocation after ProvisionUserDir:\n%s", name, got)
		}
	}
}

func TestProvisionFor_WritesUniversalAndAgentSkillTrees(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	if err := ProvisionFor(workDir, "claude"); err != nil {
		t.Fatalf("ProvisionFor: %v", err)
	}

	for _, name := range StandingNames {
		agentsSkill := filepath.Join(workDir, ".agents", "skills", name, "SKILL.md")
		if _, err := os.Stat(agentsSkill); err != nil {
			t.Errorf("missing universal skill %s: %v", name, err)
		}
		claudeSkill := filepath.Join(workDir, ".claude", "skills", name, "SKILL.md")
		if _, err := os.Stat(claudeSkill); err != nil {
			t.Errorf("missing claude skill %s: %v", name, err)
		}
	}
}

func assertStandingSkillsModelInvocable(t *testing.T, workDir, agent string) {
	t.Helper()

	roots := []string{filepath.Join(workDir, ".agents", "skills")}
	if configDir := agentConfigDir(agent); configDir != "" {
		roots = append(roots, filepath.Join(workDir, configDir, "skills"))
	}
	for _, name := range StandingNames {
		for _, root := range roots {
			path := filepath.Join(root, name, "SKILL.md")
			got, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("missing standing skill %s at %s: %v", name, path, err)
				continue
			}
			content := string(got)
			if strings.Contains(frontmatter(content), "disable-model-invocation") {
				t.Errorf("%s is not model-invocable after injection (%s):\n%s", name, path, content)
			}
			if !strings.Contains(content, "description:") {
				t.Errorf("%s lost its description, so Claude cannot discover it (%s):\n%s", name, path, content)
			}
		}
	}
}

func frontmatter(content string) string {
	if !strings.HasPrefix(content, "---\n") {
		return ""
	}
	rest := content[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func TestProvisionFor_DoesNotOverwriteExistingSkill(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	custom := filepath.Join(workDir, ".agents", "skills", "implement", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(custom), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(custom, []byte("custom implement skill\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ProvisionFor(workDir, "claude"); err != nil {
		t.Fatalf("ProvisionFor: %v", err)
	}

	got, err := os.ReadFile(custom)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "custom implement skill\n" {
		t.Fatalf("overwrote existing skill, got %q", got)
	}
}

func TestProvisionFor_CursorUsesCursorSkillsDir(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	if err := ProvisionFor(workDir, "cursor"); err != nil {
		t.Fatalf("ProvisionFor: %v", err)
	}

	path := filepath.Join(workDir, ".cursor", "skills", "implement", "SKILL.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("missing cursor skill: %v", err)
	}
}

func TestProvisionUserDir_WritesConfigDirSkills(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	if err := ProvisionUserDir(configDir); err != nil {
		t.Fatalf("ProvisionUserDir: %v", err)
	}

	path := filepath.Join(configDir, "skills", "diagnosing-bugs", "SKILL.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("missing user-dir skill: %v", err)
	}
}

func TestMissingFor_ReportsAbsentSkills(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	missing := MissingFor(workDir, "claude")
	if len(missing) == 0 {
		t.Fatal("expected missing skills in empty workspace")
	}
	if !contains(missing, "implement") {
		t.Fatalf("MissingFor should include implement, got %v", missing)
	}

	if err := ProvisionFor(workDir, "claude"); err != nil {
		t.Fatalf("ProvisionFor: %v", err)
	}
	if got := MissingFor(workDir, "claude"); len(got) != 0 {
		t.Fatalf("MissingFor after provision = %v, want empty", got)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestStripDisableModelInvocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips flag from frontmatter",
			in:   "---\nname: implement\ndescription: do the work\ndisable-model-invocation: true\n---\n\nbody\n",
			want: "---\nname: implement\ndescription: do the work\n---\n\nbody\n",
		},
		{
			name: "leaves already model-invocable frontmatter",
			in:   "---\nname: diagnosing-bugs\ndescription: diagnose\n---\n# Diagnosing\n",
			want: "---\nname: diagnosing-bugs\ndescription: diagnose\n---\n# Diagnosing\n",
		},
		{
			name: "does not strip the key from the body",
			in:   "---\nname: notes\n---\nSee disable-model-invocation: true in the spec.\n",
			want: "---\nname: notes\n---\nSee disable-model-invocation: true in the spec.\n",
		},
		{
			name: "leaves files without frontmatter",
			in:   "custom implement skill\n",
			want: "custom implement skill\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := string(stripDisableModelInvocation([]byte(tt.in)))
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
