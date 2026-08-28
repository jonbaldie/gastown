// Package beads provides custom type management for agent beads.
package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/townroot"
	"github.com/jonbaldie/gastown/internal/util"
)

// typesSentinel is a marker file indicating custom types have been configured.
// This persists across CLI invocations to avoid redundant bd config calls.
const typesSentinel = ".gt-types-configured"

// statusesSentinel is a marker file indicating custom statuses have been configured.
const statusesSentinel = ".gt-statuses-configured"

// ensuredDirs tracks which beads directories have been ensured this session.
// This provides fast in-memory caching for multiple creates in the same CLI run.
var (
	ensuredDirs = make(map[string]bool)
	ensuredMu   sync.Mutex
)

// FindTownRoot walks up from startDir to find the Gas Town root directory.
// Delegates to townroot.Find so every caller shares the outermost-town.json rule.
func FindTownRoot(startDir string) string {
	return townroot.Find(startDir)
}

// ResolveRoutingTarget determines which beads directory a bead ID will route to.
// It extracts the prefix from the bead ID and looks up the corresponding route.
// Returns the resolved beads directory path, following any redirects.
//
// If townRoot is empty or prefix is not found, falls back to the provided fallbackDir.
func ResolveRoutingTarget(townRoot, beadID, fallbackDir string) string {
	auth := NewAuthority(townRoot).WithFallback(fallbackDir)
	s := auth.ForBead(beadID)
	if s.Routed() {
		return s.BeadsDir()
	}
	if townRoot != "" {
		prefix := ExtractPrefix(beadID)
		if prefix != "" {
			fmt.Fprintf(os.Stderr, "Warning: no route found for prefix %q (bead %s), falling back to %s\n", prefix, beadID, fallbackDir)
		}
	}
	return fallbackDir
}

// EnsureCustomTypes ensures the target beads directory has custom types configured.
// Uses a two-level caching strategy:
//   - In-memory cache for multiple creates in the same CLI invocation
//   - Sentinel file on disk for persistence across CLI invocations
//
// The sentinel file stores the configured custom and infra type lists. When
// either list changes, the sentinel is detected as stale and types are
// re-configured automatically (gt-zmy, gt-26f).
//
// This function is thread-safe and idempotent.
//
// If the beads database does not exist (e.g., after a fresh rig add), this function
// will attempt to initialize it automatically using bd init --server.
func EnsureCustomTypes(beadsDir string) error {
	if beadsDir == "" {
		return fmt.Errorf("empty beads directory")
	}

	customTypes := strings.Join(constants.BeadsCustomTypesList(), ",")
	infraTypes := strings.Join(constants.BeadsInfraTypesList(), ",")
	sentinelValue := TypeConfigSentinelValue()

	ensuredMu.Lock()
	defer ensuredMu.Unlock()

	if customTypesCached(beadsDir, sentinelValue) {
		return nil
	}
	if err := validateBeadsDirectory(beadsDir); err != nil {
		return err
	}
	if err := ensureDatabaseInitialized(beadsDir); err != nil {
		return fmt.Errorf("ensure database initialized: %w", err)
	}
	if err := configureCustomTypes(beadsDir, customTypes, infraTypes); err != nil {
		return err
	}
	sentinelPath := filepath.Join(beadsDir, typesSentinel)
	_ = os.WriteFile(sentinelPath, []byte(sentinelValue+"\n"), 0644)
	ensuredDirs[beadsDir] = true
	return nil
}

func customTypesCached(beadsDir, sentinelValue string) bool {
	if ensuredDirs[beadsDir] {
		return true
	}
	data, err := os.ReadFile(filepath.Join(beadsDir, typesSentinel))
	if err != nil || strings.TrimSpace(string(data)) != sentinelValue {
		return false
	}
	ensuredDirs[beadsDir] = true
	return true
}

func validateBeadsDirectory(beadsDir string) error {
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return fmt.Errorf("beads directory does not exist: %s", beadsDir)
	}
	return nil
}

func configureCustomTypes(beadsDir, customTypes, infraTypes string) error {
	configs := []struct{ key, value string }{
		{key: "types.custom", value: customTypes},
		{key: "types.infra", value: infraTypes},
	}
	bdEnv := BuildMutationPinnedBDEnv(os.Environ(), beadsDir)
	for _, config := range configs {
		if err := setBDConfig(beadsDir, bdEnv, config.key, config.value); err != nil {
			return err
		}
		if err := verifyBDConfig(beadsDir, bdEnv, config.key, config.value); err != nil {
			return err
		}
	}
	return nil
}

