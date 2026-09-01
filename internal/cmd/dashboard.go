package cmd

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/term"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/web"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var dashboardCmd = &cobra.Command{
	Use:     "dashboard",
	GroupID: GroupDiag,
	Short:   "Start the convoy tracking web dashboard",
	Long: `Start a web server that displays the convoy tracking dashboard.

The dashboard shows real-time convoy status with:
- Convoy list with status indicators
- Progress tracking for each convoy
- Last activity indicator (green/yellow/red)
- Auto-refresh every 30 seconds via htmx

Example:
  gt dashboard                    # Start on default port 8080
  gt dashboard --port 3000        # Start on port 3000
  gt dashboard --bind 0.0.0.0     # Listen on all interfaces
  gt dashboard --open             # Start and open browser`,
	RunE: runDashboard,
}

func init() {
	dashboardCmd.Flags().Int("port", 8080, "HTTP port to listen on")
	defaultBind := "127.0.0.1"
	if os.Getenv("IS_SANDBOX") != "" {
		defaultBind = "0.0.0.0"
	}
	dashboardCmd.Flags().String("bind", defaultBind, "Address to bind to (use 0.0.0.0 for all interfaces)")
	dashboardCmd.Flags().Bool("open", false, "Open browser automatically")
	rootCmd.AddCommand(dashboardCmd)
}

type dashboardOptions struct {
	port int
	bind string
	open bool
}

func readDashboardOptions(cmd *cobra.Command) (dashboardOptions, error) {
	port, err := cmd.Flags().GetInt("port")
	if err != nil {
		return dashboardOptions{}, err
	}
	bind, err := cmd.Flags().GetString("bind")
	if err != nil {
		return dashboardOptions{}, err
	}
	open, err := cmd.Flags().GetBool("open")
	if err != nil {
		return dashboardOptions{}, err
	}
	return dashboardOptions{port: port, bind: bind, open: open}, nil
}

func runDashboard(cmd *cobra.Command, _ []string) error {
	opts, err := readDashboardOptions(cmd)
	if err != nil {
		return err
	}
	handler, webCfg, err := dashboardHandler(cmd)
	if err != nil {
		return err
	}

	listenAddr, url := dashboardAddresses(opts)
	if opts.open {
		go openBrowser(url)
	}

	printDashboardBanner(url, listenAddr)
	return newDashboardServer(listenAddr, handler, webCfg).ListenAndServe()
}

func dashboardHandler(cmd *cobra.Command) (http.Handler, *config.WebTimeoutsConfig, error) {
	// Check if we're in a workspace - if not, run in setup mode
	var handler http.Handler
	var err error
	webCfg := config.DefaultWebTimeoutsConfig()

	townRoot, wsErr := workspace.FindFromCwdOrError()
	if wsErr != nil {
		// No workspace - run in setup mode
		handler, err = web.NewSetupMux()
		if err != nil {
			return nil, nil, fmt.Errorf("creating setup handler: %w", err)
		}
	} else {
		// In a workspace - run normal dashboard

		// Set BEADS_DOLT_PORT and GT_DOLT_PORT so bd/gt subprocesses connect
		// to the actual Dolt SQL server, not the dashboard's HTTP listen port.
		// Without this, inherited env vars could point bd at the wrong port.
		ensureDoltPortEnv(townRoot)

		fetcher, fetchErr := web.NewLiveConvoyFetcher()
		if fetchErr != nil {
			return nil, nil, fmt.Errorf("creating convoy fetcher: %w", fetchErr)
		}

		// Load web timeouts config (nil-safe: NewDashboardMux applies defaults)
		if ts, loadErr := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot)); loadErr == nil {
			if ts.WebTimeouts != nil {
				webCfg = ts.WebTimeouts
			}
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: loading town settings: %v (using defaults)\n", loadErr)
		}

		handler, err = web.NewDashboardMux(fetcher, webCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("creating dashboard handler: %w", err)
		}
	}
	return handler, webCfg, nil
}
func dashboardAddresses(opts dashboardOptions) (listenAddr, url string) {
	listenAddr = fmt.Sprintf("%s:%d", opts.bind, opts.port)
	displayHost := opts.bind
	if displayHost == "0.0.0.0" {
		displayHost = dashboardDisplayHost()
	}
	return listenAddr, fmt.Sprintf("http://%s:%d", displayHost, opts.port)
}

func dashboardDisplayHost() string {
	hostname, err := os.Hostname()
	if err == nil {
		return hostname
	}
	return "localhost"
}

