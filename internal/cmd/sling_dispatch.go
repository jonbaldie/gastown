package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/nudge"
	"github.com/jonbaldie/gastown/internal/sling"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
)

// SlingResult captures the outcome of one Lifecycle attempt for caller-level tracking.
type SlingResult struct {
	BeadID           string
	PolecatName      string
	SpawnInfo        *SpawnedPolecatInfo
	ConvoyID         string
	Success          bool
	NoOp             bool
	ErrMsg           string
	AttachedMolecule string
}

// slingDestination is the resolved Hook target for one Lifecycle attempt.
// Target resolve and Polecat spawn stay inside this step.
type slingDestination struct {
	Agent             string
	Pane              string
	WorkDir           string
	SpawnInfo         *SpawnedPolecatInfo
	DelayedDog        *DogDispatchInfo
	HookSetAtomically bool
	IsSelfSling       bool
	Admission         *polecatAdmissionHandle
}

// runTownSling is the one Sling sequence owned by Lifecycle.Execute.
// Destination resolve and Polecat spawn are internal steps. Every fatal
// failure after a durable artifact exists uses the same compensation record.
func slingSameTarget(intent sling.Intent, info *beadInfo, townRoot string) (sameTarget, formulaRefresh bool) {
	if intent.RigName != "" {
		return isDefaultRigSlingNoop(intent, info, townRoot), false
	}
	formulaRefresh = intent.Formula != ""
	if intent.Target == "" || intent.Target == "." {
		if sa, _, _, err := resolveSelfTarget(); err == nil {
			return matchesSlingTarget(intent.Target, info.Assignee, sa), formulaRefresh
		}
		return false, formulaRefresh
	}
	return matchesSlingTarget(intent.Target, info.Assignee, ""), formulaRefresh
}

func resolveSlingDestination(ctx context.Context, intent sling.Intent, townRoot string, explicitForce bool) (slingDestination, string, error) {
	if err := ctx.Err(); err != nil {
		return slingDestination{}, "", err
	}
	if intent.RigName != "" {
		return resolveRigSlingDestination(intent, townRoot, explicitForce)
	}
	return resolveNamedSlingDestination(intent, townRoot)
}

func resolveRigSlingDestination(intent sling.Intent, townRoot string, explicitForce bool) (slingDestination, string, error) {
	movedID, err := ensureBeadInTargetRig(intent.BeadID, intent.RigName, townRoot, intent.DryRun)
	if err != nil {
		return slingDestination{}, "", err
	}
	beadID := intent.BeadID
	if movedID != "" {
		beadID = movedID
	}
	if !explicitForce {
		if err := checkCrossRigGuard(beadID, intent.RigName+"/polecats/_", townRoot); err != nil {
			return slingDestination{}, movedID, err
		}
	}
	spawnInfo, err := spawnPolecatForSling(intent.RigName, SlingSpawnOptions{
		TownRoot:     townRoot,
		Force:        intent.Force,
		Account:      intent.Account,
		HookBead:     beadID,
		Agent:        intent.Agent,
		BaseBranch:   intent.BaseBranch,
		ResumeBranch: intent.ResumeBranch,
		Create:       true,
	})
	if err != nil {
		return slingDestination{}, movedID, fmt.Errorf("failed to spawn polecat: %w", err)
	}
	return slingDestination{
		Agent:     spawnInfo.AgentID(),
		WorkDir:   spawnInfo.ClonePath,
		SpawnInfo: spawnInfo,
	}, movedID, nil
}

func resolveNamedSlingDestination(intent sling.Intent, townRoot string) (slingDestination, string, error) {
	resolved, err := resolveTarget(intent.Target, ResolveTargetOptions{
		DryRun:       intent.DryRun,
		Force:        intent.Force,
		Create:       intent.Create,
		Account:      intent.Account,
		Agent:        intent.Agent,
		NoBoot:       intent.NoBoot,
		HookBead:     intent.BeadID,
		BeadID:       intent.BeadID,
		WorkDesc:     intent.Formula,
		TownRoot:     townRoot,
		BaseBranch:   intent.BaseBranch,
		ResumeBranch: intent.ResumeBranch,
	})
	if err != nil {
		return slingDestination{}, "", err
	}
	dest := slingDestination{
		Agent:             resolved.Agent,
		Pane:              resolved.Pane,
		WorkDir:           resolved.WorkDir,
		SpawnInfo:         resolved.NewPolecatInfo,
		DelayedDog:        resolved.DelayedDogInfo,
		HookSetAtomically: resolved.HookSetAtomically,
		IsSelfSling:       resolved.IsSelfSling,
	}
	if err := admitNamedSlingDestination(intent, townRoot, resolved, &dest); err != nil {
		return slingDestination{}, resolved.BeadID, err
	}
	if resolved.BeadID != "" && resolved.BeadID != intent.BeadID {
		return dest, resolved.BeadID, nil
	}
	return dest, "", nil
}

