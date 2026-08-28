package witness

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/nudge"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
)

// Common errors
var (
	ErrNotRunning     = errors.New("witness not running")
	ErrAlreadyRunning = errors.New("witness already running")
)

// tmuxOps is the tmux seam for witness start and stop. Production uses *tmux.Tmux.
type tmuxOps interface {
	session.TmuxOps
	GetSessionInfo(_ string) (*tmux.SessionInfo, error)
	GetSessionCreatedUnix(_ string) (int64, error)
}

// Manager handles witness lifecycle and monitoring operations.
// ZFC-compliant: tmux session is the source of truth for running state.
type Manager struct {
	rig  *rig.Rig
	tmux tmuxOps
}

type witnessStartConfig struct {
	witnessDir       string
	runtimeConfigDir string
	command          string
	extraEnv         map[string]string
	beacon           session.BeaconConfig
}

// NewManager creates a new witness manager for a rig.
func NewManager(r *rig.Rig) *Manager {
	return &Manager{
		rig:  r,
		tmux: tmux.NewTmux(),
	}
}

// IsRunning checks if the witness session is active and healthy.
// Checks both tmux session existence AND agent process liveness to avoid
// reporting zombie sessions (tmux alive but Claude dead) as "running".
// ZFC: tmux session existence is the source of truth for session state,
// but agent liveness determines if the session is actually functional.
func (m *Manager) IsRunning() (bool, error) {
	return m.tmux.CheckSessionHealth(m.SessionName(), 0) == tmux.SessionHealthy, nil
}

// IsHealthy checks if the witness is running and has been active recently.
// Unlike IsRunning which only checks process liveness, this also detects hung
// sessions where Claude is alive but hasn't produced output in maxInactivity.
// Returns the detailed ZombieStatus for callers that need to distinguish
// between different failure modes.
func (m *Manager) IsHealthy(maxInactivity time.Duration) tmux.ZombieStatus {
	return m.tmux.CheckSessionHealth(m.SessionName(), maxInactivity)
}

// SessionName returns the tmux session name for this witness.
func (m *Manager) SessionName() string {
	return session.WitnessSessionName(session.PrefixFor(m.rig.Name))
}

// Status returns information about the witness session.
// ZFC-compliant: tmux session is the source of truth.
func (m *Manager) Status() (*tmux.SessionInfo, error) {
	t := m.tmux
	sessionID := m.SessionName()

	running, err := t.HasSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return nil, ErrNotRunning
	}

	return t.GetSessionInfo(sessionID)
}

// witnessDir returns the working directory for the witness.
// Prefers witness/rig/ for existing legacy clones, otherwise uses witness/.
func (m *Manager) witnessDir() string {
	witnessRigDir := filepath.Join(m.rig.Path, "witness", "rig")
	if _, err := os.Stat(witnessRigDir); err == nil {
		return witnessRigDir
	}

	return filepath.Join(m.rig.Path, "witness")
}

func (m *Manager) prepareWitnessDir(townRoot string) (string, error) {
	witnessDir := m.witnessDir()
	if err := os.MkdirAll(witnessDir, 0755); err != nil {
		return "", fmt.Errorf("creating witness dir: %w", err)
	}
	if err := beads.SetupRedirect(townRoot, witnessDir); err != nil {
		return "", fmt.Errorf("ensuring witness beads redirect: %w", err)
	}
	return witnessDir, nil
}

// Start starts the witness.
// If foreground is true, returns an error (foreground mode deprecated).
// Otherwise, spawns a Claude agent in a tmux session.
// agentOverride optionally specifies a different agent alias to use.
// envOverrides are KEY=VALUE pairs that override all other env var sources.
// ZFC-compliant: no state file, tmux session is source of truth.
func (m *Manager) Start(foreground bool, agentOverride string, envOverrides []string) error {
	t := m.tmux
	sessionID := m.SessionName()
	townRoot := m.townRoot()

	if foreground {
		// Foreground mode is deprecated - patrol logic moved to mol-witness-patrol
		return fmt.Errorf("foreground mode is deprecated; use background mode (remove --foreground flag)")
	}

	if err := m.ensureWitnessSessionAvailable(t, townRoot, sessionID); err != nil {
		return err
	}

	startConfig, err := m.prepareWitnessStart(townRoot, sessionID, agentOverride, envOverrides)
	if err != nil {
		return err
	}
	result, err := session.StartSession(t, "witness", session.Work{
		SessionID:        sessionID,
		WorkDir:          startConfig.witnessDir,
		TownRoot:         townRoot,
		RigPath:          m.rig.Path,
		RigName:          m.rig.Name,
		AgentName:        "witness",
		AgentOverride:    agentOverride,
		Command:          startConfig.command,
		RuntimeConfigDir: startConfig.runtimeConfigDir,
		ExtraEnv:         startConfig.extraEnv,
		Theme:            tmux.ResolveSessionTheme(townRoot, m.rig.Name, "witness", ""),
		Beacon:           startConfig.beacon,
		Instructions:     "Run `gt prime --hook` and begin patrol.",
	})
	if err != nil {
		return err
	}

	if _, pollerErr := nudge.StartPoller(townRoot, sessionID); pollerErr != nil {
		log.Printf("warning: could not start nudge poller for %s: %v", sessionID, pollerErr)
	}

	if real, ok := t.(*tmux.Tmux); ok {
		_ = runtime.RunStartupFallback(real, sessionID, "witness", result.RuntimeConfig)
		initialPrompt := session.BuildStartupPrompt(startConfig.beacon, "Run `gt prime --hook` and begin patrol.")
		_ = runtime.DeliverStartupPromptFallback(real, sessionID, initialPrompt, result.RuntimeConfig, constants.ClaudeStartTimeout)
	}

	time.Sleep(constants.ShutdownNotifyDelay)

	return nil
}

