package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jonbaldie/gastown/internal/daemon"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/worker"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type downRun struct {
	t           *tmux.Tmux
	townRoot    string
	quiet       bool
	force       bool
	polecats    bool
	all         bool
	nuke        bool
	dryRun      bool
	allOK       bool
	rigs        []string
	crewStopped int
}

func runDown(cmd *cobra.Command, _ []string) error {
	d, cleanup, err := beginDownRun(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	stopDownPolecats(d)
	stopDownCrew(d)
	stopDownRigRoleSessions(d)
	stopDownTownSessions(d)
	stopDownDaemon(d)
	stopDownDoltStack(d)
	stopDownWorkerAndLegacy(d)
	stopDownOrphans(d)
	stopDownNuke(d)
	return finishDownRun(d)
}

func beginDownRun(cmd *cobra.Command) (*downRun, func(), error) {
	d := &downRun{
		quiet:    commandBoolFlag(cmd, "quiet"),
		force:    commandBoolFlag(cmd, "force"),
		polecats: commandBoolFlag(cmd, "polecats"),
		all:      commandBoolFlag(cmd, "all"),
		nuke:     commandBoolFlag(cmd, "nuke"),
		dryRun:   commandBoolFlag(cmd, "dry-run"),
		allOK:    true,
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	d.townRoot = townRoot
	d.t = tmux.NewTmux()
	if !d.t.IsAvailable() {
		return nil, nil, fmt.Errorf("tmux not available (is tmux installed and on PATH?)")
	}
	cleanup, err := acquireDownShutdownGuard(d)
	if err != nil {
		return nil, nil, err
	}
	if d.dryRun {
		fmt.Println("═══ DRY RUN: Preview of shutdown actions ═══")
		fmt.Println()
	}
	d.rigs = discoverRigs(townRoot)
	return d, cleanup, nil
}

func acquireDownShutdownGuard(d *downRun) (func(), error) {
	if d.dryRun {
		return func() {}, nil
	}
	lock, err := acquireShutdownLock(d.townRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot proceed: %w", err)
	}
	sentinelPath := filepath.Join(d.townRoot, ShutdownSentinel)
	_ = os.WriteFile(sentinelPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
	_ = d.t.SetExitEmpty(false)
	return func() {
		_ = lock.Unlock()
		_ = os.Remove(sentinelPath)
	}, nil
}

func printDownRunStatus(d *downRun, name string, ok bool, detail string) {
	printDownStatus(name, ok, detail, d.quiet)
}

func stopDownPolecats(d *downRun) {
	if !d.polecats {
		return
	}
	if d.dryRun {
		fmt.Println("Would stop polecats...")
	} else {
		fmt.Println("Stopping polecats...")
	}
	stopped := stopAllPolecats(d.t, d.townRoot, d.rigs, d.force, d.dryRun)
	detail := "none running"
	if stopped > 0 {
		if d.dryRun {
			detail = fmt.Sprintf("%d would stop", stopped)
		} else {
			detail = fmt.Sprintf("%d stopped", stopped)
		}
	}
	printDownRunStatus(d, "Polecats", true, detail)
	fmt.Println()
}

func stopDownCrew(d *downRun) {
	d.crewStopped = stopAllCrew(d.t, d.townRoot, d.rigs, d.force, d.dryRun)
	if d.crewStopped == 0 {
		return
	}
	detail := fmt.Sprintf("%d stopped", d.crewStopped)
	if d.dryRun {
		detail = fmt.Sprintf("%d would stop", d.crewStopped)
	}
	printDownRunStatus(d, "Crew", true, detail)
}

func stopDownRigRoleSessions(d *downRun) {
	for _, rigName := range d.rigs {
		stopDownRigSession(d, rigName, "Refinery", session.RefinerySessionName(session.PrefixFor(rigName)))
	}
	for _, rigName := range d.rigs {
		stopDownRigSession(d, rigName, "Witness", session.WitnessSessionName(session.PrefixFor(rigName)))
	}
}

func stopDownRigSession(d *downRun, rigName, kind, sessionName string) {
	label := fmt.Sprintf("%s (%s)", kind, rigName)
	if d.dryRun {
		if running, _ := d.t.HasSession(sessionName); running {
			printDownRunStatus(d, label, true, "would stop")
		}
		return
	}
	wasRunning, err := stopSession(d.t, sessionName, d.force)
	if err != nil {
		printDownRunStatus(d, label, false, err.Error())
		d.allOK = false
		return
	}
	detail := "not running"
	if wasRunning {
		detail = "stopped"
	}
	printDownRunStatus(d, label, true, detail)
}

func stopDownTownSessions(d *downRun) {
	for _, ts := range session.TownSessions() {
		if d.dryRun {
			if running, _ := d.t.HasSession(ts.SessionID); running {
				printDownRunStatus(d, ts.Name, true, "would stop")
			}
			continue
		}
		stopped, err := session.StopTownSession(d.t, ts, d.force)
		if err != nil {
			printDownRunStatus(d, ts.Name, false, err.Error())
			d.allOK = false
			continue
		}
		detail := "not running"
		if stopped {
			detail = "stopped"
		}
		printDownRunStatus(d, ts.Name, true, detail)
	}
}

func stopDownDaemon(d *downRun) {
	running, pid, daemonErr := daemon.IsRunning(d.townRoot)
	if daemonErr != nil {
		printDownRunStatus(d, "Daemon", false, fmt.Sprintf("status check failed: %v", daemonErr))
		d.allOK = false
		return
	}
	if d.dryRun {
		if running {
			printDownRunStatus(d, "Daemon", true, fmt.Sprintf("would stop (PID %d)", pid))
		}
		return
	}
	if !running {
		printDownRunStatus(d, "Daemon", true, "not running")
		return
	}
	if err := daemon.StopDaemon(d.townRoot); err != nil {
		printDownRunStatus(d, "Daemon", false, err.Error())
		d.allOK = false
		return
	}
	if pid > 0 {
		printDownRunStatus(d, "Daemon", true, fmt.Sprintf("stopped (was PID %d)", pid))
		return
	}
	printDownRunStatus(d, "Daemon", true, "stopped (stale lock cleaned)")
}

func stopDownDoltStack(d *downRun) {
	stopDownIdleMonitors(d)
	stopDownCanonicalDolt(d)
	stopDownDoltImposters(d)
}

func stopDownIdleMonitors(d *downRun) {
	idleMonitors := doltserver.FindIdleMonitorProcesses(d.townRoot)
	if len(idleMonitors) == 0 {
		return
	}
	if d.dryRun {
		printDownRunStatus(d, "Dolt idle-monitors", true, fmt.Sprintf("%d would stop", len(idleMonitors)))
		return
	}
	if stopped := stopIdleMonitors(idleMonitors); stopped > 0 {
		printDownRunStatus(d, "Dolt idle-monitors", true, fmt.Sprintf("stopped %d", stopped))
	}
}

func stopDownCanonicalDolt(d *downRun) {
	doltCfg := doltserver.DefaultConfig(d.townRoot)
	if _, statErr := os.Stat(doltCfg.DataDir); statErr != nil {
		return
	}
	doltRunning, doltPid, doltErr := doltserver.IsRunning(d.townRoot)
	if doltErr != nil {
		printDownRunStatus(d, "Dolt", false, fmt.Sprintf("status check failed: %v", doltErr))
		d.allOK = false
		return
	}
	if d.dryRun {
		if doltRunning {
			printDownRunStatus(d, "Dolt", true, fmt.Sprintf("would stop (PID %d)", doltPid))
		}
		return
	}
	if !doltRunning {
		printDownRunStatus(d, "Dolt", true, "not running")
		return
	}
	if err := doltserver.Stop(d.townRoot); err != nil {
		printDownRunStatus(d, "Dolt", false, err.Error())
		d.allOK = false
		return
	}
	printDownRunStatus(d, "Dolt", true, fmt.Sprintf("stopped (was PID %d)", doltPid))
}

func stopDownDoltImposters(d *downRun) {
	if d.dryRun {
		if conflictPID, _ := doltserver.CheckPortConflict(d.townRoot); conflictPID > 0 {
			printDownRunStatus(d, "Dolt imposters", true, fmt.Sprintf("would stop imposter (PID %d)", conflictPID))
		}
		if orphanDolts := findOrphanDoltServers(d.townRoot); len(orphanDolts) > 0 {
			printDownRunStatus(d, "Dolt orphans", true, fmt.Sprintf("%d rogue server(s) would stop", len(orphanDolts)))
		}
		return
	}
	if err := doltserver.KillImposters(d.townRoot); err != nil {
		printDownRunStatus(d, "Dolt imposters", false, err.Error())
		d.allOK = false
	}
	orphanDolts := findOrphanDoltServers(d.townRoot)
	if len(orphanDolts) == 0 {
		return
	}
	if stopped := stopOrphanDoltServers(orphanDolts); stopped > 0 {
		printDownRunStatus(d, "Dolt orphans", true, fmt.Sprintf("stopped %d rogue server(s)", stopped))
	}
}

func stopDownWorkerAndLegacy(d *downRun) {
	stopDownWorkerServe(d)
	stopDownBeadsDoltDirs(d)
	stopDownLegacySockets(d)
}

func stopDownWorkerServe(d *downRun) {
	workerPIDs := worker.FindServePIDs(d.townRoot)
	if len(workerPIDs) == 0 {
		if !d.dryRun {
			printDownRunStatus(d, "Worker serve", true, "not running")
		}
		return
	}
	if d.dryRun {
		printDownRunStatus(d, "Worker serve", true, fmt.Sprintf("%d would stop", len(workerPIDs)))
		return
	}
	stopped := worker.StopServe(d.townRoot)
	if leftover := worker.FindServePIDs(d.townRoot); len(leftover) > 0 {
		printDownRunStatus(d, "Worker serve", false, fmt.Sprintf("still running (PIDs %v)", leftover))
		d.allOK = false
		return
	}
	if stopped > 0 {
		printDownRunStatus(d, "Worker serve", true, fmt.Sprintf("stopped %d", stopped))
	}
}

func stopDownBeadsDoltDirs(d *downRun) {
	beadsDoltDirs := findBeadsDoltDirs(d.townRoot)
	if len(beadsDoltDirs) == 0 {
		return
	}
	if d.dryRun {
		printDownRunStatus(d, "Beads dolt dirs", true, fmt.Sprintf("%d would remove", len(beadsDoltDirs)))
		return
	}
	if removed := removeBeadsDoltDirs(beadsDoltDirs); removed > 0 {
		printDownRunStatus(d, "Beads dolt dirs", true, fmt.Sprintf("removed %d", removed))
	}
}

func stopDownLegacySockets(d *downRun) {
	if d.dryRun {
		if count := countLegacyDefaultSocketSessions(); count > 0 {
			printDownRunStatus(d, "Legacy sessions", true, fmt.Sprintf("%d would be cleaned from 'default' socket", count))
		}
		if count := countLegacyBaseSocketSessions(d.townRoot); count > 0 {
			printDownRunStatus(d, "Legacy sessions", true, fmt.Sprintf("%d would be cleaned from old basename socket", count))
		}
		return
	}
	if cleaned := cleanupLegacyDefaultSocket(); cleaned > 0 {
		printDownRunStatus(d, "Legacy sessions", true, fmt.Sprintf("cleaned %d from 'default' socket", cleaned))
	}
	if cleaned := cleanupLegacyBaseSocket(d.townRoot); cleaned > 0 {
		printDownRunStatus(d, "Legacy sessions", true, fmt.Sprintf("cleaned %d from old basename socket", cleaned))
	}
}

func stopDownOrphans(d *downRun) {
	if (!d.all && !d.force) || d.dryRun {
		return
	}
	fmt.Println()
	killed, pidErrs := session.KillTrackedPIDs(d.townRoot)
	if killed > 0 {
		fmt.Printf("  Killed %d tracked orphan process(es) via PID files\n", killed)
	}
	for _, e := range pidErrs {
		fmt.Printf("  PID cleanup warning: %s\n", e)
	}
	fmt.Println("Cleaning up orphaned Claude processes...")
	cleanupOrphanedClaude(defaultDownOrphanGraceSecs)
	time.Sleep(500 * time.Millisecond)
	respawned := verifyShutdown(d.t, d.townRoot)
	if len(respawned) == 0 {
		return
	}
	fmt.Println()
	fmt.Printf("%s Warning: Some processes may have respawned:\n", style.Bold.Render("⚠"))
	for _, r := range respawned {
		fmt.Printf("  • %s\n", r)
	}
	fmt.Println()
	fmt.Printf("This may indicate a process manager is respawning agents.\n")
	fmt.Printf("Check with:\n")
	fmt.Printf("  %s\n", style.Dim.Render("ps aux | grep claude  # Find respawned processes"))
	fmt.Printf("  %s\n", style.Dim.Render("gt status             # Verify town state"))
	d.allOK = false
}

func stopDownNuke(d *downRun) {
	if !d.nuke {
		return
	}
	socket := tmux.GetDefaultSocket()
	socketLabel := "default"
	if socket != "" {
		socketLabel = socket
	}
	if d.dryRun {
		printDownRunStatus(d, "Tmux server", true, fmt.Sprintf("would kill (socket: %s)", socketLabel))
		return
	}
	if os.Getenv("GT_NUKE_ACKNOWLEDGED") == "" {
		fmt.Println()
		fmt.Printf("%s The --nuke flag kills this town's tmux server (socket: %s).\n",
			style.Bold.Render("⚠ BLOCKED:"), socketLabel)
		fmt.Printf("This will destroy all tmux sessions on this socket, including any custom windows you opened.\n")
		fmt.Println()
		fmt.Printf("To proceed, run with: %s\n", style.Bold.Render("GT_NUKE_ACKNOWLEDGED=1 gt down --nuke"))
		d.allOK = false
		return
	}
	if err := d.t.KillServer(); err != nil {
		printDownRunStatus(d, "Tmux server", false, err.Error())
		d.allOK = false
		return
	}
	printDownRunStatus(d, "Tmux server", true, fmt.Sprintf("killed (socket: %s)", socketLabel))
}

func finishDownRun(d *downRun) error {
	fmt.Println()
	if d.dryRun {
		fmt.Println("═══ DRY RUN COMPLETE (no changes made) ═══")
		return nil
	}
	if !d.allOK {
		fmt.Printf("%s Some services failed to stop\n", style.Bold.Render("✗"))
		return fmt.Errorf("not all services stopped")
	}
	fmt.Printf("%s All services stopped\n", style.Bold.Render("✓"))
	_ = events.LogFeed(events.TypeHalt, "gt", events.HaltPayload(downStoppedServices(d)))
	return nil
}

func downStoppedServices(d *downRun) []string {
	stoppedServices := []string{"dolt", "daemon", "deacon", "boot", "mayor"}
	for _, rigName := range d.rigs {
		stoppedServices = append(stoppedServices, fmt.Sprintf("%s/refinery", rigName))
		stoppedServices = append(stoppedServices, fmt.Sprintf("%s/witness", rigName))
	}
	if d.crewStopped > 0 {
		stoppedServices = append(stoppedServices, "crew")
	}
	if d.polecats {
		stoppedServices = append(stoppedServices, "polecats")
	}
	if d.all {
		stoppedServices = append(stoppedServices, "bd-processes")
	}
	if d.nuke {
		stoppedServices = append(stoppedServices, "tmux-server")
	}
	return stoppedServices
}
