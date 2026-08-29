package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"
)

// Valid CLI theme modes
var validCLIThemes = []string{"auto", "dark", "light"}

var themeCmd = &cobra.Command{
	Use:     "theme [name]",
	GroupID: GroupConfig,
	Short:   "View or set tmux theme for the current rig",
	Long: `Manage tmux status bar themes for Gas Town sessions.

Without arguments, shows the current theme assignment.
With a name argument, sets the theme for this rig.

Examples:
  gt theme              # Show current theme
  gt theme --list       # List available themes
  gt theme forest       # Set theme to 'forest'
  gt theme none         # Disable tmux theming for this rig
  gt theme apply        # Apply theme to all running sessions in this rig`,
	RunE: runTheme,
}

var themeApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply theme to running sessions",
	Long: `Apply theme to running Gas Town sessions.

By default, only applies to sessions in the current rig.
Use --all to apply to sessions across all rigs.`,
	RunE: runThemeApply,
}

var themeCLICmd = &cobra.Command{
	Use:   "cli [mode]",
	Short: "View or set CLI color scheme (dark/light/auto)",
	Long: `Manage CLI output color scheme for Gas Town commands.

Without arguments, shows the current CLI theme mode and detection.
With a mode argument, sets the CLI theme preference.

Modes:
  auto   - Automatically detect terminal background (default)
  dark   - Force dark mode colors (light text for dark backgrounds)
  light  - Force light mode colors (dark text for light backgrounds)

The setting is stored in town settings (settings/config.json) and can
be overridden per-session via the GT_THEME environment variable.

Examples:
  gt theme cli              # Show current CLI theme
  gt theme cli dark         # Set CLI theme to dark mode
  gt theme cli auto         # Reset to auto-detection
  GT_THEME=light gt status  # Override for a single command`,
	RunE: runThemeCLI,
}

func init() {
	rootCmd.AddCommand(themeCmd)
	themeCmd.AddCommand(themeApplyCmd)
	themeCmd.AddCommand(themeCLICmd)
	themeCmd.Flags().BoolP("list", "l", false, "List available themes")
	themeApplyCmd.Flags().BoolP("all", "a", false, "Apply to all rigs, not just current")

}

func runTheme(cmd *cobra.Command, args []string) error {
	list := commandBoolFlag(cmd, "list")
	// List mode
	if list {
		fmt.Println("Available themes:")
		for _, name := range tmux.ListThemeNames() {
			theme := tmux.GetThemeByName(name)
			fmt.Printf("  %-10s  %s\n", name, theme.Style())
		}
		fmt.Printf("  %-10s  disable tmux theming\n", "none")
		// Also show Mayor theme
		mayor := tmux.MayorTheme()
		fmt.Printf("  %-10s  %s (Mayor only)\n", mayor.Name, mayor.Style())
		return nil
	}

	// Determine current rig
	rigName := detectCurrentRig()
	if rigName == "" {
		rigName = "unknown"
	}

	// Show current theme assignment
	if len(args) == 0 {
		desc := describeRigTheme(rigName)
		fmt.Printf("Rig: %s\n", rigName)
		fmt.Printf("Theme: %s\n", desc)
		return nil
	}

	// Set theme
	themeName := args[0]
	if !strings.EqualFold(themeName, "none") && tmux.GetThemeByName(themeName) == nil {
		return fmt.Errorf("unknown theme: %s (use --list to see available themes)", themeName)
	}

	// Save to rig config
	if err := saveRigTheme(rigName, themeName); err != nil {
		return fmt.Errorf("saving theme config: %w", err)
	}

	if strings.EqualFold(themeName, "none") {
		fmt.Printf("Tmux theming disabled for rig '%s'\n", rigName)
	} else {
		fmt.Printf("Theme '%s' saved for rig '%s'\n", themeName, rigName)
	}
	fmt.Println("Run 'gt theme apply' to apply to running sessions")

	return nil
}

func runThemeApply(cmd *cobra.Command, _ []string) error {
	applyAll := commandBoolFlag(cmd, "all")
	t := tmux.NewTmux()
	townRoot, _ := workspace.FindFromCwd()

	// Get all sessions
	sessions, err := t.ListSessions()
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	// Determine current rig
	rigName := detectCurrentRig()

	// Apply to matching sessions
	applied := 0
	for _, sess := range sessions {
		cfg, ok := resolveThemeSession(sess, townRoot, rigName, applyAll)
		if !ok {
			continue
		}
		if err := t.ConfigureGasTownSession(sess, cfg.theme, cfg.rig, cfg.worker, cfg.role); err != nil {
			fmt.Printf("  %s: failed (%v)\n", sess, err)
			continue
		}
		printThemeApplication(sess, cfg.theme)
		applied++
	}

	printThemeApplicationSummary(applied)

	return nil
}

