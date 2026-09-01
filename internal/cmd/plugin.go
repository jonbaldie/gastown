package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/plugin"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var pluginCmd = &cobra.Command{
	Use:     "plugin",
	GroupID: GroupConfig,
	Short:   "Plugin management",
	Long: `Manage plugins that run during Deacon patrol cycles.

Plugins are periodic automation tasks defined by plugin.md files with TOML frontmatter.

PLUGIN LOCATIONS:
  ~/gt/plugins/           Town-level plugins (universal, apply everywhere)
  <rig>/plugins/          Rig-level plugins (project-specific)

GATE TYPES:
  cooldown    Run if enough time has passed (e.g., 1h)
  cron        Run on a schedule (e.g., "0 9 * * *")
  condition   Run if a check command returns exit 0
  event       Run on events (e.g., startup)
  manual      Never auto-run, trigger explicitly

Examples:
  gt plugin list                    # List all discovered plugins
  gt plugin show <name>             # Show plugin details
  gt plugin list --json             # JSON output`,
	RunE: requireSubcommand,
}

var pluginListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all discovered plugins",
	Long: `List all plugins from town and rig plugin directories.

Plugins are discovered from:
  - ~/gt/plugins/ (town-level)
  - <rig>/plugins/ for each registered rig

When a plugin exists at both levels, the rig-level version takes precedence.

Examples:
  gt plugin list              # Human-readable output
  gt plugin list --json       # JSON output for scripting`,
	RunE: runPluginList,
}

var pluginShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show plugin details",
	Long: `Show detailed information about a plugin.

Displays the plugin's configuration, gate settings, and instructions.

Examples:
  gt plugin show rebuild-gt
  gt plugin show rebuild-gt --json`,
	Args: cobra.ExactArgs(1),
	RunE: runPluginShow,
}

var pluginRunCmd = &cobra.Command{
	Use:   "run <name>",
	Short: "Manually trigger plugin execution",
	Long: `Manually trigger a plugin to run.

By default, checks if the gate would allow execution and informs you
if it wouldn't. Use --force to bypass gate checks.

Examples:
  gt plugin run rebuild-gt              # Run if gate allows
  gt plugin run rebuild-gt --force      # Bypass gate check
  gt plugin run rebuild-gt --dry-run    # Show what would happen`,
	Args: cobra.ExactArgs(1),
	RunE: runPluginRun,
}

var pluginSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync plugins from source repo to runtime directories",
	Long: `Copy plugins from the gastown source repository to runtime plugin directories.

By default, auto-detects the source by walking up from the current directory
looking for a gastown repo, or checks known locations within the town.

Syncs to town-level plugins (~/gt/plugins/) so all rigs see the latest plugins.

Examples:
  gt plugin sync                           # Auto-detect source, sync to town
  gt plugin sync --source ./plugins        # Explicit source directory
  gt plugin sync --clean                   # Remove plugins not in source
  gt plugin sync --dry-run                 # Show what would happen`,
	RunE: runPluginSync,
}

var pluginHistoryCmd = &cobra.Command{
	Use:   "history <name>",
	Short: "Show plugin execution history",
	Long: `Show recent execution history for a plugin.

Queries ephemeral beads (wisps) that record plugin runs.

Examples:
  gt plugin history rebuild-gt
  gt plugin history rebuild-gt --json
  gt plugin history rebuild-gt --limit 20`,
	Args: cobra.ExactArgs(1),
	RunE: runPluginHistory,
}

var pluginRecordRunCmd = &cobra.Command{
	Use:   "record-run",
	Short: "Record a plugin run receipt",
	Long: `Record a plugin run receipt through the canonical plugin recorder.

The recorder creates an ephemeral type:plugin-run bead, closes it immediately,
and leaves the receipt available to plugin history/cooldown queries that use
closed beads. This keeps plugin scripts from leaking open run-log beads.`,
	RunE: runPluginRecordRun,
}

