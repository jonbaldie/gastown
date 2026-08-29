package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// Note: Agent field parsing is now in internal/beads/fields.go (AgentFields, ParseAgentFields)

// buildAgentBeadID constructs the agent bead ID from an agent identity.
// Uses canonical naming: prefix-rig-role-name
// Town-level agents use hq- prefix; rig-level agents use rig's prefix.
// Examples:
//   - "mayor" -> "hq-mayor"
//   - "deacon" -> "hq-deacon"
//   - "gastown/witness" -> "gt-gastown-witness"
//   - "gastown/refinery" -> "gt-gastown-refinery"
//   - "gastown/nux" (polecat) -> "gt-gastown-polecat-nux"
//   - "gastown/crew/max" -> "gt-gastown-crew-max"
//
// If role is unknown, it tries to infer from the identity string.
// townRoot is needed to look up the rig's configured prefix.
func buildAgentBeadID(identity string, role Role, townRoot string) string {
	parts := strings.Split(identity, "/")
	if role == RoleUnknown || role == Role("") {
		return inferAgentBeadID(identity, parts, townRoot)
	}
	return explicitAgentBeadID(parts, role, townRoot)
}

func inferAgentBeadID(identity string, parts []string, townRoot string) string {
	if townID := inferTownAgentBeadID(identity); townID != "" {
		return townID
	}
	if len(parts) != 2 && len(parts) != 3 {
		return ""
	}
	return inferRigAgentBeadID(parts, townRoot)
}

func inferTownAgentBeadID(identity string) string {
	switch identity {
	case "mayor":
		return beads.MayorBeadIDTown()
	case "deacon":
		return beads.DeaconBeadIDTown()
	case "deacon-boot":
		return beads.DogBeadIDTown("boot")
	default:
		return ""
	}
}

func inferRigAgentBeadID(parts []string, townRoot string) string {
	prefix := config.GetRigPrefix(townRoot, parts[0])
	switch {
	case len(parts) == 2 && parts[1] == "witness":
		return beads.WitnessBeadIDWithPrefix(prefix, parts[0])
	case len(parts) == 2 && parts[1] == "refinery":
		return beads.RefineryBeadIDWithPrefix(prefix, parts[0])
	case len(parts) == 2:
		return beads.PolecatBeadIDWithPrefix(prefix, parts[0], parts[1])
	case parts[1] == "crew":
		return beads.CrewBeadIDWithPrefix(prefix, parts[0], parts[2])
	case parts[1] == "polecats":
		return beads.PolecatBeadIDWithPrefix(prefix, parts[0], parts[2])
	default:
		return ""
	}
}

func explicitAgentBeadID(parts []string, role Role, townRoot string) string {
	switch role {
	case RoleMayor:
		return beads.MayorBeadIDTown()
	case RoleDeacon:
		return beads.DeaconBeadIDTown()
	case RoleWitness, RoleRefinery, RolePolecat, RoleCrew:
		return rigAgentBeadID(parts, role, townRoot)
	case RoleDog:
		return dogAgentBeadID(parts)
	case RoleBoot:
		return beads.DogBeadIDTown("boot")
	default:
		return ""
	}
}

func rigAgentBeadID(parts []string, role Role, townRoot string) string {
	if len(parts) == 0 {
		return ""
	}
	prefix := config.GetRigPrefix(townRoot, parts[0])
	switch role {
	case RoleWitness:
		return beads.WitnessBeadIDWithPrefix(prefix, parts[0])
	case RoleRefinery:
		return beads.RefineryBeadIDWithPrefix(prefix, parts[0])
	case RolePolecat:
		return polecatAgentBeadID(parts, prefix)
	case RoleCrew:
		return crewAgentBeadID(parts, prefix)
	}
	return ""
}

func polecatAgentBeadID(parts []string, prefix string) string {
	if len(parts) == 3 && parts[1] == "polecats" {
		return beads.PolecatBeadIDWithPrefix(prefix, parts[0], parts[2])
	}
	if len(parts) >= 2 {
		return beads.PolecatBeadIDWithPrefix(prefix, parts[0], parts[1])
	}
	return ""
}

func crewAgentBeadID(parts []string, prefix string) string {
	if len(parts) >= 3 && parts[1] == "crew" {
		return beads.CrewBeadIDWithPrefix(prefix, parts[0], parts[2])
	}
	return ""
}

func dogAgentBeadID(parts []string) string {
	if len(parts) == 3 && parts[0] == "deacon" && parts[1] == "dogs" {
		return beads.DogBeadIDTown(parts[2])
	}
	return ""
}

// MoleculeProgressInfo contains progress information for a molecule instance.
type MoleculeProgressInfo struct {
	RootID       string   `json:"root_id"`
	RootTitle    string   `json:"root_title"`
	MoleculeID   string   `json:"molecule_id,omitempty"`
	TotalSteps   int      `json:"total_steps"`
	DoneSteps    int      `json:"done_steps"`
	InProgress   int      `json:"in_progress_steps"`
	ReadySteps   []string `json:"ready_steps"`
	BlockedSteps []string `json:"blocked_steps"`
	Percent      int      `json:"percent_complete"`
	Complete     bool     `json:"complete"`
}

