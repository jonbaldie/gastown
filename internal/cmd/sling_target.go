package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/dog"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
)

// spawnPolecatForSling is a seam for tests. Production uses SpawnPolecatForSling.
var spawnPolecatForSling = SpawnPolecatForSling

// resolveTargetAgentFn is a seam for tests. Production uses resolveTargetAgent.
var resolveTargetAgentFn = resolveTargetAgent

// startCrewMemberForSling is a seam for tests. Production uses startCrewMember.
var startCrewMemberForSling = startCrewMember

// resolveTargetAgent converts a target spec to agent ID, pane, and hook root.
func resolveTargetAgent(target string) (agentID string, pane string, hookRoot string, err error) {
	// First resolve to session name
	sessionName, err := resolveRoleToSession(target)
	if err != nil {
		return "", "", "", err
	}

	// Convert session name to agent ID format (this doesn't require tmux)
	agentID = sessionToAgentID(sessionName)

	// Get the pane for that session
	pane, err = getSessionPane(sessionName)
	if err != nil {
		return "", "", "", fmt.Errorf("getting pane for %s: %w", sessionName, err)
	}

	// Get the target's working directory for hook storage
	t := tmux.NewTmux()
	hookRoot, err = t.GetPaneWorkDir(sessionName)
	if err != nil {
		return "", "", "", fmt.Errorf("getting working dir for %s: %w", sessionName, err)
	}

	return agentID, pane, hookRoot, nil
}

// sessionToAgentID converts a session name to agent ID format.
// Uses session.ParseSessionName for consistent parsing across the codebase.
func sessionToAgentID(sessionName string) string {
	identity, err := session.ParseSessionName(sessionName)
	if err != nil {
		// Fallback for unparseable sessions
		return sessionName
	}
	return canonicalAssigneeAddress(identity)
}

// canonicalAssigneeAddress returns the address used for bead assignees and
// hook-status queries. This matches the form emitted by resolveSelfTarget and
// buildAgentIdentity: town-level agents (mayor, deacon) get a trailing slash.
// session.AgentIdentity.Address() returns the bare name for those roles, which
// causes the read/write mismatch in GH#3699.
func canonicalAssigneeAddress(identity *session.AgentIdentity) string {
	addr := identity.Address()
	switch identity.Role {
	case session.RoleMayor, session.RoleDeacon:
		if !strings.HasSuffix(addr, "/") {
			return addr + "/"
		}
	}
	return addr
}

// resolveSelfTarget determines agent identity, pane, and hook root for slinging to self.
func resolveSelfTarget() (agentID string, pane string, hookRoot string, err error) {
	roleInfo, err := GetRole()
	if err != nil {
		return "", "", "", fmt.Errorf("detecting role: %w", err)
	}
	agentID, err = selfTargetAgentID(roleInfo)
	if err != nil {
		return "", "", "", err
	}
	pane = os.Getenv("TMUX_PANE")
	hookRoot, err = selfTargetHookRoot(roleInfo)
	if err != nil {
		return "", "", "", err
	}
	return agentID, pane, hookRoot, nil
}

func selfTargetAgentID(roleInfo RoleInfo) (string, error) {
	builder, ok := selfTargetAgentBuilders[roleInfo.Role]
	if !ok {
		return "", fmt.Errorf("cannot determine agent identity (role: %s)", roleInfo.Role)
	}
	return builder(roleInfo), nil
}

// Town-level agents use trailing slash to match addressToIdentity() normalization.
var selfTargetAgentBuilders = map[Role]func(RoleInfo) string{
	RoleMayor:    func(RoleInfo) string { return "mayor/" },
	RoleDeacon:   func(RoleInfo) string { return "deacon/" },
	RoleBoot:     func(RoleInfo) string { return "deacon/boot" },
	RoleWitness:  func(r RoleInfo) string { return fmt.Sprintf("%s/witness", r.Rig) },
	RoleRefinery: func(r RoleInfo) string { return fmt.Sprintf("%s/refinery", r.Rig) },
	RolePolecat:  func(r RoleInfo) string { return fmt.Sprintf("%s/polecats/%s", r.Rig, r.Polecat) },
	RoleCrew:     func(r RoleInfo) string { return fmt.Sprintf("%s/crew/%s", r.Rig, r.Polecat) },
	RoleDog:      func(r RoleInfo) string { return fmt.Sprintf("deacon/dogs/%s", r.Polecat) },
}

