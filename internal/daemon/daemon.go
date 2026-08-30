package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	beadsdk "github.com/jonbaldie/beads"
	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/boot"
	agentconfig "github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/deacon"
	"github.com/jonbaldie/gastown/internal/deps"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/estop"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/feed"
	gitpkg "github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/mayor"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/refinery"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/telemetry"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/util"
	"github.com/jonbaldie/gastown/internal/wisp"
	"github.com/jonbaldie/gastown/internal/witness"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Daemon is the town-level background service.
// It ensures patrol agents (Deacon, Witnesses) are running and detects failures.
// This is recovery-focused: normal wake is handled by feed subscription (bd activity --follow).
// The daemon is the safety net for dead sessions, GUPP violations, and orphaned work.
type Daemon struct {
	daemonLifecycle
	config        *Config
	tmux          *tmux.Tmux
	logger        *log.Logger
	ctx           context.Context
	curator       *feed.Curator
	convoyManager *ConvoyManager

	// SyncFailures tracks consecutive git pull failures per workdir.
	// Used to escalate logging from WARN to ERROR after repeated failures.
	// Only accessed from heartbeat loop goroutine - no sync needed.
	SyncFailures map[string]int

	// PATCH-006: Resolved binary paths to avoid PATH issues in subprocesses.
	gtPath string
	bdPath string

	daemonOperationalState
}

// daemonOperationalState groups patrol, recovery, and telemetry state whose
// selectors remain promoted on Daemon for the heartbeat implementation.
type daemonOperationalState struct {
	daemonPatrolState
	daemonRecoveryState
	daemonTelemetryState
}

type daemonPatrolState struct {
	patrolConfig            *DaemonPatrolConfig
	beadsStores             map[string]beadsdk.Storage
	krcPruner               *KRCPruner
	disabledPatrols         map[string]bool
	deaconLastStarted       time.Time
	bootLastSpawned         time.Time
	mayorZombieCount        int
	rigPool                 *RigWorkerPool
	knownRigsCache          []string
	knownRigsCacheValid     bool
	legacySocketCleanupOnce sync.Once
}

type daemonRecoveryState struct {
	doltServer         *DoltServerManager
	deathsMu           sync.Mutex
	recentDeaths       []sessionDeath
	restartTracker     *RestartTracker
	JSONLPushFailures  int
	lastDoctorMolTime  time.Time
	LastMaintenanceRun time.Time
}

type daemonTelemetryState struct {
	otelProvider *telemetry.Provider
	metrics      *daemonMetrics
}

type daemonLifecycle struct {
	cancel context.CancelFunc
}

// Stop signals the daemon to stop.
func (l *daemonLifecycle) Stop() {
	if l != nil && l.cancel != nil {
		l.cancel()
	}
}

// sessionDeath records a detected session death for mass death analysis.
type sessionDeath struct {
	sessionName string
	timestamp   time.Time
}

// Mass death detection parameters — these are fallback defaults.
// Prefer config.OperationalConfig.GetDaemonConfig() accessors when
// a TownSettings is available (loaded via d.loadOperationalConfig()).
const (
	massDeathWindow    = 30 * time.Second // Time window to detect mass death
	massDeathThreshold = 3                // Number of deaths to trigger alert

	// doctorMolCooldown is the minimum interval between mol-dog-doctor molecules.
	// Configurable via operational.daemon.doctor_mol_cooldown.
	doctorMolCooldown = 5 * time.Minute
)

const beadsModulePath = "github.com/jonbaldie/beads"

var semverPattern = regexp.MustCompile(`v?(\d+\.\d+\.\d+)`)

func daemonPathCandidates(home, exePath string) []string {
	candidates := make([]string, 0, 5)
	if exePath != "" {
		candidates = append(candidates, filepath.Dir(exePath))
	}
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local/bin"),
			filepath.Join(home, "bin"),
		)
	}
	return append(candidates,
		"/opt/homebrew/bin",
		"/usr/local/bin",
	)
}

func augmentDaemonPath(logger *log.Logger) {
	exePath := ""
	if exe, err := os.Executable(); err == nil {
		exePath = exe
	}
	extras := daemonPathCandidates(os.Getenv("HOME"), exePath)
	if len(extras) == 0 {
		return
	}

	current := os.Getenv("PATH")
	parts := strings.Split(current, string(os.PathListSeparator))
	seen := make(map[string]struct{}, len(parts)+len(extras))
	for _, p := range parts {
		seen[p] = struct{}{}
	}
	additions := make([]string, 0, len(extras))
	for _, extra := range extras {
		if _, ok := seen[extra]; ok {
			continue
		}
		if info, statErr := os.Stat(extra); statErr == nil && info.IsDir() {
			additions = append(additions, extra)
			seen[extra] = struct{}{}
		}
	}
	augmented := append(additions, parts...)
	newPath := strings.Join(augmented, string(os.PathListSeparator))
	if newPath != current {
		_ = os.Setenv("PATH", newPath)
		logger.Printf("PATCH-007: augmented daemon PATH with user/local bin dirs (was=%q, now=%q)", current, newPath)
	}
}

var cleanupLegacySocketsForDaemon = func(townRoot string) (int, int) {
	defaultCleaned := session.CleanupLegacyDefaultSocket()
	baseCleaned := session.CleanupLegacyBaseSocket(townRoot)
	return defaultCleaned, baseCleaned
}

// New creates a new daemon instance.
func New(config *Config) (*Daemon, error) {
	logger, err := newDaemonLogger(config)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())

	initializeDaemonEnvironment(config, logger)
	patrolConfig, disabledPatrols := loadDaemonPatrolConfig(config, logger)
	doltServer := initializeDoltServer(config, patrolConfig, logger)
	propagateDaemonDoltEnv(config, logger)
	gtPath := resolveDaemonBinary("gt", logger)
	bdPath := resolveDaemonBinary("bd", logger)
	restartTracker := initializeRestartTracker(config, patrolConfig, logger)
	otelProvider, dm := initializeDaemonTelemetry(ctx, logger)

	d := &Daemon{
		daemonLifecycle: daemonLifecycle{cancel: cancel},
		config:          config,
		tmux:            tmux.NewTmux(),
		logger:          logger,
		ctx:             ctx,
		gtPath:          gtPath,
		bdPath:          bdPath,
		daemonOperationalState: daemonOperationalState{
			daemonPatrolState: daemonPatrolState{
				patrolConfig:    patrolConfig,
				disabledPatrols: disabledPatrols,
				rigPool:         newRigWorkerPool(0, 0, logger), // defaults: 10 workers, 30s timeout
			},
			daemonRecoveryState: daemonRecoveryState{
				doltServer:     doltServer,
				restartTracker: restartTracker,
			},
			daemonTelemetryState: daemonTelemetryState{
				otelProvider: otelProvider,
				metrics:      dm,
			},
		},
	}
	return d, nil
}

func newDaemonLogger(config *Config) (*log.Logger, error) {
	if err := os.MkdirAll(filepath.Dir(config.LogFile), 0755); err != nil {
		return nil, fmt.Errorf("creating daemon directory: %w", err)
	}
	logWriter := &lumberjack.Logger{
		Filename:   config.LogFile,
		MaxSize:    100, // megabytes
		MaxBackups: 3,
		MaxAge:     7, // days
		Compress:   true,
	}
	return log.New(logWriter, "", log.LstdFlags), nil
}

func initializeDaemonEnvironment(config *Config, logger *log.Logger) {
	augmentDaemonPath(logger)
	if err := session.InitRegistry(config.TownRoot); err != nil {
		logger.Printf("Warning: failed to initialize town registry: %v", err)
	}
	os.Setenv("GT_TOWN_ROOT", config.TownRoot)

	t := tmux.NewTmux()
	if err := t.SetGlobalEnvironment("GT_TOWN_ROOT", config.TownRoot); err != nil {
		logger.Printf("Warning: failed to set GT_TOWN_ROOT in tmux global env: %v", err)
	}
	for _, k := range agentconfig.IdentityEnvVars {
		_ = t.UnsetGlobalEnvironment(k)
	}
}

func loadDaemonPatrolConfig(config *Config, logger *log.Logger) (*DaemonPatrolConfig, map[string]bool) {
	if err := EnsureLifecycleConfigFile(config.TownRoot); err != nil {
		logger.Printf("Warning: failed to ensure lifecycle config: %v", err)
	}
	patrolConfig := LoadPatrolConfig(config.TownRoot)
	if patrolConfig != nil {
		logger.Printf("Loaded patrol config from %s", PatrolConfigFile(config.TownRoot))
		for k, v := range patrolConfig.Env {
			os.Setenv(k, v)
			logger.Printf("Set env %s=%s from daemon.json", k, v)
		}
	}
	agentconfig.ApplyConfiguredDoltEnv(config.TownRoot)

	disabledPatrols := loadDisabledPatrolsFromTownSettings(config.TownRoot)
	if len(disabledPatrols) > 0 {
		names := make([]string, 0, len(disabledPatrols))
		for k := range disabledPatrols {
			names = append(names, k)
		}
		logger.Printf("Patrols disabled via town settings: %v", names)
	}
	return patrolConfig, disabledPatrols
}

func initializeDoltServer(config *Config, patrolConfig *DaemonPatrolConfig, logger *log.Logger) *DoltServerManager {
	if patrolConfig == nil || patrolConfig.Patrols == nil || patrolConfig.Patrols.DoltServer == nil {
		return nil
	}
	doltServer := NewDoltServerManager(config.TownRoot, patrolConfig.Patrols.DoltServer, logger.Printf)
	if !doltServer.IsEnabled() {
		return doltServer
	}
	logger.Printf("Dolt server management enabled (port %d)", doltServer.config.Port)
	applyDoltServerConfigEnv(doltServer.config)
	return doltServer
}

func propagateDaemonDoltEnv(config *Config, logger *log.Logger) {
	portStr := os.Getenv("GT_DOLT_PORT")
	if portStr == "" {
		if port := agentconfig.ResolveConfiguredDoltPort(config.TownRoot); port > 0 {
			portStr = strconv.Itoa(port)
			os.Setenv("GT_DOLT_PORT", portStr)
			logger.Printf("Set GT_DOLT_PORT=%s from resolved Dolt config (fallback)", portStr)
		}
	}
	if portStr != "" {
		os.Setenv("BEADS_DOLT_SERVER_PORT", portStr)
		os.Setenv("BEADS_DOLT_PORT", portStr)
	}
	applyConfiguredDoltHostEnv(config.TownRoot, logger.Printf)
}

func resolveDaemonBinary(name string, logger *log.Logger) string {
	path, err := exec.LookPath(name)
	if err == nil {
		return path
	}
	logger.Printf("Warning: %s not found in PATH, subprocess calls may fail", name)
	return name
}

func initializeRestartTracker(config *Config, patrolConfig *DaemonPatrolConfig, logger *log.Logger) *RestartTracker {
	var trackerConfig RestartTrackerConfig
	if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.RestartTracker != nil {
		trackerConfig = *patrolConfig.Patrols.RestartTracker
	}
	tracker := NewRestartTracker(config.TownRoot, trackerConfig)
	if err := tracker.Load(); err != nil {
		logger.Printf("Warning: failed to load restart state: %v", err)
	}
	return tracker
}

func initializeDaemonTelemetry(ctx context.Context, logger *log.Logger) (*telemetry.Provider, *daemonMetrics) {
	provider, err := telemetry.Init(ctx, "gastown-daemon", "")
	if err != nil {
		logger.Printf("Warning: telemetry init failed: %v", err)
	}
	if provider == nil {
		return nil, nil
	}
	metrics, err := newDaemonMetrics()
	if err != nil {
		logger.Printf("Warning: failed to register daemon metrics: %v", err)
		return provider, nil
	}
	metricsURL := os.Getenv(telemetry.EnvMetricsURL)
	if metricsURL == "" {
		metricsURL = telemetry.DefaultMetricsURL
	}
	logsURL := os.Getenv(telemetry.EnvLogsURL)
	if logsURL == "" {
		logsURL = telemetry.DefaultLogsURL
	}
	logger.Printf("Telemetry active (metrics → %s, logs → %s)", metricsURL, logsURL)
	return provider, metrics
}

func applyDoltServerConfigEnv(config *DoltServerConfig) {
	if config == nil {
		return
	}
	if config.Port > 0 {
		portStr := strconv.Itoa(config.Port)
		os.Setenv("GT_DOLT_PORT", portStr)
		os.Setenv("BEADS_DOLT_SERVER_PORT", portStr)
		os.Setenv("BEADS_DOLT_PORT", portStr)
	}
	if config.Host != "" {
		os.Setenv("GT_DOLT_HOST", config.Host)
		os.Setenv("BEADS_DOLT_SERVER_HOST", config.Host)
	}
}

