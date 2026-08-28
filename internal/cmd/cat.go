package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/spf13/cobra"
)

type catOptions struct {
	json bool
}

const catLongDescription = `Display the content of a bead (issue, task, molecule, etc.).

This is a convenience wrapper around 'bd show' that integrates with gt.
Accepts any bead ID with a recognized prefix (gt-*, bd-*, hq-*, mol-*, etc.).

Examples:
  gt cat gt-abc123       # Show a gastown bead
  gt cat bd-abc123       # Show a beads bead
  gt cat hq-xyz789       # Show a town-level bead
  gt cat bd-abc --json   # Output as JSON`

func init() {
	rootCmd.AddCommand(newCatCommand())
}

func newCatCommand() *cobra.Command {
	opts := &catOptions{}
	cmd := &cobra.Command{
		Use:     "cat <bead-id>",
		GroupID: GroupWork,
		Short:   "Display bead content",
		Long:    catLongDescription,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runCat(opts, args)
		},
	}
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output as JSON")
	return cmd
}

func runCat(opts *catOptions, args []string) error {
	beadID := args[0]

	// Validate it looks like a bead ID
	if !isBeadID(beadID) {
		return fmt.Errorf("invalid bead ID %q (expected format: <prefix>-<id>, e.g. gt-abc123)", beadID)
	}

	// Build bd show command
	bdArgs := []string{"show", beadID}
	if opts.json {
		bdArgs = append(bdArgs, "--json")
	}

	bdCmd := beads.Spawn(bdArgs...)
	bdCmd.Stdout = os.Stdout
	bdCmd.Stderr = os.Stderr
	// Route to the correct rig database via prefix resolution.
	if dir := resolveBeadDir(beadID); dir != "" && dir != "." {
		bdCmd.Dir = dir
		bdCmd.Env = filterEnvKey(os.Environ(), "BEADS_DIR")
	}

	return bdCmd.Run()
}

// isBeadID checks if a string looks like a bead ID.
// Bead IDs have the format <prefix>-<id> where prefix starts with a
// lowercase letter and may contain underscores (e.g. gt-abc123,
// japanese_reader-id3a).
func isBeadID(s string) bool {
	dashIdx := strings.Index(s, "-")
	if dashIdx <= 0 || dashIdx >= len(s)-1 {
		return false
	}
	return validBeadPrefix(s[:dashIdx])
}

func validBeadPrefix(prefix string) bool {
	for i, c := range prefix {
		if i == 0 {
			if c < 'a' || c > 'z' {
				return false
			}
			continue
		}
		if !((c >= 'a' && c <= 'z') || c == '_') {
			return false
		}
	}
	return true
}
