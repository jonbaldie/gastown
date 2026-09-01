package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/crew"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type crewAtSession struct {
	cmd             *cobra.Command
	args            []string
	retried         bool
	state           *crewCommandState
	debug           bool
	name            string
	rig             *rig.Rig
	worker          *crew.CrewWorker
	townRoot        string
	claudeConfigDir string
	runtimeConfig   *config.RuntimeConfig
	tmux            *tmux.Tmux
	sessionID       string
}

func runCrewAtWithRetry(cmd *cobra.Command, args []string, retried bool) error {
	s, err := newCrewAtSession(cmd, args, retried)
	if err != nil {
		return err
	}
	if s.state.noTmux {
		fmt.Println(s.worker.ClonePath)
		return nil
	}
	if err := resolveCrewAtRuntime(s); err != nil {
		return err
	}
	if err := ensureCrewAtSession(s); err != nil {
		return err
	}
	return attachCrewAtSession(s)
}

func newCrewAtSession(cmd *cobra.Command, args []string, retried bool) (*crewAtSession, error) {
	state := crewState()
	s := &crewAtSession{
		cmd:     cmd,
		args:    args,
		retried: retried,
		state:   state,
		debug:   state.debug || os.Getenv("GT_DEBUG") != "",
	}
	if s.debug {
		cwd, _ := os.Getwd()
		fmt.Printf("[DEBUG] runCrewAt: args=%v, crewRig=%q, cwd=%q\n", args, state.rig, cwd)
	}
	if err := resolveCrewAtName(s); err != nil {
		return nil, err
	}
	if s.debug {
		fmt.Printf("[DEBUG] after detection: name=%q, crewRig=%q\n", s.name, s.state.rig)
	}
	if err := loadCrewAtWorker(s); err != nil {
		return nil, err
	}
	if err := resetOrWarnCrewAtBranch(s); err != nil {
		return nil, err
	}
	return s, nil
}

func resolveCrewAtName(s *crewAtSession) error {
	if len(s.args) > 0 {
		s.name = s.args[0]
		if rigName, crewName, ok := parseRigSlashName(s.name); ok {
			if s.state.rig == "" {
				s.state.rig = rigName
			}
			s.name = crewName
		}
		return nil
	}
	detected, err := detectCrewFromCwd()
	if err != nil {
		return fmt.Errorf("could not detect crew workspace from current directory: %w%s", err, crewAtDetectHint(s.state.rig))
	}
	s.name = detected.crewName
	if s.state.rig == "" {
		s.state.rig = detected.rigName
	}
	fmt.Printf("Detected crew workspace: %s/%s\n", detected.rigName, s.name)
	return nil
}

func crewAtDetectHint(rigName string) string {
	hint := "\n\nUsage: gt crew at <name>"
	if rigName == "" {
		return hint
	}
	mgr, _, mgrErr := getCrewManager(rigName)
	if mgrErr != nil {
		return hint
	}
	members, listErr := mgr.List()
	if listErr != nil || len(members) == 0 {
		return hint
	}
	hint = fmt.Sprintf("\n\nAvailable crew in %s:", rigName)
	for _, m := range members {
		hint += fmt.Sprintf("\n  %s", m.Name)
	}
	return hint
}

func loadCrewAtWorker(s *crewAtSession) error {
	crewMgr, r, err := getCrewManagerForMember(s.state.rig, s.name)
	if err != nil {
		return err
	}
	s.rig = r
	worker, err := crewMgr.Get(s.name)
	if err == crew.ErrCrewNotFound {
		return fmt.Errorf("crew workspace '%s' not found", s.name)
	}
	if err != nil {
		return fmt.Errorf("getting crew worker: %w", err)
	}
	s.worker = worker
	return nil
}

func resetOrWarnCrewAtBranch(s *crewAtSession) error {
	label := fmt.Sprintf("Crew workspace %s/%s", s.rig.Name, s.name)
	if !s.state.reset {
		warnIfNotDefaultBranch(s.worker.ClonePath, label, s.rig.Path)
		return nil
	}
	if err := ensureDefaultBranch(s.worker.ClonePath, label, s.rig.Path); err != nil {
		return fmt.Errorf("resetting to default branch: %w", err)
	}
	return nil
}

