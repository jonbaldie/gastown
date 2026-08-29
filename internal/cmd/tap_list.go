package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

var tapListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available tap handlers",
	Long: `List all tap handlers (guards, audits, injectors, checks).

Shows both registered (from registry.toml) and built-in tap commands.

Examples:
  gt tap list               # Show all available handlers
  gt tap list --guards      # Show only guard handlers`,
	RunE: runTapList,
}

var tapListGuardsOnly bool

func init() {
	tapCmd.AddCommand(tapListCmd)
	tapListCmd.Flags().BoolVar(&tapListGuardsOnly, "guards", false, "Show only guard handlers")
}

// tapHandler describes a tap handler for display.
type tapHandler struct {
	name        string
	kind        string // guard, audit, inject, check
	description string
	event       string
	matchers    []string
	implemented bool
}

func runTapList(_ *cobra.Command, _ []string) error {
	handlers := builtInTapHandlers()
	handlers = appendRegistryTapHandlers(handlers, tapListGuardsOnly)
	handlers = prepareTapHandlers(handlers, tapListGuardsOnly)
	return renderTapHandlers(handlers)
}

func builtInTapHandlers() []tapHandler {
	return []tapHandler{
		{
			name:        "pr-workflow",
			kind:        "guard",
			description: "Block PR creation and feature branches",
			event:       "PreToolUse",
			matchers:    []string{"Bash(gh pr create*)", "Bash(git checkout -b*)", "Bash(git switch -c*)"},
			implemented: true,
		},
		{
			name:        "dangerous-command",
			kind:        "guard",
			description: "Block sudo, package installs, rm -rf, force push, hard reset, etc.",
			event:       "PreToolUse",
			matchers:    []string{"Bash(sudo *)", "Bash(apt install*)", "Bash(dnf install*)", "Bash(brew install*)", "Bash(rm -rf /*)", "Bash(git push --force*)", "Bash(git push -f*)"},
			implemented: true,
		},
	}
}

func appendRegistryTapHandlers(handlers []tapHandler, guardsOnly bool) []tapHandler {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return handlers
	}
	registry, err := LoadRegistry(townRoot)
	if err != nil {
		return handlers
	}
	for name, def := range registry.Hooks {
		// Skip hooks already listed as built-in.
		if isBuiltIn(name, handlers) {
			continue
		}

		kind := classifyHook(def.Command)
		if guardsOnly && kind != "guard" {
			continue
		}

		handlers = append(handlers, tapHandler{
			name:        name,
			kind:        kind,
			description: def.Description,
			event:       def.Event,
			matchers:    def.Matchers,
			implemented: def.Enabled,
		})
	}
	return handlers
}

func prepareTapHandlers(handlers []tapHandler, guardsOnly bool) []tapHandler {
	sort.Slice(handlers, func(i, j int) bool {
		if handlers[i].kind != handlers[j].kind {
			return handlers[i].kind < handlers[j].kind
		}
		return handlers[i].name < handlers[j].name
	})

	if !guardsOnly {
		return handlers
	}
	var filtered []tapHandler
	for _, h := range handlers {
		if h.kind == "guard" {
			filtered = append(filtered, h)
		}
	}
	return filtered
}

func renderTapHandlers(handlers []tapHandler) error {
	if len(handlers) == 0 {
		fmt.Println(style.Dim.Render("No tap handlers found"))
		return nil
	}

	fmt.Printf("\n%s Tap Handlers\n\n", style.Bold.Render("⚡"))
	byKind := groupTapHandlersByKind(handlers)
	kindOrder := []string{"guard", "audit", "inject", "check", "hook"}
	for _, kind := range kindOrder {
		group := byKind[kind]
		if len(group) == 0 {
			continue
		}
		renderTapHandlerGroup(kind, group)
	}
	return nil
}

func groupTapHandlersByKind(handlers []tapHandler) map[string][]tapHandler {
	byKind := make(map[string][]tapHandler)
	for _, h := range handlers {
		byKind[h.kind] = append(byKind[h.kind], h)
	}
	return byKind
}

func renderTapHandlerGroup(kind string, group []tapHandler) {
	kindLabel := cases.Title(language.English).String(kind) + "s"
	fmt.Printf("%s %s\n", style.Bold.Render("▸"), kindLabel)
	for _, h := range group {
		renderTapHandler(h)
	}
	fmt.Println()
}

func renderTapHandler(handler tapHandler) {
	statusIcon := "●"
	statusStyle := style.Success
	if !handler.implemented {
		statusIcon = "○"
		statusStyle = style.Dim
	}

	fmt.Printf("  %s %s\n", statusStyle.Render(statusIcon), style.Bold.Render(handler.name))
	fmt.Printf("    %s\n", handler.description)
	fmt.Printf("    %s %s  %s %s\n",
		style.Dim.Render("event:"), handler.event,
		style.Dim.Render("matchers:"), strings.Join(handler.matchers, ", "))
}

func isBuiltIn(name string, handlers []tapHandler) bool {
	for _, h := range handlers {
		if h.name == name || h.name+"-guard" == name {
			return true
		}
	}
	return false
}

func classifyHook(command string) string {
	if strings.Contains(command, "guard") {
		return "guard"
	}
	if strings.Contains(command, "audit") {
		return "audit"
	}
	if strings.Contains(command, "inject") {
		return "inject"
	}
	if strings.Contains(command, "check") {
		return "check"
	}
	return "hook"
}
