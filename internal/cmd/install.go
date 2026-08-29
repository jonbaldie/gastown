package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/deps"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/instructions"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/templates"
	"github.com/spf13/cobra"
)

var installCmd = &cobra.Command{
	Use:     "install [path]",
	GroupID: GroupWorkspace,
	Short:   "Create a new Gas Town HQ (workspace)",
	Long: `Create a new Gas Town HQ at the specified path.

The HQ (headquarters) is the top-level directory where Gas Town is installed -
the root of your workspace where all rigs and agents live. It contains:
  - AGENTS.md            Town identity anchor (CLAUDE.md symlinks to it)
  - mayor/               Mayor config, state, and rig registry
  - .beads/              Town-level beads DB (hq-* prefix for mayor mail)

If path is omitted, uses the current directory.

See docs/hq.md for advanced HQ configurations including beads
redirects, multi-system setups, and HQ templates.

Examples:
  gt install ~/gt                              # Create HQ at ~/gt
  gt install . --name my-workspace             # Initialize current dir
  gt install ~/gt --no-beads                   # Skip .beads/ initialization
  gt install ~/gt --git                        # Also init git with .gitignore
  gt install ~/gt --github=user/repo           # Create private GitHub repo (default)
  gt install ~/gt --github=user/repo --public  # Create public GitHub repo
  gt install ~/gt --shell                      # Install shell hooks and enable Gas Town machine-wide
  gt install ~/gt --supervisor                 # Configure launchd/systemd for daemon auto-restart`,
	Args:         cobra.MaximumNArgs(1),
	RunE:         runInstall,
	SilenceUsage: true,
}

func init() {
	installCmd.Flags().BoolP("force", "f", false, "Re-run install in existing HQ (preserves town.json and rigs.json)")
	installCmd.Flags().StringP("name", "n", "", "Town name (defaults to directory name)")
	installCmd.Flags().String("owner", "", "Owner email for entity identity (defaults to git config user.email)")
	installCmd.Flags().String("public-name", "", "Public display name (defaults to town name)")
	installCmd.Flags().Bool("no-beads", false, "Skip town beads initialization")
	installCmd.Flags().Bool("git", false, "Initialize git with .gitignore")
	installCmd.Flags().String("github", "", "Create GitHub repo (format: owner/repo, private by default)")
	installCmd.Flags().Bool("public", false, "Make GitHub repo public (use with --github)")
	installCmd.Flags().Bool("shell", false, "Install shell integration and enable Gas Town machine-wide")
	installCmd.Flags().Bool("wrappers", false, "Install gt-codex/gt-gemini/gt-opencode wrapper scripts to ~/bin/")
	installCmd.Flags().Bool("supervisor", false, "Configure launchd/systemd for daemon auto-restart")
	installCmd.Flags().Int("dolt-port", 0, "Dolt SQL server port (default 3307; set when another instance owns the default port)")
	rootCmd.AddCommand(installCmd)
}

func runInstall(cmd *cobra.Command, args []string) error {
	targetPath := "."
	if len(args) > 0 {
		targetPath = args[0]
	}
	return installTown(installTownOptions{
		destPath:   targetPath,
		name:       commandStringFlag(cmd, "name"),
		owner:      commandStringFlag(cmd, "owner"),
		publicName: commandStringFlag(cmd, "public-name"),
		noBeads:    commandBoolFlag(cmd, "no-beads"),
		git:        commandBoolFlag(cmd, "git"),
		github:     commandStringFlag(cmd, "github"),
		public:     commandBoolFlag(cmd, "public"),
		shell:      commandBoolFlag(cmd, "shell"),
		wrappers:   commandBoolFlag(cmd, "wrappers"),
		supervisor: commandBoolFlag(cmd, "supervisor"),
		doltPort:   commandIntFlag(cmd, "dolt-port"),
		force:      commandBoolFlag(cmd, "force"),
	})
}