func applyConfiguredDoltHostEnv(townRoot string, logf func(format string, v ...interface{})) {
	if host := agentconfig.ResolveConfiguredDoltHost(townRoot); host != "" {
		os.Setenv("GT_DOLT_HOST", host)
		os.Setenv("BEADS_DOLT_SERVER_HOST", host)
		if logf != nil {
			logf("Set BEADS_DOLT_SERVER_HOST=%s from resolved Dolt host", host)
		}
		return
	}
	if _, _, ok := agentconfig.ManagedDoltEndpoint(townRoot); ok {
		os.Unsetenv("GT_DOLT_HOST")
	}
	os.Unsetenv("BEADS_DOLT_SERVER_HOST")
}

func cleanupLegacySocketSessions(d *Daemon) {
	d.legacySocketCleanupOnce.Do(func() {
		defaultCleaned, baseCleaned := cleanupLegacySocketsForDaemon(d.config.TownRoot)
		if defaultCleaned > 0 {
			d.logger.Printf("legacy_socket_cleanup: cleaned %d session(s) from default socket", defaultCleaned)
		}
		if baseCleaned > 0 {
			d.logger.Printf("legacy_socket_cleanup: cleaned %d session(s) from basename socket", baseCleaned)
		}
	})
}

// Run starts the daemon main loop.
func (d *Daemon) Run() (err error) {
	return runDaemon(d)
}

// recoveryHeartbeatInterval returns the config-driven recovery heartbeat interval.
// Normal wake is handled by feed subscription (bd activity --follow).
// The daemon is a safety net for dead sessions, GUPP violations, and orphaned work.
// Default: 3 minutes — fast enough to detect stuck agents promptly.
func recoveryHeartbeatInterval(d *Daemon) time.Duration {
	return d.loadOperationalConfig().GetDaemonConfig().RecoveryHeartbeatIntervalD()
}

// heartbeat performs one heartbeat cycle.
// The daemon is recovery-focused: it ensures agents are running and detects failures.
// Normal wake is handled by feed subscription (bd activity --follow).
// The daemon is the safety net for edge cases:
// - Dead sessions that need restart
// - Agents with work-on-hook not progressing (GUPP violation)
// - Orphaned work (assigned to dead agents)
func heartbeat(d *Daemon, state *State) {
	// Skip heartbeat if shutdown is in progress.
	// This prevents the daemon from fighting shutdown by auto-restarting killed agents.
	// The shutdown.lock file is created by gt down before terminating sessions.
	if isShutdownInProgress(d) {
		d.logger.Println("Shutdown in progress, skipping heartbeat")
		return
	}

	// Skip agent management if E-stop is active.
	// The daemon stays alive (to maintain Dolt, etc.) but does NOT
	// restart any agents. This prevents fighting the E-stop by auto-spawning
	// sessions that were intentionally frozen.
	if estop.IsActive(d.config.TownRoot) {
		d.logger.Println("E-STOP active, skipping agent management")
		return
	}

	d.metrics.RecordHeartbeat(d.ctx)
	d.logger.Println("Heartbeat starting (recovery-focused)")

	// Invalidate the per-tick rigs cache so this heartbeat re-reads from disk.
	// Within a tick the cache coalesces the ~10 getKnownRigs() call sites into
	// a single read; invalidating here ensures we pick up rigs.json changes
	// between ticks.
	invalidateKnownRigsCache(d)

	// 0a. Reload prefix registry so new/changed rigs get correct session names.
	// Without this, rigs added after daemon startup get the "gt" default prefix,
	// causing ghost sessions like gt-witness instead of ti-witness. (hq-ouz, hq-eqf, hq-3i4)
	if err := session.InitRegistry(d.config.TownRoot); err != nil {
		d.logger.Printf("Warning: failed to reload prefix registry: %v", err)
	}

	// 0b. Kill ghost sessions left over from stale registry (default "gt" prefix).
	killDefaultPrefixGhosts(d)

	// 0. Ensure Dolt server is running (if configured)
	// This must happen before beads operations that depend on Dolt.
	ensureDoltServerRunning(d)

	// 1. Ensure Deacon is running (restart if dead)
	// Check patrol config - can be disabled in mayor/daemon.json
	if d.isPatrolActive("deacon") {
		ensureDeaconRunning(d)
	} else {
		d.logger.Printf("Deacon patrol disabled in config, skipping")
		// Kill leftover deacon/boot sessions from before patrol was disabled.
		// Without this, a stale deacon keeps running its own patrol loop,
		// spawning witnesses and refineries despite daemon config. (hq-2mstj)
		killDeaconSessions(d)
	}

	// 2. Poke Boot for intelligent triage (stuck/nudge/interrupt)
	// Boot handles nuanced "is Deacon responsive" decisions
	// Only run if Deacon patrol is enabled
	if d.isPatrolActive("deacon") {
		ensureBootRunning(d)
	}

	// 3. Direct Deacon heartbeat check (belt-and-suspenders)
	// Boot may not detect all stuck states; this provides a fallback
	// Only run if Deacon patrol is enabled
	if d.isPatrolActive("deacon") {
		checkDeaconHeartbeat(d)
	}

	// 4. Ensure Witnesses are running for all rigs (restart if dead)
	// Check patrol config - can be disabled in mayor/daemon.json
	if d.isPatrolActive("witness") {
		ensureWitnessesRunning(d)
	} else {
		d.logger.Printf("Witness patrol disabled in config, skipping")
		// Kill leftover witness sessions from before patrol was disabled. (hq-2mstj)
		killWitnessSessions(d)
	}

	// 5. Ensure Refineries are running for all rigs (restart if dead)
	// Check patrol config - can be disabled in mayor/daemon.json
	// Pressure-gated: refineries consume API credits, defer when system is loaded.
	if d.isPatrolActive("refinery") {
		if p := d.checkPressure("refinery"); !p.OK {
			d.logger.Printf("Deferring refinery spawn: %s", p.Reason)
		} else {
			ensureRefineriesRunning(d)
		}
	} else {
		d.logger.Printf("Refinery patrol disabled in config, skipping")
		// Kill leftover refinery sessions from before patrol was disabled. (hq-2mstj)
		killRefinerySessions(d)
	}

	// 6. Ensure Mayor is running (restart if dead)
	ensureMayorRunning(d)

	// 6.5. Handle Dog lifecycle: cleanup stuck dogs and dispatch plugins
	// Pressure-gated: dog dispatch spawns new agent sessions.
	if d.isPatrolActive("handler") {
		if p := d.checkPressure("dog"); !p.OK {
			d.logger.Printf("Deferring dog dispatch: %s", p.Reason)
			// Still run cleanup phases (stuck/stale/idle) — only skip dispatch
			d.handleDogsCleanupOnly()
		} else {
			d.handleDogs()
		}
	} else {
		d.logger.Printf("Handler patrol disabled in config, skipping")
	}

	// 7. Process lifecycle requests
	processLifecycleRequests(d)

	// 9. (Removed) Stale agent check - violated "discover, don't track"

	// 10. Check for GUPP violations (agents with work-on-hook not progressing)
	d.checkGUPPViolations()

	// 11. Check for orphaned work (assigned to dead agents)
	d.checkOrphanedWork()

	// 12. Check polecat session health (proactive crash detection)
	// This validates tmux sessions are still alive for polecats with work-on-hook
	checkPolecatSessionHealth(d)

	// 12b. Reap idle polecat sessions to prevent API slot burn.
	// Polecats transition to IDLE after gt done but sessions stay alive.
	// Kill sessions that have been idle longer than the configured threshold.
	reapIdlePolecats(d)

	// 13. Clean up orphaned claude subagent processes (memory leak prevention)
	// These are Task tool subagents that didn't clean up after completion.
	// This is a safety net - Deacon patrol also does this more frequently.
	cleanupOrphanedProcesses(d)

	// 13. Prune stale local polecat tracking branches across all rig clones.
	// When polecats push branches to origin, other clones create local tracking
	// branches via git fetch. After merge, remote branches are deleted but local
	// branches persist indefinitely. This cleans them up periodically.
	pruneStaleBranches(d)

	// 14. Dispatch scheduled work (capacity-controlled polecat dispatch).
	// Shells out to `gt scheduler run` to avoid circular import between daemon and cmd.
	// Pressure-gated: polecats are the primary resource consumers.
	if p := d.checkPressure("polecat"); !p.OK {
		d.logger.Printf("Deferring polecat dispatch: %s", p.Reason)
	} else {
		dispatchQueuedWork(d)
	}

	// 15. Rotate oversized Dolt logs (copytruncate for child process fds).
	// daemon.log uses lumberjack for automatic rotation; this handles Dolt server logs.
	rotateOversizedLogs(d)

	// Update state
	state.LastHeartbeat = time.Now()
	state.HeartbeatCount++
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save state: %v", err)
	}

	d.logger.Printf("Heartbeat complete (#%d)", state.HeartbeatCount)
}

// rotateOversizedLogs checks Dolt server log files and rotates any that exceed
// the size threshold. Uses copytruncate which is safe for logs held open by
// child processes. Runs every heartbeat but is cheap (just stat calls).
func rotateOversizedLogs(d *Daemon) {
	result := RotateLogs(d.config.TownRoot)
	for _, path := range result.Rotated {
		d.logger.Printf("log_rotation: rotated %s", path)
	}
	for _, err := range result.Errors {
		d.logger.Printf("log_rotation: error: %v", err)
	}
}

// ensureDoltServerRunning ensures the Dolt SQL server is running if configured.
// This provides the backend for beads database access in server mode.
// Option B throttling: pours a mol-dog-doctor molecule only when health check
// warnings are detected, with a 5-minute cooldown to avoid wisp spam.
func ensureDoltServerRunning(d *Daemon) {
	if d.doltServer == nil || !d.doltServer.IsEnabled() {
		return
	}

	if err := d.doltServer.EnsureRunning(); err != nil {
		d.logger.Printf("Error ensuring Dolt server is running: %v", err)
	}

	// Option B throttling: pour mol-dog-doctor only on anomaly with cooldown.
	if warnings := d.doltServer.LastWarnings(); len(warnings) > 0 {
		if time.Since(d.lastDoctorMolTime) >= doctorMolCooldown {
			d.lastDoctorMolTime = time.Now()
			go pourDoctorMolecule(d, warnings)
		}
	}

	// Update OTel gauges with the latest Dolt health snapshot.
	if d.metrics != nil {
		h := doltserver.GetHealthMetrics(d.config.TownRoot)
		d.metrics.UpdateDoltHealth(
			int64(h.Connections),
			int64(h.MaxConnections),
			float64(h.QueryLatency.Milliseconds()),
			h.DiskUsageBytes,
			h.Healthy,
		)
	}
}

// pourDoctorMolecule creates a mol-dog-doctor molecule to track a health anomaly.
// Runs asynchronously — molecule lifecycle is observability, not control flow.
func pourDoctorMolecule(d *Daemon, warnings []string) {
	mol := d.pourDogMolecule(constants.MolDogDoctor, map[string]string{
		"port": strconv.Itoa(d.doltServer.config.Port),
	})
	defer mol.Close()

	// Step 1: probe — connectivity was already checked (we got here because it passed).
	mol.CloseStep("probe")

	// Step 2: inspect — resource checks produced the warnings.
	mol.CloseStep("inspect")

	// Step 3: report — log the warning summary.
	summary := strings.Join(warnings, "; ")
	d.logger.Printf("Doctor molecule: %d warning(s): %s", len(warnings), summary)
	mol.CloseStep("report")
}

// checkAllRigsDolt verifies all rigs are using the Dolt backend.
func checkAllRigsDolt(d *Daemon) error {
	var problems []string

	// Check town-level beads
	townBeadsDir := filepath.Join(d.config.TownRoot, ".beads")
	if backend := readBeadsBackend(townBeadsDir); backend != "" && backend != "dolt" {
		problems = append(problems, fmt.Sprintf(
			"Rig %q is using %s backend.\n  Gas Town requires Dolt. Run: cd %s && bd migrate dolt",
			"town-root", backend, d.config.TownRoot))
	}

	// Check each registered rig
	for _, rigName := range getKnownRigs(d) {
		rigBeadsDir := filepath.Join(d.config.TownRoot, rigName, "mayor", "rig", ".beads")
		if backend := readBeadsBackend(rigBeadsDir); backend != "" && backend != "dolt" {
			rigPath := filepath.Join(d.config.TownRoot, rigName)
			problems = append(problems, fmt.Sprintf(
				"Rig %q is using %s backend.\n  Gas Town requires Dolt. Run: cd %s && bd migrate dolt",
				rigName, backend, rigPath))
		}
	}

	if len(problems) == 0 {
		return nil
	}

	return fmt.Errorf("daemon startup blocked: %d rig(s) not on Dolt backend\n\n  %s",
		len(problems), strings.Join(problems, "\n\n  "))
}

