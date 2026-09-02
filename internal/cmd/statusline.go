package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/estop"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// maxConcurrentWorkingChecks bounds how many isSessionWorking checks (each a
// `tmux capture-pane` subprocess) run at once. Status-line is a tmux hot
// path — redrawn every status-interval tick — so these run concurrently
// rather than serially, but capped to avoid a subprocess storm on towns
// with many rigs.
const maxConcurrentWorkingChecks = 8

var statusLineCmd = &cobra.Command{
	Use:   "status-line",
	Short: "Output status line content for tmux (internal use)",
	Long: `Output formatted status line content for the tmux status bar.

Called internally by the tmux status-right configuration. Displays
the current rig, role, worker name, and active issue. Pass --session
to specify which tmux session to query.`,
	Hidden: true, // Internal command called by tmux
	RunE:   runStatusLine,
}

func init() {
	rootCmd.AddCommand(statusLineCmd)
	statusLineCmd.Flags().String("session", "", "Tmux session name")
}

func runStatusLine(cmd *cobra.Command, _ []string) error {
	sessionName := commandStringFlag(cmd, "session")
	printStatusLineEstop()

	t := tmux.NewTmux()
	env := readStatusLineEnvironment(t, sessionName)
	return dispatchStatusLine(t, sessionName, env)
}

type statusLineEnvironment struct {
	rigName string
	polecat string
	crew    string
	issue   string
	role    string
}

func printStatusLineEstop() {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return
	}

	showEstop := false
	var info *estop.Info
	if estop.IsActive(townRoot) {
		showEstop = true
		info = estop.Read(townRoot)
	} else {
		rigEnv := os.Getenv("GT_RIG")
		if rigEnv != "" && estop.IsRigActive(townRoot, rigEnv) {
			showEstop = true
			info = estop.ReadRig(townRoot, rigEnv)
		}
	}
	if !showEstop {
		return
	}

	ts := ""
	if info != nil && !info.Timestamp.IsZero() {
		ts = info.Timestamp.Format("15:04")
	}
	fmt.Printf("#[bg=red,fg=white,bold] ESTOP %s #[default] ", ts)
}

func readStatusLineEnvironment(t *tmux.Tmux, sessionName string) statusLineEnvironment {
	if sessionName != "" {
		// Fetch the session environment in one tmux call. Missing variables are
		// intentionally left empty and handled gracefully below.
		env, _ := t.GetAllEnvironment(sessionName)
		return statusLineEnvironment{
			rigName: env["GT_RIG"],
			polecat: env["GT_POLECAT"],
			crew:    env["GT_CREW"],
			issue:   env["GT_ISSUE"],
			role:    env["GT_ROLE"],
		}
	}
	return statusLineEnvironment{
		rigName: os.Getenv("GT_RIG"),
		polecat: os.Getenv("GT_POLECAT"),
		crew:    os.Getenv("GT_CREW"),
		issue:   os.Getenv("GT_ISSUE"),
		role:    os.Getenv("GT_ROLE"),
	}
}

func dispatchStatusLine(t *tmux.Tmux, sessionName string, env statusLineEnvironment) error {
	if env.role == "mayor" || sessionName == getMayorSessionName() {
		return runMayorStatusLine(t)
	}

	if env.role == "deacon" || sessionName == getDeaconSessionName() {
		return runDeaconStatusLine(t)
	}

	if env.role == "witness" || strings.HasSuffix(sessionName, "-witness") {
		return runWitnessStatusLine(t, env.rigName, sessionName)
	}

	if env.role == "refinery" || strings.HasSuffix(sessionName, "-refinery") {
		return runRefineryStatusLine(env.rigName, sessionName)
	}

	return runWorkerStatusLine(env.polecat, env.crew, env.issue)
}

