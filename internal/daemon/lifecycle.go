package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	gtgit "github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/refinery"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/util"
)

// BeadsMessage represents a message from gt mail inbox --json.
type BeadsMessage struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	Timestamp string `json:"timestamp"`
	Read      bool   `json:"read"`
	Priority  string `json:"priority"`
	Type      string `json:"type"`
}

// MaxLifecycleMessageAge is the default max age of a lifecycle message before it's ignored.
// Configurable via operational.daemon.max_lifecycle_message_age.
const MaxLifecycleMessageAge = 6 * time.Hour

// ProcessLifecycleRequests checks for and processes lifecycle requests from the deacon inbox.
func (d *Daemon) ProcessLifecycleRequests() {
	output, err := d.fetchLifecycleMessages()
	if err != nil {
		d.logger.Printf("Warning: failed to fetch deacon inbox: %v", err)
		return
	}
	if len(output) == 0 || string(output) == "[]" || string(output) == "[]\n" {
		return
	}
	var messages []BeadsMessage
	if err := json.Unmarshal(output, &messages); err != nil {
		d.logger.Printf("Error parsing mail: %v", err)
		return
	}
	for i := range messages {
		d.processLifecycleMessage(&messages[i])
	}
}

func (d *Daemon) fetchLifecycleMessages() ([]byte, error) {
	cmd := exec.Command(d.gtPath, "mail", "inbox", "--identity", "deacon/", "--json")
	cmd.Dir = d.config.TownRoot
	cmd.Env = bdReadOnlyPinnedEnv(filepath.Join(d.config.TownRoot, ".beads"))
	util.SetDetachedProcessGroup(cmd)
	return cmd.Output()
}

func (d *Daemon) processLifecycleMessage(msg *BeadsMessage) {
	if msg.Read {
		return
	}
	request := d.parseLifecycleRequest(msg)
	if request == nil {
		return
	}
	if d.lifecycleMessageStale(msg, request) {
		return
	}
	d.logger.Printf("Processing lifecycle request from %s: %s", request.From, request.Action)
	if err := d.closeMessage(msg.ID); err != nil {
		d.logger.Printf("Warning: failed to delete message %s before execution: %v", msg.ID, err)
	}
	if err := d.executeLifecycleAction(request); err != nil {
		d.logger.Printf("Error executing lifecycle action: %v", err)
	}
}

func (d *Daemon) lifecycleMessageStale(msg *BeadsMessage, request *LifecycleRequest) bool {
	maxAge := d.loadOperationalConfig().GetDaemonConfig().MaxLifecycleMessageAgeD()
	msgTime, err := time.Parse(time.RFC3339, msg.Timestamp)
	if err != nil || time.Since(msgTime) <= maxAge {
		return false
	}
	age := time.Since(msgTime)
	d.logger.Printf("Ignoring stale lifecycle request from %s (age: %v, max: %v) - deleting",
		request.From, age.Round(time.Minute), maxAge)
	if err := d.closeMessage(msg.ID); err != nil {
		d.logger.Printf("Warning: failed to delete stale message %s: %v", msg.ID, err)
	}
	return true
}

// LifecycleBody is the structured body format for lifecycle requests.
// Claude should send mail with JSON body: {"action": "cycle"} or {"action": "shutdown"}
type LifecycleBody struct {
	Action string `json:"action"`
}

// parseLifecycleRequest extracts a lifecycle request from a message.
// Uses structured body parsing instead of keyword matching on subject.
func (d *Daemon) parseLifecycleRequest(msg *BeadsMessage) *LifecycleRequest {
	subject := strings.ToLower(msg.Subject)
	if !strings.HasPrefix(subject, "lifecycle:") {
		return nil
	}
	body, ok := parseLifecycleBody(msg.Body)
	if !ok {
		d.logger.Printf("Lifecycle request with unparseable body: %q", msg.Body)
		return nil
	}
	action, ok := lifecycleActionFor(body.Action)
	if !ok {
		d.logger.Printf("Unknown lifecycle action: %q", body.Action)
		return nil
	}
	return &LifecycleRequest{
		From:      msg.From,
		Action:    action,
		Timestamp: time.Now(),
	}
}

func parseLifecycleBody(raw string) (LifecycleBody, bool) {
	var body LifecycleBody
	if json.Unmarshal([]byte(raw), &body) == nil {
		return body, true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "restart", "action: restart":
		body.Action = "restart"
	case "shutdown", "action: shutdown", "stop":
		body.Action = "shutdown"
	case "cycle", "action: cycle":
		body.Action = "cycle"
	default:
		return LifecycleBody{}, false
	}
	return body, true
}

func lifecycleActionFor(raw string) (LifecycleAction, bool) {
	switch strings.ToLower(raw) {
	case "restart":
		return ActionRestart, true
	case "shutdown", "stop":
		return ActionShutdown, true
	case "cycle":
		return ActionCycle, true
	default:
		return LifecycleAction(""), false
	}
}

// executeLifecycleAction performs the requested lifecycle action.
func (d *Daemon) executeLifecycleAction(request *LifecycleRequest) error {
	sessionName := d.identityToSession(request.From)
	if sessionName == "" {
		return fmt.Errorf("unknown agent identity: %s", request.From)
	}

	d.logger.Printf("Executing %s for session %s", request.Action, sessionName)

	d.logLifecycleAgentState(request.From)
	running, err := d.tmux.HasSession(sessionName)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}

	switch request.Action {
	case ActionShutdown:
		return d.shutdownLifecycleSession(sessionName, running)
	case ActionCycle, ActionRestart:
		return d.restartLifecycleSession(sessionName, request.From, running)
	default:
		return fmt.Errorf("unknown action: %s", request.Action)
	}
}

func (d *Daemon) logLifecycleAgentState(identity string) {
	agentBeadID := d.identityToAgentBeadID(identity)
	if agentBeadID == "" {
		return
	}
	if beadState, err := d.getAgentBeadState(agentBeadID); err == nil {
		d.logger.Printf("Agent bead %s reports state: %s", agentBeadID, beadState)
	}
}

