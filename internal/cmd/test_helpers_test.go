package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// shouldSerializePackageTests reports whether this OS needs --test.parallel=1.
// Windows bd file locks historically required that. Linux/macOS CI does not:
// the integration TestMain used to serialize every test in this package,
// including hundreds of t.Parallel unit tests, for a Windows-only constraint.
func shouldSerializePackageTests() bool {
	return runtime.GOOS == "windows"
}

func TestShouldSerializePackageTestsWindowsOnly(t *testing.T) {
	t.Parallel()
	got := shouldSerializePackageTests()
	if runtime.GOOS == "windows" && !got {
		t.Fatal("Windows must serialize package tests to avoid bd file locks")
	}
	if runtime.GOOS != "windows" && got {
		t.Fatal("non-Windows must not force --test.parallel=1")
	}
}

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
