package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/crew"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/process"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/worker"
	"github.com/spf13/cobra"
)

const (
	shutdownLockFile    = "daemon/shutdown.lock"
	shutdownLockTimeout = 5 * time.Second

	// ShutdownSentinel is a file written during gt down to prevent agents from
	// restarting the daemon mid-shutdown. Checked by ensureDaemon.
	ShutdownSentinel = "daemon/shutting-down"

	// defaultDownOrphanGraceSecs is the grace period for orphan cleanup during gt down.
	// Short because gt down is meant to be quick - processes already had SIGTERM via
	// KillSessionWithProcesses.
	defaultDownOrphanGraceSecs = 5
)

var downCmd = &cobra.Command{
	Use:     "down",
	GroupID: GroupServices,
	Short:   "Stop all Gas Town services",
	Long: `Stop Gas Town services (reversible pause).

Shutdown levels (progressively more aggressive):
  gt down                    Stop infrastructure (default)
  gt down --polecats         Also stop all polecat sessions
  gt down --all              Full shutdown with orphan cleanup
  gt down --nuke             Also kill the shared tmux server

Infrastructure agents stopped:
  • Crew       - Per-rig crew member sessions
  • Refineries - Per-rig work processors
  • Witnesses  - Per-rig polecat managers
  • Mayor      - Global work coordinator
  • Boot       - Deacon's watchdog
  • Deacon     - Health orchestrator
  • Daemon     - Go background process
  • Dolt       - Shared SQL database server
  • Worker     - Town gt worker serve process

This is a "pause" operation - use 'gt start' to bring everything back up.
For permanent cleanup (removing worktrees), use 'gt shutdown' instead.

Use cases:
  • Taking a break (stop token consumption)
  • Clean shutdown before system maintenance
  • Resetting the town to a clean state`,
	RunE: runDown,
}

func init() {
	downCmd.Flags().BoolP("quiet", "q", false, "Only show errors")
	downCmd.Flags().BoolP("force", "f", false, "Force kill without graceful shutdown")
	downCmd.Flags().BoolP("polecats", "p", false, "Also stop all polecat sessions")
	downCmd.Flags().BoolP("all", "a", false, "Full shutdown with orphan cleanup and verification")
	downCmd.Flags().Bool("nuke", false, "Kill the shared tmux server (default socket) and all its sessions")
	downCmd.Flags().Bool("dry-run", false, "Preview what would be stopped without taking action")
	rootCmd.AddCommand(downCmd)
}

// stopAllPolecats stops all polecat sessions across all rigs.
// Stops are performed in parallel for faster teardown.
// Returns the number of polecats stopped (or would be stopped in dry-run).
func downRigManager(townRoot string) *rig.Manager {
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}
	return rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot))
}

func stopAllPolecats(t *tmux.Tmux, townRoot string, rigNames []string, force bool, dryRun bool) int {
	rigMgr := downRigManager(townRoot)
	if dryRun {
		return countPolecatsDryRun(t, rigMgr, rigNames)
	}
	stopped := 0

	// Collect targets and stop all in parallel.
	type polecatResult struct {
		rigName string
		name    string
		err     error
	}

	var results []polecatResult
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, rigName := range rigNames {
		r, err := rigMgr.GetRig(rigName)
		if err != nil {
			continue
		}

		polecatMgr := polecat.NewSessionManager(t, r)
		infos, err := polecatMgr.ListPolecats()
		if err != nil {
			continue
		}

		for _, info := range infos {
			wg.Add(1)
			go func(rn, name string, mgr *polecat.SessionManager) {
				defer wg.Done()
				err := mgr.Stop(name, force)
				mu.Lock()
				results = append(results, polecatResult{rigName: rn, name: name, err: err})
				mu.Unlock()
			}(rigName, info.Polecat, polecatMgr)
		}
	}
	wg.Wait()

	for _, res := range results {
		if res.err == nil {
			stopped++
			fmt.Printf("  %s [%s] %s stopped\n", style.SuccessPrefix, res.rigName, res.name)
		} else {
			fmt.Printf("  %s [%s] %s: %s\n", style.ErrorPrefix, res.rigName, res.name, res.err.Error())
		}
	}

	return stopped
}

func countPolecatsDryRun(t *tmux.Tmux, rigMgr *rig.Manager, rigNames []string) int {
	stopped := 0
	for _, rigName := range rigNames {
		r, err := rigMgr.GetRig(rigName)
		if err != nil {
			continue
		}
		polecatMgr := polecat.NewSessionManager(t, r)
		infos, err := polecatMgr.ListPolecats()
		if err != nil {
			continue
		}
		for _, info := range infos {
			stopped++
			fmt.Printf("  %s [%s] %s would stop\n", style.Dim.Render("○"), rigName, info.Polecat)
		}
	}
	return stopped
}