func init() {
	// List subcommand flags
	pluginListCmd.Flags().Bool("json", false, "Output as JSON")

	// Show subcommand flags
	pluginShowCmd.Flags().Bool("json", false, "Output as JSON")

	// Run subcommand flags
	pluginRunCmd.Flags().Bool("force", false, "Bypass gate check")
	pluginRunCmd.Flags().Bool("dry-run", false, "Show what would happen without executing")

	// History subcommand flags
	pluginHistoryCmd.Flags().Bool("json", false, "Output as JSON")
	pluginHistoryCmd.Flags().Int("limit", 10, "Maximum number of runs to show")

	// Record-run subcommand flags
	pluginRecordRunCmd.Flags().String("plugin", "", "Plugin name")
	pluginRecordRunCmd.Flags().String("result", "", "Run result label value")
	pluginRecordRunCmd.Flags().String("title", "", "Receipt title")
	pluginRecordRunCmd.Flags().String("description", "", "Receipt description")
	pluginRecordRunCmd.Flags().String("rig", "", "Rig label value")
	pluginRecordRunCmd.Flags().StringArrayP("label", "l", nil, "Additional label for the receipt")

	// Sync subcommand flags
	pluginSyncCmd.Flags().String("source", "", "Source plugins directory (auto-detected if omitted)")
	pluginSyncCmd.Flags().Bool("clean", false, "Remove plugins from target that don't exist in source")
	pluginSyncCmd.Flags().Bool("dry-run", false, "Show what would happen without syncing")

	// Add subcommands
	pluginCmd.AddCommand(pluginListCmd)
	pluginCmd.AddCommand(pluginShowCmd)
	pluginCmd.AddCommand(pluginRunCmd)
	pluginCmd.AddCommand(pluginHistoryCmd)
	pluginCmd.AddCommand(pluginRecordRunCmd)
	pluginCmd.AddCommand(pluginSyncCmd)

	rootCmd.AddCommand(pluginCmd)
}

// getPluginScanner creates a scanner with town root and all rig names.
func getPluginScanner() (*plugin.Scanner, string, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load rigs config to get rig names
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	// Extract rig names
	rigNames := make([]string, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)

	scanner := plugin.NewScanner(townRoot, rigNames)
	return scanner, townRoot, nil
}

func runPluginList(cmd *cobra.Command, _ []string) error {
	scanner, townRoot, err := getPluginScanner()
	if err != nil {
		return err
	}

	plugins, err := scanner.DiscoverAll()
	if err != nil {
		return fmt.Errorf("discovering plugins: %w", err)
	}

	// Sort plugins by name
	sort.Slice(plugins, func(i, j int) bool {
		return plugins[i].Name < plugins[j].Name
	})

	if commandBoolFlag(cmd, "json") {
		return outputPluginListJSON(plugins)
	}

	return outputPluginListText(plugins, townRoot)
}

func outputPluginListJSON(plugins []*plugin.Plugin) error {
	summaries := make([]plugin.PluginSummary, len(plugins))
	for i, p := range plugins {
		summaries[i] = p.Summary()
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(summaries)
}

func outputPluginListText(plugins []*plugin.Plugin, townRoot string) error {
	if len(plugins) == 0 {
		fmt.Printf("%s No plugins discovered\n", style.Dim.Render("○"))
		fmt.Printf("\n  Plugin directories:\n")
		fmt.Printf("    %s/plugins/\n", townRoot)
		fmt.Printf("\n  Create a plugin by adding a directory with plugin.md\n")
		return nil
	}

	fmt.Printf("%s Discovered %d plugin(s)\n\n", style.Success.Render("●"), len(plugins))

	// Group by location
	townPlugins := make([]*plugin.Plugin, 0)
	rigPlugins := make(map[string][]*plugin.Plugin)

	for _, p := range plugins {
		if p.Location == plugin.LocationTown {
			townPlugins = append(townPlugins, p)
		} else {
			rigPlugins[p.RigName] = append(rigPlugins[p.RigName], p)
		}
	}

	// Print town-level plugins
	if len(townPlugins) > 0 {
		fmt.Printf("  %s\n", style.Bold.Render("Town-level plugins:"))
		for _, p := range townPlugins {
			printPluginSummary(p)
		}
		fmt.Println()
	}

	// Print rig-level plugins by rig
	rigNames := make([]string, 0, len(rigPlugins))
	for name := range rigPlugins {
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)

	for _, rigName := range rigNames {
		fmt.Printf("  %s\n", style.Bold.Render(fmt.Sprintf("Rig %s:", rigName)))
		for _, p := range rigPlugins[rigName] {
			printPluginSummary(p)
		}
		fmt.Println()
	}

	return nil
}

func printPluginSummary(p *plugin.Plugin) {
	gateType := "manual"
	if p.Gate != nil && p.Gate.Type != "" {
		gateType = string(p.Gate.Type)
	}

	desc := p.Description
	if len(desc) > 50 {
		desc = desc[:47] + "..."
	}

	typeTag := gateType
	if p.IsExecWrapper() {
		typeTag = "exec-wrapper"
	}

	fmt.Printf("    %s %s\n", style.Bold.Render(p.Name), style.Dim.Render(fmt.Sprintf("[%s]", typeTag)))
	if desc != "" {
		fmt.Printf("      %s\n", style.Dim.Render(desc))
	}
}

func runPluginShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	scanner, _, err := getPluginScanner()
	if err != nil {
		return err
	}

	p, err := scanner.GetPlugin(name)
	if err != nil {
		return err
	}

	if commandBoolFlag(cmd, "json") {
		return outputPluginShowJSON(p)
	}

	return outputPluginShowText(p)
}

