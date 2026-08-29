package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/refinery"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/witness"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// RigDockedLabel is the label set on rig identity beads when docked.
const RigDockedLabel = "status:docked"

var rigDockCmd = &cobra.Command{
	Use:   "dock <rig>",
	Short: "Dock a rig (global, persistent shutdown)",
	Long: `Dock a rig to persistently disable it across all clones.

Docking a rig:
  - Stops the witness if running
  - Stops the refinery if running
  - Stops all polecat sessions if running
  - Sets status:docked label on the rig identity bead
  - Syncs via git so all clones see the docked status

This is a Level 2 (global/persistent) operation:
  - Affects all clones of this rig (via git sync)
  - Persists until explicitly undocked
  - The daemon respects this status and won't auto-restart agents

Use 'gt rig undock' to resume normal operation.

Examples:
  gt rig dock gastown
  gt rig dock beads`,
	Args: cobra.ExactArgs(1),
	RunE: runRigDock,
}

var rigUndockCmd = &cobra.Command{
	Use:   "undock <rig>",
	Short: "Undock a rig (remove global docked status)",
	Long: `Undock a rig to remove the persistent docked status.

Undocking a rig:
  - Removes the status:docked label from the rig identity bead
  - Syncs via git so all clones see the undocked status
  - Allows the daemon to auto-restart agents
  - Does NOT automatically start agents (use 'gt rig start' for that)

Examples:
  gt rig undock gastown
  gt rig undock beads`,
	Args: cobra.ExactArgs(1),
	RunE: runRigUndock,
}

func init() {
	rigCmd.AddCommand(rigDockCmd)
	rigCmd.AddCommand(rigUndockCmd)
}

func runRigDock(_ *cobra.Command, args []string) error {
	rigName := args[0]
	if err := requireMainBranchForDock("dock", "Docking"); err != nil {
		return err
	}
	r, bd, rigBead, err := loadRigDockBead(rigName)
	if err != nil {
		return err
	}
	if rigHasDockedLabel(rigBead) {
		fmt.Printf("%s Rig %s is already docked\n", style.Dim.Render("•"), rigName)
		return nil
	}
	fmt.Printf("Docking rig %s...\n", style.Bold.Render(rigName))
	stoppedAgents := stopDockedRigAgents(r, rigName)
	if err := bd.Update(rigBead.ID, beads.UpdateOptions{AddLabels: []string{RigDockedLabel}}); err != nil {
		return fmt.Errorf("setting docked label: %w", err)
	}
	updateDaemonPatrolsOnDock(rigName, true)
	printRigDocked(rigName, stoppedAgents)
	return nil
}

func requireMainBranchForDock(action, gerund string) error {
	out, err := exec.Command("git", "branch", "--show-current").Output()
	if err != nil {
		return nil
	}
	currentBranch := strings.TrimSpace(string(out))
	if currentBranch == "main" || currentBranch == "master" {
		return nil
	}
	return fmt.Errorf("cannot %s: must be on main branch (currently on %s)\n"+
		"%s on other branches won't persist. Run: git checkout main", action, currentBranch, gerund)
}

