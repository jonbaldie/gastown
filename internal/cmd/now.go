package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/deacon"
	"github.com/jonbaldie/gastown/internal/deps"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/formula"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/mayor"
	gtnow "github.com/jonbaldie/gastown/internal/now"
	"github.com/jonbaldie/gastown/internal/refinery"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/skills"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/templates"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/util"
	"github.com/jonbaldie/gastown/internal/witness"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var (
	nowMayor          string
	nowWorkers        string
	nowTown           string
	nowName           string
	nowNoAttach       bool
	nowRestartWorkers bool
	nowProvisionOnly  bool
)

var nowCmd = &cobra.Command{
	Use:     "now [path]",
	GroupID: GroupWorkspace,
	Short:   "Start a Town for this git repository and attach to the Mayor",
	Long: `Start a working Town from a git repository in about five seconds.

gt now finds or creates a Town (default ~/gt), registers this repository
as a Rig without a network clone, sets the mix, starts Dolt, the daemon,
and the Mayor, then attaches this terminal to the Mayor session.

Examples:
  gt now
  gt now --mayor cursor:grok-4.6:high --workers cursor:grok-4.6:low
  gt now --mayor cursor:high --workers cursor:low --no-attach
  gt now --town /tmp/test-town --name my_proj

Success is: you are in the Mayor session.`,
	Args:         cobra.MaximumNArgs(1),
	RunE:         runNow,
	SilenceUsage: true,
}

func init() {
	nowCmd.Flags().StringVar(&nowMayor, "mayor", "", "Mayor/Deacon profile: runtime[:model[:effort]]")
	nowCmd.Flags().StringVar(&nowWorkers, "workers", "", "Worker profile: runtime[:model[:effort]]")
	nowCmd.Flags().StringVar(&nowTown, "town", "", "Town path (default $GT_TOWN_ROOT or ~/gt)")
	nowCmd.Flags().StringVar(&nowName, "name", "", "Rig name (default: directory name)")
	nowCmd.Flags().BoolVar(&nowNoAttach, "no-attach", false, "Start the Mayor session without attaching")
	nowCmd.Flags().BoolVar(&nowRestartWorkers, "restart-workers", false, "Restart Witness and Refinery so they pick up a new worker mix")
	nowCmd.Flags().BoolVar(&nowProvisionOnly, "provision-only", false, "Finish deferred Town provision (internal)")
	_ = nowCmd.Flags().MarkHidden("provision-only")
	rootCmd.AddCommand(nowCmd)
}

func runNow(cmd *cobra.Command, args []string) error {
	if nowProvisionOnly {
		townRoot, err := gtnow.ResolveTownRoot(nowTown)
		if err != nil {
			return err
		}
		return provisionTown(townRoot)
	}

	repoPath, err := resolveNowRepo(args)
	if err != nil {
		return err
	}

	mayorProfile, workersProfile, err := parseNowProfiles()
	if err != nil {
		return err
	}
	if err := fillNowRuntimes(&mayorProfile, &workersProfile); err != nil {
		return err
	}
	if err := validateNowProfile(mayorProfile, "--mayor"); err != nil {
		return err
	}
	if err := validateNowProfile(workersProfile, "--workers"); err != nil {
		return err
	}

	if err := preflightNowRepo(repoPath); err != nil {
		return err
	}

	townRoot, err := gtnow.ResolveTownRoot(nowTown)
	if err != nil {
		return err
	}

	repoIsTown, err := workspace.IsWorkspace(repoPath)
	if err != nil {
		return err
	}
	if repoIsTown && !samePath(repoPath, townRoot) {
		return fmt.Errorf("this directory is a Town HQ (%s); run gt now from a project git repository", repoPath)
	}
	if repoIsTown && samePath(repoPath, townRoot) {
		if !nowRepoIsRegisteredRig(townRoot, repoPath) {
			return fmt.Errorf("this directory is a Town HQ; run gt now from a project git repository")
		}
	}

	if err := ensureNowTown(townRoot); err != nil {
		return err
	}

	port, err := chooseNowDoltPort(townRoot)
	if err != nil {
		return err
	}
	_ = os.Setenv("GT_TOWN_ROOT", townRoot)
	_ = os.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	if err := persistNowDoltPort(townRoot, port); err != nil {
		return err
	}

	if err := session.InitRegistry(townRoot); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: failed to initialize town registry: %v\n", err)
	}

	var (
		doltErr, rigErr, mixErr error
		rigName                 string
		mayorChanged            bool
	)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		doltErr = startNowDolt(townRoot)
	}()
	go func() {
		defer wg.Done()
		rigName, rigErr = ensureNowRig(townRoot, repoPath)
	}()
	go func() {
		defer wg.Done()
		mayorChanged, mixErr = applyNowMix(townRoot, mayorProfile, workersProfile)
	}()
	wg.Wait()
	if err := errors.Join(doltErr, rigErr, mixErr); err != nil {
		return err
	}

	if err := startNowSessions(townRoot, mayorChanged); err != nil {
		return err
	}

	fmt.Printf("gt now: town=%s rig=%s mix=%s dolt=%d\n",
		townRoot, rigName, mayorProfile.Format()+" / "+workersProfile.Format(), port)

	startDeferredProvision(townRoot)

	tm := tmux.NewTmux()
	running, err := tm.HasSession(mayor.SessionName())
	if err != nil || !running {
		if err != nil {
			return fmt.Errorf("Mayor session is not running: %w", err)
		}
		return fmt.Errorf("Mayor session is not running")
	}

	if nowNoAttach {
		fmt.Println("Mayor session is running.")
		return nil
	}

	fmt.Println("You are in the Mayor session.")
	return tm.AttachSession(mayor.SessionName())
}

