package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// StaleSQLServerInfoCheck detects stale sql-server.info files left by crashed
// or stopped local Dolt servers. When running in dolt_mode=server, these files
// cause bd to connect to a dead local server instead of the central Dolt server,
// resulting in "database not found" errors. See GH#2770.
type StaleSQLServerInfoCheck struct {
	FixableCheck
	staleFiles []string
}

// NewStaleSQLServerInfoCheck creates a new stale sql-server.info check.
func NewStaleSQLServerInfoCheck() *StaleSQLServerInfoCheck {
	return &StaleSQLServerInfoCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "stale-sql-server-info",
				CheckDescription: "Detect stale Dolt sql-server.info files from dead local servers",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks for stale sql-server.info files across all beads directories.
func (c *StaleSQLServerInfoCheck) Run(ctx *CheckContext) *CheckResult {
	c.staleFiles = nil

	locations := sqlServerInfoLocations(ctx.TownRoot)
	details := c.findStaleSQLServerInfo(ctx.TownRoot, locations)
	return staleSQLServerInfoResult(c.Name(), c.staleFiles, details)
}

func sqlServerInfoLocations(townRoot string) []string {
	locations := []string{
		filepath.Join(townRoot, ".beads", "dolt", ".dolt", "sql-server.info"),
	}
	for rigName := range knownRigNames(townRoot) {
		locations = append(locations,
			filepath.Join(townRoot, rigName, ".beads", "dolt", ".dolt", "sql-server.info"),
		)
	}
	return locations
}

func knownRigNames(townRoot string) map[string]struct{} {
	rigNames := make(map[string]struct{})
	addRegisteredRigNames(townRoot, rigNames)
	addTopLevelRigNames(townRoot, rigNames)
	return rigNames
}

func addRegisteredRigNames(townRoot string, rigNames map[string]struct{}) {
	rigsConfig := filepath.Join(townRoot, "mayor", "rigs.json")
	data, err := os.ReadFile(rigsConfig)
	if err != nil {
		return
	}
	var rigs struct {
		Rigs map[string]struct{} `json:"rigs"`
	}
	if json.Unmarshal(data, &rigs) != nil {
		return
	}
	for name := range rigs.Rigs {
		rigNames[name] = struct{}{}
	}
}

func addTopLevelRigNames(townRoot string, rigNames map[string]struct{}) {
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			rigNames[entry.Name()] = struct{}{}
		}
	}
}

func (c *StaleSQLServerInfoCheck) findStaleSQLServerInfo(townRoot string, locations []string) []string {
	var details []string
	for _, path := range locations {
		if _, err := os.Stat(path); err != nil || !c.isStale(path) {
			continue
		}
		c.staleFiles = append(c.staleFiles, path)
		relPath, _ := filepath.Rel(townRoot, path)
		details = append(details, fmt.Sprintf("Stale sql-server.info: %s", relPath))
	}
	return details
}

func staleSQLServerInfoResult(name string, staleFiles, details []string) *CheckResult {
	if len(staleFiles) == 0 {
		return &CheckResult{
			Name:    name,
			Status:  StatusOK,
			Message: "No stale sql-server.info files found",
		}
	}

	return &CheckResult{
		Name:    name,
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d stale sql-server.info file(s) from dead Dolt servers", len(staleFiles)),
		Details: details,
		FixHint: "Restart the Dolt server to clear stale sql-server.info files (Dolt writes and cleans these itself)",
	}
}

// Fix is a no-op. sql-server.info is a Dolt-internal file written and managed
// by Dolt itself (see dolt/commands/sqlserver/creds.go). Restarting the Dolt
// server will create a fresh one.
//
// WARNING: DO NOT remove, delete, or modify files inside Dolt's .dolt/
// directory — including noms/LOCK files. These are Dolt-internal files.
// Removing them WILL cause unrecoverable data corruption and data loss.
// Dolt manages these files itself; external interference is never safe.
func (c *StaleSQLServerInfoCheck) Fix(_ *CheckContext) error {
	return nil
}

// isStale checks if the sql-server.info file references a dead process.
// The file format is "PID:port:UUID" (one line).
func (c *StaleSQLServerInfoCheck) isStale(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from filepath.Walk
	if err != nil {
		return false
	}

	content := strings.TrimSpace(string(data))
	if content == "" {
		return true // Empty file is stale
	}

	// Parse PID from "PID:port:UUID" format
	parts := strings.SplitN(content, ":", 3)
	if len(parts) < 1 {
		return true
	}

	pid, err := strconv.Atoi(parts[0])
	if err != nil || pid <= 0 {
		return true // Corrupt or invalid PID
	}

	// Check if the process is alive using signal 0 (no-op probe)
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}

	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return true // Process is dead
	}

	return false // Process is alive, not stale
}
