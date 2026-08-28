package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
)

// buildPrefixSet builds a set of all known rig prefixes from rigs.json.
// This maps prefix→true for efficient lookup. (gt-85w7)
func buildPrefixSet(registeredRigs map[string]bool, townRoot string) map[string]bool {
	prefixes := make(map[string]bool)
	for rigName := range registeredRigs {
		prefixes[rigName] = true // rig name itself is always valid
		prefix := config.GetRigPrefix(townRoot, rigName)
		if prefix != "" {
			prefixes[prefix] = true
		}
	}
	return prefixes
}

// StaleRuntimeFilesCheck detects stale PID files and wisp configs for rigs
// that are no longer registered. These can cause the daemon to incorrectly
// think agents are running or try to start agents for removed rigs.
type StaleRuntimeFilesCheck struct {
	FixableCheck
	stalePIDFiles    []string
	staleWispConfigs []string
}

// NewStaleRuntimeFilesCheck creates a new stale runtime files check.
func NewStaleRuntimeFilesCheck() *StaleRuntimeFilesCheck {
	return &StaleRuntimeFilesCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "stale-runtime-files",
				CheckDescription: "Detect stale PID files and wisp configs for removed rigs",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// Run checks for stale runtime files.
func (c *StaleRuntimeFilesCheck) Run(ctx *CheckContext) *CheckResult {
	c.stalePIDFiles = nil
	c.staleWispConfigs = nil

	registeredRigs, err := registeredRigNames(ctx.TownRoot)
	if err != nil {
		return staleRuntimeRegistryResult(c, err)
	}

	knownPrefixes := buildPrefixSet(registeredRigs, ctx.TownRoot)
	pidFiles, pidDetails := stalePIDFiles(ctx.TownRoot, knownPrefixes)
	wispConfigs, wispDetails := staleWispConfigs(ctx.TownRoot, registeredRigs)
	c.stalePIDFiles = pidFiles
	c.staleWispConfigs = wispConfigs
	details := append(pidDetails, wispDetails...)
	return staleRuntimeResult(c, details)
}

func staleRuntimeRegistryResult(c *StaleRuntimeFilesCheck, err error) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: "Could not load rigs registry",
		Details: []string{err.Error()},
	}
}

func registeredRigNames(townRoot string) (map[string]bool, error) {
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return nil, err
	}
	registeredRigs := make(map[string]bool)
	for rigName := range rigsConfig.Rigs {
		registeredRigs[rigName] = true
	}
	return registeredRigs, nil
}

func stalePIDFiles(townRoot string, knownPrefixes map[string]bool) ([]string, []string) {
	pidsDir := filepath.Join(townRoot, ".runtime", "pids")
	files, err := os.ReadDir(pidsDir)
	if err != nil {
		return nil, nil
	}
	var paths []string
	var details []string
	for _, file := range files {
		path, detail, ok := stalePIDFile(pidsDir, file, knownPrefixes)
		if !ok {
			continue
		}
		paths = append(paths, path)
		details = append(details, detail)
	}
	return paths, details
}

func stalePIDFile(pidsDir string, file os.DirEntry, knownPrefixes map[string]bool) (string, string, bool) {
	if file.IsDir() {
		return "", "", false
	}
	name := file.Name()
	rigPrefix := extractRigPrefix(name)
	if rigPrefix == "" || rigPrefix == "hq" || rigPrefix == "gt" || knownPrefixes[rigPrefix] {
		return "", "", false
	}
	return filepath.Join(pidsDir, name), fmt.Sprintf("Stale PID file for unregistered rig: %s", name), true
}

func staleWispConfigs(townRoot string, registeredRigs map[string]bool) ([]string, []string) {
	wispConfigDir := filepath.Join(townRoot, ".beads-wisp", "config")
	files, err := os.ReadDir(wispConfigDir)
	if err != nil {
		return nil, nil
	}
	var paths []string
	var details []string
	for _, file := range files {
		path, detail, ok := staleWispConfig(wispConfigDir, file, registeredRigs)
		if !ok {
			continue
		}
		paths = append(paths, path)
		details = append(details, detail)
	}
	return paths, details
}

func staleWispConfig(configDir string, file os.DirEntry, registeredRigs map[string]bool) (string, string, bool) {
	if file.IsDir() {
		return "", "", false
	}
	name := file.Name()
	rigName := strings.TrimSuffix(name, ".json")
	if rigName == "" || registeredRigs[rigName] {
		return "", "", false
	}
	return filepath.Join(configDir, name), fmt.Sprintf("Stale wisp config for unregistered rig: %s", name), true
}

func staleRuntimeResult(c *StaleRuntimeFilesCheck, details []string) *CheckResult {
	if len(c.stalePIDFiles) == 0 && len(c.staleWispConfigs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No stale runtime files found",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: staleRuntimeMessage(len(c.stalePIDFiles), len(c.staleWispConfigs)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to remove stale runtime files",
	}
}

func staleRuntimeMessage(pidCount, wispCount int) string {
	var parts []string
	if pidCount > 0 {
		parts = append(parts, fmt.Sprintf("%d stale PID file(s)", pidCount))
	}
	if wispCount > 0 {
		parts = append(parts, fmt.Sprintf("%d stale wisp config(s)", wispCount))
	}
	return strings.Join(parts, ", ")
}

// Fix removes stale runtime files.
func (c *StaleRuntimeFilesCheck) Fix(_ *CheckContext) error {
	for _, path := range c.stalePIDFiles {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove %s: %w", path, err)
		}
	}
	for _, path := range c.staleWispConfigs {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove %s: %w", path, err)
		}
	}
	return nil
}

// extractRigPrefix extracts the rig prefix from a PID filename.
// Examples: sw-witness.pid -> sw, pir-crew-dickle.pid -> pir, hq-deacon.pid -> hq
func extractRigPrefix(filename string) string {
	// Remove .pid extension
	name := strings.TrimSuffix(filename, ".pid")
	// Split on first hyphen or underscore
	for i, c := range name {
		if c == '-' || c == '_' {
			return name[:i]
		}
	}
	return name
}
