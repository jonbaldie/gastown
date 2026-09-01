package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/deps"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/formula"
	"github.com/jonbaldie/gastown/internal/hooks"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/shell"
	"github.com/jonbaldie/gastown/internal/skills"
	"github.com/jonbaldie/gastown/internal/state"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/templates"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/jonbaldie/gastown/internal/wrappers"
)

func installTown(opts installTownOptions) error {
	absPath, townName, err := resolveInstallTarget(opts)
	if err != nil {
		return err
	}
	done, err := checkInstallLocation(absPath, opts)
	if err != nil || done {
		return err
	}
	if err := preflightInstallBeads(absPath, opts); err != nil {
		return err
	}
	fmt.Printf("%s Creating Gas Town HQ at %s\n\n",
		style.Bold.Render("🏭"), style.Dim.Render(absPath))
	if err := createInstallLayout(absPath, townName, opts); err != nil {
		return err
	}
	if err := initInstallGitAndBeads(absPath, opts); err != nil {
		return err
	}
	finishInstallOptional(absPath, opts)
	printInstallSuccess(opts)
	return nil
}

func resolveInstallTarget(opts installTownOptions) (string, string, error) {
	targetPath := opts.destPath
	if targetPath == "" {
		targetPath = "."
	}
	if targetPath[0] == '~' {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("getting home directory: %w", err)
		}
		targetPath = filepath.Join(home, targetPath[1:])
	}
	absPath, err := filepath.Abs(targetPath)
	if err != nil {
		return "", "", fmt.Errorf("resolving path: %w", err)
	}
	townName := opts.name
	if townName == "" {
		townName = filepath.Base(absPath)
	}
	return absPath, townName, nil
}

func checkInstallLocation(absPath string, opts installTownOptions) (bool, error) {
	if isWS, _ := workspace.IsWorkspace(absPath); isWS && !opts.force {
		if !opts.wrappers {
			return false, fmt.Errorf("directory is already a Gas Town HQ (use --force to reinitialize)")
		}
		if err := wrappers.Install(); err != nil {
			return false, fmt.Errorf("installing wrapper scripts: %w", err)
		}
		fmt.Printf("✓ Installed gt-codex, gt-gemini, and gt-opencode to %s\n", wrappers.BinDir())
		return true, nil
	}
	existingRoot, _ := workspace.Find(absPath)
	if existingRoot == "" || existingRoot == absPath || opts.force {
		return false, nil
	}
	return false, fmt.Errorf("cannot create HQ inside existing Gas Town workspace\n"+
		"  Current location: %s\n"+
		"  Town root: %s\n\n"+
		"Did you mean to update the binary? Run 'make install' in the gastown repo.\n"+
		"Use --force to override (not recommended).", absPath, existingRoot)
}

func preflightInstallBeads(absPath string, opts installTownOptions) error {
	if opts.noBeads {
		return nil
	}
	if err := deps.EnsureBeads(true); err != nil {
		return fmt.Errorf("beads dependency check failed: %w", err)
	}
	if err := ensureInstallDoltReady(); err != nil {
		return err
	}
	if err := doltserver.EnsureDoltIdentity(); err != nil {
		return fmt.Errorf("dolt identity setup failed (required for beads): %w\n\nTo fix, run:\n  dolt config --global --add user.name \"Your Name\"\n  dolt config --global --add user.email \"you@example.com\"", err)
	}
	return checkInstallDoltPort(absPath, opts)
}

func checkInstallDoltPort(absPath string, opts installTownOptions) error {
	port := resolveInstallDoltPort(opts)
	if err := doltserver.CheckPortAvailable(port); err == nil {
		return nil
	}
	if canReuseInstallDoltServer(absPath, port) || useExternalTestDoltServer(port) {
		fmt.Printf("   %s Using existing Dolt server on port %d\n",
			style.Dim.Render("ℹ"), port)
		return nil
	}
	return installDoltPortInUseError(port)
}

func resolveInstallDoltPort(opts installTownOptions) int {
	port := doltserver.DefaultPort
	if opts.doltPort != 0 {
		os.Setenv("GT_DOLT_PORT", strconv.Itoa(opts.doltPort))
		return opts.doltPort
	}
	if p := os.Getenv("GT_DOLT_PORT"); p != "" {
		if envPort, err := strconv.Atoi(p); err == nil {
			return envPort
		}
	}
	return port
}

