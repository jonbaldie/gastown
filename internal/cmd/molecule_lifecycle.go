package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/telemetry"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// runMoleculeBurn burns (destroys) the current molecule attachment.
func runMoleculeBurn(cmd *cobra.Command, args []string) (retErr error) {
	target, err := resolveMoleculeTarget(args)
	if err != nil {
		return err
	}
	b, handoff, err := loadMoleculeHandoff(target)
	if err != nil {
		return err
	}

	// Check for attached molecule
	attachment := beads.ParseAttachmentFields(handoff)
	if attachment == nil || attachment.AttachedMolecule == "" {
		fmt.Printf("%s No molecule attached to %s - nothing to burn\n",
			style.Dim.Render("ℹ"), target)
		return nil
	}

	return burnAttachedMolecule(cmd, target, b, handoff, attachment.AttachedMolecule)
}

func burnAttachedMolecule(cmd *cobra.Command, target string, b *beads.Beads, handoff *beads.Issue, moleculeID string) (retErr error) {
	// Recursively close all descendant step issues before detaching
	// This prevents orphaned step issues from accumulating (gt-psj76.1)
	childrenClosed := closeDescendants(b, moleculeID)
	defer func() {
		ctx := context.Background()
		if cmd != nil {
			ctx = cmd.Context()
		}
		telemetry.RecordMolBurn(ctx, moleculeID, childrenClosed, retErr)
	}()

	// Detach the molecule with audit logging (this "burns" it by removing the attachment)
	_, err := b.DetachMoleculeWithAudit(handoff.ID, beads.DetachOptions{
		Operation: "burn",
		Agent:     target,
		Reason:    "molecule burned by agent",
	})
	if err != nil {
		return fmt.Errorf("detaching molecule: %w", err)
	}
	// Close the molecule root after detach so the audit sees original status.
	// Without this, the wisp root stays in "hooked" status indefinitely,
	// causing patrol molecule leaks (issue #1828).
	rootClosed := true
	if closeErr := b.ForceCloseWithReason("burned", moleculeID); closeErr != nil {
		style.PrintWarning("could not close molecule root %s: %v", moleculeID, closeErr)
		rootClosed = false
	}

	if moleculeJSON {
		result := map[string]interface{}{
			"burned":          moleculeID,
			"from":            target,
			"handoff_id":      handoff.ID,
			"children_closed": childrenClosed,
			"root_closed":     rootClosed,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Printf("%s Burned molecule %s from %s\n",
		style.Bold.Render("🔥"), moleculeID, target)
	if childrenClosed > 0 {
		fmt.Printf("  Closed %d step issues\n", childrenClosed)
	}

	return nil
}

func resolveMoleculeTarget(args []string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting current directory: %w", err)
	}

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return "", fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return "", fmt.Errorf("not in a Gas Town workspace")
	}
	if len(args) > 0 {
		return args[0], nil
	}

	// Auto-detect using env-aware role detection.
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return "", fmt.Errorf("detecting role: %w", err)
	}
	roleCtx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	target := buildAgentIdentity(roleCtx)
	if target == "" {
		return "", fmt.Errorf("cannot determine agent identity (role: %s)", roleCtx.Role)
	}
	return target, nil
}

func loadMoleculeHandoff(target string) (*beads.Beads, *beads.Issue, error) {
	workDir, err := findLocalBeadsDir()
	if err != nil {
		return nil, nil, fmt.Errorf("not in a beads workspace: %w", err)
	}

	b := beads.New(workDir)
	role := extractRoleFromIdentity(target)
	handoff, err := b.FindHandoffBead(role)
	if err != nil {
		return nil, nil, fmt.Errorf("finding handoff bead: %w", err)
	}
	if handoff == nil {
		return nil, nil, fmt.Errorf("no handoff bead found for %s (looked for %q with pinned status)", target, beads.HandoffBeadTitle(role))
	}
	return b, handoff, nil
}