func resolveCrewAtRuntime(s *crewAtSession) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}
	s.townRoot = townRoot
	accountsPath := constants.MayorAccountsPath(townRoot)
	claudeConfigDir, accountHandle, err := config.ResolveAccountConfigDir(accountsPath, s.state.account)
	if err != nil {
		return fmt.Errorf("resolving account: %w", err)
	}
	s.claudeConfigDir = claudeConfigDir
	if accountHandle != "" {
		fmt.Printf("Using account: %s\n", accountHandle)
	}
	s.runtimeConfig = resolveCrewAtRuntimeConfig(s.name, townRoot, s.rig.Path, s.state.agentOverride)
	crewSettingsDir := config.RoleSettingsDir("crew", s.rig.Path)
	if err := runtime.EnsureSettingsForRole(crewSettingsDir, s.worker.ClonePath, "crew", s.runtimeConfig); err != nil {
		style.PrintWarning("could not ensure settings for %s: %v", s.name, err)
	}
	return nil
}

func resolveCrewAtRuntimeConfig(name, townRoot, rigPath, agentOverride string) *config.RuntimeConfig {
	var runtimeConfig *config.RuntimeConfig
	if agentOverride != "" {
		rc, _, resolveErr := config.ResolveAgentConfigWithOverride(townRoot, rigPath, agentOverride)
		if resolveErr != nil {
			style.PrintWarning("could not resolve agent override %q: %v, falling back to default", agentOverride, resolveErr)
			runtimeConfig = config.ResolveWorkerAgentConfig(name, townRoot, rigPath)
		} else {
			runtimeConfig = rc
		}
	} else {
		runtimeConfig = config.ResolveWorkerAgentConfig(name, townRoot, rigPath)
	}
	if runtimeConfig == nil {
		return config.DefaultRuntimeConfig()
	}
	return runtimeConfig
}

