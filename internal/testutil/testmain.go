package testutil

import (
	"os"
	"os/signal"
	"sync"
	"testing"
)

type processCleanup struct {
	once sync.Once
	fn   func()
}

func (c *processCleanup) run() {
	c.once.Do(c.fn)
}

var testProcessCleanups = struct {
	sync.Mutex
	entries []*processCleanup
}{}

// RegisterProcessCleanup records cleanup that must run when an integration test
// process is interrupted. The returned function unregisters the cleanup after
// a test's normal t.Cleanup path has run it. Cleanups are once-only, so a
// signal racing ordinary test teardown is safe.
func RegisterProcessCleanup(cleanup func()) func() {
	entry := &processCleanup{fn: cleanup}
	testProcessCleanups.Lock()
	testProcessCleanups.entries = append(testProcessCleanups.entries, entry)
	testProcessCleanups.Unlock()

	return func() {
		testProcessCleanups.Lock()
		defer testProcessCleanups.Unlock()
		for i, candidate := range testProcessCleanups.entries {
			if candidate == entry {
				testProcessCleanups.entries = append(testProcessCleanups.entries[:i], testProcessCleanups.entries[i+1:]...)
				return
			}
		}
	}
}

func runRegisteredProcessCleanups() {
	testProcessCleanups.Lock()
	entries := testProcessCleanups.entries
	testProcessCleanups.entries = nil
	testProcessCleanups.Unlock()

	// Match testing.T.Cleanup's LIFO semantics: a Town's sessions stop before
	// its daemon, which stops before its owned Dolt server.
	for i := len(entries) - 1; i >= 0; i-- {
		entries[i].run()
	}
}

// RunTestMain executes m with deterministic process cleanup. It handles the
// termination signals a developer normally uses to stop a test run, so a
// detached integration daemon, test tmux server, or test Dolt server cannot
// outlive the test process. SIGKILL cannot be handled by any process.
func RunTestMain(m *testing.M, cleanup ...func()) {
	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() {
			runRegisteredProcessCleanups()
			for i := len(cleanup) - 1; i >= 0; i-- {
				cleanup[i]()
			}
		})
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, testMainTerminationSignals()...)
	done := make(chan struct{})
	go func() {
		select {
		case <-signals:
			finish()
			os.Exit(1)
		case <-done:
		}
	}()

	code := m.Run()
	close(done)
	signal.Stop(signals)
	finish()
	os.Exit(code)
}