func (m *Manager) ensureWitnessSessionAvailable(t tmuxOps, townRoot, sessionID string) error {
	running, _ := t.HasSession(sessionID)
	if !running {
		return nil
	}
	if t.IsAgentAlive(sessionID) {
		return ErrAlreadyRunning
	}

	createdAt, _ := t.GetSessionCreatedUnix(sessionID)
	time.Sleep(constants.ZombieKillGracePeriod)
	if t.IsAgentAlive(sessionID) {
		return ErrAlreadyRunning
	}
	if createdNow, _ := t.GetSessionCreatedUnix(sessionID); createdAt > 0 && createdNow != createdAt {
		return ErrAlreadyRunning
	}
	if err := session.StopSession(t, townRoot, sessionID, false); err != nil && !errors.Is(err, session.ErrNotFound) {
		return fmt.Errorf("killing zombie session: %w", err)
	}
	return nil
}

func (m *Manager) prepareWitnessStart(townRoot, sessionID, agentOverride string, envOverrides []string) (witnessStartConfig, error) {
	witnessDir, err := m.prepareWitnessDir(townRoot)
	if err != nil {
		return witnessStartConfig{}, err
	}
	if err := rig.Provision(m.rig.Path, witnessDir, "witness"); err != nil {
		style.PrintWarning("could not provision witness workspace: %v", err)
	}

	roleConfig := m.loadWitnessRoleConfig()
	runtimeConfigDir := resolveWitnessRuntimeConfig(townRoot)
	command, err := buildWitnessStartCommand(m.rig.Path, m.rig.Name, townRoot, sessionID, agentOverride, roleConfig, runtimeConfigDir)
	if err != nil {
		return witnessStartConfig{}, err
	}
	beacon := session.BeaconConfig{
		Recipient: session.BeaconRecipient("witness", "", m.rig.Name),
		Sender:    "deacon",
		Topic:     "patrol",
	}
	return witnessStartConfig{
		witnessDir:       witnessDir,
		runtimeConfigDir: runtimeConfigDir,
		command:          command,
		extraEnv:         witnessExtraEnv(roleConfig, townRoot, runtimeConfigDir, sessionID, agentOverride, m.rig.Name, envOverrides),
		beacon:           beacon,
	}, nil
}

func (m *Manager) loadWitnessRoleConfig() *beads.RoleConfig {
	roleConfig, err := m.roleConfig()
	if err != nil {
		log.Printf("warning: could not load witness role config for %s: %v", m.rig.Name, err)
		return nil
	}
	return roleConfig
}

