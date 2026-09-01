package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var doltRebaseCmd = &cobra.Command{
	Use:   "rebase <database>",
	Short: "Surgical compaction: squash old commits, keep recent ones",
	Long: `Surgically compact a Dolt database using interactive rebase.

Unlike 'gt dolt flatten' (which destroys ALL history), surgical rebase
keeps recent commits individual while squashing old history into one.

Algorithm (based on Dolt's DOLT_REBASE):
  1. Creates anchor branch at root commit
  2. Creates work branch from main
  3. Starts interactive rebase — populates dolt_rebase table
  4. Marks old commits as 'squash', keeps recent N as 'pick'
  5. Executes the rebase plan
  6. Swaps branches: work becomes the new main
  7. Cleans up temporary branches
  8. Runs GC to reclaim space

WARNING: DOLT_REBASE is NOT safe with concurrent writes. If agents are
actively committing to this database, the rebase may fail with a graph-change
error. The Compactor Dog (daemon) has automatic retry logic for this case.
For manual use, re-run the command if it fails due to concurrent writes.
Flatten mode (gt dolt flatten) is safe with concurrent writes.

Use --keep-recent to control how many recent commits to preserve.
Use --dry-run to see the plan without executing it.

Requires --yes-i-am-sure flag as safety interlock.`,
	Args: cobra.ExactArgs(1),
	RunE: runDoltRebase,
}

func init() {
	doltRebaseCmd.Flags().Bool("yes-i-am-sure", false,
		"Required safety flag to confirm compaction")
	doltRebaseCmd.Flags().Int("keep-recent", 50,
		"Number of recent commits to keep as individual picks")
	doltRebaseCmd.Flags().Bool("dry-run", false,
		"Show the rebase plan without executing it")
	doltCmd.AddCommand(doltRebaseCmd)
}

type doltRebaseRequest struct {
	dbName     string
	keepRecent int
	dryRun     bool
}

func runDoltRebase(cmd *cobra.Command, args []string) error {
	req, err := parseDoltRebaseRequest(cmd, args)
	if err != nil {
		return err
	}
	db, ctx, cancel, err := connectDoltRebaseDB(req.dbName)
	if err != nil {
		return err
	}
	defer cancel()
	defer db.Close()
	return runDoltRebaseOnDB(ctx, db, req)
}

func parseDoltRebaseRequest(cmd *cobra.Command, args []string) (*doltRebaseRequest, error) {
	confirm := commandBoolFlag(cmd, "yes-i-am-sure")
	keepRecent := commandIntFlag(cmd, "keep-recent")
	dryRun := commandBoolFlag(cmd, "dry-run")
	if !confirm && !dryRun {
		return nil, fmt.Errorf("this command rewrites commit history. Pass --yes-i-am-sure to proceed (or --dry-run to preview)")
	}
	if keepRecent < 0 {
		return nil, fmt.Errorf("--keep-recent must be non-negative (got %d)", keepRecent)
	}
	return &doltRebaseRequest{dbName: args[0], keepRecent: keepRecent, dryRun: dryRun}, nil
}

func connectDoltRebaseDB(dbName string) (*sql.DB, context.Context, context.CancelFunc, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	running, _, err := doltserver.IsRunning(townRoot)
	if err != nil || !running {
		return nil, nil, nil, fmt.Errorf("Dolt server is not running — start with 'gt dolt start'")
	}
	config := doltserver.DefaultConfig(townRoot)
	// wa-d6f: socket-first DSN (TCP fallback) — eliminates TIME_WAIT churn.
	dsn := buildDoltDSNFromConfig(config, dbName, dsnOpts{
		ParseTime:    true,
		Timeout:      "5s",
		ReadTimeout:  "60s",
		WriteTimeout: "300s",
	})
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connecting to database %s: %w", dbName, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	var dummy int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&dummy); err != nil {
		cancel()
		db.Close()
		return nil, nil, nil, fmt.Errorf("database %q not reachable: %w", dbName, err)
	}
	return db, ctx, cancel, nil
}

type doltRebasePreflight struct {
	commitCount int
	counts      map[string]int
	head        string
	rootHash    string
}

const doltRebaseBaseBranch = "compact-base"
const doltRebaseWorkBranch = "compact-work"

