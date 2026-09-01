package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/crew"
	"github.com/jonbaldie/gastown/internal/daemon"
	"github.com/jonbaldie/gastown/internal/deacon"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/mayor"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/util"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type upRun struct {
	quiet, restore, jsonOutput bool
	townRoot                   string
	rigs                       []string
	allOK                      bool
	services                   []ServiceStatus
	start                      upStartState
}

type upStartState struct {
	daemonErr      error
	daemonPID      int
	deaconResult   agentStartResult
	mayorResult    agentStartResult
	prefetchedRigs map[string]*rig.Rig
	rigErrors      map[string]error
	doltOK         bool
	doltDetail     string
	doltSkipped    bool
}

func prepareUpRun(cmd *cobra.Command) (*upRun, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if err := daemon.EnsureLifecycleConfigFile(townRoot); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not configure lifecycle defaults: %v\n", err)
	}
	if patrolCfg := daemon.LoadPatrolConfig(townRoot); patrolCfg != nil {
		for k, v := range patrolCfg.Env {
			os.Setenv(k, v)
		}
	}
	config.ApplyConfiguredDoltEnv(townRoot)
	u := &upRun{
		quiet:      commandBoolFlag(cmd, "quiet"),
		restore:    commandBoolFlag(cmd, "restore"),
		jsonOutput: commandBoolFlag(cmd, "json"),
		townRoot:   townRoot,
		rigs:       discoverRigs(townRoot),
		allOK:      true,
	}
	warnUpDNDReset(u)
	return u, nil
}

func warnUpDNDReset(u *upRun) {
	changed, err := disableCurrentAgentDND(u.townRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not reset DND state: %v\n", err)
		return
	}
	if changed && !u.quiet {
		fmt.Printf("%s DND was enabled; reset to normal for current agent\n", style.SuccessPrefix)
	}
}

func startUpParallelServices(u *upRun) {
	var wg sync.WaitGroup
	wg.Add(5)
	go startUpDolt(u, &wg)
	go startUpDaemon(u, &wg)
	go startUpDeacon(u, &wg)
	go startUpMayor(u, &wg)
	go startUpPrefetch(u, &wg)
	wg.Wait()
}

func startUpDolt(u *upRun, wg *sync.WaitGroup) {
	defer wg.Done()
	cfg := doltserver.DefaultConfig(u.townRoot)
	if _, err := os.Stat(cfg.DataDir); os.IsNotExist(err) {
		u.start.doltSkipped = true
		return
	}
	running, _, _ := doltserver.IsRunning(u.townRoot)
	if running {
		u.start.doltOK = true
		u.start.doltDetail = "already running"
		return
	}
	if err := doltserver.Start(u.townRoot); err != nil {
		u.start.doltDetail = err.Error()
		return
	}
	u.start.doltOK = true
	u.start.doltDetail = fmt.Sprintf("started (port %d)", cfg.Port)
}

func startUpDaemon(u *upRun, wg *sync.WaitGroup) {
	defer wg.Done()
	if err := ensureDaemon(u.townRoot); err != nil {
		u.start.daemonErr = err
		return
	}
	running, pid, _ := daemon.IsRunning(u.townRoot)
	if running {
		u.start.daemonPID = pid
	}
}

func startUpDeacon(u *upRun, wg *sync.WaitGroup) {
	defer wg.Done()
	deaconMgr := deacon.NewManager(u.townRoot)
	if err := deaconMgr.Start(""); err != nil {
		if err == deacon.ErrAlreadyRunning {
			u.start.deaconResult = agentStartResult{name: "Deacon", ok: true, detail: deaconMgr.SessionName()}
			return
		}
		u.start.deaconResult = agentStartResult{name: "Deacon", ok: false, detail: err.Error()}
		return
	}
	u.start.deaconResult = agentStartResult{name: "Deacon", ok: true, detail: deaconMgr.SessionName()}
}

func startUpMayor(u *upRun, wg *sync.WaitGroup) {
	defer wg.Done()
	mayorMgr := mayor.NewManager(u.townRoot)
	if err := mayorMgr.Start(""); err != nil {
		if errors.Is(err, mayor.ErrAlreadyRunning) {
			u.start.mayorResult = agentStartResult{name: "Mayor", ok: true, detail: mayorMgr.SessionName()}
			return
		}
		if errors.Is(err, mayor.ErrACPActive) {
			u.start.mayorResult = agentStartResult{name: "Mayor", ok: true, detail: "ACP active"}
			return
		}
		u.start.mayorResult = agentStartResult{name: "Mayor", ok: false, detail: err.Error()}
		return
	}
	u.start.mayorResult = agentStartResult{name: "Mayor", ok: true, detail: mayorMgr.SessionName()}
}

