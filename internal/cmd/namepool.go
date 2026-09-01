package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var namepoolCmd = &cobra.Command{
	Use:     "namepool",
	GroupID: GroupWorkspace,
	Short:   "Manage polecat name pools",
	Long: `Manage themed name pools for polecats in Gas Town.

By default, polecats get themed names from the Mad Max universe
(furiosa, nux, slit, etc.). You can change the theme or add custom names.

Examples:
  gt namepool              # Show current pool status
  gt namepool --list       # List available themes
  gt namepool themes       # Show theme names
  gt namepool set minerals # Set theme to 'minerals'
  gt namepool add ember    # Add custom name to pool
  gt namepool reset        # Reset pool state`,
	RunE: runNamepool,
}

var namepoolThemesCmd = &cobra.Command{
	Use:   "themes [theme]",
	Short: "List available themes and their names",
	Long: `List available namepool themes or show names in a specific theme.

Without arguments, lists all themes with a preview of their names.
With a theme name argument, shows all names in that theme.`,
	RunE: runNamepoolThemes,
}

var namepoolSetCmd = &cobra.Command{
	Use:   "set <theme>",
	Short: "Set the namepool theme for this rig",
	Long: `Set the namepool theme used for naming new polecats in this rig.

Changes the theme and saves it to the rig settings. Existing polecat
names are not affected. Use 'gt namepool themes' to see available themes.`,
	Args: cobra.ExactArgs(1),
	RunE: runNamepoolSet,
}

var namepoolAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a custom name to the pool",
	Long: `Add a custom name to the rig's polecat name pool.

The name is appended to the pool and saved in the rig settings.
Duplicate names are silently ignored.`,
	Args: cobra.ExactArgs(1),
	RunE: runNamepoolAdd,
}

var namepoolResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Reset the pool state (release all names)",
	Long: `Reset the polecat name pool, releasing all claimed names.

All names become available for reuse. This does not change the theme
or remove custom names from the configuration.`,
	RunE: runNamepoolReset,
}

var namepoolCreateCmd = &cobra.Command{
	Use:   "create <name> [names...]",
	Short: "Create a custom theme",
	Long: `Create a custom namepool theme stored as a text file.

The theme is saved to <town>/settings/themes/<name>.txt and can be
used with 'gt namepool set <name>'. Names can be provided as arguments
or read from a file with --from-file.

Examples:
  gt namepool create tolkien aragorn legolas gimli gandalf frodo samwise
  gt namepool create tolkien --from-file ~/tolkien-names.txt`,
	Args: cobra.MinimumNArgs(1),
	RunE: runNamepoolCreate,
}

var namepoolDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a custom theme",
	Long: `Delete a custom namepool theme file.

Built-in themes cannot be deleted. If a rig is currently using the
theme, a warning is shown but deletion proceeds.`,
	Args: cobra.ExactArgs(1),
	RunE: runNamepoolDelete,
}

func init() {
	rootCmd.AddCommand(namepoolCmd)
	namepoolCmd.AddCommand(namepoolThemesCmd)
	namepoolCmd.AddCommand(namepoolSetCmd)
	namepoolCmd.AddCommand(namepoolAddCmd)
	namepoolCmd.AddCommand(namepoolResetCmd)
	namepoolCmd.AddCommand(namepoolCreateCmd)
	namepoolCmd.AddCommand(namepoolDeleteCmd)
	namepoolCmd.Flags().BoolP("list", "l", false, "List available themes")
	namepoolCreateCmd.Flags().String("from-file", "", "Read names from file instead of arguments")
}

func runNamepool(cmd *cobra.Command, _ []string) error {
	list, err := cmd.Flags().GetBool("list")
	if err != nil {
		return err
	}
	if list {
		return runNamepoolThemes(cmd, nil)
	}

	rigName, rigPath := detectCurrentRigWithPath()
	if rigName == "" {
		return fmt.Errorf("not in a rig directory")
	}
	return showNamepoolStatus(rigName, rigPath)
}

