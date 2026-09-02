package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/spf13/cobra"
)

var polecatIdentityCmd = &cobra.Command{
	Use:     "identity",
	Aliases: []string{"id"},
	Short:   "Manage polecat identities",
	Long: `Manage polecat identity beads in rigs.

Identity beads track polecat metadata, CV history, and lifecycle state.
Use subcommands to create, list, show, rename, or remove identities.`,
	RunE: requireSubcommand,
}

var polecatIdentityAddCmd = &cobra.Command{
	Use:   "add <rig> [name]",
	Short: "Create an identity bead for a polecat",
	Long: `Create an identity bead for a polecat in a rig.

If name is not provided, a name will be generated from the rig's name pool.

The identity bead tracks:
  - Role type (polecat)
  - Rig assignment
  - Agent state
  - Hook bead (current work)
  - Cleanup status

Example:
  gt polecat identity add gastown Toast
  gt polecat identity add gastown  # auto-generate name`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runPolecatIdentityAdd,
}

var polecatIdentityListCmd = &cobra.Command{
	Use:   "list <rig>",
	Short: "List polecat identity beads in a rig",
	Long: `List all polecat identity beads in a rig.

Shows:
  - Polecat name
  - Agent state
  - Current hook (if any)
  - Whether worktree exists

Example:
  gt polecat identity list gastown
  gt polecat identity list gastown --json`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatIdentityList,
}

var polecatIdentityShowCmd = &cobra.Command{
	Use:   "show <rig> <name>",
	Short: "Show polecat identity with CV summary",
	Long: `Show detailed identity information for a polecat including work history.

Displays:
  - Identity bead ID and creation date
  - Session count
  - Completion statistics (issues completed, failed, abandoned)
  - Language breakdown from file extensions
  - Work type breakdown (feat, fix, refactor, etc.)
  - Recent work list with relative timestamps

Examples:
  gt polecat identity show gastown Toast
  gt polecat identity show gastown Toast --json`,
	Args: cobra.ExactArgs(2),
	RunE: runPolecatIdentityShow,
}

var polecatIdentityRenameCmd = &cobra.Command{
	Use:   "rename <rig> <old-name> <new-name>",
	Short: "Rename a polecat identity (preserves CV)",
	Long: `Rename a polecat identity bead, preserving CV history.

The rename:
  1. Creates a new identity bead with the new name
  2. Copies CV history links to the new bead
  3. Closes the old bead with a reference to the new one

Safety checks:
  - Old identity must exist
  - New name must not already exist
  - Polecat session must not be running

Example:
  gt polecat identity rename gastown Toast Imperator`,
	Args: cobra.ExactArgs(3),
	RunE: runPolecatIdentityRename,
}

var polecatIdentityRemoveCmd = &cobra.Command{
	Use:   "remove <rig> <name>",
	Short: "Remove a polecat identity",
	Long: `Remove a polecat identity bead.

Safety checks:
  - No active tmux session
  - No work on hook (unless using --force)
  - Warns if CV exists

Use --force to bypass safety checks.

Example:
  gt polecat identity remove gastown Toast
  gt polecat identity remove gastown Toast --force`,
	Args: cobra.ExactArgs(2),
	RunE: runPolecatIdentityRemove,
}

func init() {
	// List flags
	polecatIdentityListCmd.Flags().Bool("json", false, "Output as JSON")

	// Show flags
	polecatIdentityShowCmd.Flags().Bool("json", false, "Output as JSON")

	// Remove flags
	polecatIdentityRemoveCmd.Flags().BoolP("force", "f", false, "Force removal, bypassing safety checks")

	// Add subcommands to identity
	polecatIdentityCmd.AddCommand(polecatIdentityAddCmd)
	polecatIdentityCmd.AddCommand(polecatIdentityListCmd)
	polecatIdentityCmd.AddCommand(polecatIdentityShowCmd)
	polecatIdentityCmd.AddCommand(polecatIdentityRenameCmd)
	polecatIdentityCmd.AddCommand(polecatIdentityRemoveCmd)

	// Add identity to polecat command
	polecatCmd.AddCommand(polecatIdentityCmd)
}