func (d *Daemon) shutdownLifecycleSession(sessionName string, running bool) error {
	if !running {
		return nil
	}
	if err := d.tmux.KillSessionWithProcesses(sessionName); err != nil {
		return fmt.Errorf("killing session: %w", err)
	}
	d.logger.Printf("Killed session %s", sessionName)
	return nil
}

func (d *Daemon) restartLifecycleSession(sessionName, identity string, running bool) error {
	if running {
		if err := d.tmux.KillSessionWithProcesses(sessionName); err != nil {
			return fmt.Errorf("killing session: %w", err)
		}
		d.logger.Printf("Killed session %s for restart", sessionName)
		time.Sleep(constants.ShutdownNotifyDelay)
	}
	if err := d.restartSession(sessionName, identity); err != nil {
		return fmt.Errorf("restarting session: %w", err)
	}
	d.logger.Printf("Restarted session %s", sessionName)
	return nil
}

// ParsedIdentity holds the components extracted from an agent identity string.
// This is used to look up the appropriate role config for lifecycle management.
type ParsedIdentity struct {
	RoleType  string // mayor, deacon, witness, refinery, crew, polecat
	RigName   string // Empty for town-level agents (mayor, deacon)
	AgentName string // Empty for singletons (mayor, deacon, witness, refinery)
}

// parseIdentity extracts role type, rig name, and agent name from an identity string.
// This is the ONLY place where identity string patterns are parsed.
// All other functions should use the extracted components to look up role config.
func parseIdentity(identity string) (*ParsedIdentity, error) {
	switch identity {
	case constants.RoleMayor:
		return &ParsedIdentity{RoleType: constants.RoleMayor}, nil
	case constants.RoleDeacon:
		return &ParsedIdentity{RoleType: constants.RoleDeacon}, nil
	}
	if parsed, ok := parseIdentitySuffix(identity, "-witness", constants.RoleWitness); ok {
		return parsed, nil
	}
	if parsed, ok := parseIdentitySuffix(identity, "-refinery", constants.RoleRefinery); ok {
		return parsed, nil
	}
	if parsed, ok := parseIdentityDelimited(identity, "-crew-", constants.RoleCrew); ok {
		return parsed, nil
	}
	if parsed, ok := parseIdentityDelimited(identity, "-polecat-", constants.RolePolecat); ok {
		return parsed, nil
	}
	if parsed, ok := parseIdentityDelimited(identity, "/polecats/", constants.RolePolecat); ok {
		return parsed, nil
	}
	return nil, fmt.Errorf("unknown identity format: %s", identity)
}

func parseIdentitySuffix(identity, suffix, role string) (*ParsedIdentity, bool) {
	if !strings.HasSuffix(identity, suffix) {
		return nil, false
	}
	return &ParsedIdentity{RoleType: role, RigName: strings.TrimSuffix(identity, suffix)}, true
}

func parseIdentityDelimited(identity, delimiter, role string) (*ParsedIdentity, bool) {
	if !strings.Contains(identity, delimiter) {
		return nil, false
	}
	parts := strings.Split(identity, delimiter)
	if len(parts) != 2 {
		return nil, false
	}
	return &ParsedIdentity{RoleType: role, RigName: parts[0], AgentName: parts[1]}, true
}

// getRoleConfigForIdentity loads role configuration from the config-based role system.
// Uses config.LoadRoleDefinition() with layered override resolution (builtin → town → rig).
// Returns config in beads.RoleConfig format for backward compatibility.
func (d *Daemon) getRoleConfigForIdentity(identity string) (*beads.RoleConfig, *ParsedIdentity, error) {
	parsed, err := parseIdentity(identity)
	if err != nil {
		return nil, nil, err
	}

	// Determine rig path for rig-scoped roles
	rigPath := ""
	if parsed.RigName != "" {
		rigPath = filepath.Join(d.config.TownRoot, parsed.RigName)
	}

	// Load role definition from config system (Phase 2: config-based roles)
	roleDef, err := config.LoadRoleDefinition(d.config.TownRoot, rigPath, parsed.RoleType)
	if err != nil {
		d.logger.Printf("Warning: failed to load role definition for %s: %v", parsed.RoleType, err)
		// Return parsed identity even if config fails (caller can use defaults)
		return nil, parsed, nil
	}

	// Convert to beads.RoleConfig for backward compatibility
	roleConfig := &beads.RoleConfig{
		SessionPattern: roleDef.Session.Pattern,
		WorkDirPattern: roleDef.Session.WorkDir,
		NeedsPreSync:   roleDef.Session.NeedsPreSync,
		StartCommand:   roleDef.Session.StartCommand,
		EnvVars:        roleDef.Env,
	}

	return roleConfig, parsed, nil
}

// identityToSession converts a beads identity to a tmux session name.
// Always uses session.*SessionName() functions for consistency with gt up and daemon heartbeat.
func (d *Daemon) identityToSession(identity string) string {
	parsed, err := parseIdentity(identity)
	if err != nil {
		return ""
	}

	switch parsed.RoleType {
	case constants.RoleMayor:
		return session.MayorSessionName()
	case constants.RoleDeacon:
		return session.DeaconSessionName()
	case constants.RoleWitness:
		return session.WitnessSessionName(session.PrefixFor(parsed.RigName))
	case constants.RoleRefinery:
		return session.RefinerySessionName(session.PrefixFor(parsed.RigName))
	case constants.RoleCrew:
		return session.CrewSessionName(session.PrefixFor(parsed.RigName), parsed.AgentName)
	case constants.RolePolecat:
		return session.PolecatSessionName(session.PrefixFor(parsed.RigName), parsed.AgentName)
	default:
		return ""
	}
}

// restartSession starts a new session for the given agent.
// Uses role config if available, falls back to hardcoded defaults.
func (d *Daemon) restartSession(sessionName, identity string) error {
	roleConfig, parsed, err := d.getRoleConfigForIdentity(identity)
	if err != nil {
		return fmt.Errorf("parsing identity: %w", err)
	}
	if err := d.validateRestart(parsed, identity); err != nil {
		return err
	}
	workDir := d.getWorkDir(roleConfig, parsed)
	if workDir == "" {
		return fmt.Errorf("cannot determine working directory for %s", identity)
	}
	if d.getNeedsPreSync(roleConfig, parsed) {
		d.logger.Printf("Pre-syncing workspace for %s at %s", identity, workDir)
		d.syncWorkspace(workDir)
	}
	work := d.buildRestartWork(sessionName, roleConfig, parsed, workDir)
	started, err := d.startRestartWork(sessionName, parsed, work)
	if err != nil {
		return err
	}
	if started {
		time.Sleep(constants.ShutdownNotifyDelay)
	}
	return nil
}