// runWorkerStatusLine outputs status for crew or polecat sessions.
func runWorkerStatusLine(polecat, crew, issue string) error {
	// Determine agent type and identity
	var icon string
	if polecat != "" {
		icon = AgentTypeIcons[AgentPolecat]
	} else if crew != "" {
		icon = AgentTypeIcons[AgentCrew]
	}

	// Build status parts
	var parts []string
	currentWork := issue
	if currentWork != "" {
		if icon != "" {
			parts = append(parts, fmt.Sprintf("%s %s", icon, currentWork))
		} else {
			parts = append(parts, currentWork)
		}
	} else if icon != "" {
		parts = append(parts, icon)
	}

	// Output
	if len(parts) > 0 {
		fmt.Print(strings.Join(parts, " | ") + " |")
	}

	return nil
}

type mayorRigStatus struct {
	hasWitness  bool
	hasRefinery bool
	opState     string // "OPERATIONAL", "PARKED", or "DOCKED"
}

type mayorAgentHealth struct {
	total   int
	working int
}

type mayorPendingCheck struct {
	session string
	health  *mayorAgentHealth
}

type mayorRigInfo struct {
	name   string
	status *mayorRigStatus
}

func runMayorStatusLine(t *tmux.Tmux) error {
	sessions, err := t.ListSessions()
	if err != nil {
		return nil // Silent fail
	}

	mayorSession := getMayorSessionName()
	townRoot := statusLineTownRoot(t, mayorSession)
	registeredRigs := registeredStatusLineRigs(townRoot)
	collected := collectMayorStatus(sessions, registeredRigs)
	checkMayorWorkingSessions(t, collected.pending)
	for _, status := range collected.rigStatuses {
		// Status-line is a tmux hot path. Do not query beads for dock/park state here;
		// `gt rig list/status` remains the authoritative live status view.
		status.opState = "OPERATIONAL"
	}

	parts := buildMayorAgentParts(collected.healthByType, collected.hasDeacon)
	rigs := buildMayorRigInfos(collected.rigStatuses)
	sortMayorRigInfos(rigs)
	rigParts := renderMayorRigParts(rigs, townRoot)
	if len(rigParts) > 0 {
		parts = append(parts, strings.Join(rigParts, " "))
	}

	fmt.Print(strings.Join(parts, " | ") + " |")
	return nil
}

type collectedMayorStatus struct {
	rigStatuses  map[string]*mayorRigStatus
	healthByType map[AgentType]*mayorAgentHealth
	hasDeacon    bool
	pending      []mayorPendingCheck
}

func collectMayorStatus(sessions []string, registeredRigs map[string]bool) collectedMayorStatus {
	collected := collectedMayorStatus{
		rigStatuses: make(map[string]*mayorRigStatus, len(registeredRigs)),
		healthByType: map[AgentType]*mayorAgentHealth{
			AgentWitness:  {},
			AgentRefinery: {},
		},
	}
	for rigName := range registeredRigs {
		collected.rigStatuses[rigName] = &mayorRigStatus{}
	}

	for _, s := range sessions {
		agent := categorizeSession(s)
		if agent == nil {
			continue
		}
		if recordMayorSession(s, agent, registeredRigs, collected.rigStatuses, collected.healthByType, &collected.pending) {
			collected.hasDeacon = true
		}
	}
	return collected
}

func recordMayorSession(s string, agent *AgentSession, registeredRigs map[string]bool, rigStatuses map[string]*mayorRigStatus, healthByType map[AgentType]*mayorAgentHealth, pending *[]mayorPendingCheck) bool {
	if agent.Rig != "" && registeredRigs[agent.Rig] {
		status := rigStatuses[agent.Rig]
		if status == nil {
			status = &mayorRigStatus{}
			rigStatuses[agent.Rig] = status
		}
		setMayorRigAgentStatus(status, agent.Type)
	}
	if health := healthByType[agent.Type]; health != nil {
		health.total++
		*pending = append(*pending, mayorPendingCheck{session: s, health: health})
	}
	return agent.Type == AgentDeacon
}

func setMayorRigAgentStatus(status *mayorRigStatus, agentType AgentType) {
	switch agentType {
	case AgentWitness:
		status.hasWitness = true
	case AgentRefinery:
		status.hasRefinery = true
	}
}