type installTownOptions struct {
	destPath   string
	name       string
	owner      string
	publicName string
	noBeads    bool
	git        bool
	github     string
	public     bool
	shell      bool
	wrappers   bool
	supervisor bool
	doltPort   int
	force      bool
}

func ensureInstallDoltReady() error {
	status, version, detail := deps.CheckDolt()
	return formatInstallDoltError(status, version, detail, goruntime.GOOS)
}

const installDoltServerProbeTimeout = 2 * time.Second

func canReuseInstallDoltServer(townRoot string, port int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), installDoltServerProbeTimeout)
	defer cancel()

	probeTimeout := installDoltServerProbeTimeout.String()
	// wa-d6f: socket-first probe DSN (TCP fallback) — even the install
	// pre-flight should avoid TIME_WAIT churn when the server is up.
	dsn := buildDoltDSN("root", port, "", dsnOpts{
		Timeout:      probeTimeout,
		ReadTimeout:  probeTimeout,
		WriteTimeout: probeTimeout,
	})
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return false
	}

	// Only reuse a server that already belongs to this town. A random
	// MySQL-compatible service or another town's Dolt server on the same port
	// must remain a preflight failure; otherwise install can mutate the target
	// and then fail during bd init.
	databases, err := doltserver.ListDatabases(townRoot)
	if err != nil || len(databases) == 0 {
		return false
	}
	legitimate, err := doltserver.VerifyServerDataDir(townRoot)
	return err == nil && legitimate
}

func useExternalTestDoltServer(port int) bool {
	if os.Getenv("GT_TEST_EXTERNAL_DOLT") == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), installDoltServerProbeTimeout)
	defer cancel()

	probeTimeout := installDoltServerProbeTimeout.String()
	dsn := fmt.Sprintf("root:@tcp(127.0.0.1:%d)/?timeout=%s&readTimeout=%s&writeTimeout=%s",
		port, probeTimeout, probeTimeout, probeTimeout)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	return db.PingContext(ctx) == nil
}

func formatInstallDoltError(status deps.DoltStatus, version, detail, goos string) error {
	switch status {
	case deps.DoltOK:
		return nil
	case deps.DoltNotFound:
		return fmt.Errorf("dolt is required for gt install with beads enabled but was not found in PATH.\n\nInstall Dolt:\n  %s\n\nTo create an HQ without beads, rerun with --no-beads.\nMore install options: %s", doltInstallHint(goos), deps.DoltInstallURL)
	case deps.DoltTooOld:
		return fmt.Errorf("dolt %s is too old for gt install with beads enabled (minimum: %s).\n\nUpgrade Dolt:\n  %s\n\nTo create an HQ without beads, rerun with --no-beads.", version, deps.MinDoltVersion, doltUpgradeHint(goos))
	case deps.DoltExecFailed:
		if detail == "" {
			detail = "no diagnostic output"
		}
		return fmt.Errorf("'dolt version' failed, so gt install cannot verify the Dolt dependency required for beads.\n\nDetail: %s\n\nReinstall Dolt:\n  %s\n\nTo create an HQ without beads, rerun with --no-beads.", detail, doltReinstallHint(goos))
	case deps.DoltUnknown:
		if detail == "" {
			detail = "no version output"
		}
		return fmt.Errorf("dolt version could not be parsed, so gt install cannot verify the Dolt dependency required for beads.\n\nDetail: %s\n\nReinstall Dolt:\n  %s\n\nTo create an HQ without beads, rerun with --no-beads.", detail, doltReinstallHint(goos))
	default:
		return fmt.Errorf("dolt dependency check failed with unknown status %d.\n\nTo create an HQ without beads, rerun with --no-beads.", status)
	}
}

func doltInstallHint(goos string) string {
	if goos == "darwin" {
		return "brew install dolt"
	}
	return "Install Dolt from " + deps.DoltInstallURL
}