func (d *Daemon) validateRestart(parsed *ParsedIdentity, identity string) error {
	if parsed.RigName != "" {
		if operational, reason := isRigOperational(d, parsed.RigName); !operational {
			d.logger.Printf("Skipping session restart for %s: %s", identity, reason)
			return fmt.Errorf("cannot restart session: %s", reason)
		}
	}
	if parsed.RoleType != constants.RoleRefinery {
		return nil
	}
	stop, err := refinery.ActiveSafetyStop(d.config.TownRoot, parsed.RigName)
	if err != nil {
		return fmt.Errorf("checking refinery safety stop: %w", err)
	}
	if stop != nil {
		d.logger.Printf("Skipping session restart for %s: %s", identity, stop.Reason())
		return refinery.NewSafetyStoppedError(stop)
	}
	return nil
}

func (d *Daemon) buildRestartWork(sessionName string, roleConfig *beads.RoleConfig, parsed *ParsedIdentity, workDir string) session.Work {
	rigPath := restartRigPath(d.config.TownRoot, parsed)
	work := session.Work{
		SessionID: sessionName,
		WorkDir:   workDir,
		TownRoot:  d.config.TownRoot,
		RigPath:   rigPath,
		RigName:   parsed.RigName,
		AgentName: parsed.AgentName,
		Beacon: session.BeaconConfig{
			Recipient: session.BeaconRecipient(parsed.RoleType, parsed.AgentName, parsed.RigName),
			Sender:    "daemon",
			Topic:     "lifecycle-restart",
		},
		Instructions: "Run `gt prime --hook` and begin work.",
		Theme:        tmux.ResolveSessionTheme(d.config.TownRoot, parsed.RigName, parsed.RoleType, parsed.AgentName),
	}
	if restartUsesCustomCommand(d.config.TownRoot, rigPath, parsed, roleConfig) {
		work.Command = d.getStartCommand(roleConfig, parsed)
	}
	return work
}

func restartRigPath(townRoot string, parsed *ParsedIdentity) string {
	if parsed == nil || parsed.RigName == "" {
		return ""
	}
	return filepath.Join(townRoot, parsed.RigName)
}

func restartUsesCustomCommand(townRoot, rigPath string, parsed *ParsedIdentity, roleConfig *beads.RoleConfig) bool {
	if roleConfig == nil || roleConfig.StartCommand == "" {
		return false
	}
	rc := config.ResolveRoleAgentConfig(parsed.RoleType, townRoot, rigPath)
	return config.IsResolvedAgentClaude(rc) && !isBuiltinClaudeStartCommand(roleConfig.StartCommand)
}

func (d *Daemon) startRestartWork(sessionName string, parsed *ParsedIdentity, work session.Work) (bool, error) {
	if _, err := session.KillExistingSession(d.tmux, d.config.TownRoot, sessionName, true); err != nil {
		if errors.Is(err, session.ErrSessionAlive) {
			d.logger.Printf("Session %s already running with healthy agent, skipping restart", sessionName)
			return false, nil
		}
		return false, err
	}
	if _, err := session.StartSession(d.tmux, parsed.RoleType, work); err != nil {
		return false, err
	}
	return true, nil
}

// getWorkDir determines the working directory for an agent.
// Uses role config if available, falls back to hardcoded defaults.
func (d *Daemon) getWorkDir(config *beads.RoleConfig, parsed *ParsedIdentity) string {
	if config != nil && config.WorkDirPattern != "" {
		return beads.ExpandRolePattern(config.WorkDirPattern, d.config.TownRoot, parsed.RigName, parsed.AgentName, parsed.RoleType, session.PrefixFor(parsed.RigName))
	}
	return defaultWorkDir(d.config.TownRoot, parsed)
}

func defaultWorkDir(townRoot string, parsed *ParsedIdentity) string {
	switch parsed.RoleType {
	case constants.RoleMayor:
		return townRoot
	case constants.RoleDeacon:
		return townRoot
	case constants.RoleWitness:
		return filepath.Join(townRoot, parsed.RigName)
	case constants.RoleRefinery:
		return filepath.Join(townRoot, parsed.RigName, "refinery", "rig")
	case constants.RoleCrew:
		return filepath.Join(townRoot, parsed.RigName, "crew", parsed.AgentName)
	case constants.RolePolecat:
		newPath := filepath.Join(townRoot, parsed.RigName, "polecats", parsed.AgentName, parsed.RigName)
		if _, err := os.Stat(newPath); err == nil {
			return newPath
		}
		return filepath.Join(townRoot, parsed.RigName, "polecats", parsed.AgentName)
	default:
		return ""
	}
}

// getNeedsPreSync determines if a workspace needs git sync before starting.
// Uses role config if available, falls back to hardcoded defaults.
func (d *Daemon) getNeedsPreSync(config *beads.RoleConfig, parsed *ParsedIdentity) bool {
	// If role config is available, use it
	if config != nil {
		return config.NeedsPreSync
	}

	// Fallback: roles with persistent git clones need pre-sync
	switch parsed.RoleType {
	case constants.RoleRefinery, constants.RoleCrew:
		return true
	default:
		return false
	}
}

// isBuiltinClaudeStartCommand returns true if the start_command is the
// built-in default from role TOMLs ("exec claude --dangerously-skip-permissions").
// Custom start_commands (e.g., "exec run --town {town}") return false.
func isBuiltinClaudeStartCommand(cmd string) bool {
	trimmed := strings.TrimPrefix(cmd, "exec ")
	return trimmed == "claude --dangerously-skip-permissions"
}

