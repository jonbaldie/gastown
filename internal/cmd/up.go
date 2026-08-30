package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/daemon"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/refinery"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/witness"
	"github.com/spf13/cobra"
)

// agentStartResult holds the result of starting an agent.
type agentStartResult struct {
	name   string // Display name like "Witness (gastown)"
	ok     bool   // Whether start succeeded
	detail string // Status detail (session name or error)
}

// UpOutput represents the JSON output of the up command.
type UpOutput struct {
	Success  bool            `json:"success"`
	Services []ServiceStatus `json:"services"`
	Summary  UpSummary       `json:"summary"`
}

// ServiceStatus represents the status of a single service.
type ServiceStatus struct {
	Name   string `json:"name"`
	Type   string `json:"type"` // daemon, deacon, mayor, witness, refinery, crew, polecat
	Rig    string `json:"rig,omitempty"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// UpSummary provides counts for the up command output.
type UpSummary struct {
	Total   int `json:"total"`
	Started int `json:"started"`
	Failed  int `json:"failed"`
}

func buildUpSummary(services []ServiceStatus) UpSummary {
	started := 0
	failed := 0
	for _, svc := range services {
		if svc.OK {
			started++
		} else {
			failed++
		}
	}
	return UpSummary{
		Total:   len(services),
		Started: started,
		Failed:  failed,
	}
}

func emitUpJSON(w io.Writer, services []ServiceStatus) error {
	summary := buildUpSummary(services)
	output := UpOutput{
		Success:  summary.Failed == 0,
		Services: services,
		Summary:  summary,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(output); err != nil {
		return err
	}
	if summary.Failed > 0 {
		return NewSilentExit(1)
	}
	return nil
}

// maxConcurrentAgentStarts limits parallel agent startups to avoid resource
// exhaustion. Each agent start spawns a tmux session and runs gt prime, so
// more than ~10 concurrent starts can saturate CPU and cause timeouts.
const maxConcurrentAgentStarts = 10

// daemonStartupGrace is how long to wait after spawning the daemon process
// before verifying it started. The daemon needs time to write its PID file.
// On Windows, DETACHED_PROCESS startup is slower so we allow extra time.
var daemonStartupGrace = func() time.Duration {
	if runtime.GOOS == "windows" {
		return 2 * time.Second
	}
	return 300 * time.Millisecond
}()

var upCmd = &cobra.Command{
	Use:     "up",
	GroupID: GroupServices,
	Short:   "Bring up all Gas Town services",
	Long: `Start all Gas Town long-lived services.

This is the idempotent "boot" command for Gas Town. It ensures all
infrastructure agents are running:

  • Dolt       - Shared SQL database server for beads
  • Daemon     - Go background process that pokes agents
  • Deacon     - Health orchestrator (monitors Mayor/Witnesses)
  • Mayor      - Global work coordinator
  • Witnesses  - Per-rig polecat managers
  • Refineries - Per-rig merge queue processors

Polecats are NOT started by this command - they are transient workers
spawned on demand by the Mayor or Witnesses.

Use --restore to also start:
  • Crew       - Per rig settings (settings/config.json crew.startup)
  • Polecats   - Those with pinned beads (work attached)

Running 'gt up' multiple times is safe - it only starts services that
aren't already running.

This command does not require a prior 'gt enable'. Enable is machine-wide
(shell hooks and Claude Code SessionStart), not town-scoped.`,
	RunE: runUp,
}

func init() {
	upCmd.Flags().BoolP("quiet", "q", false, "Only show errors (ignored with --json)")
	upCmd.Flags().Bool("restore", false, "Also restore crew (from settings) and polecats (from hooks)")
	upCmd.Flags().Bool("json", false, "Output as JSON")
	rootCmd.AddCommand(upCmd)
}

func runUp(cmd *cobra.Command, _ []string) error {
	u, err := prepareUpRun(cmd)
	if err != nil {
		return err
	}
	startUpParallelServices(u)
	collectUpTownServices(u)
	startUpRigAndRestore(u)
	return finishUpRun(u)
}

func printStatus(name string, ok bool, detail string, quiet bool) {
	if quiet && ok {
		return
	}
	if ok {
		fmt.Printf("%s %s: %s\n", style.SuccessPrefix, name, style.Dim.Render(detail))
	} else {
		fmt.Printf("%s %s: %s\n", style.ErrorPrefix, name, detail)
	}
}

// disableCurrentAgentDND resets DND for the current role context (if muted).
// Returns true when a change was applied.
func disableCurrentAgentDND(townRoot string) (bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return false, fmt.Errorf("getting current directory: %w", err)
	}

	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		// No role context (or not in role workspace): nothing to change.
		return false, nil
	}

	ctx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	agentBeadID := getAgentBeadID(ctx)
	if agentBeadID == "" {
		return false, nil
	}

	bd := beads.New(townRoot)
	level, err := bd.GetAgentNotificationLevel(agentBeadID)
	if err != nil {
		// Missing bead/field should not block startup.
		return false, nil
	}
	if level != beads.NotifyMuted {
		return false, nil
	}

	if err := bd.UpdateAgentNotificationLevel(agentBeadID, beads.NotifyNormal); err != nil {
		return false, fmt.Errorf("updating notification level for %s: %w", agentBeadID, err)
	}
	return true, nil
}

// ensureDaemon starts the daemon if not running.
func ensureDaemon(townRoot string) error {
	if err := clearStaleShutdownSentinel(townRoot); err != nil {
		return err
	}
	running, _, err := daemon.IsRunning(townRoot)
	if err != nil {
		return err
	}
	if running {
		return nil
	}
	return startDetachedDaemon(townRoot)
}

// rigPrefetchResult holds the result of loading a single rig config.
type rigPrefetchResult struct {
	index int
	rig   *rig.Rig
	err   error
}

// prefetchRigs loads all rig configs in parallel for faster agent startup.
// Returns a map of rig name to loaded Rig, and any errors encountered.
func prefetchRigs(rigNames []string) (map[string]*rig.Rig, map[string]error) {
	n := len(rigNames)
	if n == 0 {
		return make(map[string]*rig.Rig), make(map[string]error)
	}

	// Use channel to collect results without locking
	results := make(chan rigPrefetchResult, n)

	for i, name := range rigNames {
		go func(idx int, rigName string) {
			_, r, err := getRig(rigName)
			results <- rigPrefetchResult{index: idx, rig: r, err: err}
		}(i, name)
	}

	// Collect results - pre-allocate maps with capacity
	rigs := make(map[string]*rig.Rig, n)
	errors := make(map[string]error)

	for i := 0; i < n; i++ {
		res := <-results
		name := rigNames[res.index]
		if res.err != nil {
			errors[name] = res.err
		} else {
			rigs[name] = res.rig
		}
	}

	return rigs, errors
}

// startRigAgentsWithPrefetch starts all Witnesses and Refineries using pre-loaded rig configs.
// Uses a worker pool with fixed goroutine count to limit concurrency and reduce overhead.
func startRigAgentsWithPrefetch(rigNames []string, prefetchedRigs map[string]*rig.Rig, rigErrors map[string]error) (witnessResults, refineryResults map[string]agentStartResult) {
	n := len(rigNames)
	witnessResults = make(map[string]agentStartResult, n)
	refineryResults = make(map[string]agentStartResult, n)
	if n == 0 {
		return
	}
	recordPrefetchRigErrors(witnessResults, refineryResults, rigErrors)
	runAgentStartWorkers(prefetchedRigs, witnessResults, refineryResults)
	return
}

// upStartWitness starts a witness for the given rig and returns a result struct.
// Respects parked/docked status - skips starting if rig is not operational.
func upStartWitness(rigName string, r *rig.Rig) agentStartResult {
	name := "Witness (" + rigName + ")"

	// Check if rig is parked or docked (wisp + bead labels).
	// Skip the check if auto_start_on_up is set — that overrides dock status.
	// Also check deprecated auto_start_on_boot for backwards compatibility with
	// rigs that still have the old key in their config.
	if !r.GetBoolConfig("auto_start_on_up") && !r.GetBoolConfig("auto_start_on_boot") {
		townRoot := filepath.Dir(r.Path)
		if blocked, reason := IsRigParkedOrDocked(townRoot, rigName); blocked {
			return agentStartResult{name: name, ok: true, detail: fmt.Sprintf("skipped (rig %s)", reason)}
		}
	}

	mgr := witness.NewManager(r)
	if err := mgr.Start(false, "", nil); err != nil {
		if err == witness.ErrAlreadyRunning {
			return agentStartResult{name: name, ok: true, detail: mgr.SessionName()}
		}
		return agentStartResult{name: name, ok: false, detail: err.Error()}
	}
	return agentStartResult{name: name, ok: true, detail: mgr.SessionName()}
}

// upStartRefinery starts a refinery for the given rig and returns a result struct.
// Respects parked/docked status - skips starting if rig is not operational.
func upStartRefinery(rigName string, r *rig.Rig) agentStartResult {
	name := "Refinery (" + rigName + ")"

	// Check if rig is parked or docked (wisp + bead labels).
	// Skip the check if auto_start_on_up is set — that overrides dock status.
	// Also check deprecated auto_start_on_boot for backwards compatibility with
	// rigs that still have the old key in their config.
	if !r.GetBoolConfig("auto_start_on_up") && !r.GetBoolConfig("auto_start_on_boot") {
		townRoot := filepath.Dir(r.Path)
		if blocked, reason := IsRigParkedOrDocked(townRoot, rigName); blocked {
			return agentStartResult{name: name, ok: true, detail: fmt.Sprintf("skipped (rig %s)", reason)}
		}
	}

	mgr := refinery.NewManager(r)
	if err := mgr.Start(false, ""); err != nil {
		if errors.Is(err, refinery.ErrAlreadyRunning) {
			return agentStartResult{name: name, ok: true, detail: mgr.SessionName()}
		}
		if errors.Is(err, refinery.ErrForkRig) {
			return agentStartResult{name: name, ok: true, detail: "skipped (fork-backed rig; use PR workflow)"}
		}
		return agentStartResult{name: name, ok: false, detail: err.Error()}
	}
	return agentStartResult{name: name, ok: true, detail: mgr.SessionName()}
}

// discoverRigs finds all rigs in the town.
func discoverRigs(townRoot string) []string {
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if rigsConfig, err := config.LoadRigsConfig(rigsConfigPath); err == nil {
		rigs := make([]string, 0, len(rigsConfig.Rigs))
		for name := range rigsConfig.Rigs {
			rigs = append(rigs, name)
		}
		return rigs
	}
	return scanTownRigs(townRoot)
}

// startCrewFromSettings starts crew members based on rig settings.
// Returns list of started crew names and map of errors.
func startCrewFromSettings(townRoot, rigName string) ([]string, map[string]error) {
	toStart, crewMgr, ok := loadCrewStartupNames(townRoot, rigName)
	if !ok {
		return []string{}, map[string]error{}
	}
	return startNamedCrewMembers(crewMgr, toStart)
}

// parseCrewStartupPreference parses the natural language crew startup preference.
// Examples: "max", "joe and max", "all", "none", "pick one"
func parseCrewStartupPreference(pref string, available []string) []string {
	pref = strings.ToLower(strings.TrimSpace(pref))
	switch pref {
	case "none", "":
		return []string{}
	case "all":
		return available
	case "pick one", "any", "any one":
		if len(available) > 0 {
			return []string{available[0]}
		}
		return []string{}
	}
	return parseCrewIncludeExclude(pref, available)
}

// startPolecatsWithWork starts polecats that have pinned beads (work attached).
// Returns list of started polecat names and map of errors.
func startPolecatsWithWork(townRoot, rigName string) ([]string, map[string]error) {
	started := []string{}
	errs := map[string]error{}
	polecatsDir := filepath.Join(townRoot, rigName, "polecats")
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return started, errs
	}
	_, r, err := getRig(rigName)
	if err != nil {
		return started, errs
	}
	polecatMgr := polecat.NewSessionManager(tmux.NewTmux(), r)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		started = startOnePolecatWithWork(polecatMgr, rigName, polecatsDir, entry.Name(), started, errs)
	}
	return started, errs
}

// doltReadyTimeout is how long gt up waits for the Dolt SQL server to accept
// connections before proceeding with witness/refinery startup. 10 seconds is
// generous: doltserver.Start() already retries for 5s, so this covers the case
// where the daemon (not gt up) started Dolt and it's still initializing.
const doltReadyTimeout = 10 * time.Second

// waitForDoltReady waits for the Dolt SQL server to be reachable before
// starting agents that depend on beads database access. If the server is not
// configured (no server-mode metadata), this is a no-op. If the timeout
// expires, logs a warning and continues (graceful degradation). (gt-zou1n)
func waitForDoltReady(townRoot string) {
	if err := doltserver.WaitForReady(townRoot, doltReadyTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v (agents may see connection errors)\n", err)
	}
}

// recoverOrphanedBeads scans each rig for beads stuck in hooked/in_progress
// status assigned to polecats that no longer exist (tmux session dead AND
// worktree directory removed). For each orphan, the bead is reset to open
// and the deacon is notified for re-dispatch.
//
// This runs during gt up after Dolt is ready, before witnesses start their
// own patrol. It catches the crash-recovery case where polecats die and
// their beads are never re-slung. (gas-udp)
func recoverOrphanedBeads(townRoot string, rigs []string, prefetchedRigs map[string]*rig.Rig) []ServiceStatus {
	var services []ServiceStatus

	bd := witness.DefaultBdCli()
	router := mail.NewRouterWithTownRoot(townRoot, townRoot)

	for _, rigName := range rigs {
		if _, ok := prefetchedRigs[rigName]; !ok {
			fmt.Fprintf(os.Stderr, "[orphan-recovery] skipping rig %s (failed to load)\n", rigName)
			continue // Rig failed to load — skip
		}

		rigPath := filepath.Join(townRoot, rigName)
		result := witness.DetectOrphanedBeads(bd, rigPath, rigName, router)

		if len(result.Orphans) == 0 {
			continue // No orphans in this rig
		}

		recovered := 0
		for _, orphan := range result.Orphans {
			if orphan.BeadRecovered {
				recovered++
			}
		}

		detail := fmt.Sprintf("found %d orphaned, recovered %d", len(result.Orphans), recovered)
		services = append(services, ServiceStatus{
			Name:   fmt.Sprintf("Orphan recovery (%s)", rigName),
			Type:   "recovery",
			Rig:    rigName,
			OK:     true,
			Detail: detail,
		})
	}

	// Flush any pending mail notifications before proceeding.
	router.WaitPendingNotifications()

	return services
}