func runDoltRebaseOnDB(ctx context.Context, db *sql.DB, req *doltRebaseRequest) error {
	fmt.Printf("%s Pre-flight checks for %s (surgical rebase)\n", style.Bold.Render("●"), style.Bold.Render(req.dbName))
	pre, err := preflightDoltRebase(ctx, db, req)
	if err != nil || pre == nil {
		return err
	}
	fmt.Printf("\n%s Starting surgical rebase...\n", style.Bold.Render("●"))
	if err := createDoltRebaseBranches(ctx, db, pre.rootHash); err != nil {
		return err
	}
	plan, err := inspectDoltRebasePlan(ctx, db, req.keepRecent)
	if err != nil || plan == nil {
		return err
	}
	if req.dryRun {
		return printDoltRebaseDryRun(ctx, db, req, plan)
	}
	if err := executeDoltRebaseSwap(ctx, db, req, pre, plan); err != nil {
		return err
	}
	verifyDoltRebase(ctx, db, req, pre)
	return nil
}

type doltRebasePlan struct {
	minOrder        int
	squashThreshold int
	toSquash        int
}

func createDoltRebaseBranches(ctx context.Context, db *sql.DB, rootHash string) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('%s', '%s')", doltRebaseBaseBranch, rootHash)); err != nil {
		return fmt.Errorf("create base branch at root: %w", err)
	}
	fmt.Printf("  Created %s at root %s\n", doltRebaseBaseBranch, rootHash[:12])
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('%s', 'main')", doltRebaseWorkBranch)); err != nil {
		rebaseCleanupBase(db, doltRebaseBaseBranch)
		return fmt.Errorf("create work branch from main: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_CHECKOUT('%s')", doltRebaseWorkBranch)); err != nil {
		rebaseCleanupAll(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return fmt.Errorf("checkout work branch: %w", err)
	}
	fmt.Printf("  Created %s from main, checked out\n", doltRebaseWorkBranch)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_REBASE('--interactive', '%s')", doltRebaseBaseBranch)); err != nil {
		rebaseCleanupAll(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return fmt.Errorf("start interactive rebase: %w", err)
	}
	fmt.Printf("  Interactive rebase started (dolt_rebase table populated)\n")
	return nil
}

