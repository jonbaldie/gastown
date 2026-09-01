package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/rig"
)

// RigConfigSyncCheck verifies that all registered rigs have a config.json file,
// Dolt database, and rig identity bead. This prevents issues where the daemon
// can't find the beads prefix to check docked/parked status.
type RigConfigSyncCheck struct {
	FixableCheck
	missingConfig    []string
	prefixMismatches []prefixMismatch
	missingRigBeads  []rigBeadInfo
	missingDoltDB    []string
	missingMetadata  []string
	missingPrefixCfg []string
	missingExportCfg []string
	dbNameMismatches []dbMismatch
	dbCheckErrors    []string
}

type prefixMismatch struct {
	rigName        string
	configPrefix   string
	registryPrefix string
}

type rigBeadInfo struct {
	rigName string
	prefix  string
	gitURL  string
}

type dbMismatch struct {
	rigName    string
	prefix     string
	currentDB  string
	expectedDB string
}

// NewRigConfigSyncCheck creates a new rig config sync check.
func NewRigConfigSyncCheck() *RigConfigSyncCheck {
	return &RigConfigSyncCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "rig-config-sync",
				CheckDescription: "Verify registered rigs have config.json, Dolt DB, and identity beads",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

func (c *RigConfigSyncCheck) Run(ctx *CheckContext) *CheckResult {
	return runRigConfigSync(c, ctx)
}

func (c *RigConfigSyncCheck) Fix(ctx *CheckContext) error {
	return fixRigConfigSync(c, ctx)
}

func runRigConfigSync(c *RigConfigSyncCheck, ctx *CheckContext) *CheckResult {
	rigsConfig, err := config.LoadRigsConfig(filepath.Join(ctx.TownRoot, "mayor", "rigs.json"))
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not load rigs registry",
			Details: []string{err.Error()},
		}
	}
	resetRigConfigSync(c)
	return rigConfigSyncResult(c, scanRegisteredRigs(c, ctx, rigsConfig))
}

func resetRigConfigSync(c *RigConfigSyncCheck) {
	c.missingConfig = nil
	c.prefixMismatches = nil
	c.missingRigBeads = nil
	c.missingDoltDB = nil
	c.missingMetadata = nil
	c.missingPrefixCfg = nil
	c.missingExportCfg = nil
	c.dbNameMismatches = nil
	c.dbCheckErrors = nil
}

func scanRegisteredRigs(c *RigConfigSyncCheck, ctx *CheckContext, rigsConfig *config.RigsConfig) []string {
	townDB := readDoltDatabase(filepath.Join(ctx.TownRoot, ".beads"))
	var details []string
	for rigName, entry := range rigsConfig.Rigs {
		details = append(details, scanOneRegisteredRig(c, ctx, rigName, entry, townDB)...)
	}
	return details
}

func scanOneRegisteredRig(c *RigConfigSyncCheck, ctx *CheckContext, rigName string, entry config.RigEntry, townDB string) []string {
	rigPath := filepath.Join(ctx.TownRoot, rigName)
	if _, err := os.Stat(rigPath); os.IsNotExist(err) {
		return []string{fmt.Sprintf("Registered rig %s directory does not exist", rigName)}
	}
	expectedPrefix := entryPrefix(entry)
	configPath := filepath.Join(rigPath, "config.json")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		c.missingConfig = append(c.missingConfig, rigName)
		return []string{fmt.Sprintf("Rig %s is registered but missing config.json", rigName)}
	}
	rigCfg, err := rig.LoadRigConfig(rigPath)
	if err != nil {
		return []string{fmt.Sprintf("Rig %s has unreadable config.json: %v", rigName, err)}
	}
	return scanLoadedRigConfig(c, ctx, rigName, entry, expectedPrefix, configPrefixOf(rigCfg), townDB)
}

func entryPrefix(entry config.RigEntry) string {
	if entry.BeadsConfig != nil {
		return entry.BeadsConfig.Prefix
	}
	return ""
}

func configPrefixOf(rigCfg *rig.RigConfig) string {
	if rigCfg != nil && rigCfg.Beads != nil {
		return rigCfg.Beads.Prefix
	}
	return ""
}

