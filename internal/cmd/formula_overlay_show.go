package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/formula"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var formulaOverlayShowCmd = &cobra.Command{
	Use:   "show <formula>",
	Short: "Show active overlay for a formula",
	Long: `Display the resolved overlay content for a formula with source annotation.

Shows which file provides the overlay (town-level or rig-level) and its contents.

Examples:
  gt formula overlay show mol-polecat-work
  gt formula overlay show mol-polecat-work --rig gastown`,
	Args: cobra.ExactArgs(1),
	RunE: runFormulaOverlayShow,
}

func init() {
	formulaOverlayCmd.AddCommand(formulaOverlayShowCmd)
	formulaOverlayShowCmd.Flags().String("rig", "", "Rig name (default: auto-detect from cwd)")
}

func runFormulaOverlayShow(cmd *cobra.Command, args []string) error {
	formulaName := args[0]

	explicitRig := ""
	if flag := cmd.Flags().Lookup("rig"); flag != nil {
		explicitRig = flag.Value.String()
	}
	townRoot, rigName, err := resolveOverlayContext(explicitRig)
	if err != nil {
		return err
	}

	paths := overlayPaths(townRoot, rigName, formulaName)

	// Load and validate overlay
	overlay, err := formula.LoadFormulaOverlay(formulaName, townRoot, rigName)
	if err != nil {
		return fmt.Errorf("loading overlay: %w", err)
	}

	if overlay == nil {
		printMissingOverlay(formulaName, paths)
		return nil
	}

	source, sourceLabel := resolveOverlaySource(paths, rigName)
	printOverlayHeader(formulaName, source, sourceLabel, len(overlay.StepOverrides))
	return printOverlayFile(source)
}

type overlayFilePaths struct {
	town string
	rig  string
}

func overlayPaths(townRoot, rigName, formulaName string) overlayFilePaths {
	paths := overlayFilePaths{
		town: filepath.Join(townRoot, "formula-overlays", formulaName+".toml"),
	}
	if rigName != "" {
		paths.rig = filepath.Join(townRoot, rigName, "formula-overlays", formulaName+".toml")
	}
	return paths
}

func printMissingOverlay(formulaName string, paths overlayFilePaths) {
	fmt.Printf("No overlay found for formula %q\n", formulaName)
	fmt.Printf("  Checked: %s\n", paths.town)
	if paths.rig != "" {
		fmt.Printf("  Checked: %s\n", paths.rig)
	}
	fmt.Println("\nUse 'gt formula overlay edit' to create one.")
}

func resolveOverlaySource(paths overlayFilePaths, rigName string) (string, string) {
	if paths.rig != "" {
		if _, err := os.Stat(paths.rig); err == nil {
			return paths.rig, "rig:" + rigName
		}
	}
	return paths.town, "town"
}

func printOverlayHeader(formulaName, source, sourceLabel string, stepOverrides int) {
	fmt.Printf("# Overlay: %s (%s)\n", formulaName, sourceLabel)
	fmt.Printf("# Source: %s\n", source)
	fmt.Printf("# Step overrides: %d\n", stepOverrides)
	fmt.Println()
}

func printOverlayFile(source string) error {
	data, err := os.ReadFile(source) //nolint:gosec // G304: path from trusted overlay directory
	if err != nil {
		return fmt.Errorf("reading overlay file: %w", err)
	}
	content := string(data)
	fmt.Print(content)
	if !strings.HasSuffix(content, "\n") {
		fmt.Println()
	}

	return nil
}

// resolveOverlayContext finds the town root and rig name for overlay commands.
func resolveOverlayContext(explicitRig string) (townRoot, rigName string, err error) {
	townRoot, err = workspace.FindFromCwdOrError()
	if err != nil {
		return "", "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	rigName = explicitRig
	if rigName == "" {
		cwd, err := os.Getwd()
		if err == nil {
			rigName = detectRigFromPath(townRoot, cwd)
		}
	}

	return townRoot, rigName, nil
}
