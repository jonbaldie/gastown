package cmd

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/jonbaldie/gastown/internal/ui"
	"github.com/spf13/cobra"
)

// colorizedHelpFunc wraps Cobra's default help with semantic coloring.
// Applies subtle accent color to group headers for visual hierarchy.
func colorizedHelpFunc(cmd *cobra.Command, _ []string) {
	// build full help output: Long description + Usage
	var output strings.Builder

	// include Long description first (like Cobra's default help)
	if cmd.Long != "" {
		output.WriteString(cmd.Long)
		output.WriteString("\n\n")
	} else if cmd.Short != "" {
		output.WriteString(cmd.Short)
		output.WriteString("\n\n")
	}

	// add the usage string which contains commands, flags, etc.
	output.WriteString(cmd.UsageString())

	// apply semantic coloring
	result := colorizeHelpOutput(output.String())
	fmt.Print(result)
}

// Regexes used by colorizeHelpOutput and its helpers, compiled once at
// package init rather than on every call (colorizeHelpOutput runs once per
// `gt help` invocation, but its ReplaceAllStringFunc callbacks run once per
// matched line, so per-call compilation multiplied out across every command
// and flag line in the output).
var (
	groupHeaderRE   = regexp.MustCompile(`(?m)^([A-Z][A-Za-z &]+:)\s*$`)
	sectionHeaderRE = regexp.MustCompile(`(?m)^(Examples|Flags|Usage|Global Flags|Aliases|Available Commands):`)
	cmdLineRE       = regexp.MustCompile(`(?m)^(  )([a-z][a-z0-9]*(?:-[a-z0-9]+)*)(\s{2,})(.*)$`)
	flagLineRE      = regexp.MustCompile(`(?m)^(\s+)(-\w,\s+--[\w-]+|--[\w-]+)(\s+)(string|int|duration|bool)?(\s*.*)$`)
	defaultRE       = regexp.MustCompile(`(\(default[^)]*\))`)
	entryRE         = regexp.MustCompile(`(\(start here\))`)
	cmdRefRE        = regexp.MustCompile(`'([a-z][a-z0-9 -]+)'`)
)

// colorizeHelpOutput applies semantic colors to help text
// - Group headers get accent color for visual hierarchy
// - Section headers (Examples:, Flags:) get accent color
// - Command names get subtle styling for scanability
// - Flag names get bold styling, types get muted
// - Default values get muted styling
func colorizeHelpOutput(help string) string {
	// match group header lines (e.g., "Working With Issues:")
	// these are standalone lines ending with ":" and followed by commands
	result := groupHeaderRE.ReplaceAllStringFunc(help, func(match string) string {
		// trim whitespace, colorize, then restore
		trimmed := strings.TrimSpace(match)
		return ui.RenderAccent(trimmed)
	})

	// match section headers in subcommand help (Examples:, Flags:, etc.)
	result = sectionHeaderRE.ReplaceAllStringFunc(result, func(match string) string {
		return ui.RenderAccent(match)
	})

	// match command lines: "  command   Description text"
	// commands are indented with 2 spaces, followed by spaces, then description
	// pattern matches: indent + command-name (with hyphens) + spacing + description
	result = cmdLineRE.ReplaceAllStringFunc(result, func(match string) string {
		parts := cmdLineRE.FindStringSubmatch(match)
		if len(parts) != 5 {
			return match
		}
		indent := parts[1]
		cmdName := parts[2]
		spacing := parts[3]
		description := parts[4]

		// colorize command references in description (e.g., 'comments add')
		description = colorizeCommandRefs(description)

		// highlight entry point hints (e.g., "(start here)")
		description = highlightEntryPoints(description)

		// subtle styling on command name for scanability
		return indent + ui.RenderCommand(cmdName) + spacing + description
	})

	// match flag lines: "  -f, --file string   Description"
	// pattern: indent + flags + spacing + optional type + description
	result = flagLineRE.ReplaceAllStringFunc(result, func(match string) string {
		parts := flagLineRE.FindStringSubmatch(match)
		if len(parts) < 6 {
			return match
		}
		indent := parts[1]
		flags := parts[2]
		spacing := parts[3]
		typeStr := parts[4]
		desc := parts[5]

		// mute default values in description
		desc = muteDefaults(desc)

		if typeStr != "" {
			return indent + ui.RenderCommand(flags) + spacing + ui.RenderMuted(typeStr) + desc
		}
		return indent + ui.RenderCommand(flags) + spacing + desc
	})

	return result
}

// muteDefaults applies muted styling to default value annotations
func muteDefaults(text string) string {
	return defaultRE.ReplaceAllStringFunc(text, func(match string) string {
		return ui.RenderMuted(match)
	})
}

// highlightEntryPoints applies accent styling to entry point hints like "(start here)"
func highlightEntryPoints(text string) string {
	return entryRE.ReplaceAllStringFunc(text, func(match string) string {
		return ui.RenderAccent(match)
	})
}

// colorizeCommandRefs applies command styling to references in text
// Matches patterns like 'command name' or 'bd command'
func colorizeCommandRefs(text string) string {
	// match 'command words' in single quotes (e.g., 'comments add')
	return cmdRefRE.ReplaceAllStringFunc(text, func(match string) string {
		// extract the command name without quotes
		inner := match[1 : len(match)-1]
		return "'" + ui.RenderCommand(inner) + "'"
	})
}

func init() {
	// Set custom help function for colorized output
	rootCmd.SetHelpFunc(colorizedHelpFunc)
}