// MoleculeStatusInfo contains status information for an agent's work.
type MoleculeStatusInfo struct {
	Target           string                `json:"target"`
	Role             string                `json:"role"`
	AgentBeadID      string                `json:"agent_bead_id,omitempty"` // The agent bead if found
	HasWork          bool                  `json:"has_work"`
	PinnedBead       *beads.Issue          `json:"pinned_bead,omitempty"`
	AttachedMolecule string                `json:"attached_molecule,omitempty"`
	AttachedFormula  string                `json:"attached_formula,omitempty"`
	AttachedAt       string                `json:"attached_at,omitempty"`
	AttachedArgs     string                `json:"attached_args,omitempty"`
	AttachedVars     []string              `json:"attached_vars,omitempty"`
	IsWisp           bool                  `json:"is_wisp"`
	Progress         *MoleculeProgressInfo `json:"progress,omitempty"`
	NextAction       string                `json:"next_action,omitempty"`
}

// MoleculeCurrentInfo contains info about what an agent should be working on.
type MoleculeCurrentInfo struct {
	Identity      string `json:"identity"`
	HandoffID     string `json:"handoff_id,omitempty"`
	HandoffTitle  string `json:"handoff_title,omitempty"`
	MoleculeID    string `json:"molecule_id,omitempty"`
	MoleculeTitle string `json:"molecule_title,omitempty"`
	StepsComplete int    `json:"steps_complete"`
	StepsTotal    int    `json:"steps_total"`
	CurrentStepID string `json:"current_step_id,omitempty"`
	CurrentStep   string `json:"current_step,omitempty"`
	Status        string `json:"status"` // "working", "naked", "complete", "blocked"
	Diagnosis     string `json:"diagnosis,omitempty"`
}

func runMoleculeProgress(_ *cobra.Command, args []string) error {
	rootID := args[0]
	workDir, err := findLocalBeadsDir()
	if err != nil {
		return fmt.Errorf("not in a beads workspace: %w", err)
	}
	progress, err := loadMoleculeProgress(beads.New(workDir), rootID)
	if err != nil {
		return err
	}
	if progress == nil {
		return fmt.Errorf("no steps found for %s (not a molecule root?)", rootID)
	}
	if moleculeState().json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(*progress)
	}
	return outputMoleculeProgress(*progress)
}

// extractMoleculeID extracts the molecule ID from an issue's description.
func extractMoleculeID(description string) string {
	lines := strings.Split(description, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "instantiated_from:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "instantiated_from:"))
		}
	}
	return ""
}

func loadMoleculeProgress(b *beads.Beads, rootID string) (*MoleculeProgressInfo, error) {
	root, err := b.Show(rootID)
	if err != nil {
		return nil, fmt.Errorf("getting molecule root: %w", err)
	}
	children, err := b.List(beads.ListOptions{Parent: rootID, Status: "all", Priority: -1})
	if err != nil {
		return nil, fmt.Errorf("listing children: %w", err)
	}
	if len(children) == 0 {
		return nil, nil
	}
	progress := &MoleculeProgressInfo{RootID: rootID, RootTitle: root.Title, MoleculeID: firstMoleculeID(children)}
	closedIDs, openStepIDs := moleculeProgressInputs(children)
	openSteps := loadOpenMoleculeSteps(b, openStepIDs)
	categorizeMoleculeProgress(progress, children, closedIDs, openSteps)
	sortStepIDsBySequence(progress.ReadySteps)
	if progress.TotalSteps > 0 {
		progress.Percent = (progress.DoneSteps * 100) / progress.TotalSteps
	}
	progress.Complete = progress.DoneSteps == progress.TotalSteps
	return progress, nil
}

func firstMoleculeID(children []*beads.Issue) string {
	for _, child := range children {
		if moleculeID := extractMoleculeID(child.Description); moleculeID != "" {
			return moleculeID
		}
	}
	return ""
}

func moleculeProgressInputs(children []*beads.Issue) (map[string]bool, []string) {
	closedIDs := make(map[string]bool)
	var openStepIDs []string
	for _, child := range children {
		switch child.Status {
		case "closed":
			closedIDs[child.ID] = true
		case "open":
			openStepIDs = append(openStepIDs, child.ID)
		}
	}
	return closedIDs, openStepIDs
}

func loadOpenMoleculeSteps(b *beads.Beads, openStepIDs []string) map[string]*beads.Issue {
	if len(openStepIDs) == 0 {
		return nil
	}
	openSteps, err := b.ShowMultiple(openStepIDs)
	if err != nil || openSteps == nil {
		return make(map[string]*beads.Issue)
	}
	return openSteps
}

func categorizeMoleculeProgress(progress *MoleculeProgressInfo, children []*beads.Issue, closedIDs map[string]bool, openSteps map[string]*beads.Issue) {
	for _, child := range children {
		progress.TotalSteps++
		switch child.Status {
		case "closed":
			progress.DoneSteps++
		case "in_progress":
			progress.InProgress++
		case "open":
			if moleculeStepReady(openSteps[child.ID], closedIDs) {
				progress.ReadySteps = append(progress.ReadySteps, child.ID)
			} else {
				progress.BlockedSteps = append(progress.BlockedSteps, child.ID)
			}
		}
	}
}

func moleculeStepReady(step *beads.Issue, closedIDs map[string]bool) bool {
	if step == nil {
		return true
	}
	hasBlockingDeps := false
	for _, dep := range step.Dependencies {
		if !isBlockingDepType(dep.DependencyType) {
			continue
		}
		hasBlockingDeps = true
		if !closedIDs[dep.ID] {
			return false
		}
	}
	return !hasBlockingDeps || allBlockingDependenciesClosed(step, closedIDs)
}

func allBlockingDependenciesClosed(step *beads.Issue, closedIDs map[string]bool) bool {
	for _, dep := range step.Dependencies {
		if isBlockingDepType(dep.DependencyType) && !closedIDs[dep.ID] {
			return false
		}
	}
	return true
}

