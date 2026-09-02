package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonbaldie/gastown/internal/atomicfile"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/townroot"
)

// resolveConfigMu serializes agent config resolution across all callers.
// ResolveRoleAgentConfig and ResolveAgentConfig load rig-specific agents
// into a global registry; concurrent calls for different rigs would corrupt
// each other's lookups.
var resolveConfigMu sync.Mutex

var (
	// ErrNotFound indicates the config file does not exist.
	ErrNotFound = errors.New("config file not found")

	// ErrInvalidVersion indicates an unsupported schema version.
	ErrInvalidVersion = errors.New("unsupported config version")

	// ErrInvalidType indicates an unexpected config type.
	ErrInvalidType = errors.New("invalid config type")

	// ErrMissingField indicates a required field is missing.
	ErrMissingField = errors.New("missing required field")
)

// LoadTownConfig loads and validates a town configuration file.
func LoadTownConfig(path string) (*TownConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is from trusted config location
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var config TownConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := validateTownConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveTownConfig saves a town configuration to a file.
func SaveTownConfig(path string, config *TownConfig) error {
	if err := validateTownConfig(config); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	return nil
}

// LoadRigsConfig loads and validates a rigs registry file.
// Retries once on read/parse errors to tolerate the brief window during which a
// concurrent non-atomic writer could leave the file truncated. With
// SaveRigsConfig now using atomic write-then-rename this is belt-and-suspenders
// against older versions that may still be writing the file.
func LoadRigsConfig(path string) (*RigsConfig, error) {
	readAndParse := func() (*RigsConfig, error) {
		data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
			}
			return nil, fmt.Errorf("reading config: %w", err)
		}

		var config RigsConfig
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("parsing config: %w", err)
		}

		if err := validateRigsConfig(&config); err != nil {
			return nil, err
		}

		return &config, nil
	}

	cfg, err := readAndParse()
	if err != nil && !errors.Is(err, ErrNotFound) {
		cfg, err = readAndParse()
	}
	return cfg, err
}

// SaveRigsConfig saves a rigs registry to a file atomically.
// Writes to a temp file in the same directory then renames into place; the
// rename is atomic on POSIX, so concurrent readers never observe a zero-byte
// or partially-written rigs.json.
func SaveRigsConfig(path string, config *RigsConfig) error {
	if err := validateRigsConfig(config); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}

	if err := atomicfile.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	return nil
}

// validateTownConfig validates a TownConfig.
func validateTownConfig(c *TownConfig) error {
	if c.Type != "town" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'town', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentTownVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentTownVersion)
	}
	if c.Name == "" {
		return fmt.Errorf("%w: name", ErrMissingField)
	}
	return nil
}

// validateRigsConfig validates a RigsConfig.
func validateRigsConfig(c *RigsConfig) error {
	if c.Version > CurrentRigsVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentRigsVersion)
	}
	if c.Rigs == nil {
		c.Rigs = make(map[string]RigEntry)
	}
	return nil
}

// LoadRigConfig loads and validates a rig configuration file.
func LoadRigConfig(path string) (*RigConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var config RigConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := validateRigConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveRigConfig saves a rig configuration to a file.
func SaveRigConfig(path string, config *RigConfig) error {
	if err := validateRigConfig(config); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil { //nolint:gosec // G306: config files don't contain secrets
		return fmt.Errorf("writing config: %w", err)
	}

	return nil
}

// validateRigConfig validates a RigConfig (identity only).
func validateRigConfig(c *RigConfig) error {
	if c.Type != "rig" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'rig', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentRigConfigVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentRigConfigVersion)
	}
	if c.Name == "" {
		return fmt.Errorf("%w: name", ErrMissingField)
	}
	return nil
}

// validateRigSettings validates a RigSettings.
func validateRigSettings(c *RigSettings) error {
	if c.Type != "rig-settings" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'rig-settings', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentRigSettingsVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentRigSettingsVersion)
	}
	if c.MergeQueue != nil {
		if err := validateMergeQueueConfig(c.MergeQueue); err != nil {
			return err
		}
	}
	return nil
}

// ErrInvalidOnConflict indicates an invalid on_conflict strategy.
var ErrInvalidOnConflict = errors.New("invalid on_conflict strategy")

// validateMergeQueueConfig validates a MergeQueueConfig.
func validateMergeQueueConfig(c *MergeQueueConfig) error {
	for _, validate := range []func(*MergeQueueConfig) error{
		validateMergeQueueConflict,
		validateMergeQueuePollInterval,
		validateMergeQueueStaleClaimTimeout,
		validateMergeQueueLimits,
	} {
		if err := validate(c); err != nil {
			return err
		}
	}
	return nil
}

func validateMergeQueueConflict(c *MergeQueueConfig) error {
	if c.OnConflict == "" || c.OnConflict == OnConflictAssignBack || c.OnConflict == OnConflictAutoRebase {
		return nil
	}
	return fmt.Errorf("%w: got '%s', want '%s' or '%s'", ErrInvalidOnConflict, c.OnConflict, OnConflictAssignBack, OnConflictAutoRebase)
}

func validateMergeQueuePollInterval(c *MergeQueueConfig) error {
	if c.PollInterval == "" {
		return nil
	}
	if _, err := time.ParseDuration(c.PollInterval); err != nil {
		return fmt.Errorf("invalid poll_interval: %w", err)
	}
	return nil
}

func validateMergeQueueStaleClaimTimeout(c *MergeQueueConfig) error {
	if c.StaleClaimTimeout == "" {
		return nil
	}
	duration, err := time.ParseDuration(c.StaleClaimTimeout)
	if err != nil {
		return fmt.Errorf("invalid stale_claim_timeout: %w", err)
	}
	if duration <= 0 {
		return fmt.Errorf("stale_claim_timeout must be positive, got %v", duration)
	}
	return nil
}

func validateMergeQueueLimits(c *MergeQueueConfig) error {
	if c.RetryFlakyTests < 0 {
		return fmt.Errorf("%w: retry_flaky_tests must be non-negative", ErrMissingField)
	}
	if c.MaxConcurrent < 0 {
		return fmt.Errorf("%w: max_concurrent must be non-negative", ErrMissingField)
	}
	return nil
}

// NewRigConfig creates a new RigConfig (identity only).
func NewRigConfig(name, gitURL string) *RigConfig {
	return &RigConfig{
		Type:    "rig",
		Version: CurrentRigConfigVersion,
		Name:    name,
		GitURL:  gitURL,
	}
}

// NewRigSettings creates a new RigSettings with defaults.
func NewRigSettings() *RigSettings {
	return &RigSettings{
		Type:       "rig-settings",
		Version:    CurrentRigSettingsVersion,
		MergeQueue: DefaultMergeQueueConfig(),
		Namepool:   DefaultNamepoolConfig(),
	}
}

// RepoSettingsPath is the conventional path within a repository where
// gastown rig settings can be stored. This file is committed to git and
// provides durable defaults that survive rig re-scaffolding.
const RepoSettingsPath = ".gastown/settings.json"

// LoadRepoSettings loads rig settings from a repository's .gastown/settings.json.
// Returns nil, nil if the file does not exist (repo has no gastown settings).
func LoadRepoSettings(repoRoot string) (*RigSettings, error) {
	path := filepath.Join(repoRoot, RepoSettingsPath)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading repo settings: %w", err)
	}

	var settings RigSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parsing repo settings %s: %w", path, err)
	}

	return &settings, nil
}

// MergeSettingsCommand merges a repo-sourced MergeQueueConfig (floor) with
// a local override. Non-empty fields in the override take precedence.
// Returns a new config without mutating either input.
func MergeSettingsCommand(repo, local *MergeQueueConfig) *MergeQueueConfig {
	if repo == nil && local == nil {
		return nil
	}
	result := &MergeQueueConfig{}
	if repo != nil {
		*result = *repo
	}
	overlayMergeQueueSettings(result, local)
	return result
}

func overlayMergeQueueSettings(result, local *MergeQueueConfig) {
	if local == nil {
		return
	}
	overlayMergeQueueCommands(&result.MergeQueueCommands, &local.MergeQueueCommands)
	overlayMergeQueueFlags(result, local)
	overlayMergeQueueLimits(result, local)
}

func overlayMergeQueueCommands(result, local *MergeQueueCommands) {
	if local.SetupCommand != "" {
		result.SetupCommand = local.SetupCommand
	}
	if local.TypecheckCommand != "" {
		result.TypecheckCommand = local.TypecheckCommand
	}
	if local.LintCommand != "" {
		result.LintCommand = local.LintCommand
	}
	if local.TestCommand != "" {
		result.TestCommand = local.TestCommand
	}
	if local.BuildCommand != "" {
		result.BuildCommand = local.BuildCommand
	}
}

func overlayMergeQueueFlags(result, local *MergeQueueConfig) {
	if local.Enabled {
		result.Enabled = local.Enabled
	}
	if local.MergeStrategy != "" {
		result.MergeStrategy = local.MergeStrategy
	}
	if local.OnConflict != "" {
		result.OnConflict = local.OnConflict
	}
	if local.RunTests != nil {
		result.RunTests = local.RunTests
	}
	if local.DeleteMergedBranches != nil {
		result.DeleteMergedBranches = local.DeleteMergedBranches
	}
}

func overlayMergeQueueLimits(result, local *MergeQueueConfig) {
	if local.RetryFlakyTests > 0 {
		result.RetryFlakyTests = local.RetryFlakyTests
	}
	if local.PollInterval != "" {
		result.PollInterval = local.PollInterval
	}
	if local.MaxConcurrent > 0 {
		result.MaxConcurrent = local.MaxConcurrent
	}
	if local.StaleClaimTimeout != "" {
		result.StaleClaimTimeout = local.StaleClaimTimeout
	}
	if local.RequireReview != nil {
		result.RequireReview = local.RequireReview
	}
}

// LoadRigSettings loads and validates a rig settings file.
func LoadRigSettings(path string) (*RigSettings, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading settings: %w", err)
	}

	var settings RigSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parsing settings: %w", err)
	}

	if err := validateRigSettings(&settings); err != nil {
		return nil, err
	}

	// Check for deprecated merge_queue keys that were removed.
	// These are silently ignored by json.Unmarshal but may indicate stale config.
	warnDeprecatedMergeQueueKeys(data, path)

	return &settings, nil
}

// DeprecatedMergeQueueKeys lists merge_queue config keys that have been removed.
// target_branch and integration_branches were replaced by rig default_branch
// and per-epic integration branch metadata.
var DeprecatedMergeQueueKeys = []string{"target_branch", "integration_branches"}