func scanLoadedRigConfig(c *RigConfigSyncCheck, ctx *CheckContext, rigName string, entry config.RigEntry, expectedPrefix, configPrefix, townDB string) []string {
	details := notePrefixMismatch(c, rigName, expectedPrefix, configPrefix)
	beadsDir := doltserver.FindRigBeadsDir(ctx.TownRoot, rigName)
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return append(details, fmt.Sprintf("Rig %s is missing .beads directory", rigName))
	}
	details = append(details, noteConfigYamlDrift(c, rigName, beadsDir, expectedPrefix)...)
	metaDetails, skipBead := noteMetadataAndDolt(c, ctx, rigName, configPrefix, townDB, beadsDir)
	details = append(details, metaDetails...)
	if skipBead {
		return details
	}
	return append(details, noteMissingRigBead(c, entry, rigName, configPrefix, beadsDir, filepath.Join(ctx.TownRoot, rigName))...)
}

func notePrefixMismatch(c *RigConfigSyncCheck, rigName, expectedPrefix, configPrefix string) []string {
	if expectedPrefix == "" || configPrefix == "" || expectedPrefix == configPrefix {
		return nil
	}
	c.prefixMismatches = append(c.prefixMismatches, prefixMismatch{
		rigName:        rigName,
		configPrefix:   configPrefix,
		registryPrefix: expectedPrefix,
	})
	return []string{fmt.Sprintf(
		"Rig %s prefix mismatch: config.json has %q, registry has %q",
		rigName, configPrefix, expectedPrefix)}
}

func noteConfigYamlDrift(c *RigConfigSyncCheck, rigName, beadsDir, expectedPrefix string) []string {
	data, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		return nil
	}
	var details []string
	content := string(data)
	if !strings.Contains(content, "issue-prefix:") && expectedPrefix != "" {
		c.missingPrefixCfg = append(c.missingPrefixCfg, rigName)
		details = append(details, fmt.Sprintf("Rig %s .beads/config.yaml missing issue-prefix", rigName))
	}
	if !beads.ConfigYAMLDisablesAutoExport(content) {
		c.missingExportCfg = append(c.missingExportCfg, rigName)
		details = append(details, fmt.Sprintf("Rig %s .beads/config.yaml must disable export.auto", rigName))
	}
	return details
}

func noteMetadataAndDolt(c *RigConfigSyncCheck, ctx *CheckContext, rigName, configPrefix, townDB, beadsDir string) ([]string, bool) {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
		return noteMissingMetadata(c, ctx, rigName, configPrefix), true
	}
	metadata, detail := readRigDoltMetadata(rigName, metadataPath)
	if detail != "" {
		return []string{detail}, true
	}
	if metadata.DoltMode != "server" {
		return nil, false
	}
	return noteServerDoltState(c, ctx, rigName, configPrefix, townDB, metadata), false
}

type rigDoltMetadata struct {
	DoltDatabase string `json:"dolt_database"`
	DoltMode     string `json:"dolt_mode"`
}

func readRigDoltMetadata(rigName, metadataPath string) (rigDoltMetadata, string) {
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return rigDoltMetadata{}, fmt.Sprintf("Rig %s could not read metadata.json: %v", rigName, err)
	}
	var metadata rigDoltMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return rigDoltMetadata{}, fmt.Sprintf("Rig %s has invalid metadata.json: %v", rigName, err)
	}
	return metadata, ""
}

func noteMissingMetadata(c *RigConfigSyncCheck, ctx *CheckContext, rigName, expectedPrefix string) []string {
	c.missingMetadata = append(c.missingMetadata, rigName)
	details := []string{fmt.Sprintf("Rig %s is missing .beads/metadata.json", rigName)}
	exists, err := doltDatabaseExists(ctx, rigName)
	if err != nil {
		c.dbCheckErrors = append(c.dbCheckErrors, rigName)
		return append(details, fmt.Sprintf("Rig %s Dolt database status could not be verified: %v", rigName, err))
	}
	if expectedPrefix != "" && !exists {
		c.missingDoltDB = append(c.missingDoltDB, rigName)
		details = append(details, fmt.Sprintf("Rig %s Dolt database '%s' not found on server", rigName, rigName))
	}
	return details
}

func noteServerDoltState(c *RigConfigSyncCheck, ctx *CheckContext, rigName, configPrefix, townDB string, metadata rigDoltMetadata) []string {
	expectedDBName := expectedRigDoltDBName(ctx, townDB, rigName)
	if expectedDBName == "" {
		return nil
	}
	details := noteDBNameMismatch(c, rigName, configPrefix, metadata.DoltDatabase, expectedDBName)
	return append(details, noteMissingServerDoltDB(c, ctx, rigName, metadata.DoltDatabase, expectedDBName)...)
}

