package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentconfig "github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/doltserver"
)

const doltCmdTimeout = 15 * time.Second

// DefaultDoltHealthCheckInterval is how often the dedicated Dolt health check
// ticker fires, independent of the general daemon heartbeat (3 min).
// 30 seconds provides fast crash detection: a Dolt server crash is detected
// within 30s instead of up to 3 minutes.
const DefaultDoltHealthCheckInterval = 30 * time.Second

// DoltServerConfig holds configuration for the Dolt SQL server.
type DoltServerConfig struct {
	// Enabled controls whether the daemon manages a Dolt server.
	Enabled bool `json:"enabled"`

	// External indicates the server is externally managed (daemon monitors only).
	External bool `json:"external,omitempty"`

	// Port is the MySQL protocol port (default 3306).
	Port int `json:"port,omitempty"`

	// Host is the bind/connect address (default 127.0.0.1).
	Host string `json:"host,omitempty"`

	// User is the MySQL user name (default root).
	User string `json:"user,omitempty"`

	// Password is the MySQL password. Empty means no password.
	Password string `json:"password,omitempty"`

	// DataDir is the directory containing Dolt databases.
	// Each subdirectory becomes a database.
	DataDir string `json:"data_dir,omitempty"`

	// LogFile is the path to the Dolt server log file.
	LogFile string `json:"log_file,omitempty"`

	// AutoRestart controls whether to restart on crash.
	AutoRestart bool `json:"auto_restart,omitempty"`

	// RestartDelay is the initial delay before restarting after crash (default 5s).
	RestartDelay time.Duration `json:"restart_delay,omitempty"`

	// MaxRestartDelay is the maximum backoff delay (default 5min).
	MaxRestartDelay time.Duration `json:"max_restart_delay,omitempty"`

	// MaxRestartsInWindow is the maximum number of restarts allowed within
	// RestartWindow before escalating instead of retrying (default 5).
	MaxRestartsInWindow int `json:"max_restarts_in_window,omitempty"`

	// RestartWindow is the time window for counting restarts (default 10min).
	RestartWindow time.Duration `json:"restart_window,omitempty"`

	// HealthyResetInterval is how long the server must stay healthy before
	// the backoff counter resets (default 5min).
	HealthyResetInterval time.Duration `json:"healthy_reset_interval,omitempty"`

	// HealthCheckInterval is how often to run the Dolt health check,
	// independent of the general daemon heartbeat. This enables fast
	// detection of Dolt server crashes without changing the overall
	// heartbeat frequency. Default 30s.
	HealthCheckInterval time.Duration `json:"health_check_interval,omitempty"`
}

// DefaultDoltServerConfig returns sensible defaults for Dolt server config.
func DefaultDoltServerConfig(townRoot string) *DoltServerConfig {
	return &DoltServerConfig{
		Enabled:              false, // Opt-in
		Port:                 3306,
		Host:                 "127.0.0.1",
		User:                 "root",
		DataDir:              filepath.Join(townRoot, "dolt"),
		LogFile:              filepath.Join(townRoot, "daemon", "dolt-server.log"),
		AutoRestart:          true,
		RestartDelay:         5 * time.Second,
		MaxRestartDelay:      5 * time.Minute,
		MaxRestartsInWindow:  5,
		RestartWindow:        10 * time.Minute,
		HealthyResetInterval: 5 * time.Minute,
		HealthCheckInterval:  DefaultDoltHealthCheckInterval,
	}
}