func startUpPrefetch(u *upRun, wg *sync.WaitGroup) {
	defer wg.Done()
	u.start.prefetchedRigs, u.start.rigErrors = prefetchRigs(u.rigs)
}

func collectUpTownServices(u *upRun) {
	if !u.start.doltSkipped && u.start.doltOK {
		_, _ = doltserver.EnsureAllMetadata(u.townRoot)
	}
	appendUpDoltStatus(u)
	appendUpDaemonStatus(u)
	appendUpAgentStatus(u, u.start.deaconResult, constants.RoleDeacon)
	appendUpAgentStatus(u, u.start.mayorResult, constants.RoleMayor)
	prepareUpDoltEnv(u)
	if !u.start.doltSkipped && u.start.doltOK {
		u.services = append(u.services, recoverOrphanedBeads(u.townRoot, u.rigs, u.start.prefetchedRigs)...)
	}
}

func appendUpDoltStatus(u *upRun) {
	if u.start.doltSkipped {
		return
	}
	u.services = append(u.services, ServiceStatus{Name: "Dolt", Type: "dolt", OK: u.start.doltOK, Detail: u.start.doltDetail})
	if !u.start.doltOK {
		u.allOK = false
	}
}

func appendUpDaemonStatus(u *upRun) {
	if u.start.daemonErr != nil {
		u.services = append(u.services, ServiceStatus{Name: "Daemon", Type: "daemon", OK: false, Detail: u.start.daemonErr.Error()})
		u.allOK = false
		return
	}
	detail := "running (PID unknown)"
	if u.start.daemonPID > 0 {
		detail = fmt.Sprintf("PID %d", u.start.daemonPID)
	}
	u.services = append(u.services, ServiceStatus{Name: "Daemon", Type: "daemon", OK: true, Detail: detail})
}

func appendUpAgentStatus(u *upRun, result agentStartResult, role string) {
	u.services = append(u.services, ServiceStatus{Name: result.name, Type: role, OK: result.ok, Detail: result.detail})
	if !result.ok {
		u.allOK = false
	}
}

func prepareUpDoltEnv(u *upRun) {
	if u.start.doltSkipped || !u.start.doltOK {
		return
	}
	waitForDoltReady(u.townRoot)
	doltCfg := doltserver.DefaultConfig(u.townRoot)
	portStr := fmt.Sprintf("%d", doltCfg.Port)
	os.Setenv("GT_DOLT_PORT", portStr)
	os.Setenv("BEADS_DOLT_SERVER_PORT", portStr)
	os.Setenv("BEADS_DOLT_PORT", portStr)
	if doltCfg.Host != "" {
		os.Setenv("GT_DOLT_HOST", doltCfg.Host)
		os.Setenv("BEADS_DOLT_SERVER_HOST", doltCfg.Host)
	}
}

func startUpRigAndRestore(u *upRun) {
	witnessResults, refineryResults := startRigAgentsWithPrefetch(u.rigs, u.start.prefetchedRigs, u.start.rigErrors)
	appendUpRigAgentStatus(u, witnessResults, constants.RoleWitness)
	appendUpRigAgentStatus(u, refineryResults, constants.RoleRefinery)
	if u.restore {
		restoreUpCrewAndPolecats(u)
	}
}

func appendUpRigAgentStatus(u *upRun, results map[string]agentStartResult, role string) {
	for _, rigName := range u.rigs {
		result, ok := results[rigName]
		if !ok {
			continue
		}
		u.services = append(u.services, ServiceStatus{Name: result.name, Type: role, Rig: rigName, OK: result.ok, Detail: result.detail})
		if !result.ok {
			u.allOK = false
		}
	}
}

func restoreUpCrewAndPolecats(u *upRun) {
	for _, rigName := range u.rigs {
		started, failed := startCrewFromSettings(u.townRoot, rigName)
		appendUpRestoreServices(u, rigName, constants.RoleCrew, started, failed, session.CrewSessionName)
	}
	for _, rigName := range u.rigs {
		started, failed := startPolecatsWithWork(u.townRoot, rigName)
		appendUpRestoreServices(u, rigName, constants.RolePolecat, started, failed, session.PolecatSessionName)
	}
}

