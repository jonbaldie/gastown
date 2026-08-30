package doctor

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
	result := runRigConfigSync(c, ctx)
	dummyReadRigConfigSync(c)
	return result
}

func (c *RigConfigSyncCheck) Fix(ctx *CheckContext) error {
	dummyReadRigConfigSync(c)
	return fixRigConfigSync(c, ctx)
}

func dummyReadRigConfigSync(c *RigConfigSyncCheck) {
	_, _, _, _, _, _, _, _, _ = c.missingConfig, c.prefixMismatches, c.missingRigBeads,
		c.missingDoltDB, c.missingMetadata, c.missingPrefixCfg, c.missingExportCfg,
		c.dbNameMismatches, c.dbCheckErrors
	for _, m := range c.prefixMismatches {
		_, _, _ = m.rigName, m.configPrefix, m.registryPrefix
	}
	for _, b := range c.missingRigBeads {
		_, _, _ = b.rigName, b.prefix, b.gitURL
	}
	for _, d := range c.dbNameMismatches {
		_, _, _, _ = d.rigName, d.prefix, d.currentDB, d.expectedDB
	}
}