func outputPluginShowJSON(p *plugin.Plugin) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

func outputPluginShowText(p *plugin.Plugin) error {
	printPluginBasics(p)
	printPluginGate(p)
	printPluginTracking(p)
	printPluginExecution(p)
	printPluginInstructions(p)
	return nil
}

func printPluginBasics(p *plugin.Plugin) {
	fmt.Printf("%s %s\n", style.Bold.Render("Plugin:"), p.Name)
	fmt.Printf("%s %s\n", style.Bold.Render("Path:"), p.Path)

	if p.Description != "" {
		fmt.Printf("%s %s\n", style.Bold.Render("Description:"), p.Description)
	}

	// Location
	locStr := string(p.Location)
	if p.RigName != "" {
		locStr = fmt.Sprintf("%s (%s)", p.Location, p.RigName)
	}
	fmt.Printf("%s %s\n", style.Bold.Render("Location:"), locStr)

	fmt.Printf("%s %d\n", style.Bold.Render("Version:"), p.Version)
}

func printPluginGate(p *plugin.Plugin) {
	fmt.Println()
	fmt.Printf("%s\n", style.Bold.Render("Gate:"))
	if p.Gate != nil {
		fmt.Printf("  Type: %s\n", p.Gate.Type)
		if p.Gate.Duration != "" {
			fmt.Printf("  Duration: %s\n", p.Gate.Duration)
		}
		if p.Gate.Schedule != "" {
			fmt.Printf("  Schedule: %s\n", p.Gate.Schedule)
		}
		if p.Gate.Check != "" {
			fmt.Printf("  Check: %s\n", p.Gate.Check)
		}
		if p.Gate.On != "" {
			fmt.Printf("  On: %s\n", p.Gate.On)
		}
	} else {
		fmt.Printf("  Type: manual (no gate section)\n")
	}
}

func printPluginTracking(p *plugin.Plugin) {
	if p.Tracking != nil {
		fmt.Println()
		fmt.Printf("%s\n", style.Bold.Render("Tracking:"))
		if len(p.Tracking.Labels) > 0 {
			fmt.Printf("  Labels: %s\n", strings.Join(p.Tracking.Labels, ", "))
		}
		fmt.Printf("  Digest: %v\n", p.Tracking.Digest)
	}
}

func printPluginExecution(p *plugin.Plugin) {
	if p.Execution != nil {
		fmt.Println()
		fmt.Printf("%s\n", style.Bold.Render("Execution:"))
		if p.Execution.Type != "" {
			fmt.Printf("  Type: %s\n", p.Execution.Type)
		}
		if len(p.Execution.Wrapper) > 0 {
			fmt.Printf("  Wrapper: %s\n", strings.Join(p.Execution.Wrapper, " "))
		}
		if p.Execution.Timeout != "" {
			fmt.Printf("  Timeout: %s\n", p.Execution.Timeout)
		}
		fmt.Printf("  Notify on failure: %v\n", p.Execution.NotifyOnFailure)
		if p.Execution.Severity != "" {
			fmt.Printf("  Severity: %s\n", p.Execution.Severity)
		}
	}
}

func printPluginInstructions(p *plugin.Plugin) {
	if p.Instructions != "" {
		fmt.Println()
		fmt.Printf("%s\n", style.Bold.Render("Instructions:"))
		lines := strings.Split(p.Instructions, "\n")
		preview := lines
		if len(lines) > 10 {
			preview = lines[:10]
		}
		for _, line := range preview {
			fmt.Printf("  %s\n", line)
		}
		if len(lines) > 10 {
			fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("... (%d more lines)", len(lines)-10)))
		}
	}
}

