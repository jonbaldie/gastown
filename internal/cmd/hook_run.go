package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/nudge"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type hookRun struct {
	beadID       string
	targetAgent  string
	subject      string
	message      string
	dryRun       bool
	force        bool
	agentID      string
	townBeadsDir string
	workDir      string
}

func runHook(cmd *cobra.Command, args []string) error {
	h, err := beginHookRun(cmd, args)
	if err != nil {
		return err
	}
	if err := resolveHookAgent(h); err != nil {
		return err
	}
	if err := resolveHookWorkDir(h); err != nil {
		return err
	}
	done, err := replaceExistingHook(h)
	if err != nil || done {
		return err
	}
	done, err = applyHookBead(h)
	if err != nil || done {
		return err
	}
	finishHookRun(h)
	return nil
}

func beginHookRun(cmd *cobra.Command, args []string) (*hookRun, error) {
	h := &hookRun{
		subject: commandStringFlag(cmd, "subject"),
		message: commandStringFlag(cmd, "message"),
		dryRun:  commandBoolFlag(cmd, "dry-run"),
		force:   commandBoolFlag(cmd, "force"),
		beadID:  args[0],
	}
	if err := ensureCurrentHookWorktreeIntegrity(); err != nil {
		return nil, err
	}
	if !isBeadID(h.beadID) {
		return nil, fmt.Errorf("%q is not a bead ID. See 'gt hook --help' for available subcommands and usage", h.beadID)
	}
	if len(args) > 1 {
		h.targetAgent = args[1]
	}
	if err := rejectPolecatHook(); err != nil {
		return nil, err
	}
	if err := verifyBeadExists(h.beadID); err != nil {
		return nil, err
	}
	return h, nil
}

func rejectPolecatHook() error {
	if role := os.Getenv("GT_ROLE"); role != "" {
		parsedRole, _, _ := parseRoleString(role)
		if parsedRole == RolePolecat {
			return fmt.Errorf("polecats cannot hook work (use gt done for handoff)")
		}
		return nil
	}
	if os.Getenv("GT_POLECAT") != "" {
		return fmt.Errorf("polecats cannot hook work (use gt done for handoff)")
	}
	return nil
}

func resolveHookAgent(h *hookRun) error {
	if h.targetAgent != "" {
		resolved, err := resolveTargetAgent(h.targetAgent)
		if err != nil {
			return fmt.Errorf("resolving target agent: %w", err)
		}
		h.agentID = resolved.agentID
		return nil
	}
	resolved, err := resolveSelfTarget()
	if err != nil {
		return fmt.Errorf("detecting agent identity: %w", err)
	}
	h.agentID = resolved.agentID
	return nil
}

func resolveHookWorkDir(h *hookRun) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}
	h.townBeadsDir = filepath.Join(townRoot, ".beads")
	if h.targetAgent == "" {
		workDir, err := findLocalBeadsDir()
		if err != nil {
			return fmt.Errorf("not in a beads workspace: %w", err)
		}
		h.workDir = workDir
		return nil
	}
	agentBeadID := agentIDToBeadID(h.agentID, townRoot)
	if agentBeadID == "" {
		return fmt.Errorf("could not convert agent ID %s to bead ID", h.agentID)
	}
	rigName := strings.Split(h.agentID, "/")[0]
	fallbackPath := filepath.Join(townRoot, rigName)
	if rigName == "mayor" || rigName == "deacon" {
		fallbackPath = townRoot
	}
	h.workDir = beads.ResolveHookDir(townRoot, agentBeadID, fallbackPath)
	return nil
}

func replaceExistingHook(h *hookRun) (bool, error) {
	b := beads.New(h.workDir)
	existingPinned, err := b.List(beads.ListOptions{
		Status:   beads.StatusHooked,
		Assignee: h.agentID,
		Priority: -1,
	})
	if err != nil {
		return false, fmt.Errorf("checking existing hooked beads: %w", err)
	}
	if len(existingPinned) == 0 {
		return false, nil
	}
	existing := existingPinned[0]
	if existing.ID == h.beadID {
		fmt.Printf("%s Already hooked: %s\n", style.Bold.Render("✓"), h.beadID)
		return true, nil
	}
	isComplete, hasAttachment := checkPinnedBeadComplete(b, existing)
	if isComplete {
		return false, replaceCompletedHook(h, b, existing, hasAttachment)
	}
	if h.force {
		return false, forceReplaceHook(h, b, existing)
	}
	return false, fmt.Errorf("existing hooked bead %s is incomplete (%s)\n  Use --force to replace, or complete the existing work first",
		existing.ID, existing.Title)
}

