package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// buildGT builds the gt binary and returns its path.
// It caches the build across tests in the same run, including parallel tests.
var (
	gtBinaryOnce sync.Once
	gtBinaryPath string
	gtBinaryErr  error
)

func buildGT(t *testing.T) string {
	t.Helper()

	gtBinaryOnce.Do(func() {
		wd, err := os.Getwd()
		if err != nil {
			gtBinaryErr = err
			return
		}

		projectRoot := wd
		for {
			if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(projectRoot)
			if parent == projectRoot {
				gtBinaryErr = os.ErrNotExist
				return
			}
			projectRoot = parent
		}

		tmpDir := os.TempDir()
		binaryName := "gt-integration-test"
		if runtime.GOOS == "windows" {
			binaryName += ".exe"
		}
		tmpBinary := filepath.Join(tmpDir, binaryName)
		cmd := exec.Command("go", "build", "-o", tmpBinary, "./cmd/gt")
		cmd.Dir = projectRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			gtBinaryErr = err
			gtBinaryPath = string(output)
			return
		}
		gtBinaryPath = tmpBinary
	})

	if gtBinaryErr != nil {
		if gtBinaryErr == os.ErrNotExist {
			t.Fatal("could not find project root (go.mod)")
		}
		if gtBinaryPath != "" {
			t.Fatalf("failed to build gt: %v\nOutput: %s", gtBinaryErr, gtBinaryPath)
		}
		t.Fatalf("failed to build gt: %v", gtBinaryErr)
	}
	return gtBinaryPath
}