// DoltServerStatus represents the current status of the Dolt server.
type DoltServerStatus struct {
	Running   bool      `json:"running"`
	PID       int       `json:"pid,omitempty"`
	Port      int       `json:"port,omitempty"`
	Host      string    `json:"host,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Version   string    `json:"version,omitempty"`
	Databases []string  `json:"databases,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// DoltServerState holds mutable lifecycle and health state for a Dolt server.
// It is embedded so DoltServerManager retains its existing selector surface
// while the stateful server concerns have one owner.
type DoltServerState struct {
	mu        sync.Mutex
	process   *os.Process
	startedAt time.Time
	lastCheck time.Time

	// Backoff state for restart logic
	currentDelay    time.Duration // Current backoff delay (grows exponentially)
	restartTimes    []time.Time   // Timestamps of recent restarts within window
	lastHealthyTime time.Time     // Last time the server was confirmed healthy
	escalated       bool          // Whether we've already escalated (avoid spamming)
	restarting      bool          // Whether a restart is in progress (guards against concurrent restarts)

	// Identity verification state
	lastIdentityCheck time.Time // Last time we ran the database identity check

	// Health check warnings (Option B throttling for doctor molecule).
	// Populated by checkHealthLocked(), consumed by Daemon.ensureDoltServerRunning().
	lastWarnings []string // Warnings from the most recent health check

	// onRecoveryFn is called (in a goroutine) when the Dolt server transitions
	// from unhealthy back to healthy, i.e., when the DOLT_UNHEALTHY signal file
	// is cleared after having been present. Set by SetRecoveryCallback.
	// Protected by mu.
	onRecoveryFn func()
}

// DoltServerHooks contains injectable implementations used by Dolt lifecycle
// tests. Nil hooks select the production implementations.
type DoltServerHooks struct {
	healthCheckFn     func() error
	writeProbeCheckFn func() error
	identityCheckFn   func() error // nil = use real VerifyServerDataDir
	startFn           func() error
	runningFn         func() (int, bool)
	stopFn            func()
	sleepFn           func(time.Duration)
	nowFn             func() time.Time
	escalateFn        func(int)
	unhealthyAlertFn  func(error)
	readOnlyAlertFn   func(error)
	crashAlertFn      func(int)
	listDatabasesFn   func() ([]string, error)
}

// DoltServerManager manages the Dolt SQL server lifecycle.
type DoltServerManager struct {
	config   *DoltServerConfig
	townRoot string
	logger   func(format string, v ...interface{})

	DoltServerState
	DoltServerHooks
}

// NewDoltServerManager creates a new Dolt server manager.
func NewDoltServerManager(townRoot string, config *DoltServerConfig, logger func(format string, v ...interface{})) *DoltServerManager {
	if config == nil {
		config = DefaultDoltServerConfig(townRoot)
	}
	config = normalizeDoltServerConfig(townRoot, config)
	return &DoltServerManager{
		config:   config,
		townRoot: townRoot,
		logger:   logger,
	}
}

func normalizeDoltServerConfig(townRoot string, config *DoltServerConfig) *DoltServerConfig {
	if config == nil {
		return nil
	}
	normalized := *config
	if host, port, ok := agentconfig.ManagedDoltEndpoint(townRoot); ok {
		normalized.Host = host
		if port > 0 {
			normalized.Port = port
		}
	}
	return &normalized
}

func doltStartedAt(m *DoltServerManager) time.Time {
	return m.startedAt
}

func doltRecoveryCallback(m *DoltServerManager) func() {
	return m.onRecoveryFn
}

func doltStartHook(m *DoltServerManager) func() error {
	return m.startFn
}

func (m *DoltServerManager) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now()
}

func (m *DoltServerManager) doSleep(d time.Duration) {
	if m.sleepFn != nil {
		m.sleepFn(d)
		return
	}
	time.Sleep(d)
}

// filterEnvKey returns env with all entries matching "<key>=..." removed.
func filterEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// isRunning checks if the Dolt server process is running.
// Must be called with m.mu held.
func (m *DoltServerManager) isRunning() (int, bool) {
	return isDoltServerRunning(m)
}

func isDoltServerRunning(m *DoltServerManager) (int, bool) {
	if m.runningFn != nil {
		return m.runningFn()
	}
	// First check our tracked process
	if m.process != nil {
		if isProcessAlive(m.process) {
			return m.process.Pid, true
		}
		// Process died, clear it
		m.process = nil
	}

	// Check PID file with nonce-based ownership verification
	pid, alive, err := verifyPIDOwnership(m.pidFile())
	if err != nil || pid == 0 {
		return 0, false
	}

	if !alive {
		// Process not running, clean up stale PID file
		_ = os.Remove(m.pidFile())
		return 0, false
	}

	// Verify it's actually our dolt server by checking port connectivity.
	// More reliable than ps string matching (ZFC fix: gt-utuk).
	if !m.isDoltServerOnPort() {
		_ = os.Remove(m.pidFile())
		return 0, false
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	m.process = process
	return pid, true
}

// EnsureRunning ensures the Dolt server is running.
// If not running, starts it. If running but unhealthy, restarts it.
// Uses exponential backoff and a max-restart cap to avoid crash-looping.
func (m *DoltServerManager) EnsureRunning() error {
	if !m.IsEnabled() {
		return nil
	}

	if m.IsExternal() {
		// External mode: just check health, don't manage lifecycle
		return m.checkHealth()
	}
	return ensureManagedRunning(m)
}

func ensureManagedRunning(m *DoltServerManager) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Another goroutine is already restarting — skip to avoid double-starts
	if m.restarting {
		m.logger("Dolt server restart already in progress, skipping")
		return nil
	}

	pid, running := m.isRunning()
	if running {
		return ensureHealthyRunning(m)
	}

	return restartStoppedServer(m, pid)
}