// IdentityInfo holds identity bead information for display.
type IdentityInfo struct {
	Rig            string `json:"rig"`
	Name           string `json:"name"`
	BeadID         string `json:"bead_id"`
	AgentState     string `json:"agent_state,omitempty"`
	HookBead       string `json:"hook_bead,omitempty"`
	CleanupStatus  string `json:"cleanup_status,omitempty"`
	WorktreeExists bool   `json:"worktree_exists"`
	SessionRunning bool   `json:"session_running"`
}

// IdentityDetails holds detailed identity information for show command.
type IdentityDetails struct {
	IdentityInfo
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
	CVBeads     []string `json:"cv_beads,omitempty"`
}

// CVSummary represents the CV/work history summary for a polecat.
type CVSummary struct {
	Identity         string           `json:"identity"`
	Created          string           `json:"created,omitempty"`
	Sessions         int              `json:"sessions"`
	IssuesCompleted  int              `json:"issues_completed"`
	IssuesFailed     int              `json:"issues_failed"`
	IssuesAbandoned  int              `json:"issues_abandoned"`
	Languages        map[string]int   `json:"languages,omitempty"`
	WorkTypes        map[string]int   `json:"work_types,omitempty"`
	AvgCompletionMin int              `json:"avg_completion_minutes,omitempty"`
	FirstPassRate    float64          `json:"first_pass_rate,omitempty"`
	RecentWork       []RecentWorkItem `json:"recent_work,omitempty"`
}

// RecentWorkItem represents a recent work item in the CV.
type RecentWorkItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Type      string `json:"type,omitempty"`
	Completed string `json:"completed"`
	Ago       string `json:"ago"`
}

func runPolecatIdentityAdd(_ *cobra.Command, args []string) error {
	rigName := args[0]
	var polecatName string

	if len(args) > 1 {
		polecatName = args[1]
		if err := polecat.ValidateNewPolecatName(polecatName); err != nil {
			return err
		}
	}

	// Get rig
	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	// Generate name if not provided
	if polecatName == "" {
		polecatGit := git.NewGit(r.Path)
		t := tmux.NewTmux()
		mgr := polecat.NewManager(r, polecatGit, t)
		polecatName, err = polecat.AllocateName(mgr)
		if err != nil {
			return fmt.Errorf("generating polecat name: %w", err)
		}
		fmt.Printf("Generated name: %s\n", polecatName)
	}

	// Check if identity already exists
	bd := beads.New(r.Path)
	beadID := polecatBeadIDForRig(r, rigName, polecatName)
	existingIssue, _, _ := bd.GetAgentBead(beadID)
	if existingIssue != nil && existingIssue.Status != "closed" {
		return fmt.Errorf("identity bead %s already exists", beadID)
	}

	// Create identity bead
	fields := &beads.AgentFields{
		RoleType:   "polecat",
		Rig:        rigName,
		AgentState: "idle",
	}

	title := fmt.Sprintf("Polecat %s in %s", polecatName, rigName)
	issue, err := bd.CreateOrReopenAgentBead(beadID, title, fields)
	if err != nil {
		return fmt.Errorf("creating identity bead: %w", err)
	}

	fmt.Printf("%s Created identity bead: %s\n", style.SuccessPrefix, issue.ID)
	fmt.Printf("  Polecat: %s\n", polecatName)
	fmt.Printf("  Rig:     %s\n", rigName)

	return nil
}

func runPolecatIdentityList(cmd *cobra.Command, args []string) error {
	identities, err := collectPolecatIdentities(args[0])
	if err != nil {
		return err
	}
	return printPolecatIdentityList(args[0], identities, commandBoolFlag(cmd, "json"))
}

func collectPolecatIdentities(rigName string) ([]IdentityInfo, error) {
	_, r, err := getRig(rigName)
	if err != nil {
		return nil, err
	}
	agentBeads, err := beads.New(r.Path).ListAgentBeads()
	if err != nil {
		return nil, fmt.Errorf("listing agent beads: %w", err)
	}
	t := tmux.NewTmux()
	polecatMgr := polecat.NewSessionManager(t, r)
	mgr := polecat.NewManager(r, nil, t)
	identities := []IdentityInfo{}
	for id, issue := range agentBeads {
		info, ok := polecatIdentityFromAgentBead(rigName, id, issue, mgr, polecatMgr)
		if ok {
			identities = append(identities, info)
		}
	}
	return identities, nil
}