func outputMoleculeProgress(progress MoleculeProgressInfo) error {
	fmt.Printf("\n%s %s\n\n", style.Bold.Render("🧬 Molecule Progress:"), progress.RootTitle)
	fmt.Printf("  Root: %s\n", progress.RootID)
	if progress.MoleculeID != "" {
		fmt.Printf("  Molecule: %s\n", progress.MoleculeID)
	}
	fmt.Println()
	fmt.Printf("  [%s] %d%% (%d/%d)\n\n", progressBar(progress.Percent, 20), progress.Percent, progress.DoneSteps, progress.TotalSteps)
	fmt.Printf("  Done:        %d\n", progress.DoneSteps)
	fmt.Printf("  In Progress: %d\n", progress.InProgress)
	fmt.Printf("  Ready:       %d", len(progress.ReadySteps))
	if len(progress.ReadySteps) > 0 {
		fmt.Printf(" (%s)", strings.Join(progress.ReadySteps, ", "))
	}
	fmt.Println()
	fmt.Printf("  Blocked:     %d\n", len(progress.BlockedSteps))
	if progress.Complete {
		fmt.Printf("\n  %s\n", style.Bold.Render("✓ Molecule complete!"))
	}
	return nil
}

func progressBar(percent, width int) string {
	filled := (percent * width) / 100
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func runMoleculeStatus(_ *cobra.Command, args []string) error {
	ctx, err := prepareMoleculeStatus(args)
	if err != nil {
		return err
	}
	hookBead := findMoleculeStatusHookWithRetry(&ctx)
	if hookBead != nil {
		applyMoleculeStatusWork(&ctx.status, hookBead, ctx.beads)
	}
	setMoleculeStatusNextAction(&ctx.status)
	return outputMoleculeStatusResult(ctx.status)
}

func findMoleculeStatusHookWithRetry(ctx *moleculeStatusContext) *beads.Issue {
	hookBead := lookupMoleculeStatusHook(ctx.beads, ctx.workDir, ctx.target, ctx.roleCtx.Role, ctx.townRoot, &ctx.status)
	if hookBead == nil && moleculeStatusIsPolecat(ctx.roleCtx) {
		return retryMoleculeStatusHook(ctx.beads, ctx.workDir, ctx.target, ctx.roleCtx.Role, ctx.townRoot, &ctx.status)
	}
	return hookBead
}

type moleculeStatusContext struct {
	townRoot string
	workDir  string
	target   string
	roleCtx  RoleContext
	beads    *beads.Beads
	status   MoleculeStatusInfo
}

func prepareMoleculeStatus(args []string) (moleculeStatusContext, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return moleculeStatusContext{}, fmt.Errorf("getting current directory: %w", err)
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return moleculeStatusContext{}, fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return moleculeStatusContext{}, fmt.Errorf("not in a Gas Town workspace")
	}
	target, roleCtx, validationRole, err := resolveMoleculeStatusTarget(cwd, townRoot, args)
	if err != nil {
		return moleculeStatusContext{}, err
	}
	if err := ensureRoleWorktreeIntegrity(cwd, townRoot, validationRole); err != nil {
		return moleculeStatusContext{}, err
	}
	workDir, err := findLocalBeadsDir()
	if err != nil {
		return moleculeStatusContext{}, fmt.Errorf("not in a beads workspace: %w", err)
	}
	workDir = resolveMoleculeStatusWorkDir(workDir, target, townRoot)
	return moleculeStatusContext{
		townRoot: townRoot,
		workDir:  workDir,
		target:   target,
		roleCtx:  roleCtx,
		beads:    beads.New(workDir),
		status:   MoleculeStatusInfo{Target: target, Role: string(roleCtx.Role)},
	}, nil
}

func resolveMoleculeStatusTarget(cwd, townRoot string, args []string) (string, RoleContext, Role, error) {
	if len(args) > 0 {
		callerCtx := detectRole(cwd, townRoot)
		return normalizeHookShowTarget(args[0]), RoleContext{}, callerCtx.Role, nil
	}
	roleCtx := detectRole(cwd, townRoot)
	if roleCtx.Role == RoleUnknown {
		roleCtx, _ = GetRoleWithContext(cwd, townRoot)
	}
	target := buildAgentIdentity(roleCtx)
	if target == "" {
		return "", RoleContext{}, RoleUnknown, fmt.Errorf("cannot determine agent identity (role: %s)", roleCtx.Role)
	}
	return target, roleCtx, roleCtx.Role, nil
}

func resolveMoleculeStatusWorkDir(workDir, target, townRoot string) string {
	if isTownLevelRole(target) || townRoot == "" {
		return workDir
	}
	return resolveHookLookupWorkDir(workDir, target, townRoot)
}

func lookupMoleculeStatusHook(b *beads.Beads, workDir, target string, role Role, townRoot string, status *MoleculeStatusInfo) *beads.Issue {
	updateMoleculeStatusAgentBeadID(b, workDir, target, role, townRoot, status)
	hookedBeads, err := lookupMoleculeStatusWork(b, target, townRoot)
	if err != nil || len(hookedBeads) == 0 {
		return nil
	}
	return hookedBeads[0]
}

