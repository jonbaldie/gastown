package daemon

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	beadsdk "github.com/jonbaldie/beads"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/feed"
)

func runDaemon(d *Daemon) (err error) {
	pid := os.Getpid()
	d.logger.Printf("Daemon starting (PID %d)", pid)
	startupComplete := false
	defer logDaemonExit(d, pid, &startupComplete, &err)

	fileLock, err := lockDaemon(d)
	if err != nil {
		return err
	}
	defer func() { _ = fileLock.Unlock() }()

	state, err := prepareDaemonRuntime(d)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(d.config.PidFile) }()

	if err := startDaemonServices(d); err != nil {
		return err
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, daemonSignals()...)
	timer := time.NewTimer(recoveryHeartbeatInterval(d))
	defer timer.Stop()
	d.logger.Printf("Daemon running, recovery heartbeat interval %v", recoveryHeartbeatInterval(d))

	tickers := startDaemonTickers(d)
	defer tickers.stop()

	heartbeat(d, state)
	startupComplete = true
	return runDaemonLoop(d, state, sigChan, timer, tickers)
}

func logDaemonExit(d *Daemon, pid int, startupComplete *bool, err *error) {
	if err == nil || *err == nil {
		return
	}
	if *startupComplete {
		d.logger.Printf("Daemon exiting with error (PID %d): %v", pid, *err)
		return
	}
	d.logger.Printf("Daemon startup failed (PID %d): %v", pid, *err)
}

func lockDaemon(d *Daemon) (*flock.Flock, error) {
	lockFile := filepath.Join(d.config.TownRoot, "daemon", "daemon.lock")
	fileLock := flock.New(lockFile)
	locked, err := fileLock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquiring lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("daemon already running (lock held by another process)")
	}
	return fileLock, nil
}

func prepareDaemonRuntime(d *Daemon) (*State, error) {
	if err := checkAllRigsDolt(d); err != nil {
		return nil, err
	}
	if _, errs := doltserver.EnsureAllMetadata(d.config.TownRoot); len(errs) > 0 {
		for _, e := range errs {
			d.logger.Printf("Warning: metadata repair: %v", e)
		}
	}
	if _, err := writePIDFile(d.config.PidFile, os.Getpid()); err != nil {
		return nil, fmt.Errorf("writing PID file: %w", err)
	}
	state := &State{Running: true, PID: os.Getpid(), StartedAt: time.Now()}
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save state: %v", err)
	}
	return state, nil
}

func startDaemonServices(d *Daemon) error {
	startFeedCurator(d)
	if err := openDaemonBeadsStores(d); err != nil {
		return err
	}
	cleanupLegacySocketSessions(d)
	startConvoyManager(d)
	startKRCPruner(d)
	return nil
}

func startFeedCurator(d *Daemon) {
	d.curator = feed.NewCurator(d.config.TownRoot)
	if err := d.curator.Start(); err != nil {
		d.logger.Printf("Warning: failed to start feed curator: %v", err)
		return
	}
	d.logger.Println("Feed curator started")
}

func openDaemonBeadsStores(d *Daemon) error {
	stores, err := openBeadsStores(d)
	if err != nil {
		return err
	}
	d.beadsStores = stores
	return nil
}

func startConvoyManager(d *Daemon) {
	isRigParked := func(rigName string) bool {
		ok, _ := isRigOperational(d, rigName)
		return !ok
	}
	var storeOpener func() map[string]beadsdk.Storage
	if len(d.beadsStores) == 0 {
		storeOpener = func() map[string]beadsdk.Storage {
			stores, err := openBeadsStores(d)
			if err != nil {
				d.logger.Printf("Convoy: beads compatibility check failed: %v", err)
				return nil
			}
			return stores
		}
	}
	d.convoyManager = NewConvoyManager(d.config.TownRoot, d.logger.Printf, d.gtPath, 0, d.beadsStores, storeOpener, isRigParked)
	if err := d.convoyManager.Start(); err != nil {
		d.logger.Printf("Warning: failed to start convoy manager: %v", err)
	} else {
		d.logger.Println("Convoy manager started")
	}
	wireDoltRecoverySweep(d)
}

func wireDoltRecoverySweep(d *Daemon) {
	if d.doltServer == nil {
		return
	}
	cm := d.convoyManager
	d.doltServer.SetRecoveryCallback(func() {
		d.logger.Printf("Dolt recovery detected: triggering convoy recovery sweep")
		cm.scan()
	})
}

func startKRCPruner(d *Daemon) {
	krcPruner, err := NewKRCPruner(d.config.TownRoot, d.logger.Printf)
	if err != nil {
		d.logger.Printf("Warning: failed to create KRC pruner: %v", err)
		return
	}
	d.krcPruner = krcPruner
	if err := d.krcPruner.Start(); err != nil {
		d.logger.Printf("Warning: failed to start KRC pruner: %v", err)
		return
	}
	d.logger.Println("KRC pruner started")
}

type daemonTickers struct {
	stops []func()
	chans map[string]<-chan time.Time
}

