package daemon

import (
	"fmt"
	"path/filepath"
	"time"
)

// Status returns the current status of the Dolt server.
func (m *DoltServerManager) Status() *DoltServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	status := &DoltServerStatus{
		Port: m.config.Port,
		Host: m.config.Host,
	}

	// Check if process is running
	pid, running := m.isRunning()
	status.Running = running
	status.PID = pid

	if running {
		status.StartedAt = doltStartedAt(m)

		// Get version
		if version, err := m.getDoltVersion(); err == nil {
			status.Version = version
		}

		// List databases
		if databases, err := m.listDatabases(); err == nil {
			status.Databases = databases
		}
	}

	return status
}

// pidFile returns the path to the Dolt server PID file.
// Production (port 3307) uses the canonical "dolt.pid" for compatibility with
// gt dolt start/stop. Other ports get a port-specific name to avoid collisions.
func (m *DoltServerManager) pidFile() string {
	if m.config.Port == 3307 {
		return filepath.Join(m.townRoot, "daemon", "dolt.pid")
	}
	return filepath.Join(m.townRoot, "daemon", fmt.Sprintf("dolt-%d.pid", m.config.Port))
}

// HealthCheckInterval returns the configured health check interval,
// falling back to DefaultDoltHealthCheckInterval if not explicitly set.
func (m *DoltServerManager) HealthCheckInterval() time.Duration {
	if m.config != nil && m.config.HealthCheckInterval > 0 {
		return m.config.HealthCheckInterval
	}
	return DefaultDoltHealthCheckInterval
}

// SetRecoveryCallback registers fn to be called (in a goroutine) whenever Dolt
// transitions from unhealthy back to healthy. Only the most recently registered
// callback is used. Pass nil to clear the callback.
func (m *DoltServerManager) SetRecoveryCallback(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRecoveryFn = fn
}
