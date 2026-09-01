// Package session provides Worker session start and stop.
package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/nudge"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/skills"
	"github.com/jonbaldie/gastown/internal/telemetry"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/worker"
)

// Work is the caller-supplied identity for a Worker session.
// Start policy (waits, bypass, respawn) is not part of Work; StartSession
// reads it from the role table.
type Work struct {
	// SessionID is the tmux session name (e.g., "gt-wyvern-Toast", "hq-mayor").
	SessionID string

	// WorkDir is the working directory for the session.
	WorkDir string

	// TownRoot is the root of the Gas Town workspace (e.g., ~/gt).
	TownRoot string

	// RigPath is the rig directory path for config resolution.
	// Empty for town-level agents (mayor, deacon, boot).
	RigPath string

	// RigName is the rig name for environment variables and theming.
	// Empty for town-level agents.
	RigName string

	// AgentName is the specific agent name within a rig.
	// Used for polecats, crew, and dogs. Empty for singletons.
	AgentName string

	// Command is a pre-built startup command. If non-empty, skips command building.
	// If empty, the command is built from Beacon + config.BuildAgentStartupCommand.
	Command string

	// Beacon configures the startup beacon message for session identification.
	// Ignored if Command is non-empty.
	Beacon BeaconConfig

	// Instructions are appended after the beacon in the startup prompt.
	// Used by roles like Boot and Deacon that need explicit instructions.
	// Ignored if Command is non-empty.
	Instructions string

	// AgentOverride optionally specifies a different agent alias (e.g., "opencode").
	AgentOverride string

	// RuntimeConfigDir overrides the config directory for the runtime.
	RuntimeConfigDir string

	// ExtraEnv adds additional environment variables beyond the standard AgentEnv set.
	// These are set in the tmux session environment after the standard vars.
	ExtraEnv map[string]string

	// Theme is the tmux theme to apply. Nil means no theme is applied.
	Theme *tmux.Theme

	// Interactive marks an attended crew start. The role table then skips
	// unattended waits and dialog dismissal.
	Interactive bool

	// SkipReady treats session creation as the ready state. Used by gt now so
	// attach does not wait for a runtime prompt or the Cursor ready delay.
	SkipReady bool
}

// StartResult contains the results of session startup.
type StartResult struct {
	// RuntimeConfig is the resolved runtime config for the role.
	// Callers may need this for role-specific post-startup steps
	// (e.g., handling fallback nudges, legacy fallback).
	RuntimeConfig *config.RuntimeConfig

	// RunID is the GASTA run identifier (GT_RUN) generated for this session.
	// All telemetry events emitted within the session carry this ID, enabling
	// waterfall correlation across prompts, BD calls, mail operations, and
	// agent conversation events.
	RunID string
}

// StartSession creates a tmux session following the standard Gas Town lifecycle.
// The caller supplies the Role and the Work. Start policy is read from the
// role table inside this module.
func StartSession(t TmuxOps, role string, work Work) (_ *StartResult, retErr error) {
	runID := uuid.New().String()
	ctx := telemetry.WithRunID(context.Background(), runID)
	defer func() { telemetry.RecordSessionStart(ctx, work.SessionID, role, retErr) }()
	start := sessionStart{tmux: t, role: role, work: work, runID: runID, ctx: ctx}
	if err := start.run(); err != nil {
		return nil, err
	}
	return &StartResult{RuntimeConfig: start.runtimeConfig, RunID: runID}, nil
}

type sessionStart struct {
	tmux          TmuxOps
	role          string
	work          Work
	policy        rolePolicy
	runtimeConfig *config.RuntimeConfig
	runID         string
	ctx           context.Context
	envVars       map[string]string
}

func (s *sessionStart) run() error {
	if err := validateStart(s.role, s.work); err != nil {
		return err
	}
	s.policy = policyFor(s.role, s.work)
	if err := prepareSessionStart(s); err != nil {
		return err
	}
	command, err := sessionStartCommand(s)
	if err != nil {
		return err
	}
	s.envVars = sessionStartEnvironment(s)
	if err := startSessionCommand(s.tmux, s.work, command, s.envVars); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	if err := configureAndWaitForSession(s); err != nil {
		return err
	}
	if err := recordAndWaitReady(s); err != nil {
		return err
	}
	if err := verifyStartedSession(s); err != nil {
		return err
	}
	finishSessionStart(s)
	return nil
}