type downCrewTarget struct {
	rigName   string
	name      string
	sessionID string
}

func collectCrewStopTargets(t *tmux.Tmux, rigMgr *rig.Manager, g *git.Git, rigNames []string, dryRun bool) ([]downCrewTarget, int) {
	var targets []downCrewTarget
	stopped := 0
	for _, rigName := range rigNames {
		r, err := rigMgr.GetRig(rigName)
		if err != nil {
			continue
		}
		crewMgr := crew.NewManager(r, g)
		workers, err := crewMgr.List()
		if err != nil {
			continue
		}
		for _, worker := range workers {
			sessionID := crewMgr.SessionName(worker.Name)
			running, err := t.HasSession(sessionID)
			if err != nil || !running {
				continue
			}
			if dryRun {
				stopped++
				fmt.Printf("  %s [%s] crew/%s would stop\n", style.Dim.Render("○"), rigName, worker.Name)
				continue
			}
			targets = append(targets, downCrewTarget{rigName: rigName, name: worker.Name, sessionID: sessionID})
		}
	}
	return targets, stopped
}

func stopAllCrew(t *tmux.Tmux, townRoot string, rigNames []string, force bool, dryRun bool) int {
	rigMgr := downRigManager(townRoot)
	targets, stopped := collectCrewStopTargets(t, rigMgr, git.NewGit(townRoot), rigNames, dryRun)
	if len(targets) == 0 {
		return stopped
	}

	type crewResult struct {
		rigName string
		name    string
		err     error
	}
	results := make([]crewResult, len(targets))
	var wg sync.WaitGroup

	for i, tgt := range targets {
		wg.Add(1)
		go func(i int, tgt downCrewTarget) {
			defer wg.Done()
			_, err := stopSession(t, tgt.sessionID, force)
			results[i] = crewResult{rigName: tgt.rigName, name: tgt.name, err: err}
		}(i, tgt)
	}
	wg.Wait()

	for _, res := range results {
		if res.err == nil {
			stopped++
			fmt.Printf("  %s [%s] crew/%s stopped\n", style.SuccessPrefix, res.rigName, res.name)
		} else {
			fmt.Printf("  %s [%s] crew/%s: %s\n", style.ErrorPrefix, res.rigName, res.name, res.err.Error())
		}
	}

	return stopped
}

func printDownStatus(name string, ok bool, detail string, quiet bool) {
	if quiet && ok {
		return
	}
	if ok {
		fmt.Printf("%s %s: %s\n", style.SuccessPrefix, name, style.Dim.Render(detail))
	} else {
		fmt.Printf("%s %s: %s\n", style.ErrorPrefix, name, detail)
	}
}

// stopSession gracefully stops a tmux session.
// Returns (wasRunning, error) - wasRunning is true if session existed and was stopped.
func stopSession(t *tmux.Tmux, sessionName string, force bool) (bool, error) {
	running, err := t.HasSession(sessionName)
	if err != nil {
		return false, err
	}
	if !running {
		return false, nil // Already stopped
	}

	// Try graceful shutdown first (Ctrl-C, best-effort interrupt)
	if !force {
		_ = t.SendKeysRaw(sessionName, "C-c")
		if session.WaitForSessionExit(t, sessionName, constants.GracefulShutdownTimeout) {
			return true, nil // Process exited gracefully
		}
	}

	// Kill the session (with explicit process termination to prevent orphans)
	return true, t.KillSessionWithProcesses(sessionName)
}

// acquireShutdownLock prevents concurrent shutdowns.
// Returns the lock (caller must defer Unlock()) or error if lock held.
func acquireShutdownLock(townRoot string) (*flock.Flock, error) {
	lockPath := filepath.Join(townRoot, shutdownLockFile)

	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}

	lock := flock.New(lockPath)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownLockTimeout)
	defer cancel()

	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("lock acquisition failed: %w", err)
	}

	if !locked {
		return nil, fmt.Errorf("another shutdown is in progress (lock held: %s)", lockPath)
	}

	return lock, nil
}

// verifyShutdown checks for respawned processes after shutdown.
// Returns list of things that are still running or respawned.
func verifyShutdown(t *tmux.Tmux, townRoot string) []string {
	var respawned []string
	respawned = appendShutdownTmuxSessions(respawned, t)
	respawned = appendShutdownDaemonPID(respawned, townRoot)
	return appendShutdownProcessChecks(respawned, townRoot)
}