func selfTargetHookRoot(roleInfo RoleInfo) (string, error) {
	if roleInfo.Home != "" {
		return roleInfo.Home, nil
	}
	hookRoot, err := detectCloneRoot()
	if err != nil {
		return "", fmt.Errorf("detecting clone root: %w", err)
	}
	return hookRoot, nil
}

// ResolveTargetOptions controls target resolution behavior.
type ResolveTargetOptions struct {
	DryRun               bool
	Force                bool
	Create               bool
	Account              string
	Agent                string
	NoBoot               bool
	HookBead             string // Bead ID to set atomically during polecat spawn (empty = skip)
	BeadID               string // For cross-rig guard checks (empty = skip guard)
	TownRoot             string
	WorkDesc             string // Description for dog dispatch (defaults to HookBead if empty)
	BaseBranch           string // Override base branch for polecat worktree
	ResumeBranch         string // Existing branch to resume (e.g. PR head); mutually exclusive with BaseBranch
	SkipPolecatAdmission bool   // Caller already holds a capacity reservation
}

// ResolvedTarget holds the results of target resolution.
type ResolvedTarget struct {
	Agent             string
	Pane              string
	WorkDir           string
	HookSetAtomically bool
	DelayedDogInfo    *DogDispatchInfo
	NewPolecatInfo    *SpawnedPolecatInfo
	IsSelfSling       bool
	BeadID            string // May differ from the input when a town bead is moved into the target rig
}

// resolveTarget resolves a target specification to agent, pane, and working directory.
// Handles: "." or empty (self), dog targets, rig targets (auto-spawn polecat),
// existing agents (with dead polecat fallback).
func resolveTarget(target string, opts ResolveTargetOptions) (*ResolvedTarget, error) {
	result := &ResolvedTarget{BeadID: opts.BeadID}
	if target == "" || target == "." {
		return resolveSelfSlingTarget(target, result)
	}
	if dogName, isDog := IsDogTarget(target); isDog {
		return resolveDogSlingTarget(dogName, opts, result)
	}

	if rigName, isRig := IsRigName(target); isRig {
		return resolveRigSlingTarget(rigName, &opts, result)
	}
	return resolveNamedSlingTarget(target, &opts, result)
}

func resolveSelfSlingTarget(target string, result *ResolvedTarget) (*ResolvedTarget, error) {
	agentID, pane, workDir, err := resolveSelfTarget()
	if err != nil {
		if target == "." {
			return nil, fmt.Errorf("resolving self for '.' target: %w", err)
		}
		return nil, err
	}
	result.Agent = agentID
	result.Pane = pane
	result.WorkDir = workDir
	result.IsSelfSling = true
	return result, nil
}

func resolveDogSlingTarget(dogName string, opts ResolveTargetOptions, result *ResolvedTarget) (*ResolvedTarget, error) {
	if opts.DryRun {
		return dryRunDogSlingTarget(dogName, result)
	}
	workDesc := opts.WorkDesc
	if workDesc == "" {
		workDesc = opts.HookBead
	}
	workKind := dog.WorkKindFormula
	if opts.HookBead != "" && workDesc == opts.HookBead {
		workKind = dog.WorkKindBead
	}
	dispatchInfo, err := DispatchToDog(dogName, DogDispatchOptions{
		Create:            opts.Create,
		WorkDesc:          workDesc,
		WorkKind:          workKind,
		DelaySessionStart: true,
		AgentOverride:     opts.Agent,
	})
	if err != nil {
		return nil, fmt.Errorf("dispatching to dog: %w", err)
	}
	result.Agent = dispatchInfo.AgentID
	result.DelayedDogInfo = dispatchInfo
	fmt.Printf("Dispatched to dog %s (session start delayed)\n", dispatchInfo.DogName)
	return result, nil
}

func dryRunDogSlingTarget(dogName string, result *ResolvedTarget) (*ResolvedTarget, error) {
	if dogName == "" {
		fmt.Printf("Would dispatch to idle dog in kennel\n")
		result.Agent = "deacon/dogs/<idle>"
	} else {
		fmt.Printf("Would dispatch to dog '%s'\n", dogName)
		result.Agent = fmt.Sprintf("deacon/dogs/%s", dogName)
	}
	result.Pane = "<dog-pane>"
	return result, nil
}

