package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var doltFlattenCmd = &cobra.Command{
	Use:   "flatten <database>",
	Short: "Flatten database history to a single commit (NUCLEAR OPTION)",
	Long: `Flatten a Dolt database's commit history to a single commit.

This is the NUCLEAR OPTION for compaction. It destroys all history.
Use only when automated compaction is insufficient.

All operations run via SQL on the running server — no downtime needed.

Safety protocol:
  1. Pre-flight: verifies backup freshness and records row counts
  2. Soft-resets to root commit on main (keeps all data staged)
  3. Commits all data as single commit
  4. Verifies row counts match (integrity check)

Requires --yes-i-am-sure flag as safety interlock.`,
	Args: cobra.ExactArgs(1),
	RunE: runDoltFlatten,
}

func init() {
	doltFlattenCmd.Flags().Bool("yes-i-am-sure", false,
		"Required safety flag to confirm you want to destroy history")
	doltCmd.AddCommand(doltFlattenCmd)
}

func runDoltFlatten(cmd *cobra.Command, args []string) error {
	dbName := args[0]

	if !commandBoolFlag(cmd, "yes-i-am-sure") {
		return fmt.Errorf("this command destroys all commit history. Pass --yes-i-am-sure to proceed")
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return runDoltFlattenDatabase(townRoot, dbName)
}

func runDoltFlattenDatabase(townRoot, dbName string) error {
	db, err := openDoltFlattenDB(townRoot, dbName)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var dummy int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&dummy); err != nil {
		return fmt.Errorf("database %q not reachable: %w", dbName, err)
	}

	plan, err := prepareDoltFlatten(ctx, db, townRoot, dbName)
	if err != nil {
		return err
	}
	if plan == nil {
		return nil
	}
	if err := executeDoltFlatten(ctx, db, dbName, plan.rootHash); err != nil {
		return err
	}
	if err := verifyDoltFlatten(db, dbName, plan.preCounts); err != nil {
		return err
	}
	finalCount := doltFlattenFinalCommitCount(ctx, db, dbName)
	fmt.Printf("\n%s Flatten complete: %d → %d commits\n",
		style.Bold.Render("✓"), plan.commitCount, finalCount)
	return nil
}

type doltFlattenPlan struct {
	commitCount int
	preCounts   map[string]int
	rootHash    string
}

func openDoltFlattenDB(townRoot, dbName string) (*sql.DB, error) {
	running, _, err := doltserver.IsRunning(townRoot)
	if err != nil || !running {
		return nil, fmt.Errorf("Dolt server is not running — start with 'gt dolt start'")
	}

	config := doltserver.DefaultConfig(townRoot)
	// wa-d6f: socket-first DSN (TCP fallback) — eliminates TIME_WAIT churn.
	dsn := buildDoltDSNFromConfig(config, dbName, dsnOpts{
		ParseTime:    true,
		Timeout:      "5s",
		ReadTimeout:  "30s",
		WriteTimeout: "30s",
	})
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to database %s: %w", dbName, err)
	}
	return db, nil
}

func prepareDoltFlatten(ctx context.Context, db *sql.DB, townRoot, dbName string) (*doltFlattenPlan, error) {
	fmt.Printf("%s Pre-flight checks for %s\n", style.Bold.Render("●"), style.Bold.Render(dbName))
	printDoltFlattenBackupStatus(townRoot)

	var commitCount int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", dbName)).Scan(&commitCount); err != nil {
		return nil, fmt.Errorf("counting commits: %w", err)
	}
	fmt.Printf("  Commits: %d\n", commitCount)
	if commitCount <= 2 {
		fmt.Printf("  %s Already minimal — nothing to flatten\n", style.Bold.Render("✓"))
		return nil, nil
	}

	preCounts, err := flattenGetRowCounts(db, dbName)
	if err != nil {
		return nil, fmt.Errorf("recording row counts: %w", err)
	}
	fmt.Printf("  Tables: %d\n", len(preCounts))
	for table, count := range preCounts {
		fmt.Printf("    %s: %d rows\n", table, count)
	}

	var rootHash string
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT commit_hash FROM `%s`.dolt_log ORDER BY date ASC LIMIT 1", dbName)).Scan(&rootHash); err != nil {
		return nil, fmt.Errorf("finding root commit: %w", err)
	}
	fmt.Printf("  Root: %s\n", rootHash[:12])
	return &doltFlattenPlan{commitCount: commitCount, preCounts: preCounts, rootHash: rootHash}, nil
}