func runPluginRun(cmd *cobra.Command, args []string) error {
	name := args[0]
	force := commandBoolFlag(cmd, "force")
	dryRun := commandBoolFlag(cmd, "dry-run")

	scanner, townRoot, err := getPluginScanner()
	if err != nil {
		return err
	}

	p, err := scanner.GetPlugin(name)
	if err != nil {
		return err
	}

	gateOpen, gateReason := pluginGateStatus(p, townRoot, force)

	if dryRun {
		printPluginDryRun(p, gateOpen, gateReason)
		return nil
	}

	if !gateOpen && !force {
		fmt.Printf("%s Gate closed: %s\n", style.Warning.Render("⚠"), gateReason)
		fmt.Printf("  Use --force to bypass gate check\n")
		return nil
	}

	printPluginRun(p, force, gateOpen)
	recordPluginRun(p, townRoot)
	return nil
}

func pluginGateStatus(p *plugin.Plugin, townRoot string, force bool) (bool, string) {
	if force || p.Gate == nil || p.Gate.Type != plugin.GateCooldown {
		return true, ""
	}
	duration := p.Gate.Duration
	if duration == "" {
		duration = "1h"
	}
	count, err := plugin.NewRecorder(townRoot).CountRunsSince(p.Name, duration)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: checking gate status: %v\n", err)
		return true, ""
	}
	if count == 0 {
		return true, ""
	}
	return false, fmt.Sprintf("ran %d time(s) within %s cooldown", count, duration)
}

func printPluginDryRun(p *plugin.Plugin, gateOpen bool, gateReason string) {
	fmt.Printf("%s Dry run for plugin: %s\n", style.Bold.Render("Plugin:"), p.Name)
	fmt.Printf("%s %s\n", style.Bold.Render("Location:"), p.Path)
	if p.Gate != nil {
		fmt.Printf("%s %s\n", style.Bold.Render("Gate type:"), p.Gate.Type)
	}
	if gateOpen {
		fmt.Printf("%s Would execute plugin instructions\n", style.Success.Render("Gate open:"))
		return
	}
	fmt.Printf("%s %s (use --force to override)\n", style.Warning.Render("Gate closed:"), gateReason)
}

func printPluginRun(p *plugin.Plugin, force, gateOpen bool) {
	fmt.Printf("%s Running plugin: %s\n", style.Success.Render("●"), p.Name)
	if force && !gateOpen {
		fmt.Printf("  %s\n", style.Dim.Render("(gate bypassed with --force)"))
	}
	fmt.Println()
	fmt.Printf("%s\n", style.Bold.Render("Instructions:"))
	fmt.Println(p.Instructions)
}

func recordPluginRun(p *plugin.Plugin, townRoot string) {
	beadID, err := plugin.NewRecorder(townRoot).RecordRun(plugin.PluginRunRecord{
		PluginName: p.Name,
		RigName:    p.RigName,
		Result:     plugin.ResultSuccess,
		Body:       "Manual run via gt plugin run",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to record run: %v\n", err)
		return
	}
	fmt.Printf("\n%s Recorded run: %s\n", style.Dim.Render("●"), beadID)
}

func runPluginSync(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	sourceDir, err := resolvePluginSource(commandStringFlag(cmd, "source"), townRoot)
	if err != nil {
		return err
	}

	targetDir := filepath.Join(townRoot, "plugins")

	dryRun := commandBoolFlag(cmd, "dry-run")
	clean := commandBoolFlag(cmd, "clean")
	if dryRun {
		return runPluginSyncDryRun(sourceDir, targetDir, clean)
	}

	result, err := plugin.SyncPlugins(sourceDir, targetDir, clean)
	if err != nil {
		return fmt.Errorf("syncing plugins: %w", err)
	}

	printPluginSyncResult(sourceDir, result)

	return nil
}

func resolvePluginSource(sourceDir, townRoot string) (string, error) {
	if sourceDir == "" {
		var err error
		sourceDir, err = plugin.FindGastownSource(townRoot)
		if err != nil {
			return "", err
		}
	}
	if filepath.IsAbs(sourceDir) {
		return sourceDir, nil
	}
	abs, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", fmt.Errorf("resolving source path: %w", err)
	}
	return abs, nil
}

func runPluginSyncDryRun(sourceDir, targetDir string, clean bool) error {
	report, err := plugin.DetectDrift(sourceDir, targetDir)
	if err != nil {
		return fmt.Errorf("detecting drift: %w", err)
	}
	fmt.Printf("%s Plugin sync dry run\n", style.Bold.Render("Plugin sync:"))
	fmt.Printf("  Source: %s\n", sourceDir)
	fmt.Printf("  Target: %s\n\n", targetDir)
	if !report.HasDrift() && len(report.Extra) == 0 {
		fmt.Printf("  %s All plugins up to date\n", style.Success.Render("✓"))
		return nil
	}
	for _, drift := range report.Drifted {
		fmt.Printf("  %s %s (content differs)\n", style.Warning.Render("~"), drift.Name)
	}
	for _, name := range report.Missing {
		fmt.Printf("  %s %s (new, would be copied)\n", style.Success.Render("+"), name)
	}
	if clean {
		for _, name := range report.Extra {
			fmt.Printf("  %s %s (would be removed)\n", style.Error.Render("-"), name)
		}
	}
	return nil
}

