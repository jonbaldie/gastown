package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/crew"
	"github.com/jonbaldie/gastown/internal/daemon"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/mayor"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var statusCmd = &cobra.Command{
	Use:         "status",
	Aliases:     []string{"stat"},
	GroupID:     GroupDiag,
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Short:       "Show overall town status",
	Long: `Display the current status of the Gas Town workspace.

Shows town name, registered rigs, polecats, and witness status.

Use --fast to skip mail lookups for faster execution.
Use --watch to continuously refresh status at regular intervals.`,
	RunE: runStatus,
}

func init() {
	statusCmd.Flags().Bool("json", false, "Output as JSON")
	statusCmd.Flags().Bool("fast", false, "Skip mail lookups for faster execution")
	statusCmd.Flags().BoolP("watch", "w", false, "Watch mode: refresh status continuously")
	statusCmd.Flags().IntP("interval", "n", 2, "Refresh interval in seconds")
	statusCmd.Flags().BoolP("verbose", "v", false, "Show detailed multi-line output per agent")
	rootCmd.AddCommand(statusCmd)
}

// TownStatus represents the overall status of the workspace.
type TownStatus struct {
	Name     string         `json:"name"`
	Location string         `json:"location"`
	Overseer *OverseerInfo  `json:"overseer,omitempty"` // Human operator
	DND      *DNDInfo       `json:"dnd,omitempty"`      // Current agent DND status
	Daemon   *ServiceInfo   `json:"daemon,omitempty"`   // Daemon status
	Dolt     *DoltInfo      `json:"dolt,omitempty"`     // Dolt server status
	Tmux     *TmuxInfo      `json:"tmux,omitempty"`     // Tmux server status
	ACP      *ServiceInfo   `json:"acp,omitempty"`      // ACP mayor status
	Agents   []AgentRuntime `json:"agents"`             // Global agents (Mayor, Deacon)
	Rigs     []RigStatus    `json:"rigs"`
	Summary  StatusSum      `json:"summary"`
}

// ServiceInfo represents a background service status.
type ServiceInfo struct {
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
}

// DoltInfo represents the Dolt server status.
type DoltInfo struct {
	Running       bool   `json:"running"`
	PID           int    `json:"pid,omitempty"`
	Port          int    `json:"port"`
	Remote        bool   `json:"remote,omitempty"`
	DataDir       string `json:"data_dir,omitempty"`
	PortConflict  bool   `json:"port_conflict,omitempty"`  // Port taken by another town's Dolt
	ConflictOwner string `json:"conflict_owner,omitempty"` // --data-dir of the process holding the port
}

// TmuxInfo represents the tmux server status.
type TmuxInfo struct {
	Socket       string `json:"socket"`                // Socket name derived from town name (e.g., "gt-test")
	SocketPath   string `json:"socket_path,omitempty"` // Full socket path (e.g., /tmp/tmux-501/gt-test)
	Running      bool   `json:"running"`               // Is the tmux server running?
	PID          int    `json:"pid,omitempty"`         // PID of the tmux server process
	SessionCount int    `json:"session_count"`         // Number of sessions
}

// OverseerInfo represents the human operator's identity and status.
type OverseerInfo struct {
	Name       string `json:"name"`
	Email      string `json:"email,omitempty"`
	Username   string `json:"username,omitempty"`
	Source     string `json:"source"`
	UnreadMail int    `json:"unread_mail"`
}

// DNDInfo represents Do Not Disturb status for the current agent context.
type DNDInfo struct {
	Enabled bool   `json:"enabled"`
	Level   string `json:"level"`
	Agent   string `json:"agent,omitempty"`
}

// AgentRuntime represents the runtime state of an agent.
type AgentRuntime struct {
	Name        string `json:"name"`                   // Display name (e.g., "mayor", "witness")
	Address     string `json:"address"`                // Full address (e.g., "greenplace/witness")
	Session     string `json:"session"`                // tmux session name
	Role        string `json:"role"`                   // Role type
	Running     bool   `json:"running"`                // Is tmux session running?
	Blocked     bool   `json:"blocked,omitempty"`      // Pane is fully blocked on an interactive dialog
	BlockReason string `json:"block_reason,omitempty"` // Why the pane is blocked (e.g. model picker)
	ACP         bool   `json:"acp"`                    // Is ACP session active?
	AgentWork
	NotificationLevel string `json:"notification_level,omitempty"` // Notification level (verbose, normal, muted)
	UnreadMail        int    `json:"unread_mail"`                  // Number of unread messages
	FirstSubject      string `json:"first_subject,omitempty"`      // Subject of first unread message
	AgentAlias        string `json:"agent_alias,omitempty"`        // Configured agent name (e.g., "opus-46", "pi")
	AgentInfo         string `json:"agent_info,omitempty"`         // Runtime summary (e.g., "claude/opus", "pi/kimi-k2p5")
}

// AgentWork contains the pinned-work state associated with an agent. It is
// embedded so status JSON keeps these fields at the same top level.
type AgentWork struct {
	HasWork   bool   `json:"has_work"`             // Has pinned work?
	WorkTitle string `json:"work_title,omitempty"` // Title of pinned work
	HookBead  string `json:"hook_bead,omitempty"`  // Pinned bead ID from agent bead
	State     string `json:"state,omitempty"`      // Agent state from agent bead
}

// RigStatus represents status of a single rig.
type RigStatus struct {
	Name         string          `json:"name"`
	Polecats     []string        `json:"polecats"`
	PolecatCount int             `json:"polecat_count"`
	Crews        []string        `json:"crews"`
	CrewCount    int             `json:"crew_count"`
	HasWitness   bool            `json:"has_witness"`
	HasRefinery  bool            `json:"has_refinery"`
	Hooks        []AgentHookInfo `json:"hooks,omitempty"`
	Agents       []AgentRuntime  `json:"agents,omitempty"` // Runtime state of all agents in rig
	MQ           *MQSummary      `json:"mq,omitempty"`     // Merge queue summary
}

// MQSummary represents the merge queue status for a rig.
type MQSummary struct {
	Pending  int    `json:"pending"`   // Open MRs ready to merge (no blockers)
	InFlight int    `json:"in_flight"` // MRs currently being processed
	Blocked  int    `json:"blocked"`   // MRs waiting on dependencies
	State    string `json:"state"`     // idle, processing, or blocked
	Health   string `json:"health"`    // healthy, stale, or empty
}

// AgentHookInfo represents an agent's hook (pinned work) status.
type AgentHookInfo struct {
	Agent    string `json:"agent"`              // Agent address (e.g., "greenplace/toast", "greenplace/witness")
	Role     string `json:"role"`               // Role type (polecat, crew, witness, refinery)
	HasWork  bool   `json:"has_work"`           // Whether agent has pinned work
	Molecule string `json:"molecule,omitempty"` // Attached molecule ID
	Title    string `json:"title,omitempty"`    // Pinned bead title
}

// StatusSum provides summary counts.
type StatusSum struct {
	RigCount      int `json:"rig_count"`
	PolecatCount  int `json:"polecat_count"`
	CrewCount     int `json:"crew_count"`
	WitnessCount  int `json:"witness_count"`
	RefineryCount int `json:"refinery_count"`
	ActiveHooks   int `json:"active_hooks"`
}

// resolveAgentDisplay inspects the actual running process in the tmux session
// to determine what runtime and model are being used. Falls back to config
// when the session isn't running.
func resolveAgentDisplay(townRoot string, townSettings *config.TownSettings, role string, sessionName string, running bool) (alias, info string) {
	configRole := statusConfigRole(role)
	alias = configuredStatusAgent(townSettings, configRole, sessionName)
	alias = statusACPAgent(townRoot, configRole, alias)

	configured := configuredAgentInfo(townSettings, alias)
	var detected string
	if running && sessionName != "" {
		detected = detectRuntimeFromSessionFn(sessionName)
	}
	return alias, chooseAgentInfo(detected, configured)
}

func statusConfigRole(role string) string {
	switch role {
	case "coordinator":
		return constants.RoleMayor
	case "health-check":
		return constants.RoleDeacon
	default:
		return role
	}
}

func configuredStatusAgent(townSettings *config.TownSettings, configRole, sessionName string) string {
	if townSettings == nil {
		return ""
	}
	if configRole == constants.RoleCrew && sessionName != "" {
		if identity, err := session.ParseSessionName(sessionName); err == nil && identity.Role == session.RoleCrew {
			if alias := townSettings.CrewAgents[identity.Name]; alias != "" {
				return alias
			}
		}
	}
	if alias := townSettings.RoleAgents[configRole]; alias != "" {
		return alias
	}
	return townSettings.DefaultAgent
}

