// Package cmd provides CLI commands for the gt tool.
package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/daemon"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/scheduler/capacity"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:     "config",
	GroupID: GroupConfig,
	Short:   "Manage Gas Town configuration",
	RunE:    requireSubcommand,
	Long: `Manage Gas Town configuration settings.

This command allows you to view and modify configuration settings
for your Gas Town workspace, including agent aliases and defaults.

Commands:
  gt config agent list              List all agents (built-in and custom)
  gt config agent get <name>         Show agent configuration
  gt config agent set <name> <cmd>   Set custom agent command
  gt config agent remove <name>      Remove custom agent
  gt config role list                Show effective agent and effort by role
  gt config role set <role> <agent> [effort]
                                     Assign an agent and optional effort to a role
  gt config role unset <role>        Clear a role assignment
  gt config mix [assignment...]      Mix agent types across roles and crew
  gt config default-agent [name]     Get or set default agent
  gt config default-agent list       List available agents`,
}

// Agent subcommands

func newConfigAgentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all agents",
		Long:  "", // Set in init() — includes full built-in preset list from config.BuiltInAgentPresetSummary()
		RunE:  runConfigAgentList,
	}
}

var configAgentGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Show agent configuration",
	Long: `Show the configuration for a specific agent.

Displays the full configuration for an agent, including command,
arguments, and other settings. Works for both built-in and custom agents.

Examples:
  gt config agent get claude
  gt config agent get my-custom-agent`,
	Args: cobra.ExactArgs(1),
	RunE: runConfigAgentGet,
}

var configAgentSetCmd = &cobra.Command{
	Use:   "set <name> <command>",
	Short: "Set custom agent command",
	Long: `Set a custom agent command in town settings.

This creates or updates a custom agent definition that overrides
or extends the built-in presets. The custom agent will be available
to all rigs in the town.

The command can include arguments. Use quotes if the command or
arguments contain spaces.

The provider preset is inferred from the command binary name when it
matches a known preset (e.g., "gemini", "claude"). Use --provider to
set it explicitly for custom binary names. The provider controls
session handling, tmux detection, hooks, and other runtime defaults.

Codex aliases inherit the built-in non-interactive flags
(--dangerously-bypass-approvals-and-sandbox and
-c check_for_update_on_startup=false) unless the command already sets a
sandbox or approval policy.

Examples:
  gt config agent set claude-glm \"claude-glm --model glm-4\"
  gt config agent set gemini-custom gemini --approval-mode yolo
  gt config agent set claude \"claude-glm\"  # Override built-in claude
  gt config agent set my-bot my-bot-cli --provider claude  # Use Claude defaults
  gt config agent set codex-cheap \"codex -m gpt-5.3-codex-spark\" --provider codex`,
	Args: cobra.ExactArgs(2),
	RunE: runConfigAgentSet,
}

func newConfigAgentRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove custom agent",
		Long:  "", // Set in init() — includes full built-in preset list
		Args:  cobra.ExactArgs(1),
		RunE:  runConfigAgentRemove,
	}
}

// Role subcommands provide a typed interface over role_agents and role_effort.

var configRoleCmd = &cobra.Command{
	Use:   "role",
	Short: "Configure agents and effort by role",
	Long: `Configure the agent profile and thinking effort used by each role.

Agent profiles are built-ins or aliases created with 'gt config agent set'.
Effort is optional. Pi profiles accept off, minimal, low, medium, high, xhigh,
or max; other runtimes accept low, medium, high, or max. For Pi profiles, the
configured effort is passed to Pi as --thinking.`,
	RunE: requireSubcommand,
}

var configRoleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List effective role assignments",
	Long: `List effective role assignments.

With no --rig flag, lists town-level role assignments (mayor, deacon, witness,
refinery, polecat, crew, boot, dog). With --rig, lists the rig-managed roles
(witness, refinery, polecat, crew) as resolved for that rig, showing whether
each is a rig-specific override or inherited from town/default.`,
	Args: cobra.NoArgs,
	RunE: runConfigRoleList,
}

var configRoleSetCmd = &cobra.Command{
	Use:   "set <role> <agent> [effort]",
	Short: "Assign an agent profile and optional effort to a role",
	Long: `Assign an agent profile and optional thinking effort to a role.

With --rig, the assignment is scoped to that rig instead of the whole town
(only witness, refinery, polecat, and crew can be rig-scoped).

Examples:
  gt config role set mayor pi-luna high
  gt config role set witness pi-luna low
  gt config role set polecat pi-sonnet max
  gt config role set witness pi-luna low --rig myrig`,
	Args: cobra.RangeArgs(2, 3),
	RunE: runConfigRoleSet,
}

var configRoleUnsetCmd = &cobra.Command{
	Use:   "unset <role>",
	Short: "Clear the agent and effort assigned to a role",
	Long: `Clear the agent and effort assigned to a role.

With --rig, clears only the rig-scoped override, leaving the town-level
assignment (if any) in place.`,
	Args: cobra.ExactArgs(1),
	RunE: runConfigRoleUnset,
}

// Cost-tier subcommand

var configCostTierCmd = &cobra.Command{
	Use:   "cost-tier [tier]",
	Short: "Get or set cost optimization tier",
	Long: `Get or set the cost optimization tier for model selection.

With no arguments, shows the current cost tier and role assignments.
With an argument, applies the specified tier preset.

Tiers control which AI model each role uses:
  standard  All roles use Opus (highest quality, default)
  economy   Patrol roles use Sonnet/Haiku, workers use Opus
  budget    Patrol roles use Haiku, workers use Sonnet

Examples:
  gt config cost-tier              # Show current tier
  gt config cost-tier economy      # Switch to economy tier
  gt config cost-tier standard     # Reset to all-Opus`,
	Args: cobra.MaximumNArgs(1),
	RunE: runConfigCostTier,
}

func runConfigCostTier(_ *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	settingsPath := config.TownSettingsPath(townRoot)
	townSettings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}

	if len(args) == 0 {
		return showCostTier(townSettings)
	}
	return applyCostTier(settingsPath, townSettings, args[0])
}

func showCostTier(townSettings *config.TownSettings) error {
	current := config.GetCurrentTier(townSettings)
	if current == "" {
		fmt.Println("Cost tier: " + style.Bold.Render("custom") + " (manual role_agents configuration)")
		return nil
	}

	tier := config.CostTier(current)
	fmt.Printf("Cost tier: %s\n", style.Bold.Render(current))
	fmt.Printf("  %s\n\n", config.TierDescription(tier))
	fmt.Println("Role assignments:")
	fmt.Println(config.FormatTierRoleTable(tier))
	return nil
}