func ensureHealthyRunning(m *DoltServerManager) error {
	m.lastCheck = m.now()
	if err := m.checkHealthLocked(); err != nil {
		return restartForHealthFailure(m, err)
	}
	if err := m.checkWriteHealthLocked(); err != nil {
		return restartForReadOnlyFailure(m, err)
	}
	if err := checkIdentityIfDue(m); err != nil {
		return err
	}
	m.clearUnhealthySignal()
	m.maybeResetBackoff()
	return nil
}

func restartForHealthFailure(m *DoltServerManager, err error) error {
	m.logger("Dolt server unhealthy: %v, restarting...", err)
	if m.writeUnhealthySignal("health_check_failed", err.Error()) {
		sendUnhealthyAlert(m, err)
	} else {
		m.logger("Dolt incident already active; suppressing duplicate unhealthy alert")
	}
	m.stopLocked()
	return m.restartWithBackoff()
}

func restartForReadOnlyFailure(m *DoltServerManager, err error) error {
	m.logger("Dolt server read-only: %v, restarting...", err)
	if m.writeUnhealthySignal("read_only", err.Error()) {
		sendReadOnlyAlert(m, err)
	} else {
		m.logger("Dolt incident already active; suppressing duplicate read-only alert")
	}
	m.stopLocked()
	return m.restartWithBackoff()
}

func checkIdentityIfDue(m *DoltServerManager) error {
	const identityCheckInterval = 5 * time.Minute
	now := m.now()
	if now.Sub(m.lastIdentityCheck) < identityCheckInterval {
		return nil
	}
	m.lastIdentityCheck = now
	if err := m.checkDatabaseIdentityLocked(); err != nil {
		m.logger("Dolt server identity check failed: %v, restarting...", err)
		if m.writeUnhealthySignal("imposter_detected", err.Error()) {
			sendUnhealthyAlert(m, fmt.Errorf("identity check: %w", err))
		} else {
			m.logger("Dolt incident already active; suppressing duplicate identity alert")
		}
		m.stopLocked()
		if killErr := doltserver.KillImposters(m.townRoot); killErr != nil {
			m.logger("Warning: failed to kill imposters: %v", killErr)
		}
		time.Sleep(500 * time.Millisecond)
		return m.restartWithBackoff()
	}
	return nil
}

func restartStoppedServer(m *DoltServerManager, pid int) error {
	if pid > 0 {
		m.logger("Dolt server PID %d is dead, cleaning up and restarting...", pid)
		if m.writeUnhealthySignal("server_dead", fmt.Sprintf("PID %d is dead", pid)) {
			sendCrashAlert(m, pid)
		} else {
			m.logger("Dolt incident already active; suppressing duplicate crash alert")
		}
	}
	return m.restartWithBackoff()
}

// restartWithBackoff attempts to restart the Dolt server with exponential backoff
// and a max-restart cap. If the cap is exceeded, it escalates instead of retrying.
// Must be called with m.mu held.
func (m *DoltServerManager) restartWithBackoff() error {
	return restartDoltWithBackoff(m)
}