func polecatIdentityFromAgentBead(rigName, id string, issue *beads.Issue, mgr *polecat.Manager, polecatMgr *polecat.SessionManager) (IdentityInfo, bool) {
	parsed, ok := beads.ParseAgentBeadID(id)
	if !ok || parsed.Role != constants.RolePolecat || parsed.Rig != rigName || issue.Status == "closed" {
		return IdentityInfo{}, false
	}
	fields := beads.ParseAgentFields(issue.Description)
	worktreeExists := false
	if p, err := polecat.Get(mgr, parsed.Name); err == nil && p != nil {
		worktreeExists = true
	}
	sessionRunning, _ := polecatMgr.IsRunning(parsed.Name)
	info := IdentityInfo{
		Rig:            rigName,
		Name:           parsed.Name,
		BeadID:         id,
		AgentState:     fields.AgentState,
		HookBead:       issue.HookBead,
		CleanupStatus:  fields.CleanupStatus,
		WorktreeExists: worktreeExists,
		SessionRunning: sessionRunning,
	}
	if info.HookBead == "" {
		info.HookBead = fields.HookBead
	}
	return info, true
}

func printPolecatIdentityList(rigName string, identities []IdentityInfo, jsonOutput bool) error {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(identities)
	}
	if len(identities) == 0 {
		fmt.Printf("No polecat identities found in %s.\n", rigName)
		return nil
	}
	fmt.Printf("%s\n\n", style.Bold.Render(fmt.Sprintf("Polecat Identities in %s", rigName)))
	for _, info := range identities {
		printPolecatIdentityListRow(info)
	}
	fmt.Printf("\n%d identity bead(s)\n", len(identities))
	return nil
}

func printPolecatIdentityListRow(info IdentityInfo) {
	sessionIcon := style.Dim.Render("○")
	if info.SessionRunning {
		sessionIcon = style.Success.Render("●")
	}
	worktreeIcon := ""
	if info.WorktreeExists {
		worktreeIcon = " " + style.Dim.Render("[worktree]")
	}
	fmt.Printf("  %s %s  %s%s\n", sessionIcon, style.Bold.Render(info.Name), polecatIdentityStateStyle(info.AgentState), worktreeIcon)
	if info.HookBead != "" {
		fmt.Printf("    Hook: %s\n", style.Dim.Render(info.HookBead))
	}
}

func polecatIdentityStateStyle(stateStr string) string {
	if stateStr == "" {
		stateStr = "unknown"
	}
	switch stateStr {
	case "working":
		return style.Info.Render(stateStr)
	case "done":
		return style.Success.Render(stateStr)
	case "stuck":
		return style.Warning.Render(stateStr)
	default:
		return style.Dim.Render(stateStr)
	}
}

func runPolecatIdentityShow(cmd *cobra.Command, args []string) error {
	jsonOutput := commandBoolFlag(cmd, "json")
	details, err := loadPolecatIdentityShow(args[0], args[1])
	if err != nil {
		return err
	}
	if jsonOutput {
		return printPolecatIdentityShowJSON(details)
	}
	printPolecatIdentityShowHuman(details)
	return nil
}

type polecatIdentityShow struct {
	rigName        string
	polecatName    string
	beadID         string
	issue          *beads.Issue
	fields         *beads.AgentFields
	worktreeExists bool
	clonePath      string
	sessionRunning bool
	cv             *CVSummary
}

func loadPolecatIdentityShow(rigName, polecatName string) (*polecatIdentityShow, error) {
	_, r, err := getRig(rigName)
	if err != nil {
		return nil, err
	}
	beadID := polecatBeadIDForRig(r, rigName, polecatName)
	issue, fields, err := beads.New(r.Path).GetAgentBead(beadID)
	if err != nil {
		return nil, fmt.Errorf("getting identity bead: %w", err)
	}
	if issue == nil {
		return nil, fmt.Errorf("identity bead %s not found", beadID)
	}
	t := tmux.NewTmux()
	details := &polecatIdentityShow{
		rigName:     rigName,
		polecatName: polecatName,
		beadID:      beadID,
		issue:       issue,
		fields:      fields,
	}
	if p, err := polecat.Get(polecat.NewManager(r, nil, t), polecatName); err == nil && p != nil {
		details.worktreeExists = true
		details.clonePath = p.ClonePath
	}
	details.sessionRunning, _ = polecat.NewSessionManager(t, r).IsRunning(polecatName)
	details.cv = buildCVSummary(r.Path, rigName, polecatName, beadID, details.clonePath)
	return details, nil
}

