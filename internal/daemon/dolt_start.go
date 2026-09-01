package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Start starts the Dolt SQL server.
func (m *DoltServerManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked()
}

// startLocked starts the Dolt server. Must be called with m.mu held.
func (m *DoltServerManager) startLocked() error {
	if startHook := doltStartHook(m); startHook != nil {
		return startHook()
	}
	return startDoltProcess(m)
}

// startDoltProcess starts a daemon-managed Dolt server. The manager owns the
// lifecycle lock; this helper keeps process preparation separate from state
// bookkeeping and health verification.
func startDoltProcess(m *DoltServerManager) error {
	// Re-check if the server is already running to close the TOCTOU window.
	// Another goroutine may have started the server while we were waiting
	// for the mutex (via Start()) or during backoff sleep (via restartWithBackoff()).
	if _, running := m.isRunning(); running {
		m.logger("Dolt server already running, skipping start")
		return nil
	}

	if err := os.MkdirAll(m.config.DataDir, 0755); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}

	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		return fmt.Errorf("dolt not found in PATH: %w", err)
	}

	configPath := filepath.Join(m.config.DataDir, "config.yaml")
	if err := writeDaemonDoltConfig(m.config, configPath); err != nil {
		m.logger("Warning: failed to write Dolt config.yaml: %v", err)
	}

	return launchDoltProcess(m, doltPath, configPath)
}

func launchDoltProcess(m *DoltServerManager, doltPath, configPath string) error {
	logFile, err := openDoltLog(m.config.LogFile)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}

	cmd := newDoltServerCommand(doltPath, m.config.DataDir, configPath, logFile)
	if err := cmd.Start(); err != nil {
		closeDoltLog(m, logFile)
		return fmt.Errorf("starting dolt sql-server: %w", err)
	}

	go waitForDoltServer(cmd, logFile, m.logger)
	recordDoltProcessStart(m, cmd)
	verifyStartedDoltProcess(m)
	return nil
}

func closeDoltLog(m *DoltServerManager, logFile *os.File) {
	if closeErr := logFile.Close(); closeErr != nil {
		m.logger("Warning: failed to close dolt log file: %v", closeErr)
	}
}

func recordDoltProcessStart(m *DoltServerManager, cmd *exec.Cmd) {
	m.process = cmd.Process
	m.startedAt = time.Now()

	if _, err := writePIDFile(m.pidFile(), cmd.Process.Pid); err != nil {
		m.logger("Warning: failed to write PID file: %v", err)
	}

	m.logger("Started Dolt SQL server (PID %d) on %s:%d", cmd.Process.Pid, m.config.Host, m.config.Port)
}

func verifyStartedDoltProcess(m *DoltServerManager) {
	time.Sleep(500 * time.Millisecond)
	if err := m.checkHealthLocked(); err != nil {
		m.logger("Warning: Dolt server may not be healthy: %v", err)
	}
}
