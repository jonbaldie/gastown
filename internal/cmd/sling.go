package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/dog"
	"github.com/jonbaldie/gastown/internal/lock"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/telemetry"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/witness"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var slingCmd = &cobra.Command{
	Use:         "sling <bead-or-formula> [target]",
	GroupID:     GroupWork,
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Short:       "Assign work to an agent (THE unified work dispatch command)",
	Long: `Sling work onto an agent's hook and start working immediately.

This is THE command for assigning work in Gas Town. It handles:
  - Existing agents (mayor, crew, witness, refinery)
  - Auto-spawning polecats when target is a rig
  - Dispatching to dogs (Deacon's helper workers)
  - Formula instantiation and wisp creation
  - Auto-convoy creation for dashboard visibility

Create work in the target rig:
  File the bead where the code lives. Do not create town hq-* work beads
  and sling them to a rig.

  bd -C <town>/<rig> create --title=... --type=feature

  That directory has its own .beads with the rig prefix (gt now and
  gt rig add create it). If it is missing, bd walks up to town beads
  and silently files hq-* work.
  gt convoy create "..." <rig-prefix-id>
  gt convoy add <convoy-id> <rig-prefix-id>
  gt sling <rig-prefix-id> <rig> --merge=local   # third-party remotes

  Do not sling infrastructure beads (hq-cv-*, hq-mayor, ...). Those stay at town.
  Use --merge=local (or --push-url) for third-party public clones.

Auto-Convoy:
  When slinging a single issue (not a formula), sling automatically creates
  a convoy to track the work unless --no-convoy is specified. This ensures
  all work appears in 'gt convoy list', even "swarm of one" assignments.

  gt sling gt-abc gastown              # Creates "Work: <issue-title>" convoy
  gt sling gt-abc gastown --no-convoy  # Skip auto-convoy creation

Town Beads:
  Mayor-created town work (hq-*) can be slung to a rig. Sling moves the bead
  into the target rig with the same path as "gt bead move", then dispatches
  the new rig-prefixed ID. Convoys (hq-cv-*) and town agent/channel/group
  beads stay in HQ and are not auto-moved.

Merge Strategy (--merge):
  Controls how completed work lands. Stored on the auto-convoy and on the issue.
  gt sling gt-abc gastown --merge=direct  # Push branch directly to main
  gt sling gt-abc gastown --merge=mr      # Merge queue (default)
  gt sling gt-abc gastown --merge=local   # Keep on feature branch

  An explicit --merge value wins. If --merge is omitted, sling reuses a stored
  merge_strategy on the issue. If the issue text says "local commit only" or
  "do not push", sling uses local and stores merge_strategy=local so later
  dispatches keep the policy without the flag.

Target Resolution:
  gt sling gt-abc                       # Self (current agent)
  gt sling gt-abc crew                  # Crew worker in current rig
  gt sling gp-abc greenplace               # Auto-spawn polecat in rig
  gt sling gt-abc greenplace/Toast         # Specific polecat
  gt sling gt-abc gastown --crew mel    # Crew member mel in gastown
  gt sling gt-abc mayor                 # Mayor
  gt sling gt-abc deacon/dogs           # Auto-dispatch to idle dog
  gt sling gt-abc deacon/dogs/alpha     # Specific dog

Spawning Options (when target is a rig):
  gt sling gp-abc greenplace --create               # Create polecat if missing
  gt sling gp-abc greenplace --force                # Ignore unread mail
  gt sling gp-abc greenplace --account work         # Use specific Claude account

Natural Language Args:
  gt sling gt-abc --args "patch release"
  gt sling code-review --args "focus on security"

The --args string is stored in the bead and shown via gt prime. Since the
executor is an LLM, it interprets these instructions naturally.

Stdin Mode (for shell-quoting-safe multi-line content):
  echo "review for security issues" | gt sling gt-abc gastown --stdin
  gt sling gt-abc gastown --stdin <<'EOF'
  Focus on:
  1. SQL injection in query builders
  2. XSS in template rendering
  EOF

  # With --args on CLI, stdin goes to --message:
  echo "Extra context here" | gt sling gt-abc gastown --args "patch release" --stdin

Formula Slinging:
  gt sling mol-release mayor/           # Cook + wisp + attach + nudge
  gt sling towers-of-hanoi --var disks=3

Formula-on-Bead (--on flag):
  gt sling mol-review --on gt-abc       # Apply formula to existing work
  gt sling shiny --on gt-abc crew       # Apply formula, sling to crew

Compare:
  gt hook <bead>      # Just attach (no action)
  gt sling <bead>     # Attach + start now (keep context)
  gt handoff <bead>   # Attach + restart (fresh context)

The propulsion principle: if it's on your hook, YOU RUN IT.

Batch Slinging:
  gt sling gt-abc gt-def gt-ghi gastown   # Sling multiple beads to a rig
  gt sling gt-abc gt-def gastown --max-concurrent 3  # Spawn 3 at a time

  When multiple beads are provided with a rig target, each bead gets its own
  polecat. This parallelizes work dispatch without running gt sling N times.
  Use --max-concurrent to throttle spawn rate and prevent Dolt server overload.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runSling,
}

var (
	slingSubject     string
	slingMessage     string
	slingDryRun      bool
	slingOnTarget    string   // --on flag: target bead when slinging a formula
	slingVars        []string // --var flag: formula variables (key=value)
	slingArgs        string   // --args flag: natural language instructions for executor
	slingStdin       bool     // --stdin: read --message and/or --args from stdin
	slingHookRawBead bool     // --hook-raw-bead: hook raw bead without default formula (expert mode)

	// Flags migrated for polecat spawning (used by sling for work assignment)
	slingCreate        bool   // --create: create polecat if it doesn't exist
	slingForce         bool   // --force: force spawn even if polecat has unread mail
	slingAccount       string // --account: Claude Code account handle to use
	slingAgent         string // --agent: override runtime agent for this sling/spawn
	slingNoConvoy      bool   // --no-convoy: skip auto-convoy creation
	slingOwned         bool   // --owned: mark auto-convoy as caller-managed lifecycle
	slingNoMerge       bool   // --no-merge: skip merge queue on completion (for upstream PRs/human review)
	slingMerge         string // --merge: merge strategy for convoy (direct/mr/local)
	slingNoBoot        bool   // --no-boot: skip wakeRigAgents (avoid witness/refinery boot and lock contention)
	slingMaxConcurrent int    // --max-concurrent: throttle spawn rate in batch mode (spawns N, pauses, spawns N more)
	slingBaseBranch    string // --base-branch: override base branch for polecat worktree
	slingResumeBranch  string // --branch: resume an existing branch instead of creating a fresh one
	slingResumePR      int    // --pr: resume the head branch of an existing PR (resolves via gh)
	slingRalph         bool   // --ralph: enable Ralph Wiggum loop mode for multi-step workflows
	slingFormula       string // --formula: override formula for dispatch (default: mol-polecat-work)
	slingCrew          string // --crew: target a crew member in the specified rig
	slingReviewOnly    bool   // --review-only: mark work as review-only (no merge/commit/push)
)

func init() {
	slingCmd.Flags().StringVarP(&slingSubject, "subject", "s", "", "Context subject for the work")
	slingCmd.Flags().StringVarP(&slingMessage, "message", "m", "", "Context message for the work")
	slingCmd.Flags().BoolVarP(&slingDryRun, "dry-run", "n", false, "Show what would be done")
	slingCmd.Flags().StringVar(&slingOnTarget, "on", "", "Apply formula to existing bead (implies wisp scaffolding)")
	slingCmd.Flags().StringArrayVar(&slingVars, "var", nil, "Formula variable (key=value), can be repeated")
	slingCmd.Flags().StringVarP(&slingArgs, "args", "a", "", "Natural language instructions for the executor (e.g., 'patch release')")
	slingCmd.Flags().BoolVar(&slingStdin, "stdin", false, "Read --message and/or --args from stdin (avoids shell quoting issues)")

	// Flags for polecat spawning (when target is a rig)
	slingCmd.Flags().BoolVar(&slingCreate, "create", false, "Create polecat if it doesn't exist")
	slingCmd.Flags().BoolVar(&slingForce, "force", false, "Force spawn even if polecat has unread mail")
	slingCmd.Flags().StringVar(&slingAccount, "account", "", "Claude Code account handle to use")
	slingCmd.Flags().StringVar(&slingAgent, "agent", "", "Override agent/runtime for this sling (e.g., claude, gemini, codex, or custom alias)")
	slingCmd.Flags().BoolVar(&slingNoConvoy, "no-convoy", false, "Skip auto-convoy creation for single-issue sling")
	slingCmd.Flags().BoolVar(&slingOwned, "owned", false, "Mark auto-convoy as caller-managed lifecycle (no automatic witness/refinery registration)")
	slingCmd.Flags().BoolVar(&slingHookRawBead, "hook-raw-bead", false, "Hook raw bead without default formula (expert mode)")
	slingCmd.Flags().BoolVar(&slingNoMerge, "no-merge", false, "Skip merge queue on completion (keep work on feature branch for review)")
	slingCmd.Flags().StringVar(&slingMerge, "merge", "", "Merge strategy: direct (push to main), mr (merge queue, default), local (keep on branch)")
	slingCmd.Flags().BoolVar(&slingNoBoot, "no-boot", false, "Skip rig boot after polecat spawn (avoids witness/refinery lock contention)")
	slingCmd.Flags().IntVar(&slingMaxConcurrent, "max-concurrent", 0, "Throttle spawn rate: spawn N polecats, pause, then spawn N more (0 = no throttle). Does not limit total concurrent polecats")
	slingCmd.Flags().StringVar(&slingBaseBranch, "base-branch", "", "Override base branch for polecat worktree (e.g., 'develop', 'release/v2')")
	slingCmd.Flags().StringVar(&slingResumeBranch, "branch", "", "Resume work on an existing branch instead of creating a fresh polecat branch (use to fix an existing PR)")
	slingCmd.Flags().IntVar(&slingResumePR, "pr", 0, "Resume work on the head branch of an existing PR (resolved via 'gh pr view'). Mutually exclusive with --branch.")
	slingCmd.Flags().BoolVar(&slingRalph, "ralph", false, "Enable Ralph Wiggum loop mode (fresh context per step, for multi-step workflows)")
	slingCmd.Flags().StringVar(&slingFormula, "formula", "", "Formula to apply (default: mol-polecat-work for polecat targets)")
	slingCmd.Flags().StringVar(&slingCrew, "crew", "", "Target a crew member in the specified rig (e.g., --crew mel with target gastown → gastown/crew/mel)")
	slingCmd.Flags().BoolVar(&slingReviewOnly, "review-only", false, "Mark work as review-only: assignee evaluates and reports back, must NOT merge/commit/push")

	slingCmd.AddCommand(slingRespawnResetCmd)
	rootCmd.AddCommand(slingCmd)
}

var slingRespawnResetCmd = &cobra.Command{
	Use:   "respawn-reset <bead-id>",
	Short: "Reset the respawn counter for a bead",
	Long: `Reset the per-bead respawn counter so it can be slung again.

When a bead hits the respawn limit (3 attempts), gt sling blocks further
dispatches to prevent spawn storms. After investigating the root cause,
use this command to allow re-dispatch.`,
	Args: cobra.ExactArgs(1),
	RunE: runSlingRespawnReset,
}

func runSlingRespawnReset(_ *cobra.Command, args []string) error {
	beadID := args[0]
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if err := witness.ResetBeadRespawnCount(townRoot, beadID); err != nil {
		return fmt.Errorf("resetting respawn count for %s: %w", beadID, err)
	}
	fmt.Printf("Reset respawn counter for %s. It can be slung again.\n", beadID)
	return nil
}

func runSling(cmd *cobra.Command, args []string) (retErr error) {
	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
	}
	defer func() {
		bead, target := "", ""
		if len(args) > 0 {
			bead = args[0]
		}
		if len(args) > 1 {
			target = args[1]
		}
		telemetry.RecordSling(ctx, bead, target, retErr)
	}()
	return runSlingCommand(cmd, ctx, args)
}

func normalizeSlingTarget(target string) string {
	return strings.TrimRight(target, "/")
}

// checkCrossRigGuard validates that a bead's prefix matches the target rig.
// Polecats work in their rig's worktree and cannot fix code owned by another rig.
// Returns an error if the bead belongs to a different rig than the target polecat.
//
// When the prefix maps to town root, the guard warns rather than errors: this
// ambiguous case arises when a crew member's redirect chain is broken and their
// rig's .beads dir shares the town-level database and prefix (gt-gbu). Blocking
// here would silently swallow all polecat work for the affected rig.
//
// Truly unknown prefixes (not in routes.jsonl and not the target rig's
// configured prefix in rigs.json) are still hard-rejected.
func checkCrossRigGuard(beadID, targetAgent, townRoot string) error {
	beadPrefix := beads.ExtractPrefix(beadID)
	if beadPrefix == "" {
		return nil // Can't determine prefix, skip check
	}

	// Extract target rig from agent path (e.g., "gastown/polecats/Toast" → "gastown")
	targetRig := strings.SplitN(targetAgent, "/", 2)[0]
	if targetRig == "" {
		return nil
	}

	beadRig := beads.GetRigNameForPrefix(townRoot, beadPrefix)
	// rigs.json is authoritative when routes.jsonl is stale or missing the
	// rig prefix. A town-bead move can land as dm-* before routes catch up.
	targetPrefix := beads.GetPrefixForRig(townRoot, targetRig)
	if targetPrefix != "" && strings.TrimSuffix(beadPrefix, "-") == targetPrefix {
		return nil
	}

	if beadRig != targetRig {
		if beadRig == "" {
			// GetRigNameForPrefix returns "" for two distinct cases:
			//   (a) prefix is in routes.jsonl with path="." (known town-root prefix)
			//   (b) prefix is not in routes.jsonl at all (unknown prefix)
			// GetRigPathForPrefix distinguishes them: it returns townRoot for (a),
			// empty string for (b).
			if beads.GetRigPathForPrefix(townRoot, beadPrefix) == "" {
				// Unknown prefix — no route exists, can't resolve rig.
				return fmt.Errorf("bead %s (prefix %q) is not in rig %q — prefix not in routes\n"+
					"Create the task from the rig directory: cd %s && bd create --title=...\n"+
					"Use --force to override", beadID, strings.TrimSuffix(beadPrefix, "-"), targetRig, targetRig)
			}
			// Known town-root prefix — warn but allow. A crew member may have a
			// broken redirect chain causing rig beads to land in the town DB with
			// the town prefix. Blocking here silently drops all their polecat work
			// (gt-gbu). The polecat will surface any true mismatch on execution.
			fmt.Printf("  %s Bead %s has prefix %q (town root) but target is rig %q — "+
				"proceeding (broken redirect chain? see gt-gbu)\n",
				style.Warning.Render("⚠"), beadID, strings.TrimSuffix(beadPrefix, "-"), targetRig)
			return nil
		}
		return fmt.Errorf("cross-rig mismatch: bead %s (prefix %q) belongs to rig %q, but target is rig %q\n"+
			"Create the task from the target rig: cd %s && bd create --title=...\n"+
			"Use --force to override", beadID, strings.TrimSuffix(beadPrefix, "-"), beadRig, targetRig, targetRig)
	}

	return nil
}

// rollbackSlingArtifactsFn is a seam for tests. Production uses rollbackSlingArtifacts.
var rollbackSlingArtifactsFn = rollbackSlingArtifacts

// Rollback seams allow tests to assert molecule-cleanup behavior without
// depending on full beads storage side effects.
var getBeadInfoForRollback = getBeadInfo
var collectExistingMoleculesForRollback = collectExistingMolecules
var burnExistingMoleculesForRollback = burnExistingMolecules

func rawWorkflowFieldValues(info *beadInfo) (noMerge, reviewOnly bool, attachedAt string) {
	if info == nil {
		return false, false, ""
	}
	issue := &beads.Issue{Description: info.Description}
	fields := beads.ParseAttachmentFields(issue)
	if fields == nil {
		return false, false, ""
	}
	return fields.NoMerge, fields.ReviewOnly, fields.AttachedAt
}

func restoreRollbackRawWorkflowFields(beadID, townRoot, hookWorkDir string, info, originalInfo *beadInfo) (bool, error) {
	if info == nil {
		return false, nil
	}
	newDesc, changed := rollbackRawWorkflowDescription(info, originalInfo)
	if !changed || newDesc == info.Description {
		return false, nil
	}
	updateDir := beads.ResolveHookDir(townRoot, beadID, hookWorkDir)
	if err := BdCmd("update", beadID, "--description="+newDesc).
		Dir(updateDir).
		StripBeadsDir().
		WithAutoCommit().
		Run(); err != nil {
		return false, err
	}
	return true, nil
}

func rollbackRawWorkflowDescription(info, originalInfo *beadInfo) (string, bool) {
	originalNoMerge, originalReviewOnly, originalAttachedAt := rawWorkflowFieldValues(originalInfo)
	issue := &beads.Issue{Description: info.Description}
	fields := beads.ParseAttachmentFields(issue)
	if fields == nil {
		if !originalNoMerge && !originalReviewOnly {
			return "", false
		}
		fields = &beads.AttachmentFields{}
	}
	if fields.NoMerge == originalNoMerge && fields.ReviewOnly == originalReviewOnly && fields.AttachedAt == originalAttachedAt {
		return "", false
	}
	fields.NoMerge = originalNoMerge
	fields.ReviewOnly = originalReviewOnly
	fields.AttachedAt = originalAttachedAt
	return beads.SetAttachmentFields(issue, fields), true
}

func clearRollbackRawWorkflowFields(beadID, townRoot, hookWorkDir string, info *beadInfo) (bool, error) {
	return restoreRollbackRawWorkflowFields(beadID, townRoot, hookWorkDir, info, nil)
}

func restoreRollbackRawWorkflowFieldsFromCurrent(beadID, townRoot, hookWorkDir string, originalInfo *beadInfo) {
	if beadID == "" || townRoot == "" {
		return
	}
	info, err := getBeadInfoForRollback(beadID)
	if err != nil {
		fmt.Printf("  %s Could not inspect bead %s for raw workflow metadata rollback: %v\n", style.Dim.Render("Warning:"), beadID, err)
		return
	}
	if restored, restoreErr := restoreRollbackRawWorkflowFields(beadID, townRoot, hookWorkDir, info, originalInfo); restoreErr != nil {
		fmt.Printf("  %s Could not restore raw workflow metadata on %s: %v\n", style.Dim.Render("Warning:"), beadID, restoreErr)
	} else if restored {
		fmt.Printf("  %s Restored raw workflow metadata on %s\n", style.Dim.Render("○"), beadID)
	}
}

func restorePinnedBead(townRoot, beadID, assignee string) {
	if townRoot == "" || beadID == "" {
		return
	}
	dir := beads.ResolveHookDir(townRoot, beadID, "")
	if err := BdCmd("update", beadID, "--status=pinned", "--assignee="+assignee).
		Dir(dir).
		WithAutoCommit().
		Run(); err != nil {
		fmt.Printf("  %s Could not restore pinned state for bead %s: %v\n", style.Dim.Render("Warning:"), beadID, err)
	} else {
		fmt.Printf("  %s Restored pinned state for bead %s\n", style.Dim.Render("○"), beadID)
	}
}

func rollbackFailedDogDispatch(dispatch *DogDispatchInfo, townRoot, beadID, hookWorkDir, expectedDescription, status, assignee, convoyID string, originalInfo *beadInfo) bool {
	if dispatch == nil {
		return false
	}
	fmt.Printf("%s Dog dispatch did not complete; rolling back %s...\n", style.Warning.Render("⚠"), dispatch.DogName)
	restored := false
	cleared, err := dispatch.ClearWorkIfMatchesAfter(func() bool {
		restored = restoreFailedDogSlingSource(townRoot, beadID, hookWorkDir, dispatch.AgentID, expectedDescription, status, assignee, originalInfo)
		return restored
	})
	if err != nil {
		fmt.Printf("%s Could not clear dog assignment after source restoration: %v\n", style.Dim.Render("Warning:"), err)
		return restored
	}
	if !cleared {
		fmt.Printf("  %s Dog or source assignment changed after dispatch; preserving dog state\n", style.Dim.Render("○"))
		return restored
	}
	clearRolledBackDogAgentHook(dispatch)
	_ = dog.KillCompletedDogSession(
		dog.NewManager(dispatch.townRoot, dispatch.rigsConfig),
		dispatch.DogName,
		fmt.Sprintf("hq-dog-%s", dispatch.DogName),
		tmux.NewTmux().KillSession,
	)
	if convoyID != "" {
		closeConvoy(convoyID, "Sling rollback - dog dispatch failed")
	}
	return restored
}

func clearRolledBackDogAgentHook(dispatch *DogDispatchInfo) {
	if dispatch == nil || dispatch.townRoot == "" || dispatch.DogName == "" {
		return
	}
	empty := ""
	agentBeadID := beads.DogBeadIDTown(dispatch.DogName)
	if err := beads.New(filepath.Join(dispatch.townRoot, ".beads")).UpdateAgentDescriptionFields(agentBeadID, beads.AgentFieldUpdates{HookBead: &empty}); err != nil {
		fmt.Printf("%s Could not clear dog agent hook after rollback: %v\n", style.Dim.Render("Warning:"), err)
	}
}

func restoreFailedDogSlingSource(townRoot, beadID, hookWorkDir, expectedAssignee, expectedDescription, status, assignee string, originalInfo *beadInfo) bool {
	if townRoot == "" || beadID == "" || originalInfo == nil {
		return false
	}
	current, err := getBeadInfoFromTownRoot(townRoot, beadID)
	if err != nil {
		fmt.Printf("  %s Could not verify source bead %s before dog rollback: %v\n", style.Dim.Render("Warning:"), beadID, err)
		return false
	}
	if dogSlingSourceMatchesOriginal(current, originalInfo) {
		// Dispatch failed before changing the source; it is already restored.
		return true
	}
	if !dogSlingSourceOwnedByDispatch(current, expectedAssignee, expectedDescription, originalInfo.Description) {
		fmt.Printf("  %s Source bead %s changed after dispatch; preserving its ownership and metadata\n",
			style.Dim.Render("○"), beadID)
		return false
	}

	if !restoreDogSlingSourceTables(townRoot, beadID, hookWorkDir, status, assignee, originalInfo.Description, current) {
		return false
	}

	if dogSlingSourceRestored(townRoot, beadID, status, assignee, originalInfo.Description) {
		fmt.Printf("  %s Restored source bead %s to status=%s assignee=%s\n", style.Dim.Render("○"), beadID, status, assignee)
		return true
	}
	return false
}

func dogSlingSourceMatchesOriginal(current, original *beadInfo) bool {
	return current != nil && current.Status == original.Status && current.Assignee == original.Assignee &&
		current.Description == original.Description
}

func dogSlingSourceOwnedByDispatch(current *beadInfo, expectedAssignee, expectedDescription, originalDescription string) bool {
	newHookOwned := current != nil && current.Status == "hooked" && current.Assignee == expectedAssignee && current.Description == expectedDescription
	forceTransitionOwned := current != nil && current.Status == "open" && current.Assignee == "" && current.Description == originalDescription
	return newHookOwned || forceTransitionOwned
}

func restoreDogSlingSourceTables(townRoot, beadID, hookWorkDir, status, assignee, description string, current *beadInfo) bool {
	// Restore source ownership and workflow metadata with a storage-level CAS.
	// The description predicate prevents rollback from overwriting any concurrent
	// source edit between readback and update.
	dir := beads.ResolveHookDir(townRoot, beadID, hookWorkDir)
	for _, table := range []string{"issues", "wisps"} {
		query := fmt.Sprintf(
			"UPDATE %s SET status=%s, assignee=%s, description=%s, updated_at=CURRENT_TIMESTAMP WHERE id=%s AND status=%s AND assignee=%s AND description=%s",
			table,
			sqlStringLiteral(status), sqlStringLiteral(assignee), sqlStringLiteral(description),
			sqlStringLiteral(beadID), sqlStringLiteral(current.Status), sqlStringLiteral(current.Assignee), sqlStringLiteral(current.Description),
		)
		if err := BdCmd("sql", query).Dir(dir).StripBeadsDir().WithAutoCommit().Run(); err != nil {
			fmt.Printf("  %s Could not restore source bead %s in %s after dog dispatch failure: %v\n",
				style.Dim.Render("Warning:"), beadID, table, err)
			return false
		}
	}
	return true
}

func dogSlingSourceRestored(townRoot, beadID, status, assignee, description string) bool {
	updated, err := getBeadInfoFromTownRoot(townRoot, beadID)
	if err != nil {
		fmt.Printf("  %s Could not verify source bead %s after dog rollback: %v\n", style.Dim.Render("Warning:"), beadID, err)
		return false
	}
	if updated.Status == status && updated.Assignee == assignee && updated.Description == description {
		return true
	}
	fmt.Printf("  %s Source bead %s changed during rollback; preserved status=%s assignee=%s\n",
		style.Dim.Render("○"), beadID, updated.Status, updated.Assignee)
	return false
}

func clearPreviousDogAssignment(townRoot, assignee, beadID, originalDescription string) error {
	name := strings.TrimPrefix(assignee, "deacon/dogs/")
	rigsConfig, err := config.LoadRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		return err
	}
	mgr := dog.NewManager(townRoot, rigsConfig)
	previous, err := mgr.Get(name)
	if err != nil {
		return err
	}
	fields := beads.ParseAttachmentFields(&beads.Issue{Description: originalDescription})
	if !previousDogAssignmentMatches(previous.Work, beadID, fields) {
		return fmt.Errorf("previous dog %s state names %q, not source %s", name, previous.Work, beadID)
	}
	cleared, err := mgr.ClearWorkIfMatchesAfter(name, previous.Work, previous.WorkStartedAt, func() bool {
		return stopPreviousDogSession(name)
	})
	if err != nil {
		return err
	}
	if !cleared {
		return fmt.Errorf("previous dog %s assignment changed or its session could not stop during handoff", name)
	}
	return nil
}

func previousDogAssignmentMatches(previousWork, beadID string, fields *beads.AttachmentFields) bool {
	return previousWork == beadID || (fields != nil && fields.AttachedFormula != "" && previousWork == fields.AttachedFormula)
}

func stopPreviousDogSession(name string) bool {
	t := tmux.NewTmux()
	running, err := t.HasSession("hq-dog-" + name)
	if err != nil {
		return false
	}
	if !running {
		return true
	}
	return t.KillSessionWithProcesses("hq-dog-"+name) == nil
}

func dogFormulaSourceStillOriginal(townRoot, beadID string, originalInfo *beadInfo) bool {
	if originalInfo == nil {
		return false
	}
	current, err := getBeadInfoFromTownRoot(townRoot, beadID)
	if err != nil || current == nil {
		return false
	}
	return current.Status == originalInfo.Status && current.Assignee == originalInfo.Assignee &&
		current.Description == originalInfo.Description
}

func sqlStringLiteral(value string) string {
	// Dolt uses MySQL string semantics: backslashes are escapes unless the
	// session enables NO_BACKSLASH_ESCAPES. Escape them before SQL quotes.
	value = strings.ReplaceAll(value, `\`, `\\`)
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func tryAcquireSlingBeadLock(townRoot, beadID string) (func(), error) {
	lockDir := filepath.Join(townRoot, ".runtime", "locks", "sling")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("creating sling lock dir: %w", err)
	}

	safeBeadID := strings.NewReplacer("/", "_", ":", "_").Replace(beadID)
	lockPath := filepath.Join(lockDir, safeBeadID+".flock")
	release, locked, err := lock.FlockTryAcquire(lockPath)
	if err != nil {
		return nil, fmt.Errorf("acquiring sling lock for bead %s: %w", beadID, err)
	}
	if !locked {
		return nil, fmt.Errorf("bead %s is already being slung; retry after the current assignment completes", beadID)
	}

	return release, nil
}

// tryAcquireSlingAssigneeLock acquires a per-assignee file lock to serialize concurrent
// hook writes to the same polecat. The per-bead lock (tryAcquireSlingBeadLock) prevents
// double-sling of the same bead, but does not prevent concurrent slings from racing on
// the same assignee's hook_bead field in Dolt. This lock is held only during
// hookBeadWithRetry. Uses non-blocking try-acquire with retry and timeout to avoid
// indefinite blocking if a sling gets stuck.
// See: https://github.com/steveyegge/gastown/issues/3114
func tryAcquireSlingAssigneeLock(townRoot, targetAgent string) (func(), error) {
	lockDir := filepath.Join(townRoot, ".runtime", "locks", "sling")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("creating sling lock dir: %w", err)
	}

	safeAgent := strings.NewReplacer("/", "_", ":", "_").Replace(targetAgent)
	lockPath := filepath.Join(lockDir, "assignee_"+safeAgent+".flock")

	// Try non-blocking acquire with retry. hookBeadWithRetry itself has 10 retries
	// with up to 30s backoff, so we allow generous total wait time for the lock.
	const maxAttempts = 20
	const retryInterval = 500 // milliseconds
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		release, locked, err := lock.FlockTryAcquire(lockPath)
		if err != nil {
			return nil, fmt.Errorf("acquiring assignee sling lock for %s: %w", targetAgent, err)
		}
		if locked {
			return release, nil
		}
		if attempt < maxAttempts {
			time.Sleep(time.Duration(retryInterval) * time.Millisecond)
		}
	}

	return nil, fmt.Errorf("timed out acquiring assignee sling lock for %s after %ds (another sling may be stuck)", targetAgent, maxAttempts*retryInterval/1000)
}

// resolvePRBranch resolves a GitHub PR number to its head branch name via `gh pr view`.
// Used by `gt sling --pr <number>` to convert the PR number into a branch name that
// the polecat worktree can check out.
func resolvePRBranch(prNumber int) (string, error) {
	cmd := exec.Command("gh", "pr", "view", fmt.Sprintf("%d", prNumber), "--json", "headRefName", "-q", ".headRefName")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("gh pr view: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("gh pr view: %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("PR #%d has no headRefName (does it exist?)", prNumber)
	}
	return branch, nil
}

// rollbackSlingArtifacts cleans up artifacts left by a partial sling when session start fails.
// This prevents zombie polecats that block subsequent sling attempts with "bead already hooked".
// Cleanup is best-effort: each step logs warnings but continues to clean as much as possible.
func rollbackSlingArtifacts(spawnInfo *SpawnedPolecatInfo, beadID, hookWorkDir, convoyID string) {
	townRoot, err := workspace.FindFromCwdOrError()
	if beadID != "" {
		rollbackSlingBead(townRoot, beadID, hookWorkDir, err)
	}

	// 3. Clean up the spawned polecat (worktree, agent bead, convoy, etc.)
	cleanupSpawnedPolecat(spawnInfo, spawnInfo.RigName, convoyID)
}

func rollbackSlingBead(townRoot, beadID, hookWorkDir string, workspaceErr error) {
	// 1. Burn any attached molecules from partial formula instantiation.
	// This clears attached_molecule metadata and closes stale wisps that
	// otherwise block subsequent sling attempts.
	// Some failure modes happen before any bead is hooked (e.g., wisp creation fails).
	if workspaceErr != nil {
		fmt.Printf("  %s Could not find workspace to rollback bead %s: %v\n", style.Dim.Render("Warning:"), beadID, workspaceErr)
		return
	}
	info, infoErr := getBeadInfoForRollback(beadID)
	if infoErr != nil {
		fmt.Printf("  %s Could not inspect bead %s for stale molecules: %v\n", style.Dim.Render("Warning:"), beadID, infoErr)
	} else {
		rollbackSlingMolecules(townRoot, beadID, hookWorkDir, info)
	}
	// 2. Unhook the bead (set status back to open so it can be re-slung).
	unhookRolledBackBead(townRoot, beadID, hookWorkDir)
}

func rollbackSlingMolecules(townRoot, beadID, hookWorkDir string, info *beadInfo) {
	existingMolecules := collectExistingMoleculesForRollback(info)
	if depMolecules, depErr := collectExistingMoleculeDeps(beadID, townRoot); depErr != nil {
		fmt.Printf("  %s Could not inspect canonical molecule bonds for %s: %v\n", style.Dim.Render("Warning:"), beadID, depErr)
	} else {
		existingMolecules = appendUniqueMolecules(existingMolecules, depMolecules...)
	}
	if len(existingMolecules) == 0 {
		clearRollbackWorkflowMetadata(beadID, townRoot, hookWorkDir, info)
		return
	}
	if burnErr := burnExistingMoleculesForRollback(existingMolecules, beadID, townRoot); burnErr != nil {
		fmt.Printf("  %s Could not burn stale molecule(s) from %s: %v\n", style.Dim.Render("Warning:"), beadID, burnErr)
		return
	}
	fmt.Printf("  %s Burned %d stale molecule(s): %s\n",
		style.Dim.Render("○"), len(existingMolecules), strings.Join(existingMolecules, ", "))
	refreshed, refreshErr := getBeadInfoForRollback(beadID)
	if refreshErr != nil {
		fmt.Printf("  %s Could not refresh bead %s after molecule cleanup: %v\n", style.Dim.Render("Warning:"), beadID, refreshErr)
		return
	}
	clearRollbackWorkflowMetadata(beadID, townRoot, hookWorkDir, refreshed)
}

func clearRollbackWorkflowMetadata(beadID, townRoot, hookWorkDir string, info *beadInfo) {
	cleared, clearErr := clearRollbackRawWorkflowFields(beadID, townRoot, hookWorkDir, info)
	if clearErr != nil {
		fmt.Printf("  %s Could not clear raw workflow metadata from %s: %v\n", style.Dim.Render("Warning:"), beadID, clearErr)
	} else if cleared {
		fmt.Printf("  %s Cleared raw workflow metadata from %s\n", style.Dim.Render("○"), beadID)
	}
}

func unhookRolledBackBead(townRoot, beadID, hookWorkDir string) {
	unhookDir := beads.ResolveHookDir(townRoot, beadID, hookWorkDir)
	if err := BdCmd("update", beadID, "--status=open", "--assignee=").
		Dir(unhookDir).
		WithAutoCommit().
		Run(); err != nil {
		fmt.Printf("  %s Could not unhook bead %s: %v\n", style.Dim.Render("Warning:"), beadID, err)
	} else {
		fmt.Printf("  %s Unhooked bead %s\n", style.Dim.Render("○"), beadID)
	}
}