func updateMoleculeStatusAgentBeadID(b *beads.Beads, workDir, target string, role Role, townRoot string, status *MoleculeStatusInfo) {
	agentBeadID := buildAgentBeadID(target, role, townRoot)
	if agentBeadID == "" {
		return
	}
	agentBeadPath := beads.ResolveHookDir(townRoot, agentBeadID, workDir)
	agentBeads := b
	if agentBeadPath != workDir {
		agentBeads = beads.New(agentBeadPath)
	}
	if agentBead, err := agentBeads.Show(agentBeadID); err == nil && beads.IsAgentBead(agentBead) {
		status.AgentBeadID = agentBeadID
	}
}

func lookupMoleculeStatusWork(b *beads.Beads, target, townRoot string) ([]*beads.Issue, error) {
	hookedBeads, err := listAssignedActiveWork(b, target)
	if err != nil {
		return nil, err
	}
	if len(hookedBeads) == 0 && isTownLevelRole(target) {
		hookedBeads = scanAllRigsForHookedBeads(townRoot, target)
	}
	if len(hookedBeads) == 0 && !isTownLevelRole(target) && townRoot != "" {
		townBeads := beads.New(filepath.Join(townRoot, ".beads"))
		if townWork, err := listAssignedActiveWork(townBeads, target); err == nil {
			hookedBeads = townWork
		}
	}
	return hookedBeads, nil
}

func outputMoleculeStatusResult(status MoleculeStatusInfo) error {
	if moleculeState().json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	outputMoleculeStatus(status)
	return nil
}

func moleculeStatusIsPolecat(roleCtx RoleContext) bool {
	if roleCtx.Role == RolePolecat {
		return true
	}
	roleString := os.Getenv("GT_ROLE")
	if roleString == "" {
		return false
	}
	role, _, _ := parseRoleString(roleString)
	return role == RolePolecat
}

func retryMoleculeStatusHook(b *beads.Beads, workDir, target string, role Role, townRoot string, status *MoleculeStatusInfo) *beads.Issue {
	const maxRetries = 5
	const baseBackoff = 500 * time.Millisecond
	const maxBackoff = 8 * time.Second
	for attempt := 1; attempt <= maxRetries; attempt++ {
		time.Sleep(slingBackoff(attempt, baseBackoff, maxBackoff))
		if hook := lookupMoleculeStatusHook(b, workDir, target, role, townRoot, status); hook != nil {
			return hook
		}
	}
	return nil
}

func applyMoleculeStatusWork(status *MoleculeStatusInfo, hookBead *beads.Issue, b *beads.Beads) {
	status.HasWork = true
	status.PinnedBead = hookBead
	attachment := beads.ParseAttachmentFields(hookBead)
	if attachment == nil {
		return
	}
	status.AttachedMolecule = attachment.AttachedMolecule
	status.AttachedFormula = attachment.AttachedFormula
	status.AttachedAt = attachment.AttachedAt
	status.AttachedArgs = attachment.AttachedArgs
	status.AttachedVars = attachment.AttachedVars
	status.IsWisp = strings.Contains(hookBead.Description, "wisp: true") || strings.Contains(hookBead.Description, "is_wisp: true")
	progressID := attachment.AttachedMolecule
	if progressID == "" {
		progressID = hookBead.ID
	}
	if attachment.AttachedMolecule != "" || attachment.AttachedFormula != "" {
		status.Progress, _ = getMoleculeProgressInfo(b, progressID)
		status.NextAction = determineNextAction(*status)
	}
}

func setMoleculeStatusNextAction(status *MoleculeStatusInfo) {
	if !status.HasWork {
		status.NextAction = "Check inbox for work assignments: gt mail inbox"
	} else if status.AttachedMolecule == "" && status.AttachedFormula == "" {
		status.NextAction = "Attach a molecule to start work: gt mol attach <bead-id> <molecule-id>"
	} else if status.AttachedFormula != "" && status.NextAction == "" && status.PinnedBead != nil {
		status.NextAction = "Show the workflow steps: gt prime or bd mol current " + status.PinnedBead.ID
	}
}

// extractRoleFromIdentity extracts the role name from an agent identity string
// for handoff bead lookup. Handles trailing slashes (e.g. "mayor/" → "mayor")
// and compound paths (e.g. "gastown/crew/jack" → "jack").
func extractRoleFromIdentity(target string) string {
	target = strings.TrimRight(target, "/")
	parts := strings.Split(target, "/")
	return parts[len(parts)-1]
}

// buildAgentIdentity constructs the agent identity string from role context.
// Town-level agents (mayor, deacon) use trailing slash to match the format
// used when setting assignee on hooked beads (see resolveSelfTarget in sling.go).
func buildAgentIdentity(ctx RoleContext) string {
	switch ctx.Role {
	case RoleMayor:
		return "mayor/"
	case RoleDeacon:
		return "deacon/"
	case RoleBoot:
		return "deacon/boot"
	case RoleWitness:
		return ctx.Rig + "/witness"
	case RoleRefinery:
		return ctx.Rig + "/refinery"
	case RolePolecat:
		return ctx.Rig + "/polecats/" + ctx.Polecat
	case RoleCrew:
		return ctx.Rig + "/crew/" + ctx.Polecat
	case RoleDog:
		return buildDogIdentity(ctx.Polecat)
	default:
		return ""
	}
}

func buildDogIdentity(name string) string {
	if name == "" {
		return ""
	}
	return "deacon/dogs/" + name
}

// getMoleculeProgressInfo gets progress info for a molecule instance.
func getMoleculeProgressInfo(b *beads.Beads, moleculeRootID string) (*MoleculeProgressInfo, error) {
	return loadMoleculeProgress(b, moleculeRootID)
}

