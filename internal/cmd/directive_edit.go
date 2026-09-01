package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/spf13/cobra"
)

var directiveEditCmd = &cobra.Command{
	Use:   "edit <role>",
	Short: "Edit directive for a role",
	Long: `Open the directive file for a role in $EDITOR.

Creates the directory and file if they do not exist. By default, edits the
rig-level directive (if a rig is detected) or the town-level directive.

Use --town to explicitly edit the town-level directive.

Examples:
  gt directive edit polecat             # Edit rig-level polecat directive
  gt directive edit crew --rig sky      # Edit sky rig crew directive
  gt directive edit witness --town      # Edit town-level witness directive`,
	Args: cobra.ExactArgs(1),
	RunE: runDirectiveEdit,
}

func init() {
	directiveCmd.AddCommand(directiveEditCmd)
	directiveEditCmd.Flags().String("rig", "", "Rig name (default: auto-detect from cwd)")
	directiveEditCmd.Flags().Bool("town", false, "Edit town-level directive instead of rig-level")
}

type directiveEditOptions struct {
	rig  string
	town bool
}

func readDirectiveEditOptions(cmd *cobra.Command) (directiveEditOptions, error) {
	rig, err := cmd.Flags().GetString("rig")
	if err != nil {
		return directiveEditOptions{}, err
	}
	town, err := cmd.Flags().GetBool("town")
	if err != nil {
		return directiveEditOptions{}, err
	}
	return directiveEditOptions{rig: rig, town: town}, nil
}

func runDirectiveEdit(cmd *cobra.Command, args []string) error {
	opts, err := readDirectiveEditOptions(cmd)
	if err != nil {
		return err
	}
	role := args[0]

	if !isValidRole(role) {
		return fmt.Errorf("unknown role %q — valid roles: %s", role, strings.Join(config.AllRoles(), ", "))
	}

	townRoot, rigName, err := resolveDirectiveContext(opts.rig)
	if err != nil {
		return err
	}

	path := directivePath(townRoot, rigName, role, opts.town)
	if err := ensureDirectiveFile(path, role); err != nil {
		return err
	}
	if err := openDirectiveEditor(path); err != nil {
		return err
	}

	fmt.Printf("Directive updated: %s\n", path)
	fmt.Println("Changes take effect at next 'gt prime'.")
	return nil
}

func directivePath(townRoot, rigName, role string, town bool) string {
	if town || rigName == "" {
		return filepath.Join(townRoot, "directives", role+".md")
	}
	return filepath.Join(townRoot, rigName, "directives", role+".md")
}

func ensureDirectiveFile(path, role string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Create with a helpful header comment
		initial := fmt.Sprintf("<!-- Directive for role: %s -->\n<!-- This content is injected at prime time. -->\n\n", role)
		if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
			return fmt.Errorf("creating directive file: %w", err)
		}
		fmt.Printf("Created new directive: %s\n", path)
	}
	return nil
}

func openDirectiveEditor(path string) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}

	editorCmd := exec.Command(editor, path)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return fmt.Errorf("running editor: %w", err)
	}
	return nil
}