func appendShutdownTmuxSessions(respawned []string, t *tmux.Tmux) []string {
	sessions, err := t.ListSessions()
	if err != nil {
		return respawned
	}
	for _, sess := range sessions {
		if session.IsKnownSession(sess) {
			respawned = append(respawned, fmt.Sprintf("tmux session %s", sess))
		}
	}
	return respawned
}

func appendShutdownDaemonPID(respawned []string, townRoot string) []string {
	pidData, err := os.ReadFile(filepath.Join(townRoot, "daemon", "daemon.pid"))
	if err != nil {
		return respawned
	}
	var pid int
	if _, err := fmt.Sscanf(string(pidData), "%d", &pid); err != nil {
		return respawned
	}
	if isProcessRunning(pid) {
		respawned = append(respawned, fmt.Sprintf("gt daemon (PID %d)", pid))
	}
	return respawned
}

func appendShutdownProcessChecks(respawned []string, townRoot string) []string {
	if pids := findOrphanedClaudeProcesses(townRoot); len(pids) > 0 {
		respawned = append(respawned, fmt.Sprintf("orphaned Claude processes (PIDs: %v)", pids))
	}
	if pids := doltserver.FindIdleMonitorProcesses(townRoot); len(pids) > 0 {
		respawned = append(respawned, fmt.Sprintf("bd dolt idle-monitor processes (PIDs: %v)", pids))
	}
	if pids := findOrphanDoltServers(townRoot); len(pids) > 0 {
		respawned = append(respawned, fmt.Sprintf("orphan Dolt servers (PIDs: %v)", pids))
	}
	if running, pid, err := doltserver.IsRunning(townRoot); err == nil && running {
		respawned = append(respawned, fmt.Sprintf("town Dolt server (PID %d)", pid))
	}
	if pids := worker.FindServePIDs(townRoot); len(pids) > 0 {
		respawned = append(respawned, fmt.Sprintf("gt worker serve (PIDs: %v)", pids))
	}
	return respawned
}

// findOrphanedClaudeProcesses finds Gas Town agent processes (claude/codex/opencode/cursor-agent/copilot/node)
// that are running in the town directory but aren't associated with any active tmux session.
// This can happen when tmux sessions are killed but child processes don't terminate.
//
// Only matches processes whose full command line references the town root path,
// which avoids false positives on unrelated Node.js applications (VS Code
// extensions, web servers, etc.).
func findOrphanedClaudeProcesses(townRoot string) []int {
	table, err := process.Capture()
	if err != nil {
		return nil
	}

	var orphaned []int
	for _, p := range table.All() {
		if !process.IsKnownAgent(p.Name) {
			continue
		}
		if strings.Contains(p.Args, townRoot) {
			orphaned = append(orphaned, p.PID)
		}
	}
	return orphaned
}

// cleanupLegacyDefaultSocket removes Gas Town sessions left on the "default"
// tmux socket by old binaries. Returns the number of sessions cleaned.
func cleanupLegacyDefaultSocket() int {
	return session.CleanupLegacyDefaultSocket()
}

// countLegacyDefaultSocketSessions counts Gas Town sessions on the "default"
// tmux socket (for dry-run output).
func countLegacyDefaultSocketSessions() int {
	return session.CountLegacyDefaultSocketSessions()
}

// cleanupLegacyBaseSocket removes Gas Town sessions left on the old basename-only
// tmux socket (e.g., "gt") by binaries from before path-hashed socket names were
// introduced (e.g., "gt-a1b2c3"). Returns the number of sessions cleaned.
func cleanupLegacyBaseSocket(townRoot string) int {
	return session.CleanupLegacyBaseSocket(townRoot)
}

// countLegacyBaseSocketSessions counts Gas Town sessions on the old basename-only
// tmux socket (for dry-run output).
func countLegacyBaseSocketSessions(townRoot string) int {
	return session.CountLegacyBaseSocketSessions(townRoot)
}

// stopIdleMonitors terminates idle-monitor processes.
// Returns the number of processes successfully stopped.
func stopIdleMonitors(pids []int) int {
	var stopped int
	for _, pid := range pids {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Signal(os.Interrupt); err != nil {
			// Process may have already exited
			continue
		}
		// Brief wait for graceful exit
		time.Sleep(200 * time.Millisecond)
		if !isProcessRunning(pid) {
			stopped++
			continue
		}
		_ = proc.Kill()
		stopped++
	}
	return stopped
}

// findOrphanDoltServers finds dolt sql-server processes whose working
// directory is within the town root but NOT the canonical .dolt-data/ dir.
// These are rogues spawned by bd from .beads/dolt/ directories.
func findOrphanDoltServers(townRoot string) []int {
	table, err := process.Capture()
	if err != nil {
		return nil
	}

	canonicalDir, _ := filepath.Abs(filepath.Join(townRoot, ".dolt-data"))
	townAbs, _ := filepath.Abs(townRoot)

	var pids []int
	for _, p := range table.All() {
		if isOrphanDoltServer(p, townAbs, canonicalDir) {
			pids = append(pids, p.PID)
		}
	}
	return pids
}

