package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/util"
)

const (
	defaultJsonlGitBackupInterval = 15 * time.Minute
	jsonlExportTimeout            = 60 * time.Second
	gitPushTimeout                = 120 * time.Second
	gitCmdTimeout                 = 30 * time.Second
	maxConsecutivePushFailures    = 3
	defaultSpikeThreshold         = 0.50 // 50% delta triggers halt (was 20%, too sensitive for bulk ops)
)

// testPollutionPatterns matches issue IDs or titles that indicate test data leaked
// into production exports. These records are filtered out before writing JSONL.
var testPollutionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^Test Issue`),                      // title: "Test Issue ..."
	regexp.MustCompile(`(?i)^test[_\s]`),                       // title: "test_something" or "test something"
	regexp.MustCompile(`^bd-[0-9]{1,2}$`),                      // id: bd-1, bd-99 (suspiciously short IDs)
	regexp.MustCompile(`^bd-[a-z]{3,5}[0-9]{1,2}$`),            // id: bd-abc12 (test-style IDs)
	regexp.MustCompile(`^(testdb_|beads_t|beads_pt|doctest_)`), // id prefixes from test databases
	regexp.MustCompile(`(?i)^--help`),                          // title: "--help" CLI artifacts
	regexp.MustCompile(`(?i)^Usage:\s`),                        // title: "Usage: ..." CLI help output
	regexp.MustCompile(`^offlinebrew-`),                        // id: offlinebrew-* test prefixes
	regexp.MustCompile(`-wisp-`),                               // id: wisp-pattern IDs leaked into issues table
}

// validDBName matches safe database names (alphanumeric, underscore, hyphen).
var validDBName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// scrubQuery is the WHERE clause for filtering ephemeral data.
// Kept separate from Sprintf to avoid %% confusion.
// The query selects only durable work product (bugs, features, tasks, epics, chores).
const scrubWhereClause = ` WHERE (ephemeral IS NULL OR ephemeral != 1)` +
	` AND status != 'tombstone'` +
	` AND issue_type NOT IN ('message', 'event', 'agent', 'convoy', 'molecule', 'role', 'merge-request', 'rig')` +
	` AND id NOT LIKE '%-wisp-%'` +
	` AND id NOT LIKE '%-cv-%'` +
	` AND id NOT LIKE '%-wf-%'` +
	` AND id NOT LIKE 'test%'` +
	` AND id NOT LIKE 'beads\_t%'` +
	` AND id NOT LIKE 'beads\_pt%'` +
	` AND id NOT LIKE 'doctest\_%'` +
	` AND id NOT LIKE 'offlinebrew-%'` +
	` AND title NOT LIKE '--%'` +
	` AND title NOT LIKE 'Usage: %'` +
	` ORDER BY id`

// jsonlGitBackupInterval returns the configured interval, or the default (15m).
func jsonlGitBackupInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.JsonlGitBackup != nil {
		if config.Patrols.JsonlGitBackup.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.JsonlGitBackup.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultJsonlGitBackupInterval
}

// syncJsonlGitBackup exports issues from each database to JSONL, scrubs ephemeral data,
// and commits/pushes to a git repository.
// Non-fatal: errors are logged but don't stop the daemon.
func (d *Daemon) syncJsonlGitBackup() {
	if !d.isPatrolActive("jsonl_git_backup") {
		return
	}

	// Pour molecule for observability (nil-safe — all methods are no-ops on nil).
	mol := d.pourDogMolecule(constants.MolDogJSONL, nil)
	defer mol.Close()

	config := d.patrolConfig.Patrols.JsonlGitBackup
	prep := d.prepareJsonlGitBackup(config)
	if !prep.ok {
		return
	}

	d.logger.Printf("jsonl_git_backup: exporting %d database(s) to %s (scrub=%v)", len(prep.databases), prep.gitRepo, prep.scrub)
	counts, failed, exported := d.exportJsonlDatabases(prep.databases, prep.gitRepo, prep.dataDir, prep.scrub)

	if exported == 0 {
		d.logger.Printf("jsonl_git_backup: no databases exported successfully")
		mol.FailStep("export", "no databases exported successfully")
		return
	}

	mol.CloseStep("export")
	if !d.verifyJsonlGitBackup(prep.gitRepo, prep.databases, counts, spikeThreshold(config), mol) {
		mol.FailStep("push", "spike detected")
		return
	}
	d.commitAndReportJsonlBackup(prep.gitRepo, prep.databases, counts, failed, exported, mol)
}

type jsonlGitBackupPrep struct {
	gitRepo   string
	scrub     bool
	databases []string
	dataDir   string
	ok        bool
}

func (d *Daemon) prepareJsonlGitBackup(config *JsonlGitBackupConfig) jsonlGitBackupPrep {
	gitRepo, ok := resolveJsonlGitRepo(d.logger, config.GitRepo)
	if !ok || !jsonlGitRepoExists(d.logger, gitRepo) {
		return jsonlGitBackupPrep{}
	}
	databases := config.Databases
	if len(databases) == 0 {
		d.logger.Printf("jsonl_git_backup: no databases configured, skipping")
		return jsonlGitBackupPrep{}
	}
	dataDir := d.jsonlDataDir()
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		d.logger.Printf("jsonl_git_backup: data dir %s does not exist, skipping", dataDir)
		return jsonlGitBackupPrep{}
	}
	scrub := config.Scrub == nil || *config.Scrub
	return jsonlGitBackupPrep{gitRepo: gitRepo, scrub: scrub, databases: databases, dataDir: dataDir, ok: true}
}

func resolveJsonlGitRepo(logger *log.Logger, configured string) (string, bool) {
	if configured != "" {
		return configured, true
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		logger.Printf("jsonl_git_backup: cannot determine home dir: %v", err)
		return "", false
	}
	return filepath.Join(homeDir, ".dolt-archive", "git"), true
}

func jsonlGitRepoExists(logger *log.Logger, gitRepo string) bool {
	if _, err := os.Stat(filepath.Join(gitRepo, ".git")); os.IsNotExist(err) {
		logger.Printf("jsonl_git_backup: git repo %s does not exist, skipping", gitRepo)
		return false
	}
	return true
}

func (d *Daemon) jsonlDataDir() string {
	if d.doltServer != nil && d.doltServer.IsEnabled() && d.doltServer.config.DataDir != "" {
		return d.doltServer.config.DataDir
	}
	return filepath.Join(d.config.TownRoot, ".dolt-data")
}

func (d *Daemon) exportJsonlDatabases(databases []string, gitRepo, dataDir string, scrub bool) (map[string]int, []string, int) {
	counts := make(map[string]int)
	var failed []string
	exported := 0
	for _, db := range databases {
		n, err := d.exportDatabaseToJsonl(db, gitRepo, dataDir, scrub)
		if err != nil {
			d.logger.Printf("jsonl_git_backup: %s: export failed: %v", db, err)
			failed = append(failed, db)
			continue
		}
		counts[db] = n
		exported++
	}
	return counts, failed, exported
}

func (d *Daemon) verifyJsonlGitBackup(gitRepo string, databases []string, counts map[string]int, threshold float64, mol *dogMol) bool {
	removed := d.applyPollutionFilter(gitRepo, databases)
	if removed > 0 {
		d.logger.Printf("jsonl_git_backup: filtered %d total test-pollution record(s)", removed)
		recountAfterFilter(gitRepo, databases, counts)
	}
	if remaining := d.verifyNoPollution(gitRepo, databases); remaining > 0 {
		d.logger.Printf("jsonl_git_backup: WARNING: %d suspicious record(s) survived scrub+filter", remaining)
		d.escalate("jsonl_git_backup", fmt.Sprintf("post-scrub verification found %d suspicious records — review JSONL exports", remaining))
	}
	mol.CloseStep("verify")
	spikes := d.verifyExportCounts(gitRepo, databases, counts, threshold)
	if len(spikes) == 0 {
		return true
	}
	report := formatSpikeReport(spikes)
	d.logger.Printf("jsonl_git_backup: HALTING — spike detected:\n%s", report)
	d.escalate("jsonl_git_backup", report)
	return false
}

func (d *Daemon) commitAndReportJsonlBackup(gitRepo string, databases []string, counts map[string]int, failed []string, exported int, mol *dogMol) {
	pushStatus := "ok"
	if err := d.commitAndPushJsonlBackup(gitRepo, databases, counts, failed); err != nil {
		d.logger.Printf("jsonl_git_backup: git operations failed: %v", err)
		pushStatus = "failed"
		mol.FailStep("push", err.Error())
		d.JSONLPushFailures++
		if d.JSONLPushFailures >= maxConsecutivePushFailures {
			d.logger.Printf("jsonl_git_backup: ESCALATION: %d consecutive push failures", d.JSONLPushFailures)
			d.escalate("jsonl_git_backup", fmt.Sprintf("git push failed %d consecutive times", d.JSONLPushFailures))
			d.JSONLPushFailures = 0
		}
	} else {
		d.JSONLPushFailures = 0
		mol.CloseStep("push")
	}
	d.logger.Printf("jsonl_git_backup: exported %d/%d database(s), push=%s", exported, len(databases), pushStatus)
	mol.CloseStep("report")
}

// supplementalTables lists non-issues tables to include in JSONL backup.
// These contain structural data (dependencies, labels, config) that would be
// lost if we only backed up the issues table. Wisp tables are excluded — they
// contain high-volume ephemeral data handled by the Reaper Dog.
var supplementalTables = []string{
	"comments",
	"config",
	"dependencies",
	"events",
	"labels",
	"metadata",
}

// exportDatabaseToJsonl exports the issues table (with optional scrub) and all
// supplemental tables to JSONL files in {gitRepo}/{db}/ directory.
//
// Issues go to {db}/issues.jsonl (scrubbed). Other tables go to {db}/{table}.jsonl.
// Also writes a legacy {db}.jsonl (symlink to {db}/issues.jsonl) for backward compat.
//
// Returns the total number of records exported across all tables.
func (d *Daemon) exportDatabaseToJsonl(db, gitRepo, dataDir string, scrub bool) (int, error) {
	if !validDBName.MatchString(db) {
		return 0, fmt.Errorf("invalid database name: %q", db)
	}

	// Create per-database subdirectory.
	dbDir := filepath.Join(gitRepo, db)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return 0, fmt.Errorf("creating dir %s: %w", dbDir, err)
	}

	total := 0

	// 1. Export issues table (with scrub filter).
	var query string
	if scrub {
		query = "SELECT * FROM `" + db + "`.issues" + scrubWhereClause
	} else {
		query = "SELECT * FROM `" + db + "`.issues ORDER BY id"
	}
	n, err := d.exportTableToJsonl("issues", query, dbDir, dataDir)
	if err != nil {
		return 0, fmt.Errorf("issues: %w", err)
	}
	total += n

	// 2. Export supplemental tables (no scrub, full export).
	for _, table := range supplementalTables {
		tQuery := fmt.Sprintf("SELECT * FROM `%s`.`%s` ORDER BY 1", db, table)
		tn, err := d.exportTableToJsonl(table, tQuery, dbDir, dataDir)
		if err != nil {
			// Non-fatal for supplemental tables — log and continue.
			d.logger.Printf("jsonl_git_backup: %s/%s: export failed (non-fatal): %v", db, table, err)
			continue
		}
		total += tn
	}

	d.logger.Printf("jsonl_git_backup: %s: exported %d records across %d tables", db, total, 1+len(supplementalTables))
	return total, nil
}

// exportTableToJsonl runs a query and writes the result as JSONL to {dir}/{table}.jsonl.
// Connects to the running Dolt server via --host/--port to get current committed data,
// falling back to embedded mode (cmd.Dir=dataDir) if no server config is available.
// Returns the number of records exported.
func (d *Daemon) exportTableToJsonl(table, query, dir, dataDir string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), jsonlExportTimeout)
	defer cancel()
	cmd := d.jsonlExportCommand(ctx, query, dataDir)
	// Always set cmd.Dir to prevent stray .doltcfg/ creation (GH#2537).
	util.SetDetachedProcessGroup(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return 0, fmt.Errorf("%s: %s", err, errMsg)
		}
		return 0, err
	}

	var result struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return 0, fmt.Errorf("parsing dolt output: %w", err)
	}
	return writeJsonlRows(dir, table, result.Rows)
}

type jsonlExportConnection struct {
	host      string
	port      int
	user      string
	password  string
	useServer bool
}

func (d *Daemon) jsonlExportCommand(ctx context.Context, query, dataDir string) *exec.Cmd {
	connection := d.jsonlExportConnection()
	var cmd *exec.Cmd
	if connection.useServer {
		cmd = exec.CommandContext(ctx, "dolt",
			"--host", connection.host,
			"--port", strconv.Itoa(connection.port),
			"--no-tls", "-u", connection.user, "-p", connection.password,
			"sql", "-r", "json", "-q", query)
	} else {
		cmd = exec.CommandContext(ctx, "dolt", "sql", "-r", "json", "-q", query)
	}
	cmd.Dir = dataDir
	return cmd
}

func (d *Daemon) jsonlExportConnection() jsonlExportConnection {
	connection := jsonlExportConnection{host: "127.0.0.1", port: 3307, user: "root"}
	if d.doltServer == nil || !d.doltServer.IsEnabled() {
		return connection
	}
	connection.useServer = true
	if d.doltServer.config.Host != "" {
		connection.host = d.doltServer.config.Host
	}
	if d.doltServer.config.Port != 0 {
		connection.port = d.doltServer.config.Port
	}
	if d.doltServer.config.User != "" {
		connection.user = d.doltServer.config.User
	}
	connection.password = d.doltServer.config.Password
	return connection
}

func writeJsonlRows(dir, table string, rows []json.RawMessage) (int, error) {
	outPath := filepath.Join(dir, table+".jsonl")
	tmpPath := outPath + ".tmp"
	var buf bytes.Buffer
	for _, row := range rows {
		var compact bytes.Buffer
		if err := json.Compact(&compact, row); err != nil {
			return 0, fmt.Errorf("compacting JSON row: %w", err)
		}
		buf.Write(compact.Bytes())
		buf.WriteByte('\n')
	}

	if err := os.WriteFile(tmpPath, buf.Bytes(), 0644); err != nil {
		return 0, fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		os.Remove(tmpPath)
		return 0, fmt.Errorf("renaming %s: %w", tmpPath, err)
	}

	return len(rows), nil
}

// commitAndPushJsonlBackup stages, commits, and pushes JSONL files if changed.
// The commit message includes counts for successful exports AND names of failed
// databases, so partial failures are visible in git history.
func (d *Daemon) commitAndPushJsonlBackup(gitRepo string, databases []string, counts map[string]int, failed []string) error {
	if err := d.runGitCmd(gitRepo, gitCmdTimeout, "add", "-A", "."); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if !jsonlChangesPending(d, gitRepo) {
		d.logger.Printf("jsonl_git_backup: no changes to commit")
		return nil
	}
	msg := jsonlBackupCommitMessage(databases, counts, failed)
	if err := d.runGitCmd(gitRepo, gitCmdTimeout, "commit", "-m", msg,
		"--author=Gas Town Daemon <daemon@gastown.local>"); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	removeSpikeBaseline(gitRepo)
	return d.pushJsonlBackup(gitRepo, msg)
}

func jsonlChangesPending(d *Daemon, gitRepo string) bool {
	return d.runGitCmd(gitRepo, gitCmdTimeout, "diff", "--cached", "--quiet") != nil
}

func jsonlBackupCommitMessage(databases []string, counts map[string]int, failed []string) string {
	timestamp := time.Now().Format("2006-01-02 15:04")
	var parts []string
	for _, db := range databases {
		if n, ok := counts[db]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", db, n))
		}
	}
	msg := fmt.Sprintf("backup %s: %s", timestamp, strings.Join(parts, " "))
	if len(failed) > 0 {
		sort.Strings(failed)
		msg += fmt.Sprintf(" [FAILED: %s]", strings.Join(failed, ", "))
	}
	return msg
}

func (d *Daemon) pushJsonlBackup(gitRepo, msg string) error {
	if d.hasGitRemote(gitRepo, "origin") {
		branch := d.currentGitBranch(gitRepo)
		if branch == "" {
			branch = "main"
		}
		if err := d.runGitCmd(gitRepo, gitPushTimeout, "push", "origin", branch); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
		d.logger.Printf("jsonl_git_backup: committed and pushed: %s", msg)
	} else {
		d.logger.Printf("jsonl_git_backup: committed (no remote configured, skipping push): %s", msg)
	}
	return nil
}

// gitChildEnv returns os.Environ() augmented with HOME/USER/LOGNAME/SSH_AUTH_SOCK
// when missing. Daemon-launched git falls back to getpwuid(uid) for committer/author
// identity if $USER and $LOGNAME are absent; on macOS that lookup can fail with
// "No user exists for uid N" once the long-lived daemon's connection to
// opendirectoryd is no longer reachable. Forwarding these vars lets git use them
// directly and skip the system passwd lookup. See gh#zt1w.
func gitChildEnv() []string {
	env := os.Environ()
	have := make(map[string]bool, len(env))
	for _, kv := range env {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			have[kv[:eq]] = true
		}
	}

	if have["HOME"] && have["USER"] && have["LOGNAME"] {
		return env
	}

	// Recover identity vars from os/user. user.Current() consults $USER/$HOME
	// before falling back to getpwuid; if all three are missing it may itself
	// fail, in which case we return env unchanged and let git error normally.
	u, err := user.Current()
	if err != nil {
		return env
	}
	return appendMissingIdentity(env, have, u)
}

func appendMissingIdentity(env []string, have map[string]bool, current *user.User) []string {
	if !have["HOME"] && current.HomeDir != "" {
		env = append(env, "HOME="+current.HomeDir)
	}
	if !have["USER"] && current.Username != "" {
		env = append(env, "USER="+current.Username)
	}
	if !have["LOGNAME"] && current.Username != "" {
		env = append(env, "LOGNAME="+current.Username)
	}
	return env
}

// hasGitRemote checks if the named remote exists in the git repo.
func (d *Daemon) hasGitRemote(gitRepo, name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", gitRepo, "remote", "get-url", name)
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)
	return cmd.Run() == nil
}

// currentGitBranch returns the current branch name, or empty string on error.
func (d *Daemon) currentGitBranch(gitRepo string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", gitRepo, "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}

// runGitCmd runs a git command in the specified directory with the given timeout.
func (d *Daemon) runGitCmd(dir string, timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
		return err
	}
	return nil
}

// escalate sends an escalation message to the mayor via gt escalate.
func (d *Daemon) escalate(source, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gt", "escalate", "-s", "HIGH",
		fmt.Sprintf("%s: %s", source, message))
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(os.Environ(), "BD_ACTOR=daemon")
	util.SetDetachedProcessGroup(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("jsonl_git_backup: escalation failed: %v (%s)", err, strings.TrimSpace(string(output)))
	}
}

// spikeThreshold returns the configured spike threshold or the default (20%).
func spikeThreshold(config *JsonlGitBackupConfig) float64 {
	if config != nil && config.SpikeThreshold != nil {
		t := *config.SpikeThreshold
		if t > 0 && t <= 1.0 {
			return t
		}
	}
	return defaultSpikeThreshold
}

// isTestPollution checks if a JSONL record looks like test data that leaked into
// production. Checks both "id" and "title" fields against known test patterns.
func isTestPollution(record map[string]interface{}) bool {
	for _, field := range []string{"id", "title"} {
		val, ok := record[field]
		if !ok {
			continue
		}
		s, ok := val.(string)
		if !ok {
			continue
		}
		for _, pat := range testPollutionPatterns {
			if pat.MatchString(s) {
				return true
			}
		}
	}
	return false
}

// filterTestPollution removes test-data records from a JSONL byte buffer.
// Returns the filtered buffer and the number of records removed.
func filterTestPollution(data []byte) ([]byte, int) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Increase buffer for large JSONL lines.
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var out bytes.Buffer
	removed := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record map[string]interface{}
		if err := json.Unmarshal(line, &record); err != nil {
			// Can't parse — keep it (don't silently drop unknown data).
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		if isTestPollution(record) {
			removed++
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), removed
}

// previousCommitLineCount returns the line count of a file in the previous git
// commit (HEAD). Returns 0, nil if the file doesn't exist in HEAD (first export).
func previousCommitLineCount(gitRepo, relPath string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", gitRepo, "show", "HEAD:"+filepath.ToSlash(relPath))
	cmd.Env = gitChildEnv()
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// stderr intentionally not captured — "does not exist" is an expected case.

	if err := cmd.Run(); err != nil {
		// File doesn't exist in HEAD — first export, no baseline.
		return 0, nil
	}

	lines := 0
	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		lines++
	}
	return lines, nil
}

// spikeInfo holds the result of a spike check for a single database file.
type spikeInfo struct {
	DB       string
	File     string
	Previous int
	Current  int
	Delta    float64 // absolute fractional change (0.0–1.0+)
}

// spikeBaseline records counts from a halted export so that subsequent runs
// can detect when the count has stabilized at a new level.
type spikeBaseline struct {
	Counts    map[string]int `json:"counts"`
	Timestamp string         `json:"timestamp"`
}

const spikeBaselineFile = ".spike-counts.json"

// loadSpikeBaseline reads the spike baseline file from the git repo directory.
// Returns nil if the file doesn't exist or can't be parsed.
func loadSpikeBaseline(gitRepo string) *spikeBaseline {
	path := filepath.Join(gitRepo, spikeBaselineFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var sb spikeBaseline
	if err := json.Unmarshal(data, &sb); err != nil {
		return nil
	}
	return &sb
}

// saveSpikeBaseline writes the current counts as a spike baseline file.
// Also ensures the file is git-ignored so it doesn't get committed.
func saveSpikeBaseline(gitRepo string, counts map[string]int) error {
	sb := spikeBaseline{
		Counts:    counts,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return err
	}
	// Ensure the spike baseline file is git-ignored.
	ensureGitIgnore(gitRepo, spikeBaselineFile)
	return os.WriteFile(filepath.Join(gitRepo, spikeBaselineFile), data, 0644)
}

// ensureGitIgnore adds an entry to .gitignore if not already present.
func ensureGitIgnore(gitRepo, entry string) {
	ignorePath := filepath.Join(gitRepo, ".gitignore")
	data, _ := os.ReadFile(ignorePath)
	content := string(data)
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == entry {
			return // Already present.
		}
	}
	// Append the entry.
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += entry + "\n"
	if err := os.WriteFile(ignorePath, []byte(content), 0644); err != nil {
		// Non-fatal: spike-baseline writes still work without the ignore entry.
		return
	}
}

// removeSpikeBaseline removes the spike baseline file after a successful commit.
func removeSpikeBaseline(gitRepo string) {
	os.Remove(filepath.Join(gitRepo, spikeBaselineFile))
}

// verifyExportCounts compares current export line counts against the previous
// commit for each database. Returns a list of anomalies that exceed the spike
// threshold. On first export (no baseline), verification is skipped.
//
// Asymmetric thresholds: drops (possible data loss) use the configured threshold;
// increases (new issues filed) use 2x the threshold since growth is normal.
// Small absolute changes (<20 records) are always allowed to avoid false alarms
// on small databases.
//
// Recovery mechanism: when spike detection fires, a baseline file is saved with
// the current counts. On the next run, if the current count is stable relative
// to the spike baseline (within threshold), the spike is cleared and the export
// proceeds. This prevents permanent blocking after legitimate large changes
// (e.g., Reaper purges, filter updates).
func (d *Daemon) verifyExportCounts(gitRepo string, databases []string, counts map[string]int, threshold float64) []spikeInfo {
	var spikes []spikeInfo
	spikeBase := loadSpikeBaseline(gitRepo)

	for _, db := range databases {
		currentCount, ok := counts[db]
		if !ok {
			continue // database failed export, skip
		}
		spike, found := d.verifyJsonlDatabaseCount(gitRepo, db, currentCount, threshold, spikeBase)
		if found {
			spikes = append(spikes, spike)
		}
	}
	if len(spikes) > 0 {
		if err := saveSpikeBaseline(gitRepo, counts); err != nil {
			d.logger.Printf("jsonl_git_backup: failed to save spike baseline: %v", err)
		}
	}

	return spikes
}

func (d *Daemon) verifyJsonlDatabaseCount(gitRepo, db string, currentCount int, threshold float64, spikeBase *spikeBaseline) (spikeInfo, bool) {
	const minAbsoluteDelta = 20
	relPath := filepath.Join(db, "issues.jsonl")
	prevCount, err := previousCommitLineCount(gitRepo, relPath)
	if err != nil {
		d.logger.Printf("jsonl_git_backup: verify: %s: error reading baseline: %v", db, err)
		return spikeInfo{}, false
	}
	if prevCount == 0 {
		d.logger.Printf("jsonl_git_backup: verify: %s: first export (%d records), skipping spike check", db, currentCount)
		return spikeInfo{}, false
	}
	absDelta := currentCount - prevCount
	if absDelta < 0 {
		absDelta = -absDelta
	}
	if absDelta < minAbsoluteDelta {
		return spikeInfo{}, false
	}
	fractionalDelta := math.Abs(float64(currentCount-prevCount)) / float64(prevCount)
	effectiveThreshold := growthAdjustedThreshold(threshold, currentCount > prevCount)
	if fractionalDelta <= effectiveThreshold {
		return spikeInfo{}, false
	}
	if d.stableSpikeBaseline(db, prevCount, currentCount, fractionalDelta, threshold, spikeBase) {
		return spikeInfo{}, false
	}
	direction := "jump"
	if currentCount < prevCount {
		direction = "drop"
	}
	d.logger.Printf("jsonl_git_backup: SPIKE DETECTED: %s: %s from %d to %d (%.1f%% %s, threshold %.1f%%)",
		db, direction, prevCount, currentCount, fractionalDelta*100, direction, effectiveThreshold*100)
	return spikeInfo{DB: db, File: relPath, Previous: prevCount, Current: currentCount, Delta: fractionalDelta}, true
}

func growthAdjustedThreshold(threshold float64, growth bool) float64 {
	if growth {
		return threshold * 2
	}
	return threshold
}

func (d *Daemon) stableSpikeBaseline(db string, prevCount, currentCount int, fractionalDelta, threshold float64, spikeBase *spikeBaseline) bool {
	if spikeBase == nil {
		return false
	}
	baseCount, ok := spikeBase.Counts[db]
	if !ok || baseCount <= 0 {
		return false
	}
	baseDelta := math.Abs(float64(currentCount-baseCount)) / float64(baseCount)
	if baseDelta > threshold {
		return false
	}
	d.logger.Printf("jsonl_git_backup: %s: count stable vs spike baseline (%d → %d, %.1f%% vs baseline %d), accepting new level",
		db, prevCount, currentCount, fractionalDelta*100, baseCount)
	return true
}

// formatSpikeReport creates a human-readable summary of spike anomalies for escalation.
func formatSpikeReport(spikes []spikeInfo) string {
	var b strings.Builder
	b.WriteString("JSONL export spike detection triggered:\n")
	for _, s := range spikes {
		direction := "JUMP (possible pollution)"
		if s.Current < s.Previous {
			direction = "DROP (possible data loss)"
		}
		fmt.Fprintf(&b, "  %s: %d → %d (%.1f%% change) — %s\n",
			s.DB, s.Previous, s.Current, s.Delta*100, direction)
	}
	b.WriteString("Export halted. Manual review required.")
	return b.String()
}

// countFileLines counts the number of non-empty lines in a file.
func countFileLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			count++
		}
	}
	return count, scanner.Err()
}

// recountAfterFilter re-reads the issues.jsonl file for each database to get
// accurate post-filter line counts. This is needed because counts from
// exportDatabaseToJsonl reflect pre-filter totals.
func recountAfterFilter(gitRepo string, databases []string, counts map[string]int) {
	for _, db := range databases {
		if _, ok := counts[db]; !ok {
			continue
		}
		issuesPath := filepath.Join(gitRepo, db, "issues.jsonl")
		n, err := countFileLines(issuesPath)
		if err != nil {
			continue
		}
		counts[db] = n
	}
}

// applyPollutionFilter reads each database's issues.jsonl, filters out test
// pollution records, and rewrites the file. Returns total records removed.
func (d *Daemon) applyPollutionFilter(gitRepo string, databases []string) int {
	totalRemoved := 0
	for _, db := range databases {
		issuesPath := filepath.Join(gitRepo, db, "issues.jsonl")
		data, err := os.ReadFile(issuesPath)
		if err != nil {
			continue
		}
		filtered, removed := filterTestPollution(data)
		if removed > 0 {
			d.logger.Printf("jsonl_git_backup: %s: filtered %d test-pollution record(s)", db, removed)
			if err := os.WriteFile(issuesPath, filtered, 0644); err != nil {
				d.logger.Printf("jsonl_git_backup: %s: error writing filtered file: %v", db, err)
				continue
			}
			totalRemoved += removed
		}
	}
	return totalRemoved
}

// verifyNoPollution re-scans all exported issues.jsonl files for any remaining
// suspicious records that survived both the SQL scrub and the regex filter.
// Returns the total number of suspicious records found across all databases.
func (d *Daemon) verifyNoPollution(gitRepo string, databases []string) int {
	total := 0
	for _, db := range databases {
		issuesPath := filepath.Join(gitRepo, db, "issues.jsonl")
		data, err := os.ReadFile(issuesPath)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var record map[string]interface{}
			if err := json.Unmarshal(line, &record); err != nil {
				continue
			}
			if isTestPollution(record) {
				id, _ := record["id"].(string)
				title, _ := record["title"].(string)
				d.logger.Printf("jsonl_git_backup: VERIFY FAIL: %s: suspicious record id=%q title=%q", db, id, title)
				total++
			}
		}
	}
	return total
}

// parseLineCount parses a line count from `wc -l` style output or plain integer.
func parseLineCount(s string) (int, error) {
	s = strings.TrimSpace(s)
	// wc -l output format: "  42 filename" or just "42"
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty input")
	}
	return strconv.Atoi(fields[0])
}