func printPolecatIdentityShowJSON(d *polecatIdentityShow) error {
	output := struct {
		IdentityInfo
		Title     string     `json:"title"`
		CreatedAt string     `json:"created_at,omitempty"`
		UpdatedAt string     `json:"updated_at,omitempty"`
		CV        *CVSummary `json:"cv,omitempty"`
	}{
		IdentityInfo: IdentityInfo{
			Rig:            d.rigName,
			Name:           d.polecatName,
			BeadID:         d.beadID,
			AgentState:     d.fields.AgentState,
			HookBead:       d.issue.HookBead,
			CleanupStatus:  d.fields.CleanupStatus,
			WorktreeExists: d.worktreeExists,
			SessionRunning: d.sessionRunning,
		},
		Title:     d.issue.Title,
		CreatedAt: d.issue.CreatedAt,
		UpdatedAt: d.issue.UpdatedAt,
		CV:        d.cv,
	}
	if output.HookBead == "" {
		output.HookBead = d.fields.HookBead
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

func printPolecatIdentityShowHuman(d *polecatIdentityShow) {
	fmt.Printf("\n%s %s/%s\n", style.Bold.Render("Identity:"), d.rigName, d.polecatName)
	fmt.Printf("  Bead ID:       %s\n", d.beadID)
	fmt.Printf("  Title:         %s\n", d.issue.Title)
	sessionStr := style.Dim.Render("stopped")
	if d.sessionRunning {
		sessionStr = style.Success.Render("running")
	}
	fmt.Printf("  Session:       %s\n", sessionStr)
	worktreeStr := style.Dim.Render("no")
	if d.worktreeExists {
		worktreeStr = style.Success.Render("yes")
	}
	fmt.Printf("  Worktree:      %s\n", worktreeStr)
	fmt.Printf("  Agent State:   %s\n", polecatIdentityStateStyle(d.fields.AgentState))
	hookBead := d.issue.HookBead
	if hookBead == "" {
		hookBead = d.fields.HookBead
	}
	if hookBead != "" {
		fmt.Printf("  Hook:          %s\n", hookBead)
	} else {
		fmt.Printf("  Hook:          %s\n", style.Dim.Render("(empty)"))
	}
	if d.fields.CleanupStatus != "" {
		fmt.Printf("  Cleanup:       %s\n", d.fields.CleanupStatus)
	}
	if d.issue.CreatedAt != "" {
		fmt.Printf("  Created:       %s\n", style.Dim.Render(d.issue.CreatedAt))
	}
	if d.issue.UpdatedAt != "" {
		fmt.Printf("  Updated:       %s\n", style.Dim.Render(d.issue.UpdatedAt))
	}
	printPolecatIdentityCV(d.cv)
}

func printPolecatIdentityCV(cv *CVSummary) {
	fmt.Printf("\n%s\n", style.Bold.Render("CV Summary:"))
	fmt.Printf("  Sessions:         %d\n", cv.Sessions)
	fmt.Printf("  Issues completed: %s\n", style.Success.Render(fmt.Sprintf("%d", cv.IssuesCompleted)))
	fmt.Printf("  Issues failed:    %s\n", formatCountStyled(cv.IssuesFailed, style.Error))
	fmt.Printf("  Issues abandoned: %s\n", formatCountStyled(cv.IssuesAbandoned, style.Warning))
	if len(cv.Languages) > 0 {
		fmt.Printf("\n  %s %s\n", style.Bold.Render("Languages:"), formatLanguageStats(cv.Languages))
	}
	if len(cv.WorkTypes) > 0 {
		fmt.Printf("  %s     %s\n", style.Bold.Render("Types:"), formatWorkTypeStats(cv.WorkTypes))
	}
	if cv.AvgCompletionMin > 0 {
		fmt.Printf("\n  Avg completion time: %d minutes\n", cv.AvgCompletionMin)
	}
	if cv.FirstPassRate > 0 {
		fmt.Printf("  First-pass success:  %.0f%%\n", cv.FirstPassRate*100)
	}
	printPolecatIdentityRecentWork(cv.RecentWork)
	fmt.Println()
}

func printPolecatIdentityRecentWork(recent []RecentWorkItem) {
	if len(recent) == 0 {
		return
	}
	fmt.Printf("\n%s\n", style.Bold.Render("Recent work:"))
	for _, work := range recent {
		typeStr := ""
		if work.Type != "" {
			typeStr = work.Type + ": "
		}
		title := work.Title
		if len(title) > 40 {
			title = title[:37] + "..."
		}
		fmt.Printf("  %-10s %s%s  %s\n", work.ID, typeStr, title, style.Dim.Render(work.Ago))
	}
}

func runPolecatIdentityRename(_ *cobra.Command, args []string) error {
	rigName, oldName, newName := args[0], args[1], args[2]
	if err := validatePolecatIdentityRename(oldName, newName); err != nil {
		return err
	}
	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}
	bd := beads.New(r.Path)
	oldBeadID := polecatBeadIDForRig(r, rigName, oldName)
	newBeadID := polecatBeadIDForRig(r, rigName, newName)
	_, oldFields, err := loadRenameSourceBead(bd, oldBeadID)
	if err != nil {
		return err
	}
	if err := rejectRenameDestinationBead(bd, newBeadID); err != nil {
		return err
	}
	if err := rejectRunningPolecatSession(polecat.NewSessionManager(tmux.NewTmux(), r), oldName); err != nil {
		return err
	}
	if err := applyPolecatIdentityRename(bd, rigName, oldBeadID, newBeadID, newName, oldFields); err != nil {
		return err
	}
	printPolecatIdentityRename(oldBeadID, newBeadID, oldName)
	return nil
}

func validatePolecatIdentityRename(oldName, newName string) error {
	if oldName == newName {
		return fmt.Errorf("old and new names are the same")
	}
	return polecat.ValidateNewPolecatName(newName)
}

func loadRenameSourceBead(bd *beads.Beads, oldBeadID string) (*beads.Issue, *beads.AgentFields, error) {
	oldIssue, oldFields, err := bd.GetAgentBead(oldBeadID)
	if err != nil {
		return nil, nil, fmt.Errorf("getting old identity bead: %w", err)
	}
	if oldIssue == nil || oldIssue.Status == "closed" {
		return nil, nil, fmt.Errorf("identity bead %s not found or already closed", oldBeadID)
	}
	return oldIssue, oldFields, nil
}

func rejectRenameDestinationBead(bd *beads.Beads, newBeadID string) error {
	newIssue, _, _ := bd.GetAgentBead(newBeadID)
	if newIssue != nil && newIssue.Status != "closed" {
		return fmt.Errorf("identity bead %s already exists", newBeadID)
	}
	return nil
}

func rejectRunningPolecatSession(polecatMgr *polecat.SessionManager, name string) error {
	running, _ := polecatMgr.IsRunning(name)
	if running {
		return fmt.Errorf("cannot rename: polecat session %s is running", name)
	}
	return nil
}

func applyPolecatIdentityRename(bd *beads.Beads, rigName, oldBeadID, newBeadID, newName string, oldFields *beads.AgentFields) error {
	newFields := &beads.AgentFields{
		RoleType:          "polecat",
		Rig:               rigName,
		AgentState:        oldFields.AgentState,
		HookBead:          oldFields.HookBead,
		CleanupStatus:     oldFields.CleanupStatus,
		ActiveMR:          oldFields.ActiveMR,
		NotificationLevel: oldFields.NotificationLevel,
	}
	newTitle := fmt.Sprintf("Polecat %s in %s", newName, rigName)
	if _, err := bd.CreateOrReopenAgentBead(newBeadID, newTitle, newFields); err != nil {
		return fmt.Errorf("creating new identity bead: %w", err)
	}
	if err := bd.CloseWithReason(fmt.Sprintf("renamed to %s", newBeadID), oldBeadID); err != nil {
		_ = bd.CloseWithReason("rename failed", newBeadID)
		return fmt.Errorf("closing old identity bead: %w", err)
	}
	return nil
}

func printPolecatIdentityRename(oldBeadID, newBeadID, oldName string) {
	fmt.Printf("%s Renamed identity:\n", style.SuccessPrefix)
	fmt.Printf("  Old: %s\n", oldBeadID)
	fmt.Printf("  New: %s\n", newBeadID)
	fmt.Printf("\n%s Note: If a worktree exists for %s, you'll need to recreate it with the new name.\n",
		style.Warning.Render("⚠"), oldName)
}

func runPolecatIdentityRemove(cmd *cobra.Command, args []string) error {
	force := commandBoolFlag(cmd, "force")
	rigName, polecatName := args[0], args[1]
	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}
	bd := beads.New(r.Path)
	beadID := polecatBeadIDForRig(r, rigName, polecatName)
	issue, fields, err := bd.GetAgentBead(beadID)
	if err != nil {
		return fmt.Errorf("getting identity bead: %w", err)
	}
	if issue == nil {
		return fmt.Errorf("identity bead %s not found", beadID)
	}
	if issue.Status == "closed" {
		return fmt.Errorf("identity bead %s is already closed", beadID)
	}
	if !force {
		sessionMgr := polecat.NewSessionManager(tmux.NewTmux(), r)
		if err := guardPolecatIdentityRemove(bd, sessionMgr, issue, fields, rigName, polecatName, beadID); err != nil {
			return err
		}
	}
	if err := bd.CloseWithReason("removed via gt polecat identity remove", beadID); err != nil {
		return fmt.Errorf("closing identity bead: %w", err)
	}
	fmt.Printf("%s Removed identity bead: %s\n", style.SuccessPrefix, beadID)
	return nil
}