func showNamepoolStatus(rigName, rigPath string) error {
	settingsPath := filepath.Join(rigPath, "settings", "config.json")
	settings, _ := config.LoadRigSettings(settingsPath)
	pool := newNamepoolForStatus(rigName, rigPath, settings)

	if err := pool.Load(); err != nil {
		printDefaultNamepoolStatus(rigName)
		return nil
	}

	printNamepoolStatus(rigName, pool, settings != nil && settings.Namepool != nil)
	return nil
}

func newNamepoolForStatus(rigName, rigPath string, settings *config.RigSettings) *polecat.NamePool {
	if settings != nil && settings.Namepool != nil {
		return polecat.NewNamePoolWithConfig(rigPath, rigName, settings.Namepool.Style, settings.Namepool.Names, settings.Namepool.MaxBeforeNumbering)
	}
	return polecat.NewNamePool(rigPath, rigName)
}

func printDefaultNamepoolStatus(rigName string) {
	fmt.Printf("Rig: %s\n", rigName)
	fmt.Printf("Theme: %s (default)\n", polecat.DefaultTheme)
	fmt.Printf("Polecats: 0\n")
	fmt.Printf("Max pool size: %d\n", polecat.DefaultPoolSize)
}

func printNamepoolStatus(rigName string, pool *polecat.NamePool, configured bool) {
	fmt.Printf("Rig: %s\n", rigName)
	theme := pool.GetTheme()
	label := "custom"
	if polecat.IsBuiltinTheme(theme) {
		label = "built-in"
	}
	fmt.Printf("Theme: %s (%s)\n", theme, label)
	fmt.Printf("Polecats: %d\n", pool.ActiveCount())
	if activeNames := pool.ActiveNames(); len(activeNames) > 0 {
		fmt.Printf("In use: %s\n", strings.Join(activeNames, ", "))
	}
	if configured {
		fmt.Printf("(configured in settings/config.json)\n")
	}
}

func runNamepoolThemes(_ *cobra.Command, args []string) error {
	townRoot, _ := workspace.FindFromCwd()
	if len(args) == 0 {
		return listNamepoolThemes(townRoot)
	}
	return showNamepoolTheme(townRoot, args[0])
}

func listNamepoolThemes(townRoot string) error {
	themes := polecat.ListAllThemes(townRoot)
	fmt.Println("Available themes:")
	for _, theme := range themes {
		label := ""
		if theme.IsCustom {
			label = "custom, "
		}
		fmt.Printf("\n  %s (%s%d names):\n", theme.Name, label, theme.Count)
		names := namepoolThemeNames(townRoot, theme.Name, theme.IsCustom)
		if len(names) == 0 {
			continue
		}
		preview := names
		if len(preview) > 10 {
			preview = preview[:10]
		}
		fmt.Printf("    %s...\n", strings.Join(preview, ", "))
	}
	return nil
}

func namepoolThemeNames(townRoot, theme string, custom bool) []string {
	if custom && townRoot != "" {
		names, _ := polecat.ResolveThemeNames(townRoot, theme)
		return names
	}
	names, _ := polecat.GetThemeNames(theme)
	return names
}

func showNamepoolTheme(townRoot, theme string) error {
	names, err := resolveNamepoolTheme(townRoot, theme)
	if err != nil {
		return fmt.Errorf("unknown theme: %s (use 'gt namepool themes' to list available themes)", theme)
	}
	label := ""
	if !polecat.IsBuiltinTheme(theme) {
		label = " (custom)"
	}
	fmt.Printf("Theme: %s%s (%d names)\n\n", theme, label, len(names))
	for i, name := range names {
		if i > 0 && i%5 == 0 {
			fmt.Println()
		}
		fmt.Printf("  %-12s", name)
	}
	fmt.Println()
	return nil
}

func resolveNamepoolTheme(townRoot, theme string) ([]string, error) {
	if townRoot != "" {
		return polecat.ResolveThemeNames(townRoot, theme)
	}
	return polecat.GetThemeNames(theme)
}