type themeSessionConfig struct {
	theme  *tmux.Theme
	rig    string
	worker string
	role   string
}

func resolveThemeSession(sessionName, townRoot, currentRig string, applyAll bool) (themeSessionConfig, bool) {
	if !session.IsKnownSession(sessionName) {
		return themeSessionConfig{}, false
	}
	identity, err := session.ParseSessionName(sessionName)
	if err != nil {
		return themeSessionConfig{}, false
	}
	if identity.Role == session.RoleMayor || identity.Role == session.RoleDeacon {
		cfg := resolveTownThemeSession(townRoot, identity.Role)
		applyWindowTint(cfg.theme, cfg.rig, cfg.role)
		return cfg, true
	}
	return resolveRigThemeSession(townRoot, identity, currentRig, applyAll)
}

func resolveTownThemeSession(townRoot string, role session.Role) themeSessionConfig {
	roleName := string(role)
	worker := "Mayor"
	if role == session.RoleDeacon {
		worker = "Deacon"
	}
	return themeSessionConfig{
		theme:  tmux.ResolveSessionTheme(townRoot, "", roleName, ""),
		worker: worker,
		role:   roleName,
	}
}

func resolveRigThemeSession(townRoot string, identity *session.AgentIdentity, currentRig string, applyAll bool) (themeSessionConfig, bool) {
	if !applyAll && currentRig != "" && identity.Rig != currentRig {
		return themeSessionConfig{}, false
	}

	role := string(identity.Role)
	worker, crewMember := rigThemeWorker(identity)
	theme := tmux.ResolveSessionTheme(townRoot, identity.Rig, role, crewMember)
	applyWindowTint(theme, identity.Rig, role)
	return themeSessionConfig{theme: theme, rig: identity.Rig, worker: worker, role: role}, true
}

func rigThemeWorker(identity *session.AgentIdentity) (string, string) {
	switch identity.Role {
	case session.RoleWitness:
		return constants.RoleWitness, ""
	case session.RoleRefinery:
		return constants.RoleRefinery, ""
	default:
		return identity.Name, identity.Name
	}
}

func applyWindowTint(theme *tmux.Theme, rigName, role string) {
	if theme == nil {
		return
	}
	theme.Window = session.ResolveWindowTint(rigName, role)
	if theme.Window != nil || !session.IsWindowTintEnabled(rigName) {
		return
	}
	factor := session.ResolveTintFactor(rigName)
	theme.Window = &tmux.WindowStyle{BG: tmux.DarkenColor(theme.BG, factor), FG: theme.FG}
}

func printThemeApplication(sessionName string, theme *tmux.Theme) {
	if theme == nil {
		fmt.Printf("  %s: disabled tmux theming\n", sessionName)
		return
	}
	fmt.Printf("  %s: applied %s theme\n", sessionName, theme.Name)
}

func printThemeApplicationSummary(applied int) {
	if applied == 0 {
		fmt.Println("No matching sessions found")
		return
	}
	fmt.Printf("\nApplied theme to %d session(s)\n", applied)
}

// detectCurrentRig determines the rig from environment or cwd.
func detectCurrentRig() string {
	if rig := os.Getenv("GT_RIG"); rig != "" {
		return rig
	}
	if rig := detectRigFromSession(); rig != "" {
		return rig
	}
	return detectRigFromCwd()
}

func detectRigFromSession() string {
	sessName := detectCurrentSession()
	if sessName == "" {
		return ""
	}
	identity, err := session.ParseSessionName(sessName)
	if err != nil {
		return ""
	}
	return identity.Rig
}

func detectRigFromCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	// Find town root to extract rig name
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return ""
	}

	// Get path relative to town root
	rel, err := filepath.Rel(townRoot, cwd)
	if err != nil {
		return ""
	}

	// Extract first path component (rig name)
	// Patterns: <rig>/..., mayor/..., deacon/...
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > 0 && parts[0] != "." && parts[0] != constants.RoleMayor && parts[0] != constants.RoleDeacon {
		return parts[0]
	}

	return ""
}

