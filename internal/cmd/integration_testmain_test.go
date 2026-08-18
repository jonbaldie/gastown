//go:build integration

package cmd

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/jonbaldie/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	// Windows bd file locks need serialized tests. Linux/macOS CI must not
	// force --test.parallel=1: unit tests that already call t.Parallel()
	// can overlap. Dolt-backed tests that share TestMain's sql-server must
	// still omit t.Parallel(); a concurrent bd-init stampede takes the
	// native server down.
	if shouldSerializePackageTests() {
		_ = flag.Set("test.parallel", "1")
	}
	flag.Parse()

	// Start an ephemeral Dolt SQL server for this package's integration tests.
	// Tests like TestAgentWorktreesStayClean and TestBeadsRoutingFromTownRoot
	// spawn gt/bd subprocesses that create databases (e.g., "tr", "hq").
	// Routing those to GT_DOLT_PORT keeps them off the developer's town Dolt.
	if err := testutil.EnsureDoltContainerForTestMain(); err != nil {
		fmt.Fprintf(os.Stderr, "integration TestMain: dolt setup: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	testutil.TerminateDoltContainer()
	os.Exit(code)
}