// readBeadsBackend reads the backend field from metadata.json in a beads directory.
// Returns empty string if the directory or metadata doesn't exist.
func readBeadsBackend(beadsDir string) string {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return ""
	}

	var metadata struct {
		Backend string `json:"backend"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return ""
	}

	return metadata.Backend
}

type beadsMetadataReader interface {
	GetMetadata(_ context.Context, _ string) (string, error)
}

type beadsDBAccessor interface {
	DB() *sql.DB
}

// embeddedBeadsVersion returns the semver of the beads module linked into this binary.
// Empty string means build info did not include a parseable module version.
func embeddedBeadsVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path != beadsModulePath {
			continue
		}
		if dep.Replace != nil {
			if version := normalizeSemver(dep.Replace.Version); version != "" {
				return version
			}
		}
		return normalizeSemver(dep.Version)
	}
	return ""
}

func normalizeSemver(version string) string {
	matches := semverPattern.FindStringSubmatch(version)
	if len(matches) != 2 {
		return ""
	}
	return matches[1]
}

func checkBeadsStoreCompatibility(ctx context.Context, stores map[string]beadsdk.Storage, binaryBeadsVersion string) error {
	if len(stores) == 0 {
		return nil
	}

	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)

	var problems []string
	for _, name := range names {
		problem := checkSingleBeadsStoreCompatibility(ctx, name, stores[name], binaryBeadsVersion)
		if problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) == 0 {
		return nil
	}

	remediation := "Upgrade or rebuild `gt` against a newer beads release, or switch to a workspace created by a matching release, then retry `gt daemon start`."
	if binaryBeadsVersion == "" {
		remediation = "Rebuild `gt` or use a release whose embedded beads version matches this workspace, then retry `gt daemon start`."
	}

	return fmt.Errorf("daemon startup blocked: incompatible beads workspace / gt binary combination\n\n  %s\n\n%s",
		strings.Join(problems, "\n  "), remediation)
}

func checkSingleBeadsStoreCompatibility(ctx context.Context, name string, store beadsdk.Storage, binaryBeadsVersion string) string {
	if store == nil {
		return ""
	}

	label := displayBeadsStoreName(name)
	var reasons []string

	if workspaceVersion, err := readStoreBDVersion(ctx, store); err != nil {
		reasons = append(reasons, fmt.Sprintf("cannot read bd_version metadata: %v", err))
	} else if workspaceVersion != "" && binaryBeadsVersion != "" && deps.CompareVersions(workspaceVersion, binaryBeadsVersion) > 0 {
		reasons = append(reasons, fmt.Sprintf("workspace bd_version %s is newer than embedded beads %s", workspaceVersion, binaryBeadsVersion))
	}

	if err := probeStoreEventSchema(ctx, store); err != nil {
		reasons = append(reasons, fmt.Sprintf("event polling probe failed: %v", err))
	}

	if len(reasons) == 0 {
		return ""
	}
	return fmt.Sprintf("%s: %s", label, strings.Join(reasons, "; "))
}

func readStoreBDVersion(ctx context.Context, store beadsdk.Storage) (string, error) {
	if metadataStore, ok := store.(beadsMetadataReader); ok {
		return metadataStore.GetMetadata(ctx, "bd_version")
	}

	dbAccessor, ok := store.(beadsDBAccessor)
	if !ok || dbAccessor.DB() == nil {
		return "", nil
	}

	var version string
	err := dbAccessor.DB().QueryRowContext(ctx, "SELECT value FROM metadata WHERE `key` = 'bd_version'").Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return version, nil
}

func probeStoreEventSchema(ctx context.Context, store beadsdk.Storage) error {
	if dbAccessor, ok := store.(beadsDBAccessor); ok && dbAccessor.DB() != nil {
		for _, table := range []string{"events", "wisp_events"} {
			if err := probeEventTable(ctx, dbAccessor.DB(), table); err != nil {
				return err
			}
		}
		return nil
	}

	// Fall back to the typed API if the store doesn't expose raw SQL.
	_, err := store.GetAllEventsSince(ctx, time.Now().Add(24*time.Hour).UTC())
	return err
}

func probeEventTable(ctx context.Context, db *sql.DB, table string) error {
	query := fmt.Sprintf("SELECT id, created_at FROM %s ORDER BY created_at DESC LIMIT 1", table)

	var (
		id        string
		createdAt time.Time
	)
	err := db.QueryRowContext(ctx, query).Scan(&id, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s table probe: %w", table, err)
	}
	return nil
}

func displayBeadsStoreName(name string) string {
	if name == "hq" {
		return "town-root beads store"
	}
	return fmt.Sprintf("rig %q beads store", name)
}

func closeBeadsStores(logger *log.Logger, stores map[string]beadsdk.Storage) {
	for name, store := range stores {
		if store == nil {
			continue
		}
		if err := store.Close(); err != nil {
			if logger != nil {
				logger.Printf("Convoy: error closing beads store (%s): %v", name, err)
			}
			continue
		}
		if logger != nil {
			logger.Printf("Convoy: closed beads store (%s)", name)
		}
	}
}

// DeaconRole is the role name for the Deacon's handoff bead.
const DeaconRole = "deacon"

// getDeaconSessionName returns the Deacon session name for the daemon's town.
func getDeaconSessionName() string {
	return session.DeaconSessionName()
}

// ensureBootRunning spawns Boot to triage the Deacon.
// Boot is a fresh-each-tick watchdog that decides whether to start/wake/nudge
// the Deacon, centralizing the "when to wake" decision in an agent.
// In degraded mode (no tmux), falls back to mechanical checks.
// bootSpawnCooldown returns the config-driven boot spawn cooldown.
// Boot triage runs are expensive (AI reasoning); if one just ran, skip.
func bootSpawnCooldown(d *Daemon) time.Duration {
	return d.loadOperationalConfig().GetDaemonConfig().BootSpawnCooldownD()
}

func ensureBootRunning(d *Daemon) {
	// Cooldown gate: skip if Boot was spawned recently (fixes #2084)
	if !d.bootLastSpawned.IsZero() && time.Since(d.bootLastSpawned) < bootSpawnCooldown(d) {
		d.logger.Printf("Boot spawned %s ago, within cooldown (%s), skipping",
			time.Since(d.bootLastSpawned).Round(time.Second), bootSpawnCooldown(d))
		return
	}

	// Idle guard: skip if Deacon is healthy AND no beads are actively in flight.
	//
	// Boot's job is to triage a stuck or unresponsive Deacon and to flag stuck
	// in_progress/hooked work. If Deacon has written a fresh heartbeat and no
	// beads are in_progress or hooked, there is nothing to triage.
	//
	// We deliberately do NOT update bootLastSpawned on an idle skip: the cooldown
	// is about rate-limiting real spawns; the idle check should re-run every
	// heartbeat so Boot fires promptly when work actually appears.
	hb := deacon.ReadHeartbeat(d.config.TownRoot)
	if hb != nil && hb.IsFresh() && !hasActiveWork(d) {
		d.logger.Println("Boot spawn skipped: Deacon is healthy and no active work in flight")
		return
	}

	b := boot.New(d.config.TownRoot)

	// Idle suppression: if Boot's last run found deacon healthy ("nothing"),
	// suppress spawning for longer to avoid burning API calls. (fixes gt-qu883c)
	idleSuppression := d.loadOperationalConfig().GetDaemonConfig().BootIdleSuppressionD()
	if status, err := b.LoadStatus(); err == nil && status.LastAction == "nothing" {
		if !status.CompletedAt.IsZero() && time.Since(status.CompletedAt) < idleSuppression {
			d.logger.Printf("Boot last reported 'nothing' %s ago, within idle suppression (%s), skipping",
				time.Since(status.CompletedAt).Round(time.Second), idleSuppression)
			return
		}
	}

	// Check for degraded mode
	degraded := os.Getenv("GT_DEGRADED") == "true"
	if degraded || !d.tmux.IsAvailable() {
		// In degraded mode, run mechanical triage directly
		d.logger.Println("Degraded mode: running mechanical Boot triage")
		runDegradedBootTriage(d, b)
		return
	}

	// Idle check: run gt-idle-check to see if the system needs waking.
	// If idle (all rigs parked, no polecats, deacon alive), skip the expensive
	// Claude Boot session and use degraded mechanical triage instead.
	// This saves ~480 Claude sessions/day when Gas Town is not in active use.
	idleCheckBin := filepath.Join(d.config.TownRoot, "bin", "gt-idle-check")
	if _, err := os.Stat(idleCheckBin); err == nil {
		//nolint:gosec // G204: path is constructed from config
		cmd := exec.Command(idleCheckBin)
		cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s:%s",
			filepath.Join(d.config.TownRoot, "bin"), os.Getenv("PATH")))
		if output, err := cmd.CombinedOutput(); err == nil {
			// Exit 0 = idle, use degraded triage (zero tokens)
			runDegradedBootTriage(d, b)
			return
		} else {
			// Exit 1 = needs waking, proceed to full Claude Boot
			d.logger.Printf("Idle check: waking — %s", strings.TrimSpace(string(output)))
		}
	}

	// Spawn Boot in a fresh tmux session
	d.logger.Println("Spawning Boot for triage...")
	if err := b.Spawn(""); err != nil {
		d.logger.Printf("Error spawning Boot: %v, falling back to direct Deacon check", err)
		// Fallback: ensure Deacon is running directly
		ensureDeaconRunning(d)
		return
	}

	d.bootLastSpawned = time.Now()
	d.logger.Println("Boot spawned successfully")
}

// hasActiveWork returns true if any bead store has in_progress or hooked beads.
// These are the only states Boot can meaningfully act on: in_progress work may be
// stuck, and hooked work is waiting on a polecat that may have died.
//
// Returns true conservatively on error or when no stores are available, so the
// caller falls through to spawn Boot rather than suppressing it incorrectly.
func hasActiveWork(d *Daemon) bool {
	if len(d.beadsStores) == 0 {
		// No stores open — cannot inspect; let Boot run to be safe.
		return true
	}

	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()

	for name, store := range d.beadsStores {
		for _, rawStatus := range []string{"in_progress"} {
			s := beadsdk.Status(rawStatus)
			filter := beadsdk.IssueFilter{Status: &s, Limit: 1}
			issues, err := store.SearchIssues(ctx, "", filter)
			if err != nil {
				d.logger.Printf("hasActiveWork: %s/%s query failed: %v — assuming work present",
					name, rawStatus, err)
				return true // conservative: don't suppress Boot on query failure
			}
			if len(issues) > 0 {
				return true
			}
		}
	}
	return false
}

// runDegradedBootTriage performs mechanical Boot logic without AI reasoning.
// This is for degraded mode when tmux is unavailable.
func runDegradedBootTriage(d *Daemon, b *boot.Boot) {
	startTime := time.Now()
	status := &boot.Status{
		StartedAt: startTime,
	}

	// Simple check: is Deacon session alive?
	hasDeacon, err := d.tmux.HasSession(getDeaconSessionName())
	if err != nil {
		d.logger.Printf("Error checking Deacon session: %v", err)
		status.LastAction = "error"
		status.Error = err.Error()
	} else if !hasDeacon {
		d.logger.Println("Deacon not running, starting...")
		ensureDeaconRunning(d)
		status.LastAction = "start"
		status.Target = "deacon"
	} else {
		status.LastAction = "nothing"
	}

	status.CompletedAt = time.Now()

	if err := b.SaveStatus(status); err != nil {
		d.logger.Printf("Warning: failed to save Boot status: %v", err)
	}
}

// ensureDeaconRunning ensures the Deacon is running.
// Uses deacon.Manager for consistent startup behavior (WaitForShellReady, GUPP, etc.).
func ensureDeaconRunning(d *Daemon) {
	const agentID = "deacon"

	// Check restart tracker for backoff/crash loop
	if d.restartTracker != nil {
		if d.restartTracker.IsInCrashLoop(agentID) {
			d.logger.Printf("Deacon is in crash loop, skipping restart (use 'gt daemon clear-backoff deacon' to reset)")
			return
		}
		if !d.restartTracker.CanRestart(agentID) {
			remaining := d.restartTracker.GetBackoffRemaining(agentID)
			d.logger.Printf("Deacon restart in backoff, %s remaining", remaining.Round(time.Second))
			return
		}
	}

	mgr := deacon.NewManager(d.config.TownRoot)

	if err := mgr.Start(""); err != nil {
		if err == deacon.ErrAlreadyRunning {
			// Deacon is running - record success to reset backoff
			if d.restartTracker != nil {
				d.restartTracker.RecordSuccess(agentID)
			}
			return
		}
		d.logger.Printf("Error starting Deacon: %v", err)
		return
	}

	// Record this restart attempt for backoff tracking
	if d.restartTracker != nil {
		d.restartTracker.RecordRestart(agentID)
		if err := d.restartTracker.Save(); err != nil {
			d.logger.Printf("Warning: failed to save restart state: %v", err)
		}
	}

	// Track when we started the Deacon to prevent race condition in checkDeaconHeartbeat.
	// The heartbeat file will still be stale until the Deacon runs a full patrol cycle.
	d.deaconLastStarted = time.Now()
	d.metrics.RecordRestart(d.ctx, "deacon")
	telemetry.RecordDaemonRestart(d.ctx, "deacon")
	d.logger.Println("Deacon started successfully")
}

// deaconGracePeriod returns the config-driven deacon grace period.
// The Deacon needs time to initialize Claude, run SessionStart hooks, execute gt prime,
// run a patrol cycle, and write a fresh heartbeat. Default: 5 minutes.
func deaconGracePeriod(d *Daemon) time.Duration {
	return d.loadOperationalConfig().GetDaemonConfig().DeaconGracePeriodD()
}

// checkDeaconHeartbeat checks if the Deacon is making progress.
// This is a belt-and-suspenders fallback in case Boot doesn't detect stuck states.
// Uses the heartbeat file that the Deacon updates on each patrol cycle.
//
// PATCH-005: Fixed grace period logic. Old logic skipped heartbeat check entirely
// during grace period, allowing stuck Deacons to go undetected. New logic:
// - Always read heartbeat first
// - Grace period only applies if heartbeat is from BEFORE we started Deacon
// - If heartbeat is from AFTER start but stale, Deacon is stuck
func checkDeaconHeartbeat(d *Daemon) {
	// Respect crash-loop guard: if the restart tracker says Deacon is in a
	// crash loop, do not kill the session — the guard is deliberately holding
	// off restarts to break the cycle. (Fixes #2086)
	if d.restartTracker != nil && d.restartTracker.IsInCrashLoop("deacon") {
		d.logger.Printf("Deacon is in crash-loop state, skipping heartbeat kill check")
		return
	}

	// Always read heartbeat first (PATCH-005)
	hb := deacon.ReadHeartbeat(d.config.TownRoot)

	sessionName := getDeaconSessionName()

	// Check if we recently started a Deacon
	if !d.deaconLastStarted.IsZero() {
		timeSinceStart := time.Since(d.deaconLastStarted)

		if hb == nil {
			// No heartbeat file exists
			if timeSinceStart < deaconGracePeriod(d) {
				d.logger.Printf("Deacon started %s ago, awaiting first heartbeat...",
					timeSinceStart.Round(time.Second))
				return
			}
			// Grace period expired without any heartbeat - Deacon failed to start
			// Stuck-agent-dog: kill and restart
			d.logger.Printf("STUCK DEACON: started %s ago but hasn't written heartbeat (session: %s)",
				timeSinceStart.Round(time.Minute), sessionName)
			restartStuckDeacon(d, sessionName, fmt.Sprintf("no heartbeat after %s", timeSinceStart.Round(time.Minute)))
			return
		}

		// Heartbeat exists - check if it's from BEFORE we started this Deacon
		if hb.Timestamp.Before(d.deaconLastStarted) {
			// Heartbeat is stale (from before restart)
			if timeSinceStart < deaconGracePeriod(d) {
				d.logger.Printf("Deacon started %s ago, heartbeat is pre-restart, awaiting fresh heartbeat...",
					timeSinceStart.Round(time.Second))
				return
			}
			// Grace period expired but heartbeat still from before start
			// Stuck-agent-dog: kill and restart
			d.logger.Printf("STUCK DEACON: started %s ago but heartbeat still pre-restart (session: %s)",
				timeSinceStart.Round(time.Minute), sessionName)
			restartStuckDeacon(d, sessionName, fmt.Sprintf("heartbeat pre-restart after %s", timeSinceStart.Round(time.Minute)))
			return
		}

		// Heartbeat is from AFTER we started - Deacon has written at least one heartbeat
		// Fall through to normal staleness check
	}

	// No recent start tracking or Deacon has written fresh heartbeat - check normally
	if hb == nil {
		// No heartbeat file - Deacon hasn't started a cycle yet
		return
	}

	age := hb.Age()

	// If heartbeat is fresh (< 5 min), nothing to do
	if hb.IsFresh() {
		return
	}

	d.logger.Printf("Deacon heartbeat is stale (%s old), checking session...", age.Round(time.Minute))

	// Check if session exists
	hasSession, err := d.tmux.HasSession(sessionName)
	if err != nil {
		d.logger.Printf("Error checking Deacon session: %v", err)
		return
	}

	if !hasSession {
		// Session doesn't exist - ensureDeaconRunning already ran earlier
		// in heartbeat, so Deacon should be starting
		return
	}

	// Session exists but heartbeat is stale - Deacon may be stuck.
	// Two-tier response: nudge for stale (5-20 min), kill and restart
	// only for very stale (>= 20 min). Kill threshold must be > backoff-max
	// to avoid false positive kills during legitimate await-signal sleep.
	if hb.IsVeryStale() {
		// Stuck-agent-dog: kill and restart
		d.logger.Printf("STUCK DEACON: heartbeat stale for %s, session %s needs restart", age.Round(time.Minute), sessionName)
		restartStuckDeacon(d, sessionName, fmt.Sprintf("heartbeat stale for %s", age.Round(time.Minute)))
	} else {
		// Stale but not very stale (5-20 min) - nudge to wake up (unless idle).
		//
		// Idle guard: skip nudge if no beads are actively in flight.
		// This mirrors the Boot idle guard (ensureBootRunning). When the Deacon's
		// heartbeat has gone stale during an await-signal backoff sleep, sending a
		// nudge interrupts the exponential backoff for no reason — the Deacon will
		// wake naturally at its next timeout. Only nudge if work is actually in
		// flight (in_progress or hooked) that the Deacon may need to act on.
		// Conservative: on store errors hasActiveWork returns true, so nudge fires.
		// See also: runtime/runtime.go:99-101 — session-started nudge was removed
		// for the same reason (it interrupted the deacon's await-signal backoff).
		if !hasActiveWork(d) {
			d.logger.Println("Deacon nudge skipped: no active work in flight, await-signal will fire naturally")
			return
		}

		d.logger.Printf("Deacon stuck for %s - nudging session", age.Round(time.Minute))
		if err := d.tmux.NudgeSession(sessionName, "HEALTH_CHECK: heartbeat stale, respond to confirm responsiveness"); err != nil {
			d.logger.Printf("Error nudging stuck Deacon: %v", err)
		}
	}
}

// restartStuckDeacon kills a stuck Deacon session and respawns it.
// Uses RestartTracker for exponential backoff and crash-loop prevention.
// Notifies via gt-notify (zero token cost) if the notify script exists.
func restartStuckDeacon(d *Daemon, sessionName, reason string) {
	const agentID = "deacon"

	// Check restart tracker before acting
	if d.restartTracker != nil {
		if d.restartTracker.IsInCrashLoop(agentID) {
			d.logger.Printf("Stuck-agent-dog: Deacon in crash loop, not restarting (use 'gt daemon clear-backoff deacon')")
			notifySlack(d, "admin", "critical", fmt.Sprintf("Deacon crash loop detected — manual intervention required. Reason: %s", reason))
			return
		}
		if !d.restartTracker.CanRestart(agentID) {
			remaining := d.restartTracker.GetBackoffRemaining(agentID)
			d.logger.Printf("Stuck-agent-dog: Deacon restart in backoff, %s remaining", remaining.Round(time.Second))
			return
		}
	}

	// Distinguish a usage-limit pause from a true crash. If Claude is sitting
	// at a rate-limit prompt the heartbeat will go stale, looking identical
	// to a crash — but killing and respawning won't help (the new session
	// hits the same limit) and the repeated kills burn the crash-loop budget.
	// Detect the rate-limit signature in the pane and let quota_dog handle
	// account rotation instead.
	if d.tmux != nil {
		if pane, err := d.tmux.CapturePane(sessionName, 30); err == nil && IsClaudeUsageLimit(pane) {
			d.logger.Printf("Stuck-agent-dog: Deacon paused — Claude usage-limit detected, skipping kill (quota_dog will rotate accounts). Reason: %s", reason)
			if d.restartTracker != nil {
				d.restartTracker.RecordPause(agentID)
				if err := d.restartTracker.Save(); err != nil {
					d.logger.Printf("Warning: failed to save restart state: %v", err)
				}
			}
			return
		}
	}

	// Kill the stuck session
	d.logger.Printf("Stuck-agent-dog: killing stuck Deacon session %s (reason: %s)", sessionName, reason)
	if err := d.tmux.KillSession(sessionName); err != nil {
		d.logger.Printf("Stuck-agent-dog: error killing session %s: %v", sessionName, err)
		// Continue — session may already be dead
	}

	// Brief pause for tmux cleanup
	time.Sleep(2 * time.Second)

	// Respawn via ensureDeaconRunning (which uses deacon.Manager)
	ensureDeaconRunning(d)

	// Verify it came back
	hasSession, err := d.tmux.HasSession(sessionName)
	if err != nil || !hasSession {
		d.logger.Printf("Stuck-agent-dog: FAILED to respawn Deacon after kill")
		notifySlack(d, "admin", "critical", fmt.Sprintf("Deacon restart FAILED — session did not respawn. Reason: %s", reason))
		return
	}

	d.logger.Printf("Stuck-agent-dog: Deacon restarted successfully")
	notifySlack(d, "admin", "high", fmt.Sprintf("Deacon was stuck (%s) — auto-restarted successfully", reason))
}

// notifySlack sends a notification via gt-notify (zero token cost).
// Channel: "admin" or "status". Priority: "critical", "high", "info", "success".
// Silently fails if gt-notify is not found — notification is best-effort.
func notifySlack(d *Daemon, channel, priority, message string) {
	notifyBin := filepath.Join(d.config.TownRoot, "bin", "gt-notify")
	if _, err := os.Stat(notifyBin); err != nil {
		d.logger.Printf("Stuck-agent-dog: gt-notify not found at %s, skipping notification", notifyBin)
		return
	}

	//nolint:gosec // G204: args are constructed internally
	cmd := exec.Command(notifyBin, "--channel", channel, "--priority", priority, message)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s:%s", filepath.Join(d.config.TownRoot, "bin"), os.Getenv("PATH")))
	if output, err := cmd.CombinedOutput(); err != nil {
		d.logger.Printf("Stuck-agent-dog: gt-notify failed: %v (output: %s)", err, string(output))
	}
}

// ensureWitnessesRunning ensures witnesses are running for configured rigs.
// Called on each heartbeat to maintain witness patrol loops.
// Respects the rigs filter in daemon.json patrol config.
func ensureWitnessesRunning(d *Daemon) {
	rigs := getPatrolRigs(d, "witness")
	d.rigPool.RunPerRig(d.ctx, rigs, func(ctx context.Context, rigName string) error {
		ensureWitnessRunning(d, rigName)
		return nil
	})
}

// hasPendingEvents checks if there are pending .event files in the given channel directory.
// Used to gate agent spawning: don't burn API credits starting a Claude session when
// there's nothing to process. The agent's await-event handles the actual consumption.
func hasPendingEvents(d *Daemon, channel string) bool {
	eventDir := filepath.Join(d.config.TownRoot, "events", channel)
	entries, err := os.ReadDir(eventDir)
	if err != nil {
		return false // Directory doesn't exist or unreadable = no pending events
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".event") {
			return true
		}
	}
	return false
}

// ensureWitnessRunning ensures the witness for a specific rig is running.
// Discover, don't track: uses Manager.Start() which checks tmux directly (gt-zecmc).
func ensureWitnessRunning(d *Daemon, rigName string) {
	// Check rig operational state before auto-starting
	if operational, reason := isRigOperational(d, rigName); !operational {
		d.logger.Printf("Skipping witness auto-start for %s: %s", rigName, reason)
		// Kill leftover witness session if rig is not operational (docked/parked).
		// Without this, sessions started before the rig was docked survive until
		// the next explicit 'gt rig dock' command. (hq-snx61)
		name := session.WitnessSessionName(session.PrefixFor(rigName))
		if exists, _ := d.tmux.HasSession(name); exists {
			d.logger.Printf("Killing leftover witness %s (rig %s)", name, reason)
			if err := d.tmux.KillSessionWithProcesses(name); err != nil {
				d.logger.Printf("Error killing leftover witness %s: %v", name, err)
			}
		}
		return
	}

	// Manager.Start() handles: zombie detection, session creation, env vars, theming,
	// startup readiness waits, and crucially - startup/propulsion nudges (GUPP).
	// It returns ErrAlreadyRunning if Claude is already running in tmux.
	r := &rig.Rig{
		Name: rigName,
		Path: filepath.Join(d.config.TownRoot, rigName),
	}
	mgr := witness.NewManager(r)

	// NOTE: Hung session detection removed for witnesses (serial killer bug).
	// Idle witnesses legitimately produce no tmux output while waiting for work.
	// The deacon's patrol health-scan step handles stuck detection with proper
	// context (checks for active work before declaring something stuck).
	// See: daemon.log "is hung (no activity for 30m0s), killing for restart"

	if err := mgr.Start(false, "", nil); err != nil {
		if err == witness.ErrAlreadyRunning {
			// Already running - this is the expected case
			d.logger.Printf("Witness for %s already running, skipping spawn", rigName)
			return
		}
		d.logger.Printf("Error starting witness for %s: %v", rigName, err)
		return
	}

	d.metrics.RecordRestart(d.ctx, "witness")
	telemetry.RecordDaemonRestart(d.ctx, "witness-"+rigName)
	d.logger.Printf("Witness session for %s started successfully", rigName)
}

// ensureRefineriesRunning ensures refineries are running for configured rigs.
// Called on each heartbeat to maintain refinery merge queue processing.
// Respects the rigs filter in daemon.json patrol config.
func ensureRefineriesRunning(d *Daemon) {
	rigs := getPatrolRigs(d, "refinery")
	d.rigPool.RunPerRig(d.ctx, rigs, func(ctx context.Context, rigName string) error {
		ensureRefineryRunning(d, rigName)
		return nil
	})
}

// ensureRefineryRunning ensures the refinery for a specific rig is running.
// Discover, don't track: uses Manager.Start() which checks tmux directly (gt-zecmc).
func ensureRefineryRunning(d *Daemon, rigName string) {
	// Check rig operational state before auto-starting
	if operational, reason := isRigOperational(d, rigName); !operational {
		d.logger.Printf("Skipping refinery auto-start for %s: %s", rigName, reason)
		// Kill leftover refinery session if rig is not operational (docked/parked).
		// Without this, sessions started before the rig was docked survive until
		// the next explicit 'gt rig dock' command. (hq-snx61)
		name := session.RefinerySessionName(session.PrefixFor(rigName))
		if exists, _ := d.tmux.HasSession(name); exists {
			d.logger.Printf("Killing leftover refinery %s (rig %s)", name, reason)
			if err := d.tmux.KillSessionWithProcesses(name); err != nil {
				d.logger.Printf("Error killing leftover refinery %s: %v", name, err)
			}
		}
		return
	}
	if stop, err := refinery.ActiveSafetyStop(d.config.TownRoot, rigName); err != nil {
		d.logger.Printf("Skipping refinery auto-start for %s: cannot verify safety stop: %v", rigName, err)
		return
	} else if stop != nil {
		d.logger.Printf("Skipping refinery auto-start for %s: %s", rigName, stop.Reason())
		name := session.RefinerySessionName(session.PrefixFor(rigName))
		if exists, _ := d.tmux.HasSession(name); exists {
			d.logger.Printf("Killing leftover refinery %s (%s)", name, stop.Reason())
			if err := d.tmux.KillSessionWithProcesses(name); err != nil {
				d.logger.Printf("Error killing leftover refinery %s: %v", name, err)
			}
		}
		return
	}

	// Event gate: don't spawn a new Claude session when there's nothing to process.
	// If a refinery session is already running, Start() returns ErrAlreadyRunning (cheap).
	// But spawning a NEW session with an empty queue burns API credits for nothing.
	// The refinery formula uses await-event internally, so it will wake when events appear.
	if !hasPendingEvents(d, "refinery") {
		// Check if session already exists before skipping — let running sessions continue
		r := &rig.Rig{
			Name: rigName,
			Path: filepath.Join(d.config.TownRoot, rigName),
		}
		mgr := refinery.NewManager(r)
		if running, _ := mgr.IsRunning(); !running {
			d.logger.Printf("No pending refinery events and no session running for %s, skipping spawn", rigName)
			return
		}
	}

	// Manager.Start() handles: zombie detection, session creation, env vars, theming,
	// WaitForClaudeReady, and crucially - startup/propulsion nudges (GUPP).
	// It returns ErrAlreadyRunning if Claude is already running in tmux.
	r := &rig.Rig{
		Name: rigName,
		Path: filepath.Join(d.config.TownRoot, rigName),
	}
	mgr := refinery.NewManager(r)

	// NOTE: Hung session detection removed for refineries (serial killer bug).
	// Idle refineries legitimately produce no tmux output while waiting for MRs.
	// The deacon's patrol health-scan step handles stuck detection with proper
	// context (checks for active work before declaring something stuck).
	// See: daemon.log "is hung (no activity for 30m0s), killing for restart"

	if err := mgr.Start(false, ""); err != nil {
		if errors.Is(err, refinery.ErrAlreadyRunning) {
			// Already running - this is the expected case when fix is working
			d.logger.Printf("Refinery for %s already running, skipping spawn", rigName)
			return
		}
		if errors.Is(err, refinery.ErrSafetyStopped) {
			d.logger.Printf("Skipping refinery auto-start for %s: %v", rigName, err)
			return
		}
		if errors.Is(err, refinery.ErrForkRig) {
			d.logger.Printf("Skipping refinery auto-start for %s: %v", rigName, err)
			return
		}
		d.logger.Printf("Error starting refinery for %s: %v", rigName, err)
		return
	}

	d.metrics.RecordRestart(d.ctx, "refinery")
	telemetry.RecordDaemonRestart(d.ctx, "refinery-"+rigName)
	d.logger.Printf("Refinery session for %s started successfully", rigName)
}

// ensureMayorRunning ensures the Mayor is running.
// Uses mayor.Manager for consistent startup behavior.
// If the tmux session exists but the agent is dead (zombie), the daemon
// stops the zombie session and starts a fresh one.
func ensureMayorRunning(d *Daemon) {
	mgr := mayor.NewManager(d.config.TownRoot)

	if err := mgr.Start(""); err != nil {
		if err == mayor.ErrAlreadyRunning {
			// Session exists — verify agent is actually alive.
			// During handoffs the agent is briefly undetectable, so we
			// only restart if the session has been a zombie for multiple
			// consecutive patrol cycles (debounce).
			if !isMayorAgentAlive(mgr) {
				d.mayorZombieCount++
				if d.mayorZombieCount >= 3 {
					d.logger.Printf("Mayor zombie detected (%d cycles), restarting", d.mayorZombieCount)
					if stopErr := mgr.Stop(); stopErr != nil && stopErr != mayor.ErrNotRunning {
						d.logger.Printf("Error stopping zombie Mayor: %v", stopErr)
						return
					}
					d.mayorZombieCount = 0
					if startErr := mgr.Start(""); startErr != nil {
						d.logger.Printf("Error restarting Mayor after zombie cleanup: %v", startErr)
						return
					}
					d.logger.Println("Mayor restarted after zombie cleanup")
				} else {
					d.logger.Printf("Mayor agent not detected (cycle %d/3), waiting before restart", d.mayorZombieCount)
				}
			} else {
				d.mayorZombieCount = 0
			}
			return
		}
		d.logger.Printf("Error starting Mayor: %v", err)
		return
	}

	d.mayorZombieCount = 0
	d.logger.Println("Mayor started successfully")
}

// isMayorAgentAlive checks if the Mayor's agent process is running in tmux.
func isMayorAgentAlive(mgr *mayor.Manager) bool {
	t := tmux.NewTmux()
	return t.IsAgentAlive(mgr.SessionName())
}

// killDeaconSessions kills leftover deacon and boot tmux sessions.
// Called when the deacon patrol is disabled to prevent stale deacons from
// running their own patrol loops and spawning agents. (hq-2mstj)
func killDeaconSessions(d *Daemon) {
	for _, name := range []string{session.DeaconSessionName(), session.BootSessionName()} {
		exists, _ := d.tmux.HasSession(name)
		if exists {
			d.logger.Printf("Killing leftover %s session (patrol disabled)", name)
			if err := d.tmux.KillSessionWithProcesses(name); err != nil {
				d.logger.Printf("Error killing %s session: %v", name, err)
			}
		}
	}
}

// killWitnessSessions kills leftover witness tmux sessions for all rigs.
// Called when the witness patrol is disabled. (hq-2mstj)
func killWitnessSessions(d *Daemon) {
	d.rigPool.RunPerRig(d.ctx, getKnownRigs(d), func(ctx context.Context, rigName string) error {
		name := session.WitnessSessionName(session.PrefixFor(rigName))
		exists, _ := d.tmux.HasSession(name)
		if exists {
			d.logger.Printf("Killing leftover %s session (patrol disabled)", name)
			if err := d.tmux.KillSessionWithProcesses(name); err != nil {
				d.logger.Printf("Error killing %s session: %v", name, err)
			}
		}
		return nil
	})
}

// killRefinerySessions kills leftover refinery tmux sessions for all rigs.
// Called when the refinery patrol is disabled. (hq-2mstj)
func killRefinerySessions(d *Daemon) {
	d.rigPool.RunPerRig(d.ctx, getKnownRigs(d), func(ctx context.Context, rigName string) error {
		name := session.RefinerySessionName(session.PrefixFor(rigName))
		exists, _ := d.tmux.HasSession(name)
		if exists {
			d.logger.Printf("Killing leftover %s session (patrol disabled)", name)
			if err := d.tmux.KillSessionWithProcesses(name); err != nil {
				d.logger.Printf("Error killing %s session: %v", name, err)
			}
		}
		return nil
	})
}

// killDefaultPrefixGhosts kills tmux sessions that use the default "gt" prefix
// for roles that should use a rig-specific prefix. These ghost sessions appear
// when the daemon starts before a rig is registered or when the registry was
// stale. After a registry reload, any "gt-witness", "gt-refinery", or "gt-*"
// sessions that correspond to rigs with their own prefix are stale duplicates.
// Fix for: hq-ouz, hq-eqf, hq-3i4.
func killDefaultPrefixGhosts(d *Daemon) {
	reg := session.DefaultRegistry()
	allRigs := reg.AllRigs() // rigName → shortPrefix
	if len(allRigs) == 0 {
		return
	}

	// Check if any rig actually has "gt" as its registered prefix.
	// If so, gt-witness is legitimate for that rig — don't kill it.
	gtIsLegitimate := false
	for _, prefix := range allRigs {
		if prefix == session.DefaultPrefix {
			gtIsLegitimate = true
			break
		}
	}
	if gtIsLegitimate {
		return
	}

	// Kill ghost sessions using the default "gt" prefix for patrol roles.
	for _, role := range []string{"witness", "refinery"} {
		ghostName := fmt.Sprintf("%s-%s", session.DefaultPrefix, role)
		exists, _ := d.tmux.HasSession(ghostName)
		if exists {
			d.logger.Printf("Killing ghost session %s (default prefix, stale registry artifact)", ghostName)
			if err := d.tmux.KillSessionWithProcesses(ghostName); err != nil {
				d.logger.Printf("Error killing ghost session %s: %v", ghostName, err)
			}
		}
	}

	// Also check for ghost polecat sessions: gt-<polecatName> where the polecat
	// actually belongs to a rig with a different prefix.
	for _, rigName := range getKnownRigs(d) {
		rigPrefix := session.PrefixFor(rigName)
		if rigPrefix == session.DefaultPrefix {
			continue // This rig uses "gt" — its sessions are fine
		}
		rigPath := filepath.Join(d.config.TownRoot, rigName, "polecats")
		entries, err := os.ReadDir(rigPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			polecatName := entry.Name()
			ghostName := fmt.Sprintf("%s-%s", session.DefaultPrefix, polecatName)
			exists, _ := d.tmux.HasSession(ghostName)
			if exists {
				// Verify the correct session isn't also running (avoid killing legit sessions)
				correctName := session.PolecatSessionName(rigPrefix, polecatName)
				correctExists, _ := d.tmux.HasSession(correctName)
				if !correctExists {
					// Ghost is the only session — it might be doing real work.
					// Log but don't kill; the registry reload will prevent new ghosts.
					d.logger.Printf("Ghost polecat session %s found (should be %s), not killing (may have active work)", ghostName, correctName)
				} else {
					// Both exist — ghost is definitely a duplicate, kill it.
					d.logger.Printf("Killing duplicate ghost polecat session %s (correct session %s exists)", ghostName, correctName)
					if err := d.tmux.KillSessionWithProcesses(ghostName); err != nil {
						d.logger.Printf("Error killing ghost session %s: %v", ghostName, err)
					}
				}
			}
		}
	}
}

// openBeadsStores opens beads stores for the town (hq) and all known rigs.
// Returns a map keyed by "hq" for town-level and rig names for per-rig stores.
// Stores that fail to open are logged and skipped. Successfully opened stores
// are compatibility-checked before being returned to Convoy polling.
func openBeadsStores(d *Daemon) (map[string]beadsdk.Storage, error) {
	stores := make(map[string]beadsdk.Storage)

	// Town-level store (hq)
	hqBeadsDir := filepath.Join(d.config.TownRoot, ".beads")
	if store, err := beads.OpenFromConfig(d.ctx, hqBeadsDir); err == nil {
		stores["hq"] = store
	} else {
		d.logger.Printf("Convoy: hq beads store unavailable: %s", util.FirstLine(err.Error()))
	}

	// Per-rig stores
	for _, rigName := range getKnownRigs(d) {
		beadsDir := doltserver.FindRigBeadsDir(d.config.TownRoot, rigName)
		if beadsDir == "" {
			continue
		}
		store, err := beads.OpenFromConfig(d.ctx, beadsDir)
		if err != nil {
			d.logger.Printf("Convoy: %s beads store unavailable: %s", rigName, util.FirstLine(err.Error()))
			continue
		}
		stores[rigName] = store
	}

	if len(stores) == 0 {
		d.logger.Printf("Convoy: no beads stores available, event polling disabled")
		return nil, nil
	}

	if err := checkBeadsStoreCompatibility(d.ctx, stores, embeddedBeadsVersion()); err != nil {
		closeBeadsStores(d.logger, stores)
		return nil, err
	}

	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	d.logger.Printf("Convoy: opened %d beads store(s): %v", len(stores), names)
	return stores, nil
}

// getKnownRigs returns list of registered rig names.
// Results are memoized per heartbeat tick to coalesce the ~10 per-tick callers
// into a single mayor/rigs.json read. The cache is invalidated at the start of
// each heartbeat.
func getKnownRigs(d *Daemon) []string {
	if d.knownRigsCacheValid {
		return d.knownRigsCache
	}
	rigs := readKnownRigsFromDisk(d)
	d.knownRigsCache = rigs
	d.knownRigsCacheValid = true
	return rigs
}

// invalidateKnownRigsCache clears the per-tick cache so the next
// getKnownRigs() call re-reads mayor/rigs.json from disk.
func invalidateKnownRigsCache(d *Daemon) {
	d.knownRigsCache = nil
	d.knownRigsCacheValid = false
}

// readKnownRigsFromDisk reads and parses mayor/rigs.json.
func readKnownRigsFromDisk(d *Daemon) []string {
	rigsPath := filepath.Join(d.config.TownRoot, "mayor", "rigs.json")
	data, err := os.ReadFile(rigsPath)
	if err != nil {
		return nil
	}

	var parsed struct {
		Rigs map[string]interface{} `json:"rigs"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}

	var rigs []string
	for name := range parsed.Rigs {
		rigs = append(rigs, name)
	}
	return rigs
}

