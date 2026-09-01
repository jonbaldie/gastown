package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// checkHealth checks if the Dolt server is healthy (can accept connections).
func (m *DoltServerManager) checkHealth() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkHealthLocked()
}

// LastWarnings returns warnings from the most recent health check.
// Used by the Daemon for Option B throttling: only pour a mol-dog-doctor
// molecule when anomalies are detected.
func (m *DoltServerManager) LastWarnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastWarnings
}

// checkHealthLocked checks health. Must be called with m.mu held.
// Performs a connectivity check (SELECT active_branch()) with latency measurement, and logs
// warnings for degraded resource conditions (high latency, high connection count,
// disk usage). Returns an error only if the server is unreachable.
// Warnings are collected in m.lastWarnings for Option B throttling: the daemon
// pours a mol-dog-doctor molecule only when anomalies are detected.
func runDoltHealthCheck(m *DoltServerManager) error {
	m.lastWarnings = nil // Reset warnings each check cycle.

	// 1. Connectivity + latency: time a SELECT active_branch()
	// Per Tim Sehn (Dolt CEO): active_branch() is a lightweight probe that
	// won't block behind queued queries, unlike SELECT 1 which goes through the
	// full query executor.
	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()

	start := time.Now()
	cmd := m.buildDoltSQLCmd(ctx, "-q", "SELECT active_branch()")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("health check failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	latency := time.Since(start)
	if latency > 1*time.Second {
		w := fmt.Sprintf("Dolt health check latency %v exceeds 1s threshold — server may be under stress", latency.Round(time.Millisecond))
		m.lastWarnings = append(m.lastWarnings, w)
		m.logger("Warning: %s", w)
	}

	// 2. Connection count (best-effort, non-fatal)
	appendDoltHealthWarning(m, m.checkConnectionCount())

	// 3. Disk space (best-effort, non-fatal)
	appendDoltHealthWarning(m, m.checkDiskUsage())

	// 4. Database count (best-effort, non-fatal) — orphan detection
	appendDoltHealthWarning(m, m.checkDatabaseCount())

	// 5. Backup freshness (best-effort, non-fatal)
	for _, w := range m.checkBackupFreshness() {
		appendDoltHealthWarning(m, w)
	}

	return nil
}

func appendDoltHealthWarning(m *DoltServerManager, warning string) {
	if warning == "" {
		return
	}
	m.lastWarnings = append(m.lastWarnings, warning)
	m.logger("Warning: %s", warning)
}

func runDoltWriteHealthCheck(m *DoltServerManager) error {
	// Get a database to test writes against
	databases, err := getDoltDatabases(m)
	if err != nil || len(databases) == 0 {
		return nil // Skip write probe if no databases available
	}

	db := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()

	// Attempt a write operation to detect read-only mode.
	// CREATE TABLE IF NOT EXISTS is idempotent (safe if table lingers from previous probe).
	// REPLACE INTO always writes a row, testing the storage layer even if the table existed.
	// DROP TABLE IF EXISTS cleans up.
	// If ANY statement triggers "database is read only", the command fails and we detect it.
	query := fmt.Sprintf(
		"USE `%s`; CREATE TABLE IF NOT EXISTS `__gt_health_probe` (v INT PRIMARY KEY); REPLACE INTO `__gt_health_probe` VALUES (1); DROP TABLE IF EXISTS `__gt_health_probe`",
		db,
	)
	cmd := m.buildDoltSQLCmd(ctx, "-q", query)

	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	errMsg := strings.TrimSpace(string(output))
	if isReadOnlyError(errMsg) {
		return fmt.Errorf("dolt server is in read-only mode: %s", errMsg)
	}
	// Non-read-only failures: log warning but don't fail health check.
	// These could be transient issues (timeout, lock contention) that
	// don't indicate a persistent read-only state.
	m.logger("Warning: Dolt write probe failed (non-read-only): %v (%s)", err, errMsg)
	return nil
}

// checkConnectionCount queries the connection count and returns a warning if approaching the limit.
// Non-fatal: failures return empty string.
func (m *DoltServerManager) checkConnectionCount() string {
	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()
	cmd := m.buildDoltSQLCmd(ctx,
		"-r", "csv",
		"-q", "SELECT COUNT(*) AS cnt FROM information_schema.PROCESSLIST",
	)

	output, err := cmd.Output()
	if err != nil {
		return "" // non-fatal
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return ""
	}
	count, err := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil {
		return ""
	}

	// Use the doltserver package default (50) as a reasonable cap reference
	maxConn := 50
	threshold := (maxConn * 80) / 100
	if count >= threshold {
		return fmt.Sprintf("Dolt connection count %d is at %d%% of max %d — approaching limit",
			count, (count*100)/maxConn, maxConn)
	}
	return ""
}

// checkDiskUsage checks disk usage of the data directory and returns a warning
// if it exceeds 1 GB. Non-fatal: failures return empty string.
func (m *DoltServerManager) checkDiskUsage() string {
	dataDir := m.config.DataDir
	if dataDir == "" {
		return ""
	}

	total := doltDirectorySize(dataDir)
	const gb = 1024 * 1024 * 1024
	if total > gb {
		return fmt.Sprintf("Dolt data directory %s is %.1f GB", dataDir, float64(total)/float64(gb))
	}
	return ""
}

// checkDatabaseCount queries the database list and returns a warning if the count exceeds
// what's expected based on the data directory contents. Non-fatal: failures return empty string.
// The expected count is derived from subdirectories in the data dir (each is a registered DB).
func (m *DoltServerManager) checkDatabaseCount() string {
	databases, err := getDoltDatabases(m)
	if err != nil {
		return "" // non-fatal
	}

	// Derive expected count from data directory — each subdirectory is a database.
	// This adapts automatically as users add/remove rigs.
	expected := m.countDataDirDatabases()
	if expected == 0 {
		expected = 6 // Fallback if data dir can't be read
	}

	// Allow a small buffer (3) above expected for transient states.
	threshold := expected + 3
	if len(databases) > threshold {
		return fmt.Sprintf("%d databases detected (expected ~%d, threshold %d) — possible orphan/test database accumulation: %v",
			len(databases), expected, threshold, databases)
	}
	return ""
}

// countDataDirDatabases counts subdirectories in the Dolt data directory.
// Each subdirectory corresponds to a registered database.
func (m *DoltServerManager) countDataDirDatabases() int {
	dataDir := m.config.DataDir
	if dataDir == "" {
		return 0
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			count++
		}
	}
	return count
}

// checkBackupFreshness checks if Dolt backups are fresh. Returns warnings for any configured
// backup database that hasn't been synced in over 2 hours. Non-fatal: failures return nil.
func (m *DoltServerManager) checkBackupFreshness() []string {
	backupDir := filepath.Join(m.townRoot, ".dolt-backup")
	info, err := os.Stat(backupDir)
	if err != nil || !info.IsDir() {
		return nil // No backup directory — backup patrol may not be configured
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return nil
	}

	const staleThreshold = 2 * time.Hour
	now := time.Now()
	var warnings []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dbInfo, err := entry.Info()
		if err != nil {
			continue
		}
		age := now.Sub(dbInfo.ModTime())
		if age > staleThreshold {
			warnings = append(warnings, fmt.Sprintf("Dolt backup %q is %.0f minutes old (threshold %.0fm) — backup patrol may be stalled",
				entry.Name(), age.Minutes(), staleThreshold.Minutes()))
		}
	}
	return warnings
}