func isOrphanDoltServer(p process.Proc, townAbs, canonicalDir string) bool {
	line := p.Args
	if !strings.Contains(line, "dolt") || !strings.Contains(line, "sql-server") {
		return false
	}
	cwd := processCWD(p.PID)
	if cwd == "" {
		return false
	}
	cwdAbs, _ := filepath.Abs(cwd)
	inTown := cwdAbs == townAbs || strings.HasPrefix(cwdAbs, townAbs+string(filepath.Separator))
	return inTown && !strings.HasPrefix(cwdAbs, canonicalDir)
}

func processCWD(pid int) string {
	cwdOut, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-Fn", "-d", "cwd").Output()
	if err != nil {
		return ""
	}
	for _, cwdLine := range strings.Split(string(cwdOut), "\n") {
		if strings.HasPrefix(cwdLine, "n") {
			return cwdLine[1:]
		}
	}
	return ""
}

// stopOrphanDoltServers terminates orphan Dolt servers.
// Returns the number of processes stopped.
func stopOrphanDoltServers(pids []int) int {
	var stopped int
	for _, pid := range pids {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Signal(os.Interrupt); err != nil {
			continue
		}
		// Wait up to 3s for Dolt to flush and exit
		for i := 0; i < 6; i++ {
			time.Sleep(500 * time.Millisecond)
			if !isProcessRunning(pid) {
				break
			}
		}
		if isProcessRunning(pid) {
			_ = proc.Kill()
		}
		stopped++
	}
	return stopped
}

// findBeadsDoltDirs finds .beads/dolt directories that trigger bd auto-spawning.
// These are legacy per-agent data directories that should have been migrated
// to .dolt-data/ by gt dolt migrate.
func findBeadsDoltDirs(townRoot string) []string {
	var dirs []string
	townAbs, _ := filepath.Abs(townRoot)

	_ = filepath.WalkDir(townAbs, func(path string, d os.DirEntry, err error) error {
		skip, match := beadsDoltWalkDecision(townAbs, path, d, err)
		if match {
			dirs = append(dirs, path)
		}
		if skip {
			return filepath.SkipDir
		}
		return nil
	})
	return dirs
}

func beadsDoltWalkDecision(townAbs, path string, d os.DirEntry, err error) (skip, match bool) {
	if err != nil || skipBeadsWalkDir(townAbs, path, d) {
		return true, false
	}
	if !d.IsDir() {
		return false, false
	}
	if d.Name() == "dolt" && strings.HasSuffix(filepath.Dir(path), ".beads") {
		return true, true
	}
	return false, false
}

func skipBeadsWalkDir(townAbs, path string, d os.DirEntry) bool {
	if !d.IsDir() {
		return false
	}
	switch d.Name() {
	case ".dolt-data", ".git", "node_modules", ".repo.git":
		return true
	}
	rel, _ := filepath.Rel(townAbs, path)
	return strings.Count(rel, string(filepath.Separator)) > 5
}

// removeBeadsDoltDirs removes legacy .beads/dolt directories that are safe to
// delete. A directory is safe if it is empty or contains only Dolt metadata
// (no .dolt subdirectory with actual database content). Directories with
// unmigrated database data are skipped to avoid data loss.
// Returns count removed.
func removeBeadsDoltDirs(dirs []string) int {
	var removed int
	for _, dir := range dirs {
		if !isSafeToRemoveBeadsDolt(dir) {
			fmt.Fprintf(os.Stderr, "Warning: skipping %s — may contain unmigrated data\n", dir)
			continue
		}
		if err := os.RemoveAll(dir); err == nil {
			removed++
		}
	}
	return removed
}

// isSafeToRemoveBeadsDolt checks if a .beads/dolt directory can be safely
// removed. Safe means: empty, or contains no actual database content
// (no .dolt subdirectory with working data). Unmigrated databases have
// a .dolt/ directory inside with noms/manifest files.
func isSafeToRemoveBeadsDolt(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false // can't read it, don't remove it
	}
	if len(entries) == 0 {
		return true // empty dir is safe
	}

	// Check if any subdirectory contains a .dolt directory (unmigrated DB)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dotDolt := filepath.Join(dir, entry.Name(), ".dolt")
		if _, err := os.Stat(dotDolt); err == nil {
			return false // has unmigrated database data
		}
	}

	// Also check if .dolt exists directly in this dir
	if _, err := os.Stat(filepath.Join(dir, ".dolt")); err == nil {
		return false
	}

	return true
}