func validateStart(role string, work Work) error {
	switch {
	case work.SessionID == "":
		return fmt.Errorf("SessionID is required")
	case work.WorkDir == "":
		return fmt.Errorf("WorkDir is required")
	case role == "":
		return fmt.Errorf("Role is required")
	default:
		return nil
	}
}

func prepareSessionStart(s *sessionStart) error {
	var err error
	s.runtimeConfig, err = resolveRuntimeConfig(s.role, s.work)
	if err != nil {
		return err
	}
	settingsDir := config.RoleSettingsDir(s.role, s.work.RigPath)
	if settingsDir == "" {
		settingsDir = s.work.WorkDir
	}
	if err := ensureStartRuntimeFiles(s, settingsDir); err != nil {
		return err
	}
	if s.work.RuntimeConfigDir != "" && !s.work.SkipReady {
		if err := skills.ProvisionUserDir(s.work.RuntimeConfigDir); err != nil {
			return fmt.Errorf("ensuring account skills: %w", err)
		}
	}
	return nil
}

func ensureStartRuntimeFiles(s *sessionStart, settingsDir string) error {
	if s.work.SkipReady {
		if err := runtime.EnsureHooksForRole(settingsDir, s.work.WorkDir, s.role, s.runtimeConfig); err != nil {
			return fmt.Errorf("ensuring runtime hooks: %w", err)
		}
		return nil
	}
	if err := runtime.EnsureSettingsForRole(settingsDir, s.work.WorkDir, s.role, s.runtimeConfig); err != nil {
		return fmt.Errorf("ensuring runtime settings: %w", err)
	}
	return nil
}

func sessionStartCommand(s *sessionStart) (string, error) {
	command := s.work.Command
	if command == "" {
		var err error
		command, err = buildCommand(s.role, s.work, buildPrompt(s.work))
		if err != nil {
			return "", fmt.Errorf("building startup command: %w", err)
		}
	}
	if s.runtimeConfig.Session != nil && s.runtimeConfig.Session.ConfigDirEnv != "" && s.work.RuntimeConfigDir != "" {
		command = config.PrependEnv(command, map[string]string{s.runtimeConfig.Session.ConfigDirEnv: s.work.RuntimeConfigDir})
	}
	return command, nil
}

func sessionStartEnvironment(s *sessionStart) map[string]string {
	envVars := config.AgentEnv(config.AgentEnvConfig{Role: s.role, Rig: s.work.RigName, AgentName: s.work.AgentName, TownRoot: s.work.TownRoot, RuntimeConfigDir: s.work.RuntimeConfigDir, Agent: s.work.AgentOverride, SessionName: s.work.SessionID})
	envVars = MergeRuntimeLivenessEnv(envVars, s.runtimeConfig)
	envVars["GT_RUN"] = s.runID
	for key, value := range s.work.ExtraEnv {
		envVars[key] = value
	}
	return envVars
}

func configureAndWaitForSession(s *sessionStart) error {
	if s.policy.RemainOnExit {
		_ = s.tmux.SetRemainOnExit(s.work.SessionID, true)
	}
	if s.work.Theme != nil {
		_ = s.tmux.ConfigureGasTownSession(s.work.SessionID, s.work.Theme, s.work.RigName, s.work.AgentName, s.role)
	}
	if err := waitForSessionCommand(s); err != nil {
		return err
	}
	if s.policy.AutoRespawn {
		if err := s.tmux.SetAutoRespawnHook(s.work.SessionID); err != nil {
			fmt.Printf("warning: failed to set auto-respawn hook for %s: %v\n", s.role, err)
		}
	}
	return acceptSessionStartupDialogs(s)
}

func waitForSessionCommand(s *sessionStart) error {
	if !s.policy.WaitForAgent {
		return nil
	}
	err := s.tmux.WaitForCommand(s.work.SessionID, constants.SupportedShells, constants.ClaudeStartTimeout)
	if err == nil || !s.policy.WaitFatal {
		return nil
	}
	_ = s.tmux.KillSessionWithProcesses(s.work.SessionID)
	return fmt.Errorf("waiting for %s to start: %w", s.role, err)
}

func acceptSessionStartupDialogs(s *sessionStart) error {
	if !s.policy.AcceptBypass {
		return nil
	}
	_ = s.tmux.AcceptStartupDialogs(s.work.SessionID)
	if err := s.tmux.CheckStartupBlocked(s.work.SessionID); err != nil {
		_ = s.tmux.KillSessionWithProcesses(s.work.SessionID)
		return fmt.Errorf("startup blocked: %w", err)
	}
	return nil
}