// getPatrolRigs returns the list of operational rigs for a patrol.
// If the patrol config specifies a rigs filter, only those rigs are returned.
// Otherwise, all known rigs are returned. In both cases, non-operational
// rigs (parked/docked) are filtered out at list-building time. (Fixes upstream #2082)
func getPatrolRigs(d *Daemon, patrol string) []string {
	configRigs := GetPatrolRigs(d.patrolConfig, patrol)
	var candidates []string
	if len(configRigs) > 0 {
		candidates = configRigs
	} else {
		candidates = getKnownRigs(d)
	}

	// Filter out non-operational rigs early to avoid per-rig skip noise
	var operational []string
	for _, rigName := range candidates {
		if ok, reason := isRigOperational(d, rigName); ok {
			operational = append(operational, rigName)
		} else {
			d.logger.Printf("Excluding %s from %s patrol: %s", rigName, patrol, reason)
		}
	}
	return operational
}

// isRigOperational checks if a rig is in an operational state.
// Returns true if the rig can have agents auto-started.
// Returns false (with reason) if the rig is parked, docked, or has auto_restart blocked/disabled.
//
// TODO(#2120): This duplicates parked/docked checking logic from
// cmd.IsRigParkedOrDocked and cmd.hasRigBeadLabel. Consolidating into a
// shared package (e.g. internal/rig) would eliminate the third implementation
// and reduce drift risk. Not done here due to circular import constraints
// (daemon cannot import cmd).
func isRigOperational(d *Daemon, rigName string) (bool, string) {
	cfg := wisp.NewConfig(d.config.TownRoot, rigName)

	// Warn if wisp config is missing - parked/docked state may have been lost
	if _, err := os.Stat(cfg.ConfigPath()); os.IsNotExist(err) {
		d.logger.Printf("Warning: no wisp config for %s - parked state may have been lost", rigName)
	}

	// Check wisp layer first (local/ephemeral overrides)
	status := cfg.GetString("status")
	switch status {
	case "parked":
		return false, "rig is parked"
	case "docked":
		return false, "rig is docked"
	}

	// Check rig bead labels (global/synced docked status)
	// This is the persistent docked state set by 'gt rig dock'
	rigPath := filepath.Join(d.config.TownRoot, rigName)

	// Try to get prefix from rig config.json, fall back to rigs.json registry
	var prefix string
	if rigCfg, err := rig.LoadRigConfig(rigPath); err == nil && rigCfg.Beads != nil {
		prefix = rigCfg.Beads.Prefix
	} else {
		// Fall back to registry (mayor/rigs.json) when config.json is missing
		prefix = agentconfig.GetRigPrefix(d.config.TownRoot, rigName)
	}

	rigBeadID := fmt.Sprintf("%s-rig-%s", prefix, rigName)
	rigBeadsDir := beads.ResolveBeadsDir(rigPath)
	bd := beads.NewWithBeadsDir(rigPath, rigBeadsDir)
	if issue, err := bd.Show(rigBeadID); err == nil {
		for _, label := range issue.Labels {
			if label == "status:docked" {
				return false, "rig is docked (global)"
			}
			if label == "status:parked" {
				return false, "rig is parked (global)"
			}
		}
	} else {
		// Log when rig bead lookup fails - this helps debug transient Dolt issues
		// FAIL-SAFE: When we can't verify docked status (Dolt down, network issue, etc.),
		// assume the rig is NOT operational. This prevents wasting API credits starting
		// witnesses that might be docked. Better to delay work than burn credits unnecessarily.
		d.logger.Printf("Warning: failed to check rig bead %s for docked/parked status: %v (assuming not operational)", rigBeadID, err)
		return false, "cannot verify rig status (Dolt unavailable)"
	}

	// Check auto_restart config
	// If explicitly blocked (nil), auto-restart is disabled
	if cfg.IsBlocked("auto_restart") {
		return false, "auto_restart is blocked"
	}

	// If explicitly set to false, auto-restart is disabled
	// Note: GetBool returns false for unset keys, so we need to check if it's explicitly set
	val := cfg.Get("auto_restart")
	if val != nil {
		if autoRestart, ok := val.(bool); ok && !autoRestart {
			return false, "auto_restart is disabled"
		}
	}

	return true, ""
}

