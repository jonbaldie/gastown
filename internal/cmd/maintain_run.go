package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// maintainDBInfo holds per-database info for the maintenance plan.
type maintainDBInfo struct {
	name        string
	commitCount int
	hasBackup   bool
}

type maintainRun struct {
	force     bool
	dryRun    bool
	threshold int
	townRoot  string
	config    *doltserver.Config
	dbs       []maintainDBInfo
	plan      maintainPlan
}

type maintainPlan struct {
	flatten int
	backup  int
}

type maintainTotals struct {
	reaped    int
	flattened int
	gc        int
}

func runMaintain(cmd *cobra.Command, _ []string) error {
	r, err := beginMaintain(cmd)
	if err != nil {
		return err
	}
	done, err := planMaintain(r)
	if done || err != nil {
		return err
	}
	if r.dryRun {
		fmt.Printf("\n%s Dry run complete — no changes made\n", style.Dim.Render("ℹ"))
		return nil
	}
	if !confirmMaintain(r) {
		return nil
	}
	return executeMaintain(r)
}

func beginMaintain(cmd *cobra.Command) (*maintainRun, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	config := doltserver.DefaultConfig(townRoot)
	if config.IsRemote() {
		return nil, fmt.Errorf("maintain requires local Dolt server (remote: %s)", config.HostPort())
	}
	running, _, err := doltserver.IsRunning(townRoot)
	if err != nil || !running {
		return nil, fmt.Errorf("Dolt server not running — start with 'gt dolt start'")
	}
	return &maintainRun{
		force:     commandBoolFlag(cmd, "force"),
		dryRun:    commandBoolFlag(cmd, "dry-run"),
		threshold: commandIntFlag(cmd, "threshold"),
		townRoot:  townRoot,
		config:    config,
	}, nil
}

func planMaintain(r *maintainRun) (bool, error) {
	fmt.Printf("%s Building maintenance plan...\n", style.Bold.Render("●"))
	databases, err := doltserver.ListDatabases(r.townRoot)
	if err != nil {
		return true, fmt.Errorf("listing databases: %w", err)
	}
	if len(databases) == 0 {
		fmt.Printf("%s No databases found — nothing to maintain\n", style.Dim.Render("○"))
		return true, nil
	}
	r.dbs = collectMaintainDBInfo(r.config, databases)
	r.plan = printMaintainPlan(r.dbs, r.threshold)
	return false, nil
}

func collectMaintainDBInfo(config *doltserver.Config, databases []string) []maintainDBInfo {
	dbInfos := make([]maintainDBInfo, 0, len(databases))
	for _, dbName := range databases {
		info := maintainDBInfo{name: dbName}
		if count, err := maintainCountCommits(config, dbName); err == nil {
			info.commitCount = count
		}
		info.hasBackup = maintainHasBackup(config.DataDir, dbName)
		dbInfos = append(dbInfos, info)
	}
	return dbInfos
}

func printMaintainPlan(dbInfos []maintainDBInfo, threshold int) maintainPlan {
	plan := maintainPlan{}
	fmt.Printf("\n%s Maintenance plan:\n", style.Bold.Render("●"))
	for _, db := range dbInfos {
		tags := ""
		if db.commitCount >= threshold {
			tags += fmt.Sprintf(" %s", style.Warning.Render("→ flatten"))
			plan.flatten++
		}
		if db.hasBackup {
			tags += fmt.Sprintf(" %s", style.Dim.Render("[backup]"))
			plan.backup++
		}
		fmt.Printf("  %s: %d commits%s\n", db.name, db.commitCount, tags)
	}
	fmt.Printf("\n  Databases: %d\n", len(dbInfos))
	fmt.Printf("  Will backup: %d\n", plan.backup)
	fmt.Printf("  Will flatten: %d (threshold: %d commits)\n", plan.flatten, threshold)
	fmt.Printf("  Will gc: %d\n", len(dbInfos))
	return plan
}

func confirmMaintain(r *maintainRun) bool {
	if r.force {
		return true
	}
	fmt.Printf("\nProceed? [y/N] ")
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "y" && answer != "yes" {
		fmt.Println("Aborted.")
		return false
	}
	return true
}

func executeMaintain(r *maintainRun) error {
	start := time.Now()
	backupMaintain(r)
	totals := maintainTotals{
		reaped:    reapMaintain(r),
		flattened: flattenMaintain(r),
		gc:        gcMaintain(r),
	}
	fmt.Printf("\n%s Maintenance complete (%v)\n", style.Success.Render("✓"), time.Since(start).Round(time.Second))
	fmt.Printf("  Wisps reaped: %d\n", totals.reaped)
	fmt.Printf("  Databases flattened: %d\n", totals.flattened)
	fmt.Printf("  Databases gc'd: %d\n", totals.gc)
	return nil
}

func backupMaintain(r *maintainRun) {
	if r.plan.backup == 0 {
		return
	}
	fmt.Printf("\n%s Backing up databases...\n", style.Bold.Render("●"))
	for _, db := range r.dbs {
		if !db.hasBackup {
			continue
		}
		backupName := db.name + "-backup"
		if err := maintainBackupSync(r.config.DataDir, db.name, backupName); err != nil {
			fmt.Printf("  %s %s: backup failed: %v\n", style.Warning.Render("!"), db.name, err)
			continue
		}
		fmt.Printf("  %s %s backed up\n", style.Bold.Render("✓"), db.name)
	}
}

func reapMaintain(r *maintainRun) int {
	fmt.Printf("\n%s Reaping closed wisps...\n", style.Bold.Render("●"))
	totalReaped := 0
	for _, db := range r.dbs {
		purged, err := doltserver.PurgeClosedEphemerals(r.townRoot, db.name, false)
		if err != nil {
			fmt.Printf("  %s %s: reap failed: %v\n", style.Warning.Render("!"), db.name, err)
			continue
		}
		if purged > 0 {
			fmt.Printf("  %s %s: reaped %d wisps\n", style.Bold.Render("✓"), db.name, purged)
			totalReaped += purged
			continue
		}
		fmt.Printf("  %s %s: nothing to reap\n", style.Dim.Render("○"), db.name)
	}
	return totalReaped
}

func flattenMaintain(r *maintainRun) int {
	if r.plan.flatten == 0 {
		return 0
	}
	fmt.Printf("\n%s Flattening databases...\n", style.Bold.Render("●"))
	totalFlattened := 0
	for _, db := range r.dbs {
		if db.commitCount < r.threshold {
			continue
		}
		if err := maintainFlattenDB(r.config, db.name); err != nil {
			fmt.Printf("  %s %s: flatten failed: %v\n", style.Bold.Render("✗"), db.name, err)
			continue
		}
		postCount, _ := maintainCountCommits(r.config, db.name)
		fmt.Printf("  %s %s: %d → %d commits\n", style.Bold.Render("✓"), db.name, db.commitCount, postCount)
		totalFlattened++
	}
	return totalFlattened
}

func gcMaintain(r *maintainRun) int {
	fmt.Printf("\n%s Running GC (via SQL on running server)...\n", style.Bold.Render("●"))
	gcCount := 0
	for _, db := range r.dbs {
		gcStart := time.Now()
		if err := maintainGCDatabase(r.config, db.name); err != nil {
			fmt.Printf("  %s %s: gc failed: %v\n", style.Warning.Render("!"), db.name, err)
			continue
		}
		fmt.Printf("  %s %s: gc completed (%v)\n",
			style.Bold.Render("✓"), db.name, time.Since(gcStart).Round(time.Millisecond))
		gcCount++
	}
	return gcCount
}
