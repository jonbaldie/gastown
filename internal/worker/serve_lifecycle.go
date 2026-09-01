package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/process"
)

// FindServePIDs returns PIDs of `gt worker serve --town <townRoot>` processes
// for this town only. Other towns' worker servers are left alone.
func FindServePIDs(townRoot string) []int {
	if townRoot == "" {
		return nil
	}
	table, err := process.Capture()
	if err != nil {
		return nil
	}
	return collectServePIDs(table, townRoot, canonicalTownRoot(townRoot), os.Getpid())
}

func collectServePIDs(table process.Table, townRoot, townAbs string, self int) []int {
	seen := map[int]bool{}
	var pids []int
	for _, p := range table.All() {
		if !eligibleServePID(p, self, seen) {
			continue
		}
		if !servePIDMatchesTown(p, townRoot, townAbs) {
			continue
		}
		seen[p.PID] = true
		pids = append(pids, p.PID)
	}
	return pids
}

func eligibleServePID(p process.Proc, self int, seen map[int]bool) bool {
	return p.PID != self && p.PID > 0 && !seen[p.PID] && process.Alive(p.PID)
}

func servePIDMatchesTown(p process.Proc, townRoot, townAbs string) bool {
	line := p.Args
	if line == "" {
		line = process.CommandLine(p.PID)
	}
	if isTownWorkerServeArgs(line, townRoot) || isTownWorkerServeArgs(line, townAbs) {
		return true
	}
	fallback := process.CommandLine(p.PID)
	return isTownWorkerServeArgs(fallback, townRoot) || isTownWorkerServeArgs(fallback, townAbs)
}

func canonicalTownRoot(townRoot string) string {
	abs, err := filepath.Abs(townRoot)
	if err != nil {
		return filepath.Clean(townRoot)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// StopServe terminates this town's `gt worker serve` processes and removes
// stale socket/port files. It does not signal processes for other towns.
func StopServe(townRoot string) int {
	stopped, _ := StopServeAndWait(townRoot)
	return stopped
}

// StopServeAndWait stops every process owned by this town's worker server and
// does not remove its connection files until each captured descendant has
// exited. A detached worker uses a fresh session, so stopping only its leader
// can otherwise leave children reparented to init after the Town is removed.
func StopServeAndWait(townRoot string) (int, error) {
	pids := FindServePIDs(townRoot)
	var errs []error
	for _, pid := range pids {
		if err := stopServePID(pid); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return len(pids), fmt.Errorf("stopping worker serve: %w", errors.Join(errs...))
	}
	_ = os.Remove(SocketPath(townRoot))
	_ = os.Remove(PortPath(townRoot))
	return len(pids), nil
}

func isTownWorkerServeArgs(args, townRoot string) bool {
	if args == "" || townRoot == "" {
		return false
	}
	fields := strings.Fields(args)
	for i, field := range fields {
		if townArgumentMatches(fields, i, field, townRoot) {
			return hasWorkerServePrefix(fields[:i])
		}
	}
	return false
}

func townArgumentMatches(fields []string, index int, field, townRoot string) bool {
	if field == "--town" {
		return index+1 < len(fields) && sameTownPath(fields[index+1], townRoot)
	}
	if strings.HasPrefix(field, "--town=") {
		return sameTownPath(strings.TrimPrefix(field, "--town="), townRoot)
	}
	return false
}

func hasWorkerServePrefix(fields []string) bool {
	sawWorker := false
	for _, field := range fields {
		if isWorkerToken(field) {
			sawWorker = true
			continue
		}
		if sawWorker && isServeToken(field) {
			return true
		}
	}
	return false
}

func isWorkerToken(field string) bool {
	return field == "worker" || filepath.Base(field) == "worker"
}

func isServeToken(field string) bool {
	return field == "serve" || filepath.Base(field) == "serve"
}

func sameTownPath(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	if got == want {
		return true
	}
	gotAbs, gotErr := filepath.Abs(got)
	wantAbs, wantErr := filepath.Abs(want)
	if gotErr != nil || wantErr != nil {
		return false
	}
	if gotAbs == wantAbs {
		return true
	}
	gotRes, gotResErr := filepath.EvalSymlinks(gotAbs)
	wantRes, wantResErr := filepath.EvalSymlinks(wantAbs)
	return gotResErr == nil && wantResErr == nil && gotRes == wantRes
}

func stopServePID(pid int) error {
	pids := ownedProcessTree(pid)
	_ = terminateServeGroup(pid)
	for _, child := range pids[1:] {
		_ = terminateServePID(child)
	}
	if waitForPIDsToExit(pids, 2*time.Second) {
		return nil
	}

	_ = killServeGroup(pid)
	for _, child := range pids[1:] {
		_ = killServePID(child)
	}
	if waitForPIDsToExit(pids, 500*time.Millisecond) {
		return nil
	}
	return fmt.Errorf("worker process tree did not exit: %v", livePIDs(pids))
}

func ownedProcessTree(pid int) []int {
	pids := []int{pid}
	table, err := process.Capture()
	if err == nil {
		pids = append(pids, table.Descendants(pid)...)
	}
	return pids
}

func waitForPIDsToExit(pids []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if len(livePIDs(pids)) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func livePIDs(pids []int) []int {
	var live []int
	for _, pid := range pids {
		if !process.Exited(pid) {
			live = append(live, pid)
		}
	}
	return live
}