// processLifecycleRequests checks for and processes lifecycle requests.
func processLifecycleRequests(d *Daemon) {
	d.ProcessLifecycleRequests()
}

// shutdown performs graceful shutdown.
func shutdown(d *Daemon, state *State) error { //nolint:unparam // error return kept for future use
	d.logger.Println("Daemon shutting down")

	// Stop feed curator
	if d.curator != nil {
		d.curator.Stop()
		d.logger.Println("Feed curator stopped")
	}

	// Stop convoy manager (also closes beads stores)
	if d.convoyManager != nil {
		d.convoyManager.Stop()
		d.logger.Println("Convoy manager stopped")
	}
	d.beadsStores = nil

	// Stop KRC pruner
	if d.krcPruner != nil {
		d.krcPruner.Stop()
		d.logger.Println("KRC pruner stopped")
	}

	// Push Dolt remotes before stopping the server (if patrol is enabled)
	d.pushDoltRemotes()

	// Stop Dolt server if we're managing it
	if d.doltServer != nil && d.doltServer.IsEnabled() && !d.doltServer.IsExternal() {
		if err := d.doltServer.Stop(); err != nil {
			d.logger.Printf("Warning: failed to stop Dolt server: %v", err)
		} else {
			d.logger.Println("Dolt server stopped")
		}
	}

	// Flush and stop OTel providers (5s deadline to avoid blocking shutdown).
	if d.otelProvider != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.otelProvider.Shutdown(shutCtx); err != nil {
			d.logger.Printf("Warning: telemetry shutdown: %v", err)
		}
	}

	state.Running = false
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save final state: %v", err)
	}

	d.logger.Println("Daemon stopped")
	return nil
}