func resolveNowRepo(args []string) (string, error) {
	path := "."
	if len(args) > 0 {
		path = args[0]
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("getting home directory: %w", err)
		}
		if path == "~" {
			path = home
		} else if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

func parseNowProfiles() (gtnow.Profile, gtnow.Profile, error) {
	mayorProfile, err := gtnow.ParseProfile(nowMayor, gtnow.DefaultMayorEffort, gtnow.SupportsEffort)
	if err != nil {
		return gtnow.Profile{}, gtnow.Profile{}, fmt.Errorf("parsing --mayor: %w", err)
	}
	workersProfile, err := gtnow.ParseProfile(nowWorkers, gtnow.DefaultWorkersEffort, gtnow.SupportsEffort)
	if err != nil {
		return gtnow.Profile{}, gtnow.Profile{}, fmt.Errorf("parsing --workers: %w", err)
	}
	return mayorProfile, workersProfile, nil
}

func preflightNowRepo(repoPath string) error {
	repoGit := git.NewGit(repoPath)
	if !repoGit.IsRepo() {
		return fmt.Errorf("not a git repository: %s", repoPath)
	}
	empty, err := repoGit.IsEmpty()
	if err != nil {
		return fmt.Errorf("checking repository: %w", err)
	}
	if empty {
		return fmt.Errorf("repository %s is empty (no commits). Commit at least once before running gt now", repoPath)
	}
	return nil
}

func fillNowRuntimes(mayorProfile, workersProfile *gtnow.Profile) error {
	detected := ""
	fill := func(profile *gtnow.Profile) error {
		if profile.Runtime != "" {
			return nil
		}
		if detected == "" {
			var err error
			detected, err = gtnow.DetectRuntime()
			if err != nil {
				return err
			}
		}
		profile.Runtime = detected
		return nil
	}
	if err := fill(mayorProfile); err != nil {
		return err
	}
	return fill(workersProfile)
}

func validateNowProfile(profile gtnow.Profile, flag string) error {
	if profile.Runtime == "" {
		return fmt.Errorf("%s: missing runtime", flag)
	}
	if !config.IsKnownPreset(profile.Runtime) {
		return fmt.Errorf("%s: unknown runtime %q", flag, profile.Runtime)
	}
	if !gtnow.RuntimePresent(profile.Runtime) {
		return fmt.Errorf("%s: %s not found on PATH", flag, gtnow.RuntimeCommand(profile.Runtime))
	}
	rc := config.RuntimeConfigFromPreset(config.AgentPreset(profile.Runtime))
	if !config.RuntimeSupportsEffort(rc, profile.Effort) {
		return fmt.Errorf("%s: invalid effort %q for runtime %s", flag, profile.Effort, profile.Runtime)
	}
	return nil
}

func nowRepoIsRegisteredRig(townRoot, repoPath string) bool {
	rigsPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		return false
	}
	mgr := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot))
	_, ok := mgr.FindByLocalRepo(repoPath)
	return ok
}