func appendUpRestoreServices(u *upRun, rigName, role string, started []string, failed map[string]error, sessionName func(string, string) string) {
	prefix := session.PrefixFor(rigName)
	label := "Crew"
	if role == constants.RolePolecat {
		label = "Polecat"
	}
	for _, name := range started {
		u.services = append(u.services, ServiceStatus{
			Name:   fmt.Sprintf("%s (%s/%s)", label, rigName, name),
			Type:   role,
			Rig:    rigName,
			OK:     true,
			Detail: sessionName(prefix, name),
		})
	}
	for name, err := range failed {
		u.services = append(u.services, ServiceStatus{
			Name:   fmt.Sprintf("%s (%s/%s)", label, rigName, name),
			Type:   role,
			Rig:    rigName,
			OK:     false,
			Detail: err.Error(),
		})
		u.allOK = false
	}
}

func finishUpRun(u *upRun) error {
	if u.allOK {
		startedServices := []string{"dolt", "daemon", "deacon", "mayor"}
		for _, rigName := range u.rigs {
			startedServices = append(startedServices, fmt.Sprintf("%s/witness", rigName), fmt.Sprintf("%s/refinery", rigName))
		}
		_ = events.LogFeed(events.TypeBoot, "gt", events.BootPayload("town", startedServices))
	}
	if u.jsonOutput {
		return emitUpJSON(os.Stdout, u.services)
	}
	for _, svc := range u.services {
		printStatus(svc.Name, svc.OK, svc.Detail, u.quiet)
	}
	fmt.Println()
	if u.allOK {
		fmt.Printf("%s All services running\n", style.Bold.Render("✓"))
		return nil
	}
	fmt.Printf("%s Some services failed to start\n", style.Bold.Render("✗"))
	return fmt.Errorf("not all services started")
}

func clearStaleShutdownSentinel(townRoot string) error {
	sentinelPath := filepath.Join(townRoot, ShutdownSentinel)
	data, err := os.ReadFile(sentinelPath)
	if err != nil {
		return nil
	}
	if shutdownSentinelIsStale(data) {
		os.Remove(sentinelPath)
		return nil
	}
	return fmt.Errorf("shutdown in progress (sentinel exists: %s)", sentinelPath)
}

func shutdownSentinelIsStale(data []byte) bool {
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return true
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	return process.Signal(syscall.Signal(0)) != nil
}

func startDetachedDaemon(townRoot string) error {
	gtPath, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(gtPath, "daemon", "run")
	cmd.Dir = townRoot
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	util.SetDetachedProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	time.Sleep(daemonStartupGrace)
	running, _, err := daemon.IsRunning(townRoot)
	if err != nil {
		return err
	}
	if running {
		return nil
	}
	if msg := readDaemonStartupFailure(townRoot, cmd.Process.Pid); msg != "" {
		return fmt.Errorf("daemon failed to start: %s", msg)
	}
	return fmt.Errorf("daemon failed to start (check logs with 'gt daemon logs')")
}

type agentTask struct {
	rigName   string
	rigObj    *rig.Rig
	isWitness bool
}

type agentResultMsg struct {
	rigName   string
	isWitness bool
	result    agentStartResult
}

func recordPrefetchRigErrors(witnessResults, refineryResults map[string]agentStartResult, rigErrors map[string]error) {
	for rigName, err := range rigErrors {
		errDetail := err.Error()
		witnessResults[rigName] = agentStartResult{name: "Witness (" + rigName + ")", ok: false, detail: errDetail}
		refineryResults[rigName] = agentStartResult{name: "Refinery (" + rigName + ")", ok: false, detail: errDetail}
	}
}

func runAgentStartWorkers(prefetchedRigs map[string]*rig.Rig, witnessResults, refineryResults map[string]agentStartResult) {
	numTasks := len(prefetchedRigs) * 2
	if numTasks == 0 {
		return
	}
	tasks := make(chan agentTask, numTasks)
	results := make(chan agentResultMsg, numTasks)
	numWorkers := maxConcurrentAgentStarts
	if numTasks < numWorkers {
		numWorkers = numTasks
	}
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go agentStartWorker(&wg, tasks, results)
	}
	for rigName, r := range prefetchedRigs {
		tasks <- agentTask{rigName: rigName, rigObj: r, isWitness: true}
		tasks <- agentTask{rigName: rigName, rigObj: r, isWitness: false}
	}
	close(tasks)
	go func() {
		wg.Wait()
		close(results)
	}()
	for msg := range results {
		if msg.isWitness {
			witnessResults[msg.rigName] = msg.result
		} else {
			refineryResults[msg.rigName] = msg.result
		}
	}
}

