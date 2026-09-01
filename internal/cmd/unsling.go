package cmd

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/nudge"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var unslingCmd = &cobra.Command{
	Use:     "unsling [bead-id] [target]",
	Aliases: []string{"unhook"},
	GroupID: GroupWork,
	Short:   "Remove work from an agent's hook",
	Long: `Remove work from an agent's hook (the inverse of sling/hook).

With no arguments, clears your own hook. With a bead ID, only unslings
if that specific bead is currently hooked. With a target, operates on
another agent's hook.

Examples:
  gt unsling                        # Clear my hook (whatever's there)
  gt unsling gt-abc                 # Only unsling if gt-abc is hooked
  gt unsling greenplace/joe            # Clear joe's hook
  gt unsling gt-abc greenplace/joe     # Unsling gt-abc from joe

The bead's status changes from 'hooked' back to 'open'.

Related commands:
  gt sling <bead>    # Hook + start (inverse of unsling)
  gt hook <bead>     # Hook without starting
  gt hook      # See what's on your hook`,
	Args: cobra.MaximumNArgs(2),
	RunE: runUnsling,
}

func init() {
	unslingCmd.Flags().BoolP("dry-run", "n", false, "Show what would be done")
	unslingCmd.Flags().BoolP("force", "f", false, "Unsling even if work is incomplete")
	rootCmd.AddCommand(unslingCmd)
}

func runUnsling(cmd *cobra.Command, args []string) error {
	return runUnslingWith(cmd, args, commandBoolFlag(cmd, "dry-run"), commandBoolFlag(cmd, "force"))
}

type unslingRequest struct {
	targetBeadID string
	targetAgent  string
	dryRun       bool
	force        bool
}

type unslingContext struct {
	request   unslingRequest
	townRoot  string
	agentID   string
	beadsPath string
	beads     *beads.Beads
}

func runUnslingWith(cmd *cobra.Command, args []string, dryRun, force bool) error {
	request := parseUnslingRequest(args, dryRun, force)
	ctx, err := resolveUnslingContext(request)
	if err != nil {
		return err
	}
	hookedBeadID := findUnslingHookedBead(ctx)
	if hookedBeadID == "" {
		// hook_bead is empty, but there may be stale beads with status "hooked"
		// still assigned to this agent (e.g., hook_bead was cleared but bead status
		// wasn't updated). Clean them up so gt hook and gt unsling stay consistent.
		if !cleanStaleHookedBeads(cmd, ctx.beads, ctx.agentID, request.targetBeadID, ctx.townRoot, ctx.beadsPath, request.dryRun) {
			reportNoUnslingWork(request, ctx.agentID)
		}
		return nil
	}

	if err := validateUnslingTarget(request, hookedBeadID); err != nil {
		return err
	}

	hookedB, hookedBead, err := loadUnslingBead(ctx, hookedBeadID)
	if err != nil {
		if !request.force {
			return fmt.Errorf("getting hooked bead %s: %w\n  Use --force to unsling anyway", hookedBeadID, err)
		}
		hookedBead = &beads.Issue{ID: hookedBeadID, Title: "(unknown)"}
	}

	if err := validateUnslingCompletion(hookedBeadID, hookedBead, request.force); err != nil {
		return err
	}

	announceUnsling(request, ctx.agentID, hookedBeadID)
	if request.dryRun {
		fmt.Printf("Would unsling hooked bead from %s\n", ctx.agentID)
		return nil
	}

	updateUnslingBead(cmd, hookedB, hookedBeadID, hookedBead)

	_ = events.LogFeed(events.TypeUnhook, ctx.agentID, events.UnhookPayload(hookedBeadID))
	notifyUnslingMayor(ctx, hookedBeadID)
	reportUnslingComplete(ctx.agentID, hookedBeadID)
	return nil
}

func parseUnslingRequest(args []string, dryRun, force bool) unslingRequest {
	request := unslingRequest{dryRun: dryRun, force: force}
	switch len(args) {
	case 1:
		if isAgentTarget(args[0]) {
			request.targetAgent = args[0]
		} else {
			request.targetBeadID = args[0]
		}
	case 2:
		request.targetBeadID = args[0]
		request.targetAgent = args[1]
	}
	return request
}

func resolveUnslingContext(request unslingRequest) (*unslingContext, error) {
	agentID, err := resolveUnslingAgent(request.targetAgent)
	if err != nil {
		return nil, err
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return nil, fmt.Errorf("finding town root: %w", err)
	}
	agentBeadID := agentIDToBeadID(agentID, townRoot)
	if agentBeadID == "" {
		return nil, fmt.Errorf("could not convert agent ID %s to bead ID", agentID)
	}
	beadsPath := unslingBeadsPath(townRoot, agentID, agentBeadID)
	return &unslingContext{
		request:   request,
		townRoot:  townRoot,
		agentID:   agentID,
		beadsPath: beadsPath,
		beads:     beads.New(beadsPath),
	}, nil
}