func doltUpgradeHint(goos string) string {
	if goos == "darwin" {
		return "brew upgrade dolt"
	}
	return "Upgrade Dolt using your package manager or reinstall from " + deps.DoltInstallURL
}

func doltReinstallHint(goos string) string {
	if goos == "darwin" {
		return "brew reinstall dolt"
	}
	return "Reinstall Dolt from " + deps.DoltInstallURL
}

// createTownRootAgentMDs writes the town-root identity pair through the shared
// instruction-file provisioner. AGENTS.md is the canonical file. CLAUDE.md is
// a symlink to it.
func createTownRootAgentMDs(townRoot string) (bool, error) {
	return instructions.Provision(townRoot, templates.TownRootAgentsMD(), "# Gas Town")
}

func writeJSON(path string, data interface{}) error {
	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, content, 0644)
}

// buildBdInitArgs returns the arguments for `bd init` including the correct
// --server-port derived from the town's Dolt configuration.
func buildBdInitArgs(townPath string) []string {
	cfg := bdInitDoltConfig(townPath)
	// gt install --force preserves town state; bd reinit flags would destroy town beads.
	return []string{"init", "--prefix", "hq", "--server",
		"--server-port", strconv.Itoa(cfg.Port)}
}

func bdInitDoltConfig(townPath string) *doltserver.Config {
	cfg := doltserver.DefaultConfig(townPath)
	// bd init targets durable town configuration. Keep non-endpoint defaults from
	// DefaultConfig, but do not let ambient endpoint env override target config.
	cfg.Host = ""
	if host := config.ResolveConfiguredDoltHost(townPath); host != "" {
		cfg.Host = host
	}
	cfg.Port = doltserver.DefaultPort
	if port := config.ResolveConfiguredDoltPort(townPath); port > 0 {
		cfg.Port = port
	}
	return cfg
}

// initTownBeads initializes town-level beads database using bd init.
// Town beads use the "hq-" prefix for mayor mail and cross-rig coordination.
// Uses Dolt backend in server mode (Gas Town requires a running Dolt sql-server).
func initTownBeads(townPath string) error {
	return initTownBeadsWith(townPath, buildBdInitArgs(townPath), 20, 500*time.Millisecond, "10s")
}

func initNowBeads(townPath string) error {
	args := append(append([]string{}, buildBdInitArgs(townPath)...),
		"--skip-agents", "--skip-hooks", "--non-interactive", "--quiet",
		"--external", "--role", "maintainer")
	return initTownBeadsWith(townPath, args, 40, 50*time.Millisecond, "2s")
}

func initTownBeadsWith(townPath string, bdInitArgs []string, attempts int, delay time.Duration, waitLabel string) error {
	if err := waitForInstallDoltReady(townPath, attempts, delay, waitLabel); err != nil {
		return err
	}
	if err := runBdInitForTown(townPath, bdInitArgs); err != nil {
		return err
	}
	beadsDir := filepath.Join(townPath, ".beads")
	if err := ensureTownBeadsLayout(townPath, beadsDir); err != nil {
		return err
	}
	if err := configureTownBeadsYAML(beadsDir); err != nil {
		return err
	}
	ensureTownBeadsRoutes(townPath)
	return nil
}

func waitForInstallDoltReady(townPath string, attempts int, delay time.Duration, waitLabel string) error {
	dsn := buildDoltDSNFromConfig(bdInitDoltConfig(townPath), "", dsnOpts{})
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		db, err := sql.Open("mysql", dsn)
		if err == nil {
			err = db.Ping()
			db.Close()
		}
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(delay)
	}
	return fmt.Errorf("Dolt server is not ready after %s: %w", waitLabel, lastErr)
}