func loadRigDockBead(rigName string) (*rig.Rig, *beads.Beads, *beads.Issue, error) {
	_, r, err := getRig(rigName)
	if err != nil {
		return nil, nil, nil, err
	}
	prefix := "gt"
	if r.Config != nil && r.Config.Prefix != "" {
		prefix = r.Config.Prefix
	}
	bd := beads.New(r.BeadsPath())
	rigBead, err := bd.EnsureRigBead(rigName, &beads.RigFields{
		Repo:   r.GitURL,
		Prefix: prefix,
		State:  beads.RigStateActive,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ensuring rig identity bead: %w", err)
	}
	return r, bd, rigBead, nil
}

func rigHasDockedLabel(rigBead *beads.Issue) bool {
	for _, label := range rigBead.Labels {
		if label == RigDockedLabel {
			return true
		}
	}
	return false
}

func stopDockedRigAgents(r *rig.Rig, rigName string) []string {
	t := tmux.NewTmux()
	var stopped []string
	stopped = append(stopped, stopDockedWitness(r, t, rigName)...)
	stopped = append(stopped, stopDockedRefinery(r, t, rigName)...)
	stopped = append(stopped, stopDockedPolecats(r, t)...)
	return stopped
}

func stopDockedWitness(r *rig.Rig, t *tmux.Tmux, rigName string) []string {
	running, _ := t.HasSession(session.WitnessSessionName(session.PrefixFor(rigName)))
	if !running {
		return nil
	}
	fmt.Printf("  Stopping witness...\n")
	if err := witness.NewManager(r).Stop(); err != nil {
		fmt.Printf("  %s Failed to stop witness: %v\n", style.Warning.Render("!"), err)
		return nil
	}
	return []string{"Witness stopped"}
}

func stopDockedRefinery(r *rig.Rig, t *tmux.Tmux, rigName string) []string {
	running, _ := t.HasSession(session.RefinerySessionName(session.PrefixFor(rigName)))
	if !running {
		return nil
	}
	fmt.Printf("  Stopping refinery...\n")
	if err := refinery.NewManager(r).Stop(); err != nil {
		fmt.Printf("  %s Failed to stop refinery: %v\n", style.Warning.Render("!"), err)
		return nil
	}
	return []string{"Refinery stopped"}
}

func stopDockedPolecats(r *rig.Rig, t *tmux.Tmux) []string {
	polecatMgr := polecat.NewSessionManager(t, r)
	polecatInfos, err := polecatMgr.List()
	if err != nil || len(polecatInfos) == 0 {
		return nil
	}
	fmt.Printf("  Stopping %d polecat session(s)...\n", len(polecatInfos))
	if err := polecatMgr.StopAll(false); err != nil {
		fmt.Printf("  %s Failed to stop polecat sessions: %v\n", style.Warning.Render("!"), err)
		return nil
	}
	return []string{fmt.Sprintf("%d polecat session(s) stopped", len(polecatInfos))}
}

func updateDaemonPatrolsOnDock(rigName string, docking bool) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return
	}
	var updateErr error
	if docking {
		updateErr = config.RemoveRigFromDaemonPatrols(townRoot, rigName)
	} else {
		updateErr = config.AddRigToDaemonPatrols(townRoot, rigName)
	}
	if updateErr != nil {
		fmt.Printf("  %s Could not update daemon.json patrols: %v\n", style.Warning.Render("!"), updateErr)
	}
}

func printRigDocked(rigName string, stoppedAgents []string) {
	fmt.Printf("%s Rig %s docked (global)\n", style.Success.Render("✓"), rigName)
	fmt.Printf("  Label added: %s\n", RigDockedLabel)
	for _, msg := range stoppedAgents {
		fmt.Printf("  %s\n", msg)
	}
	fmt.Printf("  Beads changes persisted via Dolt\n")
}

func runRigUndock(_ *cobra.Command, args []string) error {
	rigName := args[0]
	if err := requireMainBranchForDock("undock", "Undocking"); err != nil {
		return err
	}
	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}
	bd, rigBead, ok := loadUndockRigBead(r, rigName)
	if !ok {
		return nil
	}
	if !rigHasDockedLabel(rigBead) {
		fmt.Printf("%s Rig %s is not docked\n", style.Dim.Render("•"), rigName)
		return nil
	}
	if err := bd.Update(rigBead.ID, beads.UpdateOptions{RemoveLabels: []string{RigDockedLabel}}); err != nil {
		return fmt.Errorf("removing docked label: %w", err)
	}
	updateDaemonPatrolsOnDock(rigName, false)
	printRigUndocked(rigName)
	return nil
}

func loadUndockRigBead(r *rig.Rig, rigName string) (*beads.Beads, *beads.Issue, bool) {
	prefix := "gt"
	if r.Config != nil && r.Config.Prefix != "" {
		prefix = r.Config.Prefix
	}
	bd := beads.New(r.BeadsPath())
	rigBead, err := bd.Show(beads.RigBeadIDWithPrefix(prefix, rigName))
	if err != nil {
		fmt.Printf("%s Rig %s has no identity bead and is not docked\n", style.Dim.Render("•"), rigName)
		return nil, nil, false
	}
	return bd, rigBead, true
}

func printRigUndocked(rigName string) {
	fmt.Printf("%s Rig %s undocked\n", style.Success.Render("✓"), rigName)
	fmt.Printf("  Label removed: %s\n", RigDockedLabel)
	fmt.Printf("  Daemon can now auto-restart agents\n")
	fmt.Printf("  Use '%s' to start agents immediately\n", style.Dim.Render("gt rig start "+rigName))
}

// IsRigDocked checks if a rig is docked by checking for the status:docked label
// on the rig identity bead. This function is exported for use by the daemon.
func IsRigDocked(townRoot, rigName, prefix string) bool {
	// Construct the rig beads path
	rigPath := filepath.Join(townRoot, rigName)
	beadsPath := filepath.Join(rigPath, "mayor", "rig")
	if _, err := os.Stat(beadsPath); err != nil {
		beadsPath = rigPath
	}

	bd := beads.New(beadsPath)
	rigBeadID := beads.RigBeadIDWithPrefix(prefix, rigName)

	rigBead, err := bd.Show(rigBeadID)
	if err != nil {
		return false
	}

	for _, label := range rigBead.Labels {
		if label == RigDockedLabel {
			return true
		}
	}
	return false
}