func applyCostTier(settingsPath string, townSettings *config.TownSettings, tierName string) error {
	if !config.IsValidTier(tierName) {
		return fmt.Errorf("invalid cost tier %q (valid: %s)", tierName, strings.Join(config.ValidCostTiers(), ", "))
	}

	tier := config.CostTier(tierName)

	// Warn if overwriting custom role_agents
	currentTier := config.GetCurrentTier(townSettings)
	if currentTier == "" && len(townSettings.RoleAgents) > 0 {
		fmt.Println("Warning: overwriting custom role_agents configuration")
	}

	if err := config.ApplyCostTier(townSettings, tier); err != nil {
		return fmt.Errorf("applying cost tier: %w", err)
	}

	if err := config.SaveTownSettings(settingsPath, townSettings); err != nil {
		return fmt.Errorf("saving town settings: %w", err)
	}

	fmt.Printf("Cost tier set to %s\n", style.Bold.Render(tierName))
	fmt.Printf("  %s\n\n", config.TierDescription(tier))
	fmt.Println("Role assignments:")
	fmt.Println(config.FormatTierRoleTable(tier))
	return nil
}

// Default-agent subcommand

func newConfigDefaultAgentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "default-agent [name]",
		Short: "Get or set default agent",
		Long:  "", // Set in init() — includes full built-in preset list
		RunE:  runConfigDefaultAgent,
	}
}

var configDefaultAgentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available agents",
	Long: `List all available agents that can be set as the default.

Shows all built-in agent presets and any custom agents defined in
your town settings. Equivalent to 'gt config agent list'.

Examples:
  gt config default-agent list           # Text output
  gt config default-agent list --json    # JSON output`,
	RunE: runConfigAgentList,
}

var configAgentEmailDomainCmd = &cobra.Command{
	Use:   "agent-email-domain [domain]",
	Short: "Get or set agent email domain",
	Long: `Get or set the domain used for agent git commit emails.

When agents commit code via 'gt commit', their identity is converted
to a git email address. For example, "gastown/crew/jack" becomes
"gastown.crew.jack@{domain}".

With no arguments, shows the current domain.
With an argument, sets the domain.

Default: gastown.local

Examples:
  gt config agent-email-domain                 # Show current domain
  gt config agent-email-domain gastown.local   # Set to gastown.local
  gt config agent-email-domain example.com     # Set custom domain`,
	RunE: runConfigAgentEmailDomain,
}

// AgentListItem represents an agent in list output.
type AgentListItem struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	Args     string `json:"args,omitempty"`
	Type     string `json:"type"` // "built-in" or "custom"
	IsCustom bool   `json:"is_custom"`
}

func runConfigAgentList(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	// Load town settings
	settingsPath := config.TownSettingsPath(townRoot)
	townSettings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}

	// Load agent registry
	registryPath := config.DefaultAgentRegistryPath(townRoot)
	if err := config.LoadAgentRegistry(registryPath); err != nil {
		return fmt.Errorf("loading agent registry: %w", err)
	}

	// Collect all agents
	builtInAgents := config.ListAgentPresets()
	customAgents := make(map[string]*config.RuntimeConfig)
	if townSettings.Agents != nil {
		for name, runtime := range townSettings.Agents {
			customAgents[name] = runtime
		}
	}

	// Build list items
	var items []AgentListItem
	for _, name := range builtInAgents {
		preset := config.GetAgentPresetByName(name)
		if preset != nil {
			items = append(items, AgentListItem{
				Name:     name,
				Command:  preset.Command,
				Args:     strings.Join(preset.Args, " "),
				Type:     "built-in",
				IsCustom: false,
			})
		}
	}
	for name, runtime := range customAgents {
		argsStr := ""
		if runtime.Args != nil {
			argsStr = strings.Join(runtime.Args, " ")
		}
		items = append(items, AgentListItem{
			Name:     name,
			Command:  runtime.Command,
			Args:     argsStr,
			Type:     "custom",
			IsCustom: true,
		})
	}

	// Sort by name
	sort.Slice(items, func(i, j int) bool {
		return items[i].Name < items[j].Name
	})

	jsonOutput, _ := cmd.Flags().GetBool("json")
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}

	// Text output
	fmt.Printf("%s\n\n", style.Bold.Render("Available Agents"))
	for _, item := range items {
		typeLabel := style.Dim.Render("[" + item.Type + "]")
		fmt.Printf("  %s %s %s", style.Bold.Render(item.Name), typeLabel, item.Command)
		if item.Args != "" {
			fmt.Printf(" %s", item.Args)
		}
		fmt.Println()
	}

	// Show default
	defaultAgent := townSettings.DefaultAgent
	if defaultAgent == "" {
		defaultAgent = "claude"
	}
	fmt.Printf("\nDefault: %s\n", style.Bold.Render(defaultAgent))

	return nil
}

func runConfigAgentGet(_ *cobra.Command, args []string) error {
	name := args[0]

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	// Load town settings for custom agents
	settingsPath := config.TownSettingsPath(townRoot)
	townSettings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}

	// Load agent registry
	registryPath := config.DefaultAgentRegistryPath(townRoot)
	if err := config.LoadAgentRegistry(registryPath); err != nil {
		return fmt.Errorf("loading agent registry: %w", err)
	}

	// Check custom agents first
	if townSettings.Agents != nil {
		if runtime, ok := townSettings.Agents[name]; ok {
			displayAgentConfig(name, runtime, nil, true)
			return nil
		}
	}

	// Check built-in agents
	preset := config.GetAgentPresetByName(name)
	if preset != nil {
		runtime := &config.RuntimeConfig{
			Command: preset.Command,
			Args:    preset.Args,
		}
		displayAgentConfig(name, runtime, preset, false)
		return nil
	}

	return fmt.Errorf("agent '%s' not found", name)
}

func displayAgentConfig(name string, runtime *config.RuntimeConfig, preset *config.AgentPresetInfo, isCustom bool) {
	fmt.Printf("%s\n\n", style.Bold.Render("Agent: "+name))

	typeLabel := "custom"
	if !isCustom {
		typeLabel = "built-in"
	}
	fmt.Printf("Type:   %s\n", typeLabel)
	fmt.Printf("Command: %s\n", runtime.Command)

	if runtime.Args != nil && len(runtime.Args) > 0 {
		fmt.Printf("Args:    %s\n", strings.Join(runtime.Args, " "))
	}

	if preset != nil {
		if preset.SessionIDEnv != "" {
			fmt.Printf("Session ID Env: %s\n", preset.SessionIDEnv)
		}
		if preset.ResumeFlag != "" {
			fmt.Printf("Resume Style:  %s (%s)\n", preset.ResumeStyle, preset.ResumeFlag)
		}
		fmt.Printf("Supports Hooks: %v\n", preset.SupportsHooks)
	}
}