func restartDoltWithBackoff(m *DoltServerManager) error {
	now := m.now()

	// Prune restart times outside the window
	m.pruneRestartTimes(now)

	// Check if we've exceeded the restart cap
	maxRestarts := m.config.MaxRestartsInWindow
	if maxRestarts <= 0 {
		maxRestarts = 5
	}
	if len(m.restartTimes) >= maxRestarts {
		if !m.escalated {
			m.escalated = true
			m.logger("Dolt server restart cap reached (%d restarts in %v), escalating to mayor",
				len(m.restartTimes), m.config.RestartWindow)
			sendEscalationAlert(m, len(m.restartTimes))
		}
		return fmt.Errorf("dolt server restart cap exceeded (%d restarts in %v); escalated to mayor",
			len(m.restartTimes), m.config.RestartWindow)
	}

	// Mark restart in progress to prevent concurrent restarts during backoff sleep
	m.restarting = true
	defer func() { m.restarting = false }()

	// Apply exponential backoff delay
	delay := m.getBackoffDelay()
	if delay > 0 {
		m.logger("Backing off %v before Dolt server restart (attempt %d in window)",
			delay, len(m.restartTimes)+1)
		// Unlock during sleep so we don't hold the mutex during backoff
		m.mu.Unlock()
		m.doSleep(delay)
		m.mu.Lock()

		// Re-check after re-acquiring the lock: another goroutine may have
		// started the server while we were sleeping (TOCTOU guard).
		if _, running := m.isRunning(); running {
			m.logger("Dolt server started by another goroutine during backoff, skipping")
			return nil
		}
	}

	// Record this restart attempt
	m.restartTimes = append(m.restartTimes, m.now())

	// Advance the backoff for next time
	m.advanceBackoff()

	return m.startLocked()
}

// advanceBackoff doubles the current delay up to MaxRestartDelay.
func (m *DoltServerManager) advanceBackoff() {
	advanceDoltBackoff(m)
}

func advanceDoltBackoff(m *DoltServerManager) {
	baseDelay := m.config.RestartDelay
	if baseDelay <= 0 {
		baseDelay = 5 * time.Second
	}
	maxDelay := m.config.MaxRestartDelay
	if maxDelay <= 0 {
		maxDelay = 5 * time.Minute
	}

	if m.currentDelay <= 0 {
		m.currentDelay = baseDelay
	}
	m.currentDelay *= 2
	if m.currentDelay > maxDelay {
		m.currentDelay = maxDelay
	}
}

// pruneRestartTimes removes restart timestamps outside the configured window.
func (m *DoltServerManager) pruneRestartTimes(now time.Time) {
	pruneDoltRestartTimes(m, now)
}