// runMoleculeSquash squashes the current molecule into a digest.
func runMoleculeSquash(cmd *cobra.Command, args []string) (retErr error) {
	// Parse jitter early so invalid flags fail fast, but defer the sleep
	// until after workspace/attachment validation so no-op invocations
	// (wrong directory, no attached molecule) don't wait unnecessarily.
	jitterMax, err := parseMoleculeJitter(moleculeJitter)
	if err != nil {
		return err
	}

	target, err := resolveMoleculeTarget(args)
	if err != nil {
		return err
	}
	b, handoff, err := loadMoleculeHandoff(target)
	if err != nil {
		return err
	}

	moleculeID := moleculeAttachmentID(handoff)
	if moleculeID == "" {
		fmt.Printf("%s No molecule attached to %s - nothing to squash\n",
			style.Dim.Render("ℹ"), target)
		return nil
	}

	var doneSteps, totalSteps int
	defer func() {
		telemetry.RecordMolSquash(cmd.Context(), moleculeID, doneSteps, totalSteps, !moleculeNoDigest, retErr)
	}()

	// Apply jitter before acquiring any Dolt locks.
	// Multiple patrol agents (deacon, witness, refinery) squash concurrently at
	// cycle end, causing exclusive-lock contention. A random pre-sleep
	// desynchronizes them without changing semantics.
	if err := waitForMoleculeJitter(cmd.Context(), jitterMax); err != nil {
		return err
	}

	// Recursively close all descendant step issues before squashing
	// This prevents orphaned step issues from accumulating (gt-psj76.1)
	childrenClosed := closeDescendants(b, moleculeID)

	// Skip digest creation if --no-digest flag is set (gt-t2bjt).
	// Patrol molecules (deacon, witness, refinery) run frequently and their
	// digests pollute the database with thousands of low-value beads.
	if !moleculeNoDigest {
		doneSteps, totalSteps, err = createMoleculeDigest(b, moleculeID, target)
		if err != nil {
			return err
		}
	}

	rootClosed, err := detachAndCloseMolecule(b, handoff, target, moleculeID)
	if err != nil {
		return err
	}
	return renderMoleculeSquashResult(moleculeID, target, handoff.ID, childrenClosed, rootClosed)
}

func moleculeAttachmentID(handoff *beads.Issue) string {
	attachment := beads.ParseAttachmentFields(handoff)
	if attachment == nil {
		return ""
	}
	return attachment.AttachedMolecule
}

func detachAndCloseMolecule(b *beads.Beads, handoff *beads.Issue, target, moleculeID string) (bool, error) {
	detachReason := "molecule squashed"
	if moleculeNoDigest {
		detachReason = "molecule squashed (no digest)"
	}
	_, err := b.DetachMoleculeWithAudit(handoff.ID, beads.DetachOptions{
		Operation: "squash",
		Agent:     target,
		Reason:    detachReason,
	})
	if err != nil {
		return false, fmt.Errorf("detaching molecule: %w", err)
	}

	// Close the molecule root after detach so the audit sees original status.
	// Without this, the wisp root stays in "hooked" status indefinitely,
	// causing patrol molecule leaks (issue #1828).
	if err := b.ForceCloseWithReason("squashed", moleculeID); err != nil {
		style.PrintWarning("could not close molecule root %s: %v", moleculeID, err)
		return false, nil
	}
	return true, nil
}

func renderMoleculeSquashResult(moleculeID, target, handoffID string, childrenClosed int, rootClosed bool) error {
	if moleculeJSON {
		result := map[string]interface{}{
			"squashed":        moleculeID,
			"from":            target,
			"handoff_id":      handoffID,
			"children_closed": childrenClosed,
			"digest_skipped":  moleculeNoDigest,
			"root_closed":     rootClosed,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	digestSuffix := ""
	if moleculeNoDigest {
		digestSuffix = " (no digest)"
	}
	fmt.Printf("%s Squashed molecule %s%s\n", style.Bold.Render("📦"), moleculeID, digestSuffix)
	if childrenClosed > 0 {
		fmt.Printf("  Closed %d step issues\n", childrenClosed)
	}
	return nil
}

func parseMoleculeJitter(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	jitterMax, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid --jitter duration %q: %w", value, err)
	}
	if jitterMax < 0 {
		return 0, fmt.Errorf("--jitter must be non-negative, got %v", jitterMax)
	}
	return jitterMax, nil
}

func waitForMoleculeJitter(ctx context.Context, jitterMax time.Duration) error {
	if jitterMax <= 0 {
		return nil
	}
	//nolint:gosec // weak RNG is fine for jitter
	sleep := time.Duration(rand.Int63n(int64(jitterMax)))
	fmt.Fprintf(os.Stderr, "jitter: sleeping %v before squash\n", sleep)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(sleep):
		return nil
	}
}

