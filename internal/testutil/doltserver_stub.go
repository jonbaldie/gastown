//go:build !integration

package testutil

import (
	"fmt"
	"testing"
)

// DoltDockerImage is the Docker image used for Dolt test containers.
// The real starter lives behind -tags=integration.
const DoltDockerImage = "dolthub/dolt-sql-server:2.0.7"

const integrationTagRequired = "Dolt test containers require -tags=integration"

// StartIsolatedDoltContainer skips when the integration tag is off so default
// `go test` does not compile testcontainers.
func StartIsolatedDoltContainer(t *testing.T) string {
	t.Helper()
	t.Skip(integrationTagRequired)
	return ""
}

// EnsureDoltContainerForTestMain reports that the integration tag is required.
// Callers that treat this as optional keep running non-Dolt tests.
func EnsureDoltContainerForTestMain() error {
	return fmt.Errorf("%s", integrationTagRequired)
}

// RequireDoltContainer skips when the integration tag is off.
func RequireDoltContainer(t *testing.T) {
	t.Helper()
	t.Skip(integrationTagRequired)
}

// DoltContainerAddr returns empty string without the integration tag.
func DoltContainerAddr() string { return "" }

// DoltContainerPort returns empty string without the integration tag.
func DoltContainerPort() string { return "" }

// TerminateDoltContainer is a no-op without the integration tag.
func TerminateDoltContainer() {}