func expectedRigDoltDBName(ctx *CheckContext, townDB, rigName string) string {
	if rigName == "deacon" && townDB != "" {
		return townDB
	}
	doltDataDir := filepath.Join(ctx.TownRoot, ".dolt-data")
	if _, err := os.Stat(filepath.Join(doltDataDir, rigName)); os.IsNotExist(err) {
		if prefix := config.GetRigPrefix(ctx.TownRoot, rigName); prefix != "" {
			if _, err := os.Stat(filepath.Join(doltDataDir, prefix)); err == nil {
				return prefix
			}
		}
	}
	return rigName
}

func noteDBNameMismatch(c *RigConfigSyncCheck, rigName, configPrefix, currentDB, expectedDB string) []string {
	if currentDB == expectedDB {
		return nil
	}
	c.dbNameMismatches = append(c.dbNameMismatches, dbMismatch{
		rigName:    rigName,
		prefix:     configPrefix,
		currentDB:  currentDB,
		expectedDB: expectedDB,
	})
	return []string{fmt.Sprintf(
		"Rig %s database name mismatch: metadata has '%s', should be '%s' (rig name)",
		rigName, currentDB, expectedDB)}
}

func noteMissingServerDoltDB(c *RigConfigSyncCheck, ctx *CheckContext, rigName, currentDB, expectedDB string) []string {
	exists, err := doltDatabaseExists(ctx, currentDB)
	if err != nil {
		c.dbCheckErrors = append(c.dbCheckErrors, rigName)
		return []string{fmt.Sprintf("Rig %s Dolt database status could not be verified: %v", rigName, err)}
	}
	if exists || canonicalDoltDBExists(ctx, currentDB, expectedDB) {
		return nil
	}
	c.missingDoltDB = append(c.missingDoltDB, rigName)
	return []string{fmt.Sprintf("Rig %s Dolt database '%s' not found on server", rigName, currentDB)}
}

func canonicalDoltDBExists(ctx *CheckContext, currentDB, expectedDB string) bool {
	expectedExists, err := doltDatabaseExists(ctx, expectedDB)
	return err == nil && currentDB != expectedDB && expectedExists
}

func noteMissingRigBead(c *RigConfigSyncCheck, entry config.RigEntry, rigName, configPrefix, beadsDir, rigPath string) []string {
	if configPrefix == "" {
		return nil
	}
	rigBeadID := fmt.Sprintf("%s-rig-%s", configPrefix, rigName)
	if rigBeadExists(rigBeadID, rigPath, beadsDir) {
		return nil
	}
	c.missingRigBeads = append(c.missingRigBeads, rigBeadInfo{
		rigName: rigName,
		prefix:  configPrefix,
		gitURL:  entry.GitURL,
	})
	return []string{fmt.Sprintf("Rig %s is missing identity bead %s", rigName, rigBeadID)}
}

func rigConfigSyncResult(c *RigConfigSyncCheck, details []string) *CheckResult {
	if rigConfigSyncIssueCount(c) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "All registered rigs have valid configuration",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: strings.Join(rigConfigSyncMessages(c), ", "),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to create missing config files and databases",
	}
}

func rigConfigSyncIssueCount(c *RigConfigSyncCheck) int {
	return len(c.missingConfig) + len(c.prefixMismatches) + len(c.missingRigBeads) +
		len(c.missingDoltDB) + len(c.missingMetadata) + len(c.missingPrefixCfg) +
		len(c.missingExportCfg) + len(c.dbNameMismatches) + len(c.dbCheckErrors)
}

func rigConfigSyncMessages(c *RigConfigSyncCheck) []string {
	var parts []string
	parts = appendCountPart(parts, len(c.missingConfig), "missing config.json")
	parts = appendCountPart(parts, len(c.prefixMismatches), "prefix mismatch(es)")
	parts = appendCountPart(parts, len(c.missingRigBeads), "missing identity bead(s)")
	parts = appendCountPart(parts, len(c.missingDoltDB), "missing Dolt DB(s)")
	parts = appendCountPart(parts, len(c.missingMetadata), "missing metadata.json")
	parts = appendCountPart(parts, len(c.missingPrefixCfg), "missing issue-prefix")
	parts = appendCountPart(parts, len(c.missingExportCfg), "export.auto drift")
	parts = appendCountPart(parts, len(c.dbNameMismatches), "DB name mismatch(es)")
	parts = appendCountPart(parts, len(c.dbCheckErrors), "Dolt DB status unknown")
	return parts
}

