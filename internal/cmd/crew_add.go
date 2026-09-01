package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/crew"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// agentBeadUpserter captures the subset of bead operations needed by crew add.
// Using a narrow interface allows deterministic unit tests of crew bead creation
// behavior without requiring a live bd backend.
type agentBeadUpserter interface {
	CreateOrReopenAgentBead(_, _ string, _ *beads.AgentFields) (*beads.Issue, error)
}

// upsertCrewAgentBead ensures the crew agent bead exists with expected metadata.
// It uses CreateOrReopenAgentBead instead of a Show()+Create sequence so existing
// beads in alternate stores (issues/wisps) do not trigger false "issue not found"
// warnings during crew creation.
func upsertCrewAgentBead(bd agentBeadUpserter, townRoot, rigName, crewName string) (string, error) {
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	crewID := beads.CrewBeadIDWithPrefix(prefix, rigName, crewName)
	fields := &beads.AgentFields{
		RoleType:   "crew",
		Rig:        rigName,
		AgentState: "idle",
	}
	desc := fmt.Sprintf("Crew worker %s in %s - human-managed persistent workspace.", crewName, rigName)
	if _, err := bd.CreateOrReopenAgentBead(crewID, desc, fields); err != nil {
		return "", err
	}
	return crewID, nil
}

type crewAddSetup struct {
	townRoot     string
	baseRig      string
	crewMgr      *crew.Manager
	bd           *beads.Beads
	createBranch bool
}

type crewAddResult struct {
	created    []string
	failed     []string
	lastWorker *crew.CrewWorker
}

func runCrewAdd(_ *cobra.Command, args []string) error {
	args = dedupeCrewAddArgs(args)
	setup, err := prepareCrewAdd(args)
	if err != nil {
		return err
	}

	result := addCrewWorkspaces(setup, args)
	printCrewAddSummary(result)
	if len(result.created) == 0 && len(result.failed) > 0 {
		return fmt.Errorf("failed to create any crew workspaces")
	}

	return nil
}

func dedupeCrewAddArgs(args []string) []string {
	// Deduplicate args to handle cases like "gt crew add foo --branch foo"
	// where "foo" appears twice because --branch is a boolean flag.
	// This prevents confusing "already exists" errors after a successful create.
	seen := make(map[string]bool)
	var deduped []string
	for _, arg := range args {
		if seen[arg] {
			continue
		}
		seen[arg] = true
		deduped = append(deduped, arg)
	}
	return deduped
}

func prepareCrewAdd(args []string) (*crewAddSetup, error) {
	// Find workspace first (needed for all names).
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	baseRig, err := resolveCrewAddRig(townRoot, args)
	if err != nil {
		return nil, err
	}

	rigMgr := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot))
	r, err := rigMgr.GetRig(baseRig)
	if err != nil {
		return nil, fmt.Errorf("rig '%s' not found", baseRig)
	}

	return &crewAddSetup{
		townRoot:     townRoot,
		baseRig:      baseRig,
		crewMgr:      crew.NewManager(r, git.NewGit(r.Path)),
		bd:           beads.New(beads.ResolveBeadsDir(r.Path)),
		createBranch: crewState().branch,
	}, nil
}

func resolveCrewAddRig(townRoot string, args []string) (string, error) {
	baseRig := crewState().rig
	if baseRig == "" && len(args) > 0 {
		if parsedRig, _, ok := parseRigSlashName(args[0]); ok {
			baseRig = parsedRig
		}
	}
	if baseRig != "" {
		return baseRig, nil
	}

	baseRig, err := inferRigFromCwd(townRoot)
	if err != nil {
		return "", fmt.Errorf("could not determine rig (use --rig flag): %w", err)
	}
	return baseRig, nil
}

func addCrewWorkspaces(setup *crewAddSetup, args []string) crewAddResult {
	result := crewAddResult{}
	for _, arg := range args {
		addCrewWorkspace(setup, arg, &result)
	}
	return result
}

func addCrewWorkspace(setup *crewAddSetup, arg string, result *crewAddResult) {
	name := arg
	if parsedRig, crewName, ok := parseRigSlashName(arg); ok {
		if parsedRig != setup.baseRig {
			style.PrintWarning("%s: different rig '%s' ignored (use --rig to change)", arg, parsedRig)
		}
		name = crewName
	}

	fmt.Printf("Creating crew workspace %s in %s...\n", name, setup.baseRig)
	worker, err := setup.crewMgr.Add(name, setup.createBranch)
	if err != nil {
		recordCrewAddFailure(name, err, result)
		return
	}

	printCrewWorkspaceCreated(setup, name, worker)
	crewID, err := upsertCrewAgentBead(setup.bd, setup.townRoot, setup.baseRig, name)
	if err != nil {
		style.PrintWarning("could not create agent bead for %s: %v", name, err)
	} else {
		fmt.Printf("  Agent bead: %s\n", crewID)
	}

	result.created = append(result.created, name)
	result.lastWorker = worker
	fmt.Println()
}

func recordCrewAddFailure(name string, err error, result *crewAddResult) {
	if err == crew.ErrCrewExists {
		style.PrintWarning("crew workspace '%s' already exists, skipping", name)
		result.failed = append(result.failed, name+" (exists)")
		return
	}
	style.PrintWarning("creating crew workspace '%s': %v", name, err)
	result.failed = append(result.failed, name)
}

func printCrewWorkspaceCreated(setup *crewAddSetup, name string, worker *crew.CrewWorker) {
	fmt.Printf("%s Created crew workspace: %s/%s\n",
		style.Bold.Render("✓"), setup.baseRig, name)
	fmt.Printf("  Path: %s\n", worker.ClonePath)
	fmt.Printf("  Branch: %s\n", worker.Branch)
}

func printCrewAddSummary(result crewAddResult) {
	if len(result.created) > 0 {
		fmt.Printf("%s Created %d crew workspace(s): %v\n",
			style.Bold.Render("✓"), len(result.created), result.created)
		if result.lastWorker != nil && len(result.created) == 1 {
			fmt.Printf("\n%s\n", style.Dim.Render("Start working with: cd "+result.lastWorker.ClonePath))
		}
	}
	if len(result.failed) > 0 {
		fmt.Printf("%s Failed to create %d workspace(s): %v\n",
			style.Warning.Render("!"), len(result.failed), result.failed)
	}
}