func runConfigAgentSet(cmd *cobra.Command, args []string) error {
	name := args[0]
	commandLine := args[1]

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	// Load town settings
	settingsPath := config.TownSettingsPath(townRoot)
	townSettings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}

	// Parse command line into command and args
	parts := strings.Fields(commandLine)
	if len(parts) == 0 {
		return fmt.Errorf("command cannot be empty")
	}

	// Initialize agents map if needed
	if townSettings.Agents == nil {
		townSettings.Agents = make(map[string]*config.RuntimeConfig)
	}

	// Determine the provider: use --provider flag if given, otherwise infer
	// from the command binary name if it matches a known preset.
	provider := commandStringFlag(cmd, "provider")
	if provider == "" {
		cmdBase := parts[0]
		if idx := strings.LastIndexByte(cmdBase, '/'); idx >= 0 {
			cmdBase = cmdBase[idx+1:]
		}
		if config.IsKnownPreset(cmdBase) {
			provider = cmdBase
		}
	}

	// Create or update the agent. When an entry already exists, mutate it in
	// place rather than replacing it wholesale — a wholesale replace would
	// silently discard fields like Env/Session/Hooks/Tmux/Instructions that
	// aren't set by this command.
	agent := townSettings.Agents[name]
	if agent == nil {
		agent = &config.RuntimeConfig{}
		townSettings.Agents[name] = agent
	}
	agent.Provider = provider
	agent.Command = parts[0]
	agent.Args = parts[1:]

	// Save settings
	if err := config.SaveTownSettings(settingsPath, townSettings); err != nil {
		return fmt.Errorf("saving town settings: %w", err)
	}

	fmt.Printf("Agent '%s' set to: %s\n", style.Bold.Render(name), commandLine)

	// Check if this overrides a built-in
	builtInAgents := config.ListAgentPresets()
	for _, builtin := range builtInAgents {
		if name == builtin {
			fmt.Printf("\n%s\n", style.Dim.Render("(overriding built-in '"+builtin+"' preset)"))
			break
		}
	}

	return nil
}

func runConfigAgentRemove(_ *cobra.Command, args []string) error {
	name := args[0]

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	// Check if trying to remove built-in
	builtInAgents := config.ListAgentPresets()
	for _, builtin := range builtInAgents {
		if name == builtin {
			return fmt.Errorf("cannot remove built-in agent '%s' (use 'gt config agent set' to override it)", name)
		}
	}

	// Load town settings
	settingsPath := config.TownSettingsPath(townRoot)
	townSettings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}

	// Check if agent exists
	if townSettings.Agents == nil || townSettings.Agents[name] == nil {
		return fmt.Errorf("custom agent '%s' not found", name)
	}

	// Remove the agent
	delete(townSettings.Agents, name)

	// Save settings
	if err := config.SaveTownSettings(settingsPath, townSettings); err != nil {
		return fmt.Errorf("saving town settings: %w", err)
	}

	fmt.Printf("Removed custom agent '%s'\n", style.Bold.Render(name))
	return nil
}

func runConfigDefaultAgent(_ *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	// Load town settings
	settingsPath := config.TownSettingsPath(townRoot)
	townSettings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}

	// Load agent registry
	registryPath := config.DefaultAgentRegistryPath(townRoot)
	if err := config.LoadAgentRegistry(registryPath); err != nil {
		return fmt.Errorf("loading agent registry: %w", err)
	}

	if len(args) == 0 {
		// Show current default
		defaultAgent := townSettings.DefaultAgent
		if defaultAgent == "" {
			defaultAgent = "claude"
		}
		fmt.Printf("Default agent: %s\n", style.Bold.Render(defaultAgent))
		return nil
	}

	// Set new default
	name := args[0]

	// Verify agent exists
	isValid := false
	builtInAgents := config.ListAgentPresets()
	for _, builtin := range builtInAgents {
		if name == builtin {
			isValid = true
			break
		}
	}
	if !isValid && townSettings.Agents != nil {
		if _, ok := townSettings.Agents[name]; ok {
			isValid = true
		}
	}

	if !isValid {
		return fmt.Errorf("agent '%s' not found (use 'gt config default-agent list' to see available agents)", name)
	}

	// Set default
	townSettings.DefaultAgent = name

	// Save settings
	if err := config.SaveTownSettings(settingsPath, townSettings); err != nil {
		return fmt.Errorf("saving town settings: %w", err)
	}

	fmt.Printf("Default agent set to '%s'\n", style.Bold.Render(name))
	return nil
}

func runConfigRoleList(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	rigName := commandStringFlag(cmd, "rig")
	if rigName != "" {
		return runConfigRoleListForRig(townRoot, rigName)
	}

	settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}

	defaultAgent := settings.DefaultAgent
	if defaultAgent == "" {
		defaultAgent = "claude"
	}

	fmt.Printf("%-10s %-20s %s\n", "ROLE", "AGENT", "EFFORT")
	for _, role := range config.TierManagedRoles {
		agent := settings.RoleAgents[role]
		if agent == "" {
			agent = defaultAgent + " (default)"
		}
		effort := settings.RoleEffort[role]
		if effort == "" {
			effort = "runtime default"
		}
		fmt.Printf("%-10s %-20s %s\n", role, agent, effort)
	}
	fmt.Println()
	fmt.Println("Tip: assign several roles at once with gt config mix mayor=pi crew=codex")
	return nil
}

func runConfigRoleListForRig(townRoot, rigName string) error {
	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	assignments, err := config.ResolveRigRoleAssignments(townRoot, r.Path)
	if err != nil {
		return fmt.Errorf("resolving rig role assignments: %w", err)
	}

	fmt.Printf("%-10s %-20s %-20s %s\n", "ROLE", "AGENT", "EFFORT", "SOURCE")
	for _, a := range assignments {
		effort := a.Effort
		if effort == "" {
			effort = "runtime default"
		}
		source := "inherited"
		if a.RoleSpecific {
			source = "rig"
		}
		fmt.Printf("%-10s %-20s %-20s %s\n", a.Role, a.Agent, effort, source)
	}
	return nil
}