func appendCountPart(parts []string, n int, label string) []string {
	if n == 0 {
		return parts
	}
	return append(parts, fmt.Sprintf("%d %s", n, label))
}

func fixRigConfigSync(c *RigConfigSyncCheck, ctx *CheckContext) error {
	rigsConfig, err := config.LoadRigsConfig(filepath.Join(ctx.TownRoot, "mayor", "rigs.json"))
	if err != nil {
		return fmt.Errorf("could not load rigs registry: %w", err)
	}
	if err := fixRigConfigFiles(c, ctx, rigsConfig); err != nil {
		return err
	}
	if err := fixRigDoltState(c, ctx, rigsConfig); err != nil {
		return err
	}
	return fixMissingRigBeads(c, ctx)
}

func fixRigConfigFiles(c *RigConfigSyncCheck, ctx *CheckContext, rigsConfig *config.RigsConfig) error {
	if err := fixMissingRigConfigs(c, ctx, rigsConfig); err != nil {
		return err
	}
	if err := fixMissingPrefixCfg(c, ctx, rigsConfig); err != nil {
		return err
	}
	if err := fixMissingExportCfg(c, ctx, rigsConfig); err != nil {
		return err
	}
	return fixMissingMetadata(c, ctx)
}

func fixMissingRigConfigs(c *RigConfigSyncCheck, ctx *CheckContext, rigsConfig *config.RigsConfig) error {
	for _, rigName := range c.missingConfig {
		if err := writeMissingRigConfig(ctx, rigsConfig, rigName); err != nil {
			return err
		}
	}
	return nil
}

func writeMissingRigConfig(ctx *CheckContext, rigsConfig *config.RigsConfig, rigName string) error {
	entry, ok := rigsConfig.Rigs[rigName]
	if !ok {
		return nil
	}
	rigCfg := &rig.RigConfig{
		Type:      "rig",
		Version:   1,
		Name:      rigName,
		GitURL:    entry.GitURL,
		CreatedAt: entry.AddedAt,
	}
	if prefix := entryPrefix(entry); prefix != "" {
		rigCfg.Beads = &rig.BeadsConfig{Prefix: prefix}
	}
	data, err := json.MarshalIndent(rigCfg, "", "  ")
	if err != nil {
		return fmt.Errorf("could not serialize config for %s: %w", rigName, err)
	}
	configPath := filepath.Join(ctx.TownRoot, rigName, "config.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("could not write config.json for %s: %w", rigName, err)
	}
	return nil
}

func fixMissingPrefixCfg(c *RigConfigSyncCheck, ctx *CheckContext, rigsConfig *config.RigsConfig) error {
	for _, rigName := range c.missingPrefixCfg {
		if err := addIssuePrefixToConfigYaml(ctx, rigsConfig, rigName); err != nil {
			return err
		}
	}
	return nil
}

func addIssuePrefixToConfigYaml(ctx *CheckContext, rigsConfig *config.RigsConfig, rigName string) error {
	entry, ok := rigsConfig.Rigs[rigName]
	if !ok || entry.BeadsConfig == nil {
		return nil
	}
	configYamlPath := filepath.Join(doltserver.FindRigBeadsDir(ctx.TownRoot, rigName), "config.yaml")
	data, err := os.ReadFile(configYamlPath)
	if err != nil {
		return nil
	}
	content := string(data)
	if strings.Contains(content, "issue-prefix:") {
		return nil
	}
	content = insertIssuePrefix(content, entry.BeadsConfig.Prefix)
	if err := os.WriteFile(configYamlPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("could not update config.yaml for %s: %w", rigName, err)
	}
	return nil
}

func insertIssuePrefix(content, prefix string) string {
	if strings.Contains(content, "# issue-prefix:") {
		return strings.Replace(content, "# issue-prefix: \"\"", fmt.Sprintf("issue-prefix: %q", prefix), 1)
	}
	return content + fmt.Sprintf("\nissue-prefix: %q\n", prefix)
}

