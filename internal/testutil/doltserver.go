//go:build integration && !windows

package testutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // required by testcontainers Dolt module
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/dolt"
)

type doltServerState struct {
	ctr         *dolt.DoltContainer
	native      *nativeDoltServer
	attached    bool
	ctrOnce     sync.Once
	ctrErr      error
	ctrPort     string
	dockerOnce  sync.Once
	dockerAvail bool
}

var doltStateInstance = sync.OnceValue(func() *doltServerState {
	return &doltServerState{}
})

func doltState() *doltServerState {
	return doltStateInstance()
}

// IntegrationDoltEnabled reports whether this binary was built with the
// integration test tag, which includes Dolt server support.
func IntegrationDoltEnabled() bool { return true }

// isDockerAvailable returns true if the Docker daemon is reachable.
// The result is cached after the first call.
func isDockerAvailable() bool {
	state := doltState()
	state.dockerOnce.Do(func() {
		state.dockerAvail = exec.Command("docker", "info").Run() == nil
	})
	return state.dockerAvail
}

// isReaperRemovingErr returns true if the error is a transient "removing"
// status from the testcontainers Ryuk reaper. This happens when a previous
// test run's reaper container is still being cleaned up by Docker.
func isReaperRemovingErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unexpected container status") &&
		strings.Contains(err.Error(), "removing")
}

func isDockerUnavailableErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rootless docker not found") ||
		strings.Contains(msg, "cannot connect to the docker daemon") ||
		strings.Contains(msg, "no docker host")
}

func runDoltContainer(ctx context.Context) (ctr *dolt.DoltContainer, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("testcontainers docker unavailable: %v", r)
		}
	}()

	return dolt.Run(ctx, DoltDockerImage,
		dolt.WithDatabase("gt_test"),
		testcontainers.WithEnv(map[string]string{"DOLT_ROOT_HOST": "%"}),
	)
}

// runDoltContainerWithRetry calls dolt.Run, retrying on transient reaper
// "removing" errors up to 3 times with exponential backoff.
func runDoltContainerWithRetry(ctx context.Context) (*dolt.DoltContainer, error) {
	const maxRetries = 3
	delay := 2 * time.Second
	var lastErr error
	for attempt := range maxRetries {
		ctr, err := runDoltContainer(ctx)
		if err == nil {
			return ctr, nil
		}
		lastErr = err
		if !isReaperRemovingErr(err) {
			return nil, err
		}
		if attempt < maxRetries-1 {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return nil, lastErr
}

func publishDoltPort(port string) {
	doltState().ctrPort = port
	os.Setenv("GT_DOLT_PORT", port)         //nolint:tenv // intentional process-wide env
	os.Setenv("BEADS_DOLT_PORT", port)      //nolint:tenv // intentional process-wide env
	os.Setenv("GT_TEST_EXTERNAL_DOLT", "1") //nolint:tenv // tests reuse this sql-server
}

func startSharedDockerDoltContainer() {
	state := doltState()
	ctx := context.Background()
	ctr, err := runDoltContainerWithRetry(ctx)
	if err != nil {
		state.ctrErr = fmt.Errorf("starting Dolt container: %w", err)
		return
	}

	p, err := ctr.MappedPort(ctx, "3306/tcp")
	if err != nil {
		state.ctrErr = fmt.Errorf("getting mapped port: %w", err)
		_ = testcontainers.TerminateContainer(ctr)
		return
	}

	state.ctr = ctr
	publishDoltPort(p.Port())
}

// startSharedDoltContainer starts one shared Dolt SQL server for the test
// process. Native `dolt sql-server` is preferred over Docker because CI already
// installs that binary and container boot is the expensive part of the suite.
func startSharedDoltContainer() {
	state := doltState()
	port := envDoltPort()
	portOK := port != "" && portListening("127.0.0.1", port)
	backend := selectSharedDoltBackend(port, portOK, lookPathDolt(), false)
	if backend == "none" && isDockerAvailable() {
		backend = "docker"
	}
	switch backend {
	case "attached":
		state.attached = true
		publishDoltPort(port)
		return
	case "native":
		srv, err := startNativeDoltSQLServer()
		if err == nil {
			state.native = srv
			publishDoltPort(srv.port)
			return
		}
		if !isDockerAvailable() {
			state.ctrErr = fmt.Errorf("starting native Dolt sql-server: %w", err)
			return
		}
		fmt.Fprintf(os.Stderr, "testutil: native dolt sql-server failed (%v); trying Docker\n", err)
		startSharedDockerDoltContainer()
	case "docker":
		startSharedDockerDoltContainer()
	default:
		state.ctrErr = fmt.Errorf("no Dolt sql-server available: install the dolt binary or Docker")
	}
}

// StartIsolatedDoltContainer starts a per-test Dolt SQL server and returns the
// mapped host port. GT_DOLT_PORT is set via t.Setenv (scoped to the test).
// The server is terminated automatically when the test finishes.
func StartIsolatedDoltContainer(t *testing.T) string {
	t.Helper()
	if lookPathDolt() != "" {
		srv, err := startNativeDoltSQLServer()
		if err != nil {
			t.Fatalf("starting isolated Dolt sql-server: %v", err)
		}
		t.Cleanup(func() {
			if err := stopNativeDoltSQLServer(srv); err != nil {
				t.Logf("stopping isolated Dolt sql-server: %v", err)
			}
		})
		t.Setenv("GT_DOLT_PORT", srv.port)
		return srv.port
	}

	if !isDockerAvailable() {
		t.Skip("dolt binary and Docker are unavailable, skipping test")
	}

	ctx := context.Background()
	ctr, err := runDoltContainerWithRetry(ctx)
	if err != nil {
		if isDockerUnavailableErr(err) {
			t.Skipf("Dolt container unavailable: %v", err)
		}
		t.Fatalf("starting Dolt container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminating Dolt container: %v", err)
		}
	})

	port, err := ctr.MappedPort(ctx, "3306/tcp")
	if err != nil {
		t.Fatalf("getting mapped port: %v", err)
	}

	portStr := port.Port()
	t.Setenv("GT_DOLT_PORT", portStr)
	return portStr
}