// getStartCommand determines the startup command for an agent.
// Uses role config if available, then role-based agent selection, then hardcoded defaults.
// Includes beacon + role-specific instructions in the CLI prompt.
func (d *Daemon) getStartCommand(roleConfig *beads.RoleConfig, parsed *ParsedIdentity) string {
	if command, ok := d.customStartCommand(roleConfig, parsed); ok {
		return command
	}
	return d.defaultStartCommand(parsed)
}

func (d *Daemon) customStartCommand(roleConfig *beads.RoleConfig, parsed *ParsedIdentity) (string, bool) {
	if roleConfig == nil || roleConfig.StartCommand == "" {
		return "", false
	}
	rigPath := restartRigPath(d.config.TownRoot, parsed)
	rc := config.ResolveRoleAgentConfig(parsed.RoleType, d.config.TownRoot, rigPath)
	if !config.IsResolvedAgentClaude(rc) || isBuiltinClaudeStartCommand(roleConfig.StartCommand) {
		return "", false
	}
	cmd := beads.ExpandRolePattern(roleConfig.StartCommand, d.config.TownRoot, parsed.RigName, parsed.AgentName, parsed.RoleType, session.PrefixFor(parsed.RigName))
	if strings.HasPrefix(cmd, "exec ") {
		return "exec env -u CLAUDECODE NODE_OPTIONS='' " + cmd[len("exec "):], true
	}
	return "env -u CLAUDECODE NODE_OPTIONS='' " + cmd, true
}

func (d *Daemon) defaultStartCommand(parsed *ParsedIdentity) string {
	rigPath := restartRigPath(d.config.TownRoot, parsed)
	runtimeConfig := config.ResolveRoleAgentConfig(parsed.RoleType, d.config.TownRoot, rigPath)
	recipient := session.BeaconRecipient(parsed.RoleType, parsed.AgentName, parsed.RigName)
	prompt := session.BuildStartupPrompt(session.BeaconConfig{
		Recipient: recipient,
		Sender:    "daemon",
		Topic:     "lifecycle-restart",
	}, "Run `gt prime --hook` and begin work.")
	var sessionIDEnv string
	if runtimeConfig.Session != nil {
		sessionIDEnv = runtimeConfig.Session.SessionIDEnv
	}
	envVars := config.AgentEnv(config.AgentEnvConfig{
		Role:         parsed.RoleType,
		Rig:          parsed.RigName,
		AgentName:    parsed.AgentName,
		TownRoot:     d.config.TownRoot,
		SessionIDEnv: sessionIDEnv,
	})
	config.SanitizeAgentEnv(envVars, map[string]string{})
	return config.PrependEnv("exec "+runtimeConfig.BuildCommandWithPrompt(prompt), envVars)
}

// setSessionEnvironment sets environment variables for the tmux session.
// Uses centralized AgentEnv for consistency, plus custom env vars from role config if available.
func (d *Daemon) setSessionEnvironment(sessionName string, roleConfig *beads.RoleConfig, parsed *ParsedIdentity) {
	// Resolve CLAUDE_CONFIG_DIR from accounts.json so daemon-restarted sessions
	// use the correct account. Mirrors the crew startup path (start.go).
	accountsPath := constants.MayorAccountsPath(d.config.TownRoot)
	runtimeConfigDir, _, _ := config.ResolveAccountConfigDir(accountsPath, "")
	if runtimeConfigDir == "" {
		runtimeConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}

	// Use centralized AgentEnv for base environment variables
	envVars := config.AgentEnv(config.AgentEnvConfig{
		Role:             parsed.RoleType,
		Rig:              parsed.RigName,
		AgentName:        parsed.AgentName,
		TownRoot:         d.config.TownRoot,
		RuntimeConfigDir: runtimeConfigDir,
		SessionName:      sessionName,
	})
	for k, v := range envVars {
		_ = d.tmux.SetEnvironment(sessionName, k, v)
	}

	// Record agent's pane_id for ZFC-compliant liveness checks (gt-qmsx).
	if paneID, err := d.tmux.GetPaneID(sessionName); err == nil {
		_ = d.tmux.SetEnvironment(sessionName, "GT_PANE_ID", paneID)
	}

	// Set any custom env vars from role config.
	// Skip keys already set by AgentEnv to prevent TOML [env] from clobbering
	// canonical qualified values (e.g., GT_ROLE). See: https://github.com/steveyegge/gastown/issues/2492
	if roleConfig != nil {
		for k, v := range roleConfig.EnvVars {
			if _, alreadySet := envVars[k]; alreadySet {
				continue
			}
			expanded := beads.ExpandRolePattern(v, d.config.TownRoot, parsed.RigName, parsed.AgentName, parsed.RoleType, session.PrefixFor(parsed.RigName))
			_ = d.tmux.SetEnvironment(sessionName, k, expanded)
		}
	}
}

// applySessionTheme applies tmux theming to the session.
func (d *Daemon) applySessionTheme(sessionName string, parsed *ParsedIdentity) {
	rigName := parsed.RigName
	role := parsed.RoleType
	worker := parsed.RoleType
	if role == constants.RoleMayor {
		rigName = ""
		worker = "Mayor"
	}
	theme := tmux.ResolveSessionTheme(d.config.TownRoot, rigName, role, parsed.AgentName)
	_ = d.tmux.ConfigureGasTownSession(sessionName, theme, rigName, worker, role)
}

// syncFailureEscalationThreshold is the default number of consecutive pull failures
// before logging escalates from WARN to ERROR.
// Configurable via operational.daemon.sync_failure_escalation_threshold.
const syncFailureEscalationThreshold = 3

// syncWorkspace syncs a git workspace before starting a new session.
// This ensures agents with persistent clones (like refinery) start with current code.
// Handles dirty working trees by auto-stashing before pull and restoring after.
func (d *Daemon) syncWorkspace(workDir string) {
	if err := gtgit.EnsureSafeMutationWorkDir(workDir); err != nil {
		d.logger.Printf("Error: refusing daemon git sync in unsafe workdir %s: %v", workDir, err)
		return
	}
	defaultBranch := d.syncDefaultBranch(workDir)
	if err := runWorkspaceGitCommand(workDir, "fetch", "origin"); err != nil {
		d.logger.Printf("Error: git fetch failed in %s: %v", workDir, err)
		return // Fail fast - don't start agent with stale code
	}
	stashed, proceed := d.stashWorkspace(workDir)
	if !proceed {
		return
	}
	d.pullWorkspace(workDir, defaultBranch)
	if stashed {
		d.restoreWorkspace(workDir)
	}
}