func resolveRigSlingTarget(rigName string, opts *ResolveTargetOptions, result *ResolvedTarget) (*ResolvedTarget, error) {
	if err := rejectParkedOrDockedRig(opts.TownRoot, rigName); err != nil {
		return nil, err
	}
	if err := moveBeadForSlingTarget(opts, result, rigName); err != nil {
		return nil, err
	}
	if err := guardCrossRigSling(opts, rigName); err != nil {
		return nil, err
	}
	if opts.DryRun {
		fmt.Printf("Would spawn fresh polecat in rig '%s'\n", rigName)
		result.Agent = fmt.Sprintf("%s/polecats/<new>", rigName)
		result.Pane = "<new-pane>"
		return result, nil
	}
	fmt.Printf("Target is rig '%s', spawning fresh polecat...\n", rigName)
	return spawnSlingPolecat(rigName, opts, result, false)
}

func rejectParkedOrDockedRig(townRoot, rigName string) error {
	if townRoot == "" {
		townRoot, _ = workspace.FindFromCwd()
	}
	if townRoot == "" {
		return nil
	}
	blocked, reason := IsRigParkedOrDocked(townRoot, rigName)
	if !blocked {
		return nil
	}
	undoCmd := "gt rig unpark"
	if reason == "docked" {
		undoCmd = "gt rig undock"
	}
	return fmt.Errorf("cannot sling to %s rig %q\n%s %s", reason, rigName, undoCmd, rigName)
}

func moveBeadForSlingTarget(opts *ResolveTargetOptions, result *ResolvedTarget, rigName string) error {
	if opts.BeadID == "" {
		return nil
	}
	originalBeadID := opts.BeadID
	movedID, err := ensureBeadInTargetRig(opts.BeadID, rigName, opts.TownRoot, opts.DryRun)
	if err != nil {
		return err
	}
	opts.BeadID = movedID
	result.BeadID = movedID
	if opts.HookBead == "" || opts.HookBead == originalBeadID {
		opts.HookBead = movedID
	}
	return nil
}

func guardCrossRigSling(opts *ResolveTargetOptions, rigName string) error {
	if opts.BeadID == "" || opts.Force {
		return nil
	}
	return checkCrossRigGuard(opts.BeadID, rigName+"/polecats/_", opts.TownRoot)
}

func spawnSlingPolecat(rigName string, opts *ResolveTargetOptions, result *ResolvedTarget, replaceDead bool) (*ResolvedTarget, error) {
	spawnInfo, err := spawnPolecatForSling(rigName, SlingSpawnOptions{
		TownRoot:      opts.TownRoot,
		Force:         opts.Force,
		Account:       opts.Account,
		Create:        opts.Create,
		HookBead:      opts.HookBead,
		Agent:         opts.Agent,
		BaseBranch:    opts.BaseBranch,
		ResumeBranch:  opts.ResumeBranch,
		SkipAdmission: opts.SkipPolecatAdmission,
	})
	if err != nil {
		if replaceDead {
			return nil, fmt.Errorf("spawning polecat to replace dead polecat: %w", err)
		}
		return nil, fmt.Errorf("spawning polecat: %w", err)
	}
	result.Agent = spawnInfo.AgentID()
	result.NewPolecatInfo = spawnInfo
	result.WorkDir = spawnInfo.ClonePath
	result.HookSetAtomically = opts.HookBead != ""
	if !opts.NoBoot {
		wakeRigAgents(rigName)
	}
	return result, nil
}

func resolveNamedSlingTarget(target string, opts *ResolveTargetOptions, result *ResolvedTarget) (*ResolvedTarget, error) {
	// Existing agent (with dead polecat fallback).
	// Uses resolveTargetAgentFn seam — crew, mayor, and all existing agents
	// resolve here, getting their pane for nudge delivery (gt-in7b).
	agentID, pane, workDir, err := resolveTargetAgentFn(target)
	if err != nil {
		return resolveFallbackSlingTarget(target, opts, result, err)
	}
	return finishResolvedAgentTarget(agentID, pane, workDir, opts, result)
}

func resolveFallbackSlingTarget(target string, opts *ResolveTargetOptions, result *ResolvedTarget, resolveErr error) (*ResolvedTarget, error) {
	if rigName, crewName, crewDir, ok := stoppedCrewTarget(target, opts.TownRoot); ok {
		return startStoppedCrewSlingTarget(target, rigName, crewName, crewDir, opts, result)
	}
	if rigName, ok := missingPolecatTargetRig(target, opts.Create, opts.TownRoot); ok {
		return spawnReplacementPolecatSlingTarget(rigName, opts, result)
	}
	return nil, fmt.Errorf("resolving target: %w", resolveErr)
}

