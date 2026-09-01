package daemon

import "time"

// getBackoffDelay returns the current backoff delay.
func (m *DoltServerManager) getBackoffDelay() time.Duration {
	if m.currentDelay <= 0 {
		return m.config.RestartDelay
	}
	return m.currentDelay
}
