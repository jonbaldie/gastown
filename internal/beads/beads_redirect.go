// Package beads provides redirect resolution for beads databases.
package beads

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ResolveBeadsDir returns the actual beads directory, following any redirect.
// If workDir/.beads/redirect exists, it reads the redirect path and resolves it
// relative to workDir (not the .beads directory). Otherwise, returns workDir/.beads.
//
// This is essential for crew workers and polecats that use shared beads via redirect.
// The redirect file contains a relative path like "../../mayor/rig/.beads".
//
// Example: if we're at crew/max/ and .beads/redirect contains "../../mayor/rig/.beads",
// the redirect is resolved from crew/max/ (not crew/max/.beads/), giving us
// mayor/rig/.beads at the rig root level.
//
// Circular redirect detection: If the resolved path equals the original beads directory,
// this indicates an errant redirect file that should be removed. The function logs a
// warning and returns the original beads directory.
func ResolveBeadsDir(workDir string) string {
	if filepath.Base(workDir) == ".beads" {
		workDir = filepath.Dir(workDir)
	}
	beadsDir := filepath.Join(workDir, ".beads")
	redirectPath := filepath.Join(beadsDir, "redirect")

	// Check for redirect file
	data, err := os.ReadFile(redirectPath) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		// No redirect, use local .beads
		return beadsDir
	}

	// Read and clean the redirect path
	redirectTarget := strings.TrimSpace(string(data))
	if redirectTarget == "" {
		return beadsDir
	}

	// Resolve redirect target. Absolute paths are used as-is;
	// relative paths are resolved from workDir.
	var resolved string
	if filepath.IsAbs(redirectTarget) {
		resolved = filepath.Clean(redirectTarget)
	} else {
		resolved = filepath.Clean(filepath.Join(workDir, redirectTarget))
	}

	// Detect circular redirects: if resolved path equals original beads dir,
	// this is an errant redirect file (e.g., redirect in mayor/rig/.beads pointing to itself)
	if resolved == beadsDir {
		fmt.Fprintf(os.Stderr, "Warning: circular redirect detected in %s (points to itself), ignoring redirect\n", redirectPath)
		// Remove the errant redirect file to prevent future warnings
		if err := os.Remove(redirectPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not remove errant redirect file: %v\n", err)
		}
		return beadsDir
	}

	// Follow redirect chains (e.g., crew/.beads -> rig/.beads -> mayor/rig/.beads)
	// This is intentional for the rig-level redirect architecture.
	// Limit depth to prevent infinite loops from misconfigured redirects.
	return resolveBeadsDirWithDepth(resolved, 3)
}

// resolveBeadsDirWithDepth follows redirect chains with a depth limit.
func resolveBeadsDirWithDepth(beadsDir string, maxDepth int) string {
	if maxDepth <= 0 {
		fmt.Fprintf(os.Stderr, "Warning: redirect chain too deep at %s, stopping\n", beadsDir)
		return beadsDir
	}

	redirectPath := filepath.Join(beadsDir, "redirect")
	data, err := os.ReadFile(redirectPath) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		// No redirect, this is the final destination
		return beadsDir
	}

	redirectTarget := strings.TrimSpace(string(data))
	if redirectTarget == "" {
		return beadsDir
	}

	// Resolve redirect target. Absolute paths are used as-is;
	// relative paths are resolved from parent of beadsDir.
	workDir := filepath.Dir(beadsDir)
	var resolved string
	if filepath.IsAbs(redirectTarget) {
		resolved = filepath.Clean(redirectTarget)
	} else {
		resolved = filepath.Clean(filepath.Join(workDir, redirectTarget))
	}

	// Detect circular redirect
	if resolved == beadsDir {
		fmt.Fprintf(os.Stderr, "Warning: circular redirect detected in %s, stopping\n", redirectPath)
		return beadsDir
	}

	// Recursively follow
	return resolveBeadsDirWithDepth(resolved, maxDepth-1)
}

// cleanBeadsRuntimeFiles removes redirect-local runtime and identity files from a
// .beads directory while preserving tracked docs/formula surfaces (formulas/,
// README.md, .gitignore). Identity files next to a redirect can make bd bind to
// the wrong database, so tracked identity files are hidden before removal.
// This is safe to call even if the directory doesn't exist.
func cleanBeadsRuntimeFiles(beadsDir string) error {
	exists, err := beadsDirectoryExists(beadsDir)
	if err != nil || !exists {
		return err
	}
	worktreePath := filepath.Dir(beadsDir)
	if err := removeWorktreeIdentityFiles(worktreePath, beadsDir); err != nil {
		return err
	}
	return removeBeadsRuntimeFiles(beadsDir)
}