func checkMayorWorkingSessions(t *tmux.Tmux, pending []mayorPendingCheck) {
	sem := make(chan struct{}, maxConcurrentWorkingChecks)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, c := range pending {
		wg.Add(1)
		sem <- struct{}{}
		go func(c mayorPendingCheck) {
			defer wg.Done()
			defer func() { <-sem }()
			if isSessionWorking(t, c.session) {
				mu.Lock()
				c.health.working++
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
}

func buildMayorAgentParts(healthByType map[AgentType]*mayorAgentHealth, hasDeacon bool) []string {
	var agentParts []string
	for _, agentType := range []AgentType{AgentWitness, AgentRefinery} {
		health := healthByType[agentType]
		if health.total == 0 {
			continue
		}
		icon := AgentTypeIcons[agentType]
		agentParts = append(agentParts, fmt.Sprintf("%d/%d %s", health.working, health.total, icon))
	}
	var parts []string
	if len(agentParts) > 0 {
		parts = append(parts, strings.Join(agentParts, " "))
	}
	if hasDeacon {
		parts = append(parts, AgentTypeIcons[AgentDeacon])
	}
	return parts
}

func buildMayorRigInfos(statuses map[string]*mayorRigStatus) []mayorRigInfo {
	rigs := make([]mayorRigInfo, 0, len(statuses))
	for rigName, status := range statuses {
		if status.opState == "DOCKED" {
			continue
		}
		rigs = append(rigs, mayorRigInfo{name: rigName, status: status})
	}
	return rigs
}

func sortMayorRigInfos(rigs []mayorRigInfo) {
	sort.Slice(rigs, func(i, j int) bool {
		isRunningI := rigs[i].status.hasWitness || rigs[i].status.hasRefinery
		isRunningJ := rigs[j].status.hasWitness || rigs[j].status.hasRefinery
		if isRunningI != isRunningJ {
			return isRunningI
		}
		stateOrder := map[string]int{"OPERATIONAL": 0, "PARKED": 1, "DOCKED": 2}
		stateI := stateOrder[rigs[i].status.opState]
		stateJ := stateOrder[rigs[j].status.opState]
		if stateI != stateJ {
			return stateI < stateJ
		}
		return rigs[i].name < rigs[j].name
	})
}

func renderMayorRigParts(rigs []mayorRigInfo, townRoot string) []string {
	var parts []string
	lastGroup := ""
	for _, rig := range rigs {
		currentGroup, part := mayorRigPart(rig, townRoot, len(rigs))
		if lastGroup != "" && lastGroup != currentGroup {
			parts = append(parts, "|")
		}
		lastGroup = currentGroup
		parts = append(parts, part)
	}
	return parts
}

func mayorRigPart(rig mayorRigInfo, townRoot string, rigCount int) (string, string) {
	currentGroup := "idle-" + rig.status.opState
	if rig.status.hasWitness || rig.status.hasRefinery {
		currentGroup = "running"
	}
	led := GetRigLED(rig.status.hasWitness, rig.status.hasRefinery, rig.status.opState)
	space := " "
	if led == "🅿️" {
		space = "  "
	}
	displayName := rig.name
	if rigCount > 2 && townRoot != "" {
		if prefix := config.GetRigPrefix(townRoot, rig.name); prefix != "" {
			displayName = prefix
		}
	}
	return currentGroup, led + space + displayName
}

// runDeaconStatusLine outputs status for the deacon session.
// Shows: active rigs, polecat count, hook or mail preview
func runDeaconStatusLine(t *tmux.Tmux) error {
	// Count active rigs and polecats
	sessions, err := t.ListSessions()
	if err != nil {
		return nil // Silent fail
	}

	// Get town root from deacon pane's working directory. Config files only; no beads.
	deaconSession := getDeaconSessionName()
	townRoot := statusLineTownRoot(t, deaconSession)
	registeredRigs := registeredStatusLineRigs(townRoot)
	rigCount := countRegisteredDeaconRigs(sessions, registeredRigs)

	// Build status
	// Note: Polecats excluded - their sessions are ephemeral and idle detection is a GC concern
	var parts []string
	parts = append(parts, fmt.Sprintf("%d rigs", rigCount))

	fmt.Print(strings.Join(parts, " | ") + " |")
	return nil
}

func statusLineTownRoot(t *tmux.Tmux, sessionName string) string {
	paneDir, err := t.GetPaneWorkDir(sessionName)
	if err != nil || paneDir == "" {
		return ""
	}
	townRoot, _ := workspace.Find(paneDir)
	return townRoot
}

func registeredStatusLineRigs(townRoot string) map[string]bool {
	registered := make(map[string]bool)
	if townRoot == "" {
		return registered
	}
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return registered
	}
	for rigName := range rigsConfig.Rigs {
		registered[rigName] = true
	}
	return registered
}

func countRegisteredDeaconRigs(sessions []string, registered map[string]bool) int {
	rigs := make(map[string]bool)
	for _, s := range sessions {
		agent := categorizeSession(s)
		if agent == nil {
			continue
		}
		// Only count registered rigs
		if agent.Rig != "" && registered[agent.Rig] {
			rigs[agent.Rig] = true
		}
	}
	return len(rigs)
}

// runWitnessStatusLine outputs status for a witness session.
// Shows: crew count, hook or mail preview
// Note: Polecats excluded - their sessions are ephemeral and idle detection is a GC concern
func runWitnessStatusLine(t *tmux.Tmux, rigName, sessionName string) error {
	rigName = witnessRigName(rigName, sessionName)

	// Count crew in this rig (crew are persistent, worth tracking)
	sessions, err := t.ListSessions()
	if err != nil {
		return nil // Silent fail
	}

	crewCount := countCrewSessions(sessions, rigName)

	// Build status
	var parts []string
	if crewCount > 0 {
		parts = append(parts, fmt.Sprintf("%d crew", crewCount))
	}
	if len(parts) == 0 {
		parts = append(parts, "patrol")
	}

	fmt.Print(strings.Join(parts, " | ") + " |")
	return nil
}

func witnessRigName(rigName, sessionName string) string {
	if rigName != "" {
		return rigName
	}
	identity, err := session.ParseSessionName(sessionName)
	if err == nil && identity.Role == session.RoleWitness {
		return identity.Rig
	}
	return ""
}

func countCrewSessions(sessions []string, rigName string) int {
	count := 0
	for _, s := range sessions {
		agent := categorizeSession(s)
		if agent != nil && agent.Rig == rigName && agent.Type == AgentCrew {
			count++
		}
	}
	return count
}

// runRefineryStatusLine outputs status for a refinery session.
// Shows: MQ length, current item, hook or mail preview
func runRefineryStatusLine(rigName, sessionName string) error {
	if rigName == "" {
		// Try to extract from session name: <prefix>-refinery
		if identity, err := session.ParseSessionName(sessionName); err == nil && identity.Role == session.RoleRefinery {
			rigName = identity.Rig
		}
	}

	if rigName == "" {
		fmt.Printf("%s ? |", AgentTypeIcons[AgentRefinery])
		return nil
	}

	fmt.Print("idle |")
	return nil
}

// isSessionWorking detects if a Claude Code session is actively working.
// Returns true if the ✻ symbol is visible in the pane (indicates Claude is processing).
// Returns false for idle sessions (showing ❯ prompt) or if state cannot be determined.
func isSessionWorking(t *tmux.Tmux, session string) bool {
	// Capture last few lines of the pane
	lines, err := t.CapturePaneLines(session, 5)
	if err != nil || len(lines) == 0 {
		return false
	}

	// Check all captured lines for the working indicator
	// ✻ appears in Claude's status line when actively processing
	for _, line := range lines {
		if strings.Contains(line, "✻") {
			return true
		}
	}

	return false
}
