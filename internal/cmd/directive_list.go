package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var directiveListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all directive files",
	Long: `List all directive files across town and rig levels.

Shows each directive file with its scope (town or rig) and role.

Examples:
  gt directive list`,
	RunE: runDirectiveList,
}

func init() {
	directiveCmd.AddCommand(directiveListCmd)
}

type directiveListEntry struct {
	scope string // "town" or rig name
	role  string
	path  string
}

func runDirectiveList(_ *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	entries := collectDirectiveEntries(townRoot)
	printDirectiveEntries(entries)
	return nil
}

func collectDirectiveEntries(townRoot string) []directiveListEntry {
	entries := collectDirectiveFiles(filepath.Join(townRoot, "directives"), "town")
	rigDirs, err := os.ReadDir(townRoot)
	if err != nil {
		return entries
	}
	for _, d := range rigDirs {
		if !d.IsDir() || !hasRigConfig(townRoot, d.Name()) {
			continue
		}
		entries = append(entries, collectDirectiveFiles(filepath.Join(townRoot, d.Name(), "directives"), d.Name())...)
	}
	return entries
}

func collectDirectiveFiles(dir, scope string) []directiveListEntry {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	entries := make([]directiveListEntry, 0, len(files))
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, f.Name())
		if fileHasContent(path) {
			entries = append(entries, directiveListEntry{
				scope: scope,
				role:  strings.TrimSuffix(f.Name(), ".md"),
				path:  path,
			})
		}
	}
	return entries
}

func hasRigConfig(townRoot, rigName string) bool {
	_, err := os.Stat(filepath.Join(townRoot, rigName, "config.json"))
	return err == nil
}

func printDirectiveEntries(entries []directiveListEntry) {
	if len(entries) == 0 {
		fmt.Println("No directive files found.")
		fmt.Println("\nUse 'gt directive edit <role>' to create one.")
		return
	}

	fmt.Printf("Directive files (%d):\n\n", len(entries))
	fmt.Printf("  %-10s %-12s %s\n", "SCOPE", "ROLE", "PATH")
	fmt.Printf("  %-10s %-12s %s\n", "─────", "────", "────")
	for _, e := range entries {
		fmt.Printf("  %-10s %-12s %s\n", e.scope, e.role, e.path)
	}
}
