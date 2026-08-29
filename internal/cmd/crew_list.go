package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jonbaldie/gastown/internal/crew"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/spf13/cobra"
)

// CrewListItem represents a crew worker in list output.
type CrewListItem struct {
	Name       string `json:"name"`
	Rig        string `json:"rig"`
	Branch     string `json:"branch"`
	Path       string `json:"path"`
	HasSession bool   `json:"has_session"`
	GitClean   bool   `json:"git_clean"`
}

func runCrewList(_ *cobra.Command, args []string) error {
	state := crewState()
	if err := applyCrewListArgs(args); err != nil {
		return err
	}
	rigs, err := crewListRigs()
	if err != nil {
		return err
	}
	items := collectCrewListItems(rigs)

	if len(items) == 0 {
		fmt.Println("No crew workspaces found.")
		return nil
	}

	if state.json {
		return printCrewListJSON(items)
	}

	printCrewListText(items)
	return nil
}

func applyCrewListArgs(args []string) error {
	state := crewState()
	// Accept positional rig argument: gt crew list <rig>
	if len(args) > 0 {
		if state.rig != "" {
			return fmt.Errorf("cannot specify both positional rig argument and --rig flag")
		}
		state.rig = args[0]
	}

	if state.listAll && state.rig != "" {
		return fmt.Errorf("cannot use --all with a rig filter (--rig flag or positional argument)")
	}
	return nil
}

func crewListRigs() ([]*rig.Rig, error) {
	state := crewState()
	if state.listAll {
		return getAllRigs()
	}
	_, r, err := getCrewManager(state.rig)
	if err != nil {
		return nil, err
	}
	return []*rig.Rig{r}, nil
}

func collectCrewListItems(rigs []*rig.Rig) []CrewListItem {
	// Check session and git status for each worker
	t := tmux.NewTmux()
	var items []CrewListItem
	for _, r := range rigs {
		items = append(items, collectRigCrewListItems(t, r)...)
	}
	return items
}

func collectRigCrewListItems(t *tmux.Tmux, r *rig.Rig) []CrewListItem {
	crewGit := git.NewGit(r.Path)
	crewMgr := crew.NewManager(r, crewGit)
	workers, err := crewMgr.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to list crew workers in %s: %v\n", r.Name, err)
		return nil
	}

	items := make([]CrewListItem, 0, len(workers))
	for _, w := range workers {
		items = append(items, crewListItem(t, r, w))
	}
	return items
}

func crewListItem(t *tmux.Tmux, r *rig.Rig, w *crew.CrewWorker) CrewListItem {
	sessionID := crewSessionName(r.Name, w.Name)
	hasSession, _ := t.HasSession(sessionID)

	workerGit := git.NewGit(w.ClonePath)
	gitClean := true
	if status, err := workerGit.Status(); err == nil {
		gitClean = status.Clean
	}

	return CrewListItem{
		Name:       w.Name,
		Rig:        r.Name,
		Branch:     w.Branch,
		Path:       w.ClonePath,
		HasSession: hasSession,
		GitClean:   gitClean,
	}
}

func printCrewListJSON(items []CrewListItem) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(items)
}

func printCrewListText(items []CrewListItem) {
	// Text output
	fmt.Printf("%s\n\n", style.Bold.Render("Crew Workspaces"))
	for _, item := range items {
		status := style.Dim.Render("○")
		if item.HasSession {
			status = style.Bold.Render("●")
		}

		gitStatus := style.Dim.Render("clean")
		if !item.GitClean {
			gitStatus = style.Bold.Render("dirty")
		}

		fmt.Printf("  %s %s/%s\n", status, item.Rig, item.Name)
		fmt.Printf("    Branch: %s  Git: %s\n", item.Branch, gitStatus)
		fmt.Printf("    %s\n", style.Dim.Render(item.Path))
	}
}