func (d *Daemon) syncDefaultBranch(workDir string) string {
	defaultBranch := "main"
	rel, err := filepath.Rel(d.config.TownRoot, workDir)
	if err != nil {
		return defaultBranch
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 {
		return defaultBranch
	}
	rigCfg, err := rig.LoadRigConfig(filepath.Join(d.config.TownRoot, parts[0]))
	if err == nil && rigCfg.DefaultBranch != "" {
		return rigCfg.DefaultBranch
	}
	return defaultBranch
}

func runWorkspaceGitCommand(workDir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return errors.New(message)
		}
		return err
	}
	return nil
}

func (d *Daemon) stashWorkspace(workDir string) (bool, bool) {
	if !d.isWorkingTreeDirty(workDir) {
		return false, true
	}
	d.logger.Printf("Warning: dirty working tree in %s, auto-stashing before pull", workDir)
	if err := runWorkspaceGitCommand(workDir, "stash", "push", "-u", "-m", "daemon-auto-stash: pre-sync"); err != nil {
		d.logger.Printf("Warning: git stash failed in %s: %v, skipping pull", workDir, err)
		d.recordSyncFailure(workDir)
		return false, false
	}
	return true, true
}

func (d *Daemon) pullWorkspace(workDir, defaultBranch string) {
	if err := runWorkspaceGitCommand(workDir, "pull", "--rebase", "origin", defaultBranch); err == nil {
		d.resetSyncFailures(workDir)
		return
	} else {
		d.recordSyncFailure(workDir)
		failures := d.getSyncFailures(workDir)
		escalationThreshold := d.loadOperationalConfig().GetDaemonConfig().SyncFailureEscalationThresholdV()
		if failures >= escalationThreshold {
			d.logger.Printf("Error: git pull repeatedly failing in %s (%d consecutive failures): %v", workDir, failures, err)
			return
		}
		d.logger.Printf("Warning: git pull failed in %s (%d consecutive failure(s)): %v", workDir, failures, err)
	}
}

func (d *Daemon) restoreWorkspace(workDir string) {
	if err := runWorkspaceGitCommand(workDir, "stash", "pop"); err != nil {
		d.logger.Printf("Warning: git stash pop failed in %s: %v (stashed changes preserved in stash list)", workDir, err)
	}
}

// isWorkingTreeDirty checks if a git working tree has uncommitted changes.
func (d *Daemon) isWorkingTreeDirty(workDir string) bool {
	// "git status --porcelain" outputs nothing if clean
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	if err != nil {
		// If we can't check, assume dirty to be safe
		return true
	}
	return len(strings.TrimSpace(string(output))) > 0
}

// recordSyncFailure increments the consecutive failure counter for a workdir.
func (d *Daemon) recordSyncFailure(workDir string) {
	if d.SyncFailures == nil {
		d.SyncFailures = make(map[string]int)
	}
	d.SyncFailures[workDir]++
}

// getSyncFailures returns the consecutive failure count for a workdir.
func (d *Daemon) getSyncFailures(workDir string) int {
	if d.SyncFailures == nil {
		return 0
	}
	return d.SyncFailures[workDir]
}

// resetSyncFailures clears the failure counter for a workdir after a successful sync.
func (d *Daemon) resetSyncFailures(workDir string) {
	if d.SyncFailures == nil {
		return
	}
	delete(d.SyncFailures, workDir)
}

// closeMessage removes a lifecycle mail message after processing.
// We use delete instead of read because gt mail read intentionally
// doesn't mark messages as read (to preserve handoff messages).
func (d *Daemon) closeMessage(id string) error {
	// Use gt mail delete to actually remove the message
	cmd := exec.Command(d.gtPath, "mail", "delete", id)
	cmd.Dir = d.config.TownRoot
	cmd.Env = os.Environ() // Inherit PATH to find gt executable
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gt mail delete %s: %v (output: %s)", id, err, string(output))
	}
	d.logger.Printf("Deleted lifecycle message: %s", id)
	return nil
}

// AgentBeadInfo represents the parsed fields from an agent bead.
type AgentBeadInfo struct {
	ID         string `json:"id"`
	Type       string `json:"issue_type"`
	State      string // From description agent_state, fallback to legacy DB column
	HookBead   string // From DB column (hook_bead)
	RoleType   string // Parsed from description: role_type
	Rig        string // Parsed from description: rig
	LastUpdate string `json:"updated_at"`
	// Note: RoleBead field removed - role definitions are now config-based
}

// getAgentBeadState reads non-observable agent state from an agent bead.
// Per gt-zecmc: Observable states (running, dead, idle) are derived from tmux.
// Only non-observable states (stuck, awaiting-gate, muted, paused) are stored in beads.
// Returns the agent_state field value or empty string if not found.
func (d *Daemon) getAgentBeadState(agentBeadID string) (string, error) {
	info, err := d.getAgentBeadInfo(agentBeadID)
	if err != nil {
		return "", err
	}
	return info.State, nil
}

