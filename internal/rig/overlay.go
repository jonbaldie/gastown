package rig

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/style"
)

func gasTownIgnorePatterns() []string {
	return []string{
		".runtime/",
		".claude/",
		".opencode/",
		".agents/",
		".logs/",
		"__pycache__/",
		"state.json",
		"CLAUDE.md",
		"CLAUDE.local.md",
		"AGENTS.md",
		"AGENTS.local.md",
		"GEMINI.md",
	}
}

// CopyOverlay copies files from <rigPath>/.runtime/overlay/ to the destination path.
// This allows storing gitignored files (like .env) that services need at their root.
// The overlay is copied non-recursively - only files, not subdirectories.
// File permissions from the source are preserved.
//
// Structure:
//
//	rig/
//	  .runtime/
//	    overlay/
//	      .env          <- Copied to destPath
//	      config.json   <- Copied to destPath
//
// Returns nil if the overlay directory doesn't exist (nothing to copy).
// Individual file copy failures are logged as warnings but don't stop the process.
func CopyOverlay(rigPath, destPath string) error {
	overlayDir := filepath.Join(rigPath, ".runtime", "overlay")

	// Check if overlay directory exists
	entries, err := os.ReadDir(overlayDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No overlay directory - not an error, just nothing to copy
			return nil
		}
		return fmt.Errorf("reading overlay dir: %w", err)
	}

	// Copy each file (not directories) from overlay to destination
	for _, entry := range entries {
		if entry.IsDir() {
			// Skip subdirectories - only copy files at overlay root
			continue
		}

		srcPath := filepath.Join(overlayDir, entry.Name())
		dstPath := filepath.Join(destPath, entry.Name())

		if err := copyFilePreserveMode(srcPath, dstPath); err != nil {
			// Log warning but continue - don't fail spawn for overlay issues
			style.PrintWarning("could not copy overlay file %s: %v", entry.Name(), err)
			continue
		}
	}

	return nil
}

// EnsureGitignorePatterns ensures the .gitignore has required Gas Town patterns.
// This is called after cloning to add patterns that may be missing from the source repo.
func EnsureGitignorePatterns(worktreePath string) error {
	gitignorePath := filepath.Join(worktreePath, ".gitignore")

	// Required patterns for Gas Town worktrees.
	// DO NOT add ".beads/" here. Beads manages its own .beads/.gitignore
	// (created by bd init) which selectively ignores runtime files.
	// Adding .beads/ here overrides that and breaks bd sync.
	// This has regressed twice (PR #753 added it, #891 removed it,
	// #966 re-added it). See overlay_test.go for a regression guard.
	//
	// .claude/ is the broad pattern (covers commands/, settings.json, rules/, etc.).
	// Settings are installed in gastown-managed parent directories via --settings flag,
	// but Cursor still creates .claude/ inside worktrees at runtime. The narrow
	// .claude/commands/ pattern missed other Cursor-created files, causing gt done
	// to fail with "uncommitted changes would be lost" on untracked .claude/ entries.
	existingContent := readFileOrEmpty(gitignorePath)
	missing := missingIgnorePatterns(existingContent, gasTownIgnorePatterns())
	return appendIgnorePatterns(gitignorePath, existingContent, missing, "opening .gitignore")
}

// gasTownLocalExcludePatterns returns the patterns to write to git's
// info/exclude file. This is a superset of gasTownIgnorePatterns() and
// includes .beads/ — which is safe here because info/exclude is never
// committed (unlike .gitignore, where .beads/ must NOT appear because Beads
// manages its own .beads/.gitignore via bd init).
func gasTownLocalExcludePatterns() []string {
	patterns := gasTownIgnorePatterns()
	// .beads/ is excluded from gasTownIgnorePatterns() to avoid breaking bd sync
	// (see EnsureGitignorePatterns comment). info/exclude is safe to include it —
	// it is untracked and invisible to `git status` without affecting the
	// tracked .gitignore (gas-7vg defense-in-depth).
	return append(patterns, ".beads/")
}

