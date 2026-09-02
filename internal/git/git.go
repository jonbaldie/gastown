// Package git provides a wrapper for git operations via subprocess.
package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/util"
)

// ErrRemoteNotConfigured reports that a named remote has no stored URL.
var ErrRemoteNotConfigured = errors.New("remote is not configured")

var errNoComparisonRefs = errors.New("no comparison refs resolved")

// GitError contains raw output from a git command for agent observation.
// ZFC: Callers observe the raw output and decide what to do.
// The error interface methods provide human-readable messages, but agents
// should use Stdout/Stderr for programmatic observation.
type GitError struct {
	Command string // The git command that failed (e.g., "merge", "push")
	Args    []string
	Stdout  string // Raw stdout output
	Stderr  string // Raw stderr output
	Err     error  // Underlying error (e.g., exit code)
}

func (e *GitError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("git %s: %s", e.Command, e.Stderr)
	}
	return fmt.Sprintf("git %s: %v", e.Command, e.Err)
}

func (e *GitError) Unwrap() error {
	return e.Err
}

// moveDir moves a directory from src to dest. It first tries os.Rename for
// efficiency, but falls back to copy+delete if src and dest are on different
// filesystems (which causes EXDEV error on rename).
func moveDir(src, dest string) error {
	// Try rename first - works if same filesystem
	if err := os.Rename(src, dest); err == nil {
		return nil
	}

	// Rename failed, use platform-specific copy for cross-filesystem moves
	if err := copyDirPreserving(src, dest); err != nil {
		return fmt.Errorf("copying directory: %w", err)
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("removing source after copy: %w", err)
	}
	return nil
}

// Git wraps git operations for a working directory.
type Git struct {
	workDir string
	gitDir  string // Optional: explicit git directory (for bare repos)
}

// ErrUnsafeTownRootGitMutation is returned when a mutating git operation would
// act on the Gas Town town-root repository or town-root runtime paths.
var ErrUnsafeTownRootGitMutation = errors.New("unsafe git mutation targets Gas Town town root")

// NewGit creates a new Git wrapper for the given directory.
func NewGit(workDir string) *Git {
	return &Git{workDir: workDir}
}

// NewGitWithDir creates a Git wrapper with an explicit git directory.
// This is used for bare repos where gitDir points to the .git directory
// and workDir may be empty or point to a worktree.
func NewGitWithDir(gitDir, workDir string) *Git {
	return &Git{gitDir: gitDir, workDir: workDir}
}

// WorkDir returns the working directory for this Git instance.
func WorkDir(g *Git) string {
	return g.workDir
}

// IsRepo returns true if the workDir is a git repository.
func IsRepo(g *Git) bool {
	_, err := run(g, "rev-parse", "--git-dir")
	return err == nil
}

// run executes a git command and returns trimmed stdout.
func run(g *Git, args ...string) (string, error) {
	out, err := runOutput(g, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// runOutput executes a git command and returns stdout without trimming.
// Porcelain status lines start with a space for unstaged worktree edits
// (" M file"). TrimSpace would turn that into "M file" and drop the first
// path character during parse.
func runOutput(g *Git, args ...string) (string, error) {
	if err := guardUnsafeTownRootMutation(g, args); err != nil {
		return "", err
	}

	// If gitDir is set (bare repo), prepend --git-dir flag
	if g.gitDir != "" {
		args = append([]string{"--git-dir=" + g.gitDir}, args...)
	}

	cmd := exec.Command("git", args...)
	util.SetDetachedProcessGroup(cmd)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", wrapError(err, stdout.String(), stderr.String(), args)
	}

	return stdout.String(), nil
}

// pushTimeout is the maximum time a git push is allowed to run before being
// killed. This prevents gt done from hanging indefinitely when the remote
// (e.g. GitLab) is unreachable or slow.
const pushTimeout = 60 * time.Second

// runWithTimeout executes a git command with a deadline. If the command does
// not finish within the timeout, the process is killed and an error is returned.
func runWithTimeout(g *Git, timeout time.Duration, args ...string) (_ string, _ error) { //nolint:unparam // string return kept for consistency with Run()
	if err := guardUnsafeTownRootMutation(g, args); err != nil {
		return "", err
	}

	if g.gitDir != "" {
		args = append([]string{"--git-dir=" + g.gitDir}, args...)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	util.SetDetachedProcessGroup(cmd)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("git %s timed out after %v (remote may be unreachable)", args[0], timeout)
		}
		return "", wrapError(err, stdout.String(), stderr.String(), args)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// runWithEnv executes a git command with additional environment variables.
func runWithEnv(g *Git, args []string, extraEnv []string) (_ string, _ error) { //nolint:unparam // string return kept for consistency with Run()
	return runWithEnvAndTimeout(g, args, extraEnv, 0)
}

// runWithEnvAndTimeout executes a git command with extra env vars and an
// optional timeout. Pass 0 for no timeout.
func runWithEnvAndTimeout(g *Git, args []string, extraEnv []string, timeout time.Duration) (_ string, _ error) {
	if err := guardUnsafeTownRootMutation(g, args); err != nil {
		return "", err
	}
	args = prependGitDir(g, args)
	cmd, cancel := gitCommandWithTimeout(args, timeout)
	if cancel != nil {
		defer cancel()
	}
	applyGitCommandEnv(cmd, g, extraEnv)
	return finishGitCommand(cmd, args, timeout)
}

func prependGitDir(g *Git, args []string) []string {
	if g.gitDir == "" {
		return args
	}
	return append([]string{"--git-dir=" + g.gitDir}, args...)
}

func gitCommandWithTimeout(args []string, timeout time.Duration) (*exec.Cmd, context.CancelFunc) {
	if timeout <= 0 {
		return exec.Command("git", args...), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	return exec.CommandContext(ctx, "git", args...), cancel
}

func applyGitCommandEnv(cmd *exec.Cmd, g *Git, extraEnv []string) {
	util.SetDetachedProcessGroup(cmd)
	if g.workDir != "" {
		cmd.Dir = g.workDir
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
}

func finishGitCommand(cmd *exec.Cmd, args []string, timeout time.Duration) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", gitCommandRunError(err, stdout.String(), stderr.String(), args, timeout)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func gitCommandRunError(err error, stdout, stderr string, args []string, timeout time.Duration) error {
	if timeout > 0 && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("git %s timed out after %v (remote may be unreachable)", args[0], timeout)
	}
	return wrapError(err, stdout, stderr, args)
}

func guardUnsafeTownRootMutation(g *Git, args []string) error {
	cmd, rest := gitSubcommand(args)
	if cmd == "" {
		return nil
	}
	effectiveWorkDir := gitEffectiveWorkDir(args, g.workDir)

	if gitSubcommandMutatesWorktree(cmd, rest) {
		if err := EnsureSafeMutationWorkDir(effectiveWorkDir); err != nil {
			return fmt.Errorf("%w: git %s", err, strings.Join(args, " "))
		}
	}

	for _, target := range protectedWorktreeTargets(cmd, rest, effectiveWorkDir) {
		return fmt.Errorf("%w: git worktree target %s", ErrUnsafeTownRootGitMutation, target)
	}

	return nil
}

func gitEffectiveWorkDir(args []string, workDir string) string {
	effective := workDir
	argCount := len(args)
	for i := 0; i < argCount; i++ {
		next, skip, done := gitWorkDirArg(args, i, effective)
		if done {
			return effective
		}
		effective = next
		i += skip
	}
	return effective
}

func gitWorkDirArg(args []string, i int, effective string) (string, int, bool) {
	if path, skip, ok := gitWorkTreePathArg(args, i); ok {
		return gitPathAbs(path, effective), skip, false
	}
	arg := args[i]
	if gitWorkDirValueSkip(arg) {
		return effective, 1, false
	}
	if gitEqualsGlobal(arg) || strings.HasPrefix(arg, "-") {
		return effective, 0, false
	}
	return effective, 0, true
}

func gitWorkTreePathArg(args []string, i int) (string, int, bool) {
	arg := args[i]
	if arg == "-C" && i+1 < len(args) {
		return args[i+1], 1, true
	}
	if arg == "--work-tree" && i+1 < len(args) {
		return args[i+1], 1, true
	}
	if strings.HasPrefix(arg, "--work-tree=") {
		return strings.TrimPrefix(arg, "--work-tree="), 0, true
	}
	return "", 0, false
}

func gitWorkDirValueSkip(arg string) bool {
	switch arg {
	case "-c", "--git-dir", "--namespace", "--config-env", "--exec-path":
		return true
	default:
		return false
	}
}

func gitTakesValueGlobal(arg string) bool {
	switch arg {
	case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env", "--exec-path":
		return true
	default:
		return false
	}
}

func gitEqualsGlobal(arg string) bool {
	return strings.HasPrefix(arg, "--git-dir=") ||
		strings.HasPrefix(arg, "--work-tree=") ||
		strings.HasPrefix(arg, "--namespace=") ||
		strings.HasPrefix(arg, "--config-env=") ||
		strings.HasPrefix(arg, "--exec-path=")
}

// EnsureSafeMutationWorkDir fails when workDir's effective git worktree is the
// Gas Town town root. Raw git callsites use this before mutating commands.
func EnsureSafeMutationWorkDir(workDir string) error {
	if workDir == "" {
		return nil
	}

	topLevel, ok := gitTopLevel(workDir)
	if !ok {
		return nil
	}
	if isTownRoot(topLevel) {
		return fmt.Errorf("%w: %s resolves to town root git worktree %s", ErrUnsafeTownRootGitMutation, workDir, topLevel)
	}
	return nil
}

func gitTopLevel(workDir string) (string, bool) {
	cmd := exec.Command("git", "-C", workDir, "rev-parse", "--show-toplevel")
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	topLevel := strings.TrimSpace(string(out))
	if topLevel == "" {
		return "", false
	}
	abs, err := filepath.Abs(topLevel)
	if err != nil {
		return filepath.Clean(topLevel), true
	}
	return filepath.Clean(abs), true
}

func isTownRoot(path string) bool {
	return fileExists(filepath.Join(path, "mayor", "town.json")) || fileExists(filepath.Join(path, "mayor", "rigs.json"))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func gitSubcommand(args []string) (string, []string) {
	argCount := len(args)
	for i := 0; i < argCount; i++ {
		skip, cmd := gitGlobalArg(args[i])
		if cmd != "" {
			return cmd, args[i+1:]
		}
		i += skip
	}
	return "", nil
}

func gitGlobalArg(arg string) (int, string) {
	if gitTakesValueGlobal(arg) {
		return 1, ""
	}
	if gitEqualsGlobal(arg) || gitBareGlobal(arg) || strings.HasPrefix(arg, "-") {
		return 0, ""
	}
	return 0, arg
}

func gitBareGlobal(arg string) bool {
	switch arg {
	case "--no-pager", "--bare", "--literal-pathspecs", "--no-replace-objects":
		return true
	default:
		return false
	}
}

func gitSubcommandMutatesWorktree(cmd string, args []string) bool {
	switch cmd {
	case "checkout", "switch", "restore", "reset", "clean", "merge", "rebase", "pull", "rm", "mv", "cherry-pick", "revert", "am", "apply", "checkout-index", "read-tree", "sparse-checkout":
		return true
	case "stash":
		return stashArgsMutate(args)
	case "submodule":
		return submoduleArgsMutate(args)
	case "branch":
		return branchArgsMutate(args)
	case "worktree":
		return worktreeArgsMutate(args)
	case "symbolic-ref":
		return symbolicRefArgsMutate(args)
	case "update-ref":
		return true
	default:
		return false
	}
}

func stashArgsMutate(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "list", "show":
		return false
	default:
		return true
	}
}

func submoduleArgsMutate(args []string) bool {
	cmd := firstNonOptionSubcommand(args)
	if cmd == "" {
		return false
	}
	switch cmd {
	case "update", "add", "deinit", "sync", "set-url", "set-branch", "absorbgitdirs":
		return true
	default:
		return false
	}
}

func firstNonOptionSubcommand(args []string) string {
	argCount := len(args)
	for i := 0; i < argCount; i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}

func branchArgsMutate(args []string) bool {
	if len(args) == 0 {
		return false
	}
	readOnly := false
	for _, arg := range args {
		switch arg {
		case "--show-current", "--list", "-l", "-r", "-a", "--contains", "--merged", "--no-merged", "--points-at":
			readOnly = true
		case "-d", "-D", "-f", "-m", "-M", "-c", "-C", "--delete", "--move", "--copy", "--force", "--set-upstream-to", "--unset-upstream", "--track":
			return true
		}
		if strings.HasPrefix(arg, "--format") {
			readOnly = true
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return !readOnly
	}
	return false
}

func worktreeArgsMutate(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "add", "remove", "move", "prune":
		return true
	default:
		return false
	}
}

func symbolicRefArgsMutate(args []string) bool {
	nonOptions := 0
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			nonOptions++
		}
	}
	return nonOptions > 1
}

func protectedWorktreeTargets(cmd string, args []string, baseDir string) []string {
	targets := worktreeCommandTargets(cmd, args)
	protected := make([]string, 0, len(targets))
	for _, target := range targets {
		abs := gitPathAbs(target, baseDir)
		if protectedTownRuntimePath(abs) {
			protected = append(protected, abs)
		}
	}
	return protected
}

func worktreeCommandTargets(cmd string, args []string) []string {
	if cmd != "worktree" || len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "add":
		return nonEmptyStrings(firstWorktreeAddTarget(args[1:]))
	case "remove":
		return nonEmptyStrings(firstNonOptionPath(args[1:], nil))
	case "move":
		return nonOptionPaths(args[1:], nil, 2)
	default:
		return nil
	}
}