func ensureCrewAtSession(s *crewAtSession) error {
	s.tmux = tmux.NewTmux()
	s.sessionID = crewSessionName(s.rig.Name, s.name)
	if s.debug {
		fmt.Printf("[DEBUG] sessionID=%q (r.Name=%q, name=%q)\n", s.sessionID, s.rig.Name, s.name)
	}
	hasSession, err := s.tmux.HasSession(s.sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if s.debug {
		fmt.Printf("[DEBUG] hasSession=%v\n", hasSession)
	}
	if !hasSession {
		if attached, err := attachExistingCrewRuntimeSession(s); attached || err != nil {
			return err
		}
		return createCrewAtSession(s)
	}
	return restartCrewAtIfDead(s)
}

func attachExistingCrewRuntimeSession(s *crewAtSession) (bool, error) {
	if s.runtimeConfig.Tmux == nil {
		return false, nil
	}
	existingSessions, err := s.tmux.FindSessionByWorkDir(s.worker.ClonePath, s.runtimeConfig.Tmux.ProcessNames)
	if err != nil || len(existingSessions) == 0 {
		return false, nil
	}
	existingSession := existingSessions[0]
	fmt.Printf("%s Found existing runtime session '%s' in crew directory\n",
		style.Warning.Render("⚠"),
		existingSession)
	fmt.Printf("  Attaching to existing session instead of creating a new one\n")
	if tmux.IsInsideTmux() {
		fmt.Printf("Use C-b s to switch to '%s'\n", existingSession)
		return true, nil
	}
	if s.state.detached {
		fmt.Printf("Existing session: '%s'. Run 'tmux attach -t %s' to attach.\n",
			existingSession, existingSession)
		return true, nil
	}
	return true, attachToTmuxSession(existingSession)
}

func createCrewAtSession(s *crewAtSession) error {
	if err := s.tmux.NewSession(s.sessionID, s.worker.ClonePath); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	applyCrewAtSessionEnv(s, "start")
	theme := tmux.ResolveSessionTheme(s.townRoot, s.rig.Name, "crew", s.name)
	_ = s.tmux.ConfigureGasTownSession(s.sessionID, theme, s.rig.Name, s.name, "crew")
	if err := s.tmux.WaitForShellReady(s.sessionID, constants.ShellReadyTimeout); err != nil {
		return fmt.Errorf("waiting for shell: %w", err)
	}
	paneID, err := s.tmux.GetPaneID(s.sessionID)
	if err != nil {
		return fmt.Errorf("getting pane ID: %w", err)
	}
	startupCmd, err := buildCrewAtStartupCommand(s, "start")
	if err != nil {
		return err
	}
	if err := s.tmux.RespawnPane(paneID, startupCmd); err != nil {
		return fmt.Errorf("starting runtime: %w", err)
	}
	fmt.Printf("%s Created session for %s/%s\n",
		style.Bold.Render("✓"), s.rig.Name, s.name)
	return nil
}

func restartCrewAtIfDead(s *crewAtSession) error {
	if s.tmux.IsAgentAlive(s.sessionID) {
		return nil
	}
	fmt.Printf("Runtime exited, restarting...\n")
	applyCrewAtSessionEnv(s, "restart")
	paneID, err := s.tmux.GetPaneID(s.sessionID)
	if err != nil {
		return fmt.Errorf("getting pane ID: %w", err)
	}
	startupCmd, err := buildCrewAtStartupCommand(s, "restart")
	if err != nil {
		return err
	}
	if err := s.tmux.KillPaneProcesses(paneID); err != nil {
		style.PrintWarning("could not kill pane processes: %v", err)
	}
	if err := s.tmux.RespawnPane(paneID, startupCmd); err != nil {
		return handleCrewAtRestartRespawnError(s, err)
	}
	return nil
}

func handleCrewAtRestartRespawnError(s *crewAtSession, err error) error {
	if !strings.Contains(err.Error(), "can't find pane") {
		return fmt.Errorf("restarting runtime: %w", err)
	}
	if s.retried {
		return fmt.Errorf("stale session persists after cleanup: %w", err)
	}
	fmt.Printf("Stale session detected, recreating...\n")
	if killErr := s.tmux.KillSession(s.sessionID); killErr != nil && killErr != tmux.ErrSessionNotFound {
		return fmt.Errorf("failed to kill stale session: %w", killErr)
	}
	return runCrewAtWithRetry(s.cmd, s.args, true)
}

func applyCrewAtSessionEnv(s *crewAtSession, topic string) {
	envVars := config.AgentEnv(crewAtAgentEnvConfig(s, topic, s.claudeConfigDir))
	envVars = session.MergeRuntimeLivenessEnv(envVars, s.runtimeConfig)
	for k, v := range envVars {
		_ = s.tmux.SetEnvironment(s.sessionID, k, v)
	}
}

func crewAtAgentEnvConfig(s *crewAtSession, topic, runtimeConfigDir string) config.AgentEnvConfig {
	return config.AgentEnvConfig{
		Role:             "crew",
		Rig:              s.rig.Name,
		AgentName:        s.name,
		TownRoot:         s.townRoot,
		RuntimeConfigDir: runtimeConfigDir,
		Agent:            s.state.agentOverride,
		Topic:            topic,
		SessionName:      s.sessionID,
	}
}

func buildCrewAtStartupCommand(s *crewAtSession, topic string) (string, error) {
	address := session.BeaconRecipient("crew", s.name, s.rig.Name)
	beacon := session.FormatStartupBeacon(session.BeaconConfig{
		Recipient: address,
		Sender:    "human",
		Topic:     topic,
	})
	cfg := crewAtAgentEnvConfig(s, topic, "")
	cfg.Prompt = beacon
	startupCmd, err := config.BuildStartupCommandFromConfig(cfg, s.rig.Path, beacon, s.state.agentOverride)
	if err != nil {
		return "", fmt.Errorf("building startup command: %w", err)
	}
	if s.runtimeConfig.Session != nil && s.runtimeConfig.Session.ConfigDirEnv != "" && s.claudeConfigDir != "" {
		startupCmd = config.PrependEnv(startupCmd, map[string]string{s.runtimeConfig.Session.ConfigDirEnv: s.claudeConfigDir})
	}
	return startupCmd, nil
}

func attachCrewAtSession(s *crewAtSession) error {
	if isInTmuxSession(s.sessionID) {
		return startCrewAtAgentInCurrentSession(s)
	}
	insideTmux := tmux.IsInsideTmux()
	if s.debug {
		fmt.Printf("[DEBUG] tmux.IsInsideTmux()=%v\n", insideTmux)
	}
	if insideTmux {
		fmt.Printf("Session %s ready. Use C-b s to switch.\n", s.sessionID)
		return nil
	}
	if s.state.detached {
		fmt.Printf("Started %s/%s. Run 'gt crew at %s' to attach.\n", s.rig.Name, s.name, s.name)
		return nil
	}
	fmt.Printf("Attaching to %s...\n", s.sessionID)
	if s.debug {
		fmt.Printf("[DEBUG] calling attachToTmuxSession(%q)\n", s.sessionID)
	}
	return attachToTmuxSession(s.sessionID)
}

func startCrewAtAgentInCurrentSession(s *crewAtSession) error {
	if s.tmux.IsAgentAlive(s.sessionID) {
		fmt.Printf("Already in %s session with agent running.\n", s.name)
		return nil
	}
	agentCfg, _, err := config.ResolveAgentConfigWithOverride(s.townRoot, s.rig.Path, s.state.agentOverride)
	if err != nil {
		return fmt.Errorf("resolving agent: %w", err)
	}
	address := session.BeaconRecipient("crew", s.name, s.rig.Name)
	beacon := session.FormatStartupBeacon(session.BeaconConfig{
		Recipient: address,
		Sender:    "human",
		Topic:     "start",
	})
	fmt.Printf("Starting %s in current session...\n", agentCfg.Command)
	return execAgent(agentCfg, beacon)
}