func samePath(a, b string) bool {
	left, err := filepath.Abs(a)
	if err != nil {
		return a == b
	}
	right, err := filepath.Abs(b)
	if err != nil {
		return a == b
	}
	if resolved, err := filepath.EvalSymlinks(left); err == nil {
		left = resolved
	}
	if resolved, err := filepath.EvalSymlinks(right); err == nil {
		right = resolved
	}
	return left == right
}

func ensureNowTown(townRoot string) error {
	if ok, _ := workspace.IsWorkspace(townRoot); ok {
		return nil
	}

	if err := deps.EnsureBeads(true); err != nil {
		return fmt.Errorf("beads dependency check failed: %w", err)
	}
	if err := ensureInstallDoltReady(); err != nil {
		return err
	}
	if err := doltserver.EnsureDoltIdentity(); err != nil {
		return fmt.Errorf("dolt identity setup failed (required for beads): %w", err)
	}

	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		return fmt.Errorf("creating town: %w", err)
	}

	townName := filepath.Base(townRoot)
	townPath := filepath.Join(townRoot, "mayor", "town.json")
	if _, err := os.Stat(townPath); os.IsNotExist(err) {
		owner := ""
		if out, err := exec.Command("git", "config", "user.email").Output(); err == nil {
			owner = strings.TrimSpace(string(out))
		}
		townConfig := &config.TownConfig{
			Type:       "town",
			Version:    config.CurrentTownVersion,
			Name:       townName,
			Owner:      owner,
			PublicName: townName,
			CreatedAt:  time.Now(),
		}
		if err := config.SaveTownConfig(townPath, townConfig); err != nil {
			return fmt.Errorf("writing town.json: %w", err)
		}
	}

	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if _, err := os.Stat(rigsPath); os.IsNotExist(err) {
		rigsConfig := &config.RigsConfig{
			Version: config.CurrentRigsVersion,
			Rigs:    make(map[string]config.RigEntry),
		}
		if err := config.SaveRigsConfig(rigsPath, rigsConfig); err != nil {
			return fmt.Errorf("writing rigs.json: %w", err)
		}
	}

	_, _ = createTownRootAgentMDs(townRoot)

	mayorDir := filepath.Join(townRoot, "mayor")
	mayorRC := config.ResolveRoleAgentConfig("mayor", townRoot, mayorDir)
	_ = runtime.EnsureSettingsForRole(mayorDir, mayorDir, "mayor", mayorRC)

	deaconDir := filepath.Join(townRoot, "deacon")
	if err := os.MkdirAll(deaconDir, 0755); err != nil {
		return fmt.Errorf("creating deacon directory: %w", err)
	}
	deaconRC := config.ResolveRoleAgentConfig("deacon", townRoot, deaconDir)
	_ = runtime.EnsureSettingsForRole(deaconDir, deaconDir, "deacon", deaconRC)
	_ = os.MkdirAll(filepath.Join(deaconDir, "dogs", "boot"), 0755)
	_ = os.MkdirAll(filepath.Join(townRoot, "plugins"), 0755)
	_ = config.EnsureDaemonPatrolConfig(townRoot)

	return nil
}

func startNowDolt(townRoot string) error {
	if err := doltserver.Start(townRoot); err != nil {
		if !strings.Contains(err.Error(), "already running") {
			return fmt.Errorf("starting Dolt server: %w", err)
		}
	}
	if _, _, err := doltserver.InitRig(townRoot, "hq"); err != nil {
		return fmt.Errorf("initializing HQ Dolt database: %w", err)
	}
	if err := initNowBeads(townRoot); err != nil {
		return fmt.Errorf("initializing town beads: %w", err)
	}
	return nil
}