func runConfigRoleSet(cmd *cobra.Command, args []string) error {
	role, agent := args[0], args[1]
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	effort := ""
	if len(args) == 3 {
		effort = args[2]
	}

	rigName := commandStringFlag(cmd, "rig")
	if rigName != "" {
		_, r, err := getRig(rigName)
		if err != nil {
			return err
		}
		if err := config.SetRigRole(townRoot, r.Path, role, agent, effort); err != nil {
			return err
		}
	} else if err := config.SetTownRole(config.TownSettingsPath(townRoot), role, agent, effort); err != nil {
		return err
	}

	if effort == "" {
		fmt.Printf("Set role %s to agent %s (effort unchanged)\n", style.Bold.Render(role), style.Bold.Render(agent))
	} else {
		fmt.Printf("Set role %s to agent %s with %s effort\n", style.Bold.Render(role), style.Bold.Render(agent), style.Bold.Render(effort))
	}
	return nil
}

func runConfigRoleUnset(cmd *cobra.Command, args []string) error {
	role := args[0]
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	rigName := commandStringFlag(cmd, "rig")
	if rigName != "" {
		_, r, err := getRig(rigName)
		if err != nil {
			return err
		}
		if err := config.UnsetRigRole(r.Path, role); err != nil {
			return err
		}
	} else if err := config.UnsetTownRole(config.TownSettingsPath(townRoot), role); err != nil {
		return err
	}

	fmt.Printf("Cleared role configuration for %s\n", style.Bold.Render(role))
	return nil
}

func runConfigAgentEmailDomain(_ *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	// Load town settings
	settingsPath := config.TownSettingsPath(townRoot)
	townSettings, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}

	if len(args) == 0 {
		// Show current domain
		domain := townSettings.AgentEmailDomain
		if domain == "" {
			domain = DefaultAgentEmailDomain
		}
		fmt.Printf("Agent email domain: %s\n", style.Bold.Render(domain))
		fmt.Printf("\nExample: gastown/crew/jack → gastown.crew.jack@%s\n", domain)
		return nil
	}

	// Set new domain
	domain := args[0]

	// Basic validation - domain should not be empty and should not start with @
	if domain == "" {
		return fmt.Errorf("domain cannot be empty")
	}
	if strings.HasPrefix(domain, "@") {
		return fmt.Errorf("domain should not include @: use '%s' instead", strings.TrimPrefix(domain, "@"))
	}

	// Set domain
	townSettings.AgentEmailDomain = domain

	// Save settings
	if err := config.SaveTownSettings(settingsPath, townSettings); err != nil {
		return fmt.Errorf("saving town settings: %w", err)
	}

	fmt.Printf("Agent email domain set to '%s'\n", style.Bold.Render(domain))
	fmt.Printf("\nExample: gastown/crew/jack → gastown.crew.jack@%s\n", domain)
	return nil
}

// configSetCmd sets a town config value by dot-notation key.
// Long is populated in init() from the same townSettingsKeySpecs table that
// drives runConfigSet, so the key list can't drift out of sync with the code.
var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value",
	Long: buildConfigKeyHelp("Set") + `

Examples:
  gt config set convoy.notify_on_complete true
  gt config set cli_theme dark
  gt config set default_agent claude
  gt config set auto_compact_window 150k
  gt config set dolt.port 3308
  gt config set scheduler.max_polecats 5
  gt config set maintenance.window 03:00
  gt config set maintenance.interval daily
  gt config set lifecycle.reaper.delete_age 336h
  gt config set lifecycle.compactor.threshold 1000`,
	Args: cobra.ExactArgs(2),
	RunE: runConfigSet,
}

// configGetCmd gets a town config value by dot-notation key.
var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a configuration value",
	Long: buildConfigKeyHelp("Get") + `

Examples:
  gt config get convoy.notify_on_complete
  gt config get cli_theme
  gt config get maintenance.window
  gt config get lifecycle.reaper.delete_age`,
	Args: cobra.ExactArgs(1),
	RunE: runConfigGet,
}

// townSettingsKeySpec is the single source of truth for a `gt config get/set`
// key that is backed directly by TownSettings: its get/set behavior and its
// one-line help text. Adding a new TownSettings-backed key means adding one
// entry here, not touching four separate switch statements and help blocks.
//
// Keys that target a different schema entirely (maintenance.*, lifecycle.*,
// dolt.port all live in daemon.DaemonPatrolConfig / mayor/daemon.json, not
// TownSettings) are intentionally NOT part of this table — folding them in
// would misrepresent what file they actually read/write. They keep their own
// dedicated handlers below and are merged into the help/error text separately.
type townSettingsKeySpec struct {
	key  string
	help string
	get  func(*config.TownSettings) string
	set  func(*config.TownSettings, string) error
}

