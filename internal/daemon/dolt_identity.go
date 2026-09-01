package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jonbaldie/gastown/internal/doltserver"
)

func verifyDoltServerIdentity(m *DoltServerManager) error {
	legitimate, err := doltserver.VerifyServerDataDir(m.townRoot)
	if err != nil {
		return fmt.Errorf("server identity verification failed: %w", err)
	}
	if !legitimate {
		return fmt.Errorf("server is an imposter (wrong data directory)")
	}

	expectedDBs, fsErr := doltserver.ListDatabases(m.townRoot)
	if fsErr != nil || len(expectedDBs) == 0 {
		return nil
	}
	return verifyDoltDatabaseContents(m, expectedDBs[0])
}

func verifyDoltDatabaseContents(m *DoltServerManager, db string) error {
	issueCount := doltTableRowCount(m, db, "issues")
	wispCount := doltTableRowCount(m, db, "wisps")
	if issueCount < 0 && wispCount < 0 {
		return nil
	}
	if issueCount > 0 || wispCount > 0 {
		return nil
	}
	return checkDoltDatabaseDiskData(m, db)
}

func doltTableRowCount(m *DoltServerManager, db, table string) int {
	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()
	query := fmt.Sprintf("SELECT COUNT(*) AS cnt FROM `%s`.`%s`", db, table)
	cmd := m.buildDoltSQLCmd(ctx, "-r", "csv", "-q", query)
	output, err := cmd.Output()
	if err != nil {
		return -1
	}
	return parseDoltRowCount(output)
}

func parseDoltRowCount(output []byte) int {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return -1
	}
	count, err := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil {
		return -1
	}
	return count
}

func checkDoltDatabaseDiskData(m *DoltServerManager, db string) error {
	dbDir := doltserver.RigDatabaseDir(m.townRoot, db)
	commitDir := filepath.Join(dbDir, ".dolt", "noms")
	info, err := os.Stat(commitDir)
	if err != nil || !info.IsDir() {
		return nil
	}

	totalSize := doltDirectorySize(dbDir)
	if totalSize <= 1024*1024 {
		return nil
	}
	return fmt.Errorf("database %q has %s on disk but 0 rows in server (issues+wisps) — possible imposter",
		db, formatDiskSize(totalSize))
}

func doltDirectorySize(root string) int64 {
	var totalSize int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			totalSize += info.Size()
		}
		return nil
	})
	return totalSize
}