func nonEmptyStrings(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

func firstWorktreeAddTarget(args []string) string {
	valueOptions := map[string]bool{"-b": true, "-B": true, "--orphan": true, "--reason": true}
	return firstNonOptionPath(args, valueOptions)
}

func firstNonOptionPath(args []string, valueOptions map[string]bool) string {
	paths := nonOptionPaths(args, valueOptions, 1)
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

func nonOptionPaths(args []string, valueOptions map[string]bool, limit int) []string {
	paths := make([]string, 0, limit)
	argCount := len(args)
	for i := 0; i < argCount; i++ {
		arg := args[i]
		if valueOptions[arg] {
			i++
			continue
		}
		if strings.HasPrefix(arg, "--reason=") || strings.HasPrefix(arg, "--orphan=") {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		paths = append(paths, arg)
		if len(paths) == limit {
			return paths
		}
	}
	return paths
}

func gitPathAbs(path, baseDir string) string {
	if baseDir == "" {
		baseDir = "."
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(resolveExistingSymlinkAncestors(abs))
}

func resolveExistingSymlinkAncestors(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil || rel == "." {
				return resolved
			}
			return filepath.Join(resolved, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
	}
}

func protectedTownRuntimePath(path string) bool {
	abs := filepath.Clean(path)
	for dir := abs; ; dir = filepath.Dir(dir) {
		if isTownRoot(dir) {
			if samePath(abs, dir) {
				return true
			}
			rel, err := filepath.Rel(dir, abs)
			if err != nil {
				return false
			}
			first := rel
			if idx := strings.IndexRune(rel, filepath.Separator); idx >= 0 {
				first = rel[:idx]
			}
			switch first {
			case "mayor", ".dolt-data", ".runtime", ".beads", "daemon":
				return true
			default:
				return false
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
	}
}

func samePath(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}

// wrapError wraps git errors with context.
// ZFC: Returns GitError with raw output for agent observation.
// Does not detect or interpret error types - agents should observe and decide.
func wrapError(err error, stdout, stderr string, args []string) error {
	stdout = strings.TrimSpace(stdout)
	stderr = strings.TrimSpace(stderr)

	// Determine command name (first arg, or first non-flag arg)
	command := ""
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			command = arg
			break
		}
	}
	if command == "" && len(args) > 0 {
		command = args[0]
	}

	return &GitError{
		Command: command,
		Args:    args,
		Stdout:  stdout,
		Stderr:  stderr,
		Err:     err,
	}
}

// cloneOptions configures a clone operation for cloneInternal.
type cloneOptions struct {
	bare         bool   // Pass --bare to git clone
	reference    string // Pass --reference-if-able <path> to git clone
	singleBranch bool   // Pass --single-branch to git clone (only fetch default branch)
	depth        int    // Pass --depth N to git clone (shallow clone); 0 means full history
	branch       string // Pass --branch <name> to git clone (checkout specific branch)
	filter       string // Pass --filter=<spec> to git clone (e.g. "blob:none", "tree:0")
}

type cloneDirs struct {
	TmpDir  string
	TmpDest string
	Cleanup func()
}

// cloneInternal runs `git clone` in an isolated temp directory, moves the result
// to dest, and applies post-clone configuration (hooks or refspec).
func cloneInternal(_ *Git, url, dest string, opts cloneOptions) error {
	dest = gitPathAbs(dest, "")
	dirs, err := prepareCloneDirs(dest)
	if err != nil {
		return err
	}
	defer dirs.Cleanup()
	if err := runCloneCommand(url, dirs.TmpDest, dirs.TmpDir, opts); err != nil {
		return err
	}
	if err := moveDir(dirs.TmpDest, dest); err != nil {
		return fmt.Errorf("moving clone to destination: %w", err)
	}
	return configureClone(dest, opts)
}

func prepareCloneDirs(dest string) (cloneDirs, error) {
	if protectedTownRuntimePath(dest) {
		return cloneDirs{}, fmt.Errorf("%w: clone destination %s", ErrUnsafeTownRootGitMutation, dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return cloneDirs{}, fmt.Errorf("creating destination parent: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "gt-clone-*")
	if err != nil {
		return cloneDirs{}, fmt.Errorf("creating temp dir: %w", err)
	}
	return cloneDirs{
		TmpDir:  tmpDir,
		TmpDest: filepath.Join(tmpDir, filepath.Base(dest)),
		Cleanup: func() { _ = os.RemoveAll(tmpDir) },
	}, nil
}

func cloneArgs(url, tmpDest string, opts cloneOptions) []string {
	var args []string
	args = appendCloneProtocolArgs(args, url, opts)
	args = append(args, "clone")
	args = appendCloneOptionArgs(args, opts)
	return append(args, url, tmpDest)
}

func appendCloneProtocolArgs(args []string, url string, opts cloneOptions) []string {
	if strings.HasPrefix(url, "file://") || opts.reference != "" {
		args = append(args, "-c", "protocol.file.allow=always")
	}
	if opts.reference != "" && !opts.bare && runtime.GOOS == "windows" {
		args = append(args, "-c", "core.symlinks=true")
	}
	return args
}

func appendCloneOptionArgs(args []string, opts cloneOptions) []string {
	if opts.bare {
		args = append(args, "--bare")
	}
	if opts.singleBranch {
		args = append(args, "--single-branch")
	}
	if opts.filter != "" {
		args = append(args, "--filter="+opts.filter)
	}
	if opts.depth > 0 {
		args = append(args, "--depth", fmt.Sprintf("%d", opts.depth))
	}
	if opts.branch != "" {
		args = append(args, "--branch", opts.branch)
	}
	if opts.reference != "" {
		args = append(args, "--reference-if-able", opts.reference)
	}
	return args
}

func runCloneCommand(url, tmpDest, tmpDir string, opts cloneOptions) error {
	args := cloneArgs(url, tmpDest, opts)
	cmd := exec.Command("git", args...)
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = tmpDir
	cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+tmpDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return wrapError(err, stdout.String(), stderr.String(), args)
	}
	return nil
}

func configureClone(dest string, opts cloneOptions) error {
	if opts.bare {
		return configureRefspec(dest, opts.singleBranch)
	}
	if err := configureHooksPath(dest); err != nil {
		return err
	}
	return InitSubmodules(dest)
}

// Clone clones a repository to the destination.
// Uses --single-branch --depth 1 for efficiency on repos with many branches.
func Clone(g *Git, url, dest string) error {
	return cloneInternal(g, url, dest, cloneOptions{singleBranch: true, depth: 1})
}

// CloneWithReference clones a repository using a local repo as an object reference.
// This saves disk by sharing objects without changing remotes.
// Uses --single-branch --depth 1 for efficiency on repos with many branches.
func CloneWithReference(g *Git, url, dest, reference string) error {
	return cloneInternal(g, url, dest, cloneOptions{reference: reference, singleBranch: true, depth: 1})
}

// CloneBranch clones a specific branch with --single-branch --depth 1.
// Use this when you know which branch you need (avoids fetching all branches).
func CloneBranch(g *Git, url, dest, branch string) error {
	return cloneInternal(g, url, dest, cloneOptions{singleBranch: true, depth: 1, branch: branch})
}

// CloneBranchWithReference clones a specific branch using a local repo as reference.
func CloneBranchWithReference(g *Git, url, dest, branch, reference string) error {
	return cloneInternal(g, url, dest, cloneOptions{singleBranch: true, depth: 1, branch: branch, reference: reference})
}

// CloneBare clones a repository as a bare repo (no working directory).
// This is used for the shared repo architecture where all worktrees share a single git database.
func CloneBare(g *Git, url, dest string) error {
	return cloneInternal(g, url, dest, cloneOptions{bare: true, singleBranch: true, depth: 1})
}

// CloneBareWithBranch clones a bare repo, checking out a specific branch.
// Use this when the desired default branch differs from the remote HEAD.
func CloneBareWithBranch(g *Git, url, dest, branch string) error {
	return cloneInternal(g, url, dest, cloneOptions{bare: true, singleBranch: true, depth: 1, branch: branch})
}

// CloneBarePartial clones a bare repo with a partial clone filter (e.g. "blob:none", "tree:0").
// Does not use --depth since partial clones handle size reduction via the filter.
func CloneBarePartial(g *Git, url, dest, filter string) error {
	return cloneInternal(g, url, dest, cloneOptions{bare: true, singleBranch: true, filter: filter})
}

// CloneBarePartialWithBranch clones a bare repo with a partial clone filter and specific branch.
func CloneBarePartialWithBranch(g *Git, url, dest, filter, branch string) error {
	return cloneInternal(g, url, dest, cloneOptions{bare: true, singleBranch: true, filter: filter, branch: branch})
}

// CloneBarePartialWithReference clones a bare repo with a partial clone filter and local reference.
func CloneBarePartialWithReference(g *Git, url, dest, filter, reference string) error {
	return cloneInternal(g, url, dest, cloneOptions{bare: true, singleBranch: true, filter: filter, reference: reference})
}

// CloneBarePartialWithReferenceAndBranch clones a bare repo with a partial clone filter, local reference, and specific branch.
func CloneBarePartialWithReferenceAndBranch(g *Git, url, dest, filter, reference, branch string) error {
	return cloneInternal(g, url, dest, cloneOptions{bare: true, singleBranch: true, filter: filter, reference: reference, branch: branch})
}

// CloneBranchPartialWithReference clones a specific branch with a partial clone filter and reference.
func CloneBranchPartialWithReference(g *Git, url, dest, branch, filter, reference string) error {
	return cloneInternal(g, url, dest, cloneOptions{singleBranch: true, filter: filter, branch: branch, reference: reference})
}

// CloneBranchPartial clones a specific branch with a partial clone filter.
func CloneBranchPartial(g *Git, url, dest, branch, filter string) error {
	return cloneInternal(g, url, dest, cloneOptions{singleBranch: true, filter: filter, branch: branch})
}

// configureHooksPath sets core.hooksPath to use the repo's .githooks directory
// if it exists. This ensures Gas Town agents use the pre-push hook that blocks
// pushes to non-main branches (internal PRs are not allowed).
func configureHooksPath(repoPath string) error {
	hooksDir := filepath.Join(repoPath, ".githooks")
	if _, err := os.Stat(hooksDir); os.IsNotExist(err) {
		// No .githooks directory, nothing to configure
		return nil
	}

	cmd := exec.Command("git", "-C", repoPath, "config", "core.hooksPath", ".githooks")
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("configuring hooks path: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ConfigureHooksPath sets core.hooksPath for the repo/worktree if .githooks exists.
func ConfigureHooksPath(g *Git) error {
	return configureHooksPath(g.workDir)
}

// configureRefspec sets remote.origin.fetch to the standard refspec for bare repos.
// Bare clones don't have this set by default, which breaks worktrees that need to
// fetch and see origin/* refs. Without this, `git fetch` only updates FETCH_HEAD
// and origin/main never appears in refs/remotes/origin/main.
// See: https://github.com/anthropics/gastown/issues/286
//
// When singleBranch is true, fetches only the default branch's ref instead of all
// branches. This prevents failures on repos with many branches where a full fetch
// would error with "some local refs could not be updated".
func configureRefspec(repoPath string, singleBranch bool) error {
	gitDir := refspecGitDir(repoPath)
	if err := setOriginFetchRefspec(gitDir); err != nil {
		return err
	}
	hasRefs, err := gitDirHasRefs(gitDir)
	if err != nil || !hasRefs {
		return err
	}
	if singleBranch {
		return fetchSingleBranchOrigin(gitDir)
	}
	return fetchOrigin(gitDir)
}

func refspecGitDir(repoPath string) string {
	gitDir := repoPath
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil {
		gitDir = filepath.Join(repoPath, ".git")
	}
	return filepath.Clean(gitDir)
}

func setOriginFetchRefspec(gitDir string) error {
	var stderr bytes.Buffer
	cmd := exec.Command("git", "--git-dir", gitDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	util.SetDetachedProcessGroup(cmd)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("configuring refspec: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

func gitDirHasRefs(gitDir string) (bool, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", "--git-dir", gitDir, "show-ref", "--quiet")
	util.SetDetachedProcessGroup(cmd)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("checking refs: %s", strings.TrimSpace(stderr.String()))
	}
	return true, nil
}

func fetchSingleBranchOrigin(gitDir string) error {
	var headOut, stderr bytes.Buffer
	headCmd := exec.Command("git", "--git-dir", gitDir, "symbolic-ref", "HEAD")
	util.SetDetachedProcessGroup(headCmd)
	headCmd.Stdout = &headOut
	headCmd.Stderr = &stderr
	if err := headCmd.Run(); err != nil {
		return fetchOriginDepth(gitDir)
	}
	branch := strings.TrimPrefix(strings.TrimSpace(headOut.String()), "refs/heads/")
	refspec := branch + ":refs/remotes/origin/" + branch
	return fetchOriginRefspec(gitDir, branch, refspec)
}

func fetchOrigin(gitDir string) error {
	return runGitDirFetch(gitDir, []string{"origin"}, "")
}

func fetchOriginDepth(gitDir string) error {
	return runGitDirFetch(gitDir, []string{"--depth", "1", "origin"}, "")
}

func fetchOriginRefspec(gitDir, branch, refspec string) error {
	return runGitDirFetch(gitDir, []string{"--depth", "1", "origin", refspec}, branch)
}

func runGitDirFetch(gitDir string, fetchArgs []string, branch string) error {
	var stderr bytes.Buffer
	cmd := exec.Command("git", append([]string{"--git-dir", gitDir, "fetch"}, fetchArgs...)...)
	util.SetDetachedProcessGroup(cmd)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if branch != "" {
			return fmt.Errorf("fetching origin %s: %s", branch, msg)
		}
		return fmt.Errorf("fetching origin: %s", msg)
	}
	return nil
}

// CloneBareWithReference clones a bare repository using a local repo as an object reference.
// Uses --single-branch --depth 1 for efficiency on repos with many branches.
func CloneBareWithReference(g *Git, url, dest, reference string) error {
	return cloneInternal(g, url, dest, cloneOptions{bare: true, reference: reference, singleBranch: true, depth: 1})
}

// CloneBareWithReferenceAndBranch clones a bare repo using a local reference, checking out a specific branch.
func CloneBareWithReferenceAndBranch(g *Git, url, dest, reference, branch string) error {
	return cloneInternal(g, url, dest, cloneOptions{bare: true, reference: reference, singleBranch: true, depth: 1, branch: branch})
}

// Checkout checks out the given ref.
func Checkout(g *Git, ref string) error {
	_, err := run(g, "checkout", ref)
	return err
}

// CheckoutDetach checks out the given ref without attaching to a local branch.
// This is useful in shared-worktree repos where the branch may already be
// checked out by another worktree, but this worktree only needs that commit.
func CheckoutDetach(g *Git, ref string) error {
	_, err := run(g, "checkout", "--detach", ref)
	return err
}

// CheckoutNewBranch creates a new branch from startPoint and checks it out.
// Equivalent to: git checkout -b <branch> <startPoint>
func CheckoutNewBranch(g *Git, branch, startPoint string) error {
	_, err := run(g, "checkout", "-b", branch, startPoint)
	return err
}

// CheckoutResetBranch creates or resets a branch to startPoint and checks it out.
// Equivalent to: git checkout -B <branch> <startPoint>. Unlike CheckoutNewBranch
// this does not fail when the branch already exists locally — useful when reusing
// a worktree that previously had the same branch checked out.
func CheckoutResetBranch(g *Git, branch, startPoint string) error {
	_, err := run(g, "checkout", "-B", branch, startPoint)
	return err
}

// Fetch fetches from the remote.
func Fetch(g *Git, remote string) error {
	_, err := run(g, "fetch", remote)
	return err
}

// FetchPrune fetches from the remote and prunes stale remote-tracking refs.
// This removes remote-tracking branches for branches that no longer exist on the remote.
func FetchPrune(g *Git, remote string) error {
	_, err := run(g, "fetch", "--prune", remote)
	return err
}

// FetchBranch fetches a specific branch from the remote.
func FetchBranch(g *Git, remote, branch string) error {
	_, err := run(g, "fetch", remote, branch)
	return err
}

// FetchBranchShallow fetches a single branch with --depth 1 and creates the
// remote tracking ref (e.g. origin/<branch>). Use this on shallow single-branch
// clones to add a branch that wasn't included in the initial clone.
func FetchBranchShallow(g *Git, remote, branch string) error {
	refspec := branch + ":refs/remotes/" + remote + "/" + branch
	_, err := run(g, "fetch", "--depth", "1", remote, refspec)
	return err
}

// Pull pulls from the remote branch.
func Pull(g *Git, remote, branch string) error {
	_, err := run(g, "pull", remote, branch)
	return err
}

// ConfigurePushURL sets the push URL for a remote while keeping the fetch URL.
// This is useful for read-only upstream repos where you want to push to a fork.
// Example: ConfigurePushURL("origin", "https://github.com/user/fork.git")
func ConfigurePushURL(g *Git, remote, pushURL string) error {
	_, err := run(g, "remote", "set-url", remote, "--push", pushURL)
	return err
}

// ClearPushURL removes a custom push URL for a remote, reverting to the fetch URL.
// If no custom push URL is set, this is a no-op.
// Uses --unset-all to handle multi-valued pushurl entries; with --unset-all,
// exit code 5 unambiguously means "key not found" (safe to ignore).
func ClearPushURL(g *Git, remote string) error {
	_, err := run(g, "config", "--unset-all", fmt.Sprintf("remote.%s.pushurl", remote))
	if err != nil {
		// git config --unset-all returns exit code 5 if the key doesn't exist — that's fine.
		var ge *GitError
		if errors.As(err, &ge) {
			var exitErr *exec.ExitError
			if errors.As(ge.Err, &exitErr) && exitErr.ExitCode() == 5 {
				return nil
			}
		}
		return err
	}
	return nil
}

// GetPushURL returns the effective push URL for a remote.
// Note: git returns the fetch URL when no custom push URL is configured, so this
// never returns empty for a valid remote. Compare with RemoteURL to detect custom push URLs.
func GetPushURL(g *Git, remote string) (string, error) {
	out, err := run(g, "remote", "get-url", "--push", remote)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ForkBackedRemote reports whether pushes to remote land somewhere other than
// the canonical fetch base. This covers both split push URLs and fork remotes
// with a distinct upstream remote.
func ForkBackedRemote(g *Git, remote string) bool {
	fetchURL, fetchErr := RemoteURL(g, remote)
	if fetchErr != nil {
		return false
	}
	pushURL, pushErr := GetPushURL(g, remote)
	if pushErr == nil && pushURL != "" && !sameGitRemoteURL(fetchURL, pushURL) {
		return true
	}
	upstreamURL, upstreamErr := GetUpstreamURL(g)
	return upstreamErr == nil && upstreamURL != "" && !sameGitRemoteURL(fetchURL, upstreamURL)
}

// CleanDefaultBranchBaseRef returns the ref that should be used as a clean base
// for default-branch work. In split push-url setups origin still fetches from
// upstream, so origin/<default> is clean. When origin itself is a fork and a
// distinct upstream remote is present, upstream/<default> is the clean base.
func CleanDefaultBranchBaseRef(g *Git, remote, defaultBranch string) string {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	fetchURL, fetchErr := RemoteURL(g, remote)
	upstreamURL, upstreamErr := GetUpstreamURL(g)
	if fetchErr == nil && upstreamErr == nil && upstreamURL != "" && !sameGitRemoteURL(fetchURL, upstreamURL) {
		return "upstream/" + defaultBranch
	}
	return remote + "/" + defaultBranch
}

// CleanBaseRef returns a fully qualified base ref for a target branch. Explicit
// origin/ or upstream/ refs are preserved; default-branch targets use the clean
// fork-aware base.
func CleanBaseRef(g *Git, remote, defaultBranch, target string) string {
	target = strings.TrimSpace(target)
	if target == "" || target == defaultBranch {
		return CleanDefaultBranchBaseRef(g, remote, defaultBranch)
	}
	if strings.HasPrefix(target, "origin/") || strings.HasPrefix(target, "upstream/") {
		return target
	}
	return remote + "/" + target
}

// RemoteForRef returns the remote prefix from refs like origin/main or
// upstream/main. It returns an empty string for local branch names.
func RemoteForRef(ref string) string {
	remote, _, ok := strings.Cut(strings.TrimSpace(ref), "/")
	if !ok || (remote != "origin" && remote != "upstream") {
		return ""
	}
	return remote
}

// RefuseForkBackedDefaultPush fails closed before default-branch pushes in a
// fork/upstream topology. Feature branch pushes to the fork remain allowed.
func RefuseForkBackedDefaultPush(g *Git, remote, refspec, defaultBranch string) error {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	destination := pushDestinationBranch(refspec)
	if destination != defaultBranch || !ForkBackedRemote(g, remote) {
		return nil
	}
	return fmt.Errorf("refusing direct push to %s/%s: fork/upstream rig detected; push a feature branch and use the Mayor-managed fork PR flow to upstream %s (no refs were pushed)", remote, destination, defaultBranch)
}

func pushDestinationBranch(refspec string) string {
	refspec = strings.TrimSpace(refspec)
	for strings.HasPrefix(refspec, "+") {
		refspec = strings.TrimPrefix(refspec, "+")
	}
	if _, dst, ok := strings.Cut(refspec, ":"); ok {
		refspec = dst
	}
	refspec = strings.TrimPrefix(refspec, "refs/heads/")
	return strings.TrimSpace(refspec)
}

func sameGitRemoteURL(a, b string) bool {
	return normalizeGitRemoteURL(a) == normalizeGitRemoteURL(b)
}

// IsNonWritableRemoteError reports whether a git push failed because the
// remote refused write access. This covers HTTP 403, archived repos, and
// "permission denied" auth failures. Transient network errors stay false.
func IsNonWritableRemoteError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "403"),
		strings.Contains(msg, "permission to ") && strings.Contains(msg, " denied"),
		strings.Contains(msg, "permission denied"),
		strings.Contains(msg, "repository was archived"),
		strings.Contains(msg, "read-only"),
		strings.Contains(msg, "read only"),
		strings.Contains(msg, "writing to ") && strings.Contains(msg, "not allowed"),
		strings.Contains(msg, "remote rejected") && strings.Contains(msg, "hook declined"):
		return true
	default:
		return false
	}
}

func normalizeGitRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "ssh://")
	s = strings.TrimPrefix(s, "git://")
	if strings.HasPrefix(s, "git@") {
		s = strings.TrimPrefix(s, "git@")
		s = strings.Replace(s, ":", "/", 1)
	} else if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	return strings.ToLower(strings.TrimSuffix(s, "/"))
}

// Push pushes to the remote branch with a timeout to prevent indefinite hangs
// when the remote is unreachable.
func Push(g *Git, remote, branch string, force bool) error {
	if err := RefuseForkBackedDefaultPush(g, remote, branch, RemoteDefaultBranch(g)); err != nil {
		return err
	}
	args := []string{"push", remote, branch}
	if force {
		args = append(args, "--force")
	}
	_, err := runWithTimeout(g, pushTimeout, args...)
	return err
}

// PushWithEnv pushes with additional environment variables.
// Used by gt mq integration land to set GT_INTEGRATION_LAND=1, which the
// pre-push hook checks to allow integration branch content landing on main.
func PushWithEnv(g *Git, remote, branch string, force bool, env []string) error {
	if err := RefuseForkBackedDefaultPush(g, remote, branch, RemoteDefaultBranch(g)); err != nil {
		return err
	}
	args := []string{"push", remote, branch}
	if force {
		args = append(args, "--force")
	}
	_, err := runWithEnvAndTimeout(g, args, env, pushTimeout)
	return err
}

// Add stages files for commit. Paths are passed after `--` so a filename
// cannot be interpreted as a git-add flag.
func Add(g *Git, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"add", "--"}, paths...)
	_, err := run(g, args...)
	return err
}

// StageSafetyNet stages recoverable implementation work for a safety-net commit.
// It does not run git add -A. It skips runtime artifacts, tracked-file
// deletions, and binary files such as a locally built gt executable.
func StageSafetyNet(g *Git) error {
	status, err := Status(g)
	if err != nil {
		return err
	}
	var paths []string
	for _, p := range append(append([]string{}, status.Modified...), status.Added...) {
		if isSafetyNetSkipped(g.workDir, p) {
			continue
		}
		paths = append(paths, p)
	}
	for _, p := range status.Untracked {
		if isSafetyNetSkipped(g.workDir, p) {
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return nil
	}
	return Add(g, paths...)
}

// HasStagedChanges reports whether the index has a staged diff.
func HasStagedChanges(g *Git) (bool, error) {
	out, err := run(g, "diff", "--cached", "--name-only")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// Commit creates a commit with the given message.
func Commit(g *Git, message string) error {
	_, err := run(g, "commit", "-m", message)
	return err
}

// CommitAll stages all changes and commits.
func CommitAll(g *Git, message string) error {
	_, err := run(g, "commit", "-am", message)
	return err
}

// ResetFiles unstages files without modifying the working tree.
// Equivalent to: git reset HEAD -- <paths>
func ResetFiles(g *Git, paths ...string) error {
	args := append([]string{"reset", "HEAD", "--"}, paths...)
	_, err := run(g, args...)
	return err
}

// StagedDeletions returns the list of tracked files staged for deletion.
// Used by auto-save to unstage deletions — safety nets should preserve work, not destroy it.
func StagedDeletions(g *Git) ([]string, error) {
	out, err := run(g, "diff", "--cached", "--name-only", "--diff-filter=D")
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// ShowFile returns the contents of a file at a given ref (e.g., "origin/main:CLAUDE.md").
// Returns empty string and no error if the file does not exist at that ref.
func ShowFile(g *Git, ref, path string) (string, error) {
	out, err := run(g, "show", ref+":"+path)
	if err != nil {
		// "does not exist" or "exists on disk, but not in" are expected for missing files
		return "", err
	}
	return out, nil
}

// CheckoutFileFromRef restores a file from a given ref (e.g., "origin/main").
// Equivalent to: git checkout <ref> -- <path>
func CheckoutFileFromRef(g *Git, ref string, paths ...string) error {
	args := append([]string{"checkout", ref, "--"}, paths...)
	_, err := run(g, args...)
	return err
}

// RmCached removes files from the index without deleting from the working tree.
// Equivalent to: git rm --cached --force <paths>
func RmCached(g *Git, paths ...string) error {
	args := append([]string{"rm", "--cached", "--force", "--ignore-unmatch"}, paths...)
	_, err := run(g, args...)
	return err
}

// DiffNameOnly returns filenames changed between two refs.
// Equivalent to: git diff --name-only <base>...<head>
func DiffNameOnly(g *Git, base, head string) ([]string, error) {
	out, err := run(g, "diff", "--name-only", base+"..."+head)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSpace(out), "\n"), nil
}

// GitStatus represents the status of the working directory.
type GitStatus struct {
	Clean     bool
	Modified  []string
	Added     []string
	Deleted   []string
	Untracked []string
	Unmerged  []string
}

type porcelainStatusEntry struct {
	Code       string
	Path       string
	SourcePath string
	Unmerged   bool
}

// Status returns the current git status.
func Status(g *Git) (*GitStatus, error) {
	out, err := runOutput(g, "status", "--porcelain", "-uall")
	if err != nil {
		return nil, err
	}
	out = strings.TrimRight(out, "\n")
	status := &GitStatus{Clean: true}
	if out == "" {
		return status, nil
	}
	status.Clean = false
	applyPorcelainStatus(status, out, skipWorktreeFiles(g))
	if porcelainStatusEmpty(status) {
		status.Clean = true
	}
	return status, nil
}

func applyPorcelainStatus(status *GitStatus, out string, skipWorktree map[string]bool) {
	for _, line := range strings.Split(out, "\n") {
		applyPorcelainStatusLine(status, line, skipWorktree)
	}
}

func applyPorcelainStatusLine(status *GitStatus, line string, skipWorktree map[string]bool) {
	entry, ok := parsePorcelainStatusEntry(line)
	if !ok {
		return
	}
	applyPorcelainStatusEntry(status, entry, skipWorktree)
}

func applyPorcelainStatusEntry(status *GitStatus, entry porcelainStatusEntry, skipWorktree map[string]bool) {
	code := entry.Code
	file := entry.Path
	switch {
	case entry.Unmerged:
		status.Unmerged = append(status.Unmerged, entry.paths()...)
	case strings.Contains(code, "?"):
		status.Untracked = append(status.Untracked, file)
	case strings.ContainsAny(code, "RC"):
		status.Modified = append(status.Modified, entry.paths()...)
	case strings.Contains(code, "M"):
		status.Modified = append(status.Modified, file)
	case strings.Contains(code, "A"):
		status.Added = append(status.Added, file)
	case strings.Contains(code, "D"):
		if !skipWorktree[file] {
			status.Deleted = append(status.Deleted, file)
		}
	default:
		status.Modified = append(status.Modified, file)
	}
}

func porcelainStatusEmpty(status *GitStatus) bool {
	return len(status.Modified) == 0 && len(status.Added) == 0 &&
		len(status.Deleted) == 0 && len(status.Untracked) == 0 && len(status.Unmerged) == 0
}

func parsePorcelainStatusEntry(line string) (porcelainStatusEntry, bool) {
	if len(line) < 3 {
		return porcelainStatusEntry{}, false
	}

	entry := porcelainStatusEntry{
		Code:     line[:2],
		Path:     line[3:],
		Unmerged: isUnmergedPorcelainStatus(line[:2]),
	}
	if strings.ContainsAny(entry.Code, "RC") {
		entry.SourcePath, entry.Path = porcelainRenameCopyPaths(entry.Path)
	}
	return entry, true
}

func (e porcelainStatusEntry) paths() []string {
	if e.SourcePath == "" || e.SourcePath == e.Path {
		return []string{e.Path}
	}
	return []string{e.SourcePath, e.Path}
}

func porcelainRenameCopyPaths(path string) (string, string) {
	if idx := strings.LastIndex(path, " -> "); idx >= 0 {
		return path[:idx], path[idx+4:]
	}
	return "", path
}

func isUnmergedPorcelainStatus(code string) bool {
	switch code {
	case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
		return true
	default:
		return strings.Contains(code, "U")
	}
}

// skipWorktreeFiles returns a set of file paths that have the skip-worktree
// bit set (sparse-checkout hidden files). Uses `git ls-files -v` and filters
// for lines starting with 'S' (uppercase = skip-worktree). Non-fatal: returns
// empty map on error so callers degrade gracefully.
func skipWorktreeFiles(g *Git) map[string]bool {
	out, err := run(g, "ls-files", "-v")
	if err != nil || out == "" {
		return nil
	}
	result := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		// Format: "<flag> <path>" where flag is uppercase letter for skip-worktree
		if len(line) < 3 || line[0] != 'S' {
			continue
		}
		result[line[2:]] = true
	}
	return result
}

// CurrentBranch returns the current branch name.
func CurrentBranch(g *Git) (string, error) {
	return run(g, "rev-parse", "--abbrev-ref", "HEAD")
}

// DefaultBranch returns the default branch name (what HEAD points to).
// This works for both regular and bare repositories.
// Returns "main" as fallback if detection fails.
func DefaultBranch(g *Git) string {
	// Try symbolic-ref first (works for bare repos)
	branch, err := run(g, "symbolic-ref", "--short", "HEAD")
	if err == nil && branch != "" {
		return branch
	}
	// Fallback to main
	return "main"
}

// RemoteDefaultBranch returns the default branch from the remote (origin).
// This is useful in worktrees where HEAD may not reflect the repo's actual default.
// Checks origin/HEAD first, then falls back to checking if master/main exists.
// Returns "main" as final fallback.
func RemoteDefaultBranch(g *Git) string {
	// Try to get from origin/HEAD symbolic ref
	out, err := run(g, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil && out != "" {
		// Returns refs/remotes/origin/main -> extract branch name
		parts := strings.Split(out, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}

	// Fallback: check if origin/master exists
	_, err = run(g, "rev-parse", "--verify", "origin/master")
	if err == nil {
		return "master"
	}

	// Fallback: check if origin/main exists
	_, err = run(g, "rev-parse", "--verify", "origin/main")
	if err == nil {
		return "main"
	}

	return "main" // final fallback
}

// HasUncommittedChanges returns true if there are uncommitted changes.
func HasUncommittedChanges(g *Git) (bool, error) {
	status, err := Status(g)
	if err != nil {
		return false, err
	}
	return !status.Clean, nil
}

// RemoteURL returns the URL for the given remote.
// This applies url.*.insteadOf rewrites from Git config.
func RemoteURL(g *Git, remote string) (string, error) {
	return run(g, "remote", "get-url", remote)
}

// ConfiguredRemoteURL returns the remote URL stored in the repository config.
// Unlike RemoteURL, this does not apply url.*.insteadOf rewrites.
func ConfiguredRemoteURL(g *Git, remote string) (string, error) {
	out, err := run(g, "config", "--local", "--get", "remote."+remote+".url")
	if err != nil {
		if isGitConfigKeyMissing(err) {
			return "", fmt.Errorf("%w: %s", ErrRemoteNotConfigured, remote)
		}
		return "", err
	}
	return out, nil
}

func isGitConfigKeyMissing(err error) bool {
	var ge *GitError
	if !errors.As(err, &ge) {
		return false
	}
	var exit *exec.ExitError
	if !errors.As(ge.Err, &exit) {
		return false
	}
	return exit.ExitCode() == 1
}

// AddRemote adds a new remote with the given name and URL.
func AddRemote(g *Git, name, url string) (string, error) {
	return run(g, "remote", "add", name, url)
}

// SetRemoteURL updates the URL for an existing remote.
func SetRemoteURL(g *Git, name, url string) (string, error) {
	return run(g, "remote", "set-url", name, url)
}

// AddUpstreamRemote adds or updates the 'upstream' git remote.
// This is idempotent - if the remote already exists with the same URL, it's a no-op.
// If the remote exists with a different URL, it's updated.
func AddUpstreamRemote(g *Git, upstreamURL string) error {
	has, err := HasUpstreamRemote(g)
	if err != nil {
		return err
	}
	if has {
		current, err := GetUpstreamURL(g)
		if err != nil {
			return err
		}
		if current == upstreamURL {
			return nil
		}
		_, err = run(g, "remote", "set-url", "upstream", upstreamURL)
		return err
	}
	_, err = run(g, "remote", "add", "upstream", upstreamURL)
	return err
}

// GetUpstreamURL returns the URL of the upstream remote.
// Returns empty string if upstream remote doesn't exist.
func GetUpstreamURL(g *Git) (string, error) {
	out, err := run(g, "remote", "get-url", "upstream")
	if err != nil {
		if strings.Contains(err.Error(), "No such remote") {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// HasUpstreamRemote returns true if an upstream remote is configured.
func HasUpstreamRemote(g *Git) (bool, error) {
	_, err := run(g, "remote", "get-url", "upstream")
	if err != nil {
		if strings.Contains(err.Error(), "No such remote") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// FetchUpstream fetches from the upstream remote.
func FetchUpstream(g *Git) error {
	_, err := run(g, "fetch", "upstream")
	return err
}

// Remotes returns the list of configured remote names.
func Remotes(g *Git) ([]string, error) {
	out, err := run(g, "remote")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// ConfigGet returns the value of a git config key.
// Returns empty string if the key is not set.
func ConfigGet(g *Git, key string) (string, error) {
	out, err := run(g, "config", "--get", key)
	if err != nil {
		// git config --get returns exit code 1 if key not found
		return "", nil
	}
	return out, nil
}

// Merge merges the given branch into the current branch.
func Merge(g *Git, branch string) error {
	_, err := run(g, "merge", branch)
	return err
}

// MergeNoFF merges the given branch with --no-ff flag and a custom message.
func MergeNoFF(g *Git, branch, message string) error {
	_, err := run(g, "merge", "--no-ff", "-m", message, branch)
	return err
}

// MergeFFOnly performs a fast-forward-only merge of the given ref into the current branch.
// This ensures what you tested is exactly what lands — no merge commits are created.
// Returns an error if the merge cannot be performed as a fast-forward.
func MergeFFOnly(g *Git, ref string) error {
	_, err := run(g, "merge", "--ff-only", ref)
	return err
}

// MergeSquash performs a squash merge of the given branch and commits with the provided message.
// This stages all changes from the branch without creating a merge commit, then commits them
// as a single commit with the given message. This eliminates redundant merge commits while
// preserving the original commit message from the source branch.
func MergeSquash(g *Git, branch, message string) error {
	// Stage all changes from the branch without committing
	if _, err := run(g, "merge", "--squash", branch); err != nil {
		return err
	}
	// Commit the staged changes with the provided message
	_, err := run(g, "commit", "-m", message)
	return err
}

// GetBranchCommitMessage returns the commit message of the HEAD commit on the given branch.
// This is useful for preserving the original conventional commit message (feat:/fix:) when
// performing squash merges.
func GetBranchCommitMessage(g *Git, branch string) (string, error) {
	return run(g, "log", "-1", "--format=%B", branch)
}

// RecentCommits returns the last n commits as one-line summaries (hash + subject).
// Returns empty string if there are no commits or the repo is empty.
func RecentCommits(g *Git, n int) (string, error) {
	return run(g, "log", "--oneline", fmt.Sprintf("-%d", n))
}

// DeleteRemoteBranch deletes a branch on the remote.
func DeleteRemoteBranch(g *Git, remote, branch string) error {
	_, err := runWithTimeout(g, pushTimeout, "push", remote, "--delete", branch)
	return err
}

// DeleteRemoteBranchIfAt deletes a remote branch only if it still points at expectedHash.
func DeleteRemoteBranchIfAt(g *Git, remote, branch, expectedHash string) error {
	ref := "refs/heads/" + branch
	_, err := runWithTimeout(g, pushTimeout, "push", "--force-with-lease="+ref+":"+expectedHash, remote, ":"+ref)
	return err
}

// HasOpenPR checks whether the given branch has an open pull request on GitHub.
// Errors and ambiguous branch lookups protect the branch from deletion.
func HasOpenPR(g *Git, branch string) bool {
	return HasOpenPullRequest(g, PullRequestRef{Branch: branch})
}

// IsPRApproved checks whether a GitHub PR has at least one approving review.
// Returns true if approved, false if not (or on error).
func IsPRApproved(g *Git, prNumber int) (bool, error) {
	return IsPullRequestApproved(g, &PullRequestInfo{Number: prNumber})
}

// IsPullRequestApproved checks whether a resolved GitHub PR has at least one approving review.
func IsPullRequestApproved(g *Git, pr *PullRequestInfo) (bool, error) {
	if pr == nil || (pr.Number == 0 && pr.URL == "") {
		return false, fmt.Errorf("pull request identity is missing")
	}
	// Use gh pr view which includes review decision
	args := []string{"pr", "view", pullRequestSelector(pr), "--json", "reviewDecision"}
	if pr.BaseRepo != "" {
		args = append(args, "--repo", pr.BaseRepo)
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = g.workDir
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("gh pr view failed: %w", err)
	}
	var result struct {
		ReviewDecision string `json:"reviewDecision"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		return false, fmt.Errorf("failed to parse gh pr view output: %w", err)
	}
	// APPROVED is the GitHub review decision when at least one approving review exists
	return result.ReviewDecision == "APPROVED", nil
}

// GhPrMerge merges a GitHub PR using the gh CLI, respecting branch protection rules.
// The method parameter should be "merge", "squash", or "rebase".
// Returns the merge commit SHA on success.
func GhPrMerge(g *Git, prNumber int, method string) (string, error) {
	return GhPrMergePullRequest(g, &PullRequestInfo{Number: prNumber}, method)
}

// GhPrMergePullRequest merges a resolved GitHub PR using its URL when available.
func GhPrMergePullRequest(g *Git, pr *PullRequestInfo, method string) (string, error) {
	if pr == nil || (pr.Number == 0 && pr.URL == "") {
		return "", fmt.Errorf("pull request identity is missing")
	}
	args := []string{"pr", "merge", pullRequestSelector(pr), "--" + method}
	if head := strings.TrimSpace(pr.HeadSHA); head != "" {
		args = append(args, "--match-head-commit", head)
	}
	if pr.BaseRepo != "" {
		args = append(args, "--repo", pr.BaseRepo)
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = g.workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr merge failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// After merge, pull the target branch to get the merge commit locally
	if _, pullErr := run(g, "pull", "origin"); pullErr != nil {
		// Non-fatal: the merge succeeded on GitHub, we just can't get the SHA locally
		return "", nil
	}
	// Get the latest commit on HEAD (should be the merge commit)
	sha, revErr := Rev(g, "HEAD")
	if revErr != nil {
		return "", nil // Merge succeeded, just can't determine SHA
	}
	return sha, nil
}

func pullRequestSelector(pr *PullRequestInfo) string {
	if pr != nil && pr.URL != "" {
		return pr.URL
	}
	if pr != nil {
		return fmt.Sprintf("%d", pr.Number)
	}
	return ""
}

// FindBitbucketPullRequest returns the open Bitbucket PR for branch.
// It includes the source commit hash so refinery can merge only the submitted head.
func FindBitbucketPullRequest(g *Git, workspace, repoSlug, branch, headSHA string) (*PullRequestInfo, error) {
	// Use curl since there is no official Bitbucket CLI equivalent to gh.
	// The BITBUCKET_TOKEN env var provides authentication.
	token := os.Getenv("BITBUCKET_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("BITBUCKET_TOKEN is required for Bitbucket PR operations")
	}
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s/pullrequests?q=source.branch.name%%3D%%22%s%%22+AND+state%%3D%%22OPEN%%22&pagelen=1",
		workspace, repoSlug, branch)
	cmd := exec.Command("curl", "-s", "-H", "Authorization: Bearer "+token, url)
	cmd.Dir = g.workDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bitbucket API request failed: %w", err)
	}
	var resp struct {
		Values []struct {
			ID    int    `json:"id"`
			State string `json:"state"`
			Links struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
			} `json:"links"`
			Source struct {
				Branch struct {
					Name string `json:"name"`
				} `json:"branch"`
				Commit struct {
					Hash string `json:"hash"`
				} `json:"commit"`
			} `json:"source"`
		} `json:"values"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse Bitbucket response: %w", err)
	}
	if len(resp.Values) == 0 {
		return nil, nil
	}
	pr := resp.Values[0]
	info := &PullRequestInfo{
		Number:       pr.ID,
		URL:          pr.Links.HTML.Href,
		State:        strings.ToUpper(pr.State),
		HeadRefName:  pr.Source.Branch.Name,
		HeadSHA:      strings.TrimSpace(pr.Source.Commit.Hash),
		LookupSource: "bitbucket-head",
	}
	if info.State == "" {
		info.State = "OPEN"
	}
	if err := validatePullRequestHead(info, headSHA); err != nil {
		return nil, err
	}
	return info, nil
}

// IsBitbucketPRApproved checks whether a Bitbucket PR has at least one approving reviewer.
func IsBitbucketPRApproved(g *Git, workspace, repoSlug string, prID int) (bool, error) {
	token := os.Getenv("BITBUCKET_TOKEN")
	if token == "" {
		return false, fmt.Errorf("BITBUCKET_TOKEN is required for Bitbucket PR operations")
	}
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s/pullrequests/%d",
		workspace, repoSlug, prID)
	cmd := exec.Command("curl", "-s", "-H", "Authorization: Bearer "+token, url)
	cmd.Dir = g.workDir
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("bitbucket API request failed: %w", err)
	}
	var pr struct {
		Participants []struct {
			Role     string `json:"role"`
			Approved bool   `json:"approved"`
		} `json:"participants"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &pr); err != nil {
		return false, fmt.Errorf("failed to parse Bitbucket response: %w", err)
	}
	for _, p := range pr.Participants {
		if p.Role == "REVIEWER" && p.Approved {
			return true, nil
		}
	}
	return false, nil
}

// BitbucketPRMerge merges a Bitbucket PR via the REST API.
// The strategy parameter should be "merge_commit", "squash", or "fast_forward".
// Returns the merge commit SHA on success (if available).
func BitbucketPRMerge(g *Git, workspace, repoSlug string, prID int, strategy string) (string, error) {
	token := os.Getenv("BITBUCKET_TOKEN")
	if token == "" {
		return "", fmt.Errorf("BITBUCKET_TOKEN is required for Bitbucket PR operations")
	}
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s/pullrequests/%d/merge",
		workspace, repoSlug, prID)
	body := fmt.Sprintf(`{"merge_strategy":"%s","close_source_branch":false}`, strategy)
	cmd := exec.Command("curl", "-s", "-X", "POST",
		"-H", "Authorization: Bearer "+token,
		"-H", "Content-Type: application/json",
		"-d", body, url)
	cmd.Dir = g.workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("bitbucket merge failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var resp struct {
		MergeCommit struct {
			Hash string `json:"hash"`
		} `json:"merge_commit"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		// Merge may have succeeded but response parsing failed — pull to get SHA.
		if _, pullErr := run(g, "pull", "origin"); pullErr == nil {
			if sha, revErr := Rev(g, "HEAD"); revErr == nil {
				return sha, nil
			}
		}
		return "", nil
	}

	// Sync local state after remote merge.
	if _, pullErr := run(g, "pull", "origin"); pullErr != nil {
		return resp.MergeCommit.Hash, nil
	}
	if resp.MergeCommit.Hash != "" {
		return resp.MergeCommit.Hash, nil
	}
	sha, _ := Rev(g, "HEAD")
	return sha, nil
}

// RemoteRef is a ref observed through ls-remote.
type RemoteRef struct {
	Hash string
	Name string
}

// ListRemoteRefsWithHashes returns remote refs matching a prefix using ls-remote.
// The prefix filters refs (e.g., "refs/heads/polecat/" for all polecat branches).
// Returns full ref names like "refs/heads/polecat/furiosa-abc123".
func ListRemoteRefsWithHashes(g *Git, remote, prefix string) ([]RemoteRef, error) {
	out, err := run(g, "ls-remote", "--refs", remote, prefix+"*")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var refs []RemoteRef
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// ls-remote output format: <sha>\t<refname>
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			refs = append(refs, RemoteRef{Hash: parts[0], Name: parts[1]})
		}
	}
	return refs, nil
}

// ListRemoteRefs returns remote ref names matching a prefix using ls-remote.
func ListRemoteRefs(g *Git, remote, prefix string) ([]string, error) {
	refsWithHashes, err := ListRemoteRefsWithHashes(g, remote, prefix)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(refsWithHashes))
	for _, ref := range refsWithHashes {
		refs = append(refs, ref.Name)
	}
	return refs, nil
}

// RemoteHasRefs reports whether a remote has any refs at all. It deliberately
// includes tags so callers can distinguish a truly empty repo from a non-empty
// repo with no branch refs or a broken remote HEAD.
func RemoteHasRefs(g *Git, remote string) (bool, error) {
	out, err := run(g, "ls-remote", "--refs", remote)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// ListPushRemoteRefs lists remote refs from the push URL when it differs from
// the fetch URL. With a fork-based workflow (pushurl configured), branches are
// pushed to the fork but ls-remote reads from the fetch URL (upstream). This
// method queries the push URL so cleanup can find branches that were pushed.
// Falls back to ListRemoteRefs if no custom push URL is configured.
func ListPushRemoteRefs(g *Git, remote, prefix string) ([]string, error) {
	refsWithHashes, err := ListPushRemoteRefsWithHashes(g, remote, prefix)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(refsWithHashes))
	for _, ref := range refsWithHashes {
		refs = append(refs, ref.Name)
	}
	return refs, nil
}

// ListPushRemoteRefsWithHashes is ListPushRemoteRefs with commit hashes.
func ListPushRemoteRefsWithHashes(g *Git, remote, prefix string) ([]RemoteRef, error) {
	return ListRemoteRefsWithHashes(g, pushTarget(g, remote), prefix)
}

// Rebase rebases the current branch onto the given ref.
func Rebase(g *Git, onto string) error {
	_, err := run(g, "rebase", onto)
	return err
}

// AbortMerge aborts a merge in progress.
func AbortMerge(g *Git) error {
	_, err := run(g, "merge", "--abort")
	return err
}

// CheckConflicts performs a test merge to check if source can be merged into target
// without conflicts. Returns a list of conflicting files, or empty slice if clean.
// The merge is always aborted after checking - no actual changes are made.
//
// The caller must ensure the working directory is clean before calling this.
// After return, the working directory is restored to the target branch.
func CheckConflicts(g *Git, source, target string) ([]string, error) {
	// Checkout the target branch
	if err := Checkout(g, target); err != nil {
		return nil, fmt.Errorf("checkout target %s: %w", target, err)
	}

	// Attempt test merge with --no-commit --no-ff
	// We need to capture both stdout and stderr to detect conflicts
	_, mergeErr := runMergeCheck(g, "merge", "--no-commit", "--no-ff", source)

	if mergeErr != nil {
		// ZFC: Use git's porcelain output to detect conflicts instead of parsing stderr.
		// GetConflictingFiles() uses `git diff --diff-filter=U` which is the proper way.
		conflicts, err := GetConflictingFiles(g)
		if err == nil && len(conflicts) > 0 {
			// Abort the test merge (best-effort cleanup)
			_ = AbortMerge(g)
			return conflicts, nil
		}

		// No unmerged files detected - this is some other merge error
		_ = AbortMerge(g)
		return nil, mergeErr
	}

	// Merge succeeded (no conflicts) - abort the test merge
	// Use reset since --abort won't work on successful merge (best-effort cleanup)
	_, _ = run(g, "reset", "--hard", "HEAD")
	return nil, nil
}

// runMergeCheck runs a git merge command and returns error info from both stdout and stderr.
// ZFC: Returns GitError with raw output for agent observation.
func runMergeCheck(g *Git, args ...string) (string, error) {
	if err := guardUnsafeTownRootMutation(g, args); err != nil {
		return "", err
	}

	cmd := exec.Command("git", args...)
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = g.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// ZFC: Return raw output for observation, don't interpret CONFLICT
		return "", wrapError(err, stdout.String(), stderr.String(), args)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// GetConflictingFiles returns the list of files with merge conflicts.
// ZFC: Uses git's porcelain output (diff --diff-filter=U) instead of parsing stderr.
// This is the proper way to detect conflicts without violating ZFC.
func GetConflictingFiles(g *Git) ([]string, error) {
	// git diff --name-only --diff-filter=U shows unmerged files
	out, err := run(g, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}

	if out == "" {
		return nil, nil
	}

	files := strings.Split(out, "\n")
	// Filter out empty strings
	var result []string
	for _, f := range files {
		if f != "" {
			result = append(result, f)
		}
	}
	return result, nil
}

// AbortRebase aborts a rebase in progress.
func AbortRebase(g *Git) error {
	_, err := run(g, "rebase", "--abort")
	return err
}

// CreateBranch creates a new branch.
func CreateBranch(g *Git, name string) error {
	_, err := run(g, "branch", name)
	return err
}

// CreateBranchFrom creates a new branch from a specific ref.
func CreateBranchFrom(g *Git, name, ref string) error {
	_, err := run(g, "branch", name, ref)
	return err
}

// BranchExists checks if a branch exists locally.
func BranchExists(g *Git, name string) (bool, error) {
	_, err := run(g, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	if err != nil {
		// Exit code 1 means branch doesn't exist
		if strings.Contains(err.Error(), "exit status 1") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// RefExists checks if a ref exists (works for any ref including origin/<branch>).
// Uses show-ref for fully-qualified refs, falls back to rev-parse for short refs.
func RefExists(g *Git, ref string) (bool, error) {
	// Fully-qualified refs (refs/...) use show-ref which has a stable exit code contract:
	// exit 0 = exists, exit 1 = missing, exit >1 = error.
	if strings.HasPrefix(ref, "refs/") {
		_, err := run(g, "show-ref", "--verify", "--quiet", ref)
		if err != nil {
			if strings.Contains(err.Error(), "exit status 1") {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}

	// Short refs (e.g., origin/main) need rev-parse --verify.
	_, err := run(g, "rev-parse", "--verify", ref)
	if err != nil {
		// Only treat "ref missing" as false — propagate other failures
		// (e.g. corrupted repo, permissions, disk I/O).
		var gitErr *GitError
		if errors.As(err, &gitErr) &&
			strings.Contains(gitErr.Stderr, "Needed a single revision") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// IsEmpty returns true if the repository has no refs (an empty/unborn repo).
// This is the case for newly-created repos with no commits.
func IsEmpty(g *Git) (bool, error) {
	out, err := run(g, "show-ref")
	if err != nil {
		// git show-ref exits 1 when there are no refs — that means empty
		if strings.Contains(err.Error(), "exit status 1") {
			return true, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// RemoteBranchExists checks if a branch exists on the remote.
// NOTE: For named remotes with a separate pushurl, this checks the fetch URL.
// Use PushRemoteBranchExists to verify branches that were pushed.
func RemoteBranchExists(g *Git, remote, branch string) (bool, error) {
	out, err := run(g, "ls-remote", "--heads", remote, branch)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// RemoteBranchTip returns the SHA at refs/heads/<branch> on the remote.
// An empty SHA with nil error means the branch is missing.
func RemoteBranchTip(g *Git, remote, branch string) (string, error) {
	out, err := run(g, "ls-remote", "--heads", remote, branch)
	if err != nil {
		return "", err
	}
	return parseLSRemoteTip(out, branch), nil
}

// PushRemoteBranchExists checks if a branch exists on the push target of a remote.
// With a fork-based or local-bare-repo workflow (pushurl configured), pushes go to
// the push URL but ls-remote resolves the fetch URL. This method queries the push
// URL directly so verification matches where the branch was actually pushed.
// Falls back to RemoteBranchExists when no custom push URL is configured.
func PushRemoteBranchExists(g *Git, remote, branch string) (bool, error) {
	pushTarget := pushTarget(g, remote)
	if pushTarget == remote {
		return RemoteBranchExists(g, remote, branch)
	}
	out, err := run(g, "ls-remote", "--heads", pushTarget, branch)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// PushRemoteBranchTip returns the SHA at refs/heads/<branch> on the push target.
// This mirrors PushRemoteBranchExists: when remote.<name>.pushurl differs from
// the fetch URL, verification must query the push URL because that is where the
// preceding git push wrote.
func PushRemoteBranchTip(g *Git, remote, branch string) (string, error) {
	pushTarget := pushTarget(g, remote)
	if pushTarget == remote {
		return RemoteBranchTip(g, remote, branch)
	}
	return RemoteBranchTip(g, pushTarget, branch)
}

func pushTarget(g *Git, remote string) string {
	fetchURL, fetchErr := RemoteURL(g, remote)
	pushURL, pushErr := GetPushURL(g, remote)
	if fetchErr != nil || pushErr != nil || pushURL == fetchURL {
		return remote
	}
	return pushURL
}

// VerifyPushedCommit verifies that the push target branch tip is exactly commit.
// gt/refinery callers invoke this immediately after a push, before closing beads
// or creating downstream merge artifacts. Exact-tip verification catches the
// dangerous case where git push exits 0 but leaves the remote branch stale.
func VerifyPushedCommit(g *Git, remote, branch, commit string) error {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("verified_push_failed: empty commit for %s/%s", remote, branch)
	}
	tip, err := PushRemoteBranchTip(g, remote, branch)
	if err != nil {
		return fmt.Errorf("verified_push_failed: unable to read %s/%s: %w", remote, branch, err)
	}
	if tip == "" {
		return fmt.Errorf("verified_push_failed: branch %s/%s missing after push (expected %s)", remote, branch, shortSHA(commit))
	}
	if tip != commit {
		return fmt.Errorf("verified_push_failed: commit %s not on %s/%s (remote tip %s)", shortSHA(commit), remote, branch, shortSHA(tip))
	}
	return nil
}

// VerifyPushedCommitReachableFromPushTarget verifies that commit is reachable
// from the push target branch. Use this only for shared target branches where a
// later fast-forward push by another actor may legitimately advance the tip.
func VerifyPushedCommitReachableFromPushTarget(g *Git, remote, branch, commit string) error {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("verified_push_failed: empty commit for %s/%s", remote, branch)
	}
	tip, err := PushRemoteBranchTip(g, remote, branch)
	if err != nil {
		return fmt.Errorf("verified_push_failed: unable to read %s/%s: %w", remote, branch, err)
	}
	if tip == "" {
		return fmt.Errorf("verified_push_failed: branch %s/%s missing after push (expected %s)", remote, branch, shortSHA(commit))
	}
	if tip == commit {
		return nil
	}

	fetchTarget := pushTarget(g, remote)
	if _, err := run(g, "fetch", "--no-tags", fetchTarget, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("verified_push_failed: unable to fetch %s/%s for ancestry check: %w", remote, branch, err)
	}
	reachable, err := IsAncestor(g, commit, "FETCH_HEAD")
	if err != nil {
		return fmt.Errorf("verified_push_failed: unable to verify commit %s on %s/%s: %w", shortSHA(commit), remote, branch, err)
	}
	if !reachable {
		return fmt.Errorf("verified_push_failed: commit %s not on %s/%s (remote tip %s)", shortSHA(commit), remote, branch, shortSHA(tip))
	}
	return nil
}

func parseLSRemoteTip(out, branch string) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if parts[1] == "refs/heads/"+branch {
			return parts[0]
		}
	}
	return ""
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// RemoteTrackingBranchExists checks if a remote-tracking branch ref exists locally
// (e.g. refs/remotes/origin/main), without hitting the network.
func RemoteTrackingBranchExists(g *Git, remote, branch string) (bool, error) {
	ref := fmt.Sprintf("refs/remotes/%s/%s", remote, branch)
	_, err := run(g, "show-ref", "--verify", "--quiet", ref)
	if err != nil {
		if strings.Contains(err.Error(), "exit status 1") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DeleteBranch deletes a local branch.
func DeleteBranch(g *Git, name string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := run(g, "branch", flag, name)
	return err
}

// ListBranches returns all local branches matching a pattern.
// Pattern uses git's pattern matching (e.g., "polecat/*" matches all polecat branches).
// Returns branch names without the refs/heads/ prefix.
func ListBranches(g *Git, pattern string) ([]string, error) {
	args := []string{"branch", "--list", "--format=%(refname:short)"}
	if pattern != "" {
		args = append(args, pattern)
	}
	out, err := run(g, args...)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// ResetBranch force-updates a branch to point to a ref.
// This is useful for resetting stale polecat branches to main.
// NOTE: This uses `git branch -f` which fails on the currently checked-out branch.
// Use ResetHard instead when the target branch is checked out.
func ResetBranch(g *Git, name, ref string) error {
	_, err := run(g, "branch", "-f", name, ref)
	return err
}

// ResetHard resets the current working tree and index to the given ref.
// Unlike ResetBranch, this works on the currently checked-out branch.
func ResetHard(g *Git, ref string) error {
	_, err := run(g, "reset", "--hard", ref)
	return err
}

// CleanForce removes untracked files and directories from the working tree.
// Excludes .runtime/ to preserve agent lock files and session state.
func CleanForce(g *Git) error {
	_, err := run(g, "clean", "-fd", "--exclude=.runtime")
	return err
}

// Rev returns the commit hash for the given ref.
func Rev(g *Git, ref string) (string, error) {
	return run(g, "rev-parse", ref)
}

// IsAncestor checks if ancestor is an ancestor of descendant.
func IsAncestor(g *Git, ancestor, descendant string) (bool, error) {
	_, err := run(g, "merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		// Exit code 1 means not an ancestor, not an error
		if strings.Contains(err.Error(), "exit status 1") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Cherry runs `git cherry <upstream> <head>` to list commits on head that are
// not yet on upstream, comparing by patch-id. Each output line is prefixed with
// "+ " (patch not on upstream) or "- " (patch already applied upstream, e.g.
// via squash merge). Used to detect already-merged work that plain ancestor
// checks miss. See aa-apw.
func Cherry(g *Git, upstream, head string) (string, error) {
	return run(g, "cherry", upstream, head)
}

// WorktreeAdd creates a new worktree at the given path with a new branch.
// The new branch is created from the current HEAD.
// Skips LFS smudge filter during checkout (see WorktreeAddFromRef).
func WorktreeAdd(g *Git, path, branch string) error {
	if _, err := runWithEnv(g,
		[]string{"worktree", "add", "-b", branch, path},
		[]string{"GIT_LFS_SKIP_SMUDGE=1"},
	); err != nil {
		return err
	}
	return InitSubmodules(path, submoduleReferencePath(g))
}

// WorktreeAddFromRef creates a new worktree at the given path with a new branch
// starting from the specified ref (e.g., "origin/main").
// Skips LFS smudge filter during checkout to avoid downloading large LFS objects
// over NFS (~72s for 473MB). LFS files appear as pointer files initially;
// callers can run "git lfs pull" later when LFS content is actually needed.
func WorktreeAddFromRef(g *Git, path, branch, startPoint string) error {
	if _, err := runWithEnv(g,
		[]string{"worktree", "add", "-b", branch, path, startPoint},
		[]string{"GIT_LFS_SKIP_SMUDGE=1"},
	); err != nil {
		return err
	}
	return InitSubmodules(path, submoduleReferencePath(g))
}

// WorktreeAddDetached creates a new worktree at the given path with a detached HEAD.
// Skips LFS smudge filter during checkout (see WorktreeAddFromRef).
func WorktreeAddDetached(g *Git, path, ref string) error {
	if _, err := runWithEnv(g,
		[]string{"worktree", "add", "--detach", path, ref},
		[]string{"GIT_LFS_SKIP_SMUDGE=1"},
	); err != nil {
		return err
	}
	return InitSubmodules(path, submoduleReferencePath(g))
}

// WorktreeAddExisting creates a new worktree at the given path for an existing branch.
// Skips LFS smudge filter during checkout (see WorktreeAddFromRef).
func WorktreeAddExisting(g *Git, path, branch string) error {
	if _, err := runWithEnv(g,
		[]string{"worktree", "add", path, branch},
		[]string{"GIT_LFS_SKIP_SMUDGE=1"},
	); err != nil {
		return err
	}
	return InitSubmodules(path, submoduleReferencePath(g))
}

// WorktreeAddExistingForce creates a new worktree even if the branch is already checked out elsewhere.
// This is useful for cross-rig worktrees where multiple clones need to be on main.
func WorktreeAddExistingForce(g *Git, path, branch string) error {
	if _, err := run(g, "worktree", "add", "--force", path, branch); err != nil {
		return err
	}
	return InitSubmodules(path, submoduleReferencePath(g))
}

// submoduleReferencePath returns the mayor/rig path to use as --reference
// for submodule init. For bare repos (.repo.git), this resolves to the
// sibling mayor/rig directory which contains the initialized submodules.
// Returns empty string if no suitable reference path exists or if the
// reference repo is a shallow clone (git rejects shallow references).
func submoduleReferencePath(g *Git) string {
	// For bare repos, the gitDir is <rig>/.repo.git
	// The reference clone is at <rig>/mayor/rig/
	if g.gitDir != "" {
		rigDir := filepath.Dir(g.gitDir)
		mayorRig := filepath.Join(rigDir, "mayor", "rig")
		if isValidSubmoduleReference(mayorRig) {
			return mayorRig
		}
	}

	// For regular clones (workDir-based), the workDir itself could be mayor/rig
	// but we don't want to reference ourselves. Check for a sibling .repo.git
	// to find the rig root, then use mayor/rig.
	if g.workDir != "" {
		dir := g.workDir
		for i := 0; i < 4; i++ {
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			if _, err := os.Stat(filepath.Join(parent, ".repo.git")); err == nil {
				mayorRig := filepath.Join(parent, "mayor", "rig")
				if mayorRig != g.workDir && isValidSubmoduleReference(mayorRig) {
					return mayorRig
				}
				break
			}
			dir = parent
		}
	}

	return ""
}

// isValidSubmoduleReference checks if a path is suitable as a --reference
// for git submodule update. It must have a tracked .gitmodules and not be a
// shallow clone (git rejects shallow repos as references).
func isValidSubmoduleReference(repoPath string) bool {
	if !hasTrackedGitmodules(repoPath) {
		return false
	}
	// Check if shallow — git rev-parse --is-shallow-repository
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "--is-shallow-repository")
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != "true"
}

// IsSparseCheckoutConfigured checks if sparse checkout is enabled for a given repo/worktree.
// This is used by doctor to detect legacy sparse checkout configurations that should be removed.
func IsSparseCheckoutConfigured(repoPath string) bool {
	cmd := exec.Command("git", "-C", repoPath, "config", "core.sparseCheckout")
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(output)) == "true"
}

// RemoveSparseCheckout disables sparse checkout for a repo/worktree and restores all files.
// This is used by doctor to clean up legacy sparse checkout configurations.
func RemoveSparseCheckout(repoPath string) error {
	if err := EnsureSafeMutationWorkDir(repoPath); err != nil {
		return err
	}

	// Use git sparse-checkout disable which properly restores hidden files
	cmd := exec.Command("git", "-C", repoPath, "sparse-checkout", "disable")
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("disabling sparse checkout: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// WorktreeRemove removes a worktree.
func WorktreeRemove(g *Git, path string, force bool) error {
	args := []string{"worktree", "remove", path}
	if force {
		args = append(args, "--force")
	}
	_, err := run(g, args...)
	return err
}

// WorktreeMove moves a worktree to a new path, updating all git references.
// This is the correct way to relocate a worktree — using os.Rename breaks
// the .git file and worktree registry references. (GH#2056)
func WorktreeMove(g *Git, oldPath, newPath string) error {
	_, err := run(g, "worktree", "move", oldPath, newPath)
	return err
}

// WorktreePrune removes worktree entries for deleted paths.
func WorktreePrune(g *Git) error {
	_, err := run(g, "worktree", "prune")
	return err
}

// Worktree represents a git worktree.
type Worktree struct {
	Path   string
	Branch string
	Commit string
}

// WorktreeList returns all worktrees for this repository.
func WorktreeList(g *Git) ([]Worktree, error) {
	out, err := run(g, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}

	var worktrees []Worktree
	var current Worktree

	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			if current.Path != "" {
				worktrees = append(worktrees, current)
				current = Worktree{}
			}
			continue
		}

		switch {
		case strings.HasPrefix(line, "worktree "):
			current.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			current.Commit = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}

	// Don't forget the last one
	if current.Path != "" {
		worktrees = append(worktrees, current)
	}

	return worktrees, nil
}

// BranchCreatedDate returns the date when a branch was created.
// This uses the committer date of the first commit on the branch.
// Returns date in YYYY-MM-DD format.
func BranchCreatedDate(g *Git, branch string) (string, error) {
	// Get the date of the first commit on the branch that's not on the default branch
	// Use merge-base to find where the branch diverged
	defaultBranch := RemoteDefaultBranch(g)
	mergeBase, err := run(g, "merge-base", defaultBranch, branch)
	if err != nil {
		// If merge-base fails, fall back to the branch tip's date
		out, err := run(g, "log", "-1", "--format=%cs", branch)
		if err != nil {
			return "", err
		}
		return out, nil
	}

	// Get the first commit after the merge base on this branch
	out, err := run(g, "log", "--format=%cs", "--reverse", mergeBase+".."+branch)
	if err != nil {
		return "", err
	}

	// Get the first line (first commit's date)
	lines := strings.Split(out, "\n")
	if len(lines) > 0 && lines[0] != "" {
		return lines[0], nil
	}

	// If no commits after merge-base, the branch points to merge-base
	// Return the merge-base commit date
	out, err = run(g, "log", "-1", "--format=%cs", mergeBase)
	if err != nil {
		return "", err
	}
	return out, nil
}

// CommitsAhead returns the number of commits that branch has ahead of base.
// DiffStat returns the --stat output for a diff range (e.g., "main...feature").
func DiffStat(g *Git, rangeSpec string) (string, error) {
	return run(g, "diff", "--stat", rangeSpec)
}

// For example, CommitsAhead("main", "feature") returns how many commits
// are on feature that are not on main.
func CommitsAhead(g *Git, base, branch string) (int, error) {
	out, err := run(g, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return 0, err
	}

	var count int
	_, err = fmt.Sscanf(out, "%d", &count)
	if err != nil {
		return 0, fmt.Errorf("parsing commit count: %w", err)
	}

	return count, nil
}

// CountCommitsBehind returns the number of commits that HEAD is behind the given ref.
// For example, CountCommitsBehind("origin/main") returns how many commits
// are on origin/main that are not on the current HEAD.
func CountCommitsBehind(g *Git, ref string) (int, error) {
	out, err := run(g, "rev-list", "--count", "HEAD.."+ref)
	if err != nil {
		return 0, err
	}

	var count int
	_, err = fmt.Sscanf(out, "%d", &count)
	if err != nil {
		return 0, fmt.Errorf("parsing commit count: %w", err)
	}

	return count, nil
}

// BranchContamination holds the result of a branch contamination check.
type BranchContamination struct {
	Behind int // commits HEAD is behind base (e.g., origin/main)
	Ahead  int // commits HEAD is ahead of base
}

// CheckBranchContamination checks whether the current branch has diverged
// significantly from a base ref (typically origin/main). Returns the number
// of commits behind and ahead, letting callers decide severity thresholds.
// (GH#2220)
func CheckBranchContamination(g *Git, baseRef string) (BranchContamination, error) {
	var result BranchContamination

	behind, err := CountCommitsBehind(g, baseRef)
	if err != nil {
		return result, fmt.Errorf("counting commits behind %s: %w", baseRef, err)
	}
	result.Behind = behind

	ahead, err := CommitsAhead(g, baseRef, "HEAD")
	if err != nil {
		return result, fmt.Errorf("counting commits ahead of %s: %w", baseRef, err)
	}
	result.Ahead = ahead

	return result, nil
}

// StashCount returns the number of stashes belonging to the current branch.
// Git stashes are stored in the main repo (.git/refs/stash) and shared across
// all worktrees. Counting all stashes is incorrect for worktree-based polecats:
// a fresh polecat worktree would inherit stash count from siblings, blocking
// Remove(force=true) on work it never created. Filter by current branch name
// to only count stashes that actually belong to this worktree.
func StashCount(g *Git) (int, error) {
	out, err := run(g, "stash", "list")
	if err != nil {
		return 0, err
	}
	if out == "" {
		return 0, nil
	}
	filter := stashBranchFilter(g)
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if stashLineMatches(line, filter) {
			count++
		}
	}
	return count, nil
}

type stashFilter struct {
	wipPrefix string
	onPrefix  string
	filter    bool
}

func stashBranchFilter(g *Git) stashFilter {
	branch, err := CurrentBranch(g)
	if err != nil || branch == "" || branch == "HEAD" {
		return stashFilter{}
	}
	return stashFilter{
		wipPrefix: ": WIP on " + branch + ":",
		onPrefix:  ": On " + branch + ":",
		filter:    true,
	}
}

func stashLineMatches(line string, filter stashFilter) bool {
	if line == "" {
		return false
	}
	if !filter.filter {
		return true
	}
	return strings.Contains(line, filter.wipPrefix) || strings.Contains(line, filter.onPrefix)
}

// StashCountAll returns the total number of repo-wide stashes visible from the
// worktree. Git stores stashes in the shared repository, so callers must not use
// this as per-worktree risk; use StashCount for current-branch risk instead.
func StashCountAll(g *Git) (int, error) {
	out, err := run(g, "stash", "list")
	if err != nil {
		return 0, err
	}
	if out == "" {
		return 0, nil
	}

	count := 0
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			count++
		}
	}
	return count, nil
}

// StashEntry represents one entry from `git stash list`, scoped to the current branch.
type StashEntry struct {
	Ref     string // e.g. "stash@{2}"
	Message string // e.g. "WIP on main: <hash> <subject>"
}

// StashListForBranch returns all stash entries belonging to the current branch,
// ordered as `git stash list` returns them (newest first, i.e. stash@{0} first).
// Filtering matches StashCount: only entries with ": WIP on <branch>:" or
// ": On <branch>:" prefixes are returned, since stashes are global to the repo
// but conceptually belong to the worktree where they were created.
func StashListForBranch(g *Git) ([]StashEntry, error) {
	out, err := run(g, "stash", "list")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	filter := stashBranchFilter(g)
	var entries []StashEntry
	for _, line := range strings.Split(out, "\n") {
		if entry, ok := parseStashEntry(line, filter); ok {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func parseStashEntry(line string, filter stashFilter) (StashEntry, bool) {
	if !stashLineMatches(line, filter) {
		return StashEntry{}, false
	}
	colonIdx := strings.Index(line, ":")
	if colonIdx <= 0 {
		return StashEntry{}, false
	}
	return StashEntry{
		Ref:     line[:colonIdx],
		Message: strings.TrimSpace(line[colonIdx+1:]),
	}, true
}

// StashPop applies the given stash ref to the working tree and drops it on success.
// Returns an error if the pop has conflicts (working tree is left as-is for manual
// resolution). Callers should treat conflict errors as "stop, escalate to user".
func StashPop(g *Git, ref string) error {
	if ref == "" {
		return fmt.Errorf("stash ref required")
	}
	if _, err := run(g, "stash", "pop", ref); err != nil {
		return fmt.Errorf("git stash pop %s: %w", ref, err)
	}
	return nil
}

// UnpushedCommits returns the number of commits that are not pushed to the remote.
// It prefers the exact remote branch when one exists, because polecat branches may
// track origin/main while pushing work to origin/<current-branch>.
// Returns 0 if there is no upstream or exact remote branch configured.
func UnpushedCommits(g *Git) (int, error) {
	branch, branchErr := CurrentBranch(g)
	if branchErr != nil || branch == "" || branch == "HEAD" {
		branch = ""
	}

	status, err := CheckBranchPreservation(g, branch, "origin", nil)
	if err != nil {
		if errors.Is(err, errNoComparisonRefs) {
			return 0, nil
		}
		return 0, err
	}
	return status.UnpreservedPatchCount, nil
}

// BranchPreservationStatus describes whether HEAD is already preserved on a
// durable branch, and how many patch-unique commits remain if it is not.
type BranchPreservationStatus struct {
	Preserved             bool
	ComparisonBase        string
	UnpreservedPatchCount int
	Evidence              string
}

// BranchPreservationStatus checks whether HEAD is safe relative to the actual
// custody target for the branch. It prefers proof from the exact pushed source
// branch, then explicit target branches, then upstream. It only falls back to the
// remote default branch when no target/custody/upstream evidence exists.
func CheckBranchPreservation(g *Git, localBranch, remote string, targets []string) (BranchPreservationStatus, error) {
	return branchPreservationStatus(g, localBranch, remote, targets, true)
}

// BranchTargetStatus checks whether HEAD is already represented on the branch's
// target/custody refs. Unlike BranchPreservationStatus, the exact pushed source
// branch is not enough evidence because pushed-but-unsubmitted work still needs
// merge-queue recovery.
func BranchTargetStatus(g *Git, localBranch, remote string, targets []string) (BranchPreservationStatus, error) {
	return branchPreservationStatus(g, localBranch, remote, targets, false)
}

func branchPreservationStatus(g *Git, localBranch, remote string, targets []string, includeExactBranch bool) (BranchPreservationStatus, error) {
	if remote == "" {
		remote = "origin"
	}
	seeded, preserved := exactRemotePreservation(g, localBranch, remote, includeExactBranch)
	if preserved {
		return seeded, nil
	}
	candidates, hasEvidence := preservationCandidates(g, localBranch, remote, targets, includeExactBranch)
	if len(candidates) == 0 {
		if hasEvidence {
			return BranchPreservationStatus{}, fmt.Errorf("no target/custody refs resolved")
		}
		return BranchPreservationStatus{}, errNoComparisonRefs
	}
	return pickPreservationCandidate(g, candidates, seeded)
}

func exactRemotePreservation(g *Git, localBranch, remote string, includeExactBranch bool) (BranchPreservationStatus, bool) {
	if !includeExactBranch || localBranch == "" || localBranch == "HEAD" {
		return BranchPreservationStatus{}, false
	}
	remoteSHA, err := PushRemoteBranchTip(g, remote, localBranch)
	if err != nil || remoteSHA == "" {
		return BranchPreservationStatus{}, false
	}
	base := remote + "/" + localBranch
	contains, containsErr := refContainsHead(g, remoteSHA)
	if containsErr == nil && contains {
		return BranchPreservationStatus{
			Preserved:             true,
			ComparisonBase:        base,
			UnpreservedPatchCount: 0,
			Evidence:              "exact_remote_branch",
		}, true
	}
	return BranchPreservationStatus{ComparisonBase: base}, false
}

func preservationCandidates(g *Git, localBranch, remote string, targets []string, includeExactBranch bool) ([]string, bool) {
	var candidates []string
	hasEvidence := len(nonEmptyUnique(targets)) > 0
	if includeExactBranch && localBranch != "" && localBranch != "HEAD" {
		if remoteSHA, err := PushRemoteBranchTip(g, remote, localBranch); err == nil && remoteSHA != "" {
			hasEvidence = true
			candidates = append(candidates, remoteSHA)
		}
	}
	for _, target := range nonEmptyUnique(targets) {
		if ref, ok := resolveComparisonRef(g, target, remote); ok {
			candidates = append(candidates, ref)
		}
	}
	hasEvidence = appendUpstreamCandidate(g, localBranch, remote, includeExactBranch, hasEvidence, &candidates)
	if !hasEvidence {
		appendDefaultRemoteCandidates(g, remote, &candidates)
	}
	return nonEmptyUnique(candidates), hasEvidence
}

func appendUpstreamCandidate(g *Git, localBranch, remote string, includeExactBranch, hasEvidence bool, candidates *[]string) bool {
	upstream, err := run(g, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		return hasEvidence
	}
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return hasEvidence
	}
	if includeExactBranch || !isPolecatSelfUpstream(localBranch, remote, upstream) {
		*candidates = append(*candidates, upstream)
		return true
	}
	return hasEvidence
}

func appendDefaultRemoteCandidates(g *Git, remote string, candidates *[]string) {
	for _, ref := range []string{remote + "/" + RemoteDefaultBranch(g), remote + "/main", remote + "/master"} {
		if resolved, ok := resolveComparisonRef(g, ref, remote); ok {
			*candidates = append(*candidates, resolved)
		}
	}
}

func pickPreservationCandidate(g *Git, candidates []string, result BranchPreservationStatus) (BranchPreservationStatus, error) {
	var lastErr error
	for _, ref := range candidates {
		candidate, err := preservationAgainstRef(g, ref)
		if err != nil {
			lastErr = err
			continue
		}
		if candidate.Evidence == "" {
			candidate.Evidence = "comparison_ref"
		}
		if candidate.Preserved {
			return candidate, nil
		}
		if result.ComparisonBase == "" {
			result = candidate
		}
	}
	if result.ComparisonBase != "" {
		return result, nil
	}
	if lastErr != nil {
		return result, lastErr
	}
	return result, fmt.Errorf("no usable comparison refs")
}

func isPolecatSelfUpstream(localBranch, remote, upstream string) bool {
	return strings.HasPrefix(localBranch, "polecat/") && upstream == remote+"/"+localBranch
}

func refContainsHead(g *Git, ref string) (bool, error) {
	head, err := Rev(g, "HEAD")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(ref) == strings.TrimSpace(head) {
		return true, nil
	}
	return IsAncestor(g, "HEAD", ref)
}

func resolveComparisonRef(g *Git, ref, remote string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	for _, candidate := range comparisonRefCandidates(ref, remote) {
		if ok, err := RefExists(g, candidate); err == nil && ok {
			return candidate, true
		}
	}
	return "", false
}

func comparisonRefCandidates(ref, remote string) []string {
	if strings.HasPrefix(ref, "refs/") || strings.HasPrefix(ref, remote+"/") {
		return []string{ref}
	}
	if strings.HasPrefix(ref, "upstream/") {
		return []string{ref}
	}
	if !strings.Contains(ref, "/") && remote != "upstream" {
		return []string{"upstream/" + ref, remote + "/" + ref, ref}
	}
	return []string{remote + "/" + ref, ref}
}

func preservationAgainstRef(g *Git, ref string) (BranchPreservationStatus, error) {
	return preservationOfRefAgainstRef(g, "HEAD", ref)
}

func preservationOfRefAgainstRef(g *Git, head, ref string) (BranchPreservationStatus, error) {
	status := BranchPreservationStatus{ComparisonBase: ref}
	if contains, err := IsAncestor(g, head, ref); err == nil && contains {
		status.Preserved = true
		status.Evidence = "ancestor"
		return status, nil
	}
	if preserved, err := mergeTreeNoopBetweenRefs(g, head, ref); err == nil && preserved {
		status.Preserved = true
		status.Evidence = "merge_tree_noop"
		return status, nil
	}
	out, err := Cherry(g, ref, head)
	if err != nil {
		return status, err
	}
	status.UnpreservedPatchCount = CountCherryUnmergedCommits(out)
	status.Preserved = status.UnpreservedPatchCount == 0
	if status.Preserved {
		status.Evidence = "cherry"
	}
	return status, nil
}

func mergeTreeNoopBetweenRefs(g *Git, head, ref string) (bool, error) {
	refTree, err := run(g, "rev-parse", ref+"^{tree}")
	if err != nil {
		return false, err
	}
	mergedTree, err := run(g, "merge-tree", "--write-tree", ref, head)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(mergedTree) == strings.TrimSpace(refTree), nil
}

// PushRemoteRefTargetStatus checks whether a push-remote ref is preserved on
// target. It fetches the exact candidate ref first so remote-only tips and split
// fetch/push remotes are classified against the listed hash, not stale tracking
// refs.
func PushRemoteRefTargetStatus(g *Git, remote string, ref RemoteRef, target string) (BranchPreservationStatus, error) {
	var status BranchPreservationStatus
	refName := strings.TrimSpace(ref.Name)
	expectedHash := strings.TrimSpace(ref.Hash)
	if refName == "" || expectedHash == "" {
		return status, fmt.Errorf("remote ref is missing name or hash")
	}

	if _, err := run(g, "fetch", "--no-tags", pushTarget(g, remote), refName); err != nil {
		return status, fmt.Errorf("fetching candidate %s: %w", refName, err)
	}
	fetchedHash, err := Rev(g, "FETCH_HEAD")
	if err != nil {
		return status, fmt.Errorf("resolving fetched candidate %s: %w", refName, err)
	}
	fetchedHash = strings.TrimSpace(fetchedHash)
	if fetchedHash != expectedHash {
		return status, fmt.Errorf("candidate %s changed while pruning: expected %s, fetched %s", refName, shortSHA(expectedHash), shortSHA(fetchedHash))
	}

	return preservationOfRefAgainstRef(g, "FETCH_HEAD", target)
}

// CountCherryUnmergedCommits counts `git cherry` lines whose patches are not
// present on the comparison base.
func CountCherryUnmergedCommits(out string) int {
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "+") {
			count++
		}
	}
	return count
}

func nonEmptyUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// UncommittedWorkStatus contains information about uncommitted work in a repo.
type UncommittedWorkStatus struct {
	HasUncommittedChanges bool
	StashCount            int
	UnpushedCommits       int
	// Details for error messages
	ModifiedFiles  []string
	UntrackedFiles []string
	UnmergedFiles  []string
}

// Clean returns true if there is no uncommitted work.
func (s *UncommittedWorkStatus) Clean() bool {
	return !s.HasUncommittedChanges && s.StashCount == 0 && s.UnpushedCommits == 0 && len(s.UnmergedFiles) == 0
}

// CleanExcludingBeads returns true if the only uncommitted changes are .beads/ files.
// This is useful for polecat stale detection where beads database files are synced
// across worktrees and shouldn't block cleanup.
func (s *UncommittedWorkStatus) CleanExcludingBeads() bool {
	// Stashes and unpushed commits always count as uncommitted work
	if s.StashCount > 0 || s.UnpushedCommits > 0 || len(s.UnmergedFiles) > 0 {
		return false
	}

	// Check if all modified files are beads files
	for _, f := range s.ModifiedFiles {
		if !isBeadsPath(f) {
			return false
		}
	}

	// Check if all untracked files are beads files
	for _, f := range s.UntrackedFiles {
		if !isBeadsPath(f) {
			return false
		}
	}

	return true
}

// isBeadsPath returns true if the path is a .beads/ file.
func isBeadsPath(path string) bool {
	return strings.Contains(path, ".beads/") || strings.Contains(path, ".beads\\")
}

// runtimeArtifactRoot returns the path that should be reset when a runtime artifact
// is staged. Directory artifacts return the directory root so large trees like
// nested node_modules are unstaged with one pathspec instead of thousands.
func runtimeArtifactRoot(path string) (string, bool) {
	path = strings.TrimPrefix(filepath.ToSlash(strings.ReplaceAll(path, "\\", "/")), "./")
	bare := strings.TrimSuffix(path, "/")
	if bare == "" {
		return "", false
	}
	if root, ok := runtimeArtifactDirRoot(strings.Split(bare, "/")); ok {
		return root, true
	}
	return runtimeArtifactFileRoot(bare)
}

func runtimeArtifactDirRoot(parts []string) (string, bool) {
	for i, part := range parts {
		if runtimeArtifactDirs[part] {
			return strings.Join(parts[:i+1], "/") + "/", true
		}
	}
	return "", false
}

var runtimeArtifactDirs = map[string]bool{
	".beads": true, ".claude": true, ".opencode": true, ".agents": true,
	".runtime": true, ".logs": true, "__pycache__": true, "node_modules": true,
	".vite": true, ".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true,
	".cache": true, "coverage": true, "htmlcov": true,
}

func runtimeArtifactFileRoot(bare string) (string, bool) {
	base := filepath.Base(bare)
	lower := strings.ToLower(base)
	if runtimeArtifactFileName(base, lower) {
		return bare, true
	}
	return "", false
}

func runtimeArtifactFileName(base, lower string) bool {
	switch {
	case base == "CLAUDE.local.md", base == "AGENTS.local.md", base == ".DS_Store":
		return true
	case strings.HasSuffix(lower, ".db"), strings.HasSuffix(lower, ".pyc"), strings.HasSuffix(lower, ".pyo"):
		return true
	default:
		return false
	}
}

// isGasTownRuntimePath returns true if the path is a runtime artifact that should
// not block gt done. These paths are managed by tooling or test/build commands,
// not by the developer, and must not be auto-saved into polecat MRs.
func isGasTownRuntimePath(path string) bool {
	_, ok := runtimeArtifactRoot(path)
	return ok
}

// gitBinarySniffBytes is git's binary heuristic window: a NUL in the first
// 8000 bytes marks the file as binary.
const gitBinarySniffBytes = 8000

func isSafetyNetSkipped(workDir, path string) bool {
	if isGasTownRuntimePath(path) {
		return true
	}
	if workDir == "" {
		return false
	}
	return fileLooksBinary(filepath.Join(workDir, path))
}

func fileLooksBinary(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	f, err := os.Open(path) //nolint:gosec // G304: path is a git worktree relative file
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, gitBinarySniffBytes)
	n, _ := f.Read(buf)
	if n == 0 {
		return false
	}
	return looksBinary(buf[:n])
}

func looksBinary(buf []byte) bool {
	if hasExecutableMagic(buf) {
		return true
	}
	return bytes.IndexByte(buf, 0) >= 0
}

func hasExecutableMagic(buf []byte) bool {
	return hasPrefixMagic(buf, []byte{0x7f, 'E', 'L', 'F'}) ||
		hasPrefixMagic(buf, []byte{0x00, 'a', 's', 'm'}) ||
		hasPrefixMagic(buf, []byte{'M', 'Z'})
}

func hasPrefixMagic(buf, magic []byte) bool {
	return len(buf) >= len(magic) && bytes.Equal(buf[:len(magic)], magic)
}

// RuntimeArtifactPathspecs returns deduplicated git pathspecs for runtime
// artifacts in paths. Callers can pass the result to git reset after git add
// to keep generated state out of safety-net commits.
func RuntimeArtifactPathspecs(paths []string) []string {
	seen := make(map[string]bool)
	var pathspecs []string
	for _, f := range paths {
		root, ok := runtimeArtifactRoot(f)
		if !ok || seen[root] {
			continue
		}
		seen[root] = true
		pathspecs = append(pathspecs, root)
	}
	return pathspecs
}

// RuntimeArtifactPaths returns deduplicated pathspecs for runtime artifacts in the
// current uncommitted work. Callers can pass the result to git reset after git add
// to keep generated state out of safety-net commits.
func (s *UncommittedWorkStatus) RuntimeArtifactPaths() []string {
	paths := append(append([]string{}, s.ModifiedFiles...), s.UntrackedFiles...)
	return RuntimeArtifactPathspecs(paths)
}

// NonRuntimePaths returns uncommitted paths that are not covered by the runtime
// artifact policy. Recovery checks use this to ignore generated tool state while
// still blocking on real source changes.
func (s *UncommittedWorkStatus) NonRuntimePaths() []string {
	var paths []string
	paths = append(paths, s.UnmergedFiles...)
	for _, f := range append(append([]string{}, s.ModifiedFiles...), s.UntrackedFiles...) {
		if !isGasTownRuntimePath(f) {
			paths = append(paths, f)
		}
	}
	return paths
}

// CleanExcludingRuntime returns true if the only uncommitted changes are
// runtime artifacts covered by the centralized exclusion policy.
// Used by gt done to avoid blocking completion on toolchain-managed files.
//
// Note: UnpushedCommits and StashCount are intentionally NOT checked here. This
// function only evaluates whether uncommitted *file* changes are runtime artifacts.
// Unpushed commits represent committed (but not yet pushed) work, and stashes
// survive worktree deletion — both are handled separately and shouldn't block
// completion on runtime-only dirt (gas-7vg).
func (s *UncommittedWorkStatus) CleanExcludingRuntime() bool {
	if len(s.UnmergedFiles) > 0 {
		return false
	}

	for _, f := range s.ModifiedFiles {
		if !isGasTownRuntimePath(f) {
			return false
		}
	}

	for _, f := range s.UntrackedFiles {
		if !isGasTownRuntimePath(f) {
			return false
		}
	}

	return true
}

// CleanExcludingSafetyNet returns true when the only uncommitted file changes
// are runtime artifacts or binary files. Safety-net auto-save must not commit
// those, and gt done must not block on them after a source-only auto-save.
func (s *UncommittedWorkStatus) CleanExcludingSafetyNet(workDir string) bool {
	if len(s.UnmergedFiles) > 0 {
		return false
	}

	for _, f := range s.ModifiedFiles {
		if !isSafetyNetSkipped(workDir, f) {
			return false
		}
	}

	for _, f := range s.UntrackedFiles {
		if !isSafetyNetSkipped(workDir, f) {
			return false
		}
	}

	return true
}

// String returns a human-readable summary of uncommitted work.
func (s *UncommittedWorkStatus) String() string {
	var issues []string
	if s.HasUncommittedChanges {
		issues = append(issues, fmt.Sprintf("%d uncommitted change(s)", len(s.ModifiedFiles)+len(s.UntrackedFiles)+len(s.UnmergedFiles)))
	}
	if len(s.UnmergedFiles) > 0 {
		issues = append(issues, fmt.Sprintf("unmerged: %s", strings.Join(s.UnmergedFiles, ", ")))
	}
	if s.StashCount > 0 {
		issues = append(issues, fmt.Sprintf("%d stash(es)", s.StashCount))
	}
	if s.UnpushedCommits > 0 {
		issues = append(issues, fmt.Sprintf("%d unpushed commit(s)", s.UnpushedCommits))
	}
	if len(issues) == 0 {
		return "clean"
	}
	return strings.Join(issues, ", ")
}

// CheckUncommittedWork performs a comprehensive check for uncommitted work.
func CheckUncommittedWork(g *Git) (*UncommittedWorkStatus, error) {
	status := &UncommittedWorkStatus{}

	// Check git status
	gitStatus, err := Status(g)
	if err != nil {
		return nil, fmt.Errorf("checking git status: %w", err)
	}
	status.HasUncommittedChanges = !gitStatus.Clean
	status.ModifiedFiles = append(gitStatus.Modified, gitStatus.Added...)
	status.ModifiedFiles = append(status.ModifiedFiles, gitStatus.Deleted...)
	status.UntrackedFiles = gitStatus.Untracked
	status.UnmergedFiles = gitStatus.Unmerged

	// Check stashes
	stashCount, err := StashCount(g)
	if err != nil {
		return nil, fmt.Errorf("checking stashes: %w", err)
	}
	status.StashCount = stashCount

	// Check unpushed commits
	unpushed, err := UnpushedCommits(g)
	if err != nil {
		return nil, fmt.Errorf("checking unpushed commits: %w", err)
	}
	status.UnpushedCommits = unpushed

	return status, nil
}

// BranchPushedToRemote checks if a branch has been pushed to the remote.
// Returns (pushed bool, unpushedCount int, err).
// This handles polecat branches that don't have upstream tracking configured.
func BranchPushedToRemote(g *Git, localBranch, remote string) (bool, int, error) {
	status, err := CheckBranchPreservation(g, localBranch, remote, nil)
	if err != nil {
		return false, 0, err
	}
	return status.Preserved, status.UnpreservedPatchCount, nil
}

// PrunedBranch represents a local branch that was pruned (or would be pruned in dry-run).
type PrunedBranch struct {
	Name   string // Branch name (e.g., "polecat/rictus-mkb0vq9f")
	Reason string // Why it was pruned: "merged", "no-remote", "no-remote-merged"
}

// PruneStaleBranches finds and deletes local branches matching a pattern that are
// stale — either fully merged to the default branch or whose remote tracking branch
// no longer exists (indicating the remote branch was deleted after merge).
//
// This addresses cross-clone branch accumulation: when polecats push branches to
// origin, other clones create local tracking branches via git fetch. After the
// remote branch is deleted (post-merge), git fetch --prune removes the remote
// tracking ref but the local branch persists indefinitely.
//
// Safety: never deletes the current branch or the default branch (main/master).
// Uses git branch -d (not -D), so only fully-merged branches are deleted.
func PruneStaleBranches(g *Git, pattern string, dryRun bool) ([]PrunedBranch, error) {
	if pattern == "" {
		pattern = "polecat/*"
	}
	currentBranch, _ := CurrentBranch(g)
	defaultBranch := RemoteDefaultBranch(g)
	branches, err := ListBranches(g, pattern)
	if err != nil {
		return nil, fmt.Errorf("listing branches: %w", err)
	}
	var pruned []PrunedBranch
	for _, branch := range branches {
		if result, ok := pruneStaleBranch(g, strings.TrimSpace(branch), currentBranch, defaultBranch, dryRun); ok {
			pruned = append(pruned, result)
		}
	}
	return pruned, nil
}

func pruneStaleBranch(g *Git, branch, currentBranch, defaultBranch string, dryRun bool) (PrunedBranch, bool) {
	if branch == "" || branch == currentBranch || branch == defaultBranch {
		return PrunedBranch{}, false
	}
	reason, ok := staleBranchReason(g, branch, defaultBranch)
	if !ok {
		return PrunedBranch{}, false
	}
	if !dryRun {
		if err := DeleteBranch(g, branch, false); err != nil {
			return PrunedBranch{}, false
		}
	}
	return PrunedBranch{Name: branch, Reason: reason}, true
}

func staleBranchReason(g *Git, branch, defaultBranch string) (string, bool) {
	hasRemote, err := RemoteTrackingBranchExists(g, "origin", branch)
	if err != nil {
		return "", false
	}
	merged, err := IsAncestor(g, branch, "origin/"+defaultBranch)
	if err != nil {
		return "", false
	}
	switch {
	case merged && !hasRemote:
		return "no-remote-merged", true
	case merged:
		return "merged", true
	case !hasRemote:
		return "no-remote", true
	default:
		return "", false
	}
}

// SubmoduleChange represents a changed submodule pointer between two refs.
type SubmoduleChange struct {
	Path   string // Submodule path relative to repo root
	OldSHA string // Previous commit SHA (or empty for new submodule)
	NewSHA string // New commit SHA (or empty for removed submodule)
	URL    string // Submodule remote URL from .gitmodules
}

// InitSubmodules initializes and updates submodules if .gitmodules exists.
// This is a no-op for repos without submodules.
//
// If referencePath is non-empty and contains submodules, --reference is used
// to share git objects from a local clone instead of fetching from remote.
// This makes submodule init near-instant for large submodules (e.g. 655MB gitlabhq).
func InitSubmodules(repoPath string, referencePath ...string) error {
	if !hasTrackedGitmodules(repoPath) {
		return nil
	}
	if err := EnsureSafeMutationWorkDir(repoPath); err != nil {
		return err
	}

	args := []string{"-C", repoPath, "submodule", "update", "--init", "--recursive"}

	// Use --reference to share objects from a local clone (avoids remote fetch)
	if len(referencePath) > 0 && referencePath[0] != "" {
		refPath := referencePath[0]
		if hasTrackedGitmodules(refPath) {
			args = append(args, "--reference", refPath)
		}
	}

	cmd := exec.Command("git", args...)
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("initializing submodules: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// hasTrackedGitmodules checks whether .gitmodules exists on disk AND is tracked
// by git. After a submodule-to-monorepo migration, .gitmodules may linger as an
// untracked file (e.g., in a stale mayor/rig clone or bare repo worktree) even
// though it has been removed from the repository. Checking only os.Stat would
// incorrectly trigger submodule init on these stale artifacts.
func hasTrackedGitmodules(repoPath string) bool {
	gitmodules := filepath.Join(repoPath, ".gitmodules")
	if _, err := os.Stat(gitmodules); os.IsNotExist(err) {
		return false
	}
	// Verify .gitmodules is actually tracked in the index.
	cmd := exec.Command("git", "-C", repoPath, "ls-files", "--error-unmatch", ".gitmodules")
	return cmd.Run() == nil
}

// InitSparseCheckout initializes sparse checkout with cone mode and configures
// the given paths. If paths is empty, initializes with cone mode only (checkout root files).
func InitSparseCheckout(repoPath string, paths []string) error {
	if err := EnsureSafeMutationWorkDir(repoPath); err != nil {
		return err
	}

	// Initialize sparse checkout in cone mode
	cmd := exec.Command("git", "-C", repoPath, "sparse-checkout", "init", "--cone")
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("initializing sparse checkout: %s", strings.TrimSpace(stderr.String()))
	}
	if len(paths) > 0 {
		args := append([]string{"-C", repoPath, "sparse-checkout", "set"}, paths...)
		cmd = exec.Command("git", args...)
		util.SetDetachedProcessGroup(cmd)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("setting sparse checkout paths: %s", strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}

// SubmoduleChanges detects submodule pointer changes between two refs.
// Returns nil if no submodules changed or if the repo has no submodules.
func SubmoduleChanges(g *Git, base, head string) ([]SubmoduleChange, error) {
	// git diff --raw shows mode 160000 for gitlink (submodule) entries
	out, err := run(g, "diff", "--raw", base, head)
	if err != nil {
		return nil, fmt.Errorf("diffing for submodule changes: %w", err)
	}
	if out == "" {
		return nil, nil
	}

	var changes []SubmoduleChange
	for _, line := range strings.Split(out, "\n") {
		if change, ok := parseSubmoduleChange(line); ok {
			change.URL, _ = submoduleURL(g, head, change.Path)
			changes = append(changes, change)
		}
	}
	return changes, nil
}

func parseSubmoduleChange(line string) (SubmoduleChange, bool) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.Contains(line, "160000") {
		return SubmoduleChange{}, false
	}
	parts := strings.SplitN(line, "\t", 2)
	if len(parts) != 2 {
		return SubmoduleChange{}, false
	}
	path := strings.TrimSpace(parts[1])
	if strings.HasPrefix(path, ".claude/") {
		return SubmoduleChange{}, false
	}
	fields := strings.Fields(parts[0])
	if len(fields) < 5 {
		return SubmoduleChange{}, false
	}
	return SubmoduleChange{Path: path, OldSHA: nonNullSHA(fields[2]), NewSHA: nonNullSHA(fields[3])}, true
}

func nonNullSHA(sha string) string {
	if strings.Trim(sha, "0") == "" {
		return ""
	}
	return sha
}

// submoduleURL reads the URL for a submodule from .gitmodules at a given ref.
// Uses git config -f to parse the file correctly regardless of field ordering.
func submoduleURL(g *Git, ref, submodulePath string) (string, error) {
	content, err := run(g, "show", ref+":.gitmodules")
	if err != nil {
		return "", err
	}
	tmpName, cleanup, err := writeGitmodulesTemp(content)
	if err != nil {
		return "", err
	}
	defer cleanup()
	sectionName, err := submoduleSectionForPath(tmpName, submodulePath)
	if err != nil {
		return "", err
	}
	return submoduleSectionURL(tmpName, sectionName, submodulePath)
}

func writeGitmodulesTemp(content string) (string, func(), error) {
	tmpFile, err := os.CreateTemp("", "gitmodules-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp file for .gitmodules: %w", err)
	}
	if _, err := tmpFile.WriteString(content); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return "", nil, fmt.Errorf("writing temp .gitmodules: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpFile.Name())
		return "", nil, fmt.Errorf("closing temp .gitmodules: %w", err)
	}
	return tmpFile.Name(), func() { _ = os.Remove(tmpFile.Name()) }, nil
}

func submoduleSectionForPath(filename, submodulePath string) (string, error) {
	cmd := exec.Command("git", "config", "-f", filename, "--get-regexp", `^submodule\..*\.path$`)
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("reading submodule paths from .gitmodules: %w", err)
	}

	for _, line := range strings.Split(stdout.String(), "\n") {
		if sectionName, ok := matchingSubmoduleSection(line, submodulePath); ok {
			return sectionName, nil
		}
	}
	return "", fmt.Errorf("submodule URL not found for path %s", submodulePath)
}

func matchingSubmoduleSection(line, submodulePath string) (string, bool) {
	parts := strings.SplitN(strings.TrimSpace(line), " ", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != submodulePath {
		return "", false
	}
	key := strings.TrimPrefix(parts[0], "submodule.")
	return strings.TrimSuffix(key, ".path"), true
}

func submoduleSectionURL(filename, sectionName, submodulePath string) (string, error) {
	urlCmd := exec.Command("git", "config", "-f", filename, "--get", "submodule."+sectionName+".url")
	util.SetDetachedProcessGroup(urlCmd)
	var urlOut bytes.Buffer
	urlCmd.Stdout = &urlOut
	if err := urlCmd.Run(); err != nil {
		return "", fmt.Errorf("reading URL for submodule %s: %w", sectionName, err)
	}
	url := strings.TrimSpace(urlOut.String())
	if url == "" {
		return "", fmt.Errorf("submodule URL not found for path %s", submodulePath)
	}
	return url, nil
}

// PushSubmoduleCommit pushes a specific commit SHA from a submodule to its remote.
// The submodulePath is relative to the repo working directory.
// The commit must exist in the submodule's object store (shared via .repo.git/modules/).
func PushSubmoduleCommit(g *Git, submodulePath, sha, remote string) error {
	absPath := filepath.Join(g.workDir, submodulePath)
	// Detect the remote's default branch (don't assume main)
	defaultBranch, err := submoduleDefaultBranch(absPath, remote)
	if err != nil {
		return fmt.Errorf("detecting default branch for submodule %s: %w", submodulePath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", absPath, "push", remote, sha+":refs/heads/"+defaultBranch)
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("pushing submodule %s timed out after %v (remote may be unreachable)", submodulePath, pushTimeout)
		}
		abbrev := sha
		if len(abbrev) > 8 {
			abbrev = abbrev[:8]
		}
		return fmt.Errorf("pushing submodule %s commit %s: %s", submodulePath, abbrev, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// submoduleDefaultBranch detects the default branch of a submodule's remote.
// Tries local refs first to avoid network round-trips, falling back to remote queries.
func submoduleDefaultBranch(submodulePath, remote string) (string, error) {
	// Try local symbolic-ref first (no network, fastest)
	symCmd := exec.Command("git", "-C", submodulePath, "symbolic-ref", "refs/remotes/"+remote+"/HEAD")
	util.SetDetachedProcessGroup(symCmd)
	if symOut, err := symCmd.Output(); err == nil {
		ref := strings.TrimSpace(string(symOut))
		// refs/remotes/origin/HEAD -> refs/remotes/origin/main -> main
		if parts := strings.Split(ref, "/"); len(parts) > 0 {
			branch := parts[len(parts)-1]
			if branch != "" {
				return branch, nil
			}
		}
	}

	// Try local tracking refs (no network)
	for _, candidate := range []string{"main", "master"} {
		check := exec.Command("git", "-C", submodulePath, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote+"/"+candidate)
		util.SetDetachedProcessGroup(check)
		if check.Run() == nil {
			return candidate, nil
		}
	}

	// Fallback: network query via ls-remote
	for _, candidate := range []string{"main", "master"} {
		check := exec.Command("git", "-C", submodulePath, "ls-remote", "--exit-code", remote, "refs/heads/"+candidate)
		util.SetDetachedProcessGroup(check)
		if check.Run() == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not determine default branch for remote %s", remote)
}
