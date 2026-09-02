package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var assignCmd = &cobra.Command{
	Use:     "assign <crew-member> <title>",
	GroupID: GroupWork,
	Short:   "Create a bead and hook it to a crew member",
	Long: `Create a new bead and immediately hook it to a crew member.

This is a shortcut for "bd create" + "gt hook". The crew member name
is short-form (just the name), and the rig is resolved in order:
--rig flag, current directory, or by scanning all rigs for the crew
member name. This means "gt assign dave ..." works from anywhere in
the town if dave exists in exactly one rig.

The crew member must exist (the directory <rig>/crew/<name> must be
present) or the command will error.

Examples:
  gt assign monet "Fix the auth token refresh bug"
  gt assign monet "Review error handling" -d "The retry logic looks wrong"
  gt assign monet "Fix auth bug" --type bug --priority 1
  gt assign monet "Fix auth bug" --nudge
  gt assign monet "Fix auth bug" --label important
  gt assign monet "Fix auth bug" --rig beads   # Explicit rig override`,
	Args: cobra.MinimumNArgs(2),
	RunE: runAssign,
}

func init() {
	assignCmd.Flags().StringP("description", "d", "", "Bead description")
	assignCmd.Flags().StringP("type", "t", "task", "Bead type")
	assignCmd.Flags().StringP("priority", "p", "2", "Priority 0-4")
	assignCmd.Flags().StringArrayP("label", "l", nil, "Labels (repeatable)")
	assignCmd.Flags().Bool("nudge", false, "Wake the agent after hooking")
	assignCmd.Flags().String("rig", "", "Override rig inference")
	assignCmd.Flags().BoolP("dry-run", "n", false, "Show what would happen")
	assignCmd.Flags().Bool("force", false, "Replace existing hooked work")

	rootCmd.AddCommand(assignCmd)
}

type assignOptions struct {
	description string
	typeName    string
	priority    string
	labels      []string
	nudge       bool
	rig         string
	dryRun      bool
}

func readAssignOptions(cmd *cobra.Command) assignOptions {
	if cmd == nil {
		return assignOptions{typeName: "task", priority: "2"}
	}
	return assignOptions{
		description: stringAssignFlag(cmd, "description", ""),
		typeName:    stringAssignFlag(cmd, "type", "task"),
		priority:    stringAssignFlag(cmd, "priority", "2"),
		labels:      stringArrayAssignFlag(cmd, "label"),
		nudge:       boolAssignFlag(cmd, "nudge", false),
		rig:         stringAssignFlag(cmd, "rig", ""),
		dryRun:      boolAssignFlag(cmd, "dry-run", false),
	}
}

func stringAssignFlag(cmd *cobra.Command, name, fallback string) string {
	value, err := cmd.Flags().GetString(name)
	if err != nil {
		return fallback
	}
	return value
}

func stringArrayAssignFlag(cmd *cobra.Command, name string) []string {
	value, err := cmd.Flags().GetStringArray(name)
	if err != nil {
		return nil
	}
	return value
}

func boolAssignFlag(cmd *cobra.Command, name string, fallback bool) bool {
	value, err := cmd.Flags().GetBool(name)
	if err != nil {
		return fallback
	}
	return value
}

func runAssign(cmd *cobra.Command, args []string) error {
	opts := readAssignOptions(cmd)
	prepared, err := prepareAssign(opts, args)
	if err != nil {
		return err
	}

	if opts.dryRun {
		printAssignDryRun(opts, prepared.title, prepared.agentID)
		return nil
	}

	fmt.Printf("%s Creating bead for %s...\n", style.Bold.Render("📋"), prepared.agentID)
	beadID, err := createAssignedBead(opts, prepared.title, prepared.townRoot)
	if err != nil {
		return err
	}
	fmt.Printf("  Created: %s\n", beadID)

	fmt.Printf("%s Hooking %s to %s...\n", style.Bold.Render("🪝"), beadID, prepared.agentID)
	if err := hookAssignedBead(beadID, prepared.agentID, prepared.townRoot); err != nil {
		return err
	}

	// Step 3: Update agent hook_bead field so gt hook / gt mol status can find the work.
	townBeadsDir := filepath.Join(prepared.townRoot, ".beads")
	rigBeadsDir := filepath.Join(prepared.townRoot, prepared.rigName, ".beads")
	if err := updateAgentHookBead(prepared.agentID, beadID, rigBeadsDir, townBeadsDir); err != nil {
		fmt.Printf("%s Could not update agent hook: %v\n", style.Dim.Render("Warning:"), err)
	}

	// Step 4: Log event
	if err := events.LogFeed(events.TypeHook, prepared.agentID, events.HookPayload(beadID)); err != nil {
		fmt.Fprintf(os.Stderr, "%s Warning: failed to log event: %v\n", style.Dim.Render("⚠"), err)
	}

	fmt.Printf("%s Assigned %s to %s — %q\n", style.Bold.Render("✓"), beadID, prepared.agentID, prepared.title)

	return notifyAssignedAgent(opts.nudge, prepared.agentID, prepared.title)
}

