package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/jonbaldie/gastown/internal/from"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

var fromDryRun bool
var fromApplyMu sync.Mutex

var fromCmd = &cobra.Command{
	Use:     "from <parent> [town]",
	GroupID: GroupWorkspace,
	Short:   "Create a Town from a parent folder of Git repositories",
	Long: `Create a Gas Town from a parent folder of local Git repositories.

The first path is the parent of project repositories. It is not Town HQ.
gt install's path argument is Town HQ. gt from never treats the parent
folder as HQ.

Each immediate child Git repository becomes one Rig. The original folders
are left unchanged so Compose can keep running from the parent folder.

The Town is created next to the parent folder by default, as <parent>.gt.
Pass an optional Town path to reuse or create HQ somewhere else, such as ~/gt.

Examples:
  gt from ~/code                 # Town at ~/code.gt, one Rig per child repo
  gt from ~/code ~/gt            # Reuse or create HQ at ~/gt
  gt from ~/code --dry-run       # Print the plan without writing

The command does not start the Mayor, create Crew, create a Convoy,
run gt up, or enable Gas Town on the machine.`,
	Args:         cobra.RangeArgs(1, 2),
	RunE:         runFrom,
	SilenceUsage: true,
}

func init() {
	fromCmd.Flags().BoolVar(&fromDryRun, "dry-run", false, "Print the planned Town and Rigs without writing")
	rootCmd.AddCommand(fromCmd)
}

type fromRequest struct {
	Parent string
	Town   string
	DryRun bool
	Stdout io.Writer
}

func runFrom(cmd *cobra.Command, args []string) error {
	req := fromRequest{
		Parent: args[0],
		DryRun: fromDryRun,
		Stdout: cmd.OutOrStdout(),
	}
	if len(args) > 1 {
		req.Town = args[1]
	}
	return runFromRequest(req)
}

func runFromRequest(req fromRequest) error {
	if req.Stdout == nil {
		req.Stdout = os.Stdout
	}
	plan, err := from.Prepare(req.Parent, req.Town)
	if err != nil {
		return err
	}
	if req.DryRun {
		printFromPlan(req.Stdout, plan)
		return nil
	}
	return applyFromPlan(req.Stdout, plan)
}

func printFromPlan(w io.Writer, plan *from.Plan) {
	townNote := "create"
	if plan.TownExists {
		townNote = "reuse"
	}
	fmt.Fprintf(w, "Dry run — no files will be written.\n\n")
	fmt.Fprintf(w, "Town:   %s  (%s)\n", plan.TownAbs, townNote)
	fmt.Fprintf(w, "Parent: %s\n\n", plan.ParentAbs)
	fmt.Fprintf(w, "Rigs:\n")
	for _, r := range plan.Rigs {
		action := "add"
		if r.Action == from.ActionSkip {
			action = "skip"
		}
		fmt.Fprintf(w, "  %-16s %-40s %s  [%s]\n", r.Name, r.GitURL, r.SourcePath, action)
	}
	if len(plan.Glue) > 0 {
		fmt.Fprintf(w, "\nIgnored glue:\n")
		for _, glue := range plan.Glue {
			fmt.Fprintf(w, "  %s\n", glue)
		}
	}
	if len(plan.Skipped) > 0 {
		fmt.Fprintf(w, "\nSkipped:\n")
		for _, skipped := range plan.Skipped {
			fmt.Fprintf(w, "  %s\n", skipped)
		}
	}
	fmt.Fprintf(w, "\n%s\n", composeReminder())
}

func composeReminder() string {
	return "Compose sibling paths do not exist inside a Rig clone. Keep Compose in the parent folder."
}

