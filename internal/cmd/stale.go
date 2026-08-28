package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/version"
	"github.com/spf13/cobra"
)

var staleCmd = &cobra.Command{
	Use:     "stale",
	GroupID: GroupDiag,
	Short:   "Check if the gt binary is stale",
	Long: `Check if the gt binary was built from an older commit than a build ref.

This command compares the commit hash embedded in the binary at build time
with the resolved build branch of the gastown repository (main/master/carry/*).

Examples:
  gt stale              # Human-readable output
  gt stale --json       # Machine-readable JSON output
  gt stale --quiet      # Exit code only (0=stale, 1=fresh, 2=undetermined)

Exit codes:
  0 - Binary is stale
  1 - Binary is fresh (up to date)
  2 - Error or skipped (could not determine staleness)`,
	RunE: runStale,
}

func init() {
	staleCmd.Flags().Bool("json", false, "Output as JSON")
	staleCmd.Flags().BoolP("quiet", "q", false, "Exit code only (0=stale, 1=fresh, 2=undetermined)")
	rootCmd.AddCommand(staleCmd)
}

// StaleOutput represents the JSON output structure.
type StaleOutput struct {
	Stale         bool   `json:"stale"`
	Forward       bool   `json:"forward"`
	OnMainBranch  bool   `json:"on_main_branch"`
	SafeToRebuild bool   `json:"safe_to_rebuild"`
	BinaryCommit  string `json:"binary_commit"`
	RepoCommit    string `json:"repo_commit"`
	CompareRef    string `json:"compare_ref,omitempty"`
	CommitsBehind int    `json:"commits_behind,omitempty"`
	Skipped       bool   `json:"skipped,omitempty"`
	SkipReason    string `json:"skip_reason,omitempty"`
	Error         string `json:"error,omitempty"`
}

func staleOutputFlags(cmd *cobra.Command) (jsonOutput, quiet bool) {
	if cmd == nil {
		return false, false
	}
	jsonOutput, _ = cmd.Flags().GetBool("json")
	quiet, _ = cmd.Flags().GetBool("quiet")
	return jsonOutput, quiet
}

func staleRepoError(err error, jsonOutput, quiet bool) error {
	if quiet {
		return NewSilentExit(2)
	}
	if jsonOutput {
		return outputStaleJSON(StaleOutput{Error: err.Error()})
	}
	return fmt.Errorf("cannot find gastown repo: %w", err)
}

func staleCheckError(err error, jsonOutput, quiet bool) error {
	if quiet {
		return NewSilentExit(2)
	}
	if jsonOutput {
		return outputStaleJSON(StaleOutput{Error: err.Error()})
	}
	return fmt.Errorf("staleness check failed: %w", err)
}

func staleOutput(info *version.StaleBinaryInfo) StaleOutput {
	return StaleOutput{
		Stale:         info.IsStale,
		Forward:       info.IsForward,
		OnMainBranch:  info.OnMainBranch,
		SafeToRebuild: info.IsStale && info.IsForward && info.OnMainBranch,
		BinaryCommit:  info.BinaryCommit,
		RepoCommit:    info.RepoCommit,
		CompareRef:    info.CompareRef,
		CommitsBehind: info.CommitsBehind,
		Skipped:       info.Skipped,
		SkipReason:    info.SkipReason,
	}
}

func runStale(cmd *cobra.Command, _ []string) error {
	jsonOutput, quiet := staleOutputFlags(cmd)
	// Find the gastown repo
	repoRoot, err := version.GetRepoRoot()
	if err != nil {
		return staleRepoError(err, jsonOutput, quiet)
	}

	// Check staleness
	info := version.CheckStaleBinary(repoRoot)

	// Handle errors
	if info.Error != nil {
		return staleCheckError(info.Error, jsonOutput, quiet)
	}

	// Quiet mode: just exit with appropriate code
	if quiet {
		return NewSilentExit(staleQuietExitCode(info))
	}

	output := staleOutput(info)

	if jsonOutput {
		return outputStaleJSON(output)
	}

	return outputStaleText(output)
}

func staleQuietExitCode(info *version.StaleBinaryInfo) int {
	if info.Skipped {
		return 2
	}
	if info.IsStale {
		return 0
	}
	return 1
}

func outputStaleJSON(output StaleOutput) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

func outputStaleText(output StaleOutput) error {
	if output.Skipped {
		fmt.Printf("%s Binary staleness check skipped\n", style.Dim.Render("•"))
		fmt.Printf("  %s\n", output.SkipReason)
		fmt.Printf("  Binary: %s\n", version.ShortCommit(output.BinaryCommit))
		return nil
	}
	if output.Stale {
		fmt.Printf("%s Binary is stale\n", style.Warning.Render("⚠"))
		fmt.Printf("  Binary:   %s\n", version.ShortCommit(output.BinaryCommit))
		fmt.Printf("  Build ref (%s): %s\n", output.CompareRef, version.ShortCommit(output.RepoCommit))
		if output.CommitsBehind > 0 {
			fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("(%d commits behind %s)", output.CommitsBehind, output.CompareRef)))
		}
		if !output.Forward {
			fmt.Printf("  %s %s is NOT a descendant of binary commit (diverged or older)\n", style.Error.Render("✗"), output.CompareRef)
		}
		if !output.OnMainBranch {
			fmt.Printf("  %s source worktree is not on a build branch (compared against %s)\n", style.Warning.Render("⚠"), output.CompareRef)
		}
		if output.SafeToRebuild {
			fmt.Printf("\n  Safe to rebuild: run 'make build && make install'\n")
		} else {
			fmt.Printf("\n  %s NOT safe for automated rebuild (forward=%v, build_branch=%v)\n",
				style.Error.Render("✗"), output.Forward, output.OnMainBranch)
		}
	} else {
		fmt.Printf("%s Binary is fresh\n", style.Success.Render("✓"))
		fmt.Printf("  Commit: %s\n", version.ShortCommit(output.BinaryCommit))
		if output.CompareRef != "" {
			fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("(compared against %s)", output.CompareRef)))
		}
	}
	return nil
}