func resolveWitnessRuntimeConfig(townRoot string) string {
	accountsPath := constants.MayorAccountsPath(townRoot)
	runtimeConfigDir, _, _ := config.ResolveAccountConfigDir(accountsPath, "")
	if runtimeConfigDir == "" {
		runtimeConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	return runtimeConfigDir
}

func witnessExtraEnv(roleConfig *beads.RoleConfig, townRoot, runtimeConfigDir, sessionID, agentOverride, rigName string, envOverrides []string) map[string]string {
	extraEnv := roleConfigEnvVars(roleConfig, townRoot, rigName)
	if extraEnv == nil {
		extraEnv = map[string]string{}
	}
	stdEnv := config.AgentEnv(config.AgentEnvConfig{
		Role:             "witness",
		Rig:              rigName,
		TownRoot:         townRoot,
		RuntimeConfigDir: runtimeConfigDir,
		Agent:            agentOverride,
		SessionName:      sessionID,
	})
	for key := range stdEnv {
		delete(extraEnv, key)
	}
	for _, override := range envOverrides {
		if key, value, ok := strings.Cut(override, "="); ok {
			extraEnv[key] = value
		}
	}
	return extraEnv
}

func (m *Manager) roleConfig() (*beads.RoleConfig, error) {
	townRoot := m.townRoot()
	roleDef, err := config.LoadRoleDefinition(townRoot, m.rig.Path, "witness")
	if err != nil {
		return nil, fmt.Errorf("loading witness role config: %w", err)
	}
	return &beads.RoleConfig{
		SessionPattern: roleDef.Session.Pattern,
		WorkDirPattern: roleDef.Session.WorkDir,
		NeedsPreSync:   roleDef.Session.NeedsPreSync,
		StartCommand:   roleDef.Session.StartCommand,
		EnvVars:        roleDef.Env,
	}, nil
}

func (m *Manager) townRoot() string {
	townRoot, err := workspace.Find(m.rig.Path)
	if err != nil || townRoot == "" {
		return m.rig.Path
	}
	return townRoot
}

func roleConfigEnvVars(roleConfig *beads.RoleConfig, townRoot, rigName string) map[string]string {
	if roleConfig == nil || len(roleConfig.EnvVars) == 0 {
		return nil
	}
	expanded := make(map[string]string, len(roleConfig.EnvVars))
	for key, value := range roleConfig.EnvVars {
		expanded[key] = beads.ExpandRolePattern(value, townRoot, rigName, "", "witness", session.PrefixFor(rigName))
	}
	return expanded
}

func buildWitnessStartCommand(rigPath, rigName, townRoot, sessionName, agentOverride string, roleConfig *beads.RoleConfig, runtimeConfigDir string) (string, error) {
	if agentOverride != "" {
		roleConfig = nil
	}
	if roleConfig != nil && roleConfig.StartCommand != "" {
		rc := config.ResolveRoleAgentConfig("witness", townRoot, rigPath)
		if !config.IsResolvedAgentClaude(rc) {
			// Non-Claude agent: skip TOML start_command entirely.
			// Built-in role TOMLs hardcode "exec claude ..." which is wrong
			// for non-Claude agents. Fall through to BuildStartupCommandFromConfig
			// which uses the resolved agent's command and args.
		} else if !isBuiltinClaudeStartCommand(roleConfig.StartCommand) && !config.HasExplicitRoleAgent("witness", townRoot, rigPath) {
			// Custom (non-builtin) start_command with Claude agent and no explicit
			// role_agents mapping: use TOML pattern with template expansion.
			cmd := beads.ExpandRolePattern(roleConfig.StartCommand, townRoot, rigName, "", "witness", session.PrefixFor(rigName))
			if strings.HasPrefix(cmd, "exec ") {
				cmd = "exec env -u CLAUDECODE NODE_OPTIONS='' " + strings.TrimPrefix(cmd, "exec ")
			} else {
				cmd = "env -u CLAUDECODE NODE_OPTIONS='' " + cmd
			}
			return cmd, nil
		}
		// Non-Claude agent OR Claude with built-in start_command: fall
		// through to BuildStartupCommandFromConfig for proper agent and
		// model flag resolution.
	}
	initialPrompt := session.BuildStartupPrompt(session.BeaconConfig{
		Recipient: session.BeaconRecipient("witness", "", rigName),
		Sender:    "deacon",
		Topic:     "patrol",
	}, "Run `gt prime --hook` and begin patrol.")
	command, err := config.BuildStartupCommandFromConfig(config.AgentEnvConfig{
		Role:             "witness",
		Rig:              rigName,
		TownRoot:         townRoot,
		RuntimeConfigDir: runtimeConfigDir,
		Prompt:           initialPrompt,
		Topic:            "patrol",
		SessionName:      sessionName,
	}, rigPath, initialPrompt, agentOverride)
	if err != nil {
		return "", fmt.Errorf("building startup command: %w", err)
	}
	return command, nil
}

// isBuiltinClaudeStartCommand returns true if the start_command is the
// built-in default from role TOMLs ("exec claude --dangerously-skip-permissions").
// Custom start_commands (e.g., "exec run --town {town}") return false.
func isBuiltinClaudeStartCommand(cmd string) bool {
	trimmed := strings.TrimPrefix(cmd, "exec ")
	return trimmed == "claude --dangerously-skip-permissions"
}

// Stop stops the witness.
// ZFC-compliant: tmux session is the source of truth.
func (m *Manager) Stop() error {
	err := session.StopSession(m.tmux, m.townRoot(), m.SessionName(), true)
	if errors.Is(err, session.ErrNotFound) {
		return ErrNotRunning
	}
	return err
}