// warnDeprecatedMergeQueueKeys checks raw settings JSON for removed merge_queue keys
// and prints a stderr warning. This is advisory only — not a validation error.
func warnDeprecatedMergeQueueKeys(data []byte, path string) {
	var raw struct {
		MergeQueue map[string]json.RawMessage `json:"merge_queue"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || raw.MergeQueue == nil {
		return
	}
	for _, key := range DeprecatedMergeQueueKeys {
		if _, ok := raw.MergeQueue[key]; ok {
			fmt.Fprintf(os.Stderr, "warning: %s: merge_queue.%s is deprecated and ignored (use rig default_branch instead)\n", path, key)
		}
	}
}

// SaveRigSettings saves rig settings to a file.
func SaveRigSettings(path string, settings *RigSettings) error {
	if err := validateRigSettings(settings); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding settings: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil { //nolint:gosec // G306: settings files don't contain secrets
		return fmt.Errorf("writing settings: %w", err)
	}

	return nil
}

// LoadMayorConfig loads and validates a mayor config file.
func LoadMayorConfig(path string) (*MayorConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var config MayorConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := validateMayorConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveMayorConfig saves a mayor config to a file.
func SaveMayorConfig(path string, config *MayorConfig) error {
	if err := validateMayorConfig(config); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil { //nolint:gosec // G306: config files don't contain secrets
		return fmt.Errorf("writing config: %w", err)
	}

	return nil
}

// validateMayorConfig validates a MayorConfig.
func validateMayorConfig(c *MayorConfig) error {
	if c.Type != "mayor-config" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'mayor-config', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentMayorConfigVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentMayorConfigVersion)
	}
	return nil
}

// NewMayorConfig creates a new MayorConfig with defaults.
func NewMayorConfig() *MayorConfig {
	return &MayorConfig{
		Type:    "mayor-config",
		Version: CurrentMayorConfigVersion,
	}
}

// DaemonPatrolConfigPath returns the path to the daemon patrol config file.
func DaemonPatrolConfigPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirMayor, DaemonPatrolConfigFileName)
}

// LoadDaemonPatrolConfig loads and validates a daemon patrol config file.
func LoadDaemonPatrolConfig(path string) (*DaemonPatrolConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading daemon patrol config: %w", err)
	}

	var config DaemonPatrolConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing daemon patrol config: %w", err)
	}

	if err := validateDaemonPatrolConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveDaemonPatrolConfig saves a daemon patrol config to a file.
func SaveDaemonPatrolConfig(path string, config *DaemonPatrolConfig) error {
	if err := validateDaemonPatrolConfig(config); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding daemon patrol config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil { //nolint:gosec // G306: config files don't contain secrets
		return fmt.Errorf("writing daemon patrol config: %w", err)
	}

	return nil
}

func validateDaemonPatrolConfig(c *DaemonPatrolConfig) error {
	if c.Type != "daemon-patrol-config" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'daemon-patrol-config', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentDaemonPatrolConfigVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentDaemonPatrolConfigVersion)
	}
	return nil
}

// EnsureDaemonPatrolConfig creates the daemon patrol config if it doesn't exist.
func EnsureDaemonPatrolConfig(townRoot string) error {
	path := DaemonPatrolConfigPath(townRoot)
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("checking daemon patrol config: %w", err)
		}
		return SaveDaemonPatrolConfig(path, NewDaemonPatrolConfig())
	}
	return nil
}

// AddRigToDaemonPatrols adds a rig to the witness and refinery patrol rigs arrays
// in daemon.json. Uses raw JSON manipulation to preserve fields not in PatrolConfig
// (e.g., dolt_server config). If daemon.json doesn't exist, this is a no-op.
func AddRigToDaemonPatrols(townRoot string, rigName string) error {
	return updateDaemonPatrolRigs(townRoot, rigName, true)
}

// RemoveRigFromDaemonPatrols removes a rig from the witness and refinery patrol rigs arrays
// in daemon.json. Uses raw JSON manipulation to preserve fields not in PatrolConfig
// (e.g., dolt_server config). If daemon.json doesn't exist, this is a no-op.
func RemoveRigFromDaemonPatrols(townRoot string, rigName string) error {
	return updateDaemonPatrolRigs(townRoot, rigName, false)
}

func updateDaemonPatrolRigs(townRoot, rigName string, add bool) error {
	path := DaemonPatrolConfigPath(townRoot)
	raw, patrols, ok, err := loadDaemonPatrolData(path)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	modified := false
	for _, patrolName := range []string{"witness", "refinery"} {
		updated, changed, err := updateDaemonPatrol(patrols[patrolName], patrolName, rigName, add)
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		patrols[patrolName] = updated
		modified = true
	}
	if !modified {
		return nil
	}
	return saveDaemonPatrolData(path, raw, patrols)
}

func loadDaemonPatrolData(path string) (map[string]json.RawMessage, map[string]json.RawMessage, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, false, nil
		}
		return nil, nil, false, fmt.Errorf("reading daemon config: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, false, fmt.Errorf("parsing daemon config: %w", err)
	}
	patrolsRaw, ok := raw["patrols"]
	if !ok {
		return raw, nil, false, nil
	}

	var patrols map[string]json.RawMessage
	if err := json.Unmarshal(patrolsRaw, &patrols); err != nil {
		return nil, nil, false, fmt.Errorf("parsing patrols: %w", err)
	}
	return raw, patrols, true, nil
}

func updateDaemonPatrol(pRaw json.RawMessage, patrolName, rigName string, add bool) (json.RawMessage, bool, error) {
	var patrol map[string]json.RawMessage
	if err := json.Unmarshal(pRaw, &patrol); err != nil {
		return nil, false, nil
	}
	rigs := daemonPatrolRigs(patrol["rigs"])
	updatedRigs, changed := updateDaemonRigList(rigs, rigName, add)
	if !changed {
		return nil, false, nil
	}

	rigsJSON, err := json.Marshal(updatedRigs)
	if err != nil {
		return nil, false, fmt.Errorf("encoding rigs: %w", err)
	}
	patrol["rigs"] = rigsJSON
	patrolJSON, err := json.Marshal(patrol)
	if err != nil {
		return nil, false, fmt.Errorf("encoding patrol %s: %w", patrolName, err)
	}
	return patrolJSON, true, nil
}

func daemonPatrolRigs(raw json.RawMessage) []string {
	var rigs []string
	if err := json.Unmarshal(raw, &rigs); err != nil {
		return nil
	}
	return rigs
}

func updateDaemonRigList(rigs []string, rigName string, add bool) ([]string, bool) {
	if add {
		for _, rig := range rigs {
			if rig == rigName {
				return rigs, false
			}
		}
		return append(rigs, rigName), true
	}

	var filtered []string
	for _, rig := range rigs {
		if rig != rigName {
			filtered = append(filtered, rig)
		}
	}
	return filtered, len(filtered) != len(rigs)
}

func saveDaemonPatrolData(path string, raw, patrols map[string]json.RawMessage) error {
	patrolsJSON, err := json.Marshal(patrols)
	if err != nil {
		return fmt.Errorf("encoding patrols: %w", err)
	}
	raw["patrols"] = patrolsJSON

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding daemon config: %w", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0644); err != nil { //nolint:gosec // G306: config file
		return fmt.Errorf("writing daemon config: %w", err)
	}
	return nil
}

// LoadAccountsConfig loads and validates an accounts configuration file.
func LoadAccountsConfig(path string) (*AccountsConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading accounts config: %w", err)
	}

	var config AccountsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing accounts config: %w", err)
	}

	if err := validateAccountsConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveAccountsConfig saves an accounts configuration to a file.
func SaveAccountsConfig(path string, config *AccountsConfig) error {
	if err := validateAccountsConfig(config); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding accounts config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil { //nolint:gosec // G306: accounts config doesn't contain sensitive credentials
		return fmt.Errorf("writing accounts config: %w", err)
	}

	return nil
}

// validateAccountsConfig validates an AccountsConfig.
func validateAccountsConfig(c *AccountsConfig) error {
	if c.Version > CurrentAccountsVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentAccountsVersion)
	}
	if c.Accounts == nil {
		c.Accounts = make(map[string]Account)
	}
	// Validate default refers to an existing account (if set and accounts exist)
	if c.Default != "" && len(c.Accounts) > 0 {
		if _, ok := c.Accounts[c.Default]; !ok {
			return fmt.Errorf("%w: default account '%s' not found in accounts", ErrMissingField, c.Default)
		}
	}
	// Validate each account has required fields
	for handle, acct := range c.Accounts {
		if acct.ConfigDir == "" {
			return fmt.Errorf("%w: config_dir for account '%s'", ErrMissingField, handle)
		}
	}
	return nil
}

// NewAccountsConfig creates a new AccountsConfig with defaults.
func NewAccountsConfig() *AccountsConfig {
	return &AccountsConfig{
		Version:  CurrentAccountsVersion,
		Accounts: make(map[string]Account),
	}
}

// GetAccount returns an account by handle, or nil if not found.
func (c *AccountsConfig) GetAccount(handle string) *Account {
	if acct, ok := c.Accounts[handle]; ok {
		return &acct
	}
	return nil
}

// GetDefaultAccount returns the default account, or nil if not set.
func (c *AccountsConfig) GetDefaultAccount() *Account {
	if c.Default == "" {
		return nil
	}
	return c.GetAccount(c.Default)
}

// ResolveAccountConfigDir resolves the CLAUDE_CONFIG_DIR for account selection.
// Priority order:
//  1. GT_ACCOUNT environment variable
//  2. accountFlag (from --account command flag)
//  3. Default account from config
//
// Returns empty string if no account configured or resolved.
// Returns the handle that was resolved as second value.
func ResolveAccountConfigDir(accountsPath, accountFlag string) (configDir, handle string, err error) {
	// Load accounts config
	cfg, loadErr := LoadAccountsConfig(accountsPath)
	if loadErr != nil {
		// No accounts configured - that's OK, return empty
		return "", "", nil
	}

	// Priority 1: GT_ACCOUNT env var
	if envAccount := os.Getenv("GT_ACCOUNT"); envAccount != "" {
		acct := cfg.GetAccount(envAccount)
		if acct == nil {
			return "", "", fmt.Errorf("GT_ACCOUNT '%s' not found in accounts config", envAccount)
		}
		return expandPath(acct.ConfigDir), envAccount, nil
	}

	// Priority 2: --account flag
	if accountFlag != "" {
		acct := cfg.GetAccount(accountFlag)
		if acct == nil {
			return "", "", fmt.Errorf("account '%s' not found in accounts config", accountFlag)
		}
		return expandPath(acct.ConfigDir), accountFlag, nil
	}

	// Priority 3: Default account
	if cfg.Default != "" {
		acct := cfg.GetDefaultAccount()
		if acct != nil {
			return expandPath(acct.ConfigDir), cfg.Default, nil
		}
	}

	return "", "", nil
}

// expandPath expands ~ to home directory.
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// LoadMessagingConfig loads and validates a messaging configuration file.
func LoadMessagingConfig(path string) (*MessagingConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading messaging config: %w", err)
	}

	var config MessagingConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing messaging config: %w", err)
	}

	if err := validateMessagingConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveMessagingConfig saves a messaging configuration to a file.
func SaveMessagingConfig(path string, config *MessagingConfig) error {
	if err := validateMessagingConfig(config); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding messaging config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil { //nolint:gosec // G306: messaging config doesn't contain secrets
		return fmt.Errorf("writing messaging config: %w", err)
	}

	return nil
}

// validateMessagingConfig validates a MessagingConfig.
func validateMessagingConfig(c *MessagingConfig) error {
	if c.Type != "messaging" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'messaging', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentMessagingVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentMessagingVersion)
	}
	initializeMessagingMaps(c)
	if err := validateMessagingLists(c.Lists); err != nil {
		return err
	}
	if err := validateMessagingQueues(c.Queues); err != nil {
		return err
	}
	if err := validateMessagingAnnounces(c.Announces); err != nil {
		return err
	}
	return validateMessagingNudgeChannels(c.NudgeChannels)
}

func initializeMessagingMaps(c *MessagingConfig) {
	if c.Lists == nil {
		c.Lists = make(map[string][]string)
	}
	if c.Queues == nil {
		c.Queues = make(map[string]QueueConfig)
	}
	if c.Announces == nil {
		c.Announces = make(map[string]AnnounceConfig)
	}
	if c.NudgeChannels == nil {
		c.NudgeChannels = make(map[string][]string)
	}
}

func validateMessagingLists(lists map[string][]string) error {
	for name, recipients := range lists {
		if len(recipients) == 0 {
			return fmt.Errorf("%w: list '%s' has no recipients", ErrMissingField, name)
		}
	}
	return nil
}

func validateMessagingQueues(queues map[string]QueueConfig) error {
	for name, queue := range queues {
		if len(queue.Workers) == 0 {
			return fmt.Errorf("%w: queue '%s' workers", ErrMissingField, name)
		}
		if queue.MaxClaims < 0 {
			return fmt.Errorf("%w: queue '%s' max_claims must be non-negative", ErrMissingField, name)
		}
	}
	return nil
}

func validateMessagingAnnounces(announces map[string]AnnounceConfig) error {
	for name, announce := range announces {
		if len(announce.Readers) == 0 {
			return fmt.Errorf("%w: announce '%s' readers", ErrMissingField, name)
		}
		if announce.RetainCount < 0 {
			return fmt.Errorf("%w: announce '%s' retain_count must be non-negative", ErrMissingField, name)
		}
	}
	return nil
}

func validateMessagingNudgeChannels(channels map[string][]string) error {
	for name, recipients := range channels {
		if name == "" {
			return fmt.Errorf("%w: nudge channel name cannot be empty", ErrMissingField)
		}
		if len(recipients) == 0 {
			return fmt.Errorf("%w: nudge channel '%s' has no recipients", ErrMissingField, name)
		}
	}
	return nil
}

// MessagingConfigPath returns the standard path for messaging config in a town.
func MessagingConfigPath(townRoot string) string {
	return filepath.Join(townRoot, "config", "messaging.json")
}

// LoadOrCreateMessagingConfig loads the messaging config, creating a default if not found.
func LoadOrCreateMessagingConfig(path string) (*MessagingConfig, error) {
	config, err := LoadMessagingConfig(path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return NewMessagingConfig(), nil
		}
		return nil, err
	}
	return config, nil
}

// TownSettingsPath returns the path to town settings file.
func TownSettingsPath(townRoot string) string {
	return filepath.Join(townRoot, "settings", "config.json")
}

// RigSettingsPath returns the path to rig settings file.
func RigSettingsPath(rigPath string) string {
	return filepath.Join(rigPath, "settings", "config.json")
}

// LoadOrCreateTownSettings loads town settings or creates defaults if missing.
func LoadOrCreateTownSettings(path string) (*TownSettings, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		if os.IsNotExist(err) {
			return NewTownSettings(), nil
		}
		return nil, err
	}

	var settings TownSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	return &settings, nil
}

// SaveTownSettings saves town settings to a file.
func SaveTownSettings(path string, settings *TownSettings) error {
	if settings.Type != "town-settings" && settings.Type != "" {
		return fmt.Errorf("%w: expected type 'town-settings', got '%s'", ErrInvalidType, settings.Type)
	}
	if settings.Version > CurrentTownSettingsVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, settings.Version, CurrentTownSettingsVersion)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding settings: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil { //nolint:gosec // G306: settings files don't contain secrets
		return fmt.Errorf("writing settings: %w", err)
	}

	return nil
}

// ResolveAgentConfig resolves the agent configuration for a rig.
// It looks up the agent by name in town settings (custom agents) and built-in presets.
//
// Resolution order:
//  1. If rig has Runtime set directly, use it (backwards compatibility)
//  2. If rig has Agent set, look it up in:
//     a. Town's custom agents (from TownSettings.Agents)
//     b. Built-in presets (claude, gemini, codex)
//  3. If rig has no Agent set, use town's default_agent
//  4. Fall back to claude defaults
//
// townRoot is the path to the town directory (e.g., ~/gt).
// rigPath is the path to the rig directory (e.g., ~/gt/gastown).
func ResolveAgentConfig(townRoot, rigPath string) *RuntimeConfig {
	resolveConfigMu.Lock()
	defer resolveConfigMu.Unlock()
	return resolveAgentConfigInternal(townRoot, rigPath)
}

// resolveAgentConfigInternal is the lock-free version of ResolveAgentConfig.
// Caller must hold resolveConfigMu.
func resolveAgentConfigInternal(townRoot, rigPath string) *RuntimeConfig {
	// Load rig settings
	rigSettings, err := LoadRigSettings(RigSettingsPath(rigPath))
	if err != nil {
		rigSettings = nil
	}

	// Backwards compatibility: if Runtime is set directly, use it
	if rigSettings != nil && rigSettings.Runtime != nil {
		rc := fillRuntimeDefaults(rigSettings.Runtime)
		if rc.ResolvedAgent == "" {
			rc.ResolvedAgent = inferAgentName(rc)
		}
		return rc
	}

	// Load town settings for agent lookup
	townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		townSettings = NewTownSettings()
	}

	// Load custom agent registry if it exists
	_ = LoadAgentRegistry(DefaultAgentRegistryPath(townRoot))

	// Load rig-level custom agent registry if it exists (for per-rig custom agents)
	_ = LoadRigAgentRegistry(RigAgentRegistryPath(rigPath))

	// Determine which agent name to use
	agentName := ""
	if rigSettings != nil && rigSettings.Agent != "" {
		agentName = rigSettings.Agent
	} else if townSettings.DefaultAgent != "" {
		agentName = townSettings.DefaultAgent
	} else {
		agentName = "claude" // ultimate fallback
	}

	rc := lookupAgentConfig(agentName, townSettings, rigSettings)
	rc.ResolvedAgent = agentName
	return rc
}

// ResolveAgentConfigWithOverride resolves the agent configuration for a rig, with an optional override.
// If agentOverride is non-empty, it is used instead of rig/town defaults.
// Returns the resolved RuntimeConfig, the selected agent name, and an error if the override name
// does not exist in town custom agents or built-in presets.
func ResolveAgentConfigWithOverride(townRoot, rigPath, agentOverride string) (*RuntimeConfig, string, error) {
	resolveConfigMu.Lock()
	defer resolveConfigMu.Unlock()
	return resolveAgentConfigWithOverrideInternal(townRoot, rigPath, agentOverride)
}

// resolveAgentConfigWithOverrideInternal is the lock-free version.
// Caller must hold resolveConfigMu.
func resolveAgentConfigWithOverrideInternal(townRoot, rigPath, agentOverride string) (*RuntimeConfig, string, error) {
	rigSettings := loadRigSettingsOrNil(rigPath)

	// Backwards compatibility: if Runtime is set directly, use it (but still report agentOverride if present)
	if rigSettings != nil && rigSettings.Runtime != nil && agentOverride == "" {
		rc := fillRuntimeDefaults(rigSettings.Runtime)
		if rc.ResolvedAgent == "" {
			rc.ResolvedAgent = inferAgentName(rc)
		}
		return rc, "", nil
	}

	townSettings := loadTownSettingsOrDefault(townRoot)
	loadAgentRegistries(townRoot, rigPath)
	agentName, extraArgs := resolveAgentName(agentOverride, rigSettings, townSettings)

	// If an override is requested, validate it exists
	if agentOverride != "" {
		rc := resolveAgentOverride(agentName, extraArgs, townSettings, rigSettings)
		if rc == nil {
			return nil, "", fmt.Errorf("agent '%s' not found", agentName)
		}

		rc.ResolvedAgent = agentName

		// Append extra arguments from the override
		if len(extraArgs) > 0 {
			rc.Args = append(rc.Args, extraArgs...)
		}
		return rc, agentName, nil
	}

	// Normal lookup path (no override)
	rc := lookupAgentConfig(agentName, townSettings, rigSettings)
	rc.ResolvedAgent = agentName

	// If we have extra arguments from the override, append them to the config
	if len(extraArgs) > 0 {
		rc.Args = append(rc.Args, extraArgs...)
	}

	return rc, agentName, nil
}

func loadRigSettingsOrNil(rigPath string) *RigSettings {
	if rigPath == "" {
		return nil
	}
	rigSettings, err := LoadRigSettings(RigSettingsPath(rigPath))
	if err != nil {
		return nil
	}
	return rigSettings
}

func loadTownSettingsOrDefault(townRoot string) *TownSettings {
	townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		return NewTownSettings()
	}
	return townSettings
}

func loadAgentRegistries(townRoot, rigPath string) {
	_ = LoadAgentRegistry(DefaultAgentRegistryPath(townRoot))
	if rigPath != "" {
		_ = LoadRigAgentRegistry(RigAgentRegistryPath(rigPath))
	}
}

func resolveAgentName(agentOverride string, rigSettings *RigSettings, townSettings *TownSettings) (string, []string) {
	if agentOverride != "" {
		return parseAgentOverride(agentOverride)
	}
	if rigSettings != nil && rigSettings.Agent != "" {
		return rigSettings.Agent, nil
	}
	if townSettings.DefaultAgent != "" {
		return townSettings.DefaultAgent, nil
	}
	return "claude", nil
}

func parseAgentOverride(agentOverride string) (string, []string) {
	parts := strings.Fields(agentOverride)
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
}

func resolveAgentOverride(agentName string, extraArgs []string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	if len(extraArgs) > 0 {
		if preset, ok := builtinPresets[AgentPreset(agentName)]; ok {
			return runtimeConfigFromAgentInfo(AgentPreset(agentName), preset)
		}
	}
	if rc := lookupCustomAgentConfig(agentName, townSettings, rigSettings); rc != nil {
		return rc
	}
	if GetAgentPresetByName(agentName) != nil {
		return RuntimeConfigFromPreset(AgentPreset(agentName))
	}
	return nil
}

// ValidateAgentConfig checks if an agent configuration is valid and the binary exists.
// Returns an error describing the issue, or nil if valid.
func ValidateAgentConfig(agentName string, townSettings *TownSettings, rigSettings *RigSettings) error {
	// Check if agent exists in config
	rc := lookupAgentConfigIfExists(agentName, townSettings, rigSettings)
	if rc == nil {
		return fmt.Errorf("agent %q not found in config or built-in presets", agentName)
	}

	// Check if binary exists on system
	if _, err := exec.LookPath(rc.Command); err != nil {
		return fmt.Errorf("agent %q binary %q not found in PATH", agentName, rc.Command)
	}

	return nil
}

// lookupAgentConfigIfExists looks up an agent by name but returns nil if not found
// (instead of falling back to default). Used for validation.
func lookupAgentConfigIfExists(name string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	if rigSettings != nil {
		if rc := lookupCustomAgentInSettings(name, rigSettings.Agents); rc != nil {
			return rc
		}
	}
	if townSettings != nil {
		if rc := lookupCustomAgentInSettings(name, townSettings.Agents); rc != nil {
			return rc
		}
	}
	if preset := GetAgentPresetByName(name); preset != nil {
		return RuntimeConfigFromPreset(AgentPreset(name))
	}

	return nil
}

func lookupCustomAgentInSettings(name string, agents map[string]*RuntimeConfig) *RuntimeConfig {
	if custom, ok := agents[name]; ok && custom != nil {
		return fillRuntimeDefaults(custom)
	}
	return nil
}

// ResolveRoleAgentConfig resolves the agent configuration for a specific role.
// It checks role-specific agent assignments before falling back to the default agent.
//
// Resolution order:
//  1. Rig's RoleAgents[role] - if set, look up that agent
//  2. Town's RoleAgents[role] - if set, look up that agent
//  3. Fall back to ResolveAgentConfig (rig's Agent → town's DefaultAgent → "claude")
//
// If a configured agent is not found or its binary doesn't exist, a warning is
// printed to stderr and it falls back to the default agent.
//
// role is one of: "mayor", "deacon", "witness", "refinery", "polecat", "crew", "boot".
// townRoot is the path to the town directory (e.g., ~/gt).
// rigPath is the path to the rig directory (e.g., ~/gt/gastown), or empty for town-level roles.
func ResolveRoleAgentConfig(role, townRoot, rigPath string) *RuntimeConfig {
	resolveConfigMu.Lock()
	defer resolveConfigMu.Unlock()
	rc := resolveRoleAgentConfigCore(role, townRoot, rigPath)
	return withRoleSettingsFlag(rc, role, rigPath)
}

// tryResolveNamedAgent attempts to resolve a named agent through the custom agent
// and standard lookup pipelines. Returns the resolved config with ResolvedAgent set,
// or nil if validation fails. The warnPrefix is used in the fallback warning message
// (e.g., "worker_agents[denali]" or "crew_agents[denali]").
func tryResolveNamedAgent(agentName, warnPrefix string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	if rc := lookupCustomAgentConfig(agentName, townSettings, rigSettings); rc != nil {
		rc.ResolvedAgent = agentName
		return rc
	}
	if err := ValidateAgentConfig(agentName, townSettings, rigSettings); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s=%s - %v, falling back\n", warnPrefix, agentName, err)
		return nil
	}
	rc := lookupAgentConfig(agentName, townSettings, rigSettings)
	rc.ResolvedAgent = agentName
	return rc
}

// ResolveWorkerAgentConfig resolves the agent configuration for a named crew worker.
// Resolution order:
//  1. Rig's WorkerAgents[workerName] — per-worker override
//  2. Town's CrewAgents[workerName] — town-wide per-crew override
//  3. Falls back to ResolveRoleAgentConfig("crew", ...) for remaining resolution
//
// workerName is the crew member name (e.g., "denali").
func ResolveWorkerAgentConfig(workerName, townRoot, rigPath string) *RuntimeConfig {
	resolveConfigMu.Lock()
	defer resolveConfigMu.Unlock()

	if rc := resolveRigWorkerAgent(workerName, townRoot, rigPath); rc != nil {
		return rc
	}

	if rc := resolveTownWorkerAgent(workerName, townRoot, rigPath); rc != nil {
		return rc
	}

	rc := resolveRoleAgentConfigCore("crew", townRoot, rigPath)
	return withRoleSettingsFlag(rc, "crew", rigPath)
}

func resolveRigWorkerAgent(workerName, townRoot, rigPath string) *RuntimeConfig {
	if workerName == "" || rigPath == "" {
		return nil
	}
	rigSettings, err := LoadRigSettings(RigSettingsPath(rigPath))
	if err != nil || rigSettings == nil {
		return nil
	}
	agentName := rigSettings.WorkerAgents[workerName]
	if agentName == "" {
		return nil
	}
	townSettings := loadTownSettingsOrDefault(townRoot)
	loadAgentRegistries(townRoot, rigPath)
	rc := tryResolveNamedAgent(agentName, fmt.Sprintf("worker_agents[%s]", workerName), townSettings, rigSettings)
	if rc == nil {
		return nil
	}
	return withRoleSettingsFlag(rc, "crew", rigPath)
}

func resolveTownWorkerAgent(workerName, townRoot, rigPath string) *RuntimeConfig {
	if workerName == "" || townRoot == "" {
		return nil
	}
	townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil || townSettings == nil {
		return nil
	}
	agentName := townSettings.CrewAgents[workerName]
	if agentName == "" {
		return nil
	}
	rigSettings := loadRigSettingsOrNil(rigPath)
	loadAgentRegistries(townRoot, rigPath)
	rc := tryResolveNamedAgent(agentName, fmt.Sprintf("crew_agents[%s]", workerName), townSettings, rigSettings)
	if rc == nil {
		return nil
	}
	return withRoleSettingsFlag(rc, "crew", rigPath)
}

// ResolveRoleEffort resolves the effort level for a role.
// Resolution order:
//  1. Rig's RoleEffort[role]
//  2. Town's RoleEffort[role]
//  3. Returns "" (caller falls back to env var / default "high")
//
// Invalid effort levels are warned about and skipped.
func ResolveRoleEffort(role, townRoot, rigPath string) string {
	if effort, ok := resolveEphemeralRoleEffort(role); ok {
		return effort
	}
	return resolvePersistedRoleEffort(role, townRoot, rigPath)
}

func resolveEphemeralRoleEffort(role string) (string, bool) {
	tierName := os.Getenv("GT_COST_TIER")
	if tierName == "" || !IsValidTier(tierName) {
		return "", false
	}
	roleEffort := CostTierRoleEffort(CostTier(tierName))
	if effort, ok := roleEffort[role]; ok {
		return effort, true
	}
	return "", false
}

func resolvePersistedRoleEffort(role, townRoot, rigPath string) string {
	if effort := resolveRigRoleEffort(role, rigPath); effort != "" {
		return effort
	}
	if effort := resolveTownRoleEffort(role, townRoot); effort != "" {
		return effort
	}
	return ""
}

func resolveRigRoleEffort(role, rigPath string) string {
	if rigPath == "" {
		return ""
	}
	rigSettings, err := LoadRigSettings(RigSettingsPath(rigPath))
	if err != nil || rigSettings == nil {
		return ""
	}
	effort := rigSettings.RoleEffort[role]
	if effort == "" {
		return ""
	}
	if !IsValidEffortLevel(effort) {
		fmt.Fprintf(os.Stderr, "warning: rig role_effort[%s]=%q is not a valid effort level, ignoring\n", role, effort)
		return ""
	}
	return effort
}

func resolveTownRoleEffort(role, townRoot string) string {
	if townRoot == "" {
		return ""
	}
	townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil || townSettings == nil {
		return ""
	}
	effort := townSettings.RoleEffort[role]
	if effort == "" {
		return ""
	}
	if !IsValidEffortLevel(effort) {
		fmt.Fprintf(os.Stderr, "warning: town role_effort[%s]=%q is not a valid effort level, ignoring\n", role, effort)
		return ""
	}
	return effort
}

// IsResolvedAgentClaude returns true if the RuntimeConfig represents a Claude agent.
// Exported for use in witness/daemon code that needs to skip hardcoded
// Claude start commands when a non-Claude agent is configured.
func IsResolvedAgentClaude(rc *RuntimeConfig) bool {
	if rc == nil {
		return true // Default to Claude when config is unavailable
	}
	return isClaudeAgent(rc)
}

// isClaudeAgent returns true if the RuntimeConfig represents a Claude agent.
// When Provider is explicitly set, it's authoritative. When empty, the Command
// is checked: bare "claude", a path ending in "/claude" (or "\claude" on Windows),
// or an empty command (the default) all indicate Claude.
func isClaudeAgent(rc *RuntimeConfig) bool {
	if rc.Provider != "" {
		return rc.Provider == "claude"
	}
	if rc.Command == "" || rc.Command == "claude" {
		return true
	}
	base := filepath.Base(rc.Command)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return base == "claude"
}

// applyRoleEffortToRuntime translates the provider-neutral role_effort setting
// into the runtime's native controls. Claude consumes the corresponding
// CLAUDE_CODE_EFFORT_LEVEL environment variable from AgentEnv. Pi uses an
// explicit --thinking flag, so apply it to the resolved command here.
func applyRoleEffortToRuntime(rc *RuntimeConfig, effort string) {
	if rc == nil || effort == "" || !isPiAgent(rc) {
		return
	}

	var args []string
	argCount := len(rc.Args)
	for i := 0; i < argCount; i++ {
		arg := rc.Args[i]
		if arg == "--thinking" {
			if i+1 < argCount && !strings.HasPrefix(rc.Args[i+1], "-") {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--thinking=") {
			continue
		}
		args = append(args, arg)
	}
	rc.Args = append(args, "--thinking", effort)
}

func isPiAgent(rc *RuntimeConfig) bool {
	if rc.Provider != "" {
		return rc.Provider == string(AgentPi)
	}
	base := filepath.Base(rc.Command)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return base == "pi"
}

// withRoleSettingsFlag appends --settings to the Args for Claude agents whose
// settings directory differs from the session working directory. Claude Code
// resolves project-level settings from its working directory only; the --settings
// flag tells it where to find them when they live in a parent directory.
func withRoleSettingsFlag(rc *RuntimeConfig, role, rigPath string) *RuntimeConfig {
	if rc == nil || rigPath == "" {
		return rc
	}

	if !isClaudeAgent(rc) {
		return rc
	}

	if hasSettingsArgument(rc.Args) {
		return rc
	}

	settingsDir := RoleSettingsDir(role, rigPath)
	if settingsDir == "" {
		return rc
	}

	hooksDir, settingsFile := roleSettingsComponents(rc)
	rc.Args = append(rc.Args, "--settings", filepath.Join(settingsDir, hooksDir, settingsFile))
	return rc
}

func hasSettingsArgument(args []string) bool {
	for _, arg := range args {
		if arg == "--settings" {
			return true
		}
	}
	return false
}

func roleSettingsComponents(rc *RuntimeConfig) (string, string) {
	hooksDir := ".claude"
	settingsFile := "settings.json"
	if rc.Hooks == nil {
		return hooksDir, settingsFile
	}
	if rc.Hooks.Dir != "" {
		hooksDir = rc.Hooks.Dir
	}
	if rc.Hooks.SettingsFile != "" {
		settingsFile = rc.Hooks.SettingsFile
	}
	return hooksDir, settingsFile
}

// RoleSettingsDir returns the shared settings directory for roles whose session
// working directory differs from their settings location. Returns empty for
// roles where settings and session directory are the same (mayor, deacon).
func RoleSettingsDir(role, rigPath string) string {
	switch role {
	case constants.RoleCrew, constants.RoleWitness, constants.RoleRefinery:
		return filepath.Join(rigPath, role)
	case constants.RolePolecat:
		return filepath.Join(rigPath, "polecats")
	default:
		return ""
	}
}

// tryResolveFromEphemeralTier checks the GT_COST_TIER environment variable
// and returns the appropriate RuntimeConfig for the given role if an ephemeral
// cost tier is set.
//
// Returns:
//   - (rc, true)  — tier is active and has spoken for this role. rc may be nil
//     if the tier says "use default" (empty agent mapping).
//   - (nil, false) — no ephemeral tier active, or role is not tier-managed.
//
// The caller must respect handled=true even when rc is nil: it means the tier
// explicitly wants the default agent for this role, and persisted RoleAgents
// should be skipped to prevent stale config from leaking through.
func tryResolveFromEphemeralTier(role string) (*RuntimeConfig, bool) {
	tierName := os.Getenv("GT_COST_TIER")
	if tierName == "" || !IsValidTier(tierName) {
		return nil, false
	}

	tier := CostTier(tierName)
	roleAgents := CostTierRoleAgents(tier)
	if roleAgents == nil {
		return nil, false
	}

	agentName, ok := roleAgents[role]
	if !ok {
		return nil, false // Role not managed by tiers
	}

	// Empty agent name means "use default (opus)" — signal handled but no override
	if agentName == "" {
		return nil, true
	}

	// Look up the agent config from the tier's agent definitions
	agents := CostTierAgents(tier)
	if agents != nil {
		if rc, found := agents[agentName]; found && rc != nil {
			filled := fillRuntimeDefaults(rc)
			filled.ResolvedAgent = agentName
			return filled, true
		}
	}

	return nil, false
}

// hasExplicitNonClaudeOverride checks if there is an explicit non-Claude agent
// assignment either specifically for the role (in rig or town RoleAgents) or
// globally (via rig Agent or town DefaultAgent). This prevents fallback logic
// and cost tiers from silently replacing intentional non-Claude agent selections.
func hasExplicitNonClaudeOverride(role string, townSettings *TownSettings, rigSettings *RigSettings) bool {
	if rigSettings != nil && hasNonClaudeRoleAgent(role, rigSettings.RoleAgents, townSettings, rigSettings) {
		return true
	}
	if townSettings != nil && hasNonClaudeRoleAgent(role, townSettings.RoleAgents, townSettings, rigSettings) {
		return true
	}
	if rigSettings != nil && hasNonClaudeAgent(rigSettings.Agent, townSettings, rigSettings) {
		return true
	}
	if townSettings != nil && hasNonClaudeAgent(townSettings.DefaultAgent, townSettings, rigSettings) {
		return true
	}
	return false
}

func hasNonClaudeRoleAgent(role string, roleAgents map[string]string, townSettings *TownSettings, rigSettings *RigSettings) bool {
	agentName := roleAgents[role]
	return hasNonClaudeAgent(agentName, townSettings, rigSettings)
}

func hasNonClaudeAgent(agentName string, townSettings *TownSettings, rigSettings *RigSettings) bool {
	if agentName == "" {
		return false
	}
	rc := lookupAgentConfigIfExists(agentName, townSettings, rigSettings)
	return rc != nil && !isClaudeAgent(rc)
}

func resolveRoleAgentConfigCore(role, townRoot, rigPath string) *RuntimeConfig {
	rigSettings := loadRigSettingsOrNil(rigPath)
	townSettings := loadTownSettingsOrDefault(townRoot)
	loadAgentRegistries(townRoot, rigPath)

	// Dogs default to Haiku (cheap infrastructure workers), but respect
	// explicit non-Claude overrides (e.g., RoleAgents["dog"] = "opencode").
	if role == "dog" && !hasExplicitNonClaudeOverride(role, townSettings, rigSettings) {
		return claudeHaikuPreset()
	}

	tierRC, tierHandled := tryResolveFromEphemeralTier(role)
	if tierHandled && !hasExplicitNonClaudeOverride(role, townSettings, rigSettings) {
		if tierRC != nil {
			return tierRC
		}
		return resolveAgentConfigInternal(townRoot, rigPath)
	}

	if rc := resolveRoleAgentFromSettings(role, townSettings, rigSettings); rc != nil {
		return rc
	}

	return resolveAgentConfigInternal(townRoot, rigPath)
}

func resolveRoleAgentFromSettings(role string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	if rigSettings != nil {
		if rc := resolveRoleAgentFromMap(role, rigSettings.RoleAgents, townSettings, rigSettings); rc != nil {
			return rc
		}
	}
	if townSettings != nil {
		if rc := resolveRoleAgentFromMap(role, townSettings.RoleAgents, townSettings, rigSettings); rc != nil {
			return rc
		}
	}
	return nil
}

func resolveRoleAgentFromMap(role string, roleAgents map[string]string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	agentName := roleAgents[role]
	if agentName == "" {
		return nil
	}
	if rc := lookupCustomAgentConfig(agentName, townSettings, rigSettings); rc != nil {
		rc.ResolvedAgent = agentName
		return rc
	}
	if err := ValidateAgentConfig(agentName, townSettings, rigSettings); err != nil {
		fmt.Fprintf(os.Stderr, "warning: role_agents[%s]=%s - %v, falling back to default\n", role, agentName, err)
		return nil
	}
	rc := lookupAgentConfig(agentName, townSettings, rigSettings)
	rc.ResolvedAgent = agentName
	return rc
}

// ResolveRoleAgentName returns the agent name that would be used for a specific role.
// This is useful for logging and diagnostics.
// Returns the agent name and whether it came from role-specific configuration.
//
// NOTE: This function does not account for ephemeral cost tier overrides
// (GT_COST_TIER env var). It reflects persisted config only. For the actual
// runtime agent config, use ResolveRoleAgentConfig.
func ResolveRoleAgentName(role, townRoot, rigPath string) (agentName string, isRoleSpecific bool) {
	rigSettings := loadRigSettingsOrNil(rigPath)
	townSettings := loadTownSettingsOrDefault(townRoot)
	if name, ok := roleSpecificAgentName(role, rigSettings, townSettings); ok {
		return name, true
	}
	return defaultAgentName(rigSettings, townSettings), false
}

func roleSpecificAgentName(role string, rigSettings *RigSettings, townSettings *TownSettings) (string, bool) {
	if rigSettings != nil {
		if name := rigSettings.RoleAgents[role]; name != "" {
			return name, true
		}
	}
	if townSettings != nil {
		if name := townSettings.RoleAgents[role]; name != "" {
			return name, true
		}
	}
	return "", false
}

func defaultAgentName(rigSettings *RigSettings, townSettings *TownSettings) string {
	if rigSettings != nil && rigSettings.Agent != "" {
		return rigSettings.Agent
	}
	if townSettings != nil && townSettings.DefaultAgent != "" {
		return townSettings.DefaultAgent
	}
	return "claude"
}

// ResolveAgentConfigByName looks up an agent's RuntimeConfig by name without requiring
// the agent binary to be installed. Checks custom agents first, then built-in presets.
// Returns nil if the agent name is unknown. Used by hooks sync, which needs the preset's
// hooks metadata regardless of whether the binary is installed on this machine.
func ResolveAgentConfigByName(name, townRoot, rigPath string) *RuntimeConfig {
	resolveConfigMu.Lock()
	defer resolveConfigMu.Unlock()

	var rigSettings *RigSettings
	if rigPath != "" {
		if rs, err := LoadRigSettings(RigSettingsPath(rigPath)); err == nil {
			rigSettings = rs
		}
	}

	townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		townSettings = NewTownSettings()
	}

	_ = LoadAgentRegistry(DefaultAgentRegistryPath(townRoot))
	if rigPath != "" {
		_ = LoadRigAgentRegistry(RigAgentRegistryPath(rigPath))
	}

	return lookupAgentConfigIfExists(name, townSettings, rigSettings)
}

// HasExplicitRoleAgent returns true if role_agents (rig or town level)
// explicitly maps this role to a named agent. This distinguishes between
// "role_agents says use claude-sonnet" and "no role_agents entry, falling
// back to defaults". When an explicit mapping exists, the TOML start_command
// should be skipped in favor of BuildStartupCommandFromConfig which honors
// the model/settings from the mapped agent definition.
func HasExplicitRoleAgent(role, townRoot, rigPath string) bool {
	_, isRoleSpecific := ResolveRoleAgentName(role, townRoot, rigPath)
	return isRoleSpecific
}

// lookupAgentConfig looks up an agent by name.
// Checks rig-level custom agents first, then town's custom agents, then built-in presets from agents.go.
// Falls back to DefaultRuntimeConfig() if no match is found.
func lookupAgentConfig(name string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	if rc := lookupAgentConfigIfExists(name, townSettings, rigSettings); rc != nil {
		return rc
	}
	return DefaultRuntimeConfig()
}

// lookupCustomAgentConfig looks up custom agents only (rig or town).
// It skips binary validation so tests and config resolution can proceed
// even if the command isn't on PATH yet.
func lookupCustomAgentConfig(name string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	if rigSettings != nil && rigSettings.Agents != nil {
		if custom, ok := rigSettings.Agents[name]; ok && custom != nil {
			return fillRuntimeDefaults(custom)
		}
	}

	if townSettings != nil && townSettings.Agents != nil {
		if custom, ok := townSettings.Agents[name]; ok && custom != nil {
			return fillRuntimeDefaults(custom)
		}
	}

	return nil
}

// fillRuntimeDefaults fills in default values for empty RuntimeConfig fields.
// It creates a deep copy to prevent mutation of the original config.
//
// Default behavior:
//   - Command defaults to "claude" if empty
//   - Args defaults to ["--dangerously-skip-permissions"] if nil
//   - Empty Args slice ([]string{}) means "no args" and is preserved as-is
//
// All fields are deep-copied: modifying the returned config will not affect
// the input config, including nested structs and slices.
func fillRuntimeDefaults(rc *RuntimeConfig) *RuntimeConfig {
	if rc == nil {
		return DefaultRuntimeConfig()
	}

	result := cloneRuntimeConfig(rc)
	preset := runtimePresetFor(result)
	applyRuntimeCommandDefaults(result, preset)
	applyRuntimePresetDefaults(result, preset)
	return result
}

func cloneRuntimeConfig(rc *RuntimeConfig) *RuntimeConfig {
	result := &RuntimeConfig{
		Provider:      rc.Provider,
		Command:       rc.Command,
		InitialPrompt: rc.InitialPrompt,
		PromptMode:    rc.PromptMode,
		ResolvedAgent: rc.ResolvedAgent,
	}
	cloneRuntimeSlices(result, rc)
	cloneRuntimeNestedConfigs(result, rc)
	return result
}

func cloneRuntimeSlices(result, rc *RuntimeConfig) {
	if rc.Args != nil {
		result.Args = cloneStringSlice(rc.Args)
	}
	if rc.ExecWrapper != nil {
		result.ExecWrapper = cloneStringSlice(rc.ExecWrapper)
	}
	if len(rc.Env) > 0 {
		result.Env = make(map[string]string, len(rc.Env))
		for key, value := range rc.Env {
			result.Env[key] = value
		}
	}
}

func cloneRuntimeNestedConfigs(result, rc *RuntimeConfig) {
	if rc.Session != nil {
		result.Session = &RuntimeSessionConfig{
			SessionIDEnv: rc.Session.SessionIDEnv,
			ConfigDirEnv: rc.Session.ConfigDirEnv,
		}
	}
	if rc.Hooks != nil {
		result.Hooks = &RuntimeHooksConfig{
			Provider:     rc.Hooks.Provider,
			Dir:          rc.Hooks.Dir,
			SettingsFile: rc.Hooks.SettingsFile,
		}
	}
	if rc.Tmux != nil {
		result.Tmux = cloneRuntimeTmux(rc.Tmux)
	}
	if rc.Instructions != nil {
		result.Instructions = &RuntimeInstructionsConfig{File: rc.Instructions.File}
	}
	if rc.ACP != nil {
		result.ACP = cloneACPConfig(rc.ACP)
	}
}

func cloneRuntimeTmux(tmux *RuntimeTmuxConfig) *RuntimeTmuxConfig {
	return &RuntimeTmuxConfig{
		ProcessNames:      cloneStringSlice(tmux.ProcessNames),
		ReadyPromptPrefix: tmux.ReadyPromptPrefix,
		ReadyDelayMs:      tmux.ReadyDelayMs,
	}
}

func cloneACPConfig(acp *ACPConfig) *ACPConfig {
	return &ACPConfig{
		Mode:    acp.Mode,
		Command: acp.Command,
		Args:    cloneStringSlice(acp.Args),
	}
}

func cloneStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func runtimePresetFor(result *RuntimeConfig) *AgentPresetInfo {
	presetName := result.Provider
	if presetName == "" && result.Command != "" {
		presetName = result.Command
	}
	preset := GetAgentPresetByName(presetName)
	if preset == nil {
		preset = GetAgentPreset(AgentClaude)
	}
	return preset
}

func applyRuntimeCommandDefaults(result *RuntimeConfig, preset *AgentPresetInfo) {
	if result.Command == "" && preset != nil {
		result.Command = preset.Command
	}
	if result.Args == nil && preset != nil {
		result.Args = append([]string(nil), preset.Args...)
	}
	if preset != nil {
		result.Args = ensureRequiredArgGroups(result.Args, preset.RequiredArgGroups)
	}
	result.Args = ensureCodexAutomationArgs(result.Command, result.Args)
}

func applyRuntimePresetDefaults(result *RuntimeConfig, preset *AgentPresetInfo) {
	if preset == nil {
		return
	}
	applyRuntimeHooksDefaults(result, preset)
	applyRuntimeSessionDefaults(result, preset)
	applyRuntimeTmuxDefaults(result, preset)
	applyRuntimePromptDefaults(result, preset)
	applyRuntimeInstructionsDefaults(result, preset)
	applyRuntimeEnvDefaults(result, preset)
	applyRuntimeACPDefaults(result, preset)
	applyRuntimeNonInteractiveDefaults(result, preset)
}

func applyRuntimeHooksDefaults(result *RuntimeConfig, preset *AgentPresetInfo) {
	if result.Hooks == nil && preset.HooksProvider != "" {
		result.Hooks = &RuntimeHooksConfig{
			Provider:     preset.HooksProvider,
			Dir:          preset.HooksDir,
			SettingsFile: preset.HooksSettingsFile,
		}
	}
}

func applyRuntimeSessionDefaults(result *RuntimeConfig, preset *AgentPresetInfo) {
	if result.Session == nil && (preset.SessionIDEnv != "" || preset.ConfigDirEnv != "") {
		result.Session = &RuntimeSessionConfig{
			SessionIDEnv: preset.SessionIDEnv,
			ConfigDirEnv: preset.ConfigDirEnv,
		}
	}
}

func applyRuntimeTmuxDefaults(result *RuntimeConfig, preset *AgentPresetInfo) {
	if result.Tmux == nil && (len(preset.ProcessNames) > 0 || preset.ReadyPromptPrefix != "" || preset.ReadyDelayMs > 0) {
		result.Tmux = &RuntimeTmuxConfig{
			ProcessNames:      append([]string(nil), preset.ProcessNames...),
			ReadyPromptPrefix: preset.ReadyPromptPrefix,
			ReadyDelayMs:      preset.ReadyDelayMs,
		}
	}
}

func applyRuntimePromptDefaults(result *RuntimeConfig, preset *AgentPresetInfo) {
	if result.PromptMode == "" && preset.PromptMode != "" {
		result.PromptMode = preset.PromptMode
	}
}

func applyRuntimeInstructionsDefaults(result *RuntimeConfig, preset *AgentPresetInfo) {
	if result.Instructions == nil && preset.InstructionsFile != "" {
		result.Instructions = &RuntimeInstructionsConfig{File: preset.InstructionsFile}
	}
}

func applyRuntimeEnvDefaults(result *RuntimeConfig, preset *AgentPresetInfo) {
	if len(preset.Env) == 0 {
		return
	}
	if result.Env == nil {
		result.Env = make(map[string]string)
	}
	for key, value := range preset.Env {
		if _, ok := result.Env[key]; !ok {
			result.Env[key] = value
		}
	}
}

func applyRuntimeACPDefaults(result *RuntimeConfig, preset *AgentPresetInfo) {
	if result.ACP == nil && preset.ACP != nil {
		result.ACP = cloneACPConfig(preset.ACP)
	}
}

func applyRuntimeNonInteractiveDefaults(result *RuntimeConfig, preset *AgentPresetInfo) {
	if result.NonInteractive == nil && preset.NonInteractive != nil {
		ni := *preset.NonInteractive
		result.NonInteractive = &ni
	}
}

// inferAgentName determines the agent name from a legacy RuntimeConfig.
// It mirrors the preset resolution logic in fillRuntimeDefaults:
// use Provider if set, otherwise Command, falling back to "claude".
func inferAgentName(rc *RuntimeConfig) string {
	if rc.Provider != "" {
		return rc.Provider
	}
	if rc.Command != "" {
		return rc.Command
	}
	return "claude"
}

// GetRuntimeCommand is a convenience function that returns the full command string
// for starting an LLM session. It resolves the agent config and builds the command.
func GetRuntimeCommand(rigPath string) string {
	if rigPath == "" {
		// Try to detect town root from cwd for town-level agents (mayor, deacon)
		townRoot, err := findTownRootFromCwd()
		if err != nil {
			return DefaultRuntimeConfig().BuildCommand()
		}
		return ResolveAgentConfig(townRoot, "").BuildCommand()
	}
	// Derive town root from rig path (rig is typically ~/gt/<rigname>)
	townRoot := filepath.Dir(rigPath)
	return ResolveAgentConfig(townRoot, rigPath).BuildCommand()
}

// GetRuntimeCommandWithAgentOverride returns the full command for starting an LLM session,
// using agentOverride if non-empty.
func GetRuntimeCommandWithAgentOverride(rigPath, agentOverride string) (string, error) {
	if rigPath == "" {
		townRoot, err := findTownRootFromCwd()
		if err != nil {
			return DefaultRuntimeConfig().BuildCommand(), nil
		}
		rc, _, resolveErr := ResolveAgentConfigWithOverride(townRoot, "", agentOverride)
		if resolveErr != nil {
			return "", resolveErr
		}
		return rc.BuildCommand(), nil
	}

	townRoot := filepath.Dir(rigPath)
	rc, _, err := ResolveAgentConfigWithOverride(townRoot, rigPath, agentOverride)
	if err != nil {
		return "", err
	}
	return rc.BuildCommand(), nil
}

// GetRuntimeCommandWithPrompt returns the full command with an initial prompt.
func GetRuntimeCommandWithPrompt(rigPath, prompt string) string {
	if rigPath == "" {
		// Try to detect town root from cwd for town-level agents (mayor, deacon)
		townRoot, err := findTownRootFromCwd()
		if err != nil {
			return DefaultRuntimeConfig().BuildCommandWithPrompt(prompt)
		}
		return ResolveAgentConfig(townRoot, "").BuildCommandWithPrompt(prompt)
	}
	townRoot := filepath.Dir(rigPath)
	return ResolveAgentConfig(townRoot, rigPath).BuildCommandWithPrompt(prompt)
}

// GetRuntimeCommandWithPromptAndAgentOverride returns the full command with an initial prompt,
// using agentOverride if non-empty.
func GetRuntimeCommandWithPromptAndAgentOverride(rigPath, prompt, agentOverride string) (string, error) {
	if rigPath == "" {
		townRoot, err := findTownRootFromCwd()
		if err != nil {
			return DefaultRuntimeConfig().BuildCommandWithPrompt(prompt), nil
		}
		rc, _, resolveErr := ResolveAgentConfigWithOverride(townRoot, "", agentOverride)
		if resolveErr != nil {
			return "", resolveErr
		}
		return rc.BuildCommandWithPrompt(prompt), nil
	}

	townRoot := filepath.Dir(rigPath)
	rc, _, err := ResolveAgentConfigWithOverride(townRoot, rigPath, agentOverride)
	if err != nil {
		return "", err
	}
	return rc.BuildCommandWithPrompt(prompt), nil
}

// findTownRootFromCwd locates the town root by walking up from cwd.
// It uses townroot.Find so nested rigs that still have mayor/town.json
// do not shadow the outermost town.
func findTownRootFromCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting cwd: %w", err)
	}
	root := townroot.Find(cwd)
	if root == "" {
		return "", fmt.Errorf("town root not found (no %s marker)", townroot.Marker)
	}
	return root, nil
}

// ExtractSimpleRole extracts the simple role name from a GT_ROLE value.
// GT_ROLE can be:
//   - Simple: "mayor", "deacon"
//   - Compound: "rig/witness", "rig/refinery", "rig/crew/name", "rig/polecats/name"
//
// For compound format, returns the role segment (second part).
// For simple format, returns the role as-is.
func ExtractSimpleRole(gtRole string) string {
	if gtRole == "" {
		return ""
	}
	parts := strings.Split(gtRole, "/")
	switch len(parts) {
	case 1:
		// Simple format: "mayor", "deacon"
		return parts[0]
	case 2:
		// "rig/witness", "rig/refinery"
		return parts[1]
	case 3:
		// "rig/crew/name" → "crew", "rig/polecats/name" → "polecat"
		role := parts[1]
		if role == "polecats" {
			return constants.RolePolecat
		}
		return role
	default:
		return gtRole
	}
}

// BuildStartupCommand builds a full startup command with environment exports.
// envVars is a map of environment variable names to values.
// rigPath is optional - if empty, uses envVars["GT_ROOT"] to find town root,
// falling back to cwd detection if GT_ROOT is not set.
// prompt is optional - if provided, appended as the initial prompt.
//
// If envVars contains GT_ROLE, the function uses role-based agent resolution
// (ResolveRoleAgentConfig) to select the appropriate agent for the role.
// This enables per-role model selection via role_agents in settings.
func BuildStartupCommand(envVars map[string]string, rigPath, prompt string) string {
	role := ExtractSimpleRole(envVars["GT_ROLE"])
	rc, townRoot := resolveStartupConfig(envVars, role, rigPath)
	prepareStartupConfig(rc, role, townRoot, rigPath, envVars[AutoCompactWindowEnv])
	resolvedEnv := prepareStartupEnvironment(envVars, townRoot, rc, "")
	return renderStartupCommand(resolvedEnv, rc, prompt)
}

func resolveStartupConfig(envVars map[string]string, role, rigPath string) (*RuntimeConfig, string) {
	if rigPath != "" {
		townRoot := filepath.Dir(rigPath)
		if role == "crew" && envVars["GT_CREW"] != "" {
			return ResolveWorkerAgentConfig(envVars["GT_CREW"], townRoot, rigPath), townRoot
		}
		if role != "" {
			return ResolveRoleAgentConfig(role, townRoot, rigPath), townRoot
		}
		return ResolveAgentConfig(townRoot, rigPath), townRoot
	}

	townRoot := envVars["GT_ROOT"]
	if townRoot == "" {
		var err error
		townRoot, err = findTownRootFromCwd()
		if err != nil {
			return DefaultRuntimeConfig(), townRoot
		}
	}
	if role != "" {
		return ResolveRoleAgentConfig(role, townRoot, ""), townRoot
	}
	return ResolveAgentConfig(townRoot, ""), townRoot
}

func prepareStartupConfig(rc *RuntimeConfig, role, townRoot, rigPath, autoCompactOverride string) {
	if role != "" {
		applyRoleEffortToRuntime(rc, ResolveRoleEffort(role, townRoot, rigPath))
	}
	applyResolvedAutoCompact(rc, townRoot, autoCompactOverride)
	if len(rc.ExecWrapper) == 0 {
		rc.ExecWrapper = resolveExecWrapper(rigPath)
	}
}

func prepareStartupEnvironment(envVars map[string]string, townRoot string, rc *RuntimeConfig, agentOverride string) map[string]string {
	resolvedEnv := make(map[string]string, len(envVars)+2)
	for key, value := range envVars {
		resolvedEnv[key] = value
	}
	if townRoot != "" {
		resolvedEnv["GT_ROOT"] = townRoot
	}
	if rc.Session != nil && rc.Session.SessionIDEnv != "" {
		resolvedEnv["GT_SESSION_ID_ENV"] = rc.Session.SessionIDEnv
	}
	agentForProcess := rc.ResolvedAgent
	if agentOverride != "" {
		resolvedEnv["GT_AGENT"] = agentOverride
		agentForProcess = agentOverride
	} else if rc.ResolvedAgent != "" {
		resolvedEnv["GT_AGENT"] = rc.ResolvedAgent
	}
	processNames := ResolveProcessNames(agentForProcess, rc.Command, rc.Args...)
	resolvedEnv["GT_PROCESS_NAMES"] = strings.Join(processNames, ",")
	for key, value := range rc.Env {
		resolvedEnv[key] = value
	}
	SanitizeAgentEnv(resolvedEnv, envVars)
	return resolvedEnv
}

func renderStartupCommand(resolvedEnv map[string]string, rc *RuntimeConfig, prompt string) string {
	if runtime.GOOS == "windows" {
		return renderWindowsStartupCommand(resolvedEnv, rc, prompt)
	}
	return renderPosixStartupCommand(resolvedEnv, rc, prompt)
}

func startupAgentCommand(rc *RuntimeConfig, prompt string) string {
	command := ""
	if len(rc.ExecWrapper) > 0 {
		command = strings.Join(rc.ExecWrapper, " ") + " "
	}
	if prompt != "" {
		return command + rc.BuildCommandWithPrompt(prompt)
	}
	return command + rc.BuildCommand()
}

func renderWindowsStartupCommand(resolvedEnv map[string]string, rc *RuntimeConfig, prompt string) string {
	var scriptLines []string
	keys := make([]string, 0, len(resolvedEnv))
	for key := range resolvedEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		scriptLines = append(scriptLines, fmt.Sprintf("$env:%s=%s", key, psQuote(resolvedEnv[key])))
	}
	scriptLines = append(scriptLines, windowsStartupAgentCommand(rc, prompt))
	townRoot := resolvedEnv["GT_ROOT"]
	if townRoot == "" {
		townRoot = os.TempDir()
	}
	scriptDir := filepath.Join(townRoot, "daemon", "scripts")
	_ = os.MkdirAll(scriptDir, 0755)
	role := resolvedEnv["GT_ROLE"]
	if role == "" {
		role = "agent"
	}
	scriptPath := filepath.Join(scriptDir, strings.ReplaceAll(role, "/", "-")+"-startup.ps1")
	scriptContent := strings.Join(scriptLines, "\n") + "\n"
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0644); err != nil {
		return strings.Join(scriptLines, "; ")
	}
	return "& " + psQuote(scriptPath)
}

func windowsStartupAgentCommand(rc *RuntimeConfig, prompt string) string {
	command := ""
	if len(rc.ExecWrapper) > 0 {
		command = strings.Join(rc.ExecWrapper, " ") + " "
	}
	if prompt != "" {
		return command + "& " + rc.BuildCommandWithPrompt(prompt)
	}
	return command + "& " + rc.BuildCommand()
}

func renderPosixStartupCommand(resolvedEnv map[string]string, rc *RuntimeConfig, prompt string) string {
	var exports []string
	for key, value := range resolvedEnv {
		exports = append(exports, fmt.Sprintf("%s=%s", key, ShellQuote(value)))
	}
	sort.Strings(exports)
	command := ""
	if len(exports) > 0 {
		command = "exec env " + strings.Join(exports, " ") + " "
	}
	return command + startupAgentCommand(rc, prompt)
}

// SanitizeAgentEnv clears environment variables that are known to break agent
// startup when inherited from the parent shell/tmux environment.
//
// This is a SUPPLEMENTAL guard for paths that don't use AgentEnv() (which is
// the primary guard — see env.go). It protects: lifecycle.go's default path
// (non-polecat/non-crew roles) and handoff.go's manual export building.
// For callers that pass AgentEnv()-produced maps, this is a no-op since
// AgentEnv() already sets NODE_OPTIONS="".
//
// callerEnv is the original env map from the caller (before rc.Env merging).
// resolvedEnv is the post-merge map that may also contain values from rc.Env.
// NODE_OPTIONS is only cleared if neither callerEnv nor resolvedEnv (via rc.Env)
// explicitly provides it.
func SanitizeAgentEnv(resolvedEnv, callerEnv map[string]string) {
	// NODE_OPTIONS may contain debugger flags (e.g., --inspect from VSCode)
	// that cause Claude's Node.js runtime to crash with "Debugger attached" errors.
	// Only clear if not explicitly provided by the caller or agent config (rc.Env).
	if _, ok := callerEnv["NODE_OPTIONS"]; !ok {
		// Inner guard: preserve if rc.Env already set it in resolvedEnv
		if _, ok := resolvedEnv["NODE_OPTIONS"]; !ok {
			resolvedEnv["NODE_OPTIONS"] = ""
		}
	}

	// CLAUDECODE is set by Claude Code v2.x on startup and triggers nested session
	// detection. When gt sling is invoked from within a Claude Code session, tmux
	// inherits this variable into its global environment, causing new polecat sessions
	// to fail with "Nested sessions share runtime resources and will crash all active
	// sessions." Clear it unless the caller explicitly provides it.
	// See: https://github.com/steveyegge/gastown/issues/1666
	if _, ok := callerEnv["CLAUDECODE"]; !ok {
		resolvedEnv["CLAUDECODE"] = ""
	}

	clearBDTargetSelectorEnv(resolvedEnv)
}

// PrependEnv prepends export statements to a command string.
// Values containing special characters are properly shell-quoted.
// On Windows, uses PowerShell $env: syntax.
func PrependEnv(command string, envVars map[string]string) string {
	if len(envVars) == 0 {
		return command
	}

	var exports []string
	for k, v := range envVars {
		if runtime.GOOS == "windows" {
			exports = append(exports, fmt.Sprintf("$env:%s=%s", k, psQuote(v)))
		} else {
			exports = append(exports, fmt.Sprintf("%s=%s", k, ShellQuote(v)))
		}
	}

	sort.Strings(exports)
	if runtime.GOOS == "windows" {
		return strings.Join(exports, "; ") + "; " + command
	}
	return "export " + strings.Join(exports, " ") + " && " + command
}

// BuildStartupCommandWithAgentOverride builds a startup command like BuildStartupCommand,
// but uses agentOverride if non-empty.
//
// Resolution priority:
//  1. agentOverride (explicit override)
//  2. role_agents[GT_ROLE] (if GT_ROLE is in envVars)
//  3. Default agent resolution (rig's Agent → town's DefaultAgent → "claude")
func BuildStartupCommandWithAgentOverride(envVars map[string]string, rigPath, prompt, agentOverride string) (string, error) {
	role := ExtractSimpleRole(envVars["GT_ROLE"])
	rc, townRoot, err := resolveStartupConfigWithOverride(envVars, role, rigPath, agentOverride)
	if err != nil {
		return "", err
	}

	rc = withRoleSettingsFlag(rc, role, rigPath)
	prepareStartupConfig(rc, role, townRoot, rigPath, envVars[AutoCompactWindowEnv])
	resolvedEnv := prepareStartupEnvironment(envVars, townRoot, rc, agentOverride)
	return renderStartupCommand(resolvedEnv, rc, prompt), nil
}

func resolveStartupConfigWithOverride(envVars map[string]string, role, rigPath, agentOverride string) (*RuntimeConfig, string, error) {
	if rigPath != "" {
		return resolveRigStartupConfig(envVars, role, rigPath, agentOverride)
	}

	return resolveTownStartupConfig(envVars, role, agentOverride)
}

func resolveRigStartupConfig(envVars map[string]string, role, rigPath, agentOverride string) (*RuntimeConfig, string, error) {
	townRoot := filepath.Dir(rigPath)
	if agentOverride != "" {
		rc, _, err := ResolveAgentConfigWithOverride(townRoot, rigPath, agentOverride)
		return rc, townRoot, err
	}
	if role == "crew" && envVars["GT_CREW"] != "" {
		return ResolveWorkerAgentConfig(envVars["GT_CREW"], townRoot, rigPath), townRoot, nil
	}
	if role != "" {
		return ResolveRoleAgentConfig(role, townRoot, rigPath), townRoot, nil
	}
	return ResolveAgentConfig(townRoot, rigPath), townRoot, nil
}

func resolveTownStartupConfig(envVars map[string]string, role, agentOverride string) (*RuntimeConfig, string, error) {
	townRoot := envVars["GT_ROOT"]
	if townRoot == "" {
		var err error
		townRoot, err = findTownRootFromCwd()
		if err != nil {
			return resolveTownStartupWithoutRoot(agentOverride)
		}
	}
	return resolveTownStartupAtRoot(townRoot, role, agentOverride)
}

func resolveTownStartupAtRoot(townRoot, role, agentOverride string) (*RuntimeConfig, string, error) {
	if agentOverride != "" {
		rc, _, err := ResolveAgentConfigWithOverride(townRoot, "", agentOverride)
		return rc, townRoot, err
	}
	if role != "" {
		return ResolveRoleAgentConfig(role, townRoot, ""), townRoot, nil
	}
	return ResolveAgentConfig(townRoot, ""), townRoot, nil
}

func resolveTownStartupWithoutRoot(agentOverride string) (*RuntimeConfig, string, error) {
	if agentOverride != "" {
		return resolveOverrideWithoutTownRoot(agentOverride)
	}
	return DefaultRuntimeConfig(), "", nil
}

func resolveOverrideWithoutTownRoot(agentOverride string) (*RuntimeConfig, string, error) {
	if GetAgentPresetByName(agentOverride) == nil {
		return nil, "", fmt.Errorf("agent '%s' not found", agentOverride)
	}
	return RuntimeConfigFromPreset(AgentPreset(agentOverride)), "", nil
}

// BuildStartupCommandFromConfig builds a startup command from a complete AgentEnvConfig.
// Use this (instead of Build*StartupCommand helpers) when you need full OTEL context:
// Issue (gt.issue), Topic (gt.topic), SessionName (gt.session), etc.
// The rigPath, prompt, and agentOverride are passed through directly.
func BuildStartupCommandFromConfig(cfg AgentEnvConfig, rigPath, prompt, agentOverride string) (string, error) {
	envVars := AgentEnv(cfg)
	return BuildStartupCommandWithAgentOverride(envVars, rigPath, prompt, agentOverride)
}

// BuildAgentStartupCommand is a convenience function for starting agent sessions.
// It uses AgentEnv to set all standard environment variables.
// For rig-level roles (witness, refinery), pass the rig name and rigPath.
// For town-level roles (mayor, deacon, boot), pass empty rig and rigPath, but provide townRoot.
func BuildAgentStartupCommand(role, rig, townRoot, rigPath, prompt string) string {
	envVars := AgentEnv(AgentEnvConfig{
		Role:     role,
		Rig:      rig,
		TownRoot: townRoot,
		Prompt:   prompt,
	})
	return BuildStartupCommand(envVars, rigPath, prompt)
}

// BuildAgentStartupCommandWithAgentOverride is like BuildAgentStartupCommand, but uses agentOverride if non-empty.
func BuildAgentStartupCommandWithAgentOverride(role, rig, townRoot, rigPath, prompt, agentOverride string) (string, error) {
	envVars := AgentEnv(AgentEnvConfig{
		Role:     role,
		Rig:      rig,
		TownRoot: townRoot,
		Prompt:   prompt,
	})
	return BuildStartupCommandWithAgentOverride(envVars, rigPath, prompt, agentOverride)
}

// BuildPolecatStartupCommand builds the startup command for a polecat.
// Sets GT_ROLE, GT_RIG, GT_POLECAT, BD_ACTOR, GIT_AUTHOR_NAME, and GT_ROOT.
func BuildPolecatStartupCommand(rigName, polecatName, rigPath, prompt string) string {
	var townRoot string
	if rigPath != "" {
		townRoot = filepath.Dir(rigPath)
	}
	envVars := AgentEnv(AgentEnvConfig{
		Role:      constants.RolePolecat,
		Rig:       rigName,
		AgentName: polecatName,
		TownRoot:  townRoot,
		Prompt:    prompt,
	})
	return BuildStartupCommand(envVars, rigPath, prompt)
}

// BuildPolecatStartupCommandWithAgentOverride is like BuildPolecatStartupCommand, but uses agentOverride if non-empty.
func BuildPolecatStartupCommandWithAgentOverride(rigName, polecatName, rigPath, prompt, agentOverride string) (string, error) {
	var townRoot string
	if rigPath != "" {
		townRoot = filepath.Dir(rigPath)
	}
	envVars := AgentEnv(AgentEnvConfig{
		Role:      constants.RolePolecat,
		Rig:       rigName,
		AgentName: polecatName,
		TownRoot:  townRoot,
		Prompt:    prompt,
	})
	return BuildStartupCommandWithAgentOverride(envVars, rigPath, prompt, agentOverride)
}

// BuildCrewStartupCommand builds the startup command for a crew member.
// Sets GT_ROLE, GT_RIG, GT_CREW, BD_ACTOR, GIT_AUTHOR_NAME, and GT_ROOT.
func BuildCrewStartupCommand(rigName, crewName, rigPath, prompt string) string {
	var townRoot string
	if rigPath != "" {
		townRoot = filepath.Dir(rigPath)
	}
	envVars := AgentEnv(AgentEnvConfig{
		Role:      constants.RoleCrew,
		Rig:       rigName,
		AgentName: crewName,
		TownRoot:  townRoot,
		Prompt:    prompt,
	})
	return BuildStartupCommand(envVars, rigPath, prompt)
}

// BuildCrewStartupCommandWithAgentOverride is like BuildCrewStartupCommand, but uses agentOverride if non-empty.
func BuildCrewStartupCommandWithAgentOverride(rigName, crewName, rigPath, prompt, agentOverride string) (string, error) {
	var townRoot string
	if rigPath != "" {
		townRoot = filepath.Dir(rigPath)
	}
	envVars := AgentEnv(AgentEnvConfig{
		Role:      constants.RoleCrew,
		Rig:       rigName,
		AgentName: crewName,
		TownRoot:  townRoot,
		Prompt:    prompt,
	})
	return BuildStartupCommandWithAgentOverride(envVars, rigPath, prompt, agentOverride)
}

// resolveExecWrapper loads the exec_wrapper from rig settings.
// ExecWrapper is a deployment-level setting (sandbox/container) that wraps the agent binary.
// It is independent of agent choice — exitbox wraps Claude, Codex, or any other runtime.
func resolveExecWrapper(rigPath string) []string {
	if rigPath != "" {
		if rigSettings, err := LoadRigSettings(RigSettingsPath(rigPath)); err == nil && rigSettings != nil {
			if rigSettings.Runtime != nil && len(rigSettings.Runtime.ExecWrapper) > 0 {
				return rigSettings.Runtime.ExecWrapper
			}
		}
	}
	return nil
}

// ExpectedPaneCommands returns tmux pane command names that indicate the runtime is running.
// Claude can report as "node" (older versions) or "claude" (newer versions).
// Other runtimes typically report their executable name.
func ExpectedPaneCommands(rc *RuntimeConfig) []string {
	if rc == nil || rc.Command == "" {
		return nil
	}
	if filepath.Base(rc.Command) == "claude" {
		return []string{"node", "claude"}
	}
	return []string{filepath.Base(rc.Command)}
}

// GetDefaultFormula returns the default formula for a rig from settings/config.json.
// Returns empty string if no default is configured.
// rigPath is the path to the rig directory (e.g., ~/gt/gastown).
func GetDefaultFormula(rigPath string) string {
	settingsPath := RigSettingsPath(rigPath)
	settings, err := LoadRigSettings(settingsPath)
	if err != nil {
		return ""
	}
	if settings.Workflow == nil {
		return ""
	}
	return settings.Workflow.DefaultFormula
}

// GetRigPrefix returns the beads prefix for a rig from rigs.json.
// Falls back to "gt" if the rig isn't found or has no prefix configured.
// townRoot is the path to the town directory (e.g., ~/gt).
func GetRigPrefix(townRoot, rigName string) string {
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return "gt" // fallback
	}

	entry, ok := rigsConfig.Rigs[rigName]
	if !ok {
		return "gt" // fallback
	}

	if entry.BeadsConfig == nil || entry.BeadsConfig.Prefix == "" {
		return "gt" // fallback
	}

	// Strip trailing hyphen if present (prefix stored as "gt-" but used as "gt")
	prefix := entry.BeadsConfig.Prefix
	return strings.TrimSuffix(prefix, "-")
}

// AllRigPrefixes returns a sorted list of all rig beads prefixes from rigs.json.
// Trailing hyphens are stripped (e.g. "gt-" becomes "gt").
// Returns nil on error (caller should handle the fallback).
func AllRigPrefixes(townRoot string) []string {
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return nil
	}
	var prefixes []string
	for _, entry := range rigsConfig.Rigs {
		if entry.BeadsConfig != nil && entry.BeadsConfig.Prefix != "" {
			prefixes = append(prefixes, strings.TrimSuffix(entry.BeadsConfig.Prefix, "-"))
		}
	}
	sort.Strings(prefixes)
	return prefixes
}

// EscalationConfigPath returns the standard path for escalation config in a town.
func EscalationConfigPath(townRoot string) string {
	return filepath.Join(townRoot, "settings", "escalation.json")
}

// LoadEscalationConfig loads and validates an escalation configuration file.
func LoadEscalationConfig(path string) (*EscalationConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading escalation config: %w", err)
	}

	var config EscalationConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parsing escalation config: %w", err)
	}

	if err := validateEscalationConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// LoadOrCreateEscalationConfig loads the escalation config, creating a default if not found.
func LoadOrCreateEscalationConfig(path string) (*EscalationConfig, error) {
	config, err := LoadEscalationConfig(path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return NewEscalationConfig(), nil
		}
		return nil, err
	}
	return config, nil
}

// SaveEscalationConfig saves an escalation configuration to a file.
func SaveEscalationConfig(path string, config *EscalationConfig) error {
	if err := validateEscalationConfig(config); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding escalation config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil { //nolint:gosec // G306: escalation config doesn't contain secrets
		return fmt.Errorf("writing escalation config: %w", err)
	}

	return nil
}

// validateEscalationConfig validates an EscalationConfig.
func validateEscalationConfig(c *EscalationConfig) error {
	if c.Type != "escalation" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'escalation', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentEscalationVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentEscalationVersion)
	}
	if err := validateEscalationThreshold(c.StaleThreshold); err != nil {
		return err
	}
	if c.Routes == nil {
		c.Routes = make(map[string][]string)
	}
	if err := validateEscalationRoutes(c.Routes); err != nil {
		return err
	}
	return validateEscalationLimits(c.MaxReescalations)
}

func validateEscalationThreshold(threshold string) error {
	if threshold == "" {
		return nil
	}
	if _, err := time.ParseDuration(threshold); err != nil {
		return fmt.Errorf("invalid stale_threshold: %w", err)
	}
	return nil
}

func validateEscalationRoutes(routes map[string][]string) error {
	for severity := range routes {
		if !IsValidSeverity(severity) {
			return fmt.Errorf("%w: unknown severity '%s' (valid: low, medium, high, critical)", ErrMissingField, severity)
		}
	}
	return nil
}

func validateEscalationLimits(maxReescalations *int) error {
	if maxReescalations != nil && *maxReescalations < 0 {
		return fmt.Errorf("%w: max_reescalations must be non-negative", ErrMissingField)
	}
	return nil
}

// GetStaleThreshold returns the stale threshold as a time.Duration.
// Returns 4 hours if not configured or invalid.
func (c *EscalationConfig) GetStaleThreshold() time.Duration {
	if c.StaleThreshold == "" {
		return 4 * time.Hour
	}
	d, err := time.ParseDuration(c.StaleThreshold)
	if err != nil {
		return 4 * time.Hour
	}
	return d
}

// GetRouteForSeverity returns the escalation route actions for a given severity.
// Falls back to ["bead", "mail:mayor"] if no specific route is configured.
func (c *EscalationConfig) GetRouteForSeverity(severity string) []string {
	if route, ok := c.Routes[severity]; ok {
		return route
	}
	// Fallback to default route
	return []string{"bead", "mail:mayor"}
}

// GetMaxReescalations returns the maximum number of re-escalations allowed.
// Returns 2 if not configured (nil). Explicit 0 means "never re-escalate".
func (c *EscalationConfig) GetMaxReescalations() int {
	if c.MaxReescalations == nil {
		return 2
	}
	return *c.MaxReescalations
}