// determineNextAction suggests the next action based on status.
func determineNextAction(status MoleculeStatusInfo) string {
	if status.Progress == nil {
		return ""
	}

	if status.Progress.Complete {
		return "Molecule complete! Close the bead: bd close " + status.PinnedBead.ID
	}

	if status.Progress.InProgress > 0 {
		return "Continue working on in-progress steps"
	}

	if len(status.Progress.ReadySteps) > 0 {
		return fmt.Sprintf("Start next ready step: bd update %s --status=in_progress", status.Progress.ReadySteps[0])
	}

	if len(status.Progress.BlockedSteps) > 0 {
		return "All remaining steps are blocked - waiting on dependencies"
	}

	return ""
}

// outputMoleculeStatus outputs human-readable status.
func outputMoleculeStatus(status MoleculeStatusInfo) {
	outputMoleculeStatusHeader(status)
	if !status.HasWork {
		fmt.Printf("%s\n", style.Dim.Render("Nothing on hook - no work slung"))
		fmt.Printf("\n%s %s\n", style.Bold.Render("Next:"), status.NextAction)
		return
	}

	if status.PinnedBead == nil {
		fmt.Printf("%s\n", style.Dim.Render("Work indicated but no bead found"))
		return
	}

	fmt.Println(style.Bold.Render("🚀 AUTONOMOUS MODE - Work on hook triggers immediate execution"))
	fmt.Println()
	if status.PinnedBead.Status == "closed" {
		outputClosedMoleculeWork(status.PinnedBead)
		return
	}

	if status.PinnedBead.Type == "message" {
		outputMailMoleculeWork(status.PinnedBead)
		return
	}

	outputRegularMoleculeWork(status)
	showGitDivergenceWarning()
	showRecentTrailSummary()
	if status.NextAction != "" {
		fmt.Printf("\n%s %s\n", style.Bold.Render("Next:"), status.NextAction)
	}
}

func outputMoleculeStatusHeader(status MoleculeStatusInfo) {
	fmt.Printf("\n%s Hook Status: %s\n", style.Bold.Render("🪝"), status.Target)
	if status.Role != "" && status.Role != "unknown" {
		fmt.Printf("Role: %s\n", status.Role)
	}
	fmt.Println()
}

func outputClosedMoleculeWork(bead *beads.Issue) {
	fmt.Printf("%s Hooked bead %s is already closed!\n", style.Bold.Render("⚠"), bead.ID)
	fmt.Printf("   Title: %s\n", bead.Title)
	fmt.Printf("   This work was completed elsewhere. Clear your hook with: gt unsling\n")
}

func outputMailMoleculeWork(bead *beads.Issue) {
	sender := extractMailSender(bead.Labels)
	fmt.Printf("%s %s (mail)\n", style.Bold.Render("🪝 Hook:"), bead.ID)
	if sender != "" {
		fmt.Printf("   From: %s\n", sender)
	}
	fmt.Printf("   Subject: %s\n", bead.Title)
	fmt.Printf("   Run: gt mail read %s\n", bead.ID)
}

func outputRegularMoleculeWork(status MoleculeStatusInfo) {
	bead := status.PinnedBead
	fmt.Printf("%s %s: %s\n", style.Bold.Render("🪝 Hooked:"), bead.ID, bead.Title)
	if status.AttachedFormula != "" {
		fmt.Printf("%s %s\n", style.Bold.Render("📐 Formula:"), status.AttachedFormula)
	}
	if len(status.AttachedVars) > 0 {
		fmt.Printf("%s\n", style.Bold.Render("🧩 Vars:"))
		for _, variable := range status.AttachedVars {
			fmt.Printf("   --var %s\n", variable)
		}
	}
	if status.AttachedArgs != "" {
		fmt.Printf("%s %s\n", style.Bold.Render("📋 Args:"), status.AttachedArgs)
	}
	outputAttachedMolecule(status)
	outputMoleculeStatusProgress(status.Progress)
}

func outputAttachedMolecule(status MoleculeStatusInfo) {
	if status.AttachedMolecule != "" {
		molType := "Molecule"
		if status.IsWisp {
			molType = "Wisp"
		}
		fmt.Printf("%s %s: %s\n", style.Bold.Render("🧬 "+molType+":"), status.AttachedMolecule, "")
		if status.AttachedAt != "" {
			fmt.Printf("   Attached: %s\n", status.AttachedAt)
		}
	} else if status.AttachedFormula == "" {
		fmt.Printf("%s\n", style.Dim.Render("No molecule attached (hooked bead still triggers autonomous work)"))
	}
}

func outputMoleculeStatusProgress(progress *MoleculeProgressInfo) {
	if progress == nil {
		return
	}
	fmt.Println()
	fmt.Printf("Progress: [%s] %d%% (%d/%d steps)\n", progressBar(progress.Percent, 20), progress.Percent, progress.DoneSteps, progress.TotalSteps)
	fmt.Printf("  Done:        %d\n", progress.DoneSteps)
	fmt.Printf("  In Progress: %d\n", progress.InProgress)
	fmt.Printf("  Ready:       %d", len(progress.ReadySteps))
	if len(progress.ReadySteps) > 0 && len(progress.ReadySteps) <= 3 {
		fmt.Printf(" (%s)", strings.Join(progress.ReadySteps, ", "))
	}
	fmt.Println()
	fmt.Printf("  Blocked:     %d\n", len(progress.BlockedSteps))
	if progress.Complete {
		fmt.Printf("\n%s\n", style.Bold.Render("✓ Molecule complete!"))
	}
}