func inspectDoltRebasePlan(ctx context.Context, db *sql.DB, keepRecent int) (*doltRebasePlan, error) {
	var totalPlan int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_rebase").Scan(&totalPlan); err != nil {
		rebaseAbortAndCleanup(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return nil, fmt.Errorf("counting rebase entries: %w", err)
	}
	fmt.Printf("  Rebase plan: %d commits\n", totalPlan)
	var minOrderStr, maxOrderStr string
	if err := db.QueryRowContext(ctx, "SELECT MIN(rebase_order), MAX(rebase_order) FROM dolt_rebase").Scan(&minOrderStr, &maxOrderStr); err != nil {
		rebaseAbortAndCleanup(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return nil, fmt.Errorf("getting rebase order range: %w", err)
	}
	minOrder, maxOrder, err := parseRebaseOrderRange(minOrderStr, maxOrderStr)
	if err != nil {
		rebaseAbortAndCleanup(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return nil, fmt.Errorf("parsing rebase order range: %w", err)
	}
	squashThreshold := maxOrder - keepRecent
	toSquash := 0
	if squashThreshold > minOrder {
		toSquash = squashThreshold - minOrder
	}
	fmt.Printf("  Squashing: %d old commits (keeping first as pick + last %d)\n", toSquash, keepRecent)
	if toSquash == 0 {
		fmt.Printf("  %s Nothing to squash — all commits are recent\n", style.Bold.Render("✓"))
		rebaseAbortAndCleanup(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return nil, nil
	}
	return &doltRebasePlan{minOrder: minOrder, squashThreshold: squashThreshold, toSquash: toSquash}, nil
}

func printDoltRebaseDryRun(ctx context.Context, db *sql.DB, req *doltRebaseRequest, plan *doltRebasePlan) error {
	fmt.Printf("\n%s Dry-run rebase plan:\n", style.Bold.Render("●"))
	rows, err := db.QueryContext(ctx, "SELECT rebase_order, action, commit_hash, commit_message FROM dolt_rebase ORDER BY rebase_order")
	if err != nil {
		rebaseAbortAndCleanup(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return fmt.Errorf("reading rebase plan: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		printDoltRebasePlanRow(rows, plan)
	}
	fmt.Printf("\n  Would squash %d commits, keep %d recent + 1 root pick\n", plan.toSquash, req.keepRecent)
	rebaseAbortAndCleanup(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
	return nil
}

func printDoltRebasePlanRow(rows *sql.Rows, plan *doltRebasePlan) {
	var orderStr, action, hash, msg string
	if err := rows.Scan(&orderStr, &action, &hash, &msg); err != nil {
		return
	}
	order, err := parseRebaseOrder(orderStr)
	if err != nil {
		return
	}
	marker := "pick"
	if order > plan.minOrder && order <= plan.squashThreshold {
		marker = "squash"
	}
	if len(msg) > 60 {
		msg = msg[:60] + "..."
	}
	if len(hash) > 8 {
		hash = hash[:8]
	}
	fmt.Printf("  %3d  %-7s  %s  %s\n", order, marker, hash, msg)
}

func executeDoltRebaseSwap(ctx context.Context, db *sql.DB, req *doltRebaseRequest, pre *doltRebasePreflight, plan *doltRebasePlan) error {
	result, err := db.ExecContext(ctx, fmt.Sprintf(
		"UPDATE dolt_rebase SET action = 'squash' WHERE rebase_order > %d AND rebase_order <= %d",
		plan.minOrder, plan.squashThreshold))
	if err != nil {
		rebaseAbortAndCleanup(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return fmt.Errorf("updating rebase plan: %w", err)
	}
	affected, _ := result.RowsAffected()
	fmt.Printf("  Marked %d commits as squash\n", affected)
	fmt.Printf("  Executing rebase (this may take a while)...\n")
	if _, err := db.ExecContext(ctx, "CALL DOLT_REBASE('--continue')"); err != nil {
		rebaseCleanupAll(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return fmt.Errorf("rebase execution failed (possible conflicts — automatic abort): %w", err)
	}
	fmt.Printf("  %s Rebase executed successfully\n", style.Bold.Render("✓"))
	if err := checkDoltRebaseConcurrency(db, req.dbName, pre.head); err != nil {
		return err
	}
	return swapDoltRebaseMain(ctx, db)
}

func checkDoltRebaseConcurrency(db *sql.DB, dbName, preHead string) error {
	currentHead, err := flattenGetHead(db, dbName)
	if err != nil {
		rebaseCleanupAll(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return fmt.Errorf("concurrency check: %w", err)
	}
	if currentHead != preHead {
		rebaseCleanupAll(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
		return fmt.Errorf("ABORT: main HEAD moved during rebase (%s → %s)", shortHash(preHead), shortHash(currentHead))
	}
	return nil
}

func swapDoltRebaseMain(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', 'main')"); err != nil {
		return fmt.Errorf("delete old main: %w (compact-work branch preserved for manual recovery)", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-m', '%s', 'main')", doltRebaseWorkBranch)); err != nil {
		return fmt.Errorf("rename work branch to main: %w", err)
	}
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", doltRebaseBaseBranch))
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')"); err != nil {
		return fmt.Errorf("checkout new main: %w", err)
	}
	fmt.Printf("  Branch swap complete — compact-work is now main\n")
	return nil
}

func verifyDoltRebase(ctx context.Context, db *sql.DB, req *doltRebaseRequest, pre *doltRebasePreflight) {
	postCounts, err := flattenGetRowCounts(db, req.dbName)
	if err != nil {
		fmt.Printf("  %s WARNING: could not verify row counts after rebase: %v\n", style.Bold.Render("!"), err)
		fmt.Printf("  Branch swap already complete — verify manually with 'gt dolt status'\n")
	} else {
		reportDoltRebaseIntegrity(pre.counts, postCounts)
	}
	var finalCount int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", req.dbName)).Scan(&finalCount); err == nil {
		fmt.Printf("  Final commit count: %d\n", finalCount)
	}
	fmt.Printf("\n%s Surgical rebase complete: %d → %d commits (kept %d recent)\n",
		style.Bold.Render("✓"), pre.commitCount, finalCount, req.keepRecent)
}

func reportDoltRebaseIntegrity(preCounts, postCounts map[string]int) {
	integrityOK := true
	for table, preCount := range preCounts {
		postCount, ok := postCounts[table]
		if !ok {
			fmt.Printf("  %s INTEGRITY WARNING: table %q missing after rebase (was %d rows)\n", style.Bold.Render("!"), table, preCount)
			integrityOK = false
			continue
		}
		if preCount != postCount {
			fmt.Printf("  %s INTEGRITY WARNING: %q row count changed: pre=%d post=%d\n", style.Bold.Render("!"), table, preCount, postCount)
			integrityOK = false
		}
	}
	if integrityOK {
		fmt.Printf("  %s Integrity verified (%d tables match)\n", style.Bold.Render("✓"), len(preCounts))
		return
	}
	fmt.Printf("  %s Some integrity checks failed — review above warnings\n", style.Bold.Render("!"))
}

func preflightDoltRebase(ctx context.Context, db *sql.DB, req *doltRebaseRequest) (*doltRebasePreflight, error) {
	var commitCount int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", req.dbName)).Scan(&commitCount); err != nil {
		return nil, fmt.Errorf("counting commits: %w", err)
	}
	fmt.Printf("  Commits: %d\n", commitCount)
	fmt.Printf("  Keep recent: %d\n", req.keepRecent)
	minCommits := req.keepRecent + 2
	if commitCount < minCommits {
		fmt.Printf("  %s Too few commits (%d) for surgical rebase with --keep-recent=%d (need at least %d)\n",
			style.Bold.Render("✓"), commitCount, req.keepRecent, minCommits)
		return nil, nil
	}
	preCounts, err := flattenGetRowCounts(db, req.dbName)
	if err != nil {
		return nil, fmt.Errorf("recording row counts: %w", err)
	}
	fmt.Printf("  Tables: %d\n", len(preCounts))
	for table, count := range preCounts {
		fmt.Printf("    %s: %d rows\n", table, count)
	}
	preHead, err := flattenGetHead(db, req.dbName)
	if err != nil {
		return nil, fmt.Errorf("getting HEAD: %w", err)
	}
	fmt.Printf("  HEAD: %s\n", preHead[:12])
	var rootHash string
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT commit_hash FROM `%s`.dolt_log ORDER BY date ASC LIMIT 1", req.dbName)).Scan(&rootHash); err != nil {
		return nil, fmt.Errorf("finding root commit: %w", err)
	}
	fmt.Printf("  Root: %s\n", rootHash[:12])
	if _, err := db.ExecContext(ctx, fmt.Sprintf("USE `%s`", req.dbName)); err != nil {
		return nil, fmt.Errorf("use database: %w", err)
	}
	rebaseCleanup(db, doltRebaseBaseBranch, doltRebaseWorkBranch)
	return &doltRebasePreflight{
		commitCount: commitCount,
		counts:      preCounts,
		head:        preHead,
		rootHash:    rootHash,
	}, nil
}

// rebaseCleanup removes leftover branches from a previous failed rebase.
func rebaseCleanup(db *sql.DB, baseBranch, workBranch string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Try to get back to main first.
	_, _ = db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')")
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", workBranch))
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
}

// rebaseAbortAndCleanup aborts an in-progress rebase then cleans up branches.
//
//nolint:unparam // baseBranch always "compact-base" — API kept flexible for future callers
func rebaseAbortAndCleanup(db *sql.DB, baseBranch, workBranch string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx, "CALL DOLT_REBASE('--abort')")
	_, _ = db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')")
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", workBranch))
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
}

// rebaseCleanupAll cleans up both branches after a failed rebase.
//
//nolint:unparam // baseBranch always "compact-base" — API kept flexible for future callers
func rebaseCleanupAll(db *sql.DB, baseBranch, workBranch string) {
	rebaseCleanup(db, baseBranch, workBranch)
}

// parseRebaseOrder converts a rebase_order value (returned by Dolt as DECIMAL
// string, e.g. "1.00") to an int.
func parseRebaseOrder(s string) (int, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid rebase_order %q: %w", s, err)
	}
	return int(math.Round(f)), nil
}

// parseRebaseOrderRange parses min/max rebase_order strings to ints.
func parseRebaseOrderRange(minStr, maxStr string) (int, int, error) {
	minVal, err := parseRebaseOrder(minStr)
	if err != nil {
		return 0, 0, err
	}
	maxVal, err := parseRebaseOrder(maxStr)
	if err != nil {
		return 0, 0, err
	}
	return minVal, maxVal, nil
}

// rebaseCleanupBase cleans up only the base branch (work branch not yet created).
func rebaseCleanupBase(db *sql.DB, baseBranch string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
}
