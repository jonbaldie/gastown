package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// unhealthySignalFile returns the path to the DOLT_UNHEALTHY signal file.
// Witness patrols can check for this file to detect degraded Dolt state.
// Production (port 3307) uses the canonical name; other ports get a suffix
// so multiple instances don't clobber each other's signal files.
func (m *DoltServerManager) unhealthySignalFile() string {
	if m.config.Port == 3307 {
		return filepath.Join(m.townRoot, "daemon", "DOLT_UNHEALTHY")
	}
	return filepath.Join(m.townRoot, "daemon", fmt.Sprintf("DOLT_UNHEALTHY_%d", m.config.Port))
}

// writeUnhealthySignal writes the DOLT_UNHEALTHY signal file.
// This file signals to witness patrols that the Dolt server is degraded.
// It returns true only for the first write in an active incident. Existing
// signal files are preserved so repeated health ticks do not reset the
// incident timestamp or re-trigger diagnostics.
func (m *DoltServerManager) writeUnhealthySignal(reason, detail string) bool {
	return writeDoltUnhealthySignal(m, reason, detail)
}

func writeDoltUnhealthySignal(m *DoltServerManager, reason, detail string) bool {
	signalFile := m.unhealthySignalFile()
	payload := fmt.Sprintf(`{"reason":%q,"detail":%q,"timestamp":%q}`,
		reason, detail, m.now().UTC().Format(time.RFC3339))
	f, err := os.OpenFile(signalFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if os.IsExist(err) {
		return false
	}
	if err != nil {
		m.logger("Warning: failed to write DOLT_UNHEALTHY signal: %v", err)
		return true
	}
	if _, err := f.WriteString(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(signalFile)
		m.logger("Warning: failed to write DOLT_UNHEALTHY signal: %v", err)
		return true
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(signalFile)
		m.logger("Warning: failed to write DOLT_UNHEALTHY signal: %v", err)
		return true
	}
	return true
}

// clearUnhealthySignal removes the DOLT_UNHEALTHY signal file when the server is healthy.
// If the signal file was present (meaning Dolt was previously unhealthy), it fires the
// onRecoveryFn callback in a goroutine to trigger a convoy recovery sweep.
// Must be called with mu held (onRecoveryFn is protected by mu).
func (m *DoltServerManager) clearUnhealthySignal() {
	signalFile := m.unhealthySignalFile()
	_, wasUnhealthy := os.Stat(signalFile)
	_ = os.Remove(signalFile)
	// Transition detected: was unhealthy, now healthy — fire recovery callback.
	if wasUnhealthy == nil {
		fn := doltRecoveryCallback(m)
		if fn == nil {
			return
		}
		go fn()
	}
}