func admitNamedSlingDestination(intent sling.Intent, townRoot string, resolved *ResolvedTarget, dest *slingDestination) error {
	if intent.DryRun || resolved.HookSetAtomically || !strings.Contains(resolved.Agent, "/polecats/") {
		return nil
	}
	parts := strings.Split(resolved.Agent, "/")
	if len(parts) < 3 {
		return nil
	}
	admission, snapshot, err := acquirePolecatAdmissionFn(townRoot, parts[0], resolved.BeadID, "direct-target")
	if err != nil {
		return err
	}
	dest.Admission = admission
	if snapshot.Max > 0 {
		fmt.Printf("%s Polecat capacity reserved (%d free of %d)\n", style.Dim.Render("○"), snapshot.Free, snapshot.Max)
	}
	return nil
}

func forceClearOldHook(ctx context.Context, intent sling.Intent, info *beadInfo, targetAgent, townRoot string) error {
	if !shouldForceClearOldHook(intent, info) {
		return nil
	}
	if intent.RigName == "" {
		fmt.Printf("%s Bead already hooked to %s, forcing reassignment...\n", style.Warning.Render("⚠"), info.Assignee)
	}
	if intent.DryRun {
		fmt.Printf("Would send LIFECYCLE:Shutdown to previous assignee %s\n", info.Assignee)
		fmt.Printf("Would unhook %s from previous assignee\n", intent.BeadID)
		return nil
	}
	notifyForceClearOldHook(intent, info, targetAgent, townRoot)
	unhookDir := beads.ResolveHookDir(townRoot, intent.BeadID, "")
	if err := BdCmd("update", intent.BeadID, "--status=open", "--assignee=").
		Dir(unhookDir).
		WithAutoCommit().
		WithContext(ctx).
		Run(); err != nil {
		return fmt.Errorf("unhook bead %s from old owner: %w", intent.BeadID, err)
	}
	return nil
}

func shouldForceClearOldHook(intent sling.Intent, info *beadInfo) bool {
	return info != nil && (info.Status == "hooked" || info.Status == "in_progress") && intent.Force && info.Assignee != ""
}

func notifyForceClearOldHook(intent sling.Intent, info *beadInfo, targetAgent, townRoot string) {
	assigneeParts := strings.Split(info.Assignee, "/")
	if len(assigneeParts) < 3 || assigneeParts[1] != "polecats" || townRoot == "" {
		return
	}
	newAssignee := targetAgent
	if newAssignee == "" {
		newAssignee = intent.RigName
	}
	router := mail.NewRouter(townRoot)
	msg := &mail.Message{
		From:     forceClearOldHookCaller(intent),
		To:       fmt.Sprintf("%s/witness", assigneeParts[0]),
		Subject:  fmt.Sprintf("LIFECYCLE:Shutdown %s", assigneeParts[2]),
		Body:     fmt.Sprintf("Reason: work_reassigned\nRequestedBy: %s\nBead: %s\nNewAssignee: %s", forceClearOldHookRequester(intent), intent.BeadID, newAssignee),
		Type:     mail.TypeTask,
		Priority: mail.PriorityHigh,
	}
	if err := router.Send(msg); err != nil {
		fmt.Printf("  %s Could not send shutdown to witness: %v\n", style.Dim.Render("Warning:"), err)
	} else {
		fmt.Printf("  %s Sent LIFECYCLE:Shutdown to %s/witness for %s\n", style.Bold.Render("→"), assigneeParts[0], assigneeParts[2])
	}
	mail.WaitPendingNotifications(router)
}