func guardPolecatIdentityRemove(bd *beads.Beads, sessionMgr *polecat.SessionManager, issue *beads.Issue, fields *beads.AgentFields, rigName, polecatName, beadID string) error {
	running, _ := sessionMgr.IsRunning(polecatName)
	reasons := polecatIdentityRemoveReasons(bd, issue, fields, running)
	if len(reasons) > 0 {
		fmt.Printf("%s Cannot remove identity %s:\n", style.Error.Render("Error:"), beadID)
		for _, reason := range reasons {
			fmt.Printf("  - %s\n", reason)
		}
		fmt.Println("\nUse --force to bypass safety checks.")
		return fmt.Errorf("safety checks failed")
	}
	warnPolecatIdentityCV(bd, rigName, polecatName, beadID)
	return nil
}

func polecatIdentityRemoveReasons(bd *beads.Beads, issue *beads.Issue, fields *beads.AgentFields, running bool) []string {
	var reasons []string
	if running {
		reasons = append(reasons, "session is running")
	}
	hookBead := issue.HookBead
	if hookBead == "" && fields != nil {
		hookBead = fields.HookBead
	}
	if hookBead != "" {
		hookedIssue, _ := bd.Show(hookBead)
		if hookedIssue != nil && hookedIssue.Status != "closed" {
			reasons = append(reasons, fmt.Sprintf("has work on hook (%s)", hookBead))
		}
	}
	return reasons
}