func pruneDoltRestartTimes(m *DoltServerManager, now time.Time) {
	window := m.config.RestartWindow
	if window <= 0 {
		window = 10 * time.Minute
	}
	cutoff := now.Add(-window)
	pruned := m.restartTimes[:0]
	for _, t := range m.restartTimes {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	m.restartTimes = pruned
}

// maybeResetBackoff resets backoff state if the server has been healthy
// for the configured HealthyResetInterval.
// Must be called with m.mu held.
func (m *DoltServerManager) maybeResetBackoff() {
	resetDoltBackoffIfHealthy(m)
}

func resetDoltBackoffIfHealthy(m *DoltServerManager) {
	now := m.now()
	resetInterval := m.config.HealthyResetInterval
	if resetInterval <= 0 {
		resetInterval = 5 * time.Minute
	}

	if m.lastHealthyTime.IsZero() {
		m.lastHealthyTime = now
		return
	}

	if now.Sub(m.lastHealthyTime) >= resetInterval {
		if m.currentDelay > 0 || len(m.restartTimes) > 0 || m.escalated {
			m.logger("Dolt server healthy for %v, resetting backoff state", resetInterval)
			m.currentDelay = 0
			m.restartTimes = nil
			m.escalated = false
		}
		// Reset the healthy timestamp after a successful reset so the next
		// reset interval is measured from now, not from the original detection.
		m.lastHealthyTime = now
	}
}

func sendEscalationAlert(m *DoltServerManager, restartCount int) {
	if m.escalateFn != nil {
		m.escalateFn(restartCount)
		return
	}
	sendDoltEscalationAlert(m, restartCount)
}

func sendCrashAlert(m *DoltServerManager, deadPID int) {
	if m.crashAlertFn != nil {
		m.crashAlertFn(deadPID)
		return
	}
	sendDoltCrashAlert(m, deadPID)
}

func sendUnhealthyAlert(m *DoltServerManager, healthErr error) {
	if m.unhealthyAlertFn != nil {
		m.unhealthyAlertFn(healthErr)
		return
	}
	sendDoltUnhealthyAlert(m, healthErr)
}

// IsDoltUnhealthy checks if the DOLT_UNHEALTHY signal file exists.
// This is a package-level function for use by witness patrols and other consumers.
func IsDoltUnhealthy(townRoot string) bool {
	_, err := os.Stat(filepath.Join(townRoot, "daemon", "DOLT_UNHEALTHY"))
	return err == nil
}

// writeDaemonDoltConfig writes a Dolt config.yaml to configPath using the
// daemon's DoltServerConfig. Unlike CLI flags, config.yaml can set
// read_timeout_millis and write_timeout_millis, which prevents CLOSE_WAIT
// accumulation when clients disconnect without completing their SQL sessions.
func writeDaemonDoltConfig(cfg *DoltServerConfig, configPath string) error {
	content := fmt.Sprintf(`# Dolt SQL server configuration — managed by Gas Town daemon
# Do not edit manually; overwritten on each daemon-managed server start.

log_level: info

listener:
  port: %d%s
  read_timeout_millis: 30000
  write_timeout_millis: 30000
  max_connections: 1000

data_dir: %q

behavior:
  dolt_transaction_commit: false
%s%s%s`,
		cfg.Port,
		doltHostLine(cfg.Host),
		cfg.DataDir,
		doltEventSchedulerLine(),
		doltAutoGCBlock(),
		doltSystemVariablesBlock(),
	)
	return os.WriteFile(configPath, []byte(content), 0600)
}

func doltHostLine(host string) string {
	if host == "" {
		return ""
	}
	return fmt.Sprintf("\n  host: %s", host)
}

func doltEventSchedulerLine() string {
	scheduler, ok := os.LookupEnv("GT_DOLT_EVENT_SCHEDULER")
	if !ok {
		return "  event_scheduler: \"OFF\"\n"
	}
	if strings.EqualFold(scheduler, "omit") {
		return ""
	}
	if scheduler = strings.TrimSpace(scheduler); scheduler == "" {
		return "  event_scheduler: \"OFF\"\n"
	}
	return fmt.Sprintf("  event_scheduler: %q\n", strings.ToUpper(scheduler))
}

func doltSystemVariablesBlock() string {
	stats, ok := os.LookupEnv("GT_DOLT_STATS_ENABLED")
	if !ok {
		return "\nsystem_variables:\n  dolt_stats_enabled: 0\n"
	}
	if strings.EqualFold(stats, "omit") {
		return ""
	}
	if stats = strings.TrimSpace(stats); stats == "" {
		return "\nsystem_variables:\n  dolt_stats_enabled: 0\n"
	}
	return fmt.Sprintf("\nsystem_variables:\n  dolt_stats_enabled: %s\n", stats)
}

func doltAutoGCBlock() string {
	// Non-blocking storage GC bounds the sql-server's RSS (hq-excy9g); on by
	// default. GT_DOLT_AUTO_GC=off (or false/0/disabled) disables it at the next
	// Dolt restart without a source revert+rebuild — the runtime escape hatch.
	value := strings.ToLower(strings.TrimSpace(os.Getenv("GT_DOLT_AUTO_GC")))
	if value == "off" || value == "false" || value == "0" || value == "disabled" {
		return "  auto_gc_behavior:\n    enable: false\n    archive_level: 0\n"
	}
	return "  auto_gc_behavior:\n    enable: true\n    archive_level: 1\n"
}

func openDoltLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
}