func newDashboardServer(listenAddr string, handler http.Handler, webCfg *config.WebTimeoutsConfig) *http.Server {
	maxRunTimeout := config.ParseDurationOrDefault(webCfg.MaxRunTimeout, 120*time.Second)
	writeTimeout := maxRunTimeout + 15*time.Second
	if writeTimeout < 60*time.Second {
		writeTimeout = 60 * time.Second
	}

	return &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       120 * time.Second,
	}
}

func printDashboardBanner(url, listenAddr string) {
	// Only show the large banner if the terminal is wide enough (98 cols)
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err == nil && width >= 98 {
		fmt.Print(`
 __       __  ________  __        ______    ______   __       __  ________
|  \  _  |  \|        \|  \      /      \  /      \ |  \     /  \|        \
| $$ / \ | $$| $$$$$$$$| $$     |  $$$$$$\|  $$$$$$\| $$\   /  $$| $$$$$$$$
| $$/  $\| $$| $$__    | $$     | $$   \$$| $$  | $$| $$$\ /  $$$| $$__
| $$  $$$\ $$| $$  \   | $$     | $$      | $$  | $$| $$$$\  $$$$| $$  \
| $$ $$\$$\$$| $$$$$   | $$     | $$   __ | $$  | $$| $$\$$ $$ $$| $$$$$
| $$$$  \$$$$| $$_____ | $$_____| $$__/  \| $$__/ $$| $$ \$$$| $$| $$_____
| $$$    \$$$| $$     \| $$     \\$$    $$ \$$    $$| $$  \$ | $$| $$     \
 \$$      \$$ \$$$$$$$$ \$$$$$$$$ \$$$$$$   \$$$$$$  \$$      \$$ \$$$$$$$$

 ________   ______          ______    ______    ______   ________   ______   __       __  __    __
|        \ /      \        /      \  /      \  /      \ |        \ /      \ |  \  _  |  \|  \  |  \
 \$$$$$$$$|  $$$$$$\      |  $$$$$$\|  $$$$$$\|  $$$$$$\ \$$$$$$$$|  $$$$$$\| $$ / \ | $$| $$\ | $$
   | $$   | $$  | $$      | $$ __\$$| $$__| $$| $$___\$$   | $$   | $$  | $$| $$/  $\| $$| $$$\| $$
   | $$   | $$  | $$      | $$|    \| $$    $$ \$$    \    | $$   | $$  | $$| $$  $$$\ $$| $$$$\ $$
   | $$   | $$  | $$      | $$ \$$$$| $$$$$$$$ _\$$$$$$\   | $$   | $$  | $$| $$ $$\$$\$$| $$\$$ $$
   | $$   | $$__/ $$      | $$__| $$| $$  | $$|  \__| $$   | $$   | $$__/ $$| $$$$  \$$$$| $$ \$$$$
   | $$    \$$    $$       \$$    $$| $$  | $$ \$$    $$   | $$    \$$    $$| $$$    \$$$| $$  \$$$
    \$$     \$$$$$$         \$$$$$$  \$$   \$$  \$$$$$$     \$$     \$$$$$$  \$$      \$$ \$$   \$$

`)
	} else {
		fmt.Print("\n  WELCOME TO GASTOWN\n\n")
	}
	fmt.Printf("  launching dashboard at %s  •  api: %s/api/  •  listening on %s  •  ctrl+c to stop\n", url, url, listenAddr)
}

// ensureDoltPortEnv sets GT_DOLT_PORT, BEADS_DOLT_SERVER_PORT,
// BEADS_DOLT_PORT, and BEADS_DOLT_SERVER_HOST
// to the actual Dolt server connection info. This prevents bd subprocesses from
// inheriting stale or incorrect values from the environment.
// Uses the same resolver as AgentEnv and doltserver.DefaultConfig.
func ensureDoltPortEnv(townRoot string) {
	port := config.ResolveDoltPort(townRoot)
	if port <= 0 {
		port = doltserver.DefaultPort
	}
	portStr := strconv.Itoa(port)
	os.Setenv("GT_DOLT_PORT", portStr)
	os.Setenv("BEADS_DOLT_SERVER_PORT", portStr)
	os.Setenv("BEADS_DOLT_PORT", portStr)

	if host := config.ResolveDoltHost(townRoot); host != "" {
		os.Setenv("GT_DOLT_HOST", host)
		os.Setenv("BEADS_DOLT_SERVER_HOST", host)
	} else {
		os.Unsetenv("BEADS_DOLT_SERVER_HOST")
	}
}

// openBrowser opens the specified URL in the default browser.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		return
	}
	_ = cmd.Start()
}