type prepareAssignResult struct {
	townRoot string
	rigName  string
	agentID  string
	title    string
}

func prepareAssign(opts assignOptions, args []string) (prepareAssignResult, error) {
	crewName := args[0]
	title := strings.Join(args[1:], " ")
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return prepareAssignResult{}, fmt.Errorf("finding town root: %w", err)
	}
	rigName, err := resolveAssignRig(opts.rig, townRoot, crewName)
	if err != nil {
		return prepareAssignResult{}, err
	}
	crewDir := filepath.Join(townRoot, rigName, "crew", crewName)
	if _, statErr := os.Stat(crewDir); os.IsNotExist(statErr) {
		return prepareAssignResult{}, fmt.Errorf("crew member %q not found in rig %q (no directory %s)", crewName, rigName, crewDir)
	}
	return prepareAssignResult{
		townRoot: townRoot,
		rigName:  rigName,
		agentID:  rigName + "/crew/" + crewName,
		title:    title,
	}, nil
}

func resolveAssignRig(rigName, townRoot, crewName string) (string, error) {
	if rigName != "" {
		return rigName, nil
	}
	rigName, err := inferRigFromCwd(townRoot)
	if err == nil {
		return rigName, nil
	}
	rigName, err = inferRigFromCrewName(townRoot, crewName)
	if err != nil {
		return "", fmt.Errorf("inferring rig (use --rig to specify): %w", err)
	}
	return rigName, nil
}

func printAssignDryRun(opts assignOptions, title, agentID string) {
	fmt.Printf("Would create bead: %q (type=%s, priority=%s)\n", title, opts.typeName, opts.priority)
	fmt.Printf("Would hook to: %s\n", agentID)
	if opts.description != "" {
		fmt.Printf("  description: %s\n", opts.description)
	}
	for _, label := range opts.labels {
		fmt.Printf("  label: %s\n", label)
	}
	if opts.nudge {
		fmt.Printf("Would nudge: %s\n", agentID)
	}
}

func createAssignedBead(opts assignOptions, title, townRoot string) (string, error) {
	createArgs := []string{"create", "--title=" + title, "--type=" + opts.typeName, "--priority=" + opts.priority, "--silent"}
	if opts.description != "" {
		createArgs = append(createArgs, "--description="+opts.description)
	}
	for _, label := range opts.labels {
		createArgs = append(createArgs, "--label="+label)
	}
	out, err := BdCmd(createArgs...).Dir(townRoot).WithAutoCommit().Output()
	if err != nil {
		return "", fmt.Errorf("creating bead: %w", err)
	}
	beadID := strings.TrimSpace(string(out))
	if beadID == "" {
		return "", fmt.Errorf("bd create returned empty ID")
	}
	return beadID, nil
}

func hookAssignedBead(beadID, agentID, townRoot string) error {
	const maxRetries = 5
	const baseBackoff = 500 * time.Millisecond
	const backoffMax = 10 * time.Second
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		err := BdCmd("update", beadID, "--status=hooked", "--assignee="+agentID).
			Dir(townRoot).
			WithAutoCommit().
			Run()
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == maxRetries {
			break
		}
		backoff := slingBackoff(attempt, baseBackoff, backoffMax)
		fmt.Printf("%s Hook attempt %d failed, retrying in %v...\n", style.Warning.Render("⚠"), attempt, backoff)
		time.Sleep(backoff)
	}
	return fmt.Errorf("hooking bead after %d attempts: %w", maxRetries, lastErr)
}

func notifyAssignedAgent(nudge bool, agentID, title string) error {
	if !nudge {
		fmt.Printf("  %s Agent won't be notified (use --nudge to wake them)\n", style.Dim.Render("ℹ"))
		return nil
	}
	nudgeMsg := fmt.Sprintf("New work on your hook: %s", title)
	nudgeCmd := exec.Command("gt", "nudge", agentID, "-m", nudgeMsg)
	nudgeCmd.Stderr = os.Stderr
	out, err := nudgeCmd.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s Warning: nudge failed: %v\n", style.Warning.Render("⚠"), err)
	} else if len(out) > 0 {
		fmt.Print(string(out))
	} else {
		fmt.Printf("  Nudged %s\n", agentID)
	}
	return nil
}