var townSettingsKeySpecs = []townSettingsKeySpec{
	{
		key:  "convoy.notify_on_complete",
		help: "Push notification to Mayor session on convoy completion (true/false, default: false)",
		get: func(s *config.TownSettings) string {
			if s.Convoy != nil && s.Convoy.NotifyOnComplete {
				return "true"
			}
			return "false"
		},
		set: func(s *config.TownSettings, value string) error {
			b, err := parseBool(value)
			if err != nil {
				return fmt.Errorf("invalid value for convoy.notify_on_complete: %w (expected true/false)", err)
			}
			if s.Convoy == nil {
				s.Convoy = &config.ConvoyConfig{}
			}
			s.Convoy.NotifyOnComplete = b
			return nil
		},
	},
	{
		key:  "cli_theme",
		help: `CLI color scheme ("dark", "light", "auto")`,
		get: func(s *config.TownSettings) string {
			if s.CLITheme == "" {
				return "auto"
			}
			return s.CLITheme
		},
		set: func(s *config.TownSettings, value string) error {
			switch value {
			case "dark", "light", "auto":
				s.CLITheme = value
				return nil
			default:
				return fmt.Errorf("invalid cli_theme: %q (expected dark, light, or auto)", value)
			}
		},
	},
	{
		key:  "default_agent",
		help: "Default agent preset name",
		get: func(s *config.TownSettings) string {
			if s.DefaultAgent == "" {
				return "claude"
			}
			return s.DefaultAgent
		},
		set: func(s *config.TownSettings, value string) error {
			s.DefaultAgent = value
			return nil
		},
	},
	{
		key: "auto_compact_window",
		help: "Auto-compaction cap in tokens (default: 150000 / 150k). " +
			"Applied to every agent type as min(cap, model window).",
		get: func(s *config.TownSettings) string {
			if s.AutoCompactWindow > 0 {
				return strconv.Itoa(s.AutoCompactWindow)
			}
			return strconv.Itoa(config.DefaultAutoCompactWindowTokens)
		},
		set: func(s *config.TownSettings, value string) error {
			n, ok := config.ParseTokenCount(value)
			if !ok {
				return fmt.Errorf("invalid value for auto_compact_window: expected a positive token count such as 150000 or 150k")
			}
			s.AutoCompactWindow = n
			return nil
		},
	},
	{
		key:  "scheduler.max_polecats",
		help: "Dispatch mode: -1 = direct (default), N > 0 = deferred",
		get: func(s *config.TownSettings) string {
			scfg := s.Scheduler
			if scfg == nil {
				scfg = capacity.DefaultSchedulerConfig()
			}
			return strconv.Itoa(scfg.GetMaxPolecats())
		},
		set: func(s *config.TownSettings, value string) error {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("invalid value for scheduler.max_polecats: %w (expected integer)", err)
			}
			if n < -1 {
				return fmt.Errorf("invalid value for scheduler.max_polecats: must be >= -1 (-1 = direct dispatch, 0 = direct dispatch, N > 0 = deferred)")
			}
			if s.Scheduler == nil {
				s.Scheduler = capacity.DefaultSchedulerConfig()
			}
			s.Scheduler.MaxPolecats = &n
			return nil
		},
	},
	{
		key:  "scheduler.batch_size",
		help: "Beads per heartbeat (default: 1)",
		get: func(s *config.TownSettings) string {
			scfg := s.Scheduler
			if scfg == nil {
				scfg = capacity.DefaultSchedulerConfig()
			}
			return strconv.Itoa(capacity.GetBatchSize(scfg))
		},
		set: func(s *config.TownSettings, value string) error {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return fmt.Errorf("invalid value for scheduler.batch_size: expected positive integer")
			}
			if s.Scheduler == nil {
				s.Scheduler = capacity.DefaultSchedulerConfig()
			}
			s.Scheduler.BatchSize = &n
			return nil
		},
	},
	{
		key:  "scheduler.spawn_delay",
		help: "Delay between spawns (default: 0s)",
		get: func(s *config.TownSettings) string {
			scfg := s.Scheduler
			if scfg == nil {
				scfg = capacity.DefaultSchedulerConfig()
			}
			return capacity.GetSpawnDelay(scfg).String()
		},
		set: func(s *config.TownSettings, value string) error {
			if _, err := time.ParseDuration(value); err != nil {
				return fmt.Errorf("invalid value for scheduler.spawn_delay: %w (expected Go duration, e.g. 2s, 500ms)", err)
			}
			if s.Scheduler == nil {
				s.Scheduler = capacity.DefaultSchedulerConfig()
			}
			s.Scheduler.SpawnDelay = value
			return nil
		},
	},
	{
		key: "polecat.target_clean_policy",
		help: `When to delete <polecat>/target/ on reuse ("per_bead", "every_n_beads:<N>", ` +
			`"never"; default: per_bead)`,
		get: func(s *config.TownSettings) string {
			if s.Polecat != nil && s.Polecat.TargetCleanPolicy != "" {
				return s.Polecat.TargetCleanPolicy
			}
			return polecat.DefaultTargetCleanPolicy().String()
		},
		set: func(s *config.TownSettings, value string) error {
			// Validate the policy string parses cleanly. Storage form is the raw
			// input normalized via parsed.String(), so e.g. "  per_bead  " becomes
			// "per_bead".
			parsed, err := polecat.ParseTargetCleanPolicy(value)
			if err != nil {
				return fmt.Errorf("invalid value for polecat.target_clean_policy: %w", err)
			}
			if s.Polecat == nil {
				s.Polecat = &config.PolecatConfig{}
			}
			s.Polecat.TargetCleanPolicy = parsed.String()
			return nil
		},
	},
}

// buildConfigKeyHelp renders the "Supported keys" section shared by
// configSetCmd and configGetCmd Long help, from the same table that drives
// the actual get/set behavior. verb is "Get" or "Set", used in the header.
func buildConfigKeyHelp(verb string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s a town configuration value using dot-notation keys.\n\nSupported keys:\n", verb)
	for _, spec := range townSettingsKeySpecs {
		fmt.Fprintf(&b, "  %-28s %s\n", spec.key, spec.help)
	}
	b.WriteString(daemonBackedConfigKeyHelp)
	return b.String()
}

func findTownSettingsKeySpec(key string) *townSettingsKeySpec {
	for i := range townSettingsKeySpecs {
		if townSettingsKeySpecs[i].key == key {
			return &townSettingsKeySpecs[i]
		}
	}
	return nil
}

// daemonBackedConfigKeyHelp lists the `gt config get/set` keys that are NOT
// TownSettings-backed: they live in daemon.DaemonPatrolConfig
// (mayor/daemon.json) instead, and keep their own dedicated handlers.
const daemonBackedConfigKeyHelp = `  dolt.port                   Dolt SQL server port (default: 3307). Set this when
                              another Gas Town instance is using the same port.
                              Writes GT_DOLT_PORT to mayor/daemon.json env section.
  maintenance.window          Maintenance window start time in HH:MM (e.g., "03:00")
  maintenance.interval        How often: "daily", "weekly", "monthly", or duration
  maintenance.threshold       Commit count threshold (default: 1000)

  Lifecycle (Dolt data maintenance):
  lifecycle.reaper.enabled     Enable/disable wisp reaper (true/false)
  lifecycle.reaper.interval    Reaper check interval (default: 30m)
  lifecycle.reaper.delete_age  Delete closed wisps after this duration (default: 168h / 7d)
  lifecycle.compactor.enabled  Enable/disable compactor dog (true/false)
  lifecycle.compactor.interval Compactor check interval (default: 24h)
  lifecycle.compactor.threshold Commit count before compaction (default: 500)
  lifecycle.doctor.enabled     Enable/disable doctor dog (true/false)
  lifecycle.doctor.interval    Doctor check interval (default: 5m)
  lifecycle.backup.enabled     Enable/disable JSONL + Dolt backups (true/false)
  lifecycle.backup.interval    Backup interval (default: 15m)`