func installDoltPortInUseError(port int) error {
	pid, dataDir := doltserver.PortHolder(port)
	msg := fmt.Sprintf("Dolt port %d is already in use", port)
	if pid > 0 && dataDir != "" {
		msg += fmt.Sprintf("\nPort is held by dolt PID %d serving %s", pid, dataDir)
	} else if pid > 0 {
		msg += fmt.Sprintf("\nPort is held by PID %d", pid)
	}
	msg += "\n\nAnother Gas Town instance is using this port. Specify a free port:"
	origArgs := strings.Join(os.Args[1:], " ")
	if freePort := doltserver.FindFreePort(port + 1); freePort > 0 {
		msg += fmt.Sprintf("\n\n  gt %s --dolt-port %d", origArgs, freePort)
	} else {
		msg += fmt.Sprintf("\n\n  gt %s --dolt-port <port>", origArgs)
	}
	return fmt.Errorf("%s", msg)
}

func createInstallLayout(absPath, townName string, opts installTownOptions) error {
	if err := os.MkdirAll(absPath, 0755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	mayorDir := filepath.Join(absPath, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		return fmt.Errorf("creating mayor directory: %w", err)
	}
	fmt.Printf("   ✓ Created mayor/\n")
	if err := writeInstallTownJSON(mayorDir, townName, opts); err != nil {
		return err
	}
	if err := writeInstallRigsJSON(mayorDir); err != nil {
		return err
	}
	createInstallIdentityFiles(absPath, mayorDir)
	createInstallSupportDirs(absPath)
	return nil
}

func writeInstallTownJSON(mayorDir, townName string, opts installTownOptions) error {
	owner := installTownOwner(opts)
	publicName := opts.publicName
	if publicName == "" {
		publicName = townName
	}
	townPath := filepath.Join(mayorDir, "town.json")
	townInfo, err := os.Stat(townPath)
	if os.IsNotExist(err) {
		townConfig := &config.TownConfig{
			Type:       "town",
			Version:    config.CurrentTownVersion,
			Name:       townName,
			Owner:      owner,
			PublicName: publicName,
			CreatedAt:  time.Now(),
		}
		if err := config.SaveTownConfig(townPath, townConfig); err != nil {
			return fmt.Errorf("writing town.json: %w", err)
		}
		fmt.Printf("   ✓ Created mayor/town.json\n")
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking town.json: %w", err)
	}
	if !townInfo.Mode().IsRegular() {
		return fmt.Errorf("town.json exists but is not a regular file")
	}
	fmt.Printf("   • mayor/town.json already exists, preserving\n")
	return nil
}

func installTownOwner(opts installTownOptions) string {
	if opts.owner != "" {
		return opts.owner
	}
	out, err := exec.Command("git", "config", "user.email").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func writeInstallRigsJSON(mayorDir string) error {
	rigsPath := filepath.Join(mayorDir, "rigs.json")
	rigsInfo, err := os.Stat(rigsPath)
	if os.IsNotExist(err) {
		rigsConfig := &config.RigsConfig{
			Version: config.CurrentRigsVersion,
			Rigs:    make(map[string]config.RigEntry),
		}
		if err := config.SaveRigsConfig(rigsPath, rigsConfig); err != nil {
			return fmt.Errorf("writing rigs.json: %w", err)
		}
		fmt.Printf("   ✓ Created mayor/rigs.json\n")
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking rigs.json: %w", err)
	}
	if !rigsInfo.Mode().IsRegular() {
		return fmt.Errorf("rigs.json exists but is not a regular file")
	}
	fmt.Printf("   • mayor/rigs.json already exists, preserving\n")
	return nil
}

func createInstallIdentityFiles(absPath, mayorDir string) {
	if created, err := createTownRootAgentMDs(absPath); err != nil {
		fmt.Printf("   %s Could not create agent MDs at town root: %v\n", style.Dim.Render("⚠"), err)
	} else if created {
		fmt.Printf("   ✓ Created AGENTS.md + CLAUDE.md (town root identity anchor)\n")
	} else {
		fmt.Printf("   ✓ Preserved existing AGENTS.md + CLAUDE.md (town root identity anchor)\n")
	}
	ensureInstallRoleSettings(mayorDir, "mayor", absPath, "   ✓ Created mayor/.claude/settings.json\n")
	deaconDir := filepath.Join(absPath, "deacon")
	ensureInstallRoleSettings(deaconDir, "deacon", absPath, "   ✓ Created deacon/.claude/settings.json\n")
	if err := os.MkdirAll(filepath.Join(deaconDir, "dogs", "boot"), 0755); err != nil {
		fmt.Printf("   %s Could not create boot directory: %v\n", style.Dim.Render("⚠"), err)
	}
}

func ensureInstallRoleSettings(dir, role, absPath, okMsg string) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Printf("   %s Could not create %s directory: %v\n", style.Dim.Render("⚠"), role, err)
		return
	}
	runtimeConfig := config.ResolveRoleAgentConfig(role, absPath, dir)
	if err := runtime.EnsureSettingsForRole(dir, dir, role, runtimeConfig); err != nil {
		fmt.Printf("   %s Could not create %s settings: %v\n", style.Dim.Render("⚠"), role, err)
		return
	}
	fmt.Printf("%s", okMsg)
}