// EnsureDoltContainerForTestMain starts a shared Dolt SQL server for TestMain.
// Call TerminateDoltContainer() after m.Run() to clean up servers this package
// started. Sets both GT_DOLT_PORT and BEADS_DOLT_PORT process-wide.
func EnsureDoltContainerForTestMain() error {
	state := doltState()
	state.ctrOnce.Do(startSharedDoltContainer)
	return state.ctrErr
}

// RequireDoltContainer ensures a shared Dolt SQL server is running. Skips the
// test if neither a dolt binary nor Docker can provide one.
func RequireDoltContainer(t *testing.T) {
	t.Helper()
	state := doltState()
	state.ctrOnce.Do(startSharedDoltContainer)
	if state.ctrErr != nil {
		if isDockerUnavailableErr(state.ctrErr) {
			t.Skipf("Dolt sql-server unavailable: %v", state.ctrErr)
		}
		if lookPathDolt() == "" && !isDockerAvailable() {
			t.Skipf("Dolt sql-server unavailable: %v", state.ctrErr)
		}
		t.Fatalf("Dolt sql-server setup failed: %v", state.ctrErr)
	}
}

// DoltContainerAddr returns the address (host:port) of the Dolt container.
func DoltContainerAddr() string {
	return "127.0.0.1:" + doltState().ctrPort
}

// DoltContainerPort returns the mapped host port of the Dolt container.
func DoltContainerPort() string {
	return doltState().ctrPort
}

// TerminateDoltContainer stops a Dolt SQL server that this package started.
// Attached servers (GT_DOLT_PORT already live) are left running.
func TerminateDoltContainer() {
	state := doltState()
	if state.attached {
		state.attached = false
		state.ctrPort = ""
		return
	}
	if state.native != nil {
		_ = stopNativeDoltSQLServer(state.native)
		state.native = nil
		state.ctrPort = ""
		return
	}
	if state.ctr != nil {
		_ = testcontainers.TerminateContainer(state.ctr)
		state.ctr = nil
		state.ctrPort = ""
	}
}

// DoltContainersEnabled reports whether TestMain has a usable Dolt SQL server.
func DoltContainersEnabled() bool {
	state := doltState()
	return state.ctrPort != "" && state.ctrErr == nil
}