// TypeConfigSentinelValue returns the current type configuration fingerprint.
// Tests in other packages use this to avoid duplicating the sentinel format.
func TypeConfigSentinelValue() string {
	return fmt.Sprintf("types.custom=%s\ntypes.infra=%s",
		strings.Join(constants.BeadsCustomTypesList(), ","),
		strings.Join(constants.BeadsInfraTypesList(), ","))
}

func setBDConfig(beadsDir string, env []string, key, value string) error {
	cmd := Spawn("config", "set", key, value)
	cmd.Dir = beadsDir
	util.SetDetachedProcessGroup(cmd)
	// Set BEADS_DIR and BEADS_DOLT_SERVER_DATABASE explicitly to ensure bd
	// operates on the correct database. Strip inherited values first — getenv()
	// returns the first match (gt-uygpe).
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("configure %s in %s: %s: %w", key, beadsDir, strings.TrimSpace(string(output)), err)
	}
	return nil
}

func verifyBDConfig(beadsDir string, env []string, key, want string) error {
	cmd := Spawn("config", "get", key)
	cmd.Dir = beadsDir
	cmd.Env = env
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	got := ParseConfigOutput(output)
	if err != nil || got != want {
		return fmt.Errorf("%s not persisted in %s after bd config set (verify returned %q): db may be misconfigured", key, beadsDir, got)
	}
	return nil
}

// EnsureCustomTypesConfigYAML records Gas Town custom types directly in
// config.yaml. Fresh install uses this before invoking any bd config command so
// older cached bd binaries do not initialize legacy views against the new schema.
func EnsureCustomTypesConfigYAML(beadsDir string) error {
	if beadsDir == "" {
		return fmt.Errorf("empty beads directory")
	}
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return fmt.Errorf("beads directory does not exist: %s", beadsDir)
	}

	customTypes := strings.Join(constants.BeadsCustomTypesList(), ",")
	infraTypes := strings.Join(constants.BeadsInfraTypesList(), ",")

	if err := EnsureConfigYAMLValue(beadsDir, "types.custom", customTypes); err != nil {
		return err
	}
	if err := EnsureConfigYAMLValue(beadsDir, "types.infra", infraTypes); err != nil {
		return err
	}
	return nil
}

// EnsureCustomStatuses ensures the target beads directory has custom statuses configured.
// Uses the same two-level caching strategy as EnsureCustomTypes:
//   - In-memory cache for multiple operations in the same CLI invocation
//   - Sentinel file on disk for persistence across CLI invocations
//
// This function is thread-safe and idempotent.
func EnsureCustomStatuses(beadsDir string) error {
	if beadsDir == "" {
		return fmt.Errorf("empty beads directory")
	}

	statusesList := strings.Join(constants.BeadsCustomStatusesList(), ",")

	ensuredMu.Lock()
	defer ensuredMu.Unlock()

	cacheKey := beadsDir + ":statuses"
	if customStatusesCached(cacheKey, beadsDir, statusesList) {
		return nil
	}
	if err := validateBeadsDirectory(beadsDir); err != nil {
		return err
	}
	if err := ensureDatabaseInitialized(beadsDir); err != nil {
		return fmt.Errorf("ensure database initialized: %w", err)
	}
	mergedStatuses := mergedCustomStatuses(beadsDir)
	if err := setCustomStatuses(beadsDir, mergedStatuses); err != nil {
		return err
	}
	sentinelPath := filepath.Join(beadsDir, statusesSentinel)
	_ = os.WriteFile(sentinelPath, []byte(statusesList+"\n"), 0644)
	ensuredDirs[cacheKey] = true
	return nil
}

func customStatusesCached(cacheKey, beadsDir, statusesList string) bool {
	if ensuredDirs[cacheKey] {
		return true
	}
	data, err := os.ReadFile(filepath.Join(beadsDir, statusesSentinel))
	if err != nil || strings.TrimSpace(string(data)) != statusesList {
		return false
	}
	ensuredDirs[cacheKey] = true
	return true
}