func runBdInitForTown(townPath string, bdInitArgs []string) error {
	cmd := beads.Spawn(bdInitArgs...)
	cmd.Dir = townPath
	cmd.Env = withBeadsDirEnv(filepath.Join(townPath, ".beads"))
	output, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(output), "already initialized") {
		return fmt.Errorf("bd init failed: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func ensureTownBeadsLayout(townPath, beadsDir string) error {
	if _, statErr := os.Stat(beadsDir); os.IsNotExist(statErr) {
		return fmt.Errorf("bd init succeeded but .beads directory not created (check bd daemon interference)")
	}
	if err := beads.EnsureDir(beadsDir); err != nil {
		return err
	}
	if err := doltserver.EnsureMetadata(townPath, "hq"); err != nil {
		return fmt.Errorf("ensuring hq metadata: %w", err)
	}
	if err := beads.EnsureConfigYAML(beadsDir, "hq"); err != nil {
		return fmt.Errorf("ensuring config.yaml: %w", err)
	}
	return nil
}

func configureTownBeadsYAML(beadsDir string) error {
	if err := beads.EnsureConfigYAMLValue(beadsDir, "beads.role", "maintainer"); err != nil {
		fmt.Printf("   %s Could not set beads.role: %v\n", style.Dim.Render("⚠"), err)
	}
	if err := beads.EnsureCustomTypesConfigYAML(beadsDir); err != nil {
		return fmt.Errorf("ensuring custom types: %w", err)
	}
	if err := beads.EnsureCustomStatuses(beadsDir); err != nil {
		fmt.Printf("   %s Could not register custom statuses: %v\n", style.Dim.Render("⚠"), err)
	}
	if err := beads.EnsureConfigYAMLValue(beadsDir, "allowed_prefixes", "hq,hq-cv"); err != nil {
		fmt.Printf("   %s Could not set allowed_prefixes: %v\n", style.Dim.Render("⚠"), err)
	}
	ensureTownBeadsIssuesJSONL(beadsDir)
	return nil
}

func ensureTownBeadsIssuesJSONL(beadsDir string) {
	issuesJSONL := filepath.Join(beadsDir, "issues.jsonl")
	if _, err := os.Stat(issuesJSONL); !os.IsNotExist(err) {
		return
	}
	if err := os.WriteFile(issuesJSONL, []byte{}, 0644); err != nil {
		fmt.Printf("   %s Could not create issues.jsonl: %v\n", style.Dim.Render("⚠"), err)
	}
}

func ensureTownBeadsRoutes(townPath string) {
	if err := beads.AppendRoute(townPath, beads.Route{Prefix: "hq-", Path: "."}); err != nil {
		fmt.Printf("   %s Could not update routes.jsonl: %v\n", style.Dim.Render("⚠"), err)
	}
	if err := beads.AppendRoute(townPath, beads.Route{Prefix: "hq-cv-", Path: "."}); err != nil {
		fmt.Printf("   %s Could not register convoy prefix: %v\n", style.Dim.Render("⚠"), err)
	}
}

// withBeadsDirEnv returns the hardened bd mutation environment pinned to the
// target beads directory, with stale selectors stripped and canonical Dolt
// endpoint aliases rebuilt from the shared helper.
func withBeadsDirEnv(beadsDir string) []string {
	base := os.Environ()
	if townRoot := beads.FindTownRoot(filepath.Dir(beads.ResolveBeadsDir(beadsDir))); townRoot != "" {
		base = config.NormalizeConfiguredDoltEnv(base, townRoot)
		if host := config.ResolveConfiguredDoltHost(townRoot); host != "" {
			base = beads.StripEnvKey(base, "GT_DOLT_HOST")
			base = append(base, "GT_DOLT_HOST="+host)
		}
		if port := config.ResolveConfiguredDoltPort(townRoot); port > 0 {
			base = beads.StripEnvKey(base, "GT_DOLT_PORT")
			base = append(base, "GT_DOLT_PORT="+strconv.Itoa(port))
		}
	}
	return beads.BuildMutationPinnedBDEnv(base, beadsDir)
}

// ensureCustomTypes registers Gas Town issue type configuration with beads.
// Beads core only supports built-in types (bug, feature, task, etc.).
// Gas Town needs custom types and keeps rig out of infra/wisp storage.
// This is idempotent - safe to call multiple times.
func ensureCustomTypes(beadsPath string) error {
	for _, cfg := range []struct{ key, value string }{
		{"types.custom", constants.BeadsCustomTypes},
		{"types.infra", constants.BeadsInfraTypes},
	} {
		cmd := beads.Spawn("config", "set", cfg.key, cfg.value)
		cmd.Dir = beadsPath
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("bd config set %s: %s", cfg.key, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

// initTownAgentBeads creates town-level agent beads using hq- prefix.
// This creates:
//   - hq-mayor, hq-deacon (agent beads for town-level agents)
//
// These beads are stored in town beads (~/gt/.beads/) and are shared across all rigs.
// Rig-level agent beads (witness, refinery) are created by gt rig add in rig beads.
//
// Note: Role definitions are now config-based (internal/config/roles/*.toml),
// not stored as beads. See config-based-roles.md for details.
//
// Agent beads use hard fail - installation aborts if creation fails.
// Agent beads are identity beads that track agent state, hooks, and
// form the foundation of the CV/reputation ledger. Without them, agents cannot
// be properly tracked or coordinated.
func initTownAgentBeads(townPath string) error {
	bd := beads.New(townPath)

	// bd init doesn't enable "custom" issue types by default, but Gas Town uses
	// agent beads during install and runtime. Ensure these types are enabled
	// before attempting to create any town-level system beads.
	if err := beads.EnsureCustomTypesConfigYAML(beads.ResolveBeadsDir(townPath)); err != nil {
		return err
	}

	// Town-level agent beads
	agentDefs := []struct {
		id       string
		roleType string
		title    string
	}{
		{
			id:       beads.MayorBeadIDTown(),
			roleType: "mayor",
			title:    "Mayor - global coordinator, handles cross-rig communication and escalations.",
		},
		{
			id:       beads.DeaconBeadIDTown(),
			roleType: "deacon",
			title:    "Deacon (daemon beacon) - receives mechanical heartbeats, runs town plugins and monitoring.",
		},
	}

	existingAgents, err := bd.List(beads.ListOptions{
		Status:   "all",
		Label:    "gt:agent",
		Priority: -1,
	})
	if err != nil {
		return fmt.Errorf("listing existing agent beads: %w", err)
	}
	existingAgentIDs := make(map[string]struct{}, len(existingAgents))
	for _, issue := range existingAgents {
		existingAgentIDs[issue.ID] = struct{}{}
	}

	for _, agent := range agentDefs {
		if _, ok := existingAgentIDs[agent.id]; ok {
			continue
		}

		fields := &beads.AgentFields{
			RoleType:   agent.roleType,
			Rig:        "", // Town-level agents have no rig
			AgentState: "idle",
			HookBead:   "",
			// Note: RoleBead field removed - role definitions are now config-based
		}

		if _, err := bd.CreateAgentBead(agent.id, agent.title, fields); err != nil {
			return fmt.Errorf("creating %s: %w", agent.id, err)
		}
		fmt.Printf("   ✓ Created agent bead: %s\n", agent.id)
	}

	return nil
}

func ensureBeadsCustomTypes(workDir string, types []string) error {
	if len(types) == 0 {
		return nil
	}

	for _, cfg := range []struct{ key, value string }{
		{"types.custom", strings.Join(types, ",")},
		{"types.infra", constants.BeadsInfraTypes},
	} {
		cmd := beads.Spawn("config", "set", cfg.key, cfg.value)
		cmd.Dir = workDir
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("bd config set %s failed: %s", cfg.key, strings.TrimSpace(string(output)))
		}
	}
	return nil
}
