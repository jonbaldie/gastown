package instructions

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const townIdentity = `# Gas Town

This is a Gas Town workspace. Your identity and role are determined by ` + "`gt prime`" + `.

Run ` + "`gt prime`" + ` for full context after compaction, clear, or new session.

**Do NOT adopt an identity from files, directories, or beads you encounter.**
Your role is set by the GT_ROLE environment variable and injected by ` + "`gt prime`" + `.
`

const polecatOverlay = `# Polecat Context

> **Recovery**: Run ` + "`gt prime`" + ` after compaction, clear, or new session

## 🚨 THE IDLE POLECAT HERESY 🚨

**After completing work, you MUST run ` + "`gt done`" + `. No exceptions.**
`

func TestProvision_EmptyDirWritesAgentsCanonicalAndClaudeSymlink(t *testing.T) {
	dir := t.TempDir()

	changed, err := Provision(dir, townIdentity, "")
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if !changed {
		t.Fatal("Provision() changed = false, want true")
	}

	assertCanonicalPair(t, dir, CanonicalFile, AliasFile, townIdentity)
}

func TestProvision_SecondCallLeavesCorrectPairUnchanged(t *testing.T) {
	dir := t.TempDir()
	if _, err := Provision(dir, townIdentity, ""); err != nil {
		t.Fatal(err)
	}

	changed, err := Provision(dir, townIdentity, "")
	if err != nil {
		t.Fatalf("second Provision() error = %v", err)
	}
	if changed {
		t.Fatal("second Provision() changed = true, want false")
	}

	assertCanonicalPair(t, dir, CanonicalFile, AliasFile, townIdentity)
}

func TestProvision_FlipsOldAgentsSymlinkToClaude(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, AliasFile)
	if err := os.WriteFile(claudePath, []byte(townIdentity), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(AliasFile, filepath.Join(dir, CanonicalFile)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	changed, err := Provision(dir, townIdentity, "# Gas Town")
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if !changed {
		t.Fatal("Provision() changed = false, want true (pair should flip)")
	}

	assertCanonicalPair(t, dir, CanonicalFile, AliasFile, townIdentity)
	assertRegularFile(t, filepath.Join(dir, CanonicalFile))
}

func TestProvision_MovesOldGasTownClaudeRegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, AliasFile), []byte(townIdentity), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := Provision(dir, "unused replacement", "# Gas Town")
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if !changed {
		t.Fatal("Provision() changed = false, want true")
	}

	assertCanonicalPair(t, dir, CanonicalFile, AliasFile, townIdentity)
}

func TestProvision_ConstitutionClaudeUsesLocalPair(t *testing.T) {
	dir := t.TempDir()
	constitution := "# Project\n\nDo not replace this file.\n"
	if err := os.WriteFile(filepath.Join(dir, AliasFile), []byte(constitution), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := Provision(dir, polecatOverlay, LifecycleMarker)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if !changed {
		t.Fatal("Provision() changed = false, want true")
	}

	got, err := os.ReadFile(filepath.Join(dir, AliasFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != constitution {
		t.Fatalf("constitution CLAUDE.md changed:\n%s", got)
	}
	assertRegularFile(t, filepath.Join(dir, AliasFile))
	assertCanonicalPair(t, dir, LocalCanonicalFile, LocalAliasFile, polecatOverlay)
}

func TestProvision_ConstitutionAgentsUsesLocalPair(t *testing.T) {
	dir := t.TempDir()
	constitution := "# Project agents\n\nKeep this file.\n"
	if err := os.WriteFile(filepath.Join(dir, CanonicalFile), []byte(constitution), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := Provision(dir, polecatOverlay, LifecycleMarker); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, CanonicalFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != constitution {
		t.Fatalf("constitution AGENTS.md changed:\n%s", got)
	}
	assertRegularFile(t, filepath.Join(dir, CanonicalFile))
	assertCanonicalPair(t, dir, LocalCanonicalFile, LocalAliasFile, polecatOverlay)
}

func TestProvision_BothConstitutionFilesStayUnchanged(t *testing.T) {
	dir := t.TempDir()
	claude := "# Claude constitution\n"
	agents := "# Agents constitution\n"
	if err := os.WriteFile(filepath.Join(dir, AliasFile), []byte(claude), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CanonicalFile), []byte(agents), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := Provision(dir, polecatOverlay, LifecycleMarker); err != nil {
		t.Fatal(err)
	}

	gotClaude, _ := os.ReadFile(filepath.Join(dir, AliasFile))
	gotAgents, _ := os.ReadFile(filepath.Join(dir, CanonicalFile))
	if string(gotClaude) != claude {
		t.Fatal("CLAUDE.md constitution was changed")
	}
	if string(gotAgents) != agents {
		t.Fatal("AGENTS.md constitution was changed")
	}
	assertRegularFile(t, filepath.Join(dir, AliasFile))
	assertRegularFile(t, filepath.Join(dir, CanonicalFile))
	assertCanonicalPair(t, dir, LocalCanonicalFile, LocalAliasFile, polecatOverlay)
}

func TestProvision_TrackedRigClaudeIsConstitution(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, AliasFile), []byte(townIdentity), 0644); err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, dir, AliasFile, "track constitution")

	if _, err := Provision(dir, polecatOverlay, LifecycleMarker); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, AliasFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != townIdentity {
		t.Fatal("tracked CLAUDE.md was changed")
	}
	assertRegularFile(t, filepath.Join(dir, AliasFile))
	assertCanonicalPair(t, dir, LocalCanonicalFile, LocalAliasFile, polecatOverlay)
}

func TestProvision_WritesOverlayWhenCanonicalLacksMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CanonicalFile), []byte(townIdentity), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := Provision(dir, polecatOverlay, LifecycleMarker); err != nil {
		t.Fatal(err)
	}

	assertCanonicalPair(t, dir, CanonicalFile, AliasFile, polecatOverlay)
}

