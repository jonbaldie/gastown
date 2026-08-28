package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// HookRegistry represents the hooks/registry.toml structure.
type HookRegistry struct {
	Hooks map[string]HookDefinition `toml:"hooks"`
}

// HookDefinition represents a single hook definition in the registry.
type HookDefinition struct {
	Description string   `toml:"description"`
	Event       string   `toml:"event"`
	Matchers    []string `toml:"matchers"`
	Command     string   `toml:"command"`
	Roles       []string `toml:"roles"`
	Scope       string   `toml:"scope"`
	Enabled     bool     `toml:"enabled"`
}

type hookRegistryEntry struct {
	name string
	def  HookDefinition
}

var (
	hooksRegistryAll     bool
	hooksRegistryVerbose bool
)

var hooksRegistryCmd = &cobra.Command{
	Use:   "registry",
	Short: "List available hooks from the registry",
	Long: `List all hooks defined in the hook registry.

The registry is at ~/gt/hooks/registry.toml and defines hooks that can be
installed for different roles (crew, polecat, witness, etc.).

Examples:
  gt hooks registry           # Show enabled hooks
  gt hooks registry --all     # Show all hooks including disabled`,
	RunE: runHooksRegistry,
}

func init() {
	hooksCmd.AddCommand(hooksRegistryCmd)
	hooksRegistryCmd.Flags().BoolVarP(&hooksRegistryAll, "all", "a", false, "Show all hooks including disabled")
	hooksRegistryCmd.Flags().BoolVarP(&hooksRegistryVerbose, "verbose", "v", false, "Show hook commands and matchers")
}

// LoadRegistry loads the hook registry from the town's hooks directory.
func LoadRegistry(townRoot string) (*HookRegistry, error) {
	registryPath := filepath.Join(townRoot, "hooks", "registry.toml")

	data, err := os.ReadFile(registryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("hook registry not found at %s", registryPath)
		}
		return nil, fmt.Errorf("reading registry: %w", err)
	}

	var registry HookRegistry
	if _, err := toml.Decode(string(data), &registry); err != nil {
		return nil, fmt.Errorf("parsing registry: %w", err)
	}

	return &registry, nil
}

func runHooksRegistry(_ *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	registry, err := LoadRegistry(townRoot)
	if err != nil {
		return err
	}

	if len(registry.Hooks) == 0 {
		fmt.Println(style.Dim.Render("No hooks defined in registry"))
		return nil
	}

	fmt.Printf("\n%s Hook Registry\n", style.Bold.Render("📋"))
	fmt.Printf("Source: %s\n\n", style.Dim.Render(filepath.Join(townRoot, "hooks", "registry.toml")))

	byEvent, eventOrder := groupRegistryHooks(registry)
	count := printRegistryEvents(byEvent, eventOrder)

	fmt.Printf("%s %d hooks in registry\n", style.Dim.Render("Total:"), count)

	return nil
}

func groupRegistryHooks(registry *HookRegistry) (map[string][]hookRegistryEntry, []string) {
	byEvent := make(map[string][]hookRegistryEntry)
	for name, def := range registry.Hooks {
		if !hooksRegistryAll && !def.Enabled {
			continue
		}
		byEvent[def.Event] = append(byEvent[def.Event], hookRegistryEntry{name: name, def: def})
	}
	return byEvent, registryEventOrder(byEvent)
}

func registryEventOrder(byEvent map[string][]hookRegistryEntry) []string {
	// Add any events not in the predefined order.
	eventOrder := []string{"PreToolUse", "PostToolUse", "SessionStart", "PreCompact", "UserPromptSubmit", "Stop", "WorktreeCreate", "WorktreeRemove"}
	for event := range byEvent {
		if containsRegistryEvent(eventOrder, event) {
			continue
		}
		eventOrder = append(eventOrder, event)
	}
	return eventOrder
}

func containsRegistryEvent(eventOrder []string, event string) bool {
	for _, existing := range eventOrder {
		if event == existing {
			return true
		}
	}
	return false
}

func printRegistryEvents(byEvent map[string][]hookRegistryEntry, eventOrder []string) int {
	count := 0
	for _, event := range eventOrder {
		eventHooks := byEvent[event]
		if len(eventHooks) == 0 {
			continue
		}

		fmt.Printf("%s %s\n", style.Bold.Render("▸"), event)
		for _, hook := range eventHooks {
			printRegistryHook(hook)
			count++
		}
		fmt.Println()
	}
	return count
}

func printRegistryHook(hook hookRegistryEntry) {
	statusIcon := "●"
	statusColor := style.Success
	if !hook.def.Enabled {
		statusIcon = "○"
		statusColor = style.Dim
	}

	rolesStr := strings.Join(hook.def.Roles, ", ")
	fmt.Printf("  %s %s\n", statusColor.Render(statusIcon), style.Bold.Render(hook.name))
	fmt.Printf("    %s\n", hook.def.Description)
	fmt.Printf("    %s %s  %s %s\n",
		style.Dim.Render("roles:"), rolesStr,
		style.Dim.Render("scope:"), hook.def.Scope)

	if hooksRegistryVerbose {
		fmt.Printf("    %s %s\n", style.Dim.Render("command:"), hook.def.Command)
		for _, matcher := range hook.def.Matchers {
			fmt.Printf("    %s %s\n", style.Dim.Render("matcher:"), matcher)
		}
	}
}