// unknownConfigKeyError builds the single "unknown config key" error shared
// by runConfigSet and runConfigGet, listing every supported key: the
// TownSettings-backed table plus the daemon-backed keys.
func unknownConfigKeyError(key string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "unknown config key: %q\n\nSupported keys:\n", key)
	for _, spec := range townSettingsKeySpecs {
		fmt.Fprintf(&b, "  %-28s %s\n", spec.key, spec.help)
	}
	b.WriteString(daemonBackedConfigKeyHelp)
	return errors.New(b.String())
}

func runConfigSet(_ *cobra.Command, args []string) error {
	key := args[0]
	value := args[1]

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	if spec := findTownSettingsKeySpec(key); spec != nil {
		settingsPath := config.TownSettingsPath(townRoot)
		townSettings, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			return fmt.Errorf("loading town settings: %w", err)
		}
		if err := spec.set(townSettings, value); err != nil {
			return err
		}
		if err := config.SaveTownSettings(settingsPath, townSettings); err != nil {
			return fmt.Errorf("saving town settings: %w", err)
		}
		fmt.Printf("Set %s = %s\n", style.Bold.Render(key), value)
		return nil
	}

	switch key {
	case "maintenance.window", "maintenance.interval", "maintenance.threshold":
		return setMaintenanceConfig(townRoot, key, value)

	case "dolt.port":
		port, err := strconv.Atoi(value)
		if err != nil || port < 1024 || port > 65535 {
			return fmt.Errorf("invalid value for %s: expected port number 1024-65535", key)
		}
		patrolCfg := daemon.LoadPatrolConfig(townRoot)
		if patrolCfg == nil {
			patrolCfg = &daemon.DaemonPatrolConfig{Type: "daemon-patrol-config", Version: 1}
		}
		if patrolCfg.Env == nil {
			patrolCfg.Env = make(map[string]string)
		}
		patrolCfg.Env["GT_DOLT_PORT"] = value
		if err := daemon.SavePatrolConfig(townRoot, patrolCfg); err != nil {
			return fmt.Errorf("saving daemon.json: %w", err)
		}
		fmt.Printf("Set GT_DOLT_PORT = %s in mayor/daemon.json\n", style.Bold.Render(value))
		fmt.Printf("  %s\n", style.Dim.Render("Restart the daemon for the change to take effect: gt daemon restart"))
		return nil

	default:
		if strings.HasPrefix(key, "lifecycle.") {
			return setLifecycleConfig(townRoot, key, value)
		}
		return unknownConfigKeyError(key)
	}
}

func runConfigGet(_ *cobra.Command, args []string) error {
	key := args[0]

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	if spec := findTownSettingsKeySpec(key); spec != nil {
		settingsPath := config.TownSettingsPath(townRoot)
		townSettings, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			return fmt.Errorf("loading town settings: %w", err)
		}
		fmt.Println(spec.get(townSettings))
		return nil
	}

	switch key {
	case "maintenance.window", "maintenance.interval", "maintenance.threshold":
		return getMaintenanceConfig(townRoot, key)

	case "dolt.port":
		patrolCfg := daemon.LoadPatrolConfig(townRoot)
		if patrolCfg != nil {
			if v, ok := patrolCfg.Env["GT_DOLT_PORT"]; ok {
				fmt.Println(v)
				return nil
			}
		}
		fmt.Println("3307") // DefaultPort
		return nil

	default:
		if strings.HasPrefix(key, "lifecycle.") {
			return getLifecycleConfig(townRoot, key)
		}
		return unknownConfigKeyError(key)
	}
}

// setMaintenanceConfig sets a maintenance.* key in daemon.json (patrol config).
func setMaintenanceConfig(townRoot, key, value string) error {
	patrolConfig := daemon.LoadPatrolConfig(townRoot)
	if patrolConfig == nil {
		patrolConfig = &daemon.DaemonPatrolConfig{
			Type:    "daemon-patrol-config",
			Version: 1,
		}
	}
	if patrolConfig.Patrols == nil {
		patrolConfig.Patrols = &daemon.PatrolsConfig{}
	}
	if patrolConfig.Patrols.ScheduledMaintenance == nil {
		patrolConfig.Patrols.ScheduledMaintenance = &daemon.ScheduledMaintenanceConfig{}
	}
	mc := patrolConfig.Patrols.ScheduledMaintenance

	switch key {
	case "maintenance.window":
		// Validate HH:MM format
		parts := strings.SplitN(value, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid window format %q: expected HH:MM (e.g., 03:00)", value)
		}
		hour, err := strconv.Atoi(parts[0])
		if err != nil || hour < 0 || hour > 23 {
			return fmt.Errorf("invalid hour in %q: expected 0-23", value)
		}
		minute, err := strconv.Atoi(parts[1])
		if err != nil || minute < 0 || minute > 59 {
			return fmt.Errorf("invalid minute in %q: expected 0-59", value)
		}
		mc.Window = fmt.Sprintf("%02d:%02d", hour, minute)
		mc.Enabled = true // Setting window enables the patrol

	case "maintenance.interval":
		switch value {
		case "daily", "weekly", "monthly":
			mc.Interval = value
		default:
			// Try parsing as Go duration
			_, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("invalid interval %q: expected daily, weekly, monthly, or Go duration (e.g., 48h)", value)
			}
			mc.Interval = value
		}

	case "maintenance.threshold":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return fmt.Errorf("invalid threshold %q: expected positive integer", value)
		}
		mc.Threshold = &n
	}

	if err := daemon.SavePatrolConfig(townRoot, patrolConfig); err != nil {
		return fmt.Errorf("saving daemon config: %w", err)
	}

	fmt.Printf("Set %s = %s\n", style.Bold.Render(key), value)
	if key == "maintenance.window" {
		fmt.Printf("Scheduled maintenance enabled (window: %s, interval: %s)\n",
			mc.Window, mc.Interval)
		if mc.Interval == "" {
			fmt.Println("Hint: set interval with: gt config set maintenance.interval daily")
		}
	}
	return nil
}

// getMaintenanceConfig gets a maintenance.* key from daemon.json (patrol config).
func getMaintenanceConfig(townRoot, key string) error {
	patrolConfig := daemon.LoadPatrolConfig(townRoot)

	var value string
	switch key {
	case "maintenance.window":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.ScheduledMaintenance != nil {
			value = patrolConfig.Patrols.ScheduledMaintenance.Window
		}
		if value == "" {
			value = "(not set)"
		}

	case "maintenance.interval":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.ScheduledMaintenance != nil {
			value = patrolConfig.Patrols.ScheduledMaintenance.Interval
		}
		if value == "" {
			value = "daily"
		}

	case "maintenance.threshold":
		threshold := 1000 // default
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.ScheduledMaintenance != nil {
			if patrolConfig.Patrols.ScheduledMaintenance.Threshold != nil {
				threshold = *patrolConfig.Patrols.ScheduledMaintenance.Threshold
			}
		}
		value = strconv.Itoa(threshold)
	}

	fmt.Println(value)
	return nil
}