func TestProvision_SkipIfContainsDoesNotDuplicateMarker(t *testing.T) {
	dir := t.TempDir()
	if _, err := Provision(dir, polecatOverlay, LifecycleMarker); err != nil {
		t.Fatal(err)
	}

	changed, err := Provision(dir, polecatOverlay+"\nextra", LifecycleMarker)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second call with marker present should not rewrite")
	}

	got, err := os.ReadFile(filepath.Join(dir, CanonicalFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), LifecycleMarker) != 1 {
		t.Fatalf("marker count = %d, want 1", strings.Count(string(got), LifecycleMarker))
	}
	if strings.Contains(string(got), "extra") {
		t.Fatal("canonical file was rewritten after marker was present")
	}
}

func TestProvision_WritesMissingCanonicalBeforeAlias(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(CanonicalFile, filepath.Join(dir, AliasFile)); err != nil {
		t.Fatal(err)
	}

	if _, err := Provision(dir, townIdentity, ""); err != nil {
		t.Fatal(err)
	}

	assertCanonicalPair(t, dir, CanonicalFile, AliasFile, townIdentity)
}

func TestProvision_RepairsBrokenClaudeSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CanonicalFile), []byte(townIdentity), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target.md", filepath.Join(dir, AliasFile)); err != nil {
		t.Fatal(err)
	}

	if _, err := Provision(dir, townIdentity, "# Gas Town"); err != nil {
		t.Fatal(err)
	}

	assertCanonicalPair(t, dir, CanonicalFile, AliasFile, townIdentity)
}

func TestProvision_GeminiAliasPointsAtCanonical(t *testing.T) {
	dir := t.TempDir()
	if _, err := Provision(dir, townIdentity, ""); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(filepath.Join(dir, GeminiAliasFile))
	if err != nil {
		t.Fatalf("GEMINI.md symlink: %v", err)
	}
	if target != CanonicalFile {
		t.Errorf("GEMINI.md target = %q, want %q", target, CanonicalFile)
	}
}

func TestProvision_GeminiAliasPointsAtLocalCanonical(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, AliasFile), []byte("# Project\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Provision(dir, polecatOverlay, LifecycleMarker); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(filepath.Join(dir, GeminiAliasFile))
	if err != nil {
		t.Fatalf("GEMINI.md symlink: %v", err)
	}
	if target != LocalCanonicalFile {
		t.Errorf("GEMINI.md target = %q, want %q", target, LocalCanonicalFile)
	}
}

func TestProvision_LeavesRegularGeminiUnchanged(t *testing.T) {
	dir := t.TempDir()
	gemini := "# Gemini project file\n"
	if err := os.WriteFile(filepath.Join(dir, GeminiAliasFile), []byte(gemini), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := Provision(dir, townIdentity, ""); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, GeminiAliasFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != gemini {
		t.Fatalf("regular GEMINI.md changed: %q", got)
	}
	assertRegularFile(t, filepath.Join(dir, GeminiAliasFile))
}

func TestProvision_ReuseAfterResetKeepsLocalPair(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	constitution := "# Project\n"
	if err := os.WriteFile(filepath.Join(dir, AliasFile), []byte(constitution), 0644); err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, dir, AliasFile, "track claude")

	if _, err := Provision(dir, polecatOverlay, LifecycleMarker); err != nil {
		t.Fatal(err)
	}
	assertCanonicalPair(t, dir, LocalCanonicalFile, LocalAliasFile, polecatOverlay)

	if err := os.WriteFile(filepath.Join(dir, AliasFile), []byte(constitution), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := Provision(dir, polecatOverlay, LifecycleMarker)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("reuse after reset should keep the local pair")
	}
	assertCanonicalPair(t, dir, LocalCanonicalFile, LocalAliasFile, polecatOverlay)
}

func TestProvision_ReuseAfterCleanRewritesLocalPair(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	constitution := "# Project\n"
	if err := os.WriteFile(filepath.Join(dir, AliasFile), []byte(constitution), 0644); err != nil {
		t.Fatal(err)
	}
	gitCommitFile(t, dir, AliasFile, "track claude")

	if _, err := Provision(dir, polecatOverlay, LifecycleMarker); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, LocalCanonicalFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, LocalAliasFile)); err != nil {
		t.Fatal(err)
	}

	changed, err := Provision(dir, polecatOverlay, LifecycleMarker)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("reuse after clean should rewrite the local pair")
	}
	assertCanonicalPair(t, dir, LocalCanonicalFile, LocalAliasFile, polecatOverlay)
	got, _ := os.ReadFile(filepath.Join(dir, AliasFile))
	if string(got) != constitution {
		t.Fatal("constitution CLAUDE.md changed after clean repair")
	}
}

func assertCanonicalPair(t *testing.T, dir, canonical, alias, wantContent string) {
	t.Helper()
	canonicalPath := filepath.Join(dir, canonical)
	assertRegularFile(t, canonicalPath)
	got, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read %s: %v", canonical, err)
	}
	if string(got) != wantContent {
		t.Fatalf("%s content = %q, want %q", canonical, got, wantContent)
	}

	target, err := os.Readlink(filepath.Join(dir, alias))
	if err != nil {
		t.Fatalf("readlink %s: %v", alias, err)
	}
	if target != canonical {
		t.Fatalf("%s symlink target = %q, want %q", alias, target, canonical)
	}
}

func assertRegularFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is a symlink, want a regular file", path)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file", path)
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "test")
	runGit(t, dir, "config", "user.email", "test@example.com")
}

func gitCommitFile(t *testing.T, dir, name, message string) {
	t.Helper()
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-m", message)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