func forceClearOldHookRequester(intent sling.Intent) string {
	if polecat := os.Getenv("GT_POLECAT"); polecat != "" {
		return polecat
	}
	if user := os.Getenv("USER"); user != "" && intent.RigName == "" {
		return user
	}
	if intent.CallerContext != "" {
		return intent.CallerContext
	}
	return "sling"
}

func forceClearOldHookCaller(intent sling.Intent) string {
	if intent.CallerContext != "" {
		return intent.CallerContext
	}
	if intent.RigName == "" {
		return "gt-sling"
	}
	return "sling"
}

func resolveAttemptConvoy(intent sling.Intent, info *beadInfo) (string, bool) {
	if intent.DryRun {
		printAttemptConvoyDryRun(intent, info)
		return intent.Convoy, false
	}
	if intent.Formula != "" && intent.Convoy == "" {
		if intent.NoConvoy {
			return "", false
		}
		existing := isTrackedByConvoy(intent.BeadID)
		if existing != "" {
			fmt.Printf("%s Already tracked by convoy %s\n", style.Dim.Render("○"), existing)
		}
		return existing, false
	}
	return resolveSlingConvoy(intent, info)
}

func printAttemptConvoyDryRun(intent sling.Intent, info *beadInfo) {
	if intent.NoConvoy || intent.Formula != "" || intent.Convoy != "" {
		return
	}
	fmt.Printf("Would create convoy 'Work: %s' if needed\n", info.Title)
	fmt.Printf("Would add tracking relation to %s if needed\n", intent.BeadID)
	if intent.Merge != "" {
		fmt.Printf("Would set convoy merge strategy: %s\n", intent.Merge)
	}
}

func printSlingDryRun(intent sling.Intent, info *beadInfo, formulaName, targetAgent, targetPane string, convoyID string) {
	_ = convoyID
	if formulaName != "" {
		fmt.Printf("Would instantiate formula %s:\n", formulaName)
		fmt.Printf("  1. bd cook %s\n", formulaName)
		fmt.Printf("  2. bd mol bond %s %s --json --ephemeral --var feature=\"%s\" --var issue=\"%s\"\n", formulaName, intent.BeadID, info.Title, intent.BeadID)
		fmt.Printf("  3. bd update %s --status=hooked --assignee=%s\n", intent.BeadID, targetAgent)
	} else {
		fmt.Printf("Would run: bd update %s --status=hooked --assignee=%s\n", intent.BeadID, targetAgent)
	}
	if intent.Subject != "" {
		fmt.Printf("  subject (in nudge): %s\n", intent.Subject)
	}
	if intent.Message != "" {
		fmt.Printf("  context: %s\n", intent.Message)
	}
	if intent.Args != "" {
		fmt.Printf("  args (in nudge): %s\n", intent.Args)
	}
	fmt.Printf("Would inject start prompt to pane: %s\n", targetPane)
}

func nudgeMayorHook(townRoot, beadID string) error {
	message := fmt.Sprintf("Hook updated: attached bead %s", beadID)
	if root, err := workspace.FindFromCwd(); err == nil && root != "" {
		return nudge.Enqueue(root, "hq-mayor", nudge.QueuedNudge{
			Sender:   "sling",
			Message:  message,
			Priority: nudge.PriorityNormal,
		})
	}
	if townRoot != "" {
		return nudge.Enqueue(townRoot, "hq-mayor", nudge.QueuedNudge{
			Sender:   "sling",
			Message:  message,
			Priority: nudge.PriorityNormal,
		})
	}
	return fmt.Errorf("mayor nudge has no town root")
}

// isDefaultRigSlingNoop reports whether this rig dispatch would only repeat
// work already hooked to a live polecat in the same rig. Auto-applied
// mol-polecat-work (or the rig default formula) is not new formula work.
func isDefaultRigSlingNoop(intent sling.Intent, info *beadInfo, townRoot string) bool {
	if intent.RigName == "" || info == nil {
		return false
	}
	if !matchesSlingTarget(intent.RigName, info.Assignee, "") {
		return false
	}
	defaultFormula := resolveFormula("", intent.HookRawBead, townRoot, intent.RigName)
	return intent.Formula == "" || intent.Formula == defaultFormula
}

// findTownRoot is defined in hook.go
