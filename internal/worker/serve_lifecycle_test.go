package worker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonbaldie/gastown/internal/process"
)

func TestIsTownWorkerServeArgs(t *testing.T) {
	t.Parallel()
	town := "/tmp/gt-uat2/town"
	other := "/tmp/other-town"
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"real serve", "gt worker serve --town /tmp/gt-uat2/town", true},
		{"abs binary", "/usr/local/bin/gt worker serve --town /tmp/gt-uat2/town", true},
		{"equals form", "gt worker serve --town=/tmp/gt-uat2/town", true},
		{"status is not serve", "gt worker status --town /tmp/gt-uat2/town", false},
		{"other town", "gt worker serve --town /tmp/other-town", false},
		{"unrelated python", "python3 app.py --town /tmp/gt-uat2/town", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTownWorkerServeArgs(tc.args, town); got != tc.want {
				t.Errorf("isTownWorkerServeArgs(%q, %q) = %v, want %v", tc.args, town, got, tc.want)
			}
			if tc.want && isTownWorkerServeArgs(tc.args, other) {
				t.Errorf("matched other town for %q", tc.args)
			}
		})
	}
}

func TestStopServeStopsOnlyThisTown(t *testing.T) {
	if testing.Short() {
		t.Skip("starts local stand-in processes")
	}

	thisTown := t.TempDir()
	otherTown := t.TempDir()
	thisCmd := startWorkerStandIn(t, thisTown)
	otherCmd := startWorkerStandIn(t, otherTown)
	thisPID := thisCmd.Process.Pid
	otherPID := otherCmd.Process.Pid
	thisChildPID := workerChildPID(t, thisTown)

	waitUntil(t, func() bool {
		return containsPID(FindServePIDs(thisTown), thisPID) &&
			containsPID(FindServePIDs(otherTown), otherPID)
	})

	if stopped, err := StopServeAndWait(thisTown); err != nil {
		t.Fatalf("StopServeAndWait: %v", err)
	} else if stopped == 0 {
		t.Fatal("StopServe stopped 0 processes for this town")
	}
	done := make(chan struct{})
	go func() { _, _ = thisCmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}
	if leftover := FindServePIDs(thisTown); len(leftover) > 0 {
		t.Fatalf("this town worker still detected after StopServe: %v", leftover)
	}
	if process.Alive(thisChildPID) {
		t.Fatalf("StopServe left worker descendant PID %d alive", thisChildPID)
	}
	if !process.Alive(otherPID) {
		t.Fatalf("other town worker PID %d was killed", otherPID)
	}
}

func startWorkerStandIn(t *testing.T, townRoot string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(buildTinyGTStandIn(t), "worker", "serve", "--town", townRoot)
	childPIDFile := filepath.Join(townRoot, "worker-child.pid")
	cmd.Env = append(os.Environ(), "GT_WORKER_STANDIN_CHILD_PID="+childPIDFile)
	cmd.SysProcAttr = serveSysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start worker stand-in: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_, _ = StopServeAndWait(townRoot)
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})
	return cmd
}

func workerChildPID(t *testing.T, townRoot string) int {
	t.Helper()
	path := filepath.Join(townRoot, "worker-child.pid")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, err := fmt.Sscanf(string(data), "%d", &pid); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for worker child PID in %s", path)
	return 0
}

var cachedTinyGT string

func buildTinyGTStandIn(t *testing.T) string {
	t.Helper()
	if cachedTinyGT != "" {
		if _, err := os.Stat(cachedTinyGT); err == nil {
			return cachedTinyGT
		}
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	const standIn = `package main
import (
 "fmt"
 "os"
 "os/exec"
 "time"
)
func main() {
 if len(os.Args) > 1 && os.Args[1] == "--worker-child" { for { time.Sleep(time.Second) } }
 child := exec.Command(os.Args[0], "--worker-child")
 if err := child.Start(); err != nil { panic(err) }
 if path := os.Getenv("GT_WORKER_STANDIN_CHILD_PID"); path != "" { _ = os.WriteFile(path, []byte(fmt.Sprintf("%d\\n", child.Process.Pid)), 0644) }
 for { time.Sleep(time.Second) }
}
`
	if err := os.WriteFile(src, []byte(standIn), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "gt")
	cmd := exec.Command("go", "build", "-o", out, src)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build tiny gt: %v\n%s", err, data)
	}
	cachedTinyGT = out
	return out
}

func waitUntil(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for worker stand-in")
}

func containsPID(pids []int, want int) bool {
	for _, pid := range pids {
		if pid == want {
			return true
		}
	}
	return false
}