func startStoppedCrewSlingTarget(target, rigName, crewName, crewDir string, opts *ResolveTargetOptions, result *ResolvedTarget) (*ResolvedTarget, error) {
	if opts.DryRun {
		fmt.Printf("Would start stopped crew member '%s/crew/%s'\n", rigName, crewName)
		result.Agent = fmt.Sprintf("%s/crew/%s", rigName, crewName)
		result.Pane = "<crew-pane>"
		result.WorkDir = crewDir
		return result, nil
	}
	fmt.Printf("Target crew member has no active session, starting '%s/crew/%s'...\n", rigName, crewName)
	townRoot := opts.TownRoot
	if townRoot == "" {
		townRoot = filepath.Dir(filepath.Dir(filepath.Dir(crewDir)))
	}
	if startErr := startCrewMemberForSling(rigName, crewName, townRoot); startErr != nil {
		return nil, fmt.Errorf("starting stopped crew member: %w", startErr)
	}
	agentID, pane, workDir, err := resolveTargetAgentFn(target)
	if err != nil {
		return nil, fmt.Errorf("resolving target after starting crew member: %w", err)
	}
	return finishResolvedAgentTarget(agentID, pane, workDir, opts, result)
}

func spawnReplacementPolecatSlingTarget(rigName string, opts *ResolveTargetOptions, result *ResolvedTarget) (*ResolvedTarget, error) {
	if err := moveBeadForSlingTarget(opts, result, rigName); err != nil {
		return nil, err
	}
	if err := guardCrossRigSling(opts, rigName); err != nil {
		return nil, err
	}
	fmt.Printf("Target polecat has no active session, spawning fresh polecat in rig '%s'...\n", rigName)
	return spawnSlingPolecat(rigName, opts, result, true)
}

func finishResolvedAgentTarget(agentID, pane, workDir string, opts *ResolveTargetOptions, result *ResolvedTarget) (*ResolvedTarget, error) {
	if err := moveBeadForExistingPolecat(opts, result, agentID); err != nil {
		return nil, err
	}
	result.Agent = agentID
	result.Pane = pane
	result.WorkDir = workDir
	// Detect self-sling by pane: a named target (e.g. "deacon") that resolves to
	// the caller's own tmux pane should not inject the ack prompt — the caller is
	// already running and knows about the hook (GH#3839).
	if pane != "" && pane == os.Getenv("TMUX_PANE") {
		result.IsSelfSling = true
	}
	return result, nil
}

func moveBeadForExistingPolecat(opts *ResolveTargetOptions, result *ResolvedTarget, agentID string) error {
	if opts.BeadID == "" || !isPolecatTarget(agentID) {
		return nil
	}
	parts := strings.Split(agentID, "/")
	if len(parts) < 3 || parts[1] != "polecats" {
		return nil
	}
	return moveBeadForSlingTarget(opts, result, parts[0])
}

func stoppedCrewTarget(target, townRoot string) (rigName, crewName, crewDir string, ok bool) {
	parts := strings.Split(target, "/")
	switch {
	case len(parts) == 3 && parts[1] == constants.RoleCrew:
		rigName, crewName = parts[0], parts[2]
	case len(parts) == 2 && !knownRoles[strings.ToLower(parts[1])]:
		rigName, crewName = parts[0], parts[1]
	default:
		return "", "", "", false
	}
	if townRoot == "" {
		townRoot = detectTownRootFromCwd()
	}
	if townRoot == "" {
		return "", "", "", false
	}
	crewDir = filepath.Join(townRoot, rigName, "crew", crewName)
	if info, err := os.Stat(crewDir); err != nil || !info.IsDir() {
		return "", "", "", false
	}
	return rigName, crewName, crewDir, true
}

func missingPolecatTargetRig(target string, allowShorthand bool, townRoot string) (string, bool) {
	if isPolecatTarget(target) {
		parts := strings.Split(target, "/")
		return parts[0], true
	}
	if !allowShorthand {
		return "", false
	}
	parts := strings.Split(target, "/")
	if len(parts) != 2 || knownRoles[strings.ToLower(parts[1])] {
		return "", false
	}
	if townRoot == "" {
		townRoot = detectTownRootFromCwd()
	}
	if townRoot != "" {
		if info, err := os.Stat(filepath.Join(townRoot, parts[0], "crew", parts[1])); err == nil && info.IsDir() {
			return "", false
		}
	}
	return parts[0], true
}