func applyFromPlan(w io.Writer, plan *from.Plan) error {
	fromApplyMu.Lock()
	defer fromApplyMu.Unlock()

	created := !plan.TownExists
	if !plan.TownExists {
		if err := installTownForFrom(plan.TownAbs); err != nil {
			return err
		}
	}

	var added, skipped, failures []string
	for _, r := range plan.Rigs {
		if r.Action == from.ActionSkip {
			skipped = append(skipped, r.Name)
			continue
		}
		if err := addRigForFrom(plan.TownAbs, r); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		added = append(added, r.Name)
	}

	printFromReport(w, plan, created, added, skipped, failures)
	if len(failures) > 0 {
		return fmt.Errorf("failed to add %d Rig(s)", len(failures))
	}
	return nil
}

func installTownForFrom(townPath string) error {
	origGit := installGit
	origShell := installShell
	origForce := installForce
	origWrappers := installWrappers
	origSupervisor := installSupervisor
	installGit = true
	installShell = false
	installForce = false
	installWrappers = false
	installSupervisor = false
	defer func() {
		installGit = origGit
		installShell = origShell
		installForce = origForce
		installWrappers = origWrappers
		installSupervisor = origSupervisor
	}()
	return runInstall(installCmd, []string{townPath})
}

func addRigForFrom(townRoot string, r from.Rig) error {
	origLocal := rigAddLocalRepo
	origBranch := rigAddBranch
	origPrefix := rigAddPrefix
	origAdopt := rigAddAdopt
	origPush := rigAddPushURL
	origUpstream := rigAddUpstreamURL
	origFilter := rigAddFilter
	origSparse := rigAddSparseCheckout
	rigAddLocalRepo = r.LocalRepo
	rigAddBranch = r.Branch
	rigAddPrefix = r.Prefix
	rigAddAdopt = false
	rigAddPushURL = ""
	rigAddUpstreamURL = ""
	rigAddFilter = ""
	rigAddSparseCheckout = nil
	defer func() {
		rigAddLocalRepo = origLocal
		rigAddBranch = origBranch
		rigAddPrefix = origPrefix
		rigAddAdopt = origAdopt
		rigAddPushURL = origPush
		rigAddUpstreamURL = origUpstream
		rigAddFilter = origFilter
		rigAddSparseCheckout = origSparse
	}()

	origTown := os.Getenv("GT_TOWN_ROOT")
	if err := os.Setenv("GT_TOWN_ROOT", townRoot); err != nil {
		return err
	}
	defer func() { _ = os.Setenv("GT_TOWN_ROOT", origTown) }()

	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	if err := os.Chdir(townRoot); err != nil {
		return fmt.Errorf("entering Town: %w", err)
	}
	defer func() { _ = os.Chdir(wd) }()

	return runRigAdd(rigAddCmd, []string{r.Name, r.GitURL})
}

func printFromReport(w io.Writer, plan *from.Plan, created bool, added, skipped, failures []string) {
	if created {
		fmt.Fprintf(w, "%s Town created: %s\n", style.Success.Render("✓"), plan.TownAbs)
	} else {
		fmt.Fprintf(w, "%s Town reused: %s\n", style.Success.Render("✓"), plan.TownAbs)
	}
	fmt.Fprintf(w, "Added %d Rig(s)", len(added))
	if len(added) > 0 {
		fmt.Fprintf(w, ": %s", strings.Join(added, ", "))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Skipped %d Rig(s)", len(skipped))
	if len(skipped) > 0 {
		fmt.Fprintf(w, ": %s", strings.Join(skipped, ", "))
	}
	fmt.Fprintln(w)
	if len(plan.Glue) > 0 {
		fmt.Fprintf(w, "Ignored glue: %s\n", strings.Join(plan.Glue, ", "))
	}
	if len(plan.Skipped) > 0 {
		fmt.Fprintf(w, "Skipped sources: %s\n", strings.Join(plan.Skipped, ", "))
	}
	if len(failures) > 0 {
		fmt.Fprintf(w, "\n%s Failures:\n", style.Warning.Render("⚠"))
		for _, failure := range failures {
			fmt.Fprintf(w, "  %s\n", failure)
		}
	}
	fmt.Fprintf(w, "\n%s\n", composeReminder())
}