// isShutdownInProgress checks if a shutdown is currently in progress.
// The shutdown.lock file is created by gt down before terminating sessions.
// This prevents the daemon from fighting shutdown by auto-restarting killed agents.
//
// Uses flock to check actual lock status rather than file existence, since
// the lock file persists after shutdown completes. The file is intentionally
// never removed: flock works on file descriptors, not paths, and removing
// the file while another process waits on the flock defeats mutual exclusion.
func isShutdownInProgress(d *Daemon) bool {
	lockPath := filepath.Join(d.config.TownRoot, "daemon", "shutdown.lock")

	// If file doesn't exist, no shutdown in progress
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return false
	}

	// Try non-blocking lock acquisition to check if shutdown holds the lock
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		// Error acquiring lock - assume shutdown in progress to be safe
		return true
	}

	if locked {
		// We acquired the lock, so no shutdown is holding it
		// Release immediately; leave the file in place so all
		// concurrent callers flock the same inode.
		_ = lock.Unlock()
		return false
	}

	// Could not acquire lock - shutdown is in progress
	return true
}

// IsShutdownInProgress checks if a shutdown is currently in progress for the given town.
// This is the exported version of isShutdownInProgress for use by other packages
// (e.g., Boot triage) that need to avoid restarting sessions during shutdown.
func IsShutdownInProgress(townRoot string) bool {
	lockPath := filepath.Join(townRoot, "daemon", "shutdown.lock")

	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return false
	}

	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return true
	}

	if locked {
		_ = lock.Unlock()
		return false
	}

	return true
}

// IsRunning checks if a daemon is running for the given town.
// Uses the daemon.lock flock as the authoritative signal — if the lock is held,
// the daemon is running. Falls back to PID file for the process ID.
// This avoids fragile ps string matching for process identity (ZFC fix: gt-utuk).
func IsRunning(townRoot string) (bool, int, error) {
	// Primary check: is the daemon lock held?
	lockPath := filepath.Join(townRoot, "daemon", "daemon.lock")
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return false, 0, nil
	}

	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		// Can't check lock — fall back to PID file + signal check
		return isRunningFromPID(townRoot)
	}

	if locked {
		// We acquired the lock, so no daemon holds it
		_ = lock.Unlock()
		// Clean up stale PID file if present
		pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
		_ = os.Remove(pidFile)
		return false, 0, nil
	}

	// Lock is held — daemon is running. Read PID from file.
	// Use readPIDFile to handle the "PID\nNONCE" format introduced alongside
	// nonce-based ownership verification. A plain Atoi on the raw file content
	// fails when a nonce line is present, returning PID 0.
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	pid, _, err := readPIDFile(pidFile)
	if err != nil {
		// Lock held but no readable PID file — daemon running, PID unknown
		return true, 0, nil
	}

	return true, pid, nil
}