func runNamepoolSet(_ *cobra.Command, args []string) error {
	theme := args[0]
	rigName, rigPath := detectCurrentRigWithPath()
	if rigName == "" {
		return fmt.Errorf("not in a rig directory")
	}
	townRoot, _ := workspace.FindFromCwd()
	if _, err := resolveNamepoolTheme(townRoot, theme); err != nil {
		return fmt.Errorf("unknown theme: %s (use 'gt namepool themes' to list available themes)", theme)
	}
	pool := polecat.NewNamePool(rigPath, rigName)
	if townRoot != "" {
		pool.SetTownRoot(townRoot)
	}
	if err := loadNamepool(pool); err != nil {
		return err
	}
	if err := pool.SetTheme(theme); err != nil {
		return err
	}
	if err := pool.Save(); err != nil {
		return fmt.Errorf("saving pool: %w", err)
	}
	settingsPath := filepath.Join(rigPath, "settings", "config.json")
	existingNames := loadConfiguredNamepoolNames(settingsPath)
	if err := saveRigNamepoolConfig(rigPath, theme, existingNames); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	fmt.Printf("Theme '%s' set for rig '%s'\n", theme, rigName)
	fmt.Printf("New polecats will use names from this theme.\n")

	return nil
}

func loadNamepool(pool *polecat.NamePool) error {
	if err := pool.Load(); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("loading pool: %w", err)
	}
	return nil
}

func loadConfiguredNamepoolNames(settingsPath string) []string {
	settings, err := config.LoadRigSettings(settingsPath)
	if err == nil && settings.Namepool != nil {
		return settings.Namepool.Names
	}
	return nil
}

func runNamepoolAdd(_ *cobra.Command, args []string) error {
	name := strings.ToLower(args[0])
	if err := polecat.ValidatePoolName(name); err != nil {
		return err
	}

	rigName, rigPath := detectCurrentRigWithPath()
	if rigName == "" {
		return fmt.Errorf("not in a rig directory")
	}

	settingsPath := filepath.Join(rigPath, "settings", "config.json")
	settings, err := loadNamepoolSettings(settingsPath)
	if err != nil {
		return err
	}
	if settings.Namepool == nil {
		settings.Namepool = config.DefaultNamepoolConfig()
	}

	if handled, err := addToCustomNamepoolTheme(settings.Namepool, name); err != nil {
		return err
	} else if handled {
		return nil
	}

	if namepoolContains(settings.Namepool.Names, name) {
		fmt.Printf("Name '%s' already in pool\n", name)
		return nil
	}
	settings.Namepool.Names = append(settings.Namepool.Names, name)
	if err := config.SaveRigSettings(settingsPath, settings); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}

	fmt.Printf("Added '%s' to the name pool\n", name)
	return nil
}

func loadNamepoolSettings(settingsPath string) (*config.RigSettings, error) {
	settings, err := config.LoadRigSettings(settingsPath)
	if err == nil {
		return settings, nil
	}
	if os.IsNotExist(err) || strings.Contains(err.Error(), "not found") {
		return config.NewRigSettings(), nil
	}
	return nil, fmt.Errorf("loading settings: %w", err)
}

func addToCustomNamepoolTheme(namepool *config.NamepoolConfig, name string) (bool, error) {
	style := namepool.Style
	if style == "" || polecat.IsBuiltinTheme(style) || len(namepool.Names) > 0 {
		return false, nil
	}
	townRoot, _ := workspace.FindFromCwd()
	if townRoot == "" {
		return false, nil
	}
	alreadyExists, err := polecat.AppendToCustomTheme(townRoot, style, name)
	if err != nil {
		return false, fmt.Errorf("appending to custom theme %q: %w", style, err)
	}
	if alreadyExists {
		fmt.Printf("Name '%s' already in theme '%s'\n", name, style)
	} else {
		fmt.Printf("Added '%s' to custom theme '%s'\n", name, style)
	}
	return true, nil
}

func namepoolContains(names []string, target string) bool {
	for _, name := range names {
		if name == target {
			return true
		}
	}
	return false
}