func mergedCustomStatuses(beadsDir string) string {
	getCmd := Spawn("config", "get", "status.custom")
	getCmd.Dir = beadsDir
	util.SetDetachedProcessGroup(getCmd)
	getEnv := BuildReadOnlyPinnedBDEnv(os.Environ(), beadsDir)
	getCmd.Env = getEnv
	existingOutput, _ := getCmd.Output()
	return mergeCustomStatuses(ParseConfigOutput(existingOutput))
}

func mergeCustomStatuses(existing string) string {
	statusSet := make(map[string]bool)
	if existing != "" {
		for _, s := range strings.Split(existing, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				statusSet[s] = true
			}
		}
	}
	for _, s := range constants.BeadsCustomStatusesList() {
		statusSet[s] = true
	}
	var merged []string
	for s := range statusSet {
		merged = append(merged, s)
	}
	sort.Strings(merged)
	return strings.Join(merged, ",")
}

func setCustomStatuses(beadsDir, statuses string) error {
	cmd := Spawn("config", "set", "status.custom", statuses)
	cmd.Dir = beadsDir
	util.SetDetachedProcessGroup(cmd)
	setEnv := BuildMutationPinnedBDEnv(os.Environ(), beadsDir)
	cmd.Env = setEnv
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("configure custom statuses in %s: %s: %w",
			beadsDir, strings.TrimSpace(string(output)), err)
	}
	return nil
}