// setLifecycleConfig sets a lifecycle.* key in daemon.json.
func setLifecycleConfig(townRoot, key, value string) error {
	patrolConfig := daemon.LoadPatrolConfig(townRoot)
	if patrolConfig == nil {
		patrolConfig = daemon.DefaultLifecycleConfig()
	}
	if patrolConfig.Patrols == nil {
		patrolConfig.Patrols = &daemon.PatrolsConfig{}
	}

	switch key {
	// Reaper
	case "lifecycle.reaper.enabled":
		b, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %w (expected true/false)", key, err)
		}
		if patrolConfig.Patrols.WispReaper == nil {
			patrolConfig.Patrols.WispReaper = &daemon.WispReaperConfig{}
		}
		patrolConfig.Patrols.WispReaper.Enabled = b

	case "lifecycle.reaper.interval":
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("invalid duration for %s: %w", key, err)
		}
		if patrolConfig.Patrols.WispReaper == nil {
			patrolConfig.Patrols.WispReaper = &daemon.WispReaperConfig{Enabled: true}
		}
		patrolConfig.Patrols.WispReaper.IntervalStr = value

	case "lifecycle.reaper.delete_age":
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("invalid duration for %s: %w", key, err)
		}
		if patrolConfig.Patrols.WispReaper == nil {
			patrolConfig.Patrols.WispReaper = &daemon.WispReaperConfig{Enabled: true}
		}
		patrolConfig.Patrols.WispReaper.DeleteAgeStr = value

	// Compactor
	case "lifecycle.compactor.enabled":
		b, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %w (expected true/false)", key, err)
		}
		if patrolConfig.Patrols.CompactorDog == nil {
			patrolConfig.Patrols.CompactorDog = &daemon.CompactorDogConfig{}
		}
		patrolConfig.Patrols.CompactorDog.Enabled = b

	case "lifecycle.compactor.interval":
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("invalid duration for %s: %w", key, err)
		}
		if patrolConfig.Patrols.CompactorDog == nil {
			patrolConfig.Patrols.CompactorDog = &daemon.CompactorDogConfig{Enabled: true}
		}
		patrolConfig.Patrols.CompactorDog.IntervalStr = value

	case "lifecycle.compactor.threshold":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return fmt.Errorf("invalid threshold for %s: expected positive integer", key)
		}
		if patrolConfig.Patrols.CompactorDog == nil {
			patrolConfig.Patrols.CompactorDog = &daemon.CompactorDogConfig{Enabled: true}
		}
		patrolConfig.Patrols.CompactorDog.Threshold = n

	// Doctor
	case "lifecycle.doctor.enabled":
		b, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %w (expected true/false)", key, err)
		}
		if patrolConfig.Patrols.DoctorDog == nil {
			patrolConfig.Patrols.DoctorDog = &daemon.DoctorDogConfig{}
		}
		patrolConfig.Patrols.DoctorDog.Enabled = b

	case "lifecycle.doctor.interval":
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("invalid duration for %s: %w", key, err)
		}
		if patrolConfig.Patrols.DoctorDog == nil {
			patrolConfig.Patrols.DoctorDog = &daemon.DoctorDogConfig{Enabled: true}
		}
		patrolConfig.Patrols.DoctorDog.IntervalStr = value

	// Backup (controls both JSONL and Dolt backup)
	case "lifecycle.backup.enabled":
		b, err := parseBool(value)
		if err != nil {
			return fmt.Errorf("invalid value for %s: %w (expected true/false)", key, err)
		}
		if patrolConfig.Patrols.JsonlGitBackup == nil {
			patrolConfig.Patrols.JsonlGitBackup = &daemon.JsonlGitBackupConfig{}
		}
		patrolConfig.Patrols.JsonlGitBackup.Enabled = b
		if patrolConfig.Patrols.DoltBackup == nil {
			patrolConfig.Patrols.DoltBackup = &daemon.DoltBackupConfig{}
		}
		patrolConfig.Patrols.DoltBackup.Enabled = b

	case "lifecycle.backup.interval":
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("invalid duration for %s: %w", key, err)
		}
		if patrolConfig.Patrols.JsonlGitBackup == nil {
			patrolConfig.Patrols.JsonlGitBackup = &daemon.JsonlGitBackupConfig{Enabled: true}
		}
		patrolConfig.Patrols.JsonlGitBackup.IntervalStr = value
		if patrolConfig.Patrols.DoltBackup == nil {
			patrolConfig.Patrols.DoltBackup = &daemon.DoltBackupConfig{Enabled: true}
		}
		patrolConfig.Patrols.DoltBackup.IntervalStr = value

	default:
		return fmt.Errorf("unknown lifecycle key: %q\n\nSupported lifecycle keys:\n  lifecycle.reaper.enabled\n  lifecycle.reaper.interval\n  lifecycle.reaper.delete_age\n  lifecycle.compactor.enabled\n  lifecycle.compactor.interval\n  lifecycle.compactor.threshold\n  lifecycle.doctor.enabled\n  lifecycle.doctor.interval\n  lifecycle.backup.enabled\n  lifecycle.backup.interval", key)
	}

	if err := daemon.SavePatrolConfig(townRoot, patrolConfig); err != nil {
		return fmt.Errorf("saving daemon config: %w", err)
	}

	fmt.Printf("Set %s = %s\n", style.Bold.Render(key), value)
	return nil
}