func statusACPAgent(townRoot, configRole, alias string) string {
	if configRole != constants.RoleMayor || !mayor.IsACPActive(townRoot) {
		return alias
	}
	if acpAgent, err := mayor.GetACPAgent(townRoot); err == nil && acpAgent != "" {
		return acpAgent
	}
	return alias
}

// configuredAgentInfo is the model string implied by the role's alias config.
func configuredAgentInfo(townSettings *config.TownSettings, alias string) string {
	if townSettings == nil || alias == "" {
		return ""
	}
	if rc := townSettings.Agents[alias]; rc != nil {
		return buildInfoFromConfig(rc)
	}
	return alias
}

// chooseAgentInfo picks gt status agent_info.
// Prefer a live process model when it was actually present on the cmdline.
// Prefer the configured alias model when the live model cannot be read, or
// when detection only recovered an unrelated ~/.pi default.
func chooseAgentInfo(detected, configured string) string {
	if preferConfiguredAgentInfo(detected, configured) {
		return configured
	}
	if detected != "" {
		return detected
	}
	return configured
}

func preferConfiguredAgentInfo(detected, configured string) bool {
	if configured == "" {
		return false
	}
	if detected == "" {
		return true
	}
	if detected == configured {
		return false
	}
	detCmd, detModel, detHasModel := splitRuntimeInfo(detected)
	cfgCmd, _, cfgHasModel := splitRuntimeInfo(configured)
	return detectedStatusNeedsConfigured(detCmd, detModel, detHasModel, cfgCmd, cfgHasModel)
}

func detectedStatusNeedsConfigured(detCmd, detModel string, detHasModel bool, cfgCmd string, cfgHasModel bool) bool {
	// Runtime-only detection ("pi") is not a live model read.
	if !detHasModel && cfgHasModel {
		return true
	}
	// A leftover ~/.pi default is not the live session model.
	if !detHasModel || !cfgHasModel || detCmd != cfgCmd {
		return false
	}
	piDefault := readPiDefaultsFn()
	return piDefault != "" && detModel == piDefault
}

func splitRuntimeInfo(info string) (cmd, model string, hasModel bool) {
	cmd, model, ok := strings.Cut(info, "/")
	if !ok || model == "" {
		return info, "", false
	}
	return cmd, model, true
}

// detectRuntimeFromSessionFn is the session inspector used by resolveAgentDisplay.
// Tests replace it to inject a detected runtime without spawning tmux.
var detectRuntimeFromSessionFn = detectRuntimeFromSession

// detectRuntimeFromSession inspects the actual process tree in a tmux session
// to determine what agent runtime and model are in use.
func detectRuntimeFromSession(sessionName string) string {
	// Get the PID of the shell process in the tmux pane
	t := tmux.NewTmux()
	pid, err := t.GetPanePID(sessionName)
	if err != nil || pid == "" {
		return ""
	}

	// Walk child processes to find the actual agent (not the shell)
	cmdline := findAgentCmdline(pid)
	if cmdline == "" {
		return ""
	}

	return parseRuntimeInfo(cmdline)
}

// findAgentCmdline checks the pane process itself and its descendants for a known agent.
// The pane PID may BE the agent (e.g., claude), or the agent may be a child (e.g., shell → pi).
// Also handles wrapper processes (node /path/to/pi, bun /path/to/opencode).
func findAgentCmdline(panePid string) string {
	// Check the pane process itself first
	cmdline := readCmdline(panePid)
	if isAgentCmdline(cmdline) {
		return cmdline
	}

	// Walk children (shell → agent).
	for _, childPid := range childPIDs(panePid) {
		childCmd := readCmdline(childPid)
		if isAgentCmdline(childCmd) {
			return childCmd
		}
		// Check grandchildren (cgroup-wrap → agent).
		for _, gcPid := range childPIDs(childPid) {
			gcCmd := readCmdline(gcPid)
			if isAgentCmdline(gcCmd) {
				return gcCmd
			}
		}
	}

	return cmdline // return pane process cmdline as fallback
}

// isAgentCmdline returns true if the cmdline contains a known agent,
// either as the main command or as the first arg of a wrapper (node/bun).
func isAgentCmdline(cmdline string) bool {
	if cmdline == "" {
		return false
	}
	parts := strings.Split(cmdline, "\x00")
	if len(parts) == 0 {
		return false
	}
	base := filepath.Base(parts[0])
	if isKnownAgent(base) {
		return true
	}
	// Check if wrapper (node/bun) is running an agent
	if isAgentWrapper(base) && len(parts) > 1 {
		argBase := filepath.Base(parts[1])
		return isKnownAgent(argBase)
	}
	return false
}

// readCmdline reads a process command line. Linux exposes null-separated
// arguments through /proc; platforms without /proc (notably macOS) fall back
// to ps and normalize its output to the same representation.
func readCmdline(pid string) string {
	data, err := os.ReadFile("/proc/" + pid + "/cmdline")
	if err == nil && len(data) > 0 {
		return string(data)
	}

	out, err := exec.Command("ps", "-p", pid, "-o", "command=").Output() //nolint:gosec // PID is passed as an argument, not interpreted by a shell.
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(out)), "\x00")
}

