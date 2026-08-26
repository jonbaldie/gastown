//go:build integration && windows

package testutil

import (
	"fmt"
	"testing"
)

// IntegrationDoltEnabled reports whether this binary was built with the
// integration test tag, which includes Dolt server support.
func IntegrationDoltEnabled() bool { return true }

// StartIsolatedDoltContainer is not supported on Windows CI.
func StartIsolatedDoltContainer(t *testing.T) string {
	t.Helper()
	t.Skip("Docker not available on Windows CI")
	return ""
}

// EnsureDoltContainerForTestMain is not supported on Windows CI.
func EnsureDoltContainerForTestMain() error {
	return fmt.Errorf("Docker not available on Windows CI")
}

// RequireDoltContainer is not supported on Windows CI.
func RequireDoltContainer(t *testing.T) {
	t.Helper()
	t.Skip("Docker not available on Windows CI")
}

func DoltContainerAddr() string { return "" }

func DoltContainerPort() string { return "" }

func TerminateDoltContainer() {}

func DoltContainersEnabled() bool { return false }
