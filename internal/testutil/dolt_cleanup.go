package testutil

import (
	"sync"
	"testing"

	"github.com/jonbaldie/gastown/internal/doltserver"
)

// ReapOwnedDoltOnCleanup registers test cleanup for Dolt servers whose metadata
// and process args prove they belong to townRoot. It never kills by broad name or
// port, so production Dolt is protected when tests run inside a real workspace.
func ReapOwnedDoltOnCleanup(t testing.TB, townRoot string) {
	t.Helper()
	var once sync.Once
	var stopped int
	var cleanupErr error
	cleanup := func() {
		once.Do(func() {
			stopped, cleanupErr = doltserver.ReapOwnedTestServers(townRoot)
		})
	}
	unregister := RegisterProcessCleanup(cleanup)
	t.Cleanup(func() {
		cleanup()
		unregister()
		if cleanupErr != nil {
			t.Logf("owned Dolt cleanup skipped: %v", cleanupErr)
			return
		}
		if stopped > 0 {
			t.Logf("stopped %d owned Dolt sql-server process(es)", stopped)
		}
	})
}