// childPIDs reads Linux's process child list first and uses pgrep elsewhere.
func childPIDs(pid string) []string {
	childrenPath := "/proc/" + pid + "/task/" + pid + "/children"
	if data, err := os.ReadFile(childrenPath); err == nil {
		return strings.Fields(string(data))
	}

	out, err := exec.Command("pgrep", "-P", pid).Output() //nolint:gosec // PID is passed as an argument, not interpreted by a shell.
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// extractBaseName gets the base command name from a null-separated cmdline.
func extractBaseName(cmdline string) string {
	if cmdline == "" {
		return ""
	}
	parts := strings.Split(cmdline, "\x00")
	if len(parts) == 0 {
		return ""
	}
	return filepath.Base(parts[0])
}

// isKnownAgent returns true if the command is a recognized agent runtime.
func isKnownAgent(base string) bool {
	return config.IsKnownPreset(base)
}

// isAgentWrapper returns true if the command is a runtime wrapper (node, bun, etc.)
// that may host an agent as its first argument.
func isAgentWrapper(base string) bool {
	switch base {
	case "node", "bun", "npx", "bunx":
		return true
	}
	return false
}

// parseRuntimeInfo extracts "runtime/model" from a null-separated cmdline.
// Handles direct invocation (claude --model opus) and wrapper patterns (node /path/to/pi).
func parseRuntimeInfo(cmdline string) string {
	if cmdline == "" {
		return ""
	}
	parts := strings.Split(cmdline, "\x00")
	if len(parts) == 0 {
		return ""
	}

	cmd, startIdx := findRuntimeCommand(parts)
	model, provider := parseRuntimeFlags(parts, startIdx)

	if model != "" {
		return cmd + "/" + model
	}
	if provider != "" {
		return cmd + "/" + provider
	}

	// Do not invent ~/.pi defaultProvider/defaultModel here. That home-wide
	// leftover is not the live session model and hid configured alias --model
	// values from gt status (UAT: pi-cheap reported as pi/grok-4.6).
	return cmd
}

func findRuntimeCommand(parts []string) (string, int) {
	for i, part := range parts {
		base := filepath.Base(part)
		if isKnownAgent(base) {
			return base, i
		}
	}
	return filepath.Base(parts[0]), 0
}

func parseRuntimeFlags(parts []string, startIdx int) (model, provider string) {
	partCount := len(parts)
	for i := startIdx; i < partCount; i++ {
		arg := parts[i]
		if i+1 >= partCount || parts[i+1] == "" {
			continue
		}
		switch arg {
		case "--model", "-m":
			model = parts[i+1]
		case "--provider":
			provider = parts[i+1]
		}
	}
	return model, provider
}

// readPiDefaultsFn is the ~/.pi default-model lookup used when deciding
// whether a detected model is just the leftover home default.
var readPiDefaultsFn = readPiDefaults

// readPiDefaults reads ~/.pi/agent/settings.json to get the actual default provider/model.
func readPiDefaults() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "settings.json"))
	if err != nil {
		return ""
	}
	var settings struct {
		DefaultProvider string `json:"defaultProvider"`
		DefaultModel    string `json:"defaultModel"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return ""
	}
	if settings.DefaultModel != "" {
		return settings.DefaultModel
	}
	if settings.DefaultProvider != "" {
		return settings.DefaultProvider
	}
	return ""
}

// buildInfoFromConfig builds display info from a RuntimeConfig (fallback when not running).
func buildInfoFromConfig(rc *config.RuntimeConfig) string {
	if rc.Command == "" {
		return "claude"
	}
	cmd := filepath.Base(rc.Command)
	if cmd == "" {
		cmd = "claude"
	}
	if cmd == "cgroup-wrap" && len(rc.Args) > 0 {
		cmd = rc.Args[0]
	}

	model := configuredModel(rc.Args)
	if model != "" {
		return cmd + "/" + model
	}
	return cmd
}

func configuredModel(args []string) string {
	for i, arg := range args {
		if (arg == "--model" || arg == "-m") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

type statusOptions struct {
	json     bool
	fast     bool
	watch    bool
	interval int
	verbose  bool
}

func runStatus(cmd *cobra.Command, _ []string) error {
	opts := statusOptions{
		json:     commandBoolFlag(cmd, "json"),
		fast:     commandBoolFlag(cmd, "fast"),
		watch:    commandBoolFlag(cmd, "watch"),
		interval: commandIntFlag(cmd, "interval"),
		verbose:  commandBoolFlag(cmd, "verbose"),
	}
	if opts.watch {
		return runStatusWatch(opts)
	}
	return runStatusOnce(opts)
}

func runStatusWatch(opts statusOptions) error {
	if opts.json {
		return fmt.Errorf("--json and --watch cannot be used together")
	}
	if opts.interval <= 0 {
		return fmt.Errorf("interval must be positive, got %d", opts.interval)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	ticker := time.NewTicker(time.Duration(opts.interval) * time.Second)
	defer ticker.Stop()

	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	watch := statusWatchState{maxStale: time.Duration(opts.interval) * time.Second * 5}

	for {
		writeStatusWatchFrame(opts, isTTY, &watch)

		select {
		case <-sigChan:
			if isTTY {
				fmt.Println("\nStopped.")
			}
			return nil
		case <-ticker.C:
		}
	}
}

type statusWatchState struct {
	cachedStatus *TownStatus
	cachedAt     time.Time
	maxStale     time.Duration
}

func writeStatusWatchFrame(opts statusOptions, isTTY bool, watch *statusWatchState) {
	var buf bytes.Buffer
	writeStatusWatchHeader(&buf, opts, isTTY)
	status, err, usedCache := watch.refresh(opts)
	if err != nil {
		fmt.Fprintf(&buf, "Error: %v\n", err)
	} else {
		if !usedCache {
			watch.remember(status)
		}
		writeStatusWatchCacheNote(&buf, watch, isTTY, usedCache)
		if err := outputStatusText(&buf, status, opts.verbose); err != nil {
			fmt.Fprintf(&buf, "Error: %v\n", err)
		}
	}
	_, _ = os.Stdout.Write(buf.Bytes())
}

func writeStatusWatchHeader(w io.Writer, opts statusOptions, isTTY bool) {
	if isTTY {
		_, _ = fmt.Fprint(w, "\033[H\033[2J")
	}
	header := fmt.Sprintf("[%s] gt status --watch (every %ds, Ctrl+C to stop)", time.Now().Format("15:04:05"), opts.interval)
	if isTTY {
		header = style.Dim.Render(header)
	}
	fmt.Fprintf(w, "%s\n\n", header)
}

func writeStatusWatchCacheNote(w io.Writer, watch *statusWatchState, isTTY, usedCache bool) {
	if !usedCache {
		return
	}
	note := fmt.Sprintf("(using cached data from %s)", watch.cachedAt.Format("15:04:05"))
	if isTTY {
		note = style.Dim.Render(note)
	}
	fmt.Fprintf(w, "%s\n", note)
}

func (s *statusWatchState) refresh(opts statusOptions) (TownStatus, error, bool) {
	status, err := gatherStatus(opts)
	if err != nil {
		status, err = gatherStatus(opts)
	}
	if err != nil {
		if s.cacheFresh() {
			return *s.cachedStatus, nil, true
		}
		return status, err, false
	}
	if countRunningAgents(status) == 0 && s.hasRunningCache() {
		retry, retryErr := gatherStatus(opts)
		if retryErr == nil && countRunningAgents(retry) > 0 {
			return retry, nil, false
		}
		if s.cacheFresh() {
			return *s.cachedStatus, nil, true
		}
	}
	return status, nil, false
}

func (s *statusWatchState) hasRunningCache() bool {
	return s.cachedStatus != nil && countRunningAgents(*s.cachedStatus) > 0
}

func (s *statusWatchState) cacheFresh() bool {
	return s.cachedStatus != nil && time.Since(s.cachedAt) < s.maxStale
}

func (s *statusWatchState) remember(status TownStatus) {
	statusCopy := status
	s.cachedStatus = &statusCopy
	s.cachedAt = time.Now()
}

// countRunningAgents returns the number of agents with Running=true
// across all global agents and rig agents in the status.
func countRunningAgents(s TownStatus) int {
	count := 0
	for _, a := range s.Agents {
		if a.Running {
			count++
		}
	}
	for _, r := range s.Rigs {
		for _, a := range r.Agents {
			if a.Running {
				count++
			}
		}
	}
	return count
}

func runStatusOnce(opts statusOptions) error {
	status, err := gatherStatus(opts)
	if err != nil {
		return err
	}
	if opts.json {
		return outputStatusJSON(status)
	}
	return outputStatusText(os.Stdout, status, opts.verbose)
}

func gatherStatus(opts statusOptions) (TownStatus, error) {
	townRoot, fast, skipBeadsPrefetch, release, err := prepareStatusWorkspace(opts)
	if err != nil {
		return TownStatus{}, err
	}
	if release != nil {
		defer release()
	}

	townConfig, rigsConfig, townSettings := loadStatusConfiguration(townRoot)

	// Create rig manager
	g := git.NewGit(townRoot)
	mgr := rig.NewManager(townRoot, rigsConfig, g)

	// Create tmux instance for runtime checks
	t := tmux.NewTmux()

	allSessions := loadStatusSessions(t)

	// Discover rigs
	rigs, err := mgr.DiscoverRigs()
	if err != nil {
		return TownStatus{}, fmt.Errorf("discovering rigs: %w", err)
	}

	allAgentBeads, allHookBeads := prefetchStatusBeads(townRoot, rigs, skipBeadsPrefetch)

	// Create mail router for inbox lookups
	mailRouter := mail.NewRouter(townRoot)

	overseerInfo := loadStatusOverseer(townRoot, mailRouter, fast)

	// Build status - parallel fetch global agents and rigs
	status := TownStatus{
		Name:     townConfig.Name,
		Location: townRoot,
		Overseer: overseerInfo,
		DND:      detectCurrentDNDStatus(townRoot),
		Rigs:     make([]RigStatus, len(rigs)),
	}

	setStatusServices(&status, townRoot, allSessions)

	var rigActiveHooks []int
	status.Agents, status.Rigs, rigActiveHooks = discoverStatusAgentsAndRigs(
		townRoot, rigs, allSessions, allAgentBeads, allHookBeads, mailRouter, fast,
	)

	enrichStatusAgents(&status, townRoot, townSettings)
	aggregateStatusSummary(&status, rigActiveHooks, len(rigs))

	return status, nil
}

func prepareStatusWorkspace(opts statusOptions) (string, bool, bool, func(), error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", false, false, nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	fast := opts.fast
	skipBeadsPrefetch := false
	var release func()
	if !fast {
		if unlock, ok := tryStatusDetailLock(townRoot); ok {
			release = unlock
		} else {
			fast = true
			skipBeadsPrefetch = true
		}
	}
	return townRoot, fast, skipBeadsPrefetch, release, nil
}

func loadStatusConfiguration(townRoot string) (*config.TownConfig, *config.RigsConfig, *config.TownSettings) {
	townConfig, err := config.LoadTownConfig(constants.MayorTownPath(townRoot))
	if err != nil {
		townConfig = &config.TownConfig{Name: filepath.Base(townRoot)}
	}
	rigsConfig, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}
	townSettings, _ := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	return townConfig, rigsConfig, townSettings
}

func loadStatusSessions(t *tmux.Tmux) map[string]bool {
	allSessions := make(map[string]bool)
	sessions, err := t.ListSessions()
	if err != nil {
		return allSessions
	}
	var sessionMu sync.Mutex
	var sessionWg sync.WaitGroup
	for _, name := range sessions {
		if !session.IsKnownSession(name) {
			allSessions[name] = true
			continue
		}
		sessionWg.Add(1)
		go func(name string) {
			defer sessionWg.Done()
			alive := t.IsAgentAlive(name)
			sessionMu.Lock()
			allSessions[name] = alive
			sessionMu.Unlock()
		}(name)
	}
	sessionWg.Wait()
	return allSessions
}

func prefetchStatusBeads(townRoot string, rigs []*rig.Rig, skip bool) (map[string]*beads.Issue, map[string]*beads.Issue) {
	allAgentBeads := make(map[string]*beads.Issue)
	allHookBeads := make(map[string]*beads.Issue)
	if skip {
		return allAgentBeads, allHookBeads
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		prefetchTownStatusBeads(townRoot, allAgentBeads, allHookBeads, &mu)
	}()
	for _, r := range rigs {
		wg.Add(1)
		go func(r *rig.Rig) {
			defer wg.Done()
			prefetchRigStatusBeads(r, allAgentBeads, allHookBeads, &mu)
		}(r)
	}
	wg.Wait()
	return allAgentBeads, allHookBeads
}

func prefetchTownStatusBeads(townRoot string, allAgentBeads, allHookBeads map[string]*beads.Issue, mu *sync.Mutex) {
	client := beads.New(beads.GetTownBeadsPath(townRoot))
	agentBeads, _ := client.ListAgentBeads()
	mergeStatusBeads(mu, allAgentBeads, agentBeads)
	hookIDs := statusHookIDs(agentBeads)
	if len(hookIDs) == 0 {
		return
	}
	hookBeads, _ := client.ShowMultiple(hookIDs)
	mergeStatusBeads(mu, allHookBeads, hookBeads)
}

func prefetchRigStatusBeads(r *rig.Rig, allAgentBeads, allHookBeads map[string]*beads.Issue, mu *sync.Mutex) {
	client := beads.New(filepath.Join(r.Path, "mayor", "rig"))
	agentBeads, _ := client.ListAgentBeads()
	if agentBeads == nil {
		return
	}
	mergeStatusBeads(mu, allAgentBeads, agentBeads)
	hookIDs := statusHookIDs(agentBeads)
	if len(hookIDs) == 0 {
		return
	}
	hookBeads, _ := client.ShowMultiple(hookIDs)
	mergeStatusBeads(mu, allHookBeads, hookBeads)
}

func mergeStatusBeads(mu *sync.Mutex, target, source map[string]*beads.Issue) {
	mu.Lock()
	defer mu.Unlock()
	for id, issue := range source {
		target[id] = issue
	}
}

func statusHookIDs(agentBeads map[string]*beads.Issue) []string {
	var hookIDs []string
	for _, issue := range agentBeads {
		hookID := issue.HookBead
		if hookID == "" {
			if fields := beads.ParseAgentFields(issue.Description); fields != nil {
				hookID = fields.HookBead
			}
		}
		if hookID != "" {
			hookIDs = append(hookIDs, hookID)
		}
	}
	return hookIDs
}

func loadStatusOverseer(townRoot string, mailRouter *mail.Router, fast bool) *OverseerInfo {
	overseerConfig, err := config.LoadOrDetectOverseer(townRoot)
	if err != nil || overseerConfig == nil {
		return nil
	}
	overseer := &OverseerInfo{
		Name:     overseerConfig.Name,
		Email:    overseerConfig.Email,
		Username: overseerConfig.Username,
		Source:   overseerConfig.Source,
	}
	if !fast {
		if mailbox, err := mailRouter.GetMailbox("overseer"); err == nil {
			_, overseer.UnreadMail, _ = mailbox.Count()
		}
	}
	return overseer
}

func setStatusServices(status *TownStatus, townRoot string, allSessions map[string]bool) {
	status.Daemon = statusDaemonInfo(townRoot)
	status.Dolt = statusDoltInfo(townRoot)
	status.Tmux = statusTmuxInfo(allSessions)
	status.ACP = statusACPInfo(townRoot)
}

func statusDaemonInfo(townRoot string) *ServiceInfo {
	running, pid, err := daemon.IsRunning(townRoot)
	if err != nil {
		return nil
	}
	return &ServiceInfo{Running: running, PID: pid}
}

func statusDoltInfo(townRoot string) *DoltInfo {
	cfg := doltserver.DefaultConfig(townRoot)
	if cfg.IsRemote() {
		return &DoltInfo{Remote: true, Port: cfg.Port}
	}
	running, pid, _ := doltserver.IsRunning(townRoot)
	port := cfg.Port
	if running {
		if state, err := doltserver.LoadState(townRoot); err == nil && state.Port > 0 {
			port = state.Port
		}
	}
	info := &DoltInfo{Running: running, PID: pid, Port: port, DataDir: cfg.DataDir}
	if !running {
		if conflictPID, conflictDir := doltserver.CheckPortConflict(townRoot); conflictPID > 0 {
			info.PortConflict = true
			info.ConflictOwner = conflictDir
		}
	}
	return info
}

func statusTmuxInfo(allSessions map[string]bool) *TmuxInfo {
	socketLabel := tmux.GetDefaultSocket()
	if socketLabel == "" {
		socketLabel = "default"
	}
	info := &TmuxInfo{
		Socket:       socketLabel,
		SessionCount: len(allSessions),
		Running:      len(allSessions) > 0,
		SocketPath:   filepath.Join(tmux.SocketDir(), socketLabel),
	}
	if _, err := os.Stat(info.SocketPath); err == nil {
		info.Running = true
		info.PID = tmux.NewTmux().ServerPID()
	}
	return info
}

func statusACPInfo(townRoot string) *ServiceInfo {
	if !mayor.IsACPActive(townRoot) {
		return nil
	}
	pid, _ := mayor.GetACPPid(townRoot)
	return &ServiceInfo{Running: true, PID: pid}
}

func discoverStatusAgentsAndRigs(townRoot string, rigs []*rig.Rig, allSessions map[string]bool, allAgentBeads, allHookBeads map[string]*beads.Issue, mailRouter *mail.Router, fast bool) ([]AgentRuntime, []RigStatus, []int) {
	agents := make([]AgentRuntime, 0)
	rigStatuses := make([]RigStatus, len(rigs))
	rigActiveHooks := make([]int, len(rigs))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		agents = discoverGlobalAgents(townRoot, allSessions, allAgentBeads, allHookBeads, mailRouter, fast)
	}()
	for i, r := range rigs {
		wg.Add(1)
		go func(idx int, r *rig.Rig) {
			defer wg.Done()
			rigStatuses[idx], rigActiveHooks[idx] = discoverStatusRig(r, allSessions, allAgentBeads, allHookBeads, mailRouter, fast)
		}(i, r)
	}
	wg.Wait()
	return agents, rigStatuses, rigActiveHooks
}

func discoverStatusRig(r *rig.Rig, allSessions map[string]bool, allAgentBeads, allHookBeads map[string]*beads.Issue, mailRouter *mail.Router, fast bool) (RigStatus, int) {
	status := RigStatus{
		Name:         r.Name,
		Polecats:     r.Polecats,
		PolecatCount: len(r.Polecats),
		HasWitness:   r.HasWitness,
		HasRefinery:  r.HasRefinery,
	}
	if workers, err := crew.NewManager(r, git.NewGit(r.Path)).List(); err == nil {
		for _, worker := range workers {
			status.Crews = append(status.Crews, worker.Name)
		}
		status.CrewCount = len(workers)
	}
	var wg sync.WaitGroup
	if !fast {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status.Hooks = discoverRigHooks(r, status.Crews)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			status.MQ = getMQSummary(r)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		status.Agents = discoverRigAgents(allSessions, r, status.Crews, allAgentBeads, allHookBeads, mailRouter, fast)
	}()
	wg.Wait()
	return status, countActiveStatusHooks(status.Hooks)
}

func countActiveStatusHooks(hooks []AgentHookInfo) int {
	count := 0
	for _, hook := range hooks {
		if hook.HasWork {
			count++
		}
	}
	return count
}

func enrichStatusAgents(status *TownStatus, townRoot string, townSettings *config.TownSettings) {
	enrichStatusAgentList(status.Agents, townRoot, townSettings)
	for i := range status.Rigs {
		enrichStatusAgentList(status.Rigs[i].Agents, townRoot, townSettings)
	}
}

func enrichStatusAgentList(agents []AgentRuntime, townRoot string, townSettings *config.TownSettings) {
	for i := range agents {
		agent := &agents[i]
		agent.AgentAlias, agent.AgentInfo = resolveAgentDisplay(townRoot, townSettings, agent.Role, agent.Session, agent.Running)
	}
}

func aggregateStatusSummary(status *TownStatus, rigActiveHooks []int, rigCount int) {
	for i, rigStatus := range status.Rigs {
		status.Summary.PolecatCount += rigStatus.PolecatCount
		status.Summary.CrewCount += rigStatus.CrewCount
		status.Summary.ActiveHooks += rigActiveHooks[i]
		if rigStatus.HasWitness {
			status.Summary.WitnessCount++
		}
		if rigStatus.HasRefinery {
			status.Summary.RefineryCount++
		}
	}
	status.Summary.RigCount = rigCount
}

func outputStatusJSON(status TownStatus) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(status)
}

func outputStatusText(w io.Writer, status TownStatus, verbose bool) error {
	return outputStatusTextSections(w, status, verbose)
}

func outputStatusTextSections(w io.Writer, status TownStatus, verbose bool) error {
	writeStatusHeader(w, status)
	writeStatusOverseer(w, status.Overseer)
	writeStatusDND(w, status.DND)

	writeStatusServices(w, status)

	roleIcons := statusRoleIcons()
	writeStatusGlobalAgents(w, status, verbose, roleIcons)

	if len(status.Rigs) == 0 {
		fmt.Fprintf(w, "%s\n", style.Dim.Render("No rigs registered. Use 'gt rig add' to add one."))
		return nil
	}

	for _, r := range status.Rigs {
		writeStatusRig(w, r, status.Location, verbose, roleIcons)
	}

	return nil
}

func writeStatusHeader(w io.Writer, status TownStatus) {
	fmt.Fprintf(w, "%s %s\n", style.Bold.Render("Town:"), status.Name)
	fmt.Fprintf(w, "%s\n\n", style.Dim.Render(status.Location))
	addEstopToStatus(status.Location)
}

func writeStatusOverseer(w io.Writer, overseer *OverseerInfo) {
	if overseer == nil {
		return
	}
	display := overseer.Name
	if overseer.Email != "" {
		display = fmt.Sprintf("%s <%s>", overseer.Name, overseer.Email)
	} else if overseer.Username != "" && overseer.Username != overseer.Name {
		display = fmt.Sprintf("%s (@%s)", overseer.Name, overseer.Username)
	}
	fmt.Fprintf(w, "👤 %s %s\n", style.Bold.Render("Overseer:"), display)
	if overseer.UnreadMail > 0 {
		fmt.Fprintf(w, "   📬 %d unread\n", overseer.UnreadMail)
	}
	fmt.Fprintln(w)
}

func writeStatusDND(w io.Writer, dnd *DNDInfo) {
	if dnd == nil {
		return
	}
	icon, state, desc := "🔔", "off", "notifications normal"
	if dnd.Enabled {
		icon, state, desc = "🔕", "on", "notifications muted"
	}
	fmt.Fprintf(w, "%s %s %s", icon, style.Bold.Render("DND:"), style.Bold.Render(state))
	if dnd.Agent != "" {
		fmt.Fprintf(w, " %s", style.Dim.Render("("+dnd.Agent+")"))
	}
	fmt.Fprintf(w, "\n   %s\n\n", style.Dim.Render(desc))
}

func writeStatusServices(w io.Writer, status TownStatus) {
	if status.Daemon == nil && status.Dolt == nil && status.Tmux == nil {
		return
	}
	parts := statusServiceParts(status)
	fmt.Fprintf(w, "%s %s\n\n", style.Bold.Render("Services:"), strings.Join(parts, "  "))
}

func statusServiceParts(status TownStatus) []string {
	var parts []string
	if status.Daemon != nil {
		parts = append(parts, formatDaemonService(status.Daemon))
	}
	if status.Dolt != nil {
		parts = append(parts, formatDoltService(status.Dolt))
	}
	if status.Tmux != nil {
		parts = append(parts, formatTmuxService(status.Tmux))
	}
	if status.ACP != nil {
		parts = append(parts, formatACPService(status.ACP))
	}
	return parts
}

func formatDaemonService(info *ServiceInfo) string {
	if info.Running {
		return fmt.Sprintf("daemon %s", style.Dim.Render(fmt.Sprintf("(PID %d)", info.PID)))
	}
	return fmt.Sprintf("daemon %s", style.Dim.Render("(stopped)"))
}

func formatDoltService(info *DoltInfo) string {
	if info.Remote {
		return fmt.Sprintf("dolt %s", style.Dim.Render(fmt.Sprintf("(remote :%d)", info.Port)))
	}
	if info.Running {
		dataDir := info.DataDir
		if home, err := os.UserHomeDir(); err == nil {
			dataDir = strings.Replace(dataDir, home, "~", 1)
		}
		return fmt.Sprintf("dolt %s", style.Dim.Render(fmt.Sprintf("(PID %d, :%d, %s)", info.PID, info.Port, dataDir)))
	}
	if info.PortConflict {
		return fmt.Sprintf("dolt %s", style.Bold.Render(fmt.Sprintf("(stopped, :%d ⚠ port used by %s)", info.Port, info.ConflictOwner)))
	}
	return fmt.Sprintf("dolt %s", style.Dim.Render(fmt.Sprintf("(stopped, :%d)", info.Port)))
}

func formatTmuxService(info *TmuxInfo) string {
	if info.Running {
		return fmt.Sprintf("tmux %s", style.Dim.Render(fmt.Sprintf("(-L %s, PID %d, %d sessions, %s)", info.Socket, info.PID, info.SessionCount, info.SocketPath)))
	}
	return fmt.Sprintf("tmux %s", style.Dim.Render(fmt.Sprintf("(-L %s, no server)", info.Socket)))
}

func formatACPService(info *ServiceInfo) string {
	if info.Running {
		return fmt.Sprintf("acp %s", style.Dim.Render(fmt.Sprintf("(PID %d)", info.PID)))
	}
	return fmt.Sprintf("acp %s", style.Dim.Render("(stopped)"))
}

func statusRoleIcons() map[string]string {
	return map[string]string{
		constants.RoleMayor:    constants.EmojiMayor,
		constants.RoleDeacon:   constants.EmojiDeacon,
		constants.RoleWitness:  constants.EmojiWitness,
		constants.RoleRefinery: constants.EmojiRefinery,
		constants.RoleCrew:     constants.EmojiCrew,
		constants.RolePolecat:  constants.EmojiPolecat,
		"coordinator":          constants.EmojiMayor,
		"health-check":         constants.EmojiDeacon,
	}
}

func writeStatusGlobalAgents(w io.Writer, status TownStatus, verbose bool, roleIcons map[string]string) {
	for _, agent := range status.Agents {
		icon := roleIcons[agent.Role]
		if icon == "" {
			icon = roleIcons[agent.Name]
		}
		if verbose {
			fmt.Fprintf(w, "%s %s\n", icon, style.Bold.Render(capitalizeFirst(agent.Name)))
			renderAgentDetails(w, agent, "   ", nil, status.Location)
			fmt.Fprintln(w)
			continue
		}
		renderAgentCompact(w, agent, icon+" ", nil, status.Location)
	}
	if !verbose && len(status.Agents) > 0 {
		fmt.Fprintln(w)
	}
}

func writeStatusRig(w io.Writer, r RigStatus, townRoot string, verbose bool, roleIcons map[string]string) {
	fmt.Fprintf(w, "─── %s ───────────────────────────────────────────\n\n", style.Bold.Render(r.Name+"/"))
	witnesses, refineries, crews, polecats := groupStatusAgents(r.Agents)
	writeStatusWitnesses(w, witnesses, r.Hooks, townRoot, verbose, roleIcons)
	writeStatusRefineries(w, refineries, r, townRoot, verbose, roleIcons)
	writeStatusCrew(w, crews, r.Hooks, townRoot, verbose, roleIcons)
	writeStatusPolecats(w, polecats, r.Hooks, townRoot, verbose, roleIcons)
	if len(witnesses)+len(refineries)+len(crews)+len(polecats) == 0 {
		fmt.Fprintf(w, "   %s\n", style.Dim.Render("(no agents)"))
	}
	fmt.Fprintln(w)
}

func groupStatusAgents(agents []AgentRuntime) (witnesses, refineries, crews, polecats []AgentRuntime) {
	for _, agent := range agents {
		switch agent.Role {
		case constants.RoleWitness:
			witnesses = append(witnesses, agent)
		case constants.RoleRefinery:
			refineries = append(refineries, agent)
		case constants.RoleCrew:
			crews = append(crews, agent)
		case constants.RolePolecat:
			polecats = append(polecats, agent)
		}
	}
	return witnesses, refineries, crews, polecats
}

func writeStatusWitnesses(w io.Writer, agents []AgentRuntime, hooks []AgentHookInfo, townRoot string, verbose bool, roleIcons map[string]string) {
	if len(agents) == 0 {
		return
	}
	if verbose {
		fmt.Fprintf(w, "%s %s\n", roleIcons[constants.RoleWitness], style.Bold.Render("Witness"))
		writeStatusAgentDetails(w, agents, hooks, townRoot)
		fmt.Fprintln(w)
		return
	}
	writeStatusAgentCompact(w, agents, roleIcons[constants.RoleWitness]+" ", hooks, townRoot)
}

func writeStatusRefineries(w io.Writer, agents []AgentRuntime, r RigStatus, townRoot string, verbose bool, roleIcons map[string]string) {
	if len(agents) == 0 {
		return
	}
	if verbose {
		fmt.Fprintf(w, "%s %s\n", roleIcons[constants.RoleRefinery], style.Bold.Render("Refinery"))
		writeStatusAgentDetails(w, agents, r.Hooks, townRoot)
		if mqStr := formatMQSummary(r.MQ); mqStr != "" {
			fmt.Fprintf(w, "   MQ: %s\n", mqStr)
		}
		fmt.Fprintln(w)
		return
	}
	for _, agent := range agents {
		mqSuffix := ""
		if mqStr := formatMQSummaryCompact(r.MQ); mqStr != "" {
			mqSuffix = "  " + mqStr
		}
		renderAgentCompactWithSuffix(w, agent, roleIcons[constants.RoleRefinery]+" ", r.Hooks, townRoot, mqSuffix)
	}
}

func writeStatusCrew(w io.Writer, agents []AgentRuntime, hooks []AgentHookInfo, townRoot string, verbose bool, roleIcons map[string]string) {
	writeStatusRoleGroup(w, agents, "Crew", constants.RoleCrew, hooks, townRoot, verbose, roleIcons)
}

func writeStatusPolecats(w io.Writer, agents []AgentRuntime, hooks []AgentHookInfo, townRoot string, verbose bool, roleIcons map[string]string) {
	writeStatusRoleGroup(w, agents, "Polecats", constants.RolePolecat, hooks, townRoot, verbose, roleIcons)
}

func writeStatusRoleGroup(w io.Writer, agents []AgentRuntime, label, role string, hooks []AgentHookInfo, townRoot string, verbose bool, roleIcons map[string]string) {
	if len(agents) == 0 {
		return
	}
	fmt.Fprintf(w, "%s %s (%d)\n", roleIcons[role], style.Bold.Render(label), len(agents))
	if verbose {
		writeStatusAgentDetails(w, agents, hooks, townRoot)
		fmt.Fprintln(w)
		return
	}
	writeStatusAgentCompact(w, agents, "   ", hooks, townRoot)
}

func writeStatusAgentDetails(w io.Writer, agents []AgentRuntime, hooks []AgentHookInfo, townRoot string) {
	for _, agent := range agents {
		renderAgentDetails(w, agent, "   ", hooks, townRoot)
	}
}

func writeStatusAgentCompact(w io.Writer, agents []AgentRuntime, indent string, hooks []AgentHookInfo, townRoot string) {
	for _, agent := range agents {
		renderAgentCompact(w, agent, indent, hooks, townRoot)
	}
}

// renderAgentDetails renders full agent bead details
func renderAgentDetails(w io.Writer, agent AgentRuntime, indent string, hooks []AgentHookInfo, townRoot string) { //nolint:unparam // indent kept for future customization
	statusStr, stateInfo := agentStatusDisplay(agent)
	agentBeadID := statusAgentBeadID(agent, townRoot)
	fmt.Fprintf(w, "%s%s %s%s\n", indent, style.Dim.Render(agentBeadID), statusStr, stateInfo)
	if agent.AgentInfo != "" {
		fmt.Printf("%s  agent: %s\n", indent, agent.AgentInfo)
	}
	fmt.Fprintf(w, "%s  hook: %s\n", indent, statusHookDisplay(agent, hooks))
	if agent.NotificationLevel == beads.NotifyMuted {
		fmt.Fprintf(w, "%s  notify: 🔕 muted (DND)\n", indent)
	}
	statusMailDisplay(w, agent, indent)
}

func agentStatusDisplay(agent AgentRuntime) (string, string) {
	status := style.Error.Render("stopped")
	if agent.Running {
		status = style.Success.Render("running")
	}

	stateInfo := ""
	switch agent.State {
	case "stuck":
		stateInfo = style.Warning.Render(" [stuck]")
	case "awaiting-gate":
		stateInfo = style.Dim.Render(" [awaiting-gate]")
	case "muted", "paused", "degraded":
		stateInfo = style.Dim.Render(fmt.Sprintf(" [%s]", agent.State))
	}
	return status, stateInfo
}

func statusAgentBeadID(agent AgentRuntime, townRoot string) string {
	agentBeadID := "gt-" + agent.Name
	if agent.Address == "" || agent.Address == agent.Name {
		return agentBeadID
	}

	parts := strings.Split(strings.TrimSuffix(agent.Address, "/"), "/")
	if len(parts) == 1 {
		return beads.AgentBeadIDWithPrefix(beads.TownBeadsPrefix, "", parts[0], "")
	}
	if len(parts) < 2 {
		return agentBeadID
	}
	return statusRigAgentBeadID(parts, townRoot, agentBeadID)
}

func statusRigAgentBeadID(parts []string, townRoot, fallback string) string {
	rig := parts[0]
	prefix := beads.GetPrefixForRig(townRoot, rig)
	switch {
	case parts[1] == constants.RoleCrew && len(parts) >= 3:
		return beads.CrewBeadIDWithPrefix(prefix, rig, parts[2])
	case parts[1] == constants.RoleWitness:
		return beads.WitnessBeadIDWithPrefix(prefix, rig)
	case parts[1] == constants.RoleRefinery:
		return beads.RefineryBeadIDWithPrefix(prefix, rig)
	case len(parts) == 2:
		return beads.PolecatBeadIDWithPrefix(prefix, rig, parts[1])
	default:
		return fallback
	}
}

func statusHookDisplay(agent AgentRuntime, hooks []AgentHookInfo) string {
	hookBead, hookTitle := agent.HookBead, agent.WorkTitle
	if hookBead == "" && hooks != nil {
		for _, hook := range hooks {
			if hook.Agent == agent.Address && hook.HasWork {
				hookBead, hookTitle = hook.Molecule, hook.Title
				break
			}
		}
	}
	if hookBead != "" {
		if hookTitle != "" {
			return fmt.Sprintf("%s → %s", hookBead, truncateWithEllipsis(hookTitle, 40))
		}
		return hookBead
	}
	if hookTitle != "" {
		return truncateWithEllipsis(hookTitle, 50)
	}
	return style.Dim.Render("(none)")
}

func statusMailDisplay(w io.Writer, agent AgentRuntime, indent string) {
	if agent.UnreadMail == 0 {
		return
	}
	mailStr := fmt.Sprintf("📬 %d unread", agent.UnreadMail)
	if agent.FirstSubject != "" {
		mailStr = fmt.Sprintf("📬 %d unread → %s", agent.UnreadMail, truncateWithEllipsis(agent.FirstSubject, 35))
	}
	fmt.Fprintf(w, "%s  mail: %s\n", indent, mailStr)
}

// formatMQSummary formats the MQ status for verbose display
func formatMQSummary(mq *MQSummary) string {
	if mq == nil {
		return ""
	}
	mqParts := []string{}
	if mq.Pending > 0 {
		mqParts = append(mqParts, fmt.Sprintf("%d pending", mq.Pending))
	}
	if mq.InFlight > 0 {
		mqParts = append(mqParts, style.Warning.Render(fmt.Sprintf("%d in-flight", mq.InFlight)))
	}
	if mq.Blocked > 0 {
		mqParts = append(mqParts, style.Dim.Render(fmt.Sprintf("%d blocked", mq.Blocked)))
	}
	if len(mqParts) == 0 {
		return ""
	}
	// Add state indicator
	stateIcon := "○" // idle
	switch mq.State {
	case "processing":
		stateIcon = style.Success.Render("●")
	case "blocked":
		stateIcon = style.Error.Render("○")
	}
	// Add health warning if stale
	healthSuffix := ""
	if mq.Health == "stale" {
		healthSuffix = style.Error.Render(" [stale]")
	}
	return fmt.Sprintf("%s %s%s", stateIcon, strings.Join(mqParts, ", "), healthSuffix)
}

// formatMQSummaryCompact formats MQ status for compact single-line display
func formatMQSummaryCompact(mq *MQSummary) string {
	if mq == nil {
		return ""
	}
	// Very compact: "MQ:12" or "MQ:12 [stale]"
	total := mq.Pending + mq.InFlight + mq.Blocked
	if total == 0 {
		return ""
	}
	healthSuffix := ""
	if mq.Health == "stale" {
		healthSuffix = style.Error.Render("[stale]")
	}
	return fmt.Sprintf("MQ:%d%s", total, healthSuffix)
}

// renderAgentCompactWithSuffix renders a single-line agent status with an extra suffix
func renderAgentCompactWithSuffix(w io.Writer, agent AgentRuntime, indent string, hooks []AgentHookInfo, _ string, suffix string) {
	// Build status indicator (gt-zecmc: use tmux state, not bead state)
	statusIndicator := buildStatusIndicator(agent)
	hookSuffix := agentHookSuffix(agent, hooks)
	mailSuffix := agentMailSuffix(agent)
	agentSuffix := agentRuntimeSuffix(agent)

	// Print single line: name + status + agent-info + hook + mail + suffix
	fmt.Fprintf(w, "%s%-12s %s%s%s%s%s\n", indent, agent.Name, statusIndicator, agentSuffix, hookSuffix, mailSuffix, suffix)
}

// renderAgentCompact renders a single-line agent status
func renderAgentCompact(w io.Writer, agent AgentRuntime, indent string, hooks []AgentHookInfo, _ string) {
	renderAgentCompactWithSuffix(w, agent, indent, hooks, "", "")
}

func agentHookSuffix(agent AgentRuntime, hooks []AgentHookInfo) string {
	hookBead, hookTitle := agent.HookBead, agent.WorkTitle
	if hookBead == "" && hooks != nil {
		for _, h := range hooks {
			if h.Agent == agent.Address && h.HasWork {
				hookBead, hookTitle = h.Molecule, h.Title
				break
			}
		}
	}
	if hookBead != "" {
		if hookTitle != "" {
			return style.Dim.Render(" → ") + truncateWithEllipsis(hookTitle, 30)
		}
		return style.Dim.Render(" → ") + hookBead
	}
	if hookTitle != "" {
		return style.Dim.Render(" → ") + truncateWithEllipsis(hookTitle, 30)
	}
	return ""
}

func agentMailSuffix(agent AgentRuntime) string {
	if agent.UnreadMail > 0 {
		return fmt.Sprintf(" 📬%d", agent.UnreadMail)
	}
	return ""
}

func agentRuntimeSuffix(agent AgentRuntime) string {
	if agent.AgentInfo != "" {
		return " " + style.Dim.Render("["+agent.AgentInfo+"]")
	}
	return ""
}

func applyPaneBlock(agent *AgentRuntime) {
	if agent == nil || !agent.Running || agent.Session == "" {
		return
	}
	content, err := tmux.NewTmux().CapturePane(agent.Session, 80)
	if err != nil {
		return
	}
	if name, ok := tmux.ContainsBlockingPane(content); ok {
		agent.Blocked = true
		agent.BlockReason = name
	}
}

// buildStatusIndicator creates the visual status indicator for an agent.
// Per gt-zecmc: uses tmux state (observable reality), not bead state.
// Non-observable states (stuck, awaiting-gate, muted, etc.) are shown as suffixes.
func buildStatusIndicator(agent AgentRuntime) string {
	sessionExists := agent.Running

	// Base indicator from tmux state or ACP state
	var indicator string
	if agent.Blocked {
		indicator = style.Warning.Render("●") + style.Warning.Render(" blocked")
	} else if sessionExists {
		indicator = style.Success.Render("●")
	} else {
		indicator = style.Error.Render("○")
	}

	// Add mode info if ACP
	if agent.ACP {
		indicator += style.Dim.Render(" acp")
	}

	// Add non-observable state suffix if present
	beadState := agent.State
	switch beadState {
	case "stuck":
		indicator += style.Warning.Render(" stuck")
	case "awaiting-gate":
		indicator += style.Dim.Render(" gate")
	case "muted", "paused", "degraded":
		indicator += style.Dim.Render(" " + beadState)
		// Ignore observable states: running, idle, dead, done, stopped, ""
	}

	if agent.NotificationLevel == beads.NotifyMuted {
		indicator += style.Dim.Render(" 🔕")
	}

	return indicator
}

// formatHookInfo formats the hook bead and title for display
func formatHookInfo(hookBead, title string, maxLen int) string {
	if hookBead == "" {
		return ""
	}
	if title == "" {
		return fmt.Sprintf(" → %s", hookBead)
	}
	title = truncateWithEllipsis(title, maxLen)
	return fmt.Sprintf(" → %s", title)
}

// truncateWithEllipsis shortens a string to maxLen, adding "..." if truncated
func truncateWithEllipsis(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 4 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// capitalizeFirst capitalizes the first letter of a string
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:]
}

// discoverRigHooks finds all hook attachments for agents in a rig.
// It fetches all pinned handoff beads in a single bd call, then resolves
// each agent's hook in-memory. This replaces the previous N+1 pattern where
// each agent triggered a separate bd subprocess.
func discoverRigHooks(r *rig.Rig, crews []string) []AgentHookInfo {
	var hooks []AgentHookInfo

	// Create beads instance for the rig
	b := beads.New(r.Path)

	// Batch-fetch all handoff beads in one bd call
	allHandoffs, err := b.FindAllHandoffBeads()
	if err != nil {
		// On error, return empty hooks for all agents rather than failing
		allHandoffs = make(map[string]*beads.Issue)
	}

	// Check polecats
	for _, name := range r.Polecats {
		hooks = append(hooks, resolveHookFromMap(allHandoffs, name, r.Name+"/"+name, constants.RolePolecat))
	}

	// Check crew workers
	for _, name := range crews {
		hooks = append(hooks, resolveHookFromMap(allHandoffs, name, r.Name+"/crew/"+name, constants.RoleCrew))
	}

	// Check witness
	if r.HasWitness {
		hooks = append(hooks, resolveHookFromMap(allHandoffs, constants.RoleWitness, r.Name+"/witness", constants.RoleWitness))
	}

	// Check refinery
	if r.HasRefinery {
		hooks = append(hooks, resolveHookFromMap(allHandoffs, constants.RoleRefinery, r.Name+"/refinery", constants.RoleRefinery))
	}

	return hooks
}

// resolveHookFromMap builds an AgentHookInfo from a pre-fetched map of handoff beads.
// This is the in-memory equivalent of getAgentHook, avoiding per-agent bd subprocess calls.
func resolveHookFromMap(allHandoffs map[string]*beads.Issue, role, agentAddress, roleType string) AgentHookInfo {
	hook := AgentHookInfo{
		Agent: agentAddress,
		Role:  roleType,
	}

	handoff, ok := allHandoffs[role]
	if !ok || handoff == nil {
		return hook
	}

	attachment := beads.ParseAttachmentFields(handoff)
	if attachment != nil && attachment.AttachedMolecule != "" {
		hook.HasWork = true
		hook.Molecule = attachment.AttachedMolecule
		hook.Title = handoff.Title
	} else if handoff.Description != "" {
		hook.HasWork = true
		hook.Title = handoff.Title
	}

	return hook
}

// discoverGlobalAgents checks runtime state for town-level agents (Mayor, Deacon).
// Uses parallel fetching for performance. If skipMail is true, mail lookups are skipped.
// allSessions is a preloaded map of tmux sessions for O(1) lookup.
// allAgentBeads is a preloaded map of agent beads for O(1) lookup.
// allHookBeads is a preloaded map of hook beads for O(1) lookup.
func discoverGlobalAgents(townRoot string, allSessions map[string]bool, allAgentBeads map[string]*beads.Issue, allHookBeads map[string]*beads.Issue, mailRouter *mail.Router, skipMail bool) []AgentRuntime {
	// Get session names dynamically
	mayorSession := getMayorSessionName()
	deaconSession := getDeaconSessionName()

	// Define agents to discover
	// Note: Mayor and Deacon are town-level agents with hq- prefix bead IDs
	agentDefs := []struct {
		name    string
		address string
		session string
		role    string
		beadID  string
	}{
		{constants.RoleMayor, constants.RoleMayor + "/", mayorSession, "coordinator", beads.MayorBeadIDTown()},
		{constants.RoleDeacon, constants.RoleDeacon + "/", deaconSession, "health-check", beads.DeaconBeadIDTown()},
	}

	agents := make([]AgentRuntime, len(agentDefs))
	var wg sync.WaitGroup

	for i, def := range agentDefs {
		wg.Add(1)
		go func(idx int, d struct {
			name    string
			address string
			session string
			role    string
			beadID  string
		}) {
			defer wg.Done()

			agent := AgentRuntime{
				Name:    d.name,
				Address: d.address,
				Session: d.session,
				Role:    d.role,
			}

			// Check tmux session from preloaded map (O(1))
			agent.Running = allSessions[d.session]

			// Check for ACP session (for Mayor)
			if d.name == "mayor" {
				if mayor.IsACPActive(townRoot) {
					agent.ACP = true
					agent.Running = true
				}
			}
			applyPaneBlock(&agent)

			// Look up agent bead from preloaded map (O(1))
			if issue, ok := allAgentBeads[d.beadID]; ok {
				// Prefer database columns over description parsing
				// HookBead column is authoritative (cleared by unsling)
				agent.HookBead = issue.HookBead
				agent.State = beads.ResolveAgentState(issue.Description, issue.AgentState)
				if agent.HookBead != "" {
					agent.HasWork = true
					// Get hook title from preloaded map
					if pinnedIssue, ok := allHookBeads[agent.HookBead]; ok {
						agent.WorkTitle = pinnedIssue.Title
					}
				}
				// Parse description fields for notification level
				if fields := beads.ParseAgentFields(issue.Description); fields != nil {
					agent.NotificationLevel = fields.NotificationLevel
				}
			}

			// Get mail info (skip if --fast)
			if !skipMail {
				populateMailInfo(&agent, mailRouter)
			}

			agents[idx] = agent
		}(i, def)
	}

	wg.Wait()
	return agents
}

// populateMailInfo fetches unread mail count and first subject for an agent
func populateMailInfo(agent *AgentRuntime, router *mail.Router) {
	if router == nil {
		return
	}
	mailbox, err := router.GetMailbox(agent.Address)
	if err != nil {
		return
	}
	messages, err := mailbox.List()
	if err != nil {
		return
	}
	firstSubjectSet := false
	for _, msg := range messages {
		if msg.Read {
			continue
		}
		agent.UnreadMail++
		if !firstSubjectSet {
			agent.FirstSubject = msg.Subject
			firstSubjectSet = true
		}
	}
}

// detectCurrentDNDStatus returns DND status for the currently resolved role context.
// Returns nil when role context cannot be determined (e.g. outside agent context).
func detectCurrentDNDStatus(townRoot string) *DNDInfo {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}

	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return nil
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
		return nil
	}

	bd := beads.New(townRoot)
	level, err := bd.GetAgentNotificationLevel(agentBeadID)
	if err != nil || level == "" {
		level = beads.NotifyNormal
	}

	return &DNDInfo{
		Enabled: level == beads.NotifyMuted,
		Level:   level,
		Agent:   agentBeadID,
	}
}

// agentDef defines an agent to discover
type agentDef struct {
	name    string
	address string
	session string
	role    string
	beadID  string
}

// discoverRigAgents checks runtime state for all agents in a rig.
// Uses parallel fetching for performance. If skipMail is true, mail lookups are skipped.
// allSessions is a preloaded map of tmux sessions for O(1) lookup.
// allAgentBeads is a preloaded map of agent beads for O(1) lookup.
// allHookBeads is a preloaded map of hook beads for O(1) lookup.
func discoverRigAgents(allSessions map[string]bool, r *rig.Rig, crews []string, allAgentBeads map[string]*beads.Issue, allHookBeads map[string]*beads.Issue, mailRouter *mail.Router, skipMail bool) []AgentRuntime {
	defs := rigAgentDefs(r, crews)
	if len(defs) == 0 {
		return nil
	}

	agents := make([]AgentRuntime, len(defs))
	var wg sync.WaitGroup
	for i, def := range defs {
		wg.Add(1)
		go func(idx int, d agentDef) {
			defer wg.Done()
			agents[idx] = discoverRigAgent(d, allSessions, allAgentBeads, allHookBeads, mailRouter, skipMail)
		}(i, def)
	}
	wg.Wait()
	return agents
}

func rigAgentDefs(r *rig.Rig, crews []string) []agentDef {
	var defs []agentDef
	townRoot := filepath.Dir(r.Path)
	prefix := beads.GetPrefixForRig(townRoot, r.Name)
	if r.HasWitness {
		defs = append(defs, agentDef{
			name:    constants.RoleWitness,
			address: r.Name + "/witness",
			session: witnessSessionName(r.Name),
			role:    constants.RoleWitness,
			beadID:  beads.WitnessBeadIDWithPrefix(prefix, r.Name),
		})
	}
	if r.HasRefinery {
		defs = append(defs, agentDef{
			name:    constants.RoleRefinery,
			address: r.Name + "/refinery",
			session: session.RefinerySessionName(session.PrefixFor(r.Name)),
			role:    constants.RoleRefinery,
			beadID:  beads.RefineryBeadIDWithPrefix(prefix, r.Name),
		})
	}
	for _, name := range r.Polecats {
		defs = append(defs, agentDef{
			name:    name,
			address: r.Name + "/" + name,
			session: session.PolecatSessionName(session.PrefixFor(r.Name), name),
			role:    constants.RolePolecat,
			beadID:  beads.PolecatBeadIDWithPrefix(prefix, r.Name, name),
		})
	}
	for _, name := range crews {
		defs = append(defs, agentDef{
			name:    name,
			address: r.Name + "/crew/" + name,
			session: crewSessionName(r.Name, name),
			role:    constants.RoleCrew,
			beadID:  beads.CrewBeadIDWithPrefix(prefix, r.Name, name),
		})
	}
	return defs
}

func discoverRigAgent(d agentDef, allSessions map[string]bool, allAgentBeads map[string]*beads.Issue, allHookBeads map[string]*beads.Issue, mailRouter *mail.Router, skipMail bool) AgentRuntime {
	agent := AgentRuntime{
		Name:    d.name,
		Address: d.address,
		Session: d.session,
		Role:    d.role,
		Running: allSessions[d.session],
	}
	applyPaneBlock(&agent)
	if issue, ok := allAgentBeads[d.beadID]; ok {
		agent.HookBead = issue.HookBead
		agent.State = beads.ResolveAgentState(issue.Description, issue.AgentState)
		if agent.HookBead != "" {
			agent.HasWork = true
			if pinnedIssue, ok := allHookBeads[agent.HookBead]; ok {
				agent.WorkTitle = pinnedIssue.Title
			}
		}
		if fields := beads.ParseAgentFields(issue.Description); fields != nil {
			agent.NotificationLevel = fields.NotificationLevel
		}
	}
	if !skipMail {
		populateMailInfo(&agent, mailRouter)
	}
	return agent
}

// getMQSummary queries beads for merge-request issues and returns a summary.
// Uses a single bd call to fetch all non-closed merge-requests, then splits
// open vs in_progress in memory. Previously used two separate bd calls.
// Returns nil if the rig has no refinery or no MQ issues.
func getMQSummary(r *rig.Rig) *MQSummary {
	if !r.HasRefinery {
		return nil
	}

	allMRs, err := loadMergeRequestIssues(r)
	if err != nil {
		return nil
	}

	pending, blocked, inProgress := countMergeRequestStates(allMRs)
	if pending == 0 && inProgress == 0 && blocked == 0 {
		return nil
	}

	return &MQSummary{
		Pending:  pending,
		InFlight: inProgress,
		Blocked:  blocked,
		State:    mergeQueueState(pending, blocked, inProgress),
		Health:   mergeQueueHealth(pending, blocked, inProgress),
	}
}

func loadMergeRequestIssues(r *rig.Rig) ([]*beads.Issue, error) {
	b := beads.New(r.BeadsPath())
	return b.List(beads.ListOptions{
		Label:    "gt:merge-request",
		Status:   "all",
		Priority: -1,
	})
}

func countMergeRequestStates(allMRs []*beads.Issue) (pending, blocked, inProgress int) {
	for _, mr := range allMRs {
		switch mr.Status {
		case "open":
			if len(mr.BlockedBy) > 0 || mr.BlockedByCount > 0 {
				blocked++
			} else {
				pending++
			}
		case "in_progress":
			inProgress++
		}
	}
	return pending, blocked, inProgress
}

func mergeQueueState(pending, blocked, inProgress int) string {
	if inProgress > 0 {
		return "processing"
	}
	if pending > 0 {
		return "idle"
	}
	if blocked > 0 {
		return "blocked"
	}
	return "idle"
}

func mergeQueueHealth(pending, blocked, inProgress int) string {
	if pending+inProgress+blocked == 0 {
		return "empty"
	}
	if pending > 10 && inProgress == 0 {
		return "stale"
	}
	return "healthy"
}

// getAgentHook retrieves hook status for a specific agent.
func getAgentHook(b *beads.Beads, role, agentAddress, roleType string) AgentHookInfo {
	hook := AgentHookInfo{
		Agent: agentAddress,
		Role:  roleType,
	}

	// Find handoff bead for this role
	handoff, err := b.FindHandoffBead(role)
	if err != nil || handoff == nil {
		return hook
	}

	// Check for attachment
	attachment := beads.ParseAttachmentFields(handoff)
	if attachment != nil && attachment.AttachedMolecule != "" {
		hook.HasWork = true
		hook.Molecule = attachment.AttachedMolecule
		hook.Title = handoff.Title
	} else if handoff.Description != "" {
		// Has content but no molecule - still has work
		hook.HasWork = true
		hook.Title = handoff.Title
	}

	return hook
}
