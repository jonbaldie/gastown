package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var tapPolecatStopCmd = &cobra.Command{
	Use:   "polecat-stop-check",
	Short: "Auto-run gt done on session Stop if polecat has pending work",
	Long: `Safety net for the "idle polecat" problem: polecats that finish work
but forget to call gt done before the session ends.

This command is designed to run from a Claude Code Stop hook. It checks:
1. Whether this is a polecat session (GT_POLECAT env var)
2. Whether gt done has already run (heartbeat state is "exiting" or "idle")
3. Whether the polecat has commits, stashes, or non-runtime dirty work

If the polecat has pending work that wasn't submitted, this command
runs gt done to submit it. If gt done already ran or there's nothing
to submit, it exits silently.

Exit codes:
  0 - No action needed (not a polecat, already done, or gt done succeeded)
  1 - gt done was attempted but failed`,
	RunE:         runTapPolecatStop,
	SilenceUsage: true,
}

func init() {
	tapCmd.AddCommand(tapPolecatStopCmd)
}

func runTapPolecatStop(_ *cobra.Command, _ []string) error {
	polecatName, sessionName, townRoot, ok := tapPolecatContext()
	if !ok || tapPolecatAlreadyDone(townRoot, sessionName) {
		return nil
	}

	rigName := os.Getenv("GT_RIG")
	if rigName == "" {
		return nil
	}

	cloneDir, ok := tapPolecatCloneDir(townRoot, rigName, polecatName)
	if !ok {
		return nil
	}

	branch, ok := tapPolecatBranch(cloneDir)
	if !ok || isDefaultBranch(branch) {
		return nil
	}

	pending, reason, err := polecatStopPendingWork(cloneDir, branch)
	if err != nil || !pending {
		return nil
	}

	runTapPolecatDone(polecatName, branch, reason, cloneDir, os.Stderr)
	return nil
}

func tapPolecatContext() (polecatName, sessionName, townRoot string, ok bool) {
	polecatName = os.Getenv("GT_POLECAT")
	sessionName = os.Getenv("GT_SESSION")
	if polecatName == "" || sessionName == "" {
		return "", "", "", false
	}

	townRoot, _, _ = workspace.FindFromCwdWithFallback()
	if townRoot == "" {
		townRoot = os.Getenv("GT_TOWN_ROOT")
	}
	return polecatName, sessionName, townRoot, townRoot != ""
}

func tapPolecatAlreadyDone(townRoot, sessionName string) bool {
	hb := polecat.ReadSessionHeartbeat(townRoot, sessionName)
	if hb == nil {
		return false
	}
	state := hb.EffectiveState()
	return state == polecat.HeartbeatExiting || state == polecat.HeartbeatIdle
}

func tapPolecatCloneDir(townRoot, rigName, polecatName string) (string, bool) {
	polecatDir := filepath.Join(townRoot, rigName, "polecats", polecatName)
	cloneDir := filepath.Join(polecatDir, rigName)
	if hasGitDirectory(cloneDir) {
		return cloneDir, true
	}
	if hasGitDirectory(polecatDir) {
		return polecatDir, true
	}
	return "", false
}

func hasGitDirectory(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

func tapPolecatBranch(cloneDir string) (string, bool) {
	branchOut, err := exec.Command("git", "-C", cloneDir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(branchOut)), true
}

func isDefaultBranch(branch string) bool {
	if branch == "main" || branch == "master" || branch == "HEAD" {
		return true
	}
	return false
}

func runTapPolecatDone(polecatName, branch, reason, cloneDir string, stderr *os.File) {
	fmt.Fprintf(stderr, "\n")
	fmt.Fprintf(stderr, "⚠️  Polecat %s has pending work on branch %s (%s)\n", polecatName, branch, reason)
	fmt.Fprintf(stderr, "   Auto-running gt done as safety net...\n")
	fmt.Fprintf(stderr, "\n")

	gtBin, err := os.Executable()
	if err != nil {
		gtBin = "gt"
	}

	doneCmd := exec.Command(gtBin, "done")
	doneCmd.Dir = cloneDir
	doneCmd.Stdout = os.Stdout
	doneCmd.Stderr = stderr
	// Inherit environment (GT_POLECAT, GT_RIG, etc. are already set)
	doneCmd.Env = os.Environ()

	if err := doneCmd.Run(); err != nil {
		fmt.Fprintf(stderr, "⚠️  Auto gt done failed: %v\n", err)
		fmt.Fprintf(stderr, "   Witness will handle cleanup.\n")
		// Don't return error — don't block session stop
	}
}

func polecatStopPendingWork(cloneDir, branch string) (bool, string, error) {
	g := git.NewGit(cloneDir)
	workStatus, err := git.CheckUncommittedWork(g)
	if err != nil {
		return false, "", err
	}

	if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingRuntime() {
		return true, fmt.Sprintf("%d non-runtime dirty file(s)", len(workStatus.NonRuntimePaths())), nil
	}
	if workStatus.StashCount > 0 {
		return true, fmt.Sprintf("%d branch stash(es)", workStatus.StashCount), nil
	}

	targetStatus, err := git.BranchTargetStatus(g, branch, "origin", nil)
	if err != nil {
		return false, "", err
	}
	if !targetStatus.Preserved && targetStatus.UnpreservedPatchCount > 0 {
		return true, fmt.Sprintf("%d unsubmitted commit(s)", targetStatus.UnpreservedPatchCount), nil
	}

	return false, "", nil
}
