package cmd

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/daemon"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/mayor"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var mayorCmd = &cobra.Command{
	Use:     "mayor",
	Aliases: []string{"may"},
	GroupID: GroupAgents,
	Short:   "Manage the Mayor (Chief of Staff for cross-rig coordination)",
	RunE:    requireSubcommand,
	Long: `Manage the Mayor - the Overseer's Chief of Staff.

The Mayor is the global coordinator for Gas Town:
  - Receives escalations from Witnesses and Deacon
  - Coordinates work across multiple rigs
  - Handles human communication when needed
  - Routes strategic decisions and cross-project issues

The Mayor is the primary interface between the human Overseer and the
automated agents. When in doubt, escalate to the Mayor.

Role shortcuts: "mayor" in mail/nudge addresses resolves to this agent.`,
}

var mayorStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Mayor session",
	Long: `Start the Mayor tmux session.

Creates a new detached tmux session for the Mayor and launches its configured agent.
The session runs in the workspace root directory.`,
	RunE: runMayorStart,
}

var mayorStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Mayor session",
	Long: `Stop the Mayor tmux session.

Attempts graceful shutdown first (Ctrl-C), then kills the tmux session.`,
	RunE: runMayorStop,
}

var mayorAttachCmd = &cobra.Command{
	Use:     "attach",
	Aliases: []string{"at"},
	Short:   "Attach to the Mayor session",
	Long: `Attach to the running Mayor tmux session.

Attaches the current terminal to the Mayor's tmux session.
Detach with Ctrl-B D.`,
	RunE: runMayorAttach,
}

var mayorStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check Mayor session status",
	Long:  `Check if the Mayor tmux session is currently running.`,
	RunE:  runMayorStatus,
}

var mayorRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the Mayor session",
	Long: `Restart the Mayor tmux session.

Stops the current session (if running) and starts a fresh one.`,
	RunE: runMayorRestart,
}

var mayorAcpCmd = &cobra.Command{
	Use:   "acp",
	Short: "Run Mayor in headless mode (Agent Control Protocol)",
	Long: `Run the Mayor in headless mode with stdin/stdout connected.

This command initializes a headless session without tmux, designed for
IDE integration via the Agent Control Protocol. It bypasses all tmux
logic and runs directly in the current terminal.

Environment variable overrides:
  GT_RIG          - Override rig name
  GT_TOWN_ROOT    - Override town root directory
  GT_ROLE         - Override role (default: mayor)

The agent reads prompts from stdin and outputs to stdout. This enables
programmatic control by IDEs or other tools that need direct agent access.

While an ACP session is active, automatic cleanup of polecat workspaces
is vetoed to allow the Mayor to review worker diffs before they vanish.`,
	RunE: runMayorAcp,
}

func init() {
	mayorCmd.AddCommand(mayorStartCmd)
	mayorCmd.AddCommand(mayorStopCmd)
	mayorCmd.AddCommand(mayorAttachCmd)
	mayorCmd.AddCommand(mayorStatusCmd)
	mayorCmd.AddCommand(mayorRestartCmd)
	mayorCmd.AddCommand(mayorAcpCmd)

	mayorStatusCmd.Flags().Bool("running", false, "Output only true/false for running status")

	mayorStartCmd.Flags().String("agent", "", "Agent alias to run the Mayor with (overrides town default)")
	mayorAttachCmd.Flags().String("agent", "", "Agent alias to run the Mayor with (overrides town default)")
	mayorRestartCmd.Flags().String("agent", "", "Agent alias to run the Mayor with (overrides town default)")

	mayorAcpCmd.Flags().String("rig", "", "Rig name (overrides GT_RIG env)")
	mayorAcpCmd.Flags().String("town", "", "Town root directory (overrides GT_TOWN_ROOT env)")
	mayorAcpCmd.Flags().String("agent", "", "Agent alias to run (overrides town default)")

	rootCmd.AddCommand(mayorCmd)
}