// prefixRe validates beads prefix format. Must start with a letter, contain only
// alphanumerics and hyphens, max 20 chars.
// NOTE: This MUST stay in sync with beadsPrefixRegexp in internal/rig/manager.go.
// Both exist because rig/manager.go cannot import internal/beads (circular dep).
var prefixRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,19}$`)

// ensureDatabaseInitialized checks if a beads database exists and initializes it if needed.
// This handles the case where a rig was added but the database was never created,
// which causes Dolt panics when trying to create agent beads.
//
// Uses --server mode to match all production bd init callers (gastown uses a
// centralized Dolt sql-server). JSONL auto-import is handled by bd init itself.
func ensureDatabaseInitialized(beadsDir string) error {
	if databaseAlreadyInitialized(beadsDir) {
		return nil
	}
	return initializeBeadsDatabase(beadsDir)
}

func databaseAlreadyInitialized(beadsDir string) bool {
	return beadsRedirectExists(beadsDir) || beadsDoltDirectoryExists(beadsDir) || serverBeadsDatabaseExists(beadsDir)
}

func beadsRedirectExists(beadsDir string) bool {
	_, err := os.Stat(filepath.Join(beadsDir, "redirect"))
	return err == nil
}

func beadsDoltDirectoryExists(beadsDir string) bool {
	_, err := os.Stat(filepath.Join(beadsDir, "dolt"))
	return err == nil
}

func serverBeadsDatabaseExists(beadsDir string) bool {
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		return false
	}
	var metadata struct {
		DoltMode     string `json:"dolt_mode"`
		DoltDatabase string `json:"dolt_database"`
	}
	if json.Unmarshal(data, &metadata) != nil || metadata.DoltMode != "server" || metadata.DoltDatabase == "" {
		return true
	}
	townRoot := FindTownRoot(filepath.Dir(beadsDir))
	if townRoot == "" {
		return true
	}
	_, err = os.Stat(filepath.Join(townRoot, ".dolt-data", metadata.DoltDatabase))
	return !os.IsNotExist(err)
}

func initializeBeadsDatabase(beadsDir string) error {
	prefix := detectPrefix(beadsDir)
	parentDir := filepath.Dir(beadsDir)
	if err := runBeadsDatabaseInit(beadsDir, parentDir, prefix); err != nil {
		return err
	}
	persistBeadsIssuePrefix(beadsDir, parentDir, prefix)
	migrateBeadsDatabase(beadsDir, parentDir)
	return nil
}

func runBeadsDatabaseInit(beadsDir, parentDir, prefix string) error {
	args := []string{"init"}
	if prefix != "" {
		args = append(args, "--prefix", prefix)
	}
	cmd := Spawn(append(args, "--server")...)
	cmd.Dir = parentDir
	cmd.Env = BuildMutationPinnedBDEnv(os.Environ(), beadsDir)
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(output), "already initialized") {
		return nil
	}
	return fmt.Errorf("bd init: %s: %w", strings.TrimSpace(string(output)), err)
}

func persistBeadsIssuePrefix(beadsDir, parentDir, prefix string) {
	if prefix == "" {
		return
	}
	cmd := Spawn("config", "set", "issue_prefix", prefix)
	cmd.Dir = parentDir
	cmd.Env = BuildMutationPinnedBDEnv(os.Environ(), beadsDir)
	util.SetDetachedProcessGroup(cmd)
	_, _ = cmd.CombinedOutput()
}

func migrateBeadsDatabase(beadsDir, parentDir string) {
	env := BuildMutationPinnedBDEnv(os.Environ(), beadsDir)
	if runBeadsMigration(parentDir, env) == nil {
		return
	}
	time.Sleep(500 * time.Millisecond)
	_ = runBeadsMigration(parentDir, env)
}

func runBeadsMigration(parentDir string, env []string) error {
	cmd := Spawn("migrate", "--yes")
	cmd.Dir = parentDir
	cmd.Env = env
	util.SetDetachedProcessGroup(cmd)
	_, err := cmd.CombinedOutput()
	return err
}

// detectPrefix determines the beads prefix for a directory.
// Resolution order:
//  1. Town-level config: FindTownRoot → config.GetRigPrefix (authoritative source from rigs.json)
//  2. Local config.yaml: issue-prefix or prefix field
//  3. Default: "gt"
//
// All candidates are validated against prefixRe before use.
//
// Known limitation: when beadsDir is a routed path (e.g., mayor/rig/.beads
// via beads routing), filepath.Base(filepath.Dir(beadsDir)) yields "rig" not
// the actual rig name. GetRigPrefix will not find "rig" in rigs.json and
// returns the default "gt". This is a safe fallback — "gt" is the universal
// default prefix — but rigs with custom prefixes accessed via routed paths
// will silently use "gt" instead. Fixing this would require walking up the
// directory tree to resolve the actual rig name, which is out of scope for
// this crash-prevention guard.
func detectPrefix(beadsDir string) string {
	rigDir := filepath.Dir(beadsDir)
	if prefix := townPrefix(rigDir); prefix != "" {
		return prefix
	}
	if prefix := configPrefix(beadsDir); prefix != "" {
		return prefix
	}
	return "gt"
}

func townPrefix(rigDir string) string {
	townRoot := FindTownRoot(rigDir)
	if townRoot == "" {
		return ""
	}
	prefix := config.GetRigPrefix(townRoot, filepath.Base(rigDir))
	if prefixRe.MatchString(prefix) {
		return prefix
	}
	return ""
}

func configPrefix(beadsDir string) string {
	data, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if prefix := configPrefixFromLine(line); prefix != "" {
			return prefix
		}
	}
	return ""
}

func configPrefixFromLine(line string) string {
	line = strings.TrimSpace(line)
	for _, key := range []string{"issue-prefix:", "prefix:"} {
		if strings.HasPrefix(line, key) {
			return validConfigPrefix(line)
		}
	}
	return ""
}

func validConfigPrefix(line string) string {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return ""
	}
	candidate := strings.TrimSuffix(stripYAMLQuotes(strings.TrimSpace(parts[1])), "-")
	if candidate != "" && prefixRe.MatchString(candidate) {
		return candidate
	}
	return ""
}

// stripYAMLQuotes removes surrounding single or double quotes from a string.
// Note: unlike strings.Trim in detectBeadsPrefixFromConfig (rig/manager.go),
// this only strips matching pairs — arguably more correct for well-formed YAML.
func stripYAMLQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// ResetEnsuredDirs clears the in-memory cache of ensured directories.
// This is primarily useful for testing.
func ResetEnsuredDirs() {
	ensuredMu.Lock()
	defer ensuredMu.Unlock()
	ensuredDirs = make(map[string]bool)
}

// ParseConfigOutput extracts the config value from `bd config get <key>` output,
// filtering out informational lines (`Note: ...`) and the unset sentinel
// (`<key> (not set)`). Returns "" when no value line is present.
//
// Without this filter, callers that merge the parsed value back into a
// `bd config set` would pollute the config with strings like
// "status.custom (not set)", which fail bd's regex validation (gt-kbi).
func ParseConfigOutput(output []byte) string {
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "Note:") && !strings.Contains(line, "(not set)") {
			return line
		}
	}
	return ""
}
