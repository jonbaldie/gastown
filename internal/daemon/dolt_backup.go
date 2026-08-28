package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/util"
)

const (
	defaultDoltBackupInterval = 15 * time.Minute
	// doltBackupTimeout is generous so a large commit delta on a big database
	// (e.g. hq under a wisp flood) does not blow the deadline mid-sync. The old
	// 120s ceiling produced spurious exit-1 backup failures (gt-ye21).
	doltBackupTimeout = 5 * time.Minute
	// doltBackupRetries / doltBackupRetryDelay retry a failed sync after a short
	// pause, so a transient lock (a concurrent dolt op holding the db) does not
	// fail the whole backup cycle.
	doltBackupRetries    = 1
	doltBackupRetryDelay = 5 * time.Second
)

// doltBackupInterval returns the configured backup interval, or the default (15m).
func doltBackupInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.DoltBackup != nil {
		if config.Patrols.DoltBackup.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.DoltBackup.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultDoltBackupInterval
}

// syncDoltBackups syncs each production database to its configured backup location.
// Non-fatal: errors are logged but don't stop the daemon.
func (d *Daemon) syncDoltBackups() {
	// Dolt backup uses iCloud Drive for offsite sync — only available on macOS.
	// On Linux this generates HIGH priority escalation spam every ~15 minutes.
	if !d.canSyncDoltBackups() {
		return
	}

	// Pour molecule for observability (nil-safe — all methods are no-ops on nil).
	mol := d.pourDogMolecule(constants.MolDogBackup, nil)
	defer mol.Close()

	dataDir := d.doltBackupDataDir()
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		d.logger.Printf("dolt_backup: data dir %s does not exist, skipping", dataDir)
		mol.FailStep("sync", "data dir does not exist")
		return
	}

	databases := d.doltBackupDatabases(dataDir)

	if len(databases) == 0 {
		d.logger.Printf("dolt_backup: no databases with backup remotes found")
		mol.FailStep("sync", "no databases with backup remotes")
		return
	}

	synced, failures := d.syncDoltBackupDatabases(dataDir, databases)

	d.logger.Printf("dolt_backup: synced %d/%d database(s)", synced, len(databases))
	d.recordDoltBackupResult(mol, synced, len(databases), failures)

	// Offsite sync: rsync local backups to iCloud Drive for cloud replication.
	// This is a stopgap until proper dolt remote push is configured.
	if synced > 0 {
		d.syncOffsiteBackup()
	}
	mol.CloseStep("offsite")
	mol.CloseStep("report")
}

func (d *Daemon) canSyncDoltBackups() bool {
	return runtime.GOOS == "darwin" && d.isPatrolActive("dolt_backup")
}

func (d *Daemon) doltBackupDataDir() string {
	if d.doltServer != nil && d.doltServer.IsEnabled() && d.doltServer.config.DataDir != "" {
		return d.doltServer.config.DataDir
	}
	return filepath.Join(d.config.TownRoot, ".dolt-data")
}

func (d *Daemon) doltBackupDatabases(dataDir string) []string {
	databases := d.patrolConfig.Patrols.DoltBackup.Databases
	if len(databases) > 0 {
		return databases
	}
	return d.discoverDatabasesWithBackups(dataDir)
}

func (d *Daemon) syncDoltBackupDatabases(dataDir string, databases []string) (int, []string) {
	d.logger.Printf("dolt_backup: syncing %d database(s)", len(databases))

	synced := 0
	var failures []string
	for _, db := range databases {
		if err := d.syncBackup(dataDir, db, db+"-backup"); err != nil {
			d.logger.Printf("dolt_backup: %s: sync failed: %v", db, err)
			failures = append(failures, db)
			continue
		}
		synced++
	}
	return synced, failures
}

func (d *Daemon) recordDoltBackupResult(mol *dogMol, synced, total int, failures []string) {
	if len(failures) == 0 {
		mol.CloseStep("sync")
		return
	}
	mol.FailStep("sync", fmt.Sprintf("synced %d/%d, failures: %s", synced, total, strings.Join(failures, "; ")))
}

// syncBackup runs `dolt backup sync <backup-name>` for a single database,
// retrying once on failure so a transient lock or large delta does not fail the
// cycle (gt-ye21).
func (d *Daemon) syncBackup(dataDir, db, backupName string) error {
	parentCtx := d.ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	dbDir := filepath.Join(dataDir, db)

	var lastErr error
	for attempt := 0; attempt <= doltBackupRetries; attempt++ {
		if attempt > 0 {
			d.logger.Printf("dolt_backup: %s: retry %d/%d after error: %v", db, attempt, doltBackupRetries, lastErr)
			timer := time.NewTimer(doltBackupRetryDelay)
			select {
			case <-timer.C:
			case <-parentCtx.Done():
				timer.Stop()
				return parentCtx.Err()
			}
		}

		ctx, cancel := context.WithTimeout(parentCtx, doltBackupTimeout)
		cmd := exec.CommandContext(ctx, "dolt", "backup", "sync", backupName)
		cmd.Dir = dbDir
		util.SetProcessGroup(cmd)

		output, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			d.logger.Printf("dolt_backup: %s: synced to %s", db, backupName)
			return nil
		}
		lastErr = fmt.Errorf("%s: %s", err, strings.TrimSpace(string(output)))
	}
	return lastErr
}

// syncOffsiteBackup rsyncs the local backup directory to iCloud Drive.
// iCloud automatically syncs to Apple's cloud, providing offsite replication.
// Non-fatal: if iCloud is unavailable or rsync fails, we just log and continue.
func (d *Daemon) syncOffsiteBackup() {
	backupDir := filepath.Join(d.config.TownRoot, ".dolt-backup")
	if _, err := os.Stat(backupDir); os.IsNotExist(err) {
		return
	}

	// iCloud Drive path (macOS)
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	icloudDir := filepath.Join(homeDir, "Library", "Mobile Documents", "com~apple~CloudDocs", "gt-dolt-backup")
	if err := os.MkdirAll(icloudDir, 0755); err != nil {
		d.logger.Printf("dolt_backup: offsite: cannot create iCloud dir: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rsync", "-a", "--delete", backupDir+"/", icloudDir+"/")
	util.SetDetachedProcessGroup(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("dolt_backup: offsite sync failed: %v (%s)", err, strings.TrimSpace(string(output)))
	} else {
		d.logger.Printf("dolt_backup: offsite synced to iCloud")
	}
}

// discoverDatabasesWithBackups lists databases in the data directory
// that have a <name>-backup backup remote configured.
func (d *Daemon) discoverDatabasesWithBackups(dataDir string) []string {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		d.logger.Printf("dolt_backup: error reading data dir: %v", err)
		return nil
	}

	var databases []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		// Check if this directory has a <name>-backup configured
		backupName := name + "-backup"
		if d.hasBackupRemote(dataDir, name, backupName) {
			databases = append(databases, name)
		}
	}

	return databases
}

// hasBackupRemote checks if a database has the specified backup remote configured.
func (d *Daemon) hasBackupRemote(dataDir, db, backupName string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbDir := dataDir + "/" + db
	cmd := exec.CommandContext(ctx, "dolt", "backup")
	cmd.Dir = dbDir
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.Output()
	if err != nil {
		return false
	}

	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == backupName {
			return true
		}
	}
	return false
}