// getAgentBeadInfo fetches and parses an agent bead by ID.
//
// Agent beads (gt:agent-labeled, one per polecat/witness/refinery/dog) live
// in the town/hq Dolt DB but their IDs carry the rig prefix (e.g. za-zack-
// polecat-furiosa). Without forcing BEADS_DIR to the town's .beads, prefix
// routing would send the lookup to the rig's DB and return "issue not found"
// — which the reaper at daemon.go:2796 interprets as a stale polecat and
// kills mid-work after 3x threshold (hq-3kri). Pin the lookup to the town
// .beads so the reaper sees the truth.
func (d *Daemon) getAgentBeadInfo(agentBeadID string) (*AgentBeadInfo, error) {
	cmd := exec.Command(d.bdPath, "show", agentBeadID, "--json")
	cmd.Dir = d.config.TownRoot
	cmd.Env = bdReadOnlyPinnedEnv(filepath.Join(d.config.TownRoot, ".beads"))
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd show %s: %w", agentBeadID, err)
	}

	// bd show --json returns an array with one element
	var issues []struct {
		ID          string   `json:"id"`
		Type        string   `json:"issue_type"`
		Labels      []string `json:"labels"`
		Description string   `json:"description"`
		UpdatedAt   string   `json:"updated_at"`
		HookBead    string   `json:"hook_bead"`   // Read from database column
		AgentState  string   `json:"agent_state"` // Read from database column
	}

	if err := json.Unmarshal(output, &issues); err != nil {
		return nil, fmt.Errorf("parsing bd show output: %w", err)
	}

	if len(issues) == 0 {
		return nil, fmt.Errorf("agent bead not found: %s", agentBeadID)
	}

	issue := issues[0]
	if !beads.IsAgentBead(&beads.Issue{Type: issue.Type, Labels: issue.Labels}) {
		return nil, fmt.Errorf("bead %s is not an agent bead (type=%s)", agentBeadID, issue.Type)
	}

	// Parse agent fields from description for role/state info
	fields := beads.ParseAgentFields(issue.Description)

	info := &AgentBeadInfo{
		ID:         issue.ID,
		Type:       issue.Type,
		LastUpdate: issue.UpdatedAt,
	}

	if fields != nil {
		info.RoleType = fields.RoleType
		info.Rig = fields.Rig
	}

	info.State = beads.ResolveAgentState(issue.Description, issue.AgentState)

	// Use HookBead from database column directly (not from description)
	// The description may contain stale data - the slot is the source of truth.
	info.HookBead = issue.HookBead

	return info, nil
}

// getAgentHookBead re-reads the hook_bead for an agent bead from the database.
// Used for TOCTOU re-verification before taking destructive action on agents.
// Returns empty string on error or if no hook_bead is set.
func (d *Daemon) getAgentHookBead(agentBeadID string) string {
	cmd := exec.Command(d.bdPath, "show", agentBeadID, "--json")
	cmd.Dir = d.config.TownRoot
	cmd.Env = bdReadOnlyPinnedEnv(filepath.Join(d.config.TownRoot, ".beads"))
	util.SetDetachedProcessGroup(cmd)

	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	var issues []struct {
		HookBead string `json:"hook_bead"`
	}
	if err := json.Unmarshal(output, &issues); err != nil || len(issues) == 0 {
		return ""
	}
	return issues[0].HookBead
}

// identityToAgentBeadID maps a daemon identity to an agent bead ID.
// Uses parseIdentity to extract components, then uses beads package helpers.
func (d *Daemon) identityToAgentBeadID(identity string) string {
	parsed, err := parseIdentity(identity)
	if err != nil {
		return ""
	}

	switch parsed.RoleType {
	case constants.RoleDeacon:
		return beads.DeaconBeadIDTown()
	case constants.RoleMayor:
		return beads.MayorBeadIDTown()
	case constants.RoleWitness:
		prefix := config.GetRigPrefix(d.config.TownRoot, parsed.RigName)
		return beads.WitnessBeadIDWithPrefix(prefix, parsed.RigName)
	case constants.RoleRefinery:
		prefix := config.GetRigPrefix(d.config.TownRoot, parsed.RigName)
		return beads.RefineryBeadIDWithPrefix(prefix, parsed.RigName)
	case constants.RoleCrew:
		prefix := config.GetRigPrefix(d.config.TownRoot, parsed.RigName)
		return beads.CrewBeadIDWithPrefix(prefix, parsed.RigName, parsed.AgentName)
	case constants.RolePolecat:
		prefix := config.GetRigPrefix(d.config.TownRoot, parsed.RigName)
		return beads.PolecatBeadIDWithPrefix(prefix, parsed.RigName, parsed.AgentName)
	default:
		return ""
	}
}

// NOTE: checkStaleAgents() and markAgentDead() were removed in gt-zecmc.
// Agent liveness is now discovered from tmux, not recorded in beads.
// "Discover, don't track" principle: observable state should not be recorded.

// identityToBDActor converts a daemon identity to BD_ACTOR format (with slashes).
// Uses parseIdentity to extract components, then builds the slash format.
func identityToBDActor(identity string) string {
	if isSlashIdentity(identity) {
		return identity
	}
	parsed, err := parseIdentity(identity)
	if err != nil {
		return identity
	}
	return bdActorForIdentity(parsed, identity)
}

func isSlashIdentity(identity string) bool {
	return strings.Contains(identity, "/polecats/") || strings.Contains(identity, "/crew/") ||
		strings.Contains(identity, "/witness") || strings.Contains(identity, "/refinery")
}

func bdActorForIdentity(parsed *ParsedIdentity, fallback string) string {
	switch parsed.RoleType {
	case constants.RoleMayor, constants.RoleDeacon:
		return parsed.RoleType
	case constants.RoleWitness:
		return parsed.RigName + "/witness"
	case constants.RoleRefinery:
		return parsed.RigName + "/refinery"
	case constants.RoleCrew:
		return parsed.RigName + "/crew/" + parsed.AgentName
	case constants.RolePolecat:
		return parsed.RigName + "/polecats/" + parsed.AgentName
	default:
		return fallback
	}
}

// GUPPViolationTimeout is the canonical GUPP violation threshold.
// Defined in constants package — this alias avoids updating all call sites.
const GUPPViolationTimeout = constants.GUPPViolationTimeout

