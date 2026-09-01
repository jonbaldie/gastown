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

type processCleanupState struct {
	sync.Mutex
	entries []*processCleanup
}

var processCleanupStateInstance = sync.OnceValue(func() *processCleanupState {
	return &processCleanupState{}
})

// processExit is the test process's replaceable termination boundary.
var processExit = os.Exit

func processCleanups() *processCleanupState {
	return processCleanupStateInstance()
}

// RegisterProcessCleanup records cleanup that must run when an integration test
// process is interrupted. The returned function unregisters the cleanup after
// a test's normal t.Cleanup path has run it. Cleanups are once-only, so a
// signal racing ordinary test teardown is safe.
func RegisterProcessCleanup(cleanup func()) func() {
	entry := &processCleanup{fn: cleanup}
	state := processCleanups()
	state.Lock()
	state.entries = append(state.entries, entry)
	state.Unlock()

	return func() {
		state.Lock()
		defer state.Unlock()
		for i, candidate := range state.entries {
			if candidate == entry {
				state.entries = append(state.entries[:i], state.entries[i+1:]...)
				return
			}
		}
	}
}

func runRegisteredProcessCleanups() {
	state := processCleanups()
	state.Lock()
	entries := state.entries
	state.entries = nil
	state.Unlock()

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
			processExit(1)
		case <-done:
		}
	}()

	code := m.Run()
	close(done)
	signal.Stop(signals)
	finish()
	processExit(code)
}