func warnPolecatIdentityCV(bd *beads.Beads, rigName, polecatName, beadID string) {
	cvBeads, _ := bd.ListByAssignee(fmt.Sprintf("%s/%s", rigName, polecatName))
	cvCount := 0
	for _, cv := range cvBeads {
		if cv.ID != beadID && cv.Status == "closed" {
			cvCount++
		}
	}
	if cvCount > 0 {
		fmt.Printf("%s Warning: This polecat has %d completed work item(s) in CV.\n",
			style.Warning.Render("⚠"), cvCount)
	}
}

func buildCVSummary(rigPath, rigName, polecatName, identityBeadID, clonePath string) *CVSummary {
	cv := &CVSummary{
		Identity:   identityBeadID,
		Languages:  make(map[string]int),
		WorkTypes:  make(map[string]int),
		RecentWork: []RecentWorkItem{},
	}
	beadsQueryPath := clonePath
	if beadsQueryPath == "" {
		beadsQueryPath = rigPath
	}
	fillCVCreated(cv, beadsQueryPath, identityBeadID)
	cv.Sessions = countPolecatSessions(rigPath, polecatName)
	assignee := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
	fillCVCompletedWork(cv, beadsQueryPath, assignee)
	fillCVFailedWork(cv, beadsQueryPath, assignee)
	fillCVLanguages(cv, clonePath)
	total := cv.IssuesCompleted + cv.IssuesFailed + cv.IssuesAbandoned
	if total > 0 {
		cv.FirstPassRate = float64(cv.IssuesCompleted) / float64(total)
	}
	return cv
}

