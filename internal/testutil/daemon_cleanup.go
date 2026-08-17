package testutil

import (
	"testing"

	"github.com/jonbaldie/gastown/internal/daemon"
)

// StopDaemonOnCleanup registers test cleanup for the town daemon that a command
// under test started. A daemon detaches from its caller, so without this it
// outlives the test and keeps patrolling the town: it respawns and kills tmux
// sessions, and restarts Dolt, long after the town directory is gone. It stops
// only the daemon that owns townRoot, so a production daemon is never touched.
func StopDaemonOnCleanup(t testing.TB, townRoot string) {
	t.Helper()
	t.Cleanup(func() {
		if err := daemon.StopDaemon(townRoot); err != nil {
			t.Logf("town daemon cleanup skipped: %v", err)
		}
	})
}