// getLifecycleConfig gets a lifecycle.* key from daemon.json.
func getLifecycleConfig(townRoot, key string) error {
	patrolConfig := daemon.LoadPatrolConfig(townRoot)

	var value string
	switch key {
	// Reaper
	case "lifecycle.reaper.enabled":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.WispReaper != nil {
			value = strconv.FormatBool(patrolConfig.Patrols.WispReaper.Enabled)
		} else {
			value = "false (not configured)"
		}

	case "lifecycle.reaper.interval":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.WispReaper != nil && patrolConfig.Patrols.WispReaper.IntervalStr != "" {
			value = patrolConfig.Patrols.WispReaper.IntervalStr
		} else {
			value = "30m (default)"
		}

	case "lifecycle.reaper.delete_age":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.WispReaper != nil && patrolConfig.Patrols.WispReaper.DeleteAgeStr != "" {
			value = patrolConfig.Patrols.WispReaper.DeleteAgeStr
		} else {
			value = "168h (default, 7 days)"
		}

	// Compactor
	case "lifecycle.compactor.enabled":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.CompactorDog != nil {
			value = strconv.FormatBool(patrolConfig.Patrols.CompactorDog.Enabled)
		} else {
			value = "false (not configured)"
		}

	case "lifecycle.compactor.interval":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.CompactorDog != nil && patrolConfig.Patrols.CompactorDog.IntervalStr != "" {
			value = patrolConfig.Patrols.CompactorDog.IntervalStr
		} else {
			value = "24h (default)"
		}

	case "lifecycle.compactor.threshold":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.CompactorDog != nil && patrolConfig.Patrols.CompactorDog.Threshold > 0 {
			value = strconv.Itoa(patrolConfig.Patrols.CompactorDog.Threshold)
		} else {
			value = "500 (default)"
		}

	// Doctor
	case "lifecycle.doctor.enabled":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.DoctorDog != nil {
			value = strconv.FormatBool(patrolConfig.Patrols.DoctorDog.Enabled)
		} else {
			value = "false (not configured)"
		}

	case "lifecycle.doctor.interval":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.DoctorDog != nil && patrolConfig.Patrols.DoctorDog.IntervalStr != "" {
			value = patrolConfig.Patrols.DoctorDog.IntervalStr
		} else {
			value = "5m (default)"
		}

	// Backup
	case "lifecycle.backup.enabled":
		jsonlEnabled := patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.JsonlGitBackup != nil && patrolConfig.Patrols.JsonlGitBackup.Enabled
		doltEnabled := patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.DoltBackup != nil && patrolConfig.Patrols.DoltBackup.Enabled
		if jsonlEnabled || doltEnabled {
			value = fmt.Sprintf("jsonl=%v dolt=%v", jsonlEnabled, doltEnabled)
		} else {
			value = "false (not configured)"
		}

	case "lifecycle.backup.interval":
		if patrolConfig != nil && patrolConfig.Patrols != nil && patrolConfig.Patrols.JsonlGitBackup != nil && patrolConfig.Patrols.JsonlGitBackup.IntervalStr != "" {
			value = patrolConfig.Patrols.JsonlGitBackup.IntervalStr
		} else {
			value = "15m (default)"
		}

	default:
		return fmt.Errorf("unknown lifecycle key: %q\n\nSupported lifecycle keys:\n  lifecycle.reaper.enabled\n  lifecycle.reaper.interval\n  lifecycle.reaper.delete_age\n  lifecycle.compactor.enabled\n  lifecycle.compactor.interval\n  lifecycle.compactor.threshold\n  lifecycle.doctor.enabled\n  lifecycle.doctor.interval\n  lifecycle.backup.enabled\n  lifecycle.backup.interval", key)
	}

	fmt.Println(value)
	return nil
}

// parseBool parses a boolean string (true/false, yes/no, 1/0).
func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "yes", "1", "on":
		return true, nil
	case "false", "no", "0", "off":
		return false, nil
	default:
		return false, fmt.Errorf("cannot parse %q as boolean", s)
	}
}

func init() {
	presets := config.BuiltInAgentPresetSummary()
	configAgentListCmd := newConfigAgentListCmd()
	configAgentRemoveCmd := newConfigAgentRemoveCmd()
	configDefaultAgentCmd := newConfigDefaultAgentCmd()

	configAgentListCmd.Long = fmt.Sprintf(`List all available agents (built-in and custom).

Shows all built-in agent presets (%s) and any
custom agents defined in your town settings.

Examples:
  gt config agent list           # Text output
  gt config agent list --json    # JSON output`, presets)

	configAgentRemoveCmd.Long = fmt.Sprintf(`Remove a custom agent definition from town settings.

This removes a custom agent from your town settings. Built-in agents
(%s) cannot be removed.

Examples:
  gt config agent remove claude-glm`, presets)

	configDefaultAgentCmd.Long = fmt.Sprintf(`Get or set the default agent for the town.

With no arguments, shows the current default agent.
With an argument, sets the default agent to the specified name.

The default agent is used when a rig doesn't specify its own agent
setting. Can be a built-in preset (%s) or a
custom agent name.

Use 'gt config default-agent list' to see all available agents.

Examples:
  gt config default-agent           # Show current default
  gt config default-agent list      # List available agents
  gt config default-agent claude    # Set to claude
  gt config default-agent gemini    # Set to gemini
  gt config default-agent my-custom # Set to custom agent`, presets)

	// Add flags
	configAgentListCmd.Flags().Bool("json", false, "Output as JSON")
	configDefaultAgentListCmd.Flags().Bool("json", false, "Output as JSON")
	configMixCmd.Flags().Bool("json", false, "Output the effective mix as JSON")
	configAgentSetCmd.Flags().String("provider", "", fmt.Sprintf("Agent provider preset (e.g. %s); inferred from command name if not set", presets))

	// Add agent subcommands
	configAgentCmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage agent configuration",
		Long: `Manage per-agent configuration settings.

Subcommands allow listing, getting, setting, and removing agent-specific
config values such as the default AI model or provider.`,
		RunE: requireSubcommand,
	}
	configAgentCmd.AddCommand(configAgentListCmd)
	configAgentCmd.AddCommand(configAgentGetCmd)
	configAgentCmd.AddCommand(configAgentSetCmd)
	configAgentCmd.AddCommand(configAgentRemoveCmd)
	configRoleCmd.AddCommand(configRoleListCmd)
	configRoleCmd.AddCommand(configRoleSetCmd)
	configRoleCmd.AddCommand(configRoleUnsetCmd)
	configRoleListCmd.Flags().String("rig", "", "Scope to a specific rig instead of the whole town")
	configRoleSetCmd.Flags().String("rig", "", "Scope to a specific rig instead of the whole town")
	configRoleUnsetCmd.Flags().String("rig", "", "Scope to a specific rig instead of the whole town")

	// Add default-agent subcommands
	configDefaultAgentCmd.AddCommand(configDefaultAgentListCmd)

	// Add subcommands to config
	configCmd.AddCommand(configAgentCmd)
	configCmd.AddCommand(configRoleCmd)
	configCmd.AddCommand(configMixCmd)
	configCmd.AddCommand(configCostTierCmd)
	configCmd.AddCommand(configDefaultAgentCmd)
	configCmd.AddCommand(configAgentEmailDomainCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)

	// Register with root
	rootCmd.AddCommand(configCmd)
}