func createInstallSupportDirs(absPath string) {
	if err := os.MkdirAll(filepath.Join(absPath, "plugins"), 0755); err != nil {
		fmt.Printf("   %s Could not create plugins directory: %v\n", style.Dim.Render("⚠"), err)
	} else {
		fmt.Printf("   ✓ Created plugins/\n")
	}
	if err := config.EnsureDaemonPatrolConfig(absPath); err != nil {
		fmt.Printf("   %s Could not create daemon.json: %v\n", style.Dim.Render("⚠"), err)
	} else {
		fmt.Printf("   ✓ Created mayor/daemon.json\n")
	}
}

func initInstallGitAndBeads(absPath string, opts installTownOptions) error {
	if opts.git || opts.github != "" {
		fmt.Println()
		if err := InitGitForHarness(absPath, opts.github, !opts.public); err != nil {
			return fmt.Errorf("git initialization failed: %w", err)
		}
	}
	if opts.noBeads {
		return nil
	}
	return initInstallBeads(absPath)
}

func initInstallBeads(absPath string) error {
	if err := startInstallDolt(absPath); err != nil {
		return err
	}
	if err := initTownBeads(absPath); err != nil {
		return fmt.Errorf("initializing town beads: %w", err)
	}
	fmt.Printf("   ✓ Initialized .beads/ (town-level beads with hq- prefix)\n")
	provisionInstallFormulas(absPath)
	if err := initTownAgentBeads(absPath); err != nil {
		fmt.Printf("   %s Could not create town-level agent beads: %v\n", style.Dim.Render("⚠"), err)
	}
	setInstallBeadsRouting(absPath)
	return nil
}

func startInstallDolt(absPath string) error {
	port := doltserver.DefaultConfig(absPath).Port
	if useExternalTestDoltServer(port) {
		return nil
	}
	if _, _, err := doltserver.InitRig(absPath, "hq"); err != nil {
		return fmt.Errorf("initializing HQ Dolt database: %w", err)
	}
	if err := doltserver.Start(absPath); err != nil && !strings.Contains(err.Error(), "already running") {
		return fmt.Errorf("starting Dolt server for beads: %w", err)
	}
	return nil
}

func provisionInstallFormulas(absPath string) {
	count, err := formula.ProvisionFormulas(absPath)
	if err != nil {
		fmt.Printf("   %s Could not provision formulas: %v\n", style.Dim.Render("⚠"), err)
		return
	}
	if count > 0 {
		fmt.Printf("   ✓ Provisioned %d formulas\n", count)
	}
}

func setInstallBeadsRouting(absPath string) {
	routingCmd := beads.Spawn("config", "set", "routing.mode", "explicit")
	routingCmd.Dir = absPath
	routingCmd.Env = withBeadsDirEnv(filepath.Join(absPath, ".beads"))
	if out, err := routingCmd.CombinedOutput(); err != nil {
		fmt.Printf("   %s Could not set routing.mode: %s\n", style.Dim.Render("⚠"), strings.TrimSpace(string(out)))
	}
}

func finishInstallOptional(absPath string, opts installTownOptions) {
	saveInstallOverseer(absPath)
	saveInstallEscalation(absPath)
	provisionInstallCommandsAndSkills(absPath)
	syncInstallHooks(absPath)
	if opts.shell {
		installInstallShell()
	}
	if opts.wrappers {
		installInstallWrappers()
	}
	if opts.supervisor {
		installInstallSupervisor(absPath)
	}
}