func startDaemonTickers(d *Daemon) *daemonTickers {
	t := &daemonTickers{chans: map[string]<-chan time.Time{}}
	startDoltHealthTicker(d, t)
	startPatrolTicker(d, t, "dolt_remotes", doltRemotesInterval(d.patrolConfig), "Dolt remotes push ticker started")
	startPatrolTicker(d, t, "dolt_backup", doltBackupInterval(d.patrolConfig), "Dolt backup ticker started")
	startPatrolTicker(d, t, "jsonl_git_backup", jsonlGitBackupInterval(d.patrolConfig), "JSONL git backup ticker started")
	startPatrolTicker(d, t, "wisp_reaper", wispReaperInterval(d.patrolConfig), "Wisp reaper ticker started")
	startPatrolTicker(d, t, "doctor_dog", doctorDogInterval(d.patrolConfig), "Doctor dog ticker started")
	startPatrolTicker(d, t, "compactor_dog", compactorDogInterval(d.patrolConfig), "Compactor dog ticker started")
	startPatrolTicker(d, t, "checkpoint_dog", checkpointDogInterval(d.patrolConfig), "Checkpoint dog ticker started")
	startMaintenanceTicker(d, t)
	startPatrolTicker(d, t, "main_branch_test", mainBranchTestInterval(d.patrolConfig), "Main branch test ticker started")
	startPatrolTicker(d, t, "quota_dog", quotaDogInterval(d.patrolConfig), "Quota dog ticker started")
	return t
}

func startDoltHealthTicker(d *Daemon, t *daemonTickers) {
	if d.doltServer == nil || !d.doltServer.IsEnabled() {
		return
	}
	interval := d.doltServer.HealthCheckInterval()
	ticker := time.NewTicker(interval)
	t.chans["dolt_health"] = ticker.C
	t.stops = append(t.stops, ticker.Stop)
	d.logger.Printf("Dolt health check ticker started (interval %v)", interval)
}

func startPatrolTicker(d *Daemon, t *daemonTickers, name string, interval time.Duration, startedMsg string) {
	if !d.isPatrolActive(name) {
		return
	}
	ticker := time.NewTicker(interval)
	t.chans[name] = ticker.C
	t.stops = append(t.stops, ticker.Stop)
	d.logger.Printf("%s (interval %v)", startedMsg, interval)
}

func startMaintenanceTicker(d *Daemon, t *daemonTickers) {
	if !d.isPatrolActive("scheduled_maintenance") {
		return
	}
	interval := maintenanceCheckInterval(d.patrolConfig)
	ticker := time.NewTicker(interval)
	t.chans["scheduled_maintenance"] = ticker.C
	t.stops = append(t.stops, ticker.Stop)
	d.logger.Printf("Scheduled maintenance ticker started (check interval %v, window %s)", interval, maintenanceWindow(d.patrolConfig))
}

func (t *daemonTickers) stop() {
	for _, stop := range t.stops {
		stop()
	}
}

func startDaemonPatrolMux(tickers *daemonTickers, stop <-chan struct{}) <-chan string {
	ch := make(chan string, 1)
	for name, src := range tickers.chans {
		forwardPatrolTicks(name, src, ch, stop)
	}
	return ch
}

func forwardPatrolTicks(name string, src <-chan time.Time, dest chan<- string, stop <-chan struct{}) {
	if src == nil {
		return
	}
	go func() {
		for {
			select {
			case <-src:
				select {
				case dest <- name:
				case <-stop:
					return
				}
			case <-stop:
				return
			}
		}
	}()
}

func runDaemonLoop(d *Daemon, state *State, sigChan <-chan os.Signal, timer *time.Timer, tickers *daemonTickers) error {
	stop := make(chan struct{})
	defer close(stop)
	patrolCh := startDaemonPatrolMux(tickers, stop)
	for {
		select {
		case <-d.ctx.Done():
			d.logger.Println("Daemon context canceled, shutting down")
			return shutdown(d, state)
		case sig := <-sigChan:
			if err := handleDaemonSignal(d, state, sig); err != nil {
				return err
			}
		case name := <-patrolCh:
			handleDaemonPatrolTick(d, name)
		case <-timer.C:
			heartbeat(d, state)
			timer.Reset(recoveryHeartbeatInterval(d))
		}
	}
}

func handleDaemonSignal(d *Daemon, state *State, sig os.Signal) error {
	if isLifecycleSignal(sig) {
		d.logger.Println("Received lifecycle signal, processing lifecycle requests immediately")
		processLifecycleRequests(d)
		return nil
	}
	if isReloadRestartSignal(sig) {
		reloadDaemonRestartTracker(d)
		return nil
	}
	d.logger.Printf("Received signal %v, shutting down", sig)
	return shutdown(d, state)
}

func reloadDaemonRestartTracker(d *Daemon) {
	d.logger.Println("Received reload-restart signal, reloading restart tracker from disk")
	if d.restartTracker == nil {
		return
	}
	if err := d.restartTracker.Load(); err != nil {
		d.logger.Printf("Warning: failed to reload restart tracker: %v", err)
	}
}

func handleDaemonPatrolTick(d *Daemon, name string) {
	if isShutdownInProgress(d) {
		return
	}
	if runDoltPatrolTick(d, name) || runDogPatrolTick(d, name) {
		return
	}
	runWorkPatrolTick(d, name)
}

func runDoltPatrolTick(d *Daemon, name string) bool {
	switch name {
	case "dolt_health":
		ensureDoltServerRunning(d)
	case "dolt_remotes":
		d.pushDoltRemotes()
	case "dolt_backup":
		d.syncDoltBackups()
	case "jsonl_git_backup":
		d.syncJsonlGitBackup()
	default:
		return false
	}
	return true
}

func runDogPatrolTick(d *Daemon, name string) bool {
	switch name {
	case "wisp_reaper":
		d.reapWisps()
	case "doctor_dog":
		d.runDoctorDog()
	case "compactor_dog":
		d.runCompactorDog()
	case "checkpoint_dog":
		d.runCheckpointDog()
	default:
		return false
	}
	return true
}

func runWorkPatrolTick(d *Daemon, name string) {
	switch name {
	case "scheduled_maintenance":
		d.runScheduledMaintenance()
	case "main_branch_test":
		d.runMainBranchTests()
	case "quota_dog":
		d.runQuotaDog()
	}
}