func agentStartWorker(wg *sync.WaitGroup, tasks <-chan agentTask, results chan<- agentResultMsg) {
	defer wg.Done()
	for task := range tasks {
		result := upStartRefinery(task.rigName, task.rigObj)
		if task.isWitness {
			result = upStartWitness(task.rigName, task.rigObj)
		}
		results <- agentResultMsg{rigName: task.rigName, isWitness: task.isWitness, result: result}
	}
}

func scanTownRigs(townRoot string) []string {
	var rigs []string
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return rigs
	}
	for _, entry := range entries {
		if name, ok := townDirRigName(townRoot, entry); ok {
			rigs = append(rigs, name)
		}
	}
	return rigs
}

func townDirRigName(townRoot string, entry os.DirEntry) (string, bool) {
	if !entry.IsDir() || isSkippedTownDir(entry.Name()) {
		return "", false
	}
	name := entry.Name()
	dirPath := filepath.Join(townRoot, name)
	if _, err := os.Stat(filepath.Join(dirPath, ".beads")); err == nil {
		return name, true
	}
	if _, err := os.Stat(filepath.Join(dirPath, "polecats")); err == nil {
		return name, true
	}
	return "", false
}

func isSkippedTownDir(name string) bool {
	return name == "mayor" || name == "daemon" || name == "deacon" || name == ".git" || name == "docs" || name[0] == '.'
}

func loadCrewStartupNames(townRoot, rigName string) ([]string, *crew.Manager, bool) {
	settings, err := config.LoadRigSettings(filepath.Join(townRoot, rigName, "settings", "config.json"))
	if err != nil || settings.Crew == nil || settings.Crew.Startup == "" {
		return nil, nil, false
	}
	crewMgr, _, err := getCrewManager(rigName)
	if err != nil {
		return nil, nil, false
	}
	crewWorkers, err := crewMgr.List()
	if err != nil || len(crewWorkers) == 0 {
		return nil, nil, false
	}
	crewNames := make([]string, len(crewWorkers))
	for i, w := range crewWorkers {
		crewNames[i] = w.Name
	}
	return parseCrewStartupPreference(settings.Crew.Startup, crewNames), crewMgr, true
}

func startNamedCrewMembers(crewMgr *crew.Manager, toStart []string) ([]string, map[string]error) {
	started := []string{}
	errs := map[string]error{}
	for _, crewName := range toStart {
		if err := crewMgr.Start(crewName, crew.StartOptions{}); err != nil {
			if err == crew.ErrSessionRunning {
				started = append(started, crewName)
			} else {
				errs[crewName] = err
			}
			continue
		}
		started = append(started, crewName)
	}
	return started, errs
}

func parseCrewIncludeExclude(pref string, available []string) []string {
	pref = strings.ReplaceAll(pref, " and ", ",")
	pref = strings.ReplaceAll(pref, ", but not ", ",-")
	pref = strings.ReplaceAll(pref, " but not ", ",-")
	include := []string{}
	exclude := map[string]bool{}
	for _, part := range strings.Split(pref, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "-") {
			exclude[strings.TrimPrefix(part, "-")] = true
			continue
		}
		include = append(include, part)
	}
	return filterAvailableCrew(include, exclude, available)
}

func filterAvailableCrew(include []string, exclude map[string]bool, available []string) []string {
	result := []string{}
	for _, name := range include {
		if exclude[name] {
			continue
		}
		for _, avail := range available {
			if avail == name {
				result = append(result, name)
				break
			}
		}
	}
	return result
}

func startOnePolecatWithWork(polecatMgr *polecat.SessionManager, rigName, polecatsDir, polecatName string, started []string, errs map[string]error) []string {
	polecatPath := filepath.Join(polecatsDir, polecatName)
	agentID := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
	pinnedBeads, err := beads.New(polecatPath).List(beads.ListOptions{
		Status:   beads.StatusPinned,
		Assignee: agentID,
		Priority: -1,
	})
	if err != nil || len(pinnedBeads) == 0 {
		return started
	}
	if err := polecatMgr.Start(polecatName, polecat.SessionStartOptions{}); err != nil {
		if err == polecat.ErrSessionRunning {
			return append(started, polecatName)
		}
		errs[polecatName] = err
		return started
	}
	return append(started, polecatName)
}