func recordAndWaitReady(s *sessionStart) error {
	if s.work.TownRoot == "" {
		return waitForSessionRuntime(s)
	}
	w, err := worker.Open(s.work.TownRoot)
	if err != nil {
		return waitForSessionRuntime(s)
	}
	recordSessionWorker(s, w)
	if !sessionNeedsReadyWait(s) {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(s.ctx, constants.ClaudeStartTimeout)
	err = w.WaitReady(waitCtx, s.runID)
	cancel()
	return handleSessionReadyError(s, err)
}

func recordSessionWorker(s *sessionStart, w *worker.Worker) {
	_, err := w.StartRun(s.ctx, worker.StartSpec{RunID: s.runID, SessionID: s.work.SessionID, BeadID: s.work.Beacon.MolID, Role: s.role, Rig: s.work.RigName, AgentName: s.work.AgentName, AgentType: s.runtimeConfig.ResolvedAgent})
	if err != nil && !errors.Is(err, worker.ErrLiveRun) {
		fmt.Fprintf(os.Stderr, "Warning: worker start run for %s: %v\n", s.work.SessionID, err)
		return
	}
	_ = w.PushIdentity(s.ctx, worker.Identity{RunID: s.runID, Role: s.role, Rig: s.work.RigName, AgentName: s.work.AgentName, SessionID: s.work.SessionID, Env: s.envVars})
	_ = w.PushContext(s.ctx, worker.ContextPush{RunID: s.runID, Sections: sessionContextSections(s), Mode: worker.ContextFull})
}

func sessionContextSections(s *sessionStart) []worker.ContextSection {
	sections := []worker.ContextSection{{Type: worker.SectionRole, Content: s.role}}
	if s.work.Beacon.MolID != "" {
		sections = append(sections, worker.ContextSection{Type: worker.SectionWork, Content: s.work.Beacon.MolID})
	}
	if s.work.Instructions != "" {
		sections = append(sections, worker.ContextSection{Type: worker.SectionDirective, Content: s.work.Instructions})
	}
	return sections
}

func sessionNeedsReadyWait(s *sessionStart) bool {
	return s.policy.ReadyDelay || s.policy.ReadyFatal
}

func waitForSessionRuntime(s *sessionStart) error {
	if !sessionNeedsReadyWait(s) {
		return nil
	}
	err := s.tmux.WaitForRuntimeReady(s.work.SessionID, s.runtimeConfig, constants.ClaudeStartTimeout)
	return handleSessionReadyError(s, err)
}

func handleSessionReadyError(s *sessionStart, err error) error {
	if err == nil {
		return nil
	}
	if s.policy.ReadyFatal {
		_ = s.tmux.KillSessionWithProcesses(s.work.SessionID)
		return fmt.Errorf("waiting for %s to become ready: %w", s.role, err)
	}
	fmt.Fprintf(os.Stderr, "Warning: agent readiness detection timed out for %s: %v\n", s.work.SessionID, err)
	return nil
}

func verifyStartedSession(s *sessionStart) error {
	if !s.policy.VerifySurvived {
		return nil
	}
	running, err := s.tmux.HasSession(s.work.SessionID)
	if err != nil {
		return killStartedSessionWithError(s, fmt.Errorf("verifying session: %w", err))
	}
	if !running {
		return fmt.Errorf("session %s died during startup (agent command may have failed)", s.work.SessionID)
	}
	if err := s.tmux.CheckStartupBlocked(s.work.SessionID); err != nil {
		return killStartedSessionWithError(s, fmt.Errorf("startup blocked: %w", err))
	}
	if status := s.tmux.CheckSessionHealth(s.work.SessionID, 0); status != tmux.SessionHealthy {
		return killStartedSessionWithError(s, fmt.Errorf("session %s unhealthy during startup: %s", s.work.SessionID, status))
	}
	return nil
}

func killStartedSessionWithError(s *sessionStart, err error) error {
	_ = s.tmux.KillSessionWithProcesses(s.work.SessionID)
	return err
}

func finishSessionStart(s *sessionStart) {
	if paneID, err := s.tmux.GetPaneID(s.work.SessionID); err == nil {
		_ = s.tmux.SetEnvironment(s.work.SessionID, "GT_PANE_ID", paneID)
	}
	if s.policy.TrackPID && s.work.TownRoot != "" {
		_ = TrackSessionPID(s.work.TownRoot, s.work.SessionID, s.tmux)
	}
	activateSessionLogging(s)
	RecordAgentInstantiateFromDir(s.ctx, telemetry.AgentInstantiateInfo{RunID: s.runID, AgentType: s.runtimeConfig.ResolvedAgent, Role: s.role, AgentName: s.work.AgentName, SessionID: s.work.SessionID, RigName: s.work.RigName, TownRoot: s.work.TownRoot, IssueID: s.work.Beacon.MolID}, s.work.WorkDir)
}

func activateSessionLogging(s *sessionStart) {
	if os.Getenv("GT_LOG_AGENT_OUTPUT") != "true" || os.Getenv("GT_OTEL_LOGS_URL") == "" {
		return
	}
	if err := ActivateAgentLogging(s.work.SessionID, s.work.WorkDir, s.runID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: agent log watcher setup failed for %s: %v\n", s.work.SessionID, err)
	}
}

type sessionNoWait interface {
	NewSessionWithCommandAndEnvNoWait(_, _, _ string, _ map[string]string) error
}

func startSessionCommand(tm TmuxOps, work Work, command string, envVars map[string]string) error {
	if work.SkipReady {
		if n, ok := tm.(sessionNoWait); ok {
			return n.NewSessionWithCommandAndEnvNoWait(work.SessionID, work.WorkDir, command, envVars)
		}
	}
	return tm.NewSessionWithCommandAndEnv(work.SessionID, work.WorkDir, command, envVars)
}

// RecordAgentInstantiateFromDir resolves the git branch/commit from workDir and
// emits the agent.instantiate root telemetry event. AgentType defaults to
// "claudecode" when empty. Use this instead of calling telemetry.RecordAgentInstantiate
// directly to avoid duplicating the git-lookup boilerplate.
func RecordAgentInstantiateFromDir(ctx context.Context, info telemetry.AgentInstantiateInfo, workDir string) {
	if info.AgentType == "" {
		info.AgentType = "claudecode"
	}
	branch, commit := "", ""
	if g := git.NewGit(workDir); g != nil {
		if b, err := git.CurrentBranch(g); err == nil {
			branch = b
		}
		if c, err := git.Rev(g, "HEAD"); err == nil {
			commit = c
		}
	}
	info.GitBranch = branch
	info.GitCommit = commit
	telemetry.RecordAgentInstantiate(ctx, info)
}

// ErrNotFound is returned by StopSession when the tmux session is already gone.
var ErrNotFound = errors.New("session not found")

// StopSession is the single Worker stop path. It stops the nudge poller,
// marks the Worker run stopped, stops the agent-log watcher, and kills the
// tmux session. Role managers call this instead of writing their own stop body.
func StopSession(t TmuxOps, townRoot, sessionID string, graceful bool) error {
	stopSessionPoller(townRoot, sessionID)
	running, err := t.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	stopErr := stopRunningSession(t, sessionID, running, graceful)
	stopErr = recordStoppedSession(townRoot, sessionID, stopErr)
	if stopErr != nil {
		return stopErr
	}
	if !running {
		return fmt.Errorf("%w: %s", ErrNotFound, sessionID)
	}
	return nil
}

func stopSessionPoller(townRoot, sessionID string) {
	if townRoot == "" {
		return
	}
	if err := nudge.StopPoller(townRoot, sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not stop nudge poller for %s: %v\n", sessionID, err)
	}
}

func stopRunningSession(t TmuxOps, sessionID string, running, graceful bool) error {
	if !running {
		return nil
	}
	if graceful {
		_ = t.SendKeysRaw(sessionID, "C-c")
		WaitForSessionExit(t, sessionID, constants.GracefulShutdownTimeout)
	}
	DeactivateAgentLogging(sessionID)
	if err := t.KillSessionWithProcesses(sessionID); err != nil {
		return fmt.Errorf("killing session: %w", err)
	}
	return nil
}

func recordStoppedSession(townRoot, sessionID string, stopErr error) error {
	if townRoot == "" || stopErr != nil {
		return stopErr
	}
	if err := worker.MarkSessionStopped(townRoot, sessionID); err != nil {
		return fmt.Errorf("recording stopped worker run: %w", err)
	}
	return nil
}

func mapKeysSorted(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// MergeRuntimeLivenessEnv ensures liveness-critical env vars are present in the
// tmux session environment table, even when agent resolution came from
// workspace/default settings rather than an explicit --agent override.
//
// Call this after config.AgentEnv() to add GT_AGENT and GT_PROCESS_NAMES
// before writing env vars to the tmux session via SetEnvironment.
func MergeRuntimeLivenessEnv(envVars map[string]string, runtimeConfig *config.RuntimeConfig) map[string]string {
	if envVars == nil {
		envVars = make(map[string]string)
	}
	if runtimeConfig == nil {
		return envVars
	}

	if _, hasGTAgent := envVars["GT_AGENT"]; !hasGTAgent && runtimeConfig.ResolvedAgent != "" {
		envVars["GT_AGENT"] = runtimeConfig.ResolvedAgent
	}

	if _, exists := envVars["GT_PROCESS_NAMES"]; !exists {
		setRuntimeProcessNames(envVars, runtimeConfig)
	}
	return envVars
}

func setRuntimeProcessNames(envVars map[string]string, runtimeConfig *config.RuntimeConfig) {
	agent, command, args := runtimeProcessLookup(envVars["GT_AGENT"], runtimeConfig)
	processNames := config.ResolveProcessNames(agent, command, args...)
	if len(processNames) > 0 {
		envVars["GT_PROCESS_NAMES"] = strings.Join(processNames, ",")
	}
}

func runtimeProcessLookup(existingAgent string, runtimeConfig *config.RuntimeConfig) (string, string, []string) {
	if existingAgent == "" || existingAgent == runtimeConfig.ResolvedAgent {
		return runtimeConfig.ResolvedAgent, runtimeConfig.Command, runtimeConfig.Args
	}
	return existingAgent, "", nil
}

// ErrSessionAlive is returned by KillExistingSession when checkAlive is true
// and the tmux session still has a live agent.
var ErrSessionAlive = errors.New("session already running")

// KillExistingSession kills an existing session if one is found.
// Returns true if a session was killed.
//
// If checkAlive is true, only kills zombie sessions (tmux alive but agent dead).
// If the session exists and the agent is alive, returns ErrSessionAlive.
// If checkAlive is false, kills any existing session unconditionally.
// The kill goes through StopSession so the nudge poller, worker run, and
// log watcher are torn down before the next start.
func KillExistingSession(t TmuxOps, townRoot, sessionID string, checkAlive bool) (bool, error) {
	running, err := t.HasSession(sessionID)
	if err != nil {
		return false, fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return false, nil
	}

	if checkAlive && t.IsAgentAlive(sessionID) {
		return false, fmt.Errorf("%w: %s", ErrSessionAlive, sessionID)
	}

	if err := StopSession(t, townRoot, sessionID, false); err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	return true, nil
}

// resolveRuntimeConfig chooses the agent config for this session.
// An explicit AgentOverride wins. Crew members without an override use
// per-worker resolution. Every other role uses the role default.
func resolveRuntimeConfig(role string, work Work) (*config.RuntimeConfig, error) {
	if work.AgentOverride != "" {
		rc, _, err := config.ResolveAgentConfigWithOverride(work.TownRoot, work.RigPath, work.AgentOverride)
		if err != nil {
			return nil, fmt.Errorf("resolving agent config for %s: %w", work.AgentOverride, err)
		}
		return rc, nil
	}
	if role == constants.RoleCrew && work.AgentName != "" {
		return config.ResolveWorkerAgentConfig(work.AgentName, work.TownRoot, work.RigPath), nil
	}
	return config.ResolveRoleAgentConfig(role, work.TownRoot, work.RigPath), nil
}

// buildPrompt creates the startup prompt from beacon + instructions.
func buildPrompt(work Work) string {
	if work.Instructions != "" {
		return BuildStartupPrompt(work.Beacon, work.Instructions)
	}
	return FormatStartupBeacon(work.Beacon)
}

// buildCommand creates the startup command using the config package.
func buildCommand(role string, work Work, prompt string) (string, error) {
	return config.BuildStartupCommandFromConfig(config.AgentEnvConfig{
		Role:             role,
		Rig:              work.RigName,
		AgentName:        work.AgentName,
		TownRoot:         work.TownRoot,
		RuntimeConfigDir: work.RuntimeConfigDir,
		Agent:            work.AgentOverride,
		Prompt:           prompt,
		Issue:            work.Beacon.MolID,
		Topic:            work.Beacon.Topic,
		SessionName:      work.SessionID,
	}, work.RigPath, prompt, work.AgentOverride)
}

// ShutdownDelay is the standard delay after session creation.
// Some roles use this instead of the runtime's ready delay.
func ShutdownDelay() time.Duration {
	return constants.ShutdownNotifyDelay
}