func chooseNowDoltPort(townRoot string) (int, error) {
	cfg := doltserver.DefaultConfig(townRoot)
	port := doltserver.DefaultPort
	if configured := config.ResolveConfiguredDoltPort(townRoot); configured > 0 {
		port = configured
	}

	if err := doltserver.CheckPortAvailable(port); err == nil {
		pid, dataDir := doltserver.PortHolder(port)
		if pid > 0 && (dataDir == "" || !samePath(dataDir, cfg.DataDir)) {
			return nextFreeDoltPort(port, dataDir)
		}
		return port, nil
	}

	_, dataDir := doltserver.PortHolder(port)
	if dataDir != "" && samePath(dataDir, cfg.DataDir) {
		return port, nil
	}
	return nextFreeDoltPort(port, dataDir)
}

func nextFreeDoltPort(busyPort int, otherDataDir string) (int, error) {
	free := doltserver.FindFreePort(busyPort + 1)
	if free <= 0 {
		if otherDataDir != "" {
			return 0, fmt.Errorf("Dolt port %d belongs to another Town (%s); no free port found", busyPort, otherDataDir)
		}
		return 0, fmt.Errorf("Dolt port %d is in use and no free port was found", busyPort)
	}
	return free, nil
}

func persistNowDoltPort(townRoot string, port int) error {
	if err := config.EnsureDaemonPatrolConfig(townRoot); err != nil {
		return err
	}
	path := config.DaemonPatrolConfigPath(townRoot)
	data, err := os.ReadFile(path) //nolint:gosec // G304: town path
	if err != nil {
		return fmt.Errorf("reading daemon.json: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing daemon.json: %w", err)
	}
	env := map[string]string{}
	if envRaw, ok := raw["env"]; ok {
		_ = json.Unmarshal(envRaw, &env)
	}
	env["GT_DOLT_PORT"] = strconv.Itoa(port)
	envBytes, err := json.Marshal(env)
	if err != nil {
		return err
	}
	raw["env"] = envBytes
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

func ensureNowRig(townRoot, repoPath string) (string, error) {
	rigsPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Version: config.CurrentRigsVersion, Rigs: map[string]config.RigEntry{}}
	}
	mgr := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot))

	if name, ok := mgr.FindByLocalRepo(repoPath); ok {
		if nowName != "" && nowName != name {
			return "", fmt.Errorf("repository already registered as rig %q (got --name %q)", name, nowName)
		}
		return name, nil
	}

	name := strings.TrimSpace(nowName)
	if name == "" {
		name = gtnow.SanitizeRigName(filepath.Base(repoPath))
	}
	if name == "" {
		return "", fmt.Errorf("could not derive a rig name; pass --name")
	}
	if mgr.RigExists(name) {
		return "", fmt.Errorf("rig %q already exists; pass --name for this repository", name)
	}

	if _, err := mgr.AddLocalRig(name, repoPath); err != nil {
		return "", err
	}
	_ = config.AddRigToDaemonPatrols(townRoot, name)
	return name, nil
}

func applyNowMix(townRoot string, mayorProfile, workersProfile gtnow.Profile) (mayorChanged bool, err error) {
	settingsPath := config.TownSettingsPath(townRoot)
	settings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return false, fmt.Errorf("loading town settings: %w", err)
	}

	needMayor := nowMayor != "" || settings.Agents == nil || settings.Agents[gtnow.MayorAlias] == nil
	needWorkers := nowWorkers != "" || settings.Agents == nil || settings.Agents[gtnow.WorkersAlias] == nil
	if !needMayor && !needWorkers {
		return false, nil
	}

	before := mayorMixFingerprint(settings)

	if settings.Agents == nil {
		settings.Agents = make(map[string]*config.RuntimeConfig)
	}
	if needMayor {
		settings.Agents[gtnow.MayorAlias] = gtnow.AliasConfig(mayorProfile)
	}
	if needWorkers {
		settings.Agents[gtnow.WorkersAlias] = gtnow.AliasConfig(workersProfile)
	}
	if err := config.SaveTownSettings(settingsPath, settings); err != nil {
		return false, fmt.Errorf("saving agent aliases: %w", err)
	}

	var assignments []config.MixAssignment
	if needMayor {
		assignments = append(assignments, gtnow.MixAssignments(gtnow.MayorAlias, gtnow.MayorRoles, mayorProfile.Effort)...)
	}
	if needWorkers {
		assignments = append(assignments, gtnow.MixAssignments(gtnow.WorkersAlias, gtnow.WorkerRoles, workersProfile.Effort)...)
	}
	if err := config.ApplyTownMix(settingsPath, assignments); err != nil {
		return false, err
	}

	after, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return false, err
	}
	return mayorMixFingerprint(after) != before, nil
}