// isRunningFromPID is the fallback when flock check fails. Uses PID file + signal.
func isRunningFromPID(townRoot string) (bool, int, error) {
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")

	pid, alive, err := verifyPIDOwnership(pidFile)
	if err != nil {
		return false, 0, fmt.Errorf("checking PID file: %w", err)
	}

	if pid == 0 {
		// No PID file
		return false, 0, nil
	}

	if !alive {
		// Process not running, clean up stale PID file.
		// This is a successful recovery, not an error — the caller can
		// proceed as if no daemon is running (fixes #2107).
		os.Remove(pidFile) // best-effort cleanup
		return false, 0, nil
	}

	return true, pid, nil
}

// StopDaemon stops the running daemon for the given town.
// Note: The file lock in Run() prevents multiple daemons per town, so we only
// need to kill the process from the PID file.
func StopDaemon(townRoot string) error {
	running, pid, err := IsRunning(townRoot)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("daemon is not running")
	}

	if pid <= 0 {
		// Lock is held but PID is unknown (race: daemon starting, or stale lock).
		// Clean up the lock file so the next gt up can start fresh.
		lockPath := filepath.Join(townRoot, "daemon", "daemon.lock")
		_ = os.Remove(lockPath)
		pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
		_ = os.Remove(pidFile)
		return nil
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process: %w", err)
	}

	// Send termination signal for graceful shutdown
	if err := sendTermSignal(process); err != nil {
		return fmt.Errorf("sending termination signal: %w", err)
	}

	// Wait a bit for graceful shutdown
	time.Sleep(constants.ShutdownNotifyDelay)

	// Check if still running
	if isProcessAlive(process) {
		// Still running, force kill
		_ = sendKillSignal(process)
	}

	// Clean up PID file
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	_ = os.Remove(pidFile)

	return nil
}

// FindOrphanedDaemons detects daemon processes not tracked by the PID file.
// Uses flock on daemon.lock to detect running daemons without relying on
// pgrep or ps string matching (ZFC fix: gt-utuk).
//
// With flock-based daemon management, only one daemon can hold the lock.
// An "orphan" is detected when the lock is held but the PID file is stale
// (process dead) or missing. Returns the stale PID if available.
func FindOrphanedDaemons(townRoot string) ([]int, error) {
	lockPath := filepath.Join(townRoot, "daemon", "daemon.lock")
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return nil, nil // No lock file — no daemon has ever run
	}

	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return nil, nil // Can't check lock — assume no orphans
	}

	if locked {
		// We acquired the lock — no daemon holds it, no orphans possible
		_ = lock.Unlock()
		return nil, nil
	}

	// Lock is held — a daemon is running. Check if it's tracked.
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	trackedPID, _, err := readPIDFile(pidFile)
	if err != nil {
		// Lock held but no/invalid PID file — daemon is running but untracked.
		// We can't determine its PID without ps/pgrep, so return empty.
		// The caller (start.go) should use IsRunning() which handles this case.
		return nil, nil
	}

	// Check if the tracked PID is actually alive
	process, findErr := os.FindProcess(trackedPID)
	if findErr != nil {
		return nil, nil
	}
	if !isProcessAlive(process) {
		// PID file exists but process is dead — stale PID file with held lock.
		// This shouldn't happen (lock should release on process death), but
		// report the stale PID for cleanup.
		return []int{trackedPID}, nil
	}

	// Lock held, PID alive, PID tracked — daemon is properly running, not orphaned.
	return nil, nil
}

// KillOrphanedDaemons finds and kills any orphaned gt daemon processes.
// Returns number of processes killed.
func KillOrphanedDaemons(townRoot string) (int, error) {
	pids, err := FindOrphanedDaemons(townRoot)
	if err != nil {
		return 0, err
	}

	killed := 0
	for _, pid := range pids {
		process, err := os.FindProcess(pid)
		if err != nil {
			continue
		}

		// Try termination signal first
		if err := sendTermSignal(process); err != nil {
			continue
		}

		// Wait for graceful shutdown
		time.Sleep(200 * time.Millisecond)

		// Check if still alive
		if isProcessAlive(process) {
			// Still alive, force kill
			_ = sendKillSignal(process)
		}

		killed++
	}

	return killed, nil
}

// checkPolecatSessionHealth proactively validates polecat tmux sessions.
// This detects crashed polecats that:
// 1. Have work-on-hook (assigned work)
// 2. Report state=running/working in their agent bead
// 3. But the tmux session is actually dead
//
// When a crash is detected, the polecat is automatically restarted.
// This provides faster recovery than waiting for GUPP timeout or Witness detection.
func checkPolecatSessionHealth(d *Daemon) {
	d.rigPool.RunPerRig(d.ctx, getKnownRigs(d), func(ctx context.Context, rigName string) error {
		checkRigPolecatHealth(d, rigName)
		return nil
	})
}

// checkRigPolecatHealth checks polecat session health for a specific rig.
func checkRigPolecatHealth(d *Daemon, rigName string) {
	// Get polecat directories for this rig
	polecatsDir := filepath.Join(d.config.TownRoot, rigName, "polecats")
	polecats, err := listPolecatWorktrees(polecatsDir)
	if err != nil {
		return // No polecats directory - rig might not have polecats
	}

	for _, polecatName := range polecats {
		checkPolecatHealth(d, rigName, polecatName)
	}
}

func listPolecatWorktrees(polecatsDir string) ([]string, error) {
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return nil, err
	}

	polecats := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		polecats = append(polecats, name)
	}

	return polecats, nil
}

// checkPolecatHealth checks a single polecat's session health.
// If the polecat has work-on-hook but the tmux session is dead, it's restarted.
func checkPolecatHealth(d *Daemon, rigName, polecatName string) {
	// Build the expected tmux session name
	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)

	// Check if tmux session exists
	sessionAlive, err := d.tmux.HasSession(sessionName)
	if err != nil {
		d.logger.Printf("Error checking session %s: %v", sessionName, err)
		return
	}

	if sessionAlive {
		// Session is alive - nothing to do
		return
	}

	// Session is dead. Check if the polecat has work-on-hook.
	prefix := beads.GetPrefixForRig(d.config.TownRoot, rigName)
	agentBeadID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)
	info, err := d.getAgentBeadInfo(agentBeadID)
	if err != nil {
		// Agent bead doesn't exist or error - polecat might not be registered
		return
	}

	// Check if polecat has hooked work
	if info.HookBead == "" {
		// No hooked work - this polecat is orphaned (should have self-nuked).
		// Self-cleaning model: polecats nuke themselves on completion.
		// An orphan with a dead session doesn't need restart - it needs cleanup.
		// Let the Witness handle orphan detection/cleanup during patrol.
		return
	}

	// Terminal state guard: skip polecats in intentional shutdown states.
	// agent_state='done' means normal completion; agent_state='nuked' means forced shutdown.
	// Their sessions being dead is expected, not a crash. Without this check,
	// the dead session + open hook_bead combination can fire false CRASHED_POLECAT
	// alerts during the race window before the hook_bead is closed.
	// This check is pure in-memory (info.State is already populated), so it runs before
	// the more expensive isBeadClosed subprocess call.
	agentState := beads.AgentState(info.State)
	if agentState == beads.AgentStateDone || agentState == beads.AgentStateNuked {
		d.logger.Printf("Skipping crash detection for %s/%s: agent_state=%s (intentional shutdown, not a crash)",
			rigName, polecatName, info.State)
		return
	}

	// Stale hook guard: skip polecats whose hook_bead is already closed.
	// When a polecat completes work normally (gt done), the hook_bead gets closed
	// but may not be cleared from the agent bead before the session stops.
	// Without this check, every heartbeat cycle fires a false CRASHED_POLECAT alert
	// for the dead session + non-empty hook_bead combination.
	if isBeadClosed(d, info.HookBead) {
		d.logger.Printf("Skipping crash detection for %s/%s: hook_bead %s is already closed (work completed normally)",
			rigName, polecatName, info.HookBead)
		return
	}

	// Spawning guard: skip polecats being actively started by gt sling.
	// agent_state='spawning' means the polecat bead was created (with hook_bead
	// set atomically) but the tmux session hasn't been launched yet. Restarting
	// here would create a second Claude process alongside the one gt sling is
	// about to start, causing the double-spawn bug (issue #1752).
	//
	// Time-bound: only skip if the bead was updated recently (within 5 minutes).
	// If gt sling crashed during spawn, the polecat would be stuck in 'spawning'
	// indefinitely. The Witness patrol also catches spawning-as-zombie, but a
	// time-bound here makes the daemon self-sufficient for this edge case.
	if beads.AgentState(info.State) == beads.AgentStateSpawning {
		if updatedAt, err := time.Parse(time.RFC3339, info.LastUpdate); err == nil {
			if time.Since(updatedAt) < 5*time.Minute {
				d.logger.Printf("Skipping restart for %s/%s: agent_state=spawning (gt sling in progress, updated %s ago)",
					rigName, polecatName, time.Since(updatedAt).Round(time.Second))
				return
			}
			d.logger.Printf("Spawning guard expired for %s/%s: agent_state=spawning but last updated %s ago (>5m), proceeding with crash detection",
				rigName, polecatName, time.Since(updatedAt).Round(time.Second))
		} else {
			// Can't parse timestamp — be safe, skip restart during spawning
			d.logger.Printf("Skipping restart for %s/%s: agent_state=spawning (gt sling in progress, unparseable updated_at)",
				rigName, polecatName)
			return
		}
	}

	// TOCTOU guard: re-verify session is still dead before restarting.
	// Between the initial check and now, the session may have been restarted
	// by another heartbeat cycle, witness, or the polecat itself.
	sessionRevived, err := d.tmux.HasSession(sessionName)
	if err == nil && sessionRevived {
		return // Session came back - no restart needed
	}

	// Polecat has work but session is dead - this is a crash!
	d.logger.Printf("CRASH DETECTED: polecat %s/%s has hook_bead=%s but session %s is dead",
		rigName, polecatName, info.HookBead, sessionName)

	// Track this death for mass death detection
	recordSessionDeath(d, sessionName)

	// Emit session_death event for audit trail / feed visibility
	_ = events.LogFeed(events.TypeSessionDeath, sessionName,
		events.SessionDeathPayload(sessionName, rigName+"/polecats/"+polecatName, "crash detected by daemon health check", "daemon"))

	// Notify witness — stuck-agent-dog plugin handles context-aware restart
	notifyWitnessOfCrashedPolecat(d, rigName, polecatName, info.HookBead)
}

// recordSessionDeath records a session death and checks for mass death pattern.
func recordSessionDeath(d *Daemon, sessionName string) {
	d.deathsMu.Lock()
	defer d.deathsMu.Unlock()

	now := time.Now()

	// Add this death
	d.recentDeaths = append(d.recentDeaths, sessionDeath{
		sessionName: sessionName,
		timestamp:   now,
	})

	// Prune deaths outside the window
	cutoff := now.Add(-massDeathWindow)
	var recent []sessionDeath
	for _, death := range d.recentDeaths {
		if death.timestamp.After(cutoff) {
			recent = append(recent, death)
		}
	}
	d.recentDeaths = recent

	// Check for mass death
	if len(d.recentDeaths) >= massDeathThreshold {
		emitMassDeathEvent(d)
	}
}

// emitMassDeathEvent logs a mass death event when multiple sessions die in a short window.
func emitMassDeathEvent(d *Daemon) {
	// Collect session names
	var sessions []string
	for _, death := range d.recentDeaths {
		sessions = append(sessions, death.sessionName)
	}

	count := len(sessions)
	window := massDeathWindow.String()

	d.logger.Printf("MASS DEATH DETECTED: %d sessions died in %s: %v", count, window, sessions)

	// Emit feed event
	_ = events.LogFeed(events.TypeMassDeath, "daemon",
		events.MassDeathPayload(count, window, sessions, ""))

	// Clear the deaths to avoid repeated alerts
	d.recentDeaths = nil
}

// isBeadClosed checks if a bead's status is "closed" by querying bd show --json.
// Returns true if the bead exists and has status "closed", false otherwise.
// On any error (bead not found, bd failure), returns false to err on the side
// of crash detection rather than silently suppressing alerts.
func isBeadClosed(d *Daemon, beadID string) bool {
	cmd := exec.Command(d.bdPath, "show", beadID, "--json") //nolint:gosec // G204: args are constructed internally
	setSysProcAttr(cmd)
	cmd.Dir = d.config.TownRoot
	cmd.Env = bdReadOnlyRoutingEnv(d.config.TownRoot)

	output, err := cmd.Output()
	if err != nil {
		return false
	}

	var issues []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &issues); err != nil || len(issues) == 0 {
		return false
	}

	return issues[0].Status == "closed"
}