func printPluginSyncResult(sourceDir string, result *plugin.SyncResult) {
	if len(result.Copied) == 0 && len(result.Removed) == 0 {
		fmt.Printf("%s Plugins already up to date (%d checked)\n", style.Success.Render("✓"), len(result.Skipped))
		return
	}
	fmt.Printf("%s Synced plugins from %s\n", style.Success.Render("●"), style.Dim.Render(sourceDir))
	for _, name := range result.Copied {
		fmt.Printf("  %s %s\n", style.Success.Render("↑"), name)
	}
	for _, name := range result.Removed {
		fmt.Printf("  %s %s\n", style.Error.Render("×"), name)
	}
	if len(result.Skipped) > 0 {
		fmt.Printf("  %s %d plugin(s) already current\n", style.Dim.Render("·"), len(result.Skipped))
	}
	for _, err := range result.Errors {
		fmt.Fprintf(os.Stderr, "  %s %s\n", style.Error.Render("!"), err)
	}
}

func runPluginHistory(cmd *cobra.Command, args []string) error {
	name := args[0]
	limit := commandIntFlag(cmd, "limit")
	jsonOutput := commandBoolFlag(cmd, "json")

	_, townRoot, err := getPluginScanner()
	if err != nil {
		return err
	}

	recorder := plugin.NewRecorder(townRoot)
	runs, err := recorder.GetRunsSince(name, "")
	if err != nil {
		return fmt.Errorf("querying history: %w", err)
	}

	if runs == nil {
		runs = []*plugin.PluginRunBead{}
	}

	runs = limitPluginHistory(runs, limit)

	if jsonOutput {
		return outputPluginHistoryJSON(runs)
	}

	if len(runs) == 0 {
		fmt.Printf("%s No execution history for plugin: %s\n", style.Dim.Render("○"), name)
		return nil
	}

	printPluginHistory(runs, name)

	return nil
}

func limitPluginHistory(runs []*plugin.PluginRunBead, limit int) []*plugin.PluginRunBead {
	if limit > 0 && len(runs) > limit {
		return runs[:limit]
	}
	return runs
}

func outputPluginHistoryJSON(runs []*plugin.PluginRunBead) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(runs)
}

func printPluginHistory(runs []*plugin.PluginRunBead, name string) {
	fmt.Printf("%s Execution history for %s (%d runs)\n\n", style.Success.Render("●"), name, len(runs))
	for _, run := range runs {
		printPluginHistoryRun(run)
	}
}

func printPluginHistoryRun(run *plugin.PluginRunBead) {
	resultStyle := style.Success
	resultIcon := "✓"
	if run.Result == plugin.ResultFailure {
		resultStyle = style.Error
		resultIcon = "✗"
	} else if run.Result == plugin.ResultSkipped {
		resultStyle = style.Dim
		resultIcon = "○"
	}

	fmt.Printf("  %s %s  %s\n",
		resultStyle.Render(resultIcon),
		run.CreatedAt.Format("2006-01-02 15:04"),
		style.Dim.Render(run.ID))
}

func runPluginRecordRun(cmd *cobra.Command, _ []string) error {
	pluginName := commandStringFlag(cmd, "plugin")
	result := commandStringFlag(cmd, "result")
	title := commandStringFlag(cmd, "title")
	body := commandStringFlag(cmd, "description")
	rigName := commandStringFlag(cmd, "rig")
	labels := commandStringArrayFlag(cmd, "label")

	if pluginName == "" {
		return fmt.Errorf("--plugin is required")
	}
	if result == "" {
		return fmt.Errorf("--result is required")
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	recorder := plugin.NewRecorder(townRoot)
	beadID, err := recorder.RecordRun(plugin.PluginRunRecord{
		PluginName:  pluginName,
		RigName:     rigName,
		Result:      plugin.RunResult(result),
		Title:       title,
		Body:        body,
		ExtraLabels: labels,
	})
	if err != nil {
		return err
	}

	fmt.Fprintln(cmd.OutOrStdout(), beadID)
	return nil
}
