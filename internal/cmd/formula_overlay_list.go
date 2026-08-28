package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var formulaOverlayListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all overlay files",
	Long: `List all formula overlay files across town and rig levels.

Shows each overlay file with its scope (town or rig) and formula name.

Examples:
  gt formula overlay list`,
	RunE: runFormulaOverlayList,
}

func init() {
	formulaOverlayCmd.AddCommand(formulaOverlayListCmd)
}

type formulaOverlayListEntry struct {
	scope   string // "town" or rig name
	formula string
	path    string
}

func runFormulaOverlayList(_ *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	entries := collectFormulaOverlayEntries(townRoot)
	printFormulaOverlayEntries(entries)
	return nil
}

func collectFormulaOverlayEntries(townRoot string) []formulaOverlayListEntry {
	entries := collectFormulaOverlayFiles(filepath.Join(townRoot, "formula-overlays"), "town")
	rigDirs, err := os.ReadDir(townRoot)
	if err != nil {
		return entries
	}
	for _, d := range rigDirs {
		if !d.IsDir() || !hasRigConfig(townRoot, d.Name()) {
			continue
		}
		entries = append(entries, collectFormulaOverlayFiles(filepath.Join(townRoot, d.Name(), "formula-overlays"), d.Name())...)
	}
	return entries
}

func collectFormulaOverlayFiles(dir, scope string) []formulaOverlayListEntry {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	entries := make([]formulaOverlayListEntry, 0, len(files))
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".toml") {
			continue
		}
		entries = append(entries, formulaOverlayListEntry{
			scope:   scope,
			formula: strings.TrimSuffix(f.Name(), ".toml"),
			path:    filepath.Join(dir, f.Name()),
		})
	}
	return entries
}

func printFormulaOverlayEntries(entries []formulaOverlayListEntry) {
	if len(entries) == 0 {
		fmt.Println("No overlay files found.")
		fmt.Println("\nUse 'gt formula overlay edit <formula>' to create one.")
		return
	}

	fmt.Printf("Formula overlay files (%d):\n\n", len(entries))
	fmt.Printf("  %-10s %-30s %s\n", "SCOPE", "FORMULA", "PATH")
	fmt.Printf("  %-10s %-30s %s\n", "─────", "───────", "────")
	for _, e := range entries {
		fmt.Printf("  %-10s %-30s %s\n", e.scope, e.formula, e.path)
	}
}