func mayorMixFingerprint(settings *config.TownSettings) string {
	if settings == nil {
		return ""
	}
	mayorAgent := settings.RoleAgents["mayor"]
	mayorEffort := settings.RoleEffort["mayor"]
	mayorArgs := ""
	if settings.Agents != nil {
		if rc := settings.Agents[gtnow.MayorAlias]; rc != nil {
			mayorArgs = strings.Join(rc.Args, " ")
		}
	}
	return strings.Join([]string{mayorAgent, mayorEffort, mayorArgs}, "|")
}

func startNowSessions(townRoot string, mayorChanged bool) error {
	if err := config.EnsureDaemonPatrolConfig(townRoot); err != nil {
		fmt.Printf("  %s Could not ensure daemon config: %v\n", style.Dim.Render("○"), err)
	}

	if err := ensureDaemon(townRoot); err != nil {
		return fmt.Errorf("starting daemon: %w", err)
	}

	var wg sync.WaitGroup
	var firstErr error
	var mu sync.Mutex
	setErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		mgr := mayor.NewManager(townRoot)
		running, _ := mgr.IsRunning()
		if running && mayorChanged && nowMayor != "" {
			_ = mgr.Stop()
			running = false
		}
		if running {
			return
		}
		if err := mgr.StartImmediate(""); err != nil && !errors.Is(err, mayor.ErrAlreadyRunning) && !errors.Is(err, mayor.ErrACPActive) {
			setErr(fmt.Errorf("starting Mayor: %w", err))
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		mgr := deacon.NewManager(townRoot)
		if err := mgr.StartImmediate(""); err != nil && !errors.Is(err, deacon.ErrAlreadyRunning) {
			setErr(fmt.Errorf("starting Deacon: %w", err))
		}
	}()

	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	if nowRestartWorkers {
		if err := restartNowWorkers(townRoot); err != nil {
			return err
		}
	}
	return nil
}

func restartNowWorkers(townRoot string) error {
	rigsPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		return nil
	}
	mgr := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot))
	for name := range rigsConfig.Rigs {
		r, err := mgr.GetRig(name)
		if err != nil {
			continue
		}
		wit := witness.NewManager(r)
		_ = wit.Stop()
		if err := wit.Start(false, "", nil); err != nil && !errors.Is(err, witness.ErrAlreadyRunning) {
			fmt.Fprintf(os.Stderr, "warning: restarting witness for %s: %v\n", name, err)
		}
		ref := refinery.NewManager(r)
		_ = ref.Stop()
		if err := ref.Start(false, ""); err != nil && !errors.Is(err, refinery.ErrAlreadyRunning) {
			fmt.Fprintf(os.Stderr, "warning: restarting refinery for %s: %v\n", name, err)
		}
	}
	return nil
}

func startDeferredProvision(townRoot string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "now", "--provision-only", "--town", townRoot)
	cmd.Dir = townRoot
	cmd.Env = append(os.Environ(), "GT_TOWN_ROOT="+townRoot)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	util.SetDetachedProcessGroup(cmd)
	_ = cmd.Start()
}

func provisionTown(townRoot string) error {
	if ok, _ := workspace.IsWorkspace(townRoot); !ok {
		return fmt.Errorf("not a Gas Town HQ: %s", townRoot)
	}
	if count, err := formula.ProvisionFormulas(townRoot); err == nil && count > 0 {
		fmt.Printf("provisioned %d formulas\n", count)
	}
	_ = templates.ProvisionCommands(townRoot)
	_ = skills.ProvisionFor(townRoot, "claude")
	_ = initTownAgentBeads(townRoot)
	return nil
}