func fixMissingExportCfg(c *RigConfigSyncCheck, ctx *CheckContext, rigsConfig *config.RigsConfig) error {
	for _, rigName := range c.missingExportCfg {
		entry, ok := rigsConfig.Rigs[rigName]
		if !ok || entry.BeadsConfig == nil {
			continue
		}
		beadsDir := doltserver.FindRigBeadsDir(ctx.TownRoot, rigName)
		if err := beads.EnsureConfigYAML(beadsDir, entry.BeadsConfig.Prefix); err != nil {
			return fmt.Errorf("could not update export.auto for %s: %w", rigName, err)
		}
	}
	return nil
}

func fixMissingMetadata(c *RigConfigSyncCheck, ctx *CheckContext) error {
	for _, rigName := range c.missingMetadata {
		beadsDir := doltserver.FindRigBeadsDir(ctx.TownRoot, rigName)
		if err := doltserver.EnsureMetadataForBeadsDir(ctx.TownRoot, beadsDir, rigName, rigName); err != nil {
			return fmt.Errorf("could not write metadata.json for %s: %w", rigName, err)
		}
	}
	return nil
}

func fixRigDoltState(c *RigConfigSyncCheck, ctx *CheckContext, rigsConfig *config.RigsConfig) error {
	if err := fixMissingDoltDBs(c, ctx, rigsConfig); err != nil {
		return err
	}
	return fixDBNameMismatches(c, ctx)
}

func fixMissingDoltDBs(c *RigConfigSyncCheck, ctx *CheckContext, rigsConfig *config.RigsConfig) error {
	for _, rigName := range c.missingDoltDB {
		if err := initMissingDoltDB(ctx, rigsConfig, rigName); err != nil {
			return err
		}
	}
	return nil
}

func initMissingDoltDB(ctx *CheckContext, rigsConfig *config.RigsConfig, rigName string) error {
	entry, ok := rigsConfig.Rigs[rigName]
	if !ok || entry.BeadsConfig == nil {
		return nil
	}
	cmdDir, beadsDir := rigBdInitPaths(ctx, rigName)
	exists, err := doltDatabaseExists(ctx, rigName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: skipping Dolt DB initialization for %s because database status could not be verified: %v\n", rigName, err)
		return nil
	}
	if exists {
		return nil
	}
	return runBdInitForRig(ctx, rigName, entry.BeadsConfig.Prefix, cmdDir, beadsDir)
}

func rigBdInitPaths(ctx *CheckContext, rigName string) (cmdDir, beadsDir string) {
	rigPath := filepath.Join(ctx.TownRoot, rigName)
	beadsDir = doltserver.FindRigBeadsDir(ctx.TownRoot, rigName)
	cmdDir = rigPath
	mayorRigPath := filepath.Join(rigPath, "mayor", "rig")
	mayorBeads := filepath.Join(mayorRigPath, ".beads")
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		if _, statErr := os.Stat(mayorRigPath); statErr == nil {
			beadsDir = mayorBeads
		}
	}
	if beadsDir == mayorBeads {
		cmdDir = mayorRigPath
	}
	return cmdDir, beadsDir
}

func runBdInitForRig(ctx *CheckContext, rigName, prefix, cmdDir, beadsDir string) error {
	doltCfg := doltserver.DefaultConfig(ctx.TownRoot)
	destroyToken := fmt.Sprintf("DESTROY-%s", prefix)
	cmd := beads.Spawn("init", "--prefix", prefix, "--database", rigName, "--server", "--server-port", strconv.Itoa(doltCfg.Port), "--force", "--destroy-token="+destroyToken)
	cmd.Dir = cmdDir
	cmd.Env = append(stripEnvPrefixes(os.Environ(), "BEADS_DIR=", "BEADS_DB=", "BEADS_DOLT_SERVER_DATABASE="),
		"BEADS_DIR="+beadsDir,
		"BEADS_DOLT_SERVER_DATABASE="+rigName,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not initialize Dolt DB for %s: %w\n%s", rigName, err, string(output))
	}
	return nil
}

func fixDBNameMismatches(c *RigConfigSyncCheck, ctx *CheckContext) error {
	renamedDBs := false
	for _, mismatch := range c.dbNameMismatches {
		didRename, err := repairDBNameMismatch(ctx, mismatch)
		if err != nil {
			return err
		}
		if didRename {
			renamedDBs = true
		}
	}
	if !renamedDBs {
		return nil
	}
	return restartDoltIfStable(ctx)
}