func replaceCompletedHook(h *hookRun, b *beads.Beads, existing *beads.Issue, hasAttachment bool) error {
	fmt.Printf("%s Replacing completed bead %s...\n", style.Dim.Render("ℹ"), existing.ID)
	if h.dryRun {
		return nil
	}
	if hasAttachment {
		if err := closeCompletedHookedMolecule(h.workDir, existing.ID); err != nil {
			return fmt.Errorf("closing completed bead %s: %w", existing.ID, err)
		}
		return nil
	}
	status := "open"
	if err := b.Update(existing.ID, beads.UpdateOptions{Status: &status}); err != nil {
		return fmt.Errorf("unpinning bead %s: %w", existing.ID, err)
	}
	return nil
}

func forceReplaceHook(h *hookRun, b *beads.Beads, existing *beads.Issue) error {
	fmt.Printf("%s Force-replacing incomplete bead %s...\n", style.Dim.Render("⚠"), existing.ID)
	if h.dryRun {
		return nil
	}
	status := "open"
	if err := b.Update(existing.ID, beads.UpdateOptions{Status: &status}); err != nil {
		return fmt.Errorf("unpinning bead %s: %w", existing.ID, err)
	}
	return nil
}

func applyHookBead(h *hookRun) (bool, error) {
	if h.targetAgent != "" {
		fmt.Printf("%s Hooking %s for %s...\n", style.Bold.Render("🪝"), h.beadID, h.agentID)
	} else {
		fmt.Printf("%s Hooking %s...\n", style.Bold.Render("🪝"), h.beadID)
	}
	if h.dryRun {
		fmt.Printf("Would run: bd update %s --status=hooked --assignee=%s\n", h.beadID, h.agentID)
		if h.subject != "" {
			fmt.Printf("  subject (for handoff mail): %s\n", h.subject)
		}
		if h.message != "" {
			fmt.Printf("  context (for handoff mail): %s\n", h.message)
		}
		return true, nil
	}
	if err := applyHookBeadUpdate(h); err != nil {
		return false, err
	}
	enqueueMayorHookNudge(h)
	return false, nil
}

func applyHookBeadUpdate(h *hookRun) error {
	const hookMaxRetries = 5
	const hookBaseBackoff = 500 * time.Millisecond
	const hookBackoffMax = 10 * time.Second
	var lastHookErr error
	for attempt := 1; attempt <= hookMaxRetries; attempt++ {
		if err := BdCmd("update", h.beadID, "--status=hooked", "--assignee="+h.agentID).
			Dir(resolveBeadDir(h.beadID)).
			StripBeadsDir().
			WithAutoCommit().
			Run(); err != nil {
			lastHookErr = err
			if attempt < hookMaxRetries {
				backoff := slingBackoff(attempt, hookBaseBackoff, hookBackoffMax)
				fmt.Printf("%s Hook attempt %d failed, retrying in %v...\n", style.Warning.Render("⚠"), attempt, backoff)
				time.Sleep(backoff)
				continue
			}
			return fmt.Errorf("hooking bead after %d attempts: %w", hookMaxRetries, lastHookErr)
		}
		return nil
	}
	return lastHookErr
}

func enqueueMayorHookNudge(h *hookRun) {
	if h.agentID != "mayor/" {
		return
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return
	}
	_ = nudge.Enqueue(townRoot, "hq-mayor", nudge.QueuedNudge{
		Sender:   "hook",
		Message:  fmt.Sprintf("Hook updated: attached bead %s", h.beadID),
		Priority: nudge.PriorityNormal,
	})
}

func finishHookRun(h *hookRun) {
	if h.targetAgent != "" {
		fmt.Printf("%s Work attached to %s's hook\n", style.Bold.Render("✓"), h.agentID)
	} else {
		fmt.Printf("%s Work attached to hook (hooked bead)\n", style.Bold.Render("✓"))
	}
	if err := updateAgentHookBead(h.agentID, h.beadID, h.workDir, h.townBeadsDir); err != nil {
		fmt.Printf("%s Could not update agent hook: %v\n", style.Dim.Render("Warning:"), err)
	}
	if h.targetAgent != "" {
		fmt.Printf("  Use 'gt hook show %s' to verify\n", h.targetAgent)
	} else {
		fmt.Printf("  Use 'gt handoff' to restart with this work\n")
		fmt.Printf("  Use 'gt hook' to see hook status\n")
	}
	if err := events.LogFeed(events.TypeHook, h.agentID, events.HookPayload(h.beadID)); err != nil {
		fmt.Fprintf(os.Stderr, "%s Warning: failed to log hook event: %v\n", style.Dim.Render("⚠"), err)
	}
}
