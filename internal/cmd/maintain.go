package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/spf13/cobra"
)

const (
	// defaultMaintainThreshold is the minimum commit count before flatten triggers.
	defaultMaintainThreshold = 100
	// maintainGCTimeout is the timeout for CALL dolt_gc() on a single database.
	maintainGCTimeout = 5 * time.Minute
	// maintainBackupTimeout is the timeout for dolt backup sync on a single database.
	maintainBackupTimeout = 2 * time.Minute
	// maintainQueryTimeout is the timeout for individual SQL queries during flatten.
	maintainQueryTimeout = 30 * time.Second
)

var maintainCmd = &cobra.Command{
	Use:     "maintain",
	GroupID: GroupServices,
	Short:   "Run full Dolt maintenance (reap + flatten + gc)",
	Long: `Run the full Dolt maintenance pipeline in a single command.

All operations run via SQL on the running server — no downtime needed.

This encapsulates the maintenance procedure:
  1. Backup all databases (dolt backup sync)
  2. Reap closed wisps from each database
  3. Flatten databases over commit threshold
  4. Run dolt_gc() on each database

Use --force for non-interactive mode (daemon/cron), or run interactively
to review the plan before proceeding.

Examples:
  gt maintain                # Interactive (shows plan, asks confirmation)
  gt maintain --force        # Non-interactive (daemon/cron use)
  gt maintain --dry-run      # Preview what would happen
  gt maintain --threshold 50 # Custom commit threshold`,
	RunE: runMaintain,
}

func init() {
	maintainCmd.Flags().Bool("force", false, "Non-interactive mode (skip confirmation)")
	maintainCmd.Flags().Bool("dry-run", false, "Preview without making changes")
	maintainCmd.Flags().Int("threshold", defaultMaintainThreshold, "Commit count threshold for flatten")
	rootCmd.AddCommand(maintainCmd)
}

// maintainCountCommits returns the number of Dolt commits in a database.
func maintainCountCommits(config *doltserver.Config, dbName string) (int, error) {
	db, err := maintainOpenDB(config, dbName)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), maintainQueryTimeout)
	defer cancel()

	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", dbName)
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// maintainHasBackup checks if a database has a <name>-backup remote configured.
func maintainHasBackup(dataDir, dbName string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbDir := filepath.Join(dataDir, dbName)
	cmd := exec.CommandContext(ctx, "dolt", "backup")
	cmd.Dir = dbDir

	output, err := cmd.Output()
	if err != nil {
		return false
	}

	backupName := dbName + "-backup"
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == backupName {
			return true
		}
	}
	return false
}

// maintainBackupSync runs dolt backup sync for a single database.
func maintainBackupSync(dataDir, dbName, backupName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), maintainBackupTimeout)
	defer cancel()

	dbDir := filepath.Join(dataDir, dbName)
	cmd := exec.CommandContext(ctx, "dolt", "backup", "sync", backupName)
	cmd.Dir = dbDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// maintainOpenDB opens a connection to the Dolt server for a database.
func maintainOpenDB(config *doltserver.Config, dbName string) (*sql.DB, error) {
	// wa-d6f: socket-first DSN (TCP fallback) to avoid TIME_WAIT churn from
	// short-lived gt maintain invocations.
	dsn := buildDoltDSNFromConfig(config, dbName, dsnOpts{
		ParseTime:    true,
		Timeout:      "5s",
		ReadTimeout:  "30s",
		WriteTimeout: "30s",
	})
	return sql.Open("mysql", dsn)
}

// maintainFlattenDB flattens a database's commit history to a single commit.
// Uses direct SQL on the running server — no branches, no downtime.
// Per Tim Sehn (2026-02-28): DOLT_RESET --soft + DOLT_COMMIT is safe on a
// running server. Concurrent writes during flatten are safe.
func maintainFlattenDB(config *doltserver.Config, dbName string) error {
	db, err := maintainOpenDB(config, dbName)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), maintainQueryTimeout)
	defer cancel()

	preCounts, err := flattenPrepare(ctx, db, dbName)
	if err != nil {
		return err
	}
	if err := flattenResetAndCommit(ctx, db, dbName); err != nil {
		return err
	}
	return flattenVerifyIntegrity(db, dbName, preCounts)
}

func flattenPrepare(ctx context.Context, db *sql.DB, dbName string) (map[string]int, error) {
	var dummy int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&dummy); err != nil {
		return nil, fmt.Errorf("connection check: %w", err)
	}
	preCounts, err := flattenGetRowCounts(db, dbName)
	if err != nil {
		return nil, fmt.Errorf("pre-flight row counts: %w", err)
	}
	return preCounts, nil
}

func flattenResetAndCommit(ctx context.Context, db *sql.DB, dbName string) error {
	var rootHash string
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT commit_hash FROM `%s`.dolt_log ORDER BY date ASC LIMIT 1", dbName),
	).Scan(&rootHash); err != nil {
		return fmt.Errorf("find root commit: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("USE `%s`", dbName)); err != nil {
		return fmt.Errorf("use database: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_RESET('--soft', '%s')", rootHash)); err != nil {
		return fmt.Errorf("soft reset: %w", err)
	}
	commitMsg := fmt.Sprintf("maintain: flatten %s history", dbName)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func flattenVerifyIntegrity(db *sql.DB, dbName string, preCounts map[string]int) error {
	postCounts, err := flattenGetRowCounts(db, dbName)
	if err != nil {
		return fmt.Errorf("post-flatten row counts: %w", err)
	}
	for table, preCount := range preCounts {
		postCount, ok := postCounts[table]
		if !ok {
			return fmt.Errorf("integrity: table %q missing after flatten", table)
		}
		if preCount != postCount {
			return fmt.Errorf("integrity: %q pre=%d post=%d", table, preCount, postCount)
		}
	}
	return nil
}

// maintainGCDatabase runs dolt gc via SQL on the running server.
// Safe on a running server — no downtime needed (Tim Sehn, 2026-02-28).
func maintainGCDatabase(config *doltserver.Config, dbName string) error {
	db, err := maintainOpenDB(config, dbName)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), maintainGCTimeout)
	defer cancel()

	if _, err := db.ExecContext(ctx, "CALL dolt_gc()"); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("timeout after %v", maintainGCTimeout)
		}
		return fmt.Errorf("dolt_gc: %w", err)
	}
	return nil
}