// listAgentBeadsJSON queries both the issues and wisps tables for agent beads
// and unmarshals the combined results into the provided slice pointer.
// The wisps query is best-effort (gracefully ignored if table doesn't exist).
func (d *Daemon) listAgentBeadsJSON(dest interface{}) error {
	// Query issues table (backward compat during migration)
	cmd := exec.Command(d.bdPath, "list", "--label=gt:agent", "--json", "--flat") //nolint:gosec // G204: bd is a trusted internal tool
	cmd.Dir = d.config.TownRoot
	cmd.Env = bdReadOnlyPinnedEnv(filepath.Join(d.config.TownRoot, ".beads"))
	util.SetDetachedProcessGroup(cmd)

	issuesOutput, issuesErr := cmd.Output()

	// Query wisps table (primary source after agent bead migration)
	wispCmd := exec.Command(d.bdPath, "mol", "wisp", "list", "--json") //nolint:gosec // G204: bd is a trusted internal tool
	wispCmd.Dir = d.config.TownRoot
	wispCmd.Env = bdReadOnlyPinnedEnv(filepath.Join(d.config.TownRoot, ".beads"))
	util.SetDetachedProcessGroup(wispCmd)

	wispOutput, _ := wispCmd.Output() // Best-effort: wisps table may not exist

	// Merge results: parse wisps first, then issues (wisps take precedence)
	combined := mergeAgentBeadJSON(wispOutput, issuesOutput)
	if combined == nil {
		if issuesErr != nil {
			return fmt.Errorf("bd list failed: %w", issuesErr)
		}
		return fmt.Errorf("no agent beads found")
	}

	return json.Unmarshal(combined, dest)
}

// mergeAgentBeadJSON merges JSON arrays from wisps and issues queries.
// Returns a combined JSON array with wisps taking precedence for duplicate IDs.
// Filters wisps to only include agent beads (type=agent or label gt:agent).
func mergeAgentBeadJSON(wispJSON, issuesJSON []byte) []byte {
	seenIDs := make(map[string]bool)
	result := mergeWispAgentJSON(wispJSON, seenIDs)
	result = mergeIssueAgentJSON(issuesJSON, seenIDs, result)
	if len(result) == 0 {
		return nil
	}

	combined, _ := json.Marshal(result)
	return combined
}

type agentBeadJSONEntry struct {
	ID     string   `json:"id"`
	Type   string   `json:"issue_type"`
	Labels []string `json:"labels"`
}

func mergeWispAgentJSON(rawJSON []byte, seenIDs map[string]bool) []json.RawMessage {
	if len(rawJSON) == 0 {
		return nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(rawJSON, &entries) != nil {
		return nil
	}
	var result []json.RawMessage
	for _, raw := range entries {
		var entry agentBeadJSONEntry
		if json.Unmarshal(raw, &entry) != nil || !beads.IsAgentBead(&beads.Issue{Type: entry.Type, Labels: entry.Labels}) {
			continue
		}
		seenIDs[entry.ID] = true
		result = append(result, raw)
	}
	return result
}

func mergeIssueAgentJSON(rawJSON []byte, seenIDs map[string]bool, result []json.RawMessage) []json.RawMessage {
	if len(rawJSON) == 0 {
		return result
	}
	var entries []json.RawMessage
	if json.Unmarshal(rawJSON, &entries) != nil {
		return result
	}
	for _, raw := range entries {
		var entry agentBeadJSONEntry
		if json.Unmarshal(raw, &entry) == nil && !seenIDs[entry.ID] {
			result = append(result, raw)
		}
	}
	return result
}

// checkGUPPViolations looks for agents that have work-on-hook but aren't
// progressing. This is a GUPP violation: agents with hooked work must execute.
// The daemon detects these and notifies the relevant Witness for remediation.
func (d *Daemon) checkGUPPViolations() {
	// Check if any rigs are operational before querying agent beads
	rigs := getKnownRigs(d)
	hasOperationalRig := false
	for _, rigName := range rigs {
		if operational, _ := isRigOperational(d, rigName); operational {
			hasOperationalRig = true
			break
		}
	}

	// Skip entirely if no rigs are operational (all docked/parked)
	if !hasOperationalRig {
		return
	}

	for _, rigName := range rigs {
		d.checkRigGUPPViolations(rigName)
	}
}

// checkRigGUPPViolations checks polecats in a specific rig for GUPP violations.
func (d *Daemon) checkRigGUPPViolations(rigName string) {
	// List polecat agent beads for this rig (issues + wisps tables)
	// Pattern: <prefix>-<rig>-polecat-<name> (e.g., gt-gastown-polecat-Toast)
	var agents []struct {
		ID          string   `json:"id"`
		Description string   `json:"description"`
		UpdatedAt   string   `json:"updated_at"`
		HookBead    string   `json:"hook_bead"` // Read from database column, not description
		AgentState  string   `json:"agent_state"`
		Labels      []string `json:"labels"`
		Type        string   `json:"issue_type"`
	}

	if err := d.listAgentBeadsJSON(&agents); err != nil {
		// Suppress warning when there are simply no agent beads (expected when all rigs are docked)
		d.logger.Printf("Warning: listing agent beads failed for GUPP check: %v", err)
		return
	}

	// Use the rig's configured prefix (e.g., "gt" for gastown, "bd" for beads)
	rigPrefix := config.GetRigPrefix(d.config.TownRoot, rigName)
	// Pattern: <prefix>-<rig>-polecat-<name>
	prefix := rigPrefix + "-" + rigName + "-polecat-"
	for _, agent := range agents {
		// Only check polecats for this rig
		if !strings.HasPrefix(agent.ID, prefix) {
			continue
		}

		// Check if agent has work on hook
		// Use HookBead from database column directly (not parsed from description)
		if agent.HookBead == "" {
			continue // No hooked work - no GUPP violation possible
		}

		agentState := beads.ResolveAgentState(agent.Description, agent.AgentState)

		// Skip nuked agents — they're intentionally terminated and should not
		// trigger alerts even if stale hook_bead data remains in the database.
		if beads.AgentState(agentState) == beads.AgentStateNuked {
			continue
		}

		// Per gt-zecmc: derive running state from tmux, not agent_state
		// Extract polecat name from agent ID (<prefix>-<rig>-polecat-<name> -> <name>)
		polecatName := strings.TrimPrefix(agent.ID, prefix)
		sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)

		// Check if tmux session exists and agent is running
		if d.tmux.IsAgentAlive(sessionName) {
			// Session is alive - check if it's been stuck too long
			updatedAt, err := time.Parse(time.RFC3339, agent.UpdatedAt)
			if err != nil {
				continue
			}

			age := time.Since(updatedAt)
			if age > GUPPViolationTimeout {
				d.logger.Printf("GUPP violation: agent %s has hook_bead=%s but hasn't updated in %v (timeout: %v)",
					agent.ID, agent.HookBead, age.Round(time.Minute), GUPPViolationTimeout)

				// Notify the witness for this rig
				d.notifyWitnessOfGUPP(rigName, agent.ID, agent.HookBead, age)
			}
		}
	}
}