// getMayorManager returns a mayor manager for the current workspace.
func getMayorManager() (*mayor.Manager, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return mayor.NewManager(townRoot), nil
}

// getMayorSessionName returns the Mayor session name.
func getMayorSessionName() string {
	return mayor.SessionName()
}

func runMayorStart(cmd *cobra.Command, _ []string) error {
	mgr, err := getMayorManager()
	if err != nil {
		return err
	}

	fmt.Println("Starting Mayor session...")
	if err := mgr.Start(commandStringFlag(cmd, "agent")); err != nil {
		if err == mayor.ErrAlreadyRunning {
			return fmt.Errorf("Mayor session already running. Attach with: gt mayor attach")
		}
		return err
	}

	fmt.Printf("%s Mayor session started. Attach with: %s\n",
		style.Bold.Render("✓"),
		style.Dim.Render("gt mayor attach"))

	return nil
}

func runMayorStop(_ *cobra.Command, _ []string) error {
	mgr, err := getMayorManager()
	if err != nil {
		return err
	}

	fmt.Println("Stopping Mayor session...")
	if err := mgr.Stop(); err != nil {
		if err == mayor.ErrNotRunning {
			return fmt.Errorf("Mayor session is not running")
		}
		return err
	}

	fmt.Printf("%s Mayor session stopped.\n", style.Bold.Render("✓"))
	return nil
}

func runMayorAttach(cmd *cobra.Command, _ []string) error {
	mgr, err := getMayorManager()
	if err != nil {
		return err
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding workspace: %w", err)
	}

	shutdownActiveMayorACP(townRoot)

	// Ensure daemon and dolt are running before attaching.
	if err := ensureMayorInfra(townRoot); err != nil {
		return err
	}

	t := tmux.NewTmux()
	sessionID := mgr.SessionName()

	running, err := mgr.IsRunning()
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		fmt.Println("Mayor session not running, starting...")
		if err := mgr.Start(commandStringFlag(cmd, "agent")); err != nil {
			return err
		}
	} else if !t.IsAgentAlive(sessionID) {
		if err := restartMayorRuntime(t, townRoot, sessionID, commandStringFlag(cmd, "agent")); err != nil {
			return err
		}
	}

	// Use shared attach helper (smart: links if inside tmux, attaches if outside)
	return attachToTmuxSession(sessionID)
}

func shutdownActiveMayorACP(townRoot string) {
	// Only 'gt mayor attach' is allowed to transition from ACP to tmux mode.
	if !mayor.IsACPActive(townRoot) {
		return
	}
	fmt.Fprintf(os.Stderr, "ACP Mayor is active. Switching to tmux mode...\n")
	if err := gracefullyShutdownACP(townRoot); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not gracefully shutdown ACP: %v\n", err)
	}
}

func restartMayorRuntime(t *tmux.Tmux, townRoot, sessionID, agentOverride string) error {
	fmt.Println("Runtime exited, restarting with context...")
	paneID, err := t.GetPaneID(sessionID)
	if err != nil {
		return fmt.Errorf("getting pane ID: %w", err)
	}
	startupCmd, err := mayorStartupCommand(townRoot, agentOverride)
	if err != nil {
		return err
	}
	if claudeConfigDir := mayorConfigDir(townRoot); claudeConfigDir != "" {
		startupCmd = config.PrependEnv(startupCmd, map[string]string{"CLAUDE_CONFIG_DIR": claudeConfigDir})
		_ = t.SetEnvironment(sessionID, "CLAUDE_CONFIG_DIR", claudeConfigDir)
	}
	if err := t.SetRemainOnExit(paneID, true); err != nil {
		style.PrintWarning("could not set remain-on-exit: %v", err)
	}
	if err := t.KillPaneProcesses(paneID); err != nil {
		style.PrintWarning("could not kill pane processes: %v", err)
	}
	if err := t.RespawnPane(paneID, startupCmd); err != nil {
		return fmt.Errorf("restarting runtime: %w", err)
	}
	fmt.Printf("%s Mayor restarted with context\n", style.Bold.Render("✓"))
	return nil
}

