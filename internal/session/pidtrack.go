package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/jonbaldie/gastown/internal/util"
)

// pidStartTimeFunc is overridden in tests. This package's tests must NOT use
// t.Parallel() because they mutate this package-level variable without synchronization.
var pidStartTimeFunc = processStartTime

type trackedPID struct {
	PID       int
	StartTime string
}

// pidsDir returns the directory for PID tracking files.
// All PID files live under <townRoot>/.runtime/pids/ since tmux session
// names are globally unique (they include the rig name).
func pidsDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "pids")
}

// pidFile returns the path to a PID file for a given session.
func pidFile(townRoot, sessionID string) string {
	return filepath.Join(pidsDir(townRoot), sessionID+".pid")
}

// TrackSessionPID captures the pane PID of a tmux session and writes it
// to a PID tracking file. This is defense-in-depth: if a session dies
// unexpectedly and KillSessionWithProcesses can't find the tmux pane,
// we still have the PID on disk for cleanup.
//
// This is best-effort — errors are returned but callers should treat them
// as non-fatal since the primary kill mechanism (KillSessionWithProcesses)
// doesn't depend on PID files.
func TrackSessionPID(townRoot, sessionID string, t TmuxOps) error {
	pidStr, err := t.GetPanePID(sessionID)
	if err != nil {
		return fmt.Errorf("getting pane PID: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
	if err != nil {
		return fmt.Errorf("parsing PID %q: %w", pidStr, err)
	}

	return TrackPID(townRoot, sessionID, pid)
}

// TrackPID writes a PID to a tracking file for later cleanup.
func TrackPID(townRoot, sessionID string, pid int) error {
	dir := pidsDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating pids directory: %w", err)
	}

	path := pidFile(townRoot, sessionID)
	record := strconv.Itoa(pid)
	if start, err := pidStartTimeFunc(pid); err == nil && start != "" {
		record = fmt.Sprintf("%d|%s", pid, start)
	}
	return os.WriteFile(path, []byte(record+"\n"), 0644)
}

// UntrackPID removes the PID tracking file for a session.
func UntrackPID(townRoot, sessionID string) {
	_ = os.Remove(pidFile(townRoot, sessionID))
}

// KillTrackedPIDs reads all PID files and kills any processes that are
// still running. Returns the number of processes killed and any session
// names that had errors.
//
// This is designed for the shutdown orphan-cleanup phase: after all
// sessions have been killed through normal means, this catches any
// processes that survived (e.g., reparented to init after SIGHUP).
func KillTrackedPIDs(townRoot string) (killed int, errSessions []string) {
	dir := pidsDir(townRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, []string{fmt.Sprintf("read pids dir: %v", err)}
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}

		killedOne, errSession := killTrackedPID(dir, entry)
		killed += killedOne
		if errSession != "" {
			errSessions = append(errSessions, errSession)
		}
	}

	return killed, errSessions
}

func killTrackedPID(dir string, entry os.DirEntry) (int, string) {
	sessionID := strings.TrimSuffix(entry.Name(), ".pid")
	path := filepath.Join(dir, entry.Name())
	record, ok, errSession := readTrackedPID(path, sessionID)
	if errSession != "" || !ok {
		return 0, errSession
	}

	proc, ok, errSession := findTrackedProcess(path, sessionID, record)
	if errSession != "" || !ok {
		return 0, errSession
	}
	return terminateTrackedProcess(path, sessionID, record.PID, proc)
}

func readTrackedPID(path, sessionID string) (trackedPID, bool, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return trackedPID{}, false, fmt.Sprintf("%s: read error: %v", sessionID, err)
	}
	record, err := parseTrackedPID(strings.TrimSpace(string(data)))
	if err != nil {
		// Corrupt PID file — remove it.
		_ = os.Remove(path)
		return trackedPID{}, false, ""
	}
	return record, true, ""
}

func findTrackedProcess(path, sessionID string, record trackedPID) (*os.Process, bool, string) {
	// Check if process is still alive.
	proc, err := os.FindProcess(record.PID)
	if err != nil {
		_ = os.Remove(path)
		return nil, false, ""
	}

	// Signal 0 checks existence without killing.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process is already dead — clean up PID file.
		_ = os.Remove(path)
		return nil, false, ""
	}

	return verifyTrackedProcess(path, sessionID, record, proc)
}

func verifyTrackedProcess(path, sessionID string, record trackedPID, proc *os.Process) (*os.Process, bool, string) {
	if record.StartTime == "" {
		return proc, true, ""
	}

	currentStart, err := pidStartTimeFunc(record.PID)
	if err != nil {
		// Cannot verify process identity — leave the PID file so a future cleanup
		// attempt can retry once ps is available again.
		return nil, false, fmt.Sprintf("%s (PID %d): cannot verify start time: %v — skipping kill, preserving tracking file", sessionID, record.PID, err)
	}
	if currentStart != record.StartTime {
		// Confirmed PID reuse — safe to remove tracking file.
		_ = os.Remove(path)
		return nil, false, ""
	}
	return proc, true, ""
}

func terminateTrackedProcess(path, sessionID string, pid int, proc *os.Process) (int, string) {
	defer func() { _ = os.Remove(path) }()
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return 0, fmt.Sprintf("%s (PID %d): SIGTERM failed: %v", sessionID, pid, err)
	}
	return 1, ""
}

func parseTrackedPID(value string) (trackedPID, error) {
	if value == "" {
		return trackedPID{}, fmt.Errorf("empty pid record")
	}
	parts := strings.SplitN(value, "|", 2)
	pid, err := strconv.Atoi(parts[0])
	if err != nil {
		return trackedPID{}, err
	}
	record := trackedPID{PID: pid}
	if len(parts) == 2 {
		record.StartTime = parts[1]
	}
	return record, nil
}

// processStartTime returns the start time of a process via ps(1).
// This works on Linux and macOS. On Windows (or minimal containers without ps),
// the call will fail and callers degrade gracefully to PID-only tracking.
func processStartTime(pid int) (string, error) {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	util.SetDetachedProcessGroup(cmd)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