// notifyWitnessOfGUPP sends a mail to the rig's witness about a GUPP violation.
func (d *Daemon) notifyWitnessOfGUPP(rigName, agentID, hookBead string, stuckDuration time.Duration) {
	witnessAddr := rigName + "/witness"
	subject := fmt.Sprintf("GUPP_VIOLATION: %s stuck for %v", agentID, stuckDuration.Round(time.Minute))
	body := fmt.Sprintf(`Agent %s has work on hook but isn't progressing.

hook_bead: %s
stuck_duration: %v

Action needed: Check if agent is alive and responsive. Consider restarting if stuck.`,
		agentID, hookBead, stuckDuration.Round(time.Minute))

	cmd := exec.Command(d.gtPath, "mail", "send", witnessAddr, "-s", subject, "-m", body)
	cmd.Dir = d.config.TownRoot
	cmd.Env = os.Environ() // Inherit PATH to find gt executable
	util.SetDetachedProcessGroup(cmd)

	if err := cmd.Run(); err != nil {
		d.logger.Printf("Warning: failed to notify witness of GUPP violation: %v", err)
	} else {
		d.logger.Printf("Notified %s of GUPP violation for %s", witnessAddr, agentID)
	}
}

// checkOrphanedWork looks for work assigned to dead agents.
// Orphaned work needs to be reassigned or the agent needs to be restarted.
// Per gt-zecmc: derive agent liveness from tmux, not agent_state.
func (d *Daemon) checkOrphanedWork() {
	// Check if any rigs are operational before querying agent beads
	rigs := getKnownRigs(d)
	hasOperationalRig := false
	for _, rigName := range rigs {
		if operational, _ := isRigOperational(d, rigName); operational {
			hasOperationalRig = true
			break
		}
	}

	// Skip entirely if no rigs are operational (all docked/parked)
	if !hasOperationalRig {
		return
	}

	for _, rigName := range rigs {
		d.checkRigOrphanedWork(rigName)
	}
}

// checkRigOrphanedWork checks polecats in a specific rig for orphaned work.
func (d *Daemon) checkRigOrphanedWork(rigName string) {
	// List polecat agent beads (issues + wisps tables)
	var agents []struct {
		ID          string   `json:"id"`
		HookBead    string   `json:"hook_bead"`
		AgentState  string   `json:"agent_state"`
		Description string   `json:"description"`
		Labels      []string `json:"labels"`
		Type        string   `json:"issue_type"`
	}

	if err := d.listAgentBeadsJSON(&agents); err != nil {
		d.logger.Printf("Warning: listing agent beads failed for orphaned work check: %v", err)
		return
	}

	// Use the rig's configured prefix (e.g., "gt" for gastown, "bd" for beads)
	rigPrefix := config.GetRigPrefix(d.config.TownRoot, rigName)
	// Pattern: <prefix>-<rig>-polecat-<name>
	prefix := rigPrefix + "-" + rigName + "-polecat-"
	for _, agent := range agents {
		// Only check polecats for this rig
		if !strings.HasPrefix(agent.ID, prefix) {
			continue
		}

		// No hooked work = nothing to orphan
		if agent.HookBead == "" {
			continue
		}

		agentState := beads.ResolveAgentState(agent.Description, agent.AgentState)

		// Skip nuked agents — they're intentionally terminated and should not
		// trigger alerts even if stale hook_bead data remains in the database.
		if beads.AgentState(agentState) == beads.AgentStateNuked {
			continue
		}

		// Check if tmux session is alive (derive state from tmux, not bead)
		polecatName := strings.TrimPrefix(agent.ID, prefix)
		sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)

		// Session running = not orphaned (work is being processed)
		if d.tmux.IsAgentAlive(sessionName) {
			continue
		}

		// TOCTOU guard: re-verify agent state before taking action.
		// Between the bd list above and now, the agent may have been
		// restarted or its hook_bead cleared. Re-check both conditions.
		if d.tmux.IsAgentAlive(sessionName) {
			continue
		}
		currentHookBead := d.getAgentHookBead(agent.ID)
		if currentHookBead == "" {
			continue
		}

		// Session dead but has hooked work = orphaned!
		d.logger.Printf("Orphaned work detected: agent %s session is dead but has hook_bead=%s",
			agent.ID, currentHookBead)

		d.notifyWitnessOfOrphanedWork(rigName, agent.ID, currentHookBead)
	}
}

// extractRigFromAgentID extracts the rig name from a polecat agent ID.
// Example: gt-gastown-polecat-max → gastown
func (d *Daemon) extractRigFromAgentID(agentID string) string {
	// Use the beads package helper to correctly parse agent bead IDs.
	// Pattern: <prefix>-<rig>-polecat-<name> (e.g., gt-gastown-polecat-Toast)
	rig, role, _, ok := beads.ParseAgentBeadID(agentID)
	if !ok || role != constants.RolePolecat {
		return ""
	}
	return rig
}

// notifyWitnessOfOrphanedWork sends a mail to the rig's witness about orphaned work.
func (d *Daemon) notifyWitnessOfOrphanedWork(rigName, agentID, hookBead string) {
	witnessAddr := rigName + "/witness"
	subject := fmt.Sprintf("ORPHANED_WORK: %s has hooked work but is dead", agentID)
	body := fmt.Sprintf(`Agent %s is dead but has work on its hook.

hook_bead: %s

Action needed: Either restart the agent or reassign the work.`,
		agentID, hookBead)

	cmd := exec.Command(d.gtPath, "mail", "send", witnessAddr, "-s", subject, "-m", body)
	cmd.Dir = d.config.TownRoot
	cmd.Env = os.Environ() // Inherit PATH to find gt executable
	util.SetDetachedProcessGroup(cmd)

	if err := cmd.Run(); err != nil {
		d.logger.Printf("Warning: failed to notify witness of orphaned work: %v", err)
	} else {
		d.logger.Printf("Notified %s of orphaned work for %s", witnessAddr, agentID)
	}
}