func fillCVCreated(cv *CVSummary, beadsQueryPath, identityBeadID string) {
	agentBead, _, err := beads.New(beadsQueryPath).GetAgentBead(identityBeadID)
	if err != nil || agentBead == nil || agentBead.CreatedAt == "" || len(agentBead.CreatedAt) < 10 {
		return
	}
	cv.Created = agentBead.CreatedAt[:10]
}

func fillCVCompletedWork(cv *CVSummary, beadsQueryPath, assignee string) {
	completedIssues, err := queryAssignedIssues(beadsQueryPath, assignee, "closed")
	if err != nil {
		return
	}
	cv.IssuesCompleted = len(completedIssues)
	for _, issue := range completedIssues {
		workType := extractWorkType(issue.Title, issue.Type)
		if workType != "" {
			cv.WorkTypes[workType]++
		}
		if len(cv.RecentWork) < 5 {
			cv.RecentWork = append(cv.RecentWork, RecentWorkItem{
				ID:        issue.ID,
				Title:     issue.Title,
				Type:      workType,
				Completed: issue.Updated,
				Ago:       formatRelativeTimeCV(issue.Updated),
			})
		}
	}
}

func fillCVFailedWork(cv *CVSummary, beadsQueryPath, assignee string) {
	if escalatedIssues, err := queryAssignedIssues(beadsQueryPath, assignee, "escalated"); err == nil {
		cv.IssuesFailed = len(escalatedIssues)
	}
	if deferredIssues, err := queryAssignedIssues(beadsQueryPath, assignee, "deferred"); err == nil {
		cv.IssuesAbandoned = len(deferredIssues)
	}
}

func fillCVLanguages(cv *CVSummary, clonePath string) {
	if clonePath == "" {
		return
	}
	langStats := getLanguageStats(clonePath)
	if len(langStats) > 0 {
		cv.Languages = langStats
	}
}

func extractWorkType(title, issueType string) string {
	if workType := workTypeFromIssueType(issueType); workType != "" {
		return workType
	}
	title = strings.ToLower(title)
	if workType := workTypeFromTitlePrefix(title); workType != "" {
		return workType
	}
	return workTypeFromTitleKeywords(title)
}

func workTypeFromIssueType(issueType string) string {
	switch issueType {
	case "bug":
		return "fix"
	case "task", "feature":
		return "feat"
	case "epic":
		return "epic"
	default:
		return ""
	}
}

func workTypeFromTitlePrefix(title string) string {
	prefixes := []string{"feat:", "fix:", "refactor:", "docs:", "test:", "chore:", "style:", "perf:"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(title, prefix) {
			return strings.TrimSuffix(prefix, ":")
		}
	}
	return ""
}

func workTypeFromTitleKeywords(title string) string {
	if strings.Contains(title, "fix") || strings.Contains(title, "bug") {
		return "fix"
	}
	if strings.Contains(title, "add") || strings.Contains(title, "implement") || strings.Contains(title, "create") {
		return "feat"
	}
	if strings.Contains(title, "refactor") || strings.Contains(title, "cleanup") {
		return "refactor"
	}
	return ""
}

func formatRelativeTimeCV(timestamp string) string {
	t, ok := parseCVTimestamp(timestamp)
	if !ok {
		return ""
	}
	return formatCVDuration(time.Since(t))
}

func parseCVTimestamp(timestamp string) (time.Time, bool) {
	layouts := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}
	for _, layout := range layouts {
		t, err := time.Parse(layout, timestamp)
		if err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func formatCVDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return formatCVUnit(int(d.Minutes()), "m")
	case d < 24*time.Hour:
		return formatCVUnit(int(d.Hours()), "h")
	case d < 7*24*time.Hour:
		return formatCVUnit(int(d.Hours()/24), "d")
	default:
		return formatCVUnit(int(d.Hours()/24/7), "w")
	}
}

func formatCVUnit(n int, unit string) string {
	if n == 1 {
		return "1" + unit + " ago"
	}
	return fmt.Sprintf("%d%s ago", n, unit)
}

// IssueInfo holds basic issue information for CV queries.
type IssueInfo struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Type    string `json:"issue_type"`
	Status  string `json:"status"`
	Updated string `json:"updated_at"`
}

