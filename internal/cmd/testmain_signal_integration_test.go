//go:build integration

package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/jonbaldie/gastown/internal/testutil"
)

const testMainSignalHelperEnv = "GT_TESTMAIN_SIGNAL_HELPER"

// TestIntegrationTestMainRunsRegisteredCleanupOnTermination is the red-capable
// regression loop for interrupted integration tests. It runs this package's
// actual test binary, makes the helper register cleanup for a resource it owns,
// then sends SIGTERM. The marker proves TestMain ran that cleanup before exit.
func TestIntegrationTestMainRunsRegisteredCleanupOnTermination(t *testing.T) {
	if os.Getenv(testMainSignalHelperEnv) == "1" {
		marker := os.Getenv("GT_TESTMAIN_SIGNAL_MARKER")
		testutil.RegisterProcessCleanup(func() {
			_ = os.WriteFile(marker, []byte("cleaned"), 0o600)
		})
		fmt.Println("test-main-signal-helper-ready")
		select {}
	}

	marker := filepath.Join(t.TempDir(), "cleanup-marker")
	cmd := exec.Command(os.Args[0], "-test.run=^TestIntegrationTestMainRunsRegisteredCleanupOnTermination$")
	cmd.Env = append(os.Environ(), testMainSignalHelperEnv+"=1", "GT_TESTMAIN_SIGNAL_MARKER="+marker)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("opening helper stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}

	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "test-main-signal-helper-ready" {
				ready <- nil
				return
			}
		}
		ready <- scanner.Err()
	}()

	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("helper exited before becoming ready: %v", err)
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("helper did not become ready")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("terminating helper: %v", err)
	}
	_ = cmd.Wait()

	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "cleaned" {
		t.Fatalf("SIGTERM left registered integration cleanup unrun: marker=%q err=%v", data, err)
	}
}
