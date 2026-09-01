package daemon

import (
	"time"

	"github.com/jonbaldie/gastown/internal/estop"
	"github.com/jonbaldie/gastown/internal/session"
)

func heartbeat(d *Daemon, state *State) {
	if skipHeartbeat(d) {
		return
	}

	d.metrics.RecordHeartbeat(d.ctx)
	d.logger.Println("Heartbeat starting (recovery-focused)")
	prepareHeartbeat(d)
	runHeartbeatPatrols(d)
	runHeartbeatRecovery(d)
	finishHeartbeat(d, state)
}

func skipHeartbeat(d *Daemon) bool {
	if isShutdownInProgress(d) {
		d.logger.Println("Shutdown in progress, skipping heartbeat")
		return true
	}
	if estop.IsActive(d.config.TownRoot) {
		d.logger.Println("E-STOP active, skipping agent management")
		return true
	}
	return false
}

func prepareHeartbeat(d *Daemon) {
	invalidateKnownRigsCache(d)
	if err := session.InitRegistry(d.config.TownRoot); err != nil {
		d.logger.Printf("Warning: failed to reload prefix registry: %v", err)
	}
	killDefaultPrefixGhosts(d)
	ensureDoltServerRunning(d)
}

func runHeartbeatPatrols(d *Daemon) {
	runHeartbeatDeaconPatrol(d)
	runHeartbeatWitnessPatrol(d)
	runHeartbeatRefineryPatrol(d)
	ensureMayorRunning(d)
	runHeartbeatHandlerPatrol(d)
}

func runHeartbeatDeaconPatrol(d *Daemon) {
	if !d.isPatrolActive("deacon") {
		d.logger.Printf("Deacon patrol disabled in config, skipping")
		killDeaconSessions(d)
		return
	}
	ensureDeaconRunning(d)
	ensureBootRunning(d)
	checkDeaconHeartbeat(d)
}

func runHeartbeatWitnessPatrol(d *Daemon) {
	if d.isPatrolActive("witness") {
		ensureWitnessesRunning(d)
		return
	}
	d.logger.Printf("Witness patrol disabled in config, skipping")
	killWitnessSessions(d)
}

func runHeartbeatRefineryPatrol(d *Daemon) {
	if !d.isPatrolActive("refinery") {
		d.logger.Printf("Refinery patrol disabled in config, skipping")
		killRefinerySessions(d)
		return
	}
	if p := d.checkPressure("refinery"); !p.OK {
		d.logger.Printf("Deferring refinery spawn: %s", p.Reason)
		return
	}
	ensureRefineriesRunning(d)
}

func runHeartbeatHandlerPatrol(d *Daemon) {
	if !d.isPatrolActive("handler") {
		d.logger.Printf("Handler patrol disabled in config, skipping")
		return
	}
	if p := d.checkPressure("dog"); !p.OK {
		d.logger.Printf("Deferring dog dispatch: %s", p.Reason)
		d.handleDogsCleanupOnly()
		return
	}
	d.handleDogs()
}

func runHeartbeatRecovery(d *Daemon) {
	processLifecycleRequests(d)
	d.checkGUPPViolations()
	d.checkOrphanedWork()
	checkPolecatSessionHealth(d)
	reapIdlePolecats(d)
	cleanupOrphanedProcesses(d)
	pruneStaleBranches(d)
	dispatchHeartbeatWork(d)
	rotateOversizedLogs(d)
}

func dispatchHeartbeatWork(d *Daemon) {
	if p := d.checkPressure("polecat"); !p.OK {
		d.logger.Printf("Deferring polecat dispatch: %s", p.Reason)
		return
	}
	dispatchQueuedWork(d)
}

func finishHeartbeat(d *Daemon, state *State) {
	state.LastHeartbeat = time.Now()
	state.HeartbeatCount++
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save state: %v", err)
	}
	d.logger.Printf("Heartbeat complete (#%d)", state.HeartbeatCount)
}