func resolveUnslingAgent(targetAgent string) (string, error) {
	if targetAgent != "" {
		agentID, _, _, err := resolveTargetAgent(targetAgent)
		if err != nil {
			return "", fmt.Errorf("resolving target agent: %w", err)
		}
		return agentID, nil
	}
	agentID, _, _, err := resolveSelfTarget()
	if err != nil {
		return "", fmt.Errorf("detecting agent identity: %w", err)
	}
	return agentID, nil
}

func unslingBeadsPath(townRoot, agentID, agentBeadID string) string {
	rigName := strings.Split(agentID, "/")[0]
	fallbackPath := filepath.Join(townRoot, rigName)
	if rigName == "mayor" || rigName == "deacon" {
		fallbackPath = townRoot
	}
	return beads.ResolveHookDir(townRoot, agentBeadID, fallbackPath)
}

func findUnslingHookedBead(ctx *unslingContext) string {
	if hooked := firstHookedBead(ctx.beads, ctx.agentID); hooked != "" {
		return hooked
	}
	if isTownLevelRole(ctx.agentID) || ctx.townRoot == "" {
		return ""
	}
	return findTownHookedBead(ctx.townRoot, ctx.agentID)
}

func firstHookedBead(b *beads.Beads, agentID string) string {
	hooked, err := b.List(beads.ListOptions{Status: beads.StatusHooked, Assignee: agentID, Priority: -1})
	if err != nil || len(hooked) == 0 {
		return ""
	}
	return hooked[0].ID
}

func findTownHookedBead(townRoot, agentID string) string {
	townB := beads.New(filepath.Join(townRoot, ".beads"))
	if hooked := firstHookedBead(townB, agentID); hooked != "" {
		return hooked
	}
	normalizedID := mailNormalizedAgentID(agentID)
	if normalizedID == agentID {
		return ""
	}
	return firstHookedBead(townB, normalizedID)
}

func validateUnslingTarget(request unslingRequest, hookedBeadID string) error {
	if request.targetBeadID != "" && hookedBeadID != request.targetBeadID {
		return fmt.Errorf("bead %s is not hooked (current hook: %s)", request.targetBeadID, hookedBeadID)
	}
	return nil
}

func loadUnslingBead(ctx *unslingContext, hookedBeadID string) (*beads.Beads, *beads.Issue, error) {
	hookedBeadPath := beads.ResolveHookDir(ctx.townRoot, hookedBeadID, ctx.beadsPath)
	hookedB := ctx.beads
	if hookedBeadPath != ctx.beadsPath {
		hookedB = beads.New(hookedBeadPath)
	}
	hookedBead, err := hookedB.Show(hookedBeadID)
	return hookedB, hookedBead, err
}

func validateUnslingCompletion(hookedBeadID string, hookedBead *beads.Issue, force bool) error {
	if hookedBead.Status == "closed" || force {
		return nil
	}
	return fmt.Errorf("hooked work %s is incomplete (%s)\n  Use --force to unsling anyway", hookedBeadID, hookedBead.Title)
}

func announceUnsling(request unslingRequest, agentID, hookedBeadID string) {
	if request.targetAgent != "" {
		fmt.Printf("%s Unslinging %s from %s...\n", style.Bold.Render("🪝"), hookedBeadID, agentID)
		return
	}
	fmt.Printf("%s Unslinging %s...\n", style.Bold.Render("🪝"), hookedBeadID)
}

func updateUnslingBead(cmd *cobra.Command, hookedB *beads.Beads, hookedBeadID string, hookedBead *beads.Issue) {
	if hookedBead.Status != beads.StatusHooked {
		return
	}
	openStatus := "open"
	emptyAssignee := ""
	if err := hookedB.Update(hookedBeadID, beads.UpdateOptions{Status: &openStatus, Assignee: &emptyAssignee}); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: couldn't update bead %s status: %v\n", hookedBeadID, err)
	}
}

func notifyUnslingMayor(ctx *unslingContext, hookedBeadID string) {
	if ctx.agentID != "mayor/" || ctx.townRoot == "" {
		return
	}
	message := fmt.Sprintf("Hook updated: cleared bead %s", hookedBeadID)
	_ = nudge.Enqueue(ctx.townRoot, "hq-mayor", nudge.QueuedNudge{Sender: "unsling", Message: message, Priority: nudge.PriorityNormal})
}

func reportNoUnslingWork(request unslingRequest, agentID string) {
	if request.targetAgent != "" {
		fmt.Printf("%s No work hooked for %s\n", style.Dim.Render("ℹ"), agentID)
		return
	}
	fmt.Printf("%s Nothing on your hook\n", style.Dim.Render("ℹ"))
}

func reportUnslingComplete(agentID, hookedBeadID string) {
	fmt.Printf("%s Work removed from hook\n", style.Bold.Render("✓"))
	fmt.Printf("  Agent %s hook cleared (was: %s)\n", agentID, hookedBeadID)

}