func createMoleculeDigest(b *beads.Beads, moleculeID, target string) (int, int, error) {
	progress, _ := getMoleculeProgressInfo(b, moleculeID)
	digestDesc := fmt.Sprintf(`Squashed molecule execution.

molecule: %s
agent: %s
squashed_at: %s
`, moleculeID, target, time.Now().UTC().Format(time.RFC3339))

	doneSteps, totalSteps := 0, 0
	if moleculeSummary != "" {
		digestDesc += fmt.Sprintf("\n## Summary\n%s\n", moleculeSummary)
	}
	if progress != nil {
		doneSteps = progress.DoneSteps
		totalSteps = progress.TotalSteps
		digestDesc += fmt.Sprintf(`
## Execution Summary
- Steps: %d/%d completed
- Status: %s
`, progress.DoneSteps, progress.TotalSteps, moleculeProgressStatus(progress.Complete))
	}

	digestIssue, err := b.Create(beads.CreateOptions{
		Title:       fmt.Sprintf("Digest: %s", moleculeID),
		Description: digestDesc,
		Labels:      []string{"gt:task"},
		Priority:    4,
		Actor:       target,
		Ephemeral:   true,
	})
	if err != nil {
		return doneSteps, totalSteps, fmt.Errorf("creating digest: %w", err)
	}

	_ = b.Update(digestIssue.ID, beads.UpdateOptions{AddLabels: []string{"digest"}})
	closedStatus := "closed"
	if err := b.Update(digestIssue.ID, beads.UpdateOptions{Status: &closedStatus}); err != nil {
		style.PrintWarning("Created digest but couldn't close it: %v", err)
	}
	return doneSteps, totalSteps, nil
}

func moleculeProgressStatus(complete bool) string {
	if complete {
		return "complete"
	}
	return "partial"
}

// closeDescendants recursively closes all descendant issues of a parent.
// Returns the count of issues closed. Logs warnings on errors but doesn't fail.
func closeDescendants(b *beads.Beads, parentID string) int {
	count, err := closeDescendantsImpl(b, parentID, false)
	if err != nil {
		style.PrintWarning("closing descendants of %s: %v", parentID, err)
	}
	return count
}

// forceCloseDescendants is like closeDescendants but uses force-close,
// which succeeds even for beads in invalid states. Returns the count of
// issues closed and any error encountered. Callers should check the error
// to avoid closing a parent while children survive (gt-7lx3).
func forceCloseDescendants(b *beads.Beads, parentID string) (int, error) {
	return closeDescendantsImpl(b, parentID, true)
}

func closeDescendantsImpl(b *beads.Beads, parentID string, force bool) (int, error) {
	children, err := b.List(beads.ListOptions{
		Parent: parentID,
		Status: "all",
	})
	if err != nil {
		return 0, fmt.Errorf("listing children of %s: %w", parentID, err)
	}

	if len(children) == 0 {
		return 0, nil
	}

	// First, recursively close grandchildren.
	totalClosed, errs := closeDescendantTrees(b, children, force)

	// Then close direct children.
	directClosed, directErr := closeDirectChildren(b, parentID, children, force)
	totalClosed += directClosed
	if directErr != nil {
		errs = append(errs, directErr)
	}

	if len(errs) > 0 {
		return totalClosed, errors.Join(errs...)
	}
	return totalClosed, nil
}

func closeDescendantTrees(b *beads.Beads, children []*beads.Issue, force bool) (int, []error) {
	totalClosed := 0
	var errs []error
	for _, child := range children {
		closed, childErr := closeDescendantsImpl(b, child.ID, force)
		totalClosed += closed
		if childErr != nil {
			errs = append(errs, childErr)
		}
	}
	return totalClosed, errs
}

func closeDirectChildren(b *beads.Beads, parentID string, children []*beads.Issue, force bool) (int, error) {
	idsToClose := openChildIDs(children)
	if len(idsToClose) == 0 {
		return 0, nil
	}

	if err := closeChildIssues(b, idsToClose, force); err != nil {
		return 0, fmt.Errorf("closing children of %s: %w", parentID, err)
	}
	return len(idsToClose), nil
}

func openChildIDs(children []*beads.Issue) []string {
	var ids []string
	for _, child := range children {
		if child.Status != "closed" {
			ids = append(ids, child.ID)
		}
	}
	return ids
}

func closeChildIssues(b *beads.Beads, ids []string, force bool) error {
	if force {
		return b.ForceCloseWithReason("burned: force-close descendants", ids...)
	}
	return b.Close(ids...)
}