func describeRigTheme(rigName string) string {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		theme := tmux.AssignTheme(rigName)
		return fmt.Sprintf("%s (%s, default auto-assignment)", theme.Name, theme.Style())
	}

	settingsPath := filepath.Join(townRoot, rigName, "settings", "config.json")
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil {
		theme := tmux.AssignTheme(rigName)
		return fmt.Sprintf("%s (%s, default auto-assignment)", theme.Name, theme.Style())
	}

	if settings.Theme == nil {
		theme := tmux.AssignTheme(rigName)
		return fmt.Sprintf("%s (%s, default auto-assignment)", theme.Name, theme.Style())
	}
	if settings.Theme.Disabled {
		return "none (configured)"
	}
	if settings.Theme.Custom != nil {
		return fmt.Sprintf("custom (bg=%s, fg=%s)", settings.Theme.Custom.BG, settings.Theme.Custom.FG)
	}
	if settings.Theme.Name != "" {
		if theme := tmux.GetThemeByName(settings.Theme.Name); theme != nil {
			return fmt.Sprintf("%s (%s, configured)", theme.Name, theme.Style())
		}
		return fmt.Sprintf("%s (configured)", settings.Theme.Name)
	}
	theme := tmux.AssignTheme(rigName)
	return fmt.Sprintf("%s (%s, auto-assignment)", theme.Name, theme.Style())
}

// saveRigTheme saves the theme name to rig settings.
func saveRigTheme(rigName, themeName string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return fmt.Errorf("not in a Gas Town workspace")
	}

	settingsPath := filepath.Join(townRoot, rigName, "settings", "config.json")

	// Load existing settings or create new
	var settings *config.RigSettings
	settings, err = config.LoadRigSettings(settingsPath)
	if err != nil {
		// Create new settings if not found
		if os.IsNotExist(err) || strings.Contains(err.Error(), "not found") {
			settings = config.NewRigSettings()
		} else {
			return fmt.Errorf("loading settings: %w", err)
		}
	}

	// Update theme name, preserving existing RoleThemes and Custom
	if settings.Theme == nil {
		settings.Theme = &config.ThemeConfig{}
	}
	if strings.EqualFold(themeName, "none") {
		settings.Theme.Disabled = true
		settings.Theme.Name = ""
		settings.Theme.Custom = nil
	} else {
		settings.Theme.Disabled = false
		settings.Theme.Name = themeName
		settings.Theme.Custom = nil
	}

	// Save
	if err := config.SaveRigSettings(settingsPath, settings); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}

	return nil
}

func runThemeCLI(_ *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return fmt.Errorf("not in a Gas Town workspace")
	}

	settingsPath := config.TownSettingsPath(townRoot)
	if len(args) == 0 {
		return showCLITheme(settingsPath)
	}
	return setCLITheme(settingsPath, args[0])
}

func showCLITheme(settingsPath string) error {
	settings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}
	configValue := settings.CLITheme
	if configValue == "" {
		configValue = "auto"
	}
	envValue := os.Getenv("GT_THEME")
	effectiveMode := configValue
	if envValue != "" {
		effectiveMode = strings.ToLower(envValue)
	}

	fmt.Printf("CLI Theme:\n")
	fmt.Printf("  Configured: %s\n", configValue)
	if envValue != "" {
		fmt.Printf("  Override:   %s (via GT_THEME)\n", envValue)
	}
	fmt.Printf("  Effective:  %s\n", effectiveMode)
	if effectiveMode == "auto" {
		printDetectedTerminalBackground()
	}
	return nil
}

func printDetectedTerminalBackground() {
	detected := "light"
	if detectTerminalBackground() {
		detected = "dark"
	}
	fmt.Printf("  Detected:   %s background\n", detected)
}

func setCLITheme(settingsPath, requestedMode string) error {
	mode := strings.ToLower(requestedMode)
	if !isValidCLITheme(mode) {
		return fmt.Errorf("invalid CLI theme '%s' (valid: auto, dark, light)", mode)
	}

	settings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}

	settings.CLITheme = mode
	if err := config.SaveTownSettings(settingsPath, settings); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}

	fmt.Printf("CLI theme set to '%s'\n", mode)
	printCLIThemeAdvice(mode)
	return nil
}

func printCLIThemeAdvice(mode string) {
	if mode == "auto" {
		fmt.Println("Colors will adapt to your terminal's background.")
		return
	}
	fmt.Printf("Colors optimized for %s backgrounds.\n", mode)
}

// isValidCLITheme checks if a CLI theme mode is valid.
func isValidCLITheme(mode string) bool {
	for _, valid := range validCLIThemes {
		if mode == valid {
			return true
		}
	}
	return false
}

// detectTerminalBackground returns true if terminal has dark background.
func detectTerminalBackground() bool {
	// Use termenv for detection
	return termenv.HasDarkBackground()
}