// hasAssignedOpenWork checks if any work bead is assigned to the given polecat
// with a non-terminal status (hooked, in_progress, or open). This is the
// authoritative source of polecat work — the sling code sets status=hooked +
// assignee on the work bead, but no longer maintains the agent bead's hook_bead
// field (updateAgentHookBead is a no-op). Without this fallback, the idle reaper
// kills working polecats whose agent bead hook_bead is stale.
func hasAssignedOpenWork(d *Daemon, rigName, assignee string) bool {
	rigDir := beads.GetRigDirForName(d.config.TownRoot, rigName)

	for _, status := range []string{"hooked", "in_progress", "open"} {
		args := beads.InjectFlatForListJSON([]string{"list", "--assignee=" + assignee, "--status=" + status, "--json"})
		cmd := exec.Command(d.bdPath, args...) //nolint:gosec // G204: args are constructed internally
		cmd.Dir = d.config.TownRoot
		if rigDir != "" {
			cmd.Env = bdReadOnlyPinnedEnv(beads.ResolveBeadsDir(rigDir))
		} else {
			cmd.Env = bdReadOnlyRoutingEnv(d.config.TownRoot)
		}
		output, err := cmd.Output()
		if err != nil {
			continue
		}
		var issues []json.RawMessage
		if json.Unmarshal(output, &issues) == nil && len(issues) > 0 {
			return true
		}
	}
	return false
}

// notifyWitnessOfCrashedPolecat notifies the witness when a polecat crash is detected.
// The stuck-agent-dog plugin handles context-aware restart decisions.
func notifyWitnessOfCrashedPolecat(d *Daemon, rigName, polecatName, hookBead string) {
	witnessAddr := rigName + "/witness"
	subject := fmt.Sprintf("CRASHED_POLECAT: %s/%s detected", rigName, polecatName)
	body := fmt.Sprintf(`Polecat %s crash detected (session dead, work on hook).

hook_bead: %s

Restart deferred to stuck-agent-dog plugin for context-aware recovery.`,
		polecatName, hookBead)

	cmd := exec.Command(d.gtPath, "mail", "send", witnessAddr, "-s", subject, "-m", body) //nolint:gosec // G204: args are constructed internally
	setSysProcAttr(cmd)
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(os.Environ(), "BD_ACTOR=daemon") // Identify as daemon, not overseer
	if err := cmd.Run(); err != nil {
		d.logger.Printf("Warning: failed to notify witness of crashed polecat: %v", err)
	}
}

// reapIdlePolecats kills polecat tmux sessions that have been idle too long.
// The persistent polecat model (gt-4ac) keeps sessions alive after gt done for reuse,
// but idle sessions consume API slots (Claude Code process stays alive at 0% CPU).
// This reaper checks heartbeat state and kills sessions idle longer than the threshold.
func reapIdlePolecats(d *Daemon) {
	opCfg := d.loadOperationalConfig().GetDaemonConfig()
	idleTimeout := opCfg.PolecatIdleSessionTimeoutD()

	d.rigPool.RunPerRig(d.ctx, getKnownRigs(d), func(ctx context.Context, rigName string) error {
		reapRigIdlePolecats(d, rigName, idleTimeout)
		return nil
	})
}

// reapRigIdlePolecats checks all polecats in a rig and kills idle sessions.
func reapRigIdlePolecats(d *Daemon, rigName string, timeout time.Duration) {
	polecatsDir := filepath.Join(d.config.TownRoot, rigName, "polecats")
	polecats, err := listPolecatWorktrees(polecatsDir)
	if err != nil {
		return // No polecats directory
	}

	for _, polecatName := range polecats {
		reapIdlePolecat(d, rigName, polecatName, timeout)
	}
}

// reapIdlePolecat checks a single polecat and kills it if idle too long.
// A polecat is considered idle if:
//   - Heartbeat state is "exiting" or "idle" and timestamp exceeds threshold, OR
//   - Heartbeat state is "working" but timestamp is stale AND the polecat has no
//     hooked work (agent_state=idle in beads). This catches polecats that completed
//     gt done — persistentPreRun resets heartbeat to "working" on every gt sub-command,
//     so after gt done finishes the heartbeat shows "working" with a stale timestamp.
func reapIdlePolecat(d *Daemon, rigName, polecatName string, timeout time.Duration) {
	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)

	// Only check sessions that are actually alive
	alive, err := d.tmux.HasSession(sessionName)
	if err != nil || !alive {
		return
	}

	// Read heartbeat to check state and idle duration
	hb := polecat.ReadSessionHeartbeat(d.config.TownRoot, sessionName)
	if hb == nil {
		return // No heartbeat file — can't determine state
	}

	staleDuration := time.Since(hb.Timestamp)
	if staleDuration < timeout {
		return // Heartbeat is fresh — polecat is active
	}

	state := hb.EffectiveState()

	// Explicitly idle or exiting — safe to reap
	if state == polecat.HeartbeatIdle || state == polecat.HeartbeatExiting {
		killIdlePolecat(d, rigName, polecatName, sessionName, staleDuration, timeout, string(state))
		return
	}

	// Heartbeat says "working" but is stale — check if polecat actually has hooked work.
	// If agent_state=idle in beads and no hook_bead, the polecat finished gt done
	// and is sitting idle (heartbeat wasn't updated to "idle" because persistentPreRun
	// resets to "working" on every gt sub-command during gt done).
	if state == polecat.HeartbeatWorking {
		prefix := beads.GetPrefixForRig(d.config.TownRoot, rigName)
		agentBeadID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)
		info, err := d.getAgentBeadInfo(agentBeadID)
		if err != nil {
			// Agent bead lookup failed — use the authoritative work bead assignee
			// to determine whether the polecat has real work before reaping.
			// Bead infrastructure failures (Dolt issues, version mismatches) cause
			// spurious lookup errors while the polecat is actively working (GH#3342).
			assignee := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
			if hasAssignedOpenWork(d, rigName, assignee) {
				return
			}
			// No assigned work and agent not running — safe to reap.
			// Use 3x threshold (not 2x) to avoid killing polecats during transient
			// infrastructure degradation when the agent process is alive but not
			// detectable (e.g. long thinking sessions, slow process inspection).
			if staleDuration >= timeout*3 || !d.tmux.IsAgentAlive(sessionName) && staleDuration >= timeout*2 {
				killIdlePolecat(d, rigName, polecatName, sessionName, staleDuration, timeout, "working-bead-lookup-failed")
			}
			return
		}

		// If polecat has hooked work that is still open, it might be stuck (not idle).
		// Don't reap — let checkPolecatSessionHealth handle stuck polecats.
		// But if the hook_bead is closed, the work is done and this is just an idle
		// polecat with a stale hook reference — safe to reap.
		if info.HookBead != "" && !isBeadClosed(d, info.HookBead) {
			return
		}

		// Fallback: agent bead hook_bead may be stale (updateAgentHookBead is a
		// no-op since the sling code declared work bead assignee as authoritative).
		// Before killing, check if any work bead is assigned to this polecat with
		// a non-terminal status. This prevents the reaper from killing polecats
		// whose agent bead hook_bead points to a closed bead from a previous swarm
		// while the polecat is actively working on a newly-slung bead.
		assignee := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
		if hasAssignedOpenWork(d, rigName, assignee) {
			return
		}

		// No hooked work + stale heartbeat — but check if the agent process
		// is still actively running before reaping. A failed gt sling rollback
		// can clear the hook while the agent is still working (GH#3342).
		if d.tmux.IsAgentAlive(sessionName) {
			return
		}
		killIdlePolecat(d, rigName, polecatName, sessionName, staleDuration, timeout, "working-no-hook")
	}
}

// killIdlePolecat terminates an idle polecat session and cleans up.
func killIdlePolecat(d *Daemon, rigName, polecatName, sessionName string, idleDuration, timeout time.Duration, reason string) {
	d.logger.Printf("Reaping idle polecat %s/%s (state=%s, idle %v, threshold %v)",
		rigName, polecatName, reason, idleDuration.Truncate(time.Second), timeout)

	// Kill the tmux session (and all descendant processes)
	if err := d.tmux.KillSessionWithProcesses(sessionName); err != nil {
		d.logger.Printf("Warning: failed to kill idle polecat session %s: %v", sessionName, err)
		return
	}

	// Clean up heartbeat file
	polecat.RemoveSessionHeartbeat(d.config.TownRoot, sessionName)

	d.logger.Printf("Reaped idle polecat %s/%s — session killed, API slot freed", rigName, polecatName)

	// Emit feed event so the activity feed shows the reap
	_ = events.LogFeed(events.TypeSessionDeath, fmt.Sprintf("%s/%s", rigName, polecatName),
		events.SessionDeathPayload(sessionName, fmt.Sprintf("%s/polecats/%s", rigName, polecatName),
			fmt.Sprintf("idle-reap: %s, idle %v (threshold %v)", reason, idleDuration.Truncate(time.Second), timeout),
			"daemon"))
}

// cleanupOrphanedProcesses kills orphaned claude subagent processes.
// These are Task tool subagents that didn't clean up after completion.
// Detection uses TTY column: processes with TTY "?" have no controlling terminal.
// This is a safety net fallback - Deacon patrol also runs this more frequently.
func cleanupOrphanedProcesses(d *Daemon) {
	results, err := util.CleanupOrphanedClaudeProcesses()
	if err != nil {
		d.logger.Printf("Warning: orphan process cleanup failed: %v", err)
		return
	}

	if len(results) > 0 {
		d.logger.Printf("Orphan cleanup: processed %d process(es)", len(results))
		for _, r := range results {
			if r.Signal == "UNKILLABLE" {
				d.logger.Printf("  WARNING: PID %d (%s) survived SIGKILL", r.Process.PID, r.Process.Cmd)
			} else {
				d.logger.Printf("  Sent %s to PID %d (%s)", r.Signal, r.Process.PID, r.Process.Cmd)
			}
		}
	}
}

// pruneStaleBranches removes stale local polecat tracking branches from all rig clones.
// This runs in every heartbeat but is very fast when there are no stale branches.
func pruneStaleBranches(d *Daemon) {
	// pruneInDir prunes stale polecat branches in a single git directory.
	pruneInDir := func(dir, label string) {
		g := gitpkg.NewGit(dir)
		if !g.IsRepo() {
			return
		}

		// Fetch --prune first to clean up stale remote tracking refs
		_ = g.FetchPrune("origin")

		pruned, err := g.PruneStaleBranches("polecat/*", false)
		if err != nil {
			d.logger.Printf("Warning: branch prune failed for %s: %v", label, err)
			return
		}

		if len(pruned) > 0 {
			d.logger.Printf("Branch prune: removed %d stale polecat branch(es) in %s", len(pruned), label)
			for _, b := range pruned {
				d.logger.Printf("  %s (%s)", b.Name, b.Reason)
			}
		}
	}

	// Prune in each rig's git directory (parallel — each rig is independent).
	d.rigPool.RunPerRig(d.ctx, getKnownRigs(d), func(ctx context.Context, rigName string) error {
		rigPath := filepath.Join(d.config.TownRoot, rigName)
		pruneInDir(rigPath, rigName)
		return nil
	})

	// Also prune in the town root itself (mayor clone)
	pruneInDir(d.config.TownRoot, "town-root")
}

// dispatchQueuedWork shells out to `gt scheduler run` to dispatch scheduled beads.
// This avoids circular import between the daemon and cmd packages.
// Uses a 5m timeout to allow multi-bead dispatch with formula cooking and hook retries.
//
// Timeout safety: if the timeout fires mid-dispatch, a bead may be left with
// metadata written but label not yet swapped (or vice versa). The dispatch flock
// is released on process death, and dispatchSingleBead's label swap retry logic
// prevents double-dispatch on the next cycle. The batch_size config (default: 1)
// limits how many beads are in-flight per heartbeat, reducing the timeout window.
func dispatchQueuedWork(d *Daemon) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gt", "scheduler", "run")
	setSysProcAttr(cmd)
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(beads.BuildMutationRoutingBDEnv(os.Environ(), filepath.Join(d.config.TownRoot, ".beads")), "GT_DAEMON=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		d.logger.Printf("Scheduler dispatch timed out after 5m")
	} else if err != nil {
		d.logger.Printf("Scheduler dispatch failed: %v (output: %s)", err, string(out))
	} else if len(out) > 0 {
		d.logger.Printf("Scheduler dispatch: %s", string(out))
	}
}