func runNamepoolReset(_ *cobra.Command, _ []string) error {
	rigName, rigPath := detectCurrentRigWithPath()
	if rigName == "" {
		return fmt.Errorf("not in a rig directory")
	}

	// Load pool
	pool := polecat.NewNamePool(rigPath, rigName)
	if err := pool.Load(); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("loading pool: %w", err)
	}

	pool.Reset()

	if err := pool.Save(); err != nil {
		return fmt.Errorf("saving pool: %w", err)
	}

	fmt.Printf("Pool reset for rig '%s'\n", rigName)
	fmt.Printf("All names released and available for reuse.\n")
	return nil
}

// detectCurrentRigWithPath determines the rig name and path from cwd.
func detectCurrentRigWithPath() (string, string) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", ""
	}

	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return "", ""
	}

	// Get path relative to town root
	rel, err := filepath.Rel(townRoot, cwd)
	if err != nil {
		return "", ""
	}

	// Extract first path component (rig name)
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > 0 && parts[0] != "." && parts[0] != constants.RoleMayor && parts[0] != constants.RoleDeacon {
		return parts[0], filepath.Join(townRoot, parts[0])
	}

	return "", ""
}

func runNamepoolCreate(cmd *cobra.Command, args []string) error {
	themeName := args[0]
	if polecat.IsBuiltinTheme(themeName) {
		return fmt.Errorf("cannot create custom theme %q: conflicts with built-in theme", themeName)
	}
	if err := polecat.ValidatePoolName(themeName); err != nil {
		return fmt.Errorf("invalid theme name: %w", err)
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	fromFile, err := cmd.Flags().GetString("from-file")
	if err != nil {
		return err
	}
	names, err := readNamepoolThemeNames(args, fromFile)
	if err != nil {
		return err
	}

	if err := polecat.SaveCustomTheme(townRoot, themeName, names); err != nil {
		return err
	}

	fmt.Printf("Created custom theme '%s' with %d names\n", themeName, len(names))
	fmt.Printf("Use 'gt namepool set %s' to activate it for a rig.\n", themeName)
	return nil
}

func readNamepoolThemeNames(args []string, fromFile string) ([]string, error) {
	if fromFile != "" {
		names, err := polecat.ParseThemeFile(fromFile)
		if err != nil {
			return nil, fmt.Errorf("reading names from file: %w", err)
		}
		return names, nil
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("provide names as arguments or use --from-file")
	}
	var names []string
	for _, name := range args[1:] {
		name = strings.ToLower(name)
		if err := polecat.ValidatePoolName(name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

func runNamepoolDelete(_ *cobra.Command, args []string) error {
	themeName := args[0]

	// Validate theme name to prevent path traversal (e.g., "../../etc/foo")
	if err := polecat.ValidatePoolName(themeName); err != nil {
		return fmt.Errorf("invalid theme name: %w", err)
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	// Check if any rigs are using this theme and warn
	if using := polecat.FindRigsUsingTheme(townRoot, themeName); len(using) > 0 {
		fmt.Fprintf(os.Stderr, "warning: theme '%s' is currently used by: %s\n", themeName, strings.Join(using, ", "))
		fmt.Fprintf(os.Stderr, "  Those rigs will fall back to the default theme (%s).\n", polecat.DefaultTheme)
	}

	if err := polecat.DeleteCustomTheme(townRoot, themeName); err != nil {
		return err
	}

	fmt.Printf("Deleted custom theme '%s'\n", themeName)
	return nil
}

// saveRigNamepoolConfig saves the namepool config to rig settings.
func saveRigNamepoolConfig(rigPath, theme string, customNames []string) error {
	settingsPath := filepath.Join(rigPath, "settings", "config.json")

	// Load existing settings or create new
	var settings *config.RigSettings
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil {
		// Create new settings if not found
		if os.IsNotExist(err) || strings.Contains(err.Error(), "not found") {
			settings = config.NewRigSettings()
		} else {
			return fmt.Errorf("loading settings: %w", err)
		}
	}

	// Set namepool
	settings.Namepool = &config.NamepoolConfig{
		Style: theme,
		Names: customNames,
	}

	// Save (creates directory if needed)
	if err := config.SaveRigSettings(settingsPath, settings); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}

	return nil
}