// cleanStaleHookedBeads finds and cleans up beads with status "hooked" assigned to
// agentID when the agent bead's hook_bead field is already null. This handles the
// inconsistency where hook_bead was cleared (e.g., by another process) but the
// bead's status wasn't updated back to "open". Without this, gt hook shows the
// stale hook (via fallback query) but gt unsling says "Nothing on your hook".
// Returns true if any stale beads were cleaned up.
func cleanStaleHookedBeads(cmd *cobra.Command, b *beads.Beads, agentID, targetBeadID, townRoot, beadsPath string, dryRun bool) bool {
	staleBeads := collectStaleHookedBeads(b, agentID, townRoot, beadsPath)
	staleBeads = filterStaleHookedBeads(staleBeads, targetBeadID)
	if len(staleBeads) == 0 {
		return false
	}
	if dryRun {
		printStaleHookedDryRun(staleBeads)
		return true
	}
	for _, sb := range staleBeads {
		cleanOneStaleHookedBead(cmd, b, sb, townRoot, beadsPath)
	}
	return true
}

func collectStaleHookedBeads(b *beads.Beads, agentID, townRoot, beadsPath string) []*beads.Issue {
	staleBeads, err := listStaleHookedBeads(b, agentID)
	if err != nil {
		staleBeads = nil
	}
	return append(staleBeads, collectTownStaleHookedBeads(agentID, townRoot, beadsPath)...)
}

func listStaleHookedBeads(b *beads.Beads, agentID string) ([]*beads.Issue, error) {
	return b.List(beads.ListOptions{Status: beads.StatusHooked, Assignee: agentID, Priority: -1})
}

func collectTownStaleHookedBeads(agentID, townRoot, beadsPath string) []*beads.Issue {
	if isTownLevelRole(agentID) || townRoot == "" {
		return nil
	}
	townBeadsPath := filepath.Join(townRoot, ".beads")
	if townBeadsPath == beadsPath {
		return nil
	}
	townB := beads.New(townBeadsPath)
	var staleBeads []*beads.Issue
	if townStale, err := listStaleHookedBeads(townB, agentID); err == nil {
		staleBeads = append(staleBeads, townStale...)
	}
	normalizedID := mailNormalizedAgentID(agentID)
	if normalizedID != agentID {
		if townStale, err := listStaleHookedBeads(townB, normalizedID); err == nil {
			staleBeads = append(staleBeads, townStale...)
		}
	}
	return staleBeads
}

func filterStaleHookedBeads(staleBeads []*beads.Issue, targetBeadID string) []*beads.Issue {
	if targetBeadID == "" {
		return staleBeads
	}
	filtered := make([]*beads.Issue, 0, len(staleBeads))
	for _, stale := range staleBeads {
		if stale.ID == targetBeadID {
			filtered = append(filtered, stale)
		}
	}
	return filtered
}

func printStaleHookedDryRun(staleBeads []*beads.Issue) {
	for _, stale := range staleBeads {
		fmt.Printf("Would clean up stale hooked bead %s (%s)\n", stale.ID, stale.Title)
	}
}

func cleanOneStaleHookedBead(cmd *cobra.Command, b *beads.Beads, stale *beads.Issue, townRoot, beadsPath string) {
	fmt.Printf("%s Cleaning up stale hooked bead %s...\n", style.Bold.Render("🪝"), stale.ID)
	staleB := b
	stalePath := beads.ResolveHookDir(townRoot, stale.ID, beadsPath)
	if stalePath != beadsPath {
		staleB = beads.New(stalePath)
	}
	openStatus := "open"
	emptyAssignee := ""
	if err := staleB.Update(stale.ID, beads.UpdateOptions{Status: &openStatus, Assignee: &emptyAssignee}); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: couldn't clean up stale bead %s: %v\n", stale.ID, err)
		return
	}
	fmt.Printf("%s Cleaned up stale bead %s (was hooked, now open)\n", style.Bold.Render("✓"), stale.ID)
}

// mailNormalizedAgentID normalizes crew/polecats path to the canonical form
// used by sendHandoffMail (via mail.AddressToIdentity).
// "rig/crew/name" → "rig/name", "rig/polecats/name" → "rig/name".
func mailNormalizedAgentID(agentID string) string {
	parts := strings.Split(agentID, "/")
	if len(parts) == 3 && (parts[1] == "crew" || parts[1] == "polecats") {
		return parts[0] + "/" + parts[2]
	}
	return agentID
}

// isAgentTarget checks if a string looks like an agent target rather than a bead ID.
// Agent targets contain "/" or are known role names.
func isAgentTarget(s string) bool {
	// Contains "/" means it's a path like "greenplace/joe"
	for _, c := range s {
		if c == '/' {
			return true
		}
	}

	// Known role names
	switch s {
	case constants.RoleMayor, constants.RoleDeacon, constants.RoleWitness, constants.RoleRefinery, constants.RoleCrew:
		return true
	}

	return false
}