func printDoltFlattenBackupStatus(townRoot string) {
	backupDir := filepath.Join(townRoot, ".dolt-backup")
	if _, err := os.Stat(backupDir); err != nil {
		fmt.Printf("  %s No backup directory found — proceed with caution\n", style.Bold.Render("!"))
		return
	}
	newest := findNewestFile(backupDir)
	if newest.IsZero() {
		return
	}
	age := time.Since(newest)
	if age > 30*time.Minute {
		fmt.Printf("  %s Backup is %v old — consider running backup first\n",
			style.Bold.Render("!"), age.Round(time.Minute))
		return
	}
	fmt.Printf("  %s Backup is %v old (OK)\n", style.Bold.Render("✓"), age.Round(time.Second))
}

func executeDoltFlatten(ctx context.Context, db *sql.DB, dbName, rootHash string) error {
	fmt.Printf("\n%s Flattening %s (direct SQL — no downtime)...\n", style.Bold.Render("●"), dbName)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("USE `%s`", dbName)); err != nil {
		return fmt.Errorf("use database: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_RESET('--soft', '%s')", rootHash)); err != nil {
		return fmt.Errorf("soft reset: %w", err)
	}
	fmt.Printf("  Soft-reset to root %s\n", rootHash[:12])
	commitMsg := fmt.Sprintf("flatten: compact %s history to single commit", dbName)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	fmt.Printf("  Committed flattened data\n")
	return nil
}

func verifyDoltFlatten(db *sql.DB, dbName string, preCounts map[string]int) error {
	postCounts, err := flattenGetRowCounts(db, dbName)
	if err != nil {
		return fmt.Errorf("post-compact row counts: %w", err)
	}
	for table, preCount := range preCounts {
		postCount, ok := postCounts[table]
		if !ok {
			return fmt.Errorf("integrity FAIL: table %q missing after flatten", table)
		}
		if postCount < preCount {
			return fmt.Errorf("integrity FAIL: %q lost rows: pre=%d post=%d", table, preCount, postCount)
		}
		if postCount > preCount {
			fmt.Printf("  %s table %q gained %d rows during flatten (concurrent write, safe)\n",
				style.Bold.Render("⚠"), table, postCount-preCount)
		}
	}
	fmt.Printf("  %s Integrity verified (%d tables match)\n", style.Bold.Render("✓"), len(preCounts))
	return nil
}

func doltFlattenFinalCommitCount(ctx context.Context, db *sql.DB, dbName string) int {
	var finalCount int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", dbName)).Scan(&finalCount); err == nil {
		fmt.Printf("  Final commit count: %d\n", finalCount)
	}
	return finalCount
}

// flattenGetRowCounts returns table -> row count for all user tables.
func flattenGetRowCounts(db *sql.DB, dbName string) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	query := fmt.Sprintf("SELECT table_name FROM information_schema.tables WHERE table_schema = '%s' AND table_name NOT LIKE 'dolt_%%'", dbName)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}

	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`.`%s`", dbName, table)).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = count
	}
	return counts, nil
}

// findNewestFile walks a directory and returns the most recent file mtime.
func findNewestFile(dir string) time.Time {
	var newest time.Time
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}

// flattenGetHead returns the HEAD commit hash via dolt_log.
func flattenGetHead(db *sql.DB, dbName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var hash string
	query := fmt.Sprintf("SELECT commit_hash FROM `%s`.dolt_log ORDER BY date DESC LIMIT 1", dbName)
	if err := db.QueryRowContext(ctx, query).Scan(&hash); err != nil {
		return "", err
	}
	return hash, nil
}