func newDoltServerCommand(doltPath, dataDir, configPath string, logFile *os.File) *exec.Cmd {
	cmd := doltserver.NewSQLServerCommand(doltPath, dataDir, configPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSysProcAttr(cmd)
	return cmd
}

func waitForDoltServer(cmd *exec.Cmd, logFile *os.File, logger func(format string, v ...interface{})) {
	_ = cmd.Wait()
	if closeErr := logFile.Close(); closeErr != nil {
		logger("Warning: failed to close dolt log file: %v", closeErr)
	}
}

// Stop stops the Dolt SQL server.
func (m *DoltServerManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
	return nil
}

// checkHealthLocked checks health. Must be called with m.mu held.
func (m *DoltServerManager) checkHealthLocked() error {
	m.lastWarnings = nil // Reset warnings each check cycle.
	if m.healthCheckFn != nil {
		return m.healthCheckFn()
	}
	return runDoltHealthCheck(m)
}

// stopLocked stops the Dolt server. Must be called with m.mu held.
func (m *DoltServerManager) stopLocked() {
	stopDoltProcess(m)
}

func stopDoltProcess(m *DoltServerManager) {
	if m.stopFn != nil {
		m.stopFn()
		return
	}
	pid, running := m.isRunning()
	if !running {
		return
	}

	m.logger("Stopping Dolt SQL server (PID %d)...", pid)

	process, err := os.FindProcess(pid)
	if err != nil {
		return // Already gone
	}

	// Send termination signal for graceful shutdown
	if err := sendTermSignal(process); err != nil {
		m.logger("Warning: failed to send termination signal: %v", err)
	}

	// Wait for graceful shutdown (up to 5 seconds)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			if !isProcessAlive(process) {
				close(done)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	select {
	case <-done:
		m.logger("Dolt SQL server stopped gracefully")
	case <-time.After(30 * time.Second):
		// Force kill — 30s allows Dolt to flush its append-only journal under load.
		// A SIGKILL mid-journal-write causes corruption requiring dolt fsck to recover.
		m.logger("Dolt SQL server did not stop gracefully after 30s, forcing termination")
		_ = sendKillSignal(process)
	}

	// Clean up
	_ = os.Remove(m.pidFile())
	m.process = nil
}

// checkWriteHealthLocked probes the Dolt server's write capability by attempting
// a test write operation. If the server is in read-only mode (e.g., from concurrent
// write contention on the manifest), the write probe will fail with a characteristic
// error. Returns an error only if the server is confirmed read-only.
// Must be called with m.mu held.
func (m *DoltServerManager) checkWriteHealthLocked() error {
	if m.writeProbeCheckFn != nil {
		return m.writeProbeCheckFn()
	}
	return runDoltWriteHealthCheck(m)
}

// checkDatabaseIdentityLocked verifies the running Dolt server is serving the
// correct databases from the expected data directory. Detects "imposter" servers
// where another process (e.g., bd's embedded Dolt) hijacked the port.
// Must be called with m.mu held.
func (m *DoltServerManager) checkDatabaseIdentityLocked() error {
	if m.identityCheckFn != nil {
		return m.identityCheckFn()
	}
	return verifyDoltServerIdentity(m)
}

// formatDiskSize returns a human-readable size string.
func formatDiskSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// getDoltDatabases returns the list of databases. Uses the test hook if set.
func getDoltDatabases(m *DoltServerManager) ([]string, error) {
	if m.listDatabasesFn != nil {
		return m.listDatabasesFn()
	}
	return m.listDatabases()
}

// isReadOnlyError checks if an error message indicates a Dolt read-only state.
func isReadOnlyError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "read only") ||
		strings.Contains(lower, "read-only") ||
		strings.Contains(lower, "readonly")
}

func sendReadOnlyAlert(m *DoltServerManager, readOnlyErr error) {
	if m.readOnlyAlertFn != nil {
		m.readOnlyAlertFn(readOnlyErr)
		return
	}
	sendDoltReadOnlyAlert(m, readOnlyErr)
}

// CountDoltServers returns the count of running dolt sql-server processes.
// Uses lsof-based listener discovery instead of pgrep string matching (ZFC fix: gt-fj87).
func CountDoltServers() int {
	return len(doltserver.FindAllDoltListeners())
}

// StopAllDoltServers stops all dolt sql-server processes.
// Returns (killed, remaining).
// Uses lsof-based discovery and direct signal delivery instead of pkill -f (ZFC fix: gt-fj87).
func StopAllDoltServers(force bool) (int, int) {
	pids := uniqueDoltServerPIDs(doltserver.FindAllDoltListeners())
	if len(pids) == 0 {
		return 0, 0
	}
	before := len(pids)
	signalDoltServers(pids, force)

	if !force {
		time.Sleep(2 * time.Second)
		forceStopRemainingDoltServers()
	}

	time.Sleep(100 * time.Millisecond)
	after := CountDoltServers()
	return maxDoltServersKilled(before - after), after
}
