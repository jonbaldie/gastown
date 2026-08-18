package testutil

import "testing"

// SkipWithoutDoltContainer skips tests that need the shared Dolt SQL server.
// Default `go test` (no integration tag) always skips. With -tags=integration,
// tests skip when TestMain did not start or attach a server.
func SkipWithoutDoltContainer(t *testing.T) {
	t.Helper()
	if !DoltContainersEnabled() {
		t.Skip("Dolt test sql-server not running; need -tags=integration and a dolt binary or Docker")
	}
}