func saveInstallOverseer(absPath string) {
	overseer, err := config.DetectOverseer(absPath)
	if err != nil {
		fmt.Printf("   %s Could not detect overseer identity: %v\n", style.Dim.Render("⚠"), err)
		return
	}
	if err := config.SaveOverseerConfig(config.OverseerConfigPath(absPath), overseer); err != nil {
		fmt.Printf("   %s Could not save overseer config: %v\n", style.Dim.Render("⚠"), err)
		return
	}
	fmt.Printf("   ✓ Detected overseer: %s (via %s)\n", overseer.FormatOverseerIdentity(), overseer.Source)
}

func saveInstallEscalation(absPath string) {
	if err := config.SaveEscalationConfig(config.EscalationConfigPath(absPath), config.NewEscalationConfig()); err != nil {
		fmt.Printf("   %s Could not create escalation config: %v\n", style.Dim.Render("⚠"), err)
		return
	}
	fmt.Printf("   ✓ Created settings/escalation.json\n")
}

func provisionInstallCommandsAndSkills(absPath string) {
	if err := templates.ProvisionCommands(absPath); err != nil {
		fmt.Printf("   %s Could not provision slash commands: %v\n", style.Dim.Render("⚠"), err)
	} else {
		fmt.Printf("   ✓ Created .claude/commands/ (slash commands for all agents)\n")
	}
	if err := skills.ProvisionFor(absPath, "claude"); err != nil {
		fmt.Printf("   %s Could not provision mattpocock skills: %v\n", style.Dim.Render("⚠"), err)
	} else {
		fmt.Printf("   ✓ Created .agents/skills/ (mattpocock skills for all role sessions)\n")
	}
}

func syncInstallHooks(absPath string) {
	targets, err := hooks.DiscoverTargets(absPath)
	if err != nil {
		return
	}
	synced := 0
	for _, target := range targets {
		if _, err := syncTarget(target, false); err == nil {
			synced++
		}
	}
	if synced > 0 {
		fmt.Printf("   ✓ Synced %d hook target(s)\n", synced)
	}
}

func installInstallShell() {
	fmt.Println()
	if err := shell.Install(); err != nil {
		fmt.Printf("   %s Could not install shell integration: %v\n", style.Dim.Render("⚠"), err)
	} else {
		fmt.Printf("   ✓ Installed shell integration (%s)\n", shell.RCFilePath(shell.DetectShell()))
	}
	if err := state.Enable(Version); err != nil {
		fmt.Printf("   %s Could not enable Gas Town: %v\n", style.Dim.Render("⚠"), err)
	} else {
		fmt.Printf("   ✓ Enabled Gas Town globally\n")
	}
}

func installInstallWrappers() {
	fmt.Println()
	if err := wrappers.Install(); err != nil {
		fmt.Printf("   %s Could not install wrapper scripts: %v\n", style.Dim.Render("⚠"), err)
		return
	}
	fmt.Printf("   ✓ Installed gt-codex and gt-opencode to %s\n", wrappers.BinDir())
}

func installInstallSupervisor(absPath string) {
	fmt.Println()
	msg, err := templates.ProvisionSupervisor(absPath)
	if err != nil {
		fmt.Printf("   %s Could not configure supervisor: %v\n", style.Dim.Render("⚠"), err)
		return
	}
	fmt.Printf("   ✓ %s\n", msg)
}

func printInstallSuccess(opts installTownOptions) {
	fmt.Printf("\n%s HQ created successfully!\n", style.Bold.Render("✓"))
	fmt.Println()
	fmt.Println("Next steps:")
	step := 1
	if !opts.git && opts.github == "" {
		fmt.Printf("  %d. Initialize git: %s\n", step, style.Dim.Render("gt git-init"))
		step++
	}
	fmt.Printf("  %d. Add a rig: %s\n", step, style.Dim.Render("gt rig add <name> <git-url>"))
	step++
	fmt.Printf("  %d. (Optional) Configure agents: %s\n", step, style.Dim.Render("gt config agent list"))
	step++
	fmt.Printf("  %d. Enter the Mayor's office: %s\n", step, style.Dim.Render("gt mayor attach"))
	fmt.Println()
	if !opts.noBeads {
		fmt.Printf("Note: Dolt server is running (stop with %s)\n", style.Dim.Render("gt dolt stop"))
	}
}