// showGitDivergenceWarning fetches from origin and checks if the current branch
// has diverged from its remote tracking branch, showing a warning if so.
func showGitDivergenceWarning() {
	g := git.NewGit(".")
	if !g.IsRepo() {
		return
	}

	branch, err := g.CurrentBranch()
	if err != nil || branch == "" {
		return
	}

	_ = g.Fetch("origin")
	remote, ahead, behind, ok := resolveDivergence(g, branch)
	if !ok || ahead == 0 && behind == 0 {
		return
	}
	printDivergenceWarning(remote, ahead, behind)
}

func resolveDivergence(g *git.Git, branch string) (string, int, int, bool) {
	remote := "origin/" + branch
	ahead, aheadErr := g.CommitsAhead(remote, "HEAD")
	behind, behindErr := g.CountCommitsBehind(remote)
	if aheadErr == nil && behindErr == nil {
		return remote, ahead, behind, true
	}
	remote = "origin/main"
	ahead, aheadErr = g.CommitsAhead(remote, "HEAD")
	behind, behindErr = g.CountCommitsBehind(remote)
	return remote, ahead, behind, aheadErr == nil && behindErr == nil
}

func printDivergenceWarning(remote string, ahead, behind int) {
	fmt.Println()
	if ahead > 0 && behind > 0 {
		fmt.Printf("%s Branch diverged: %d ahead, %d behind %s\n",
			style.Warning.Render("⚠"), ahead, behind, remote)
		fmt.Printf("  Run 'git pull --rebase' before starting work\n")
	} else if behind > 0 {
		fmt.Printf("%s Branch is %d commits behind %s\n",
			style.Warning.Render("⚠"), behind, remote)
		fmt.Printf("  Run 'git pull' to update\n")
	} else {
		fmt.Printf("%s Branch is %d commits ahead of %s (unpushed work)\n",
			style.Dim.Render("ℹ"), ahead, remote)
	}
}

// showRecentTrailSummary shows a compact summary of recent agent activity.
// Leverages git log and beads to show what happened since last activity.
func showRecentTrailSummary() {
	g := git.NewGit(".")
	if !g.IsRepo() {
		return
	}

	// Get recent commits (last 24h) — summarize by author
	since := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	gitArgs := []string{
		"log",
		"--format=%an",
		"--since=" + since,
		"-n50",
		"--all",
	}
	gitCmd := exec.Command("git", gitArgs...)
	output, err := gitCmd.Output()
	if err != nil {
		return
	}

	// Count commits per author
	authorCounts := make(map[string]int)
	totalCommits := 0
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		authorCounts[line]++
		totalCommits++
	}

	if totalCommits == 0 {
		return
	}

	// Build compact author summary (e.g., "3 commits by darcy, 2 by nux")
	type authorCount struct {
		name  string
		count int
	}
	var authors []authorCount
	for name, count := range authorCounts {
		authors = append(authors, authorCount{name, count})
	}
	// Sort by count descending.
	sort.Slice(authors, func(i, j int) bool {
		return authors[i].count > authors[j].count
	})

	var parts []string
	for i, a := range authors {
		if i >= 3 {
			remaining := len(authors) - 3
			parts = append(parts, fmt.Sprintf("+%d others", remaining))
			break
		}
		parts = append(parts, fmt.Sprintf("%d by %s", a.count, a.name))
	}

	fmt.Printf("\n%s Recent (24h): %d commits (%s)\n",
		style.Dim.Render("📍"), totalCommits, strings.Join(parts, ", "))
}

type moleculeCurrentContext struct {
	townRoot    string
	target      string
	beads       *beads.Beads
	lookupBeads *beads.Beads
	handoff     *beads.Issue
	moleculeID  string
	diagnosis   string
}

func runMoleculeCurrent(_ *cobra.Command, args []string) error {
	ctx, err := prepareMoleculeCurrent(args)
	if err != nil {
		return err
	}
	info := MoleculeCurrentInfo{
		Identity:   ctx.target,
		Diagnosis:  ctx.diagnosis,
		MoleculeID: ctx.moleculeID,
	}
	if ctx.handoff != nil {
		info.HandoffID = ctx.handoff.ID
		info.HandoffTitle = ctx.handoff.Title
	}
	if ctx.moleculeID == "" {
		info.Status = "naked"
		return outputMoleculeCurrent(info)
	}
	populateMoleculeCurrentInfo(&info, ctx)
	return outputMoleculeCurrent(info)
}

func prepareMoleculeCurrent(args []string) (moleculeCurrentContext, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return moleculeCurrentContext{}, fmt.Errorf("getting current directory: %w", err)
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return moleculeCurrentContext{}, fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return moleculeCurrentContext{}, fmt.Errorf("not in a Gas Town workspace")
	}
	target, err := resolveMoleculeCurrentTarget(cwd, townRoot, args)
	if err != nil {
		return moleculeCurrentContext{}, err
	}
	workDir, err := findLocalBeadsDir()
	if err != nil {
		return moleculeCurrentContext{}, fmt.Errorf("not in a beads workspace: %w", err)
	}
	if !isTownLevelRole(target) {
		workDir = resolveHookLookupWorkDir(workDir, target, townRoot)
	}
	b := beads.New(workDir)
	lookupBeads, handoff, moleculeID, diagnosis, err := resolveCurrentMoleculeSource(b, townRoot, target)
	if err != nil {
		return moleculeCurrentContext{}, err
	}
	return moleculeCurrentContext{
		townRoot:    townRoot,
		target:      target,
		beads:       b,
		lookupBeads: lookupBeads,
		handoff:     handoff,
		moleculeID:  moleculeID,
		diagnosis:   diagnosis,
	}, nil
}