func mayorStartupCommand(townRoot, agentOverride string) (string, error) {
	beacon := session.FormatStartupBeacon(session.BeaconConfig{
		Recipient: "mayor",
		Sender:    "human",
		Topic:     "attach",
	})
	startupCmd, err := config.BuildAgentStartupCommandWithAgentOverride("mayor", "", townRoot, "", beacon, agentOverride)
	if err != nil {
		return "", fmt.Errorf("building startup command: %w", err)
	}
	return startupCmd, nil
}

func mayorConfigDir(townRoot string) string {
	accountsPath := constants.MayorAccountsPath(townRoot)
	claudeConfigDir, _, _ := config.ResolveAccountConfigDir(accountsPath, "")
	if claudeConfigDir == "" {
		claudeConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	return claudeConfigDir
}

// gracefullyShutdownACP removes the PID file to signal the ACP proxy to exit,
// then waits for the process to terminate.
func gracefullyShutdownACP(townRoot string) error {
	// Get the PID before removing the file
	pid, err := mayor.GetACPPid(townRoot)
	if err != nil {
		// PID file doesn't exist or is invalid, nothing to shut down
		return nil
	}

	// Remove the PID file - this signals the ACP proxy to shut down gracefully
	if err := mayor.RemoveACPPid(townRoot); err != nil {
		return fmt.Errorf("removing ACP PID file: %w", err)
	}

	// Find the process
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil // Process doesn't exist
	}

	// Wait for the process to exit (with timeout)
	fmt.Fprintf(os.Stderr, "Waiting for ACP session to shut down")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(os.Stderr, ".")
		// Check if process is still alive
		if err := process.Signal(syscall.Signal(0)); err != nil {
			// Process has exited
			fmt.Fprintf(os.Stderr, " done\n")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Process didn't exit gracefully, force kill
	fmt.Fprintf(os.Stderr, " forcing shutdown\n")
	_ = process.Kill()
	time.Sleep(100 * time.Millisecond)
	return nil
}

func runMayorStatus(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	mgr := mayor.NewManager(townRoot)
	status, err := mgr.CombinedStatus()
	if err != nil {
		return err
	}

	if commandBoolFlag(cmd, "running") {
		fmt.Println(status.Active)
		return nil
	}
	printMayorStatus(status)
	return nil
}

func printMayorStatus(status *mayor.MayorStatus) {
	if !status.Active {
		fmt.Printf("%s Mayor session is %s\n",
			style.Dim.Render("○"),
			"not running")
		fmt.Printf("\nStart with: %s\n", style.Dim.Render("gt mayor start"))
		return
	}

	if status.Tmux != nil {
		attachedStatus := "detached"
		if status.Tmux.Attached {
			attachedStatus = "attached"
		}
		fmt.Printf("%s Mayor (tmux) is %s\n",
			style.Bold.Render("●"),
			style.Bold.Render("running"))
		fmt.Printf("  Status: %s\n", attachedStatus)
		fmt.Printf("  Created: %s\n", status.Tmux.Created)
	}

	if status.ACPPid != 0 {
		fmt.Printf("%s Mayor (ACP) is %s\n",
			style.Bold.Render("●"),
			style.Bold.Render("running (headless)"))
		fmt.Printf("  PID: %d\n", status.ACPPid)
	}

	if status.Tmux != nil {
		fmt.Printf("\nAttach with: %s\n", style.Dim.Render("gt mayor attach"))
	} else if status.ACPPid != 0 {
		fmt.Printf("\nAttach with: %s\n", style.Dim.Render("gt mayor acp"))
	}
}

func runMayorRestart(cmd *cobra.Command, args []string) error {
	mgr, err := getMayorManager()
	if err != nil {
		return err
	}

	// Stop if running (ignore not-running error)
	if err := mgr.Stop(); err != nil && err != mayor.ErrNotRunning {
		return fmt.Errorf("stopping session: %w", err)
	}

	// Start fresh
	return runMayorStart(cmd, args)
}

// ensureMayorInfra checks that daemon and dolt are running before attaching
// to the Mayor session. Warns and auto-starts each if absent.
// Returns an error if Dolt fails to start — a missing Dolt server is fatal
// for the Mayor (it cannot operate without database access).
// Daemon failures are non-fatal (warned but do not block).
func ensureMayorInfra(townRoot string) error {
	// Load daemon.json env vars (e.g., GT_DOLT_PORT) so Dolt uses the right port.
	if patrolCfg := daemon.LoadPatrolConfig(townRoot); patrolCfg != nil {
		for k, v := range patrolCfg.Env {
			os.Setenv(k, v)
		}
	}
	ensureMayorDaemon(townRoot)
	return ensureMayorDolt(townRoot)
}

func ensureMayorDaemon(townRoot string) {
	daemonRunning, _, _ := daemon.IsRunning(townRoot)
	if daemonRunning {
		return
	}
	style.PrintWarning("daemon is not running, starting...")
	if err := ensureDaemon(townRoot); err != nil {
		style.PrintWarning("daemon start failed: %v", err)
		return
	}
	fmt.Printf("  %s Daemon started\n", style.Bold.Render("✓"))
}

func ensureMayorDolt(townRoot string) error {
	doltCfg := doltserver.DefaultConfig(townRoot)
	if doltCfg.IsRemote() {
		return nil
	}
	if _, err := os.Stat(doltCfg.DataDir); err != nil {
		return nil
	}
	doltRunning, _, _ := doltserver.IsRunning(townRoot)
	if doltRunning {
		return nil
	}
	style.PrintWarning("Dolt server is not running, starting...")
	if err := doltserver.Start(townRoot); err != nil {
		return mayorDoltStartError(doltCfg, err)
	}
	fmt.Printf("  %s Dolt server started (port %d)\n", style.Bold.Render("✓"), doltCfg.Port)
	return nil
}

func mayorDoltStartError(doltCfg *doltserver.Config, startErr error) error {
	msg := fmt.Sprintf("Dolt server start failed: %v", startErr)
	if pid, dataDir := doltserver.PortHolder(doltCfg.Port); pid > 0 {
		if dataDir != "" {
			msg += fmt.Sprintf("\n  port %d held by dolt PID %d serving %s", doltCfg.Port, pid, dataDir)
		} else {
			msg += fmt.Sprintf("\n  port %d held by PID %d", doltCfg.Port, pid)
		}
	}
	if freePort := doltserver.FindFreePort(doltCfg.Port + 1); freePort > 0 {
		msg += fmt.Sprintf("\n\nConfigure a free port for this town, then retry:\n  gt config set dolt.port %d && gt mayor at", freePort)
	}
	return fmt.Errorf("%s", msg)
}

// runMayorAcp runs the Mayor in headless mode for IDE integration.
// It bypasses tmux and execs the agent directly with stdin/stdout connected.
// A PID file is created to signal that automatic cleanup should be vetoed,
// allowing the Mayor to review worker diffs before cleanup.
func runMayorAcp(cmd *cobra.Command, _ []string) error {
	ctx := context.Background()

	townRoot := commandStringFlag(cmd, "town")
	if townRoot == "" {
		townRoot = os.Getenv("GT_TOWN_ROOT")
	}
	if townRoot == "" {
		var err error
		townRoot, err = workspace.FindFromCwdOrError()
		if err != nil {
			return fmt.Errorf("not in a Gas Town workspace: %w", err)
		}
	}

	if err := ensureMayorInfra(townRoot); err != nil {
		return err
	}

	rigName := commandStringFlag(cmd, "rig")
	if rigName == "" {
		rigName = os.Getenv("GT_RIG")
	}

	mgr := mayor.NewManager(townRoot)
	return mgr.StartACP(ctx, commandStringFlag(cmd, "agent"), rigName)
}
