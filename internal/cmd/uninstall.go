// ABOUTME: Command to completely uninstall Gas Town from the system.
// ABOUTME: Removes shell integration, wrappers, state, and optionally workspace.

package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/shell"
	"github.com/jonbaldie/gastown/internal/state"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/wrappers"
	"github.com/spf13/cobra"
)

var uninstallCmd = &cobra.Command{
	Use:     "uninstall",
	GroupID: GroupConfig,
	Short:   "Remove Gas Town from the system",
	Long: `Completely remove Gas Town from the system.

By default, removes:
  - Shell integration (~/.zshrc or ~/.bashrc)
  - Wrapper scripts (~/bin/gt-codex, ~/bin/gt-gemini, ~/bin/gt-opencode)
  - State directory (~/.local/state/gastown/)
  - Config directory (~/.config/gastown/)
  - Cache directory (~/.cache/gastown/)

The workspace (e.g., ~/gt) is NOT removed unless --workspace is specified.

Use --force to skip confirmation prompts.

Examples:
  gt uninstall                    # Remove Gas Town, keep workspace
  gt uninstall --workspace        # Also remove workspace directory
  gt uninstall --force            # Skip confirmation`,
	RunE: runUninstall,
}

func init() {
	uninstallCmd.Flags().Bool("workspace", false,
		"Also remove the workspace directory (DESTRUCTIVE)")
	uninstallCmd.Flags().BoolP("force", "f", false,
		"Skip confirmation prompts")
	rootCmd.AddCommand(uninstallCmd)
}

func runUninstall(cmd *cobra.Command, _ []string) error {
	workspaceFlag := commandBoolFlag(cmd, "workspace")
	force := commandBoolFlag(cmd, "force")
	if !confirmUninstall(force, workspaceFlag) {
		return nil
	}

	fmt.Println()
	fmt.Println("Removing Gas Town...")
	errors := removeUninstallComponents(workspaceFlag)
	if len(errors) > 0 {
		printUninstallFailures(errors)
		return fmt.Errorf("uninstall incomplete")
	}

	printUninstallSuccess()
	return nil
}

func confirmUninstall(force, workspaceFlag bool) bool {
	if force {
		return true
	}
	printUninstallNotice(workspaceFlag)
	fmt.Print("Continue? [y/N] ")

	reader := bufio.NewReader(os.Stdin)
	response, _ := reader.ReadString('\n')
	response = strings.TrimSpace(strings.ToLower(response))
	if response == "y" || response == "yes" {
		return true
	}
	fmt.Println("Aborted.")
	return false
}

func printUninstallNotice(workspaceFlag bool) {
	fmt.Println("This will remove Gas Town from your system.")
	fmt.Println()
	fmt.Println("The following will be removed:")
	fmt.Printf("  • Shell integration (%s)\n", shell.RCFilePath(shell.DetectShell()))
	fmt.Printf("  • Wrapper scripts (%s)\n", wrappers.BinDir())
	fmt.Printf("  • State directory (%s)\n", state.StateDir())
	fmt.Printf("  • Config directory (%s)\n", state.ConfigDir())
	fmt.Printf("  • Cache directory (%s)\n", state.CacheDir())
	if workspaceFlag {
		fmt.Println()
		fmt.Printf("  %s WORKSPACE WILL BE DELETED\n", style.Warning.Render("⚠"))
		fmt.Println("     This cannot be undone!")
	}
	fmt.Println()
}

func removeUninstallComponents(workspaceFlag bool) []string {
	var failures []string
	appendFailure := func(failure string) {
		if failure != "" {
			failures = append(failures, failure)
		}
	}
	appendFailure(removeUninstallComponent("shell integration", "Removed shell integration", shell.Remove))
	appendFailure(removeUninstallComponent("wrapper scripts", "Removed wrapper scripts", wrappers.Remove))
	appendFailure(removeUninstallDirectory("state directory", state.StateDir()))
	appendFailure(removeUninstallDirectory("config directory", state.ConfigDir()))
	appendFailure(removeUninstallDirectory("cache directory", state.CacheDir()))
	if workspaceFlag {
		appendFailure(removeUninstallWorkspace())
	}
	return failures
}

func removeUninstallComponent(label, success string, remove func() error) string {
	if err := remove(); err != nil {
		return fmt.Sprintf("%s: %v", label, err)
	}
	fmt.Printf("  %s %s\n", style.Success.Render("✓"), success)
	return ""
}

func removeUninstallDirectory(label, path string) string {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return fmt.Sprintf("%s: %v", label, err)
	}
	fmt.Printf("  %s Removed %s\n", style.Success.Render("✓"), label)
	return ""
}

func removeUninstallWorkspace() string {
	workspaceDir := findWorkspaceForUninstall()
	if workspaceDir == "" {
		return ""
	}
	if err := os.RemoveAll(workspaceDir); err != nil {
		return fmt.Sprintf("workspace: %v", err)
	}
	fmt.Printf("  %s Removed workspace: %s\n", style.Success.Render("✓"), workspaceDir)
	return ""
}

func printUninstallFailures(failures []string) {
	fmt.Println()
	fmt.Printf("%s Some components could not be removed:\n", style.Warning.Render("⚠"))
	for _, failure := range failures {
		fmt.Printf("  • %s\n", failure)
	}
}

func printUninstallSuccess() {
	fmt.Println()
	fmt.Printf("%s Gas Town has been uninstalled\n", style.Success.Render("✓"))
	fmt.Println()
	fmt.Println("To reinstall, run:")
	fmt.Printf("  %s\n", style.Dim.Render("go install github.com/jonbaldie/gastown/cmd/gt@latest"))
	fmt.Printf("  %s\n", style.Dim.Render("gt install ~/gt --shell"))
}

func findWorkspaceForUninstall() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	candidates := []string{
		filepath.Join(home, "gt"),
		filepath.Join(home, "gastown"),
	}

	for _, path := range candidates {
		mayorDir := filepath.Join(path, "mayor")
		if _, err := os.Stat(mayorDir); err == nil {
			return path
		}
	}

	return ""
}