func resolveMoleculeCurrentTarget(cwd, townRoot string, args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	roleCtx := detectRole(cwd, townRoot)
	if roleCtx.Role == RoleUnknown {
		roleCtx, _ = GetRoleWithContext(cwd, townRoot)
	}
	target := buildAgentIdentity(roleCtx)
	if target == "" {
		return "", fmt.Errorf("cannot determine agent identity (role: %s)", roleCtx.Role)
	}
	return target, nil
}

func populateMoleculeCurrentInfo(info *MoleculeCurrentInfo, ctx moleculeCurrentContext) {
	molRoot, err := ctx.lookupBeads.Show(ctx.moleculeID)
	if err != nil {
		info.Status = "working"
		return
	}
	info.MoleculeTitle = molRoot.Title
	children, err := ctx.lookupBeads.List(beads.ListOptions{
		Parent:   ctx.moleculeID,
		Status:   "all",
		Priority: -1,
	})
	if err != nil {
		info.Status = "working"
		return
	}
	info.StepsTotal = len(children)
	closedIDs, inProgressSteps, openStepIDs := classifyMoleculeCurrentChildren(children, info)
	openSteps := loadMoleculeCurrentOpenSteps(ctx.beads, openStepIDs)
	readySteps := readyMoleculeCurrentSteps(openStepIDs, openSteps, closedIDs)
	sortStepsBySequence(readySteps)
	setMoleculeCurrentStatus(info, inProgressSteps, readySteps)
}

func classifyMoleculeCurrentChildren(children []*beads.Issue, info *MoleculeCurrentInfo) (map[string]bool, []*beads.Issue, []string) {
	closedIDs := make(map[string]bool)
	var inProgressSteps []*beads.Issue
	var openStepIDs []string
	for _, child := range children {
		switch child.Status {
		case "closed":
			info.StepsComplete++
			closedIDs[child.ID] = true
		case "in_progress":
			inProgressSteps = append(inProgressSteps, child)
		case "open":
			openStepIDs = append(openStepIDs, child.ID)
		}
	}
	return closedIDs, inProgressSteps, openStepIDs
}

func loadMoleculeCurrentOpenSteps(b *beads.Beads, openStepIDs []string) map[string]*beads.Issue {
	if len(openStepIDs) == 0 {
		return nil
	}
	openSteps, _ := b.ShowMultiple(openStepIDs)
	if openSteps == nil {
		return make(map[string]*beads.Issue)
	}
	return openSteps
}

func readyMoleculeCurrentSteps(openStepIDs []string, openSteps map[string]*beads.Issue, closedIDs map[string]bool) []*beads.Issue {
	var readySteps []*beads.Issue
	for _, stepID := range openStepIDs {
		step := openSteps[stepID]
		if step != nil && moleculeStepReady(step, closedIDs) {
			readySteps = append(readySteps, step)
		}
	}
	return readySteps
}

func setMoleculeCurrentStatus(info *MoleculeCurrentInfo, inProgressSteps, readySteps []*beads.Issue) {
	switch {
	case info.StepsComplete == info.StepsTotal && info.StepsTotal > 0:
		info.Status = "complete"
	case len(inProgressSteps) > 0:
		info.Status = "working"
		info.CurrentStepID = inProgressSteps[0].ID
		info.CurrentStep = inProgressSteps[0].Title
	case len(readySteps) > 0:
		info.Status = "working"
		info.CurrentStepID = readySteps[0].ID
		info.CurrentStep = readySteps[0].Title
	case info.StepsTotal > 0:
		info.Status = "blocked"
	default:
		info.Status = "working"
	}
}

// outputMoleculeCurrent outputs the current info in the appropriate format.
func outputMoleculeCurrent(info MoleculeCurrentInfo) error {
	if moleculeState().json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	outputMoleculeCurrentText(info)
	return nil
}

func outputMoleculeCurrentText(info MoleculeCurrentInfo) {
	outputMoleculeCurrentHeader(info)
	outputMoleculeCurrentMolecule(info)
	outputMoleculeCurrentProgress(info)
	outputMoleculeCurrentStatus(info)
	if info.Diagnosis != "" {
		fmt.Printf("Diagnose: %s\n", info.Diagnosis)
	}
}

func outputMoleculeCurrentHeader(info MoleculeCurrentInfo) {
	fmt.Printf("Identity: %s\n", info.Identity)
	if info.HandoffID != "" {
		fmt.Printf("Handoff:  %s (%s)\n", info.HandoffID, info.HandoffTitle)
	} else {
		fmt.Printf("Handoff:  %s\n", style.Dim.Render("(none)"))
	}
}

func outputMoleculeCurrentMolecule(info MoleculeCurrentInfo) {
	if info.MoleculeID != "" {
		if info.MoleculeTitle != "" {
			fmt.Printf("Molecule: %s (%s)\n", info.MoleculeID, info.MoleculeTitle)
		} else {
			fmt.Printf("Molecule: %s\n", info.MoleculeID)
		}
	} else {
		fmt.Printf("Molecule: %s\n", style.Dim.Render("(none attached)"))
	}
}

func outputMoleculeCurrentProgress(info MoleculeCurrentInfo) {
	if info.StepsTotal > 0 {
		fmt.Printf("Progress: %d/%d steps complete\n", info.StepsComplete, info.StepsTotal)
	}
}

