//go:build integration && !windows

package testutil

import (
	"testing"
	"time"
)

func TestNativeDoltSQLServerAcceptsConnectionsQuickly(t *testing.T) {
	if lookPathDolt() == "" {
		t.Skip("dolt binary not installed")
	}

	start := time.Now()
	srv, err := startNativeDoltSQLServer()
	if err != nil {
		t.Fatalf("startNativeDoltSQLServer: %v", err)
	}
	elapsed := time.Since(start)
	t.Cleanup(func() {
		if stopErr := stopNativeDoltSQLServer(srv); stopErr != nil {
			t.Logf("stop native dolt: %v", stopErr)
		}
	})

	if elapsed > 5*time.Second {
		t.Fatalf("native dolt sql-server took %s to accept connections, want <= 5s", elapsed)
	}
	if !portListening("127.0.0.1", srv.port) {
		t.Fatalf("native dolt sql-server is not listening on %s", srv.port)
	}
	if elapsed > time.Second {
		t.Logf("native dolt sql-server ready in %s", elapsed)
	}
}