// EnsureLocalExcludePatterns writes the standard Gas Town ignore patterns to
// git's info/exclude file so the worktree stays clean without mutating a
// tracked .gitignore. Git stores info/exclude in the common dir, so the
// patterns apply to every worktree of that repository.
func EnsureLocalExcludePatterns(worktreePath string) error {
	excludePath, err := gitLocalExcludePath(worktreePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0755); err != nil {
		return fmt.Errorf("creating local exclude dir: %w", err)
	}
	existingContent, err := readOptionalFile(excludePath)
	if err != nil {
		return fmt.Errorf("reading local exclude: %w", err)
	}
	missing := missingIgnorePatterns(existingContent, gasTownLocalExcludePatterns())
	return appendIgnorePatterns(excludePath, existingContent, missing, "opening local exclude")
}

func readFileOrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func readOptionalFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return string(data), nil
	}
	if os.IsNotExist(err) {
		return "", nil
	}
	return "", err
}

func missingIgnorePatterns(existingContent string, required []string) []string {
	var missing []string
	for _, pattern := range required {
		if !gitignoreContainsPattern(existingContent, pattern) {
			missing = append(missing, pattern)
		}
	}
	return missing
}

func gitignoreContainsPattern(existingContent, pattern string) bool {
	for _, line := range strings.Split(existingContent, "\n") {
		if matchesGitignorePattern(strings.TrimSpace(line), pattern) {
			return true
		}
	}
	return false
}

func appendIgnorePatterns(path, existingContent string, missing []string, openErrPrefix string) error {
	if len(missing) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("%s: %w", openErrPrefix, err)
	}
	defer f.Close()
	if err := writeIgnoreHeader(f, existingContent); err != nil {
		return err
	}
	for _, pattern := range missing {
		if _, err := f.WriteString(pattern + "\n"); err != nil {
			return err
		}
	}
	return nil
}

func writeIgnoreHeader(f *os.File, existingContent string) error {
	if existingContent == "" {
		return nil
	}
	if !strings.HasSuffix(existingContent, "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err := f.WriteString("\n# Gas Town (added by gt)\n")
	return err
}

func gitLocalExcludePath(worktreePath string) (string, error) {
	// --git-path resolves info/exclude to the common dir. Linked worktrees
	// have their own $GIT_DIR under .git/worktrees/<name>, but git reads
	// exclude from the common repository, so writing $GIT_DIR/info/exclude
	// leaves provisioned files such as .agents/ visible in git status.
	cmd := exec.Command("git", "-C", worktreePath, "rev-parse", "--git-path", "info/exclude")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolving git exclude path: %w: %s", err, strings.TrimSpace(string(out)))
	}

	excludePath := strings.TrimSpace(string(out))
	if excludePath == "" {
		return "", fmt.Errorf("empty git exclude path for %s", worktreePath)
	}
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(worktreePath, excludePath)
	}
	return excludePath, nil
}

// matchesGitignorePattern checks if a gitignore line covers the required pattern.
// Handles variant forms (with/without trailing slash, leading slash) and recognizes
// that a broader directory pattern (e.g., ".claude/") covers more specific paths
// (e.g., ".claude/commands/").
func matchesGitignorePattern(line, pattern string) bool {
	// Strip leading slash for comparison
	normLine := strings.TrimPrefix(line, "/")
	normPattern := strings.TrimPrefix(pattern, "/")

	// Exact match or trailing-slash variants
	if normLine == normPattern ||
		normLine == strings.TrimSuffix(normPattern, "/") ||
		normLine+"/" == normPattern {
		return true
	}

	// A broader directory pattern covers more specific paths underneath it.
	// e.g., ".claude/" covers ".claude/commands/"
	if strings.HasSuffix(normLine, "/") && strings.HasPrefix(normPattern, normLine) {
		return true
	}
	// Also handle directory pattern without trailing slash
	if !strings.Contains(normLine, "/") && strings.HasPrefix(normPattern, normLine+"/") {
		return true
	}

	return false
}

// copyFilePreserveMode copies a file from src to dst, preserving the source file's permissions.
func copyFilePreserveMode(src, dst string) error {
	// Get source file info for permissions
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	// Open source file
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer srcFile.Close()

	// Create destination file with same permissions
	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}

	// Copy contents
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		return fmt.Errorf("copy contents: %w", err)
	}

	// Explicitly check Close() — on many filesystems, buffered data is flushed
	// at Close() time, so a full-disk error surfaces here, not during Write.
	if err := dstFile.Close(); err != nil {
		return fmt.Errorf("closing destination: %w", err)
	}

	return nil
}
