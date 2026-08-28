package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jonbaldie/gastown/internal/crew"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/spf13/cobra"
)

func runCrewRename(_ *cobra.Command, args []string) error {
	oldName, newName := crewRenameNames(args)

	crewMgr, r, err := getCrewManagerForMember(crewRig, oldName)
	if err != nil {
		return err
	}

	if err := killCrewRenameSession(r.Name, oldName); err != nil {
		return err
	}

	if err := crewMgr.Rename(oldName, newName); err != nil {
		return crewRenameError(err, oldName, newName)
	}

	printCrewRename(r.Name, oldName, newName)

	return nil
}

func crewRenameNames(args []string) (string, string) {
	oldName := args[0]
	newName := args[1]
	// Parse rig/name format for oldName (e.g., "beads/emma" -> rig=beads, name=emma).
	if rig, crewName, ok := parseRigSlashName(oldName); ok {
		if crewRig == "" {
			crewRig = rig
		}
		oldName = crewName
	}
	// Note: newName is just the new name, no rig prefix expected.
	return oldName, newName
}

func killCrewRenameSession(rigName, crewName string) error {
	// Kill any running session for the old name. Use KillSessionWithProcesses to
	// ensure all descendant processes are killed.
	t := tmux.NewTmux()
	sessionID := crewSessionName(rigName, crewName)
	hasSession, _ := t.HasSession(sessionID)
	if !hasSession {
		return nil
	}
	if err := t.KillSessionWithProcesses(sessionID); err != nil {
		return fmt.Errorf("killing old session: %w", err)
	}
	fmt.Printf("Killed session %s\n", sessionID)
	return nil
}

func crewRenameError(err error, oldName, newName string) error {
	if errors.Is(err, crew.ErrInvalidCrewName) {
		return fmt.Errorf("invalid new name '%s': %w", newName, err)
	}
	if err == crew.ErrCrewNotFound {
		return fmt.Errorf("crew workspace '%s' not found", oldName)
	}
	if err == crew.ErrCrewExists {
		return fmt.Errorf("crew workspace '%s' already exists", newName)
	}
	return fmt.Errorf("renaming crew workspace: %w", err)
}

func printCrewRename(rigName, oldName, newName string) {
	fmt.Printf("%s Renamed crew workspace: %s/%s → %s/%s\n",
		style.Bold.Render("✓"), rigName, oldName, rigName, newName)
	fmt.Printf("New session will be: %s\n", style.Dim.Render(crewSessionName(rigName, newName)))
}

func runCrewPristine(_ *cobra.Command, args []string) error {
	crewMgr, r, err := getCrewManager(crewRig)
	if err != nil {
		return err
	}

	workers, err := pristineWorkers(crewMgr, args)
	if err != nil {
		return err
	}

	if len(workers) == 0 {
		fmt.Println("No crew workspaces found.")
		return nil
	}

	results, err := pristineCrewWorkers(crewMgr, workers)
	if err != nil {
		return err
	}

	if crewJSON {
		return printPristineJSON(results)
	}

	printPristineText(r.Name, results)
	return nil
}

func pristineWorkers(crewMgr *crew.Manager, args []string) ([]*crew.CrewWorker, error) {
	if len(args) == 0 {
		workers, err := crewMgr.List()
		if err != nil {
			return nil, fmt.Errorf("listing crew workers: %w", err)
		}
		return workers, nil
	}

	name := args[0]
	// Parse rig/name format (e.g., "beads/emma" -> rig=beads, name=emma).
	if _, crewName, ok := parseRigSlashName(name); ok {
		name = crewName
	}
	worker, err := crewMgr.Get(name)
	if err != nil {
		if err == crew.ErrCrewNotFound {
			return nil, fmt.Errorf("crew workspace '%s' not found", name)
		}
		return nil, fmt.Errorf("getting crew worker: %w", err)
	}
	return []*crew.CrewWorker{worker}, nil
}

func pristineCrewWorkers(crewMgr *crew.Manager, workers []*crew.CrewWorker) ([]*crew.PristineResult, error) {
	results := make([]*crew.PristineResult, 0, len(workers))
	for _, worker := range workers {
		result, err := crewMgr.Pristine(worker.Name)
		if err != nil {
			return nil, fmt.Errorf("pristine %s: %w", worker.Name, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func printPristineJSON(results []*crew.PristineResult) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

func printPristineText(rigName string, results []*crew.PristineResult) {
	for _, result := range results {
		fmt.Printf("%s %s/%s\n", style.Bold.Render("→"), rigName, result.Name)

		if result.HadChanges {
			fmt.Printf("  %s\n", style.Bold.Render("⚠ Has uncommitted changes"))
		}

		if result.Pulled {
			fmt.Printf("  %s git pull\n", style.Dim.Render("✓"))
		} else if result.PullError != "" {
			fmt.Printf("  %s git pull: %s\n", style.Bold.Render("✗"), result.PullError)
		}
	}
}