func outputMoleculeCurrentStatus(info MoleculeCurrentInfo) {
	if info.CurrentStepID != "" {
		fmt.Printf("Current:  %s - %s\n", info.CurrentStepID, info.CurrentStep)
	} else if info.Status == "naked" {
		fmt.Printf("Status:   %s\n", style.Dim.Render("naked - awaiting work assignment"))
	} else if info.Status == "complete" {
		fmt.Printf("Status:   %s\n", style.Bold.Render("complete - molecule finished"))
	} else if info.Status == "blocked" {
		fmt.Printf("Status:   %s\n", style.Dim.Render("blocked - waiting on dependencies"))
	}
}

func attachedMoleculeID(issue *beads.Issue) string {
	if issue == nil {
		return ""
	}
	fields := beads.ParseAttachmentFields(issue)
	if fields == nil {
		return ""
	}
	return strings.TrimSpace(fields.AttachedMolecule)
}

// resolveCurrentMoleculeSource prefers the live Hook over a Handoff bead.
// Patrol uses the same hooked-work lookup; a stale Handoff attachment is never
// reported as current when a live Hook exists. Disagreement is diagnosed.
func resolveCurrentMoleculeSource(b *beads.Beads, townRoot, identity string) (lookupBeads *beads.Beads, handoff *beads.Issue, moleculeID, diagnosis string, err error) {
	role := extractRoleFromIdentity(identity)
	handoff, err = b.FindHandoffBead(role)
	if err != nil {
		return nil, nil, "", "", fmt.Errorf("finding handoff bead: %w", err)
	}
	hook, lookupBeads, err := findCurrentMoleculeHook(b, townRoot, identity)
	if err != nil {
		return nil, handoff, "", "", err
	}
	if hook != nil {
		return lookupBeads, handoff, attachedMoleculeID(hook), moleculeDisagreementDiagnosis(hook, handoff), nil
	}
	if handoffMol := attachedMoleculeID(handoff); handoffMol != "" {
		diagnosis = fmt.Sprintf("stale Handoff attachment %s has no live Hook", handoffMol)
	}
	return b, handoff, "", diagnosis, nil
}

func findCurrentMoleculeHook(b *beads.Beads, townRoot, identity string) (*beads.Issue, *beads.Beads, error) {
	hook, err := lookupLiveHook(b, identity)
	if err != nil {
		return nil, b, fmt.Errorf("finding live hook: %w", err)
	}
	if hook != nil {
		return hook, b, nil
	}
	if isTownLevelRole(identity) {
		hooks := scanAllRigsForHookedBeads(townRoot, identity)
		if len(hooks) > 0 {
			return hooks[0], b, nil
		}
	}
	if isTownLevelRole(identity) || townRoot == "" {
		return nil, b, nil
	}
	townBeads := beads.New(filepath.Join(townRoot, ".beads"))
	hook, err = lookupLiveHook(townBeads, identity)
	if err != nil {
		return nil, b, fmt.Errorf("finding live Hook in Town beads: %w", err)
	}
	if hook == nil {
		return nil, b, nil
	}
	return hook, townBeads, nil
}

func moleculeDisagreementDiagnosis(hook, handoff *beads.Issue) string {
	hookMol := attachedMoleculeID(hook)
	handoffMol := attachedMoleculeID(handoff)
	if handoffMol == "" || hookMol == handoffMol {
		return ""
	}
	if hookMol == "" {
		return fmt.Sprintf("stale Handoff attachment %s disagrees with live Hook %s, which has no attached molecule", handoffMol, hook.ID)
	}
	return fmt.Sprintf("stale Handoff attachment %s disagrees with live Hook molecule %s", handoffMol, hookMol)
}

// isTownLevelRole returns true if the agent ID is a town-level role.
// Town-level roles (Mayor, Deacon) operate from the town root and may have
// pinned beads in any rig's beads directory.
// Accepts both "mayor" and "mayor/" formats for compatibility.
func isTownLevelRole(agentID string) bool {
	return agentID == "mayor" || agentID == "mayor/" ||
		agentID == "deacon" || agentID == "deacon/" ||
		agentID == "deacon/boot" || agentID == "deacon-boot"
}

// extractMailSender extracts the sender from mail bead labels.
// Mail beads have a "from:X" label containing the sender address.
func extractMailSender(labels []string) string {
	for _, label := range labels {
		if strings.HasPrefix(label, "from:") {
			return strings.TrimPrefix(label, "from:")
		}
	}
	return ""
}

// scanAllRigsForHookedBeads scans all registered rigs for hooked beads
// assigned to the target agent. Used for town-level roles that may have
// work hooked in any rig.
func scanAllRigsForHookedBeads(townRoot, target string) []*beads.Issue {
	// Load routes from town beads
	townBeadsDir := filepath.Join(townRoot, ".beads")
	routes, err := beads.LoadRoutes(townBeadsDir)
	if err != nil {
		return nil
	}

	// Scan each rig's beads directory
	for _, route := range routes {
		// Handle both absolute and relative paths in routes.jsonl
		// Go's filepath.Join doesn't replace with absolute paths like Python
		var rigBeadsDir string
		if filepath.IsAbs(route.Path) {
			rigBeadsDir = route.Path
		} else {
			rigBeadsDir = filepath.Join(townRoot, route.Path)
		}
		if _, err := os.Stat(rigBeadsDir); os.IsNotExist(err) {
			continue
		}

		b := beads.New(rigBeadsDir)
		hookedBeads, err := listAssignedActiveWork(b, target)
		if err != nil {
			continue
		}

		if len(hookedBeads) > 0 {
			return hookedBeads
		}
	}

	return nil
}