func queryAssignedIssues(rigPath, assignee, status string) ([]IssueInfo, error) {
	args := []string{"list", "--assignee=" + assignee, "--json"}
	if status != "" {
		args = append(args, "--status="+status)
	}
	cmd := beads.Spawn(args...)
	cmd.Dir = rigPath
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return []IssueInfo{}, nil
	}
	var issues []IssueInfo
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, err
	}
	sort.Slice(issues, func(i, j int) bool {
		return issues[i].Updated > issues[j].Updated
	})
	return issues, nil
}

func getLanguageStats(clonePath string) map[string]int {
	stats := make(map[string]int)
	cmd := exec.Command("git", "log", "--name-only", "--pretty=format:", "--diff-filter=ACMR", "-100")
	cmd.Dir = clonePath
	out, err := cmd.Output()
	if err != nil {
		return stats
	}
	extCount := make(map[string]int)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ext := filepath.Ext(line)
		if ext != "" {
			extCount[ext]++
		}
	}
	for ext, count := range extCount {
		if lang, ok := languageFromExt(ext); ok {
			stats[lang] += count
		}
	}
	return stats
}

func languageFromExt(ext string) (string, bool) {
	extToLang := map[string]string{
		".go":    "Go",
		".ts":    "TypeScript",
		".tsx":   "TypeScript",
		".js":    "JavaScript",
		".jsx":   "JavaScript",
		".py":    "Python",
		".rs":    "Rust",
		".java":  "Java",
		".rb":    "Ruby",
		".c":     "C",
		".cpp":   "C++",
		".h":     "C",
		".hpp":   "C++",
		".cs":    "C#",
		".swift": "Swift",
		".kt":    "Kotlin",
		".scala": "Scala",
		".php":   "PHP",
		".sh":    "Shell",
		".bash":  "Shell",
		".zsh":   "Shell",
		".md":    "Markdown",
		".yaml":  "YAML",
		".yml":   "YAML",
		".json":  "JSON",
		".toml":  "TOML",
		".sql":   "SQL",
		".html":  "HTML",
		".css":   "CSS",
		".scss":  "SCSS",
	}
	lang, ok := extToLang[ext]
	return lang, ok
}

// formatCountStyled formats a count with appropriate styling using lipgloss.Style.
func formatCountStyled(count int, s lipgloss.Style) string {
	if count == 0 {
		return style.Dim.Render("0")
	}
	return s.Render(strconv.Itoa(count))
}

// countPolecatSessions counts the number of sessions from checkpoint files.
func countPolecatSessions(rigPath, polecatName string) int {
	// Look for checkpoint files in the polecat's directory
	checkpointDir := filepath.Join(rigPath, "polecats", polecatName, ".checkpoints")
	entries, err := os.ReadDir(checkpointDir)
	if err != nil {
		// Also check at rig level
		checkpointDir = filepath.Join(rigPath, ".checkpoints")
		entries, err = os.ReadDir(checkpointDir)
		if err != nil {
			return 0
		}
	}

	// Count checkpoint files that contain this polecat's name
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.Contains(entry.Name(), polecatName) {
			count++
		}
	}

	// If no checkpoint files found, return at least 1 if polecat exists
	if count == 0 {
		return 1
	}
	return count
}

// formatLanguageStats formats language statistics for display.
func formatLanguageStats(langs map[string]int) string {
	// Sort by count descending
	type langCount struct {
		lang  string
		count int
	}
	var sorted []langCount
	for lang, count := range langs {
		sorted = append(sorted, langCount{lang, count})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].count > sorted[j].count
	})

	// Format top languages
	var parts []string
	for i, lc := range sorted {
		if i >= 3 { // Show top 3
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", lc.lang, lc.count))
	}
	return strings.Join(parts, ", ")
}

// formatWorkTypeStats formats work type statistics for display.
func formatWorkTypeStats(types map[string]int) string {
	// Sort by count descending
	type typeCount struct {
		typ   string
		count int
	}
	var sorted []typeCount
	for typ, count := range types {
		sorted = append(sorted, typeCount{typ, count})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].count > sorted[j].count
	})

	// Format all types
	var parts []string
	for _, tc := range sorted {
		parts = append(parts, fmt.Sprintf("%s (%d)", tc.typ, tc.count))
	}
	return strings.Join(parts, ", ")
}