func repairDBNameMismatch(ctx *CheckContext, mismatch dbMismatch) (bool, error) {
	metadataPath := filepath.Join(doltserver.FindRigBeadsDir(ctx.TownRoot, mismatch.rigName), "metadata.json")
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return false, fmt.Errorf("could not read metadata.json for %s: %w", mismatch.rigName, err)
	}
	var metadata map[string]interface{}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return false, fmt.Errorf("could not parse metadata.json for %s: %w", mismatch.rigName, err)
	}
	metadata["dolt_database"] = mismatch.expectedDB
	newMetadata, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return false, fmt.Errorf("could not serialize metadata.json for %s: %w", mismatch.rigName, err)
	}
	if err := os.WriteFile(metadataPath, newMetadata, 0644); err != nil {
		return false, fmt.Errorf("could not write metadata.json for %s: %w", mismatch.rigName, err)
	}
	return renameDoltDataDir(ctx, mismatch)
}

func renameDoltDataDir(ctx *CheckContext, mismatch dbMismatch) (bool, error) {
	dataDir := filepath.Join(ctx.TownRoot, ".dolt-data")
	oldDBPath := filepath.Join(dataDir, mismatch.currentDB)
	newDBPath := filepath.Join(dataDir, mismatch.expectedDB)
	if _, err := os.Stat(oldDBPath); err != nil {
		return false, nil
	}
	if _, err := os.Stat(newDBPath); err == nil {
		return false, nil
	}
	if err := os.Rename(oldDBPath, newDBPath); err != nil {
		return false, fmt.Errorf("could not rename database %s to %s: %w", mismatch.currentDB, mismatch.expectedDB, err)
	}
	return true, nil
}

func restartDoltIfStable(ctx *CheckContext) error {
	running, pid, _ := doltserver.IsRunning(ctx.TownRoot)
	if !running || pid <= 0 || !doltServerIsStable(ctx) {
		return nil
	}
	if err := doltserver.Stop(ctx.TownRoot); err != nil {
		return fmt.Errorf("could not stop Dolt server for restart: %w", err)
	}
	if err := doltserver.Start(ctx.TownRoot); err != nil {
		return fmt.Errorf("could not restart Dolt server: %w", err)
	}
	return nil
}

func doltServerIsStable(ctx *CheckContext) bool {
	const minStableAge = 60 * time.Second
	state, _ := doltserver.LoadState(ctx.TownRoot)
	return state == nil || state.StartedAt.IsZero() || time.Since(state.StartedAt) >= minStableAge
}

func fixMissingRigBeads(c *RigConfigSyncCheck, ctx *CheckContext) error {
	for _, info := range c.missingRigBeads {
		if err := createMissingRigBead(ctx, info); err != nil {
			return err
		}
	}
	return nil
}

func createMissingRigBead(ctx *CheckContext, info rigBeadInfo) error {
	rigPath := filepath.Join(ctx.TownRoot, info.rigName)
	beadsDir := doltserver.FindRigBeadsDir(ctx.TownRoot, info.rigName)
	bd := beads.NewWithBeadsDir(rigPath, beadsDir)
	fields := &beads.RigFields{
		Repo:   info.gitURL,
		Prefix: info.prefix,
		State:  beads.RigStateActive,
	}
	if _, err := bd.CreateRigBead(info.rigName, fields); err != nil {
		return fmt.Errorf("could not create rig bead for %s: %w", info.rigName, err)
	}
	rigBeadID := fmt.Sprintf("%s-rig-%s", info.prefix, info.rigName)
	cmd := beads.Spawn("label", rigBeadID, "--add", "status:docked")
	cmd.Dir = rigPath
	cmd.Env = append(stripEnvPrefixes(os.Environ(), "BEADS_DIR="), "BEADS_DIR="+beadsDir)
	_ = cmd.Run()
	return nil
}

func doltDatabaseExists(ctx *CheckContext, dbName string) (bool, error) {
	databases, err := doltserver.ListDatabases(ctx.TownRoot)
	if err != nil {
		return false, err
	}
	for _, db := range databases {
		if db == dbName {
			return true, nil
		}
	}
	return false, nil
}

func rigBeadExists(rigBeadID, rigPath, beadsDir string) bool {
	cmd := beads.Spawn("show", rigBeadID, "--json")
	cmd.Dir = rigPath
	cmd.Env = append(stripEnvPrefixes(os.Environ(), "BEADS_DIR="), "BEADS_DIR="+beadsDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), rigBeadID)
}