func beadsDirectoryExists(beadsDir string) (bool, error) {
	info, err := os.Lstat(beadsDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

func removeWorktreeIdentityFiles(worktreePath, beadsDir string) error {
	for _, name := range []string{"metadata.json", "config.yaml"} {
		if err := removeWorktreeIdentityFile(worktreePath, filepath.Join(beadsDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func removeBeadsRuntimeFiles(beadsDir string) error {
	runtimePatterns := []string{
		// Daemon runtime
		"daemon.lock", "daemon.log", "daemon.pid", "bd.sock",
		// Sync state
		"last-touched",
		// Version tracking
		".local_version",
		// Redirect file (we're about to recreate it)
		"redirect",
		// Runtime directories
		"mq",
	}

	var firstErr error
	for _, pattern := range runtimePatterns {
		matches, err := filepath.Glob(filepath.Join(beadsDir, pattern))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, match := range matches {
			if err := os.RemoveAll(match); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

func removeWorktreeIdentityFile(worktreePath, path string) error {
	present, err := identityFilePresent(path)
	if err != nil || !present {
		return err
	}
	rel, err := safeWorktreeRelativePath(worktreePath, path)
	if err != nil {
		return err
	}
	if err := hideTrackedIdentityFile(worktreePath, rel); err != nil {
		return err
	}
	return removeIdentityFile(path)
}

func identityFilePresent(path string) (bool, error) {
	_, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking %s: %w", path, err)
	}
	return true, nil
}

func safeWorktreeRelativePath(worktreePath, path string) (string, error) {
	rel, err := filepath.Rel(worktreePath, path)
	if err != nil {
		return "", fmt.Errorf("computing git path for %s: %w", path, err)
	}
	if rel == "." || rel == "" || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to clean identity file outside worktree: %s", path)
	}
	return filepath.ToSlash(rel), nil
}

func hideTrackedIdentityFile(worktreePath, relPath string) error {
	tracked, err := gitPathTracked(worktreePath, relPath)
	if err != nil {
		return err
	}
	if tracked {
		return markGitPathSkipWorktree(worktreePath, relPath)
	}
	return nil
}

func removeIdentityFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

func gitPathTracked(worktreePath, relPath string) (bool, error) {
	cmd := exec.Command("git", "-C", worktreePath, "ls-files", "--stage", "--", relPath) //nolint:gosec // argv is fixed; relPath is passed after --.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git ls-files %s: %w%s", relPath, err, gitOutputSuffix(out))
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

func markGitPathSkipWorktree(worktreePath, relPath string) error {
	cmd := exec.Command("git", "-C", worktreePath, "update-index", "--skip-worktree", "--", relPath) //nolint:gosec // argv is fixed; relPath is passed after --.
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git update-index --skip-worktree %s: %w%s", relPath, err, gitOutputSuffix(out))
	}
	return nil
}

func gitOutputSuffix(out []byte) string {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return ""
	}
	return ": " + trimmed
}

// ComputeRedirectTarget computes the expected redirect target for a worktree.
// This is the canonical function for determining what a redirect should contain.
// Both SetupRedirect and doctor checks should use this to stay in sync.
//
// Parameters:
//   - townRoot: the town root directory (e.g., ~/gt)
//   - worktreePath: the worktree directory (e.g., <rig>/crew/<name> or <rig>/refinery/rig)
//
// Returns the redirect target path (e.g., "../../.beads" or "../../mayor/rig/.beads"),
// or an error if the path is invalid or no beads location exists.
func ComputeRedirectTarget(townRoot, worktreePath string) (string, error) {
	location, err := redirectWorktreeLocation(townRoot, worktreePath)
	if err != nil {
		return "", err
	}
	paths := newRedirectPaths(location)

	// Check rig-level .beads first: if the rig has its own database
	// (metadata.json with dolt_database), crew must use rig-level beads
	// so they see the correct prefix (e.g., lc- for laneassist, not hq-).
	// If the rig-level .beads is itself a redirect, flatten it here: bd does
	// not support redirect chains and will ignore the worktree redirect.
	if rigHasOwnDB(paths.rigBeads) {
		return ownRigRedirectTarget(location, paths), nil
	}

	// Rig has no own database — try town-level .beads (has routes.jsonl,
	// config.yaml, Dolt server info, and hq- prefix).
	// Only use town-level beads if the rig doesn't have its own redirect chain.
	// Rigs using Dolt server (not embedded DB) have a .beads/redirect file pointing
	// to mayor/rig/.beads — this must take priority over the town fallback.
	if townHasBeadsDatabase(paths.townBeads) && !redirectFileExists(paths.rigBeads) {
		return location.upPath() + "../.beads", nil
	}

	return fallbackRedirectTarget(location, paths)
}

type redirectLocation struct {
	townRoot string
	parts    []string
}

func redirectWorktreeLocation(townRoot, worktreePath string) (redirectLocation, error) {
	townRootAbs, err := filepath.Abs(townRoot)
	if err != nil {
		return redirectLocation{}, fmt.Errorf("resolving town root: %w", err)
	}
	worktreeAbs, err := filepath.Abs(worktreePath)
	if err != nil {
		return redirectLocation{}, fmt.Errorf("resolving worktree path: %w", err)
	}
	relPath, err := worktreePathWithinTown(townRootAbs, worktreeAbs, townRoot, worktreePath)
	if err != nil {
		return redirectLocation{}, err
	}
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	if len(parts) < 2 {
		return redirectLocation{}, fmt.Errorf("invalid worktree path: must be at least 2 levels deep from town root")
	}
	if parts[0] == "mayor" || parts[1] == "mayor" {
		return redirectLocation{}, fmt.Errorf("cannot create redirect in canonical beads location (mayor/rig)")
	}
	return redirectLocation{townRoot: townRootAbs, parts: parts}, nil
}

func worktreePathWithinTown(townRootAbs, worktreeAbs, townRoot, worktreePath string) (string, error) {
	if worktreeAbs == townRootAbs {
		return "", fmt.Errorf("cannot create redirect at town root")
	}
	relPath, err := filepath.Rel(townRootAbs, worktreeAbs)
	if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("worktree path %s is outside town root %s", worktreePath, townRoot)
	}
	return relPath, nil
}

func (l redirectLocation) upPath() string { return strings.Repeat("../", len(l.parts)-1) }

type redirectPaths struct {
	townBeads  string
	rigBeads   string
	mayorBeads string
}

func newRedirectPaths(location redirectLocation) redirectPaths {
	rigRoot := filepath.Join(location.townRoot, location.parts[0])
	return redirectPaths{
		townBeads:  filepath.Join(location.townRoot, ".beads"),
		rigBeads:   filepath.Join(rigRoot, ".beads"),
		mayorBeads: filepath.Join(rigRoot, "mayor", "rig", ".beads"),
	}
}

func ownRigRedirectTarget(location redirectLocation, paths redirectPaths) string {
	if target, ok := directRigRedirectTarget(location.upPath(), filepath.Join(paths.rigBeads, "redirect")); ok {
		return target
	}
	return location.upPath() + ".beads"
}

func townHasBeadsDatabase(townBeadsPath string) bool {
	info, err := os.Stat(townBeadsPath)
	if err != nil || !info.IsDir() {
		return false
	}
	return pathExists(filepath.Join(townBeadsPath, "dolt")) || pathExists(filepath.Join(townBeadsPath, "config.yaml"))
}

func redirectFileExists(rigBeadsPath string) bool {
	return pathExists(filepath.Join(rigBeadsPath, "redirect"))
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fallbackRedirectTarget(location redirectLocation, paths redirectPaths) (string, error) {
	rigBeadsExists := pathExists(paths.rigBeads)
	rigHasDatabase := pathExists(filepath.Join(paths.rigBeads, "dolt")) || redirectFileExists(paths.rigBeads)
	if !rigBeadsExists || !rigHasDatabase {
		if _, err := os.Stat(paths.mayorBeads); !os.IsNotExist(err) {
			return location.upPath() + "mayor/rig/.beads", nil
		}
		if !rigBeadsExists {
			return "", fmt.Errorf("no beads found at %s, %s, or %s", paths.townBeads, paths.rigBeads, paths.mayorBeads)
		}
	}
	return ownRigRedirectTarget(location, paths), nil
}

func directRigRedirectTarget(upPath, rigRedirectPath string) (string, bool) {
	data, err := os.ReadFile(rigRedirectPath) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		return "", false
	}
	rigRedirectTarget := strings.TrimSpace(string(data))
	if rigRedirectTarget == "" {
		return "", false
	}
	if filepath.IsAbs(rigRedirectTarget) {
		// Absolute redirect — pass through as-is (ResolveBeadsDir handles it).
		return rigRedirectTarget, true
	}
	// Relative redirect (e.g., "mayor/rig/.beads" for tracked beads).
	// Redirect worktree directly to the final destination.
	return upPath + rigRedirectTarget, true
}

// SetupRedirect creates a .beads/redirect file for a worktree to point to the rig's shared beads.
// This is used by crew, polecats, and refinery worktrees to share the rig's beads database.
//
// Parameters:
//   - townRoot: the town root directory (e.g., ~/gt)
//   - worktreePath: the worktree directory (e.g., <rig>/crew/<name> or <rig>/refinery/rig)
//
// The function:
//  1. Computes the relative path from worktree to rig-level .beads
//  2. Cleans up runtime files (preserving tracked files like formulas/)
//  3. Creates the redirect file
//
// Safety: This function refuses to create redirects in the canonical beads location
// (mayor/rig) to prevent circular redirect chains.
func SetupRedirect(townRoot, worktreePath string) error {
	redirectPath, err := ComputeRedirectTarget(townRoot, worktreePath)
	if err != nil {
		return err
	}
	warnMayorFallback(townRoot, worktreePath, redirectPath)
	worktreeBeadsDir := filepath.Join(worktreePath, ".beads")
	if err := prepareRedirectDirectory(worktreeBeadsDir); err != nil {
		return err
	}
	return writeBeadsRedirect(worktreeBeadsDir, redirectPath)
}

func warnMayorFallback(townRoot, worktreePath, redirectPath string) {
	// Warn only when using mayor fallback WITHOUT a redirect file.
	// When rig/.beads/redirect exists pointing to mayor/rig/.beads, that's the
	// intended configuration for tracked beads — not a fallback worth warning about.
	if !strings.Contains(redirectPath, "mayor/rig/.beads") {
		return
	}
	relPath, _ := filepath.Rel(townRoot, worktreePath)
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	rigRoot := filepath.Join(townRoot, parts[0])
	rigRedirectPath := filepath.Join(rigRoot, ".beads", "redirect")
	if _, err := os.Stat(rigRedirectPath); !os.IsNotExist(err) {
		return
	}
	// No redirect file — this is an unexpected fallback.
	rigBeadsPath := filepath.Join(rigRoot, ".beads")
	mayorBeadsPath := filepath.Join(rigRoot, "mayor", "rig", ".beads")
	fmt.Fprintf(os.Stderr, "Warning: rig .beads not found at %s, using %s\n", rigBeadsPath, mayorBeadsPath)
	fmt.Fprintf(os.Stderr, "  Run 'bd doctor' to fix rig beads configuration\n")
}

func prepareRedirectDirectory(worktreeBeadsDir string) error {
	// Clean up runtime/identity files in .beads/ but preserve tracked docs (formulas/, README.md, etc.)
	// Handle edge cases: if .beads exists as a file or symlink, remove that path.
	// This can happen with stale state from previous failed operations or
	// unusual clone state. MkdirAll would fail with "file exists" in this case.
	if info, err := os.Lstat(worktreeBeadsDir); err == nil && !info.IsDir() {
		if err := os.Remove(worktreeBeadsDir); err != nil {
			return fmt.Errorf("removing stale .beads file: %w", err)
		}
	}

	if err := cleanBeadsRuntimeFiles(worktreeBeadsDir); err != nil {
		return fmt.Errorf("cleaning runtime files: %w", err)
	}

	// Create .beads directory if it doesn't exist
	if err := EnsureDir(worktreeBeadsDir); err != nil {
		return err
	}
	return nil
}

func writeBeadsRedirect(worktreeBeadsDir, redirectPath string) error {
	redirectFile := filepath.Join(worktreeBeadsDir, "redirect")
	if err := os.WriteFile(redirectFile, []byte(redirectPath+"\n"), 0644); err != nil {
		return fmt.Errorf("creating redirect file: %w", err)
	}

	return nil
}

// IsLocalBeadsDir returns true if resolvedPath is the cwd's own .beads/ directory
// (i.e., no redirect was followed). This indicates the beads client will write to
// a local database that other agents (e.g., the Refinery) will never read.
func IsLocalBeadsDir(cwd, resolvedPath string) bool {
	localBeads := filepath.Join(cwd, ".beads")
	cleanResolved, _ := filepath.Abs(resolvedPath)
	cleanLocal, _ := filepath.Abs(localBeads)
	return cleanResolved == cleanLocal
}

// rigHasOwnDB checks if a rig's .beads/metadata.json declares its own
// dolt_database. Rigs with their own database (e.g., laneassist with "lc-"
// prefix) must not be redirected to town-level beads ("hq-" prefix).
func rigHasOwnDB(rigBeadsPath string) bool {
	metadataPath := filepath.Join(rigBeadsPath, "metadata.json")
	data, err := os.ReadFile(metadataPath) //nolint:gosec // G304: trusted beads path
	if err != nil {
		return false
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return false
	}
	return meta.DoltDatabase != ""
}
