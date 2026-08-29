package daemon

// DefaultLifecycleConfig returns a DaemonPatrolConfig with sensible defaults
// for the six-stage Dolt lifecycle (CREATE → LIVE → CLOSE → DECAY → COMPACT → FLATTEN).
//
// All patrols are enabled with conservative intervals:
//   - Wisp Reaper (DECAY): every 30m, delete closed wisps after 7d
//   - Compactor Dog (COMPACT): every 24h, threshold 2000 commits
//   - Checkpoint Dog: every 10m, auto-commit dirty polecat worktrees
//   - Doctor Dog (health): every 5m
//   - JSONL Git Backup: every 15m
//   - Dolt Filesystem Backup: every 15m
//   - Scheduled Maintenance (FLATTEN): daily at 03:00, threshold 1000
//   - Main Branch Test: every 30m, 10m timeout per rig
func DefaultLifecycleConfig() *DaemonPatrolConfig {
	threshold := 1000
	scrub := true
	return &DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:      true,
				IntervalStr:  "30m",
				MaxAgeStr:    "24h",
				DeleteAgeStr: "168h", // 7 days
			},
			CompactorDog: &CompactorDogConfig{
				Enabled:     true,
				IntervalStr: "24h",
				Threshold:   defaultCompactorCommitThreshold,
			},
			CheckpointDog: &CheckpointDogConfig{
				Enabled:     true,
				IntervalStr: "10m",
			},
			DoctorDog: &DoctorDogConfig{
				Enabled:     true,
				IntervalStr: "5m",
			},
			JsonlGitBackup: &JsonlGitBackupConfig{
				Enabled:     true,
				IntervalStr: "15m",
				Scrub:       &scrub,
			},
			DoltPatrols: DoltPatrols{
				DoltBackup: &DoltBackupConfig{
					Enabled:     true,
					IntervalStr: "15m",
				},
			},
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled:   true,
				Window:    "03:00",
				Interval:  "daily",
				Threshold: &threshold,
			},
			MainBranchTest: &MainBranchTestConfig{
				Enabled:     true,
				IntervalStr: "30m",
				TimeoutStr:  "10m",
			},
			CorePatrols: CorePatrols{
				Handler: &PatrolConfig{
					Enabled: true,
				},
			},
		},
	}
}

// EnsureLifecycleDefaults populates missing patrol configuration with sensible
// defaults. It never overwrites existing user configuration — only fills in
// patrols that are nil (not yet configured).
//
// Returns true if any defaults were applied (caller should persist the config).
func EnsureLifecycleDefaults(config *DaemonPatrolConfig) bool {
	if config == nil {
		return false
	}

	defaults := DefaultLifecycleConfig()

	if config.Patrols == nil {
		config.Patrols = defaults.Patrols
		return true
	}

	p := config.Patrols
	d := defaults.Patrols
	return applyPatrolDefaults(p, d)
}

func applyPatrolDefaults(p, d *PatrolsConfig) bool {
	patrols := []struct {
		missing func() bool
		apply   func()
	}{
		{func() bool { return p.WispReaper == nil }, func() { p.WispReaper = d.WispReaper }},
		{func() bool { return p.CompactorDog == nil }, func() { p.CompactorDog = d.CompactorDog }},
		{func() bool { return p.CheckpointDog == nil }, func() { p.CheckpointDog = d.CheckpointDog }},
		{func() bool { return p.DoctorDog == nil }, func() { p.DoctorDog = d.DoctorDog }},
		{func() bool { return p.JsonlGitBackup == nil }, func() { p.JsonlGitBackup = d.JsonlGitBackup }},
		{func() bool { return p.DoltBackup == nil }, func() { p.DoltBackup = d.DoltBackup }},
		{func() bool { return p.ScheduledMaintenance == nil }, func() { p.ScheduledMaintenance = d.ScheduledMaintenance }},
		{func() bool { return p.MainBranchTest == nil }, func() { p.MainBranchTest = d.MainBranchTest }},
		{func() bool { return p.Handler == nil }, func() { p.Handler = d.Handler }},
	}

	changed := false
	for _, patrol := range patrols {
		if patrol.missing() {
			patrol.apply()
			changed = true
		}
	}
	return changed
}

// EnsureLifecycleConfigFile loads the patrol config from disk (or creates a new
// one if it doesn't exist), applies lifecycle defaults for any unconfigured
// patrols, and saves the result. Returns nil on success.
//
// This is the top-level function called by gt init and gt up.
func EnsureLifecycleConfigFile(townRoot string) error {
	config := LoadPatrolConfig(townRoot)
	if config == nil {
		config = DefaultLifecycleConfig()
		return SavePatrolConfig(townRoot, config)
	}

	if EnsureLifecycleDefaults(config) {
		return SavePatrolConfig(townRoot, config)
	}

	return nil // Already configured, nothing to do
}
