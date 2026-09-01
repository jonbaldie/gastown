package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// Environment variables for role detection
const (
	EnvGTRole     = "GT_ROLE"
	EnvGTRoleHome = "GT_ROLE_HOME"
)

// RoleInfo contains information about a role and its detection source.
// This is the canonical struct for role detection - used by both GetRole()
// and detectRole() functions.
type RoleInfo struct {
	Role          Role   `json:"role"`
	Source        string `json:"source"` // "env", "cwd", or "explicit"
	Home          string `json:"home"`
	Rig           string `json:"rig,omitempty"`
	Polecat       string `json:"polecat,omitempty"`
	EnvRole       string `json:"env_role,omitempty"`       // Value of GT_ROLE if set
	CwdRole       Role   `json:"cwd_role,omitempty"`       // Role detected from cwd
	Mismatch      bool   `json:"mismatch,omitempty"`       // True if env != cwd detection
	EnvIncomplete bool   `json:"env_incomplete,omitempty"` // True if env was set but missing rig/polecat, filled from cwd
	TownRoot      string `json:"town_root,omitempty"`
	WorkDir       string `json:"work_dir,omitempty"` // Current working directory
}

var roleCmd = &cobra.Command{
	Use:     "role",
	GroupID: GroupAgents,
	Short:   "Show or manage agent role",
	Long: `Display the current agent role and its detection source.

Role is determined by:
1. GT_ROLE environment variable (authoritative if set)
2. Current working directory (fallback)

If both are available and disagree, a warning is shown.`,
	RunE: runRoleShow,
}

var roleShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show current role",
	Long: `Show the current agent role, its detection source, and associated metadata.

Displays the role name, whether it was detected from the GT_ROLE environment
variable or the current working directory, and the rig/worker identity if
applicable. Warns if the two detection methods disagree.`,
	RunE: runRoleShow,
}

var roleHomeCmd = &cobra.Command{
	Use:   "home [ROLE]",
	Short: "Show home directory for a role",
	Long: `Show the canonical home directory for a role.

If no role is specified, shows the home for the current role.

Examples:
  gt role home           # Home for current role
  gt role home mayor     # Home for mayor
  gt role home witness   # Home for witness (requires --rig)`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRoleHome,
}

var roleDetectCmd = &cobra.Command{
	Use:   "detect",
	Short: "Force cwd-based role detection (debugging)",
	Long: `Detect role from current working directory, ignoring GT_ROLE env var.

This is useful for debugging role detection issues.`,
	RunE: runRoleDetect,
}

var roleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all known roles",
	Long: `List all known Gas Town agent roles and their descriptions.

Roles include mayor, deacon, witness, refinery, polecat, and crew.
Each role has a specific scope and responsibilities within the
Gas Town multi-agent architecture.`,
	RunE: runRoleList,
}

var roleEnvCmd = &cobra.Command{
	Use:   "env",
	Short: "Print export statements for current role",
	Long: `Print shell export statements for the current role.

Role is determined from GT_ROLE environment variable or current working directory.
This is a read-only command that displays the current role's env vars.

Examples:
  eval $(gt role env)    # Export current role's env vars
  gt role env            # View what would be exported`,
	RunE: runRoleEnv,
}

var roleDefCmd = &cobra.Command{
	Use:   "def <role>",
	Short: "Display role definition (session, health, env config)",
	Long: `Display the effective role definition after all overrides are applied.

Role configuration is layered:
  1. Built-in defaults (embedded in binary)
  2. Town-level overrides (<town>/roles/<role>.toml)
  3. Rig-level overrides (<rig>/roles/<role>.toml)

Examples:
  gt role def witness    # Show witness role definition
  gt role def crew       # Show crew role definition`,
	Args: cobra.ExactArgs(1),
	RunE: runRoleDef,
}

func init() {
	rootCmd.AddCommand(roleCmd)
	roleCmd.AddCommand(roleShowCmd)
	roleCmd.AddCommand(roleHomeCmd)
	roleCmd.AddCommand(roleDetectCmd)
	roleCmd.AddCommand(roleListCmd)
	roleCmd.AddCommand(roleEnvCmd)
	roleCmd.AddCommand(roleDefCmd)

	// Add --rig and --polecat flags to home command for overrides
	roleHomeCmd.Flags().String("rig", "", "Rig name (required for rig-specific roles)")
	roleHomeCmd.Flags().String("polecat", "", "Polecat/crew member name")
}

// GetRole returns the current role, checking GT_ROLE first then falling back to cwd.
// This is the canonical function for role detection.
func GetRole() (RoleInfo, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return RoleInfo{}, fmt.Errorf("getting current directory: %w", err)
	}

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return RoleInfo{}, fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return RoleInfo{}, fmt.Errorf("not in a Gas Town workspace")
	}

	return GetRoleWithContext(cwd, townRoot)
}

// GetRoleWithContext returns role info given explicit cwd and town root.
func GetRoleWithContext(cwd, townRoot string) (RoleInfo, error) {
	info := RoleInfo{
		TownRoot: townRoot,
		WorkDir:  cwd,
	}

	// Check environment variable first
	envRole := os.Getenv(EnvGTRole)
	info.EnvRole = envRole

	// Always detect from cwd for comparison/fallback
	cwdCtx := detectRole(cwd, townRoot)
	info.CwdRole = cwdCtx.Role

	if envRole != "" {
		applyEnvRole(&info, envRole, cwdCtx)
	} else {
		applyCwdRole(&info, cwdCtx)
	}

	// Determine home directory
	info.Home = getRoleHome(info.Role, info.Rig, info.Polecat, townRoot)

	return info, nil
}

func applyEnvRole(info *RoleInfo, envRole string, cwdCtx RoleInfo) {
	parsedRole, rig, polecat := parseRoleString(envRole)
	info.Role = parsedRole
	info.Rig = rig
	info.Polecat = polecat
	info.Source = "env"
	fillRoleFromEnvironment(info)
	fillRoleGapsFromCwd(info, parsedRole, cwdCtx)
	info.Mismatch = cwdCtx.Role != RoleUnknown && cwdCtx.Role != parsedRole
}

func fillRoleFromEnvironment(info *RoleInfo) {
	if info.Rig == "" {
		info.Rig = os.Getenv("GT_RIG")
	}
	if info.Polecat == "" {
		info.Polecat = firstNonEmptyEnv("GT_CREW", "GT_POLECAT")
	}
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func fillRoleGapsFromCwd(info *RoleInfo, role Role, cwdCtx RoleInfo) {
	if roleNeedsRig(role) && info.Rig == "" && cwdCtx.Rig != "" {
		info.Rig = cwdCtx.Rig
		info.EnvIncomplete = true
	}
	if roleNeedsPolecat(role) && info.Polecat == "" && cwdCtx.Polecat != "" {
		info.Polecat = cwdCtx.Polecat
		info.EnvIncomplete = true
	}
}

func roleNeedsRig(role Role) bool {
	switch role {
	case RoleWitness, RoleRefinery, RolePolecat, RoleCrew:
		return true
	default:
		return false
	}
}

func roleNeedsPolecat(role Role) bool {
	switch role {
	case RolePolecat, RoleCrew, RoleDog:
		return true
	default:
		return false
	}
}

func applyCwdRole(info *RoleInfo, cwdCtx RoleInfo) {
	info.Role = cwdCtx.Role
	info.Rig = cwdCtx.Rig
	info.Polecat = cwdCtx.Polecat
	info.Source = "cwd"
}

// detectRole detects the agent role from the current working directory path.
// This is the cwd-based fallback used by GetRoleWithContext when GT_ROLE is not set.
func detectRole(cwd, townRoot string) RoleInfo {
	ctx := RoleInfo{
		Role:     RoleUnknown,
		TownRoot: townRoot,
		WorkDir:  cwd,
		Source:   "cwd",
	}

	// Get relative path from town root
	relPath, err := filepath.Rel(townRoot, cwd)
	if err != nil {
		return ctx
	}

	// Normalize and split path
	relPath = filepath.ToSlash(relPath)
	parts := strings.Split(relPath, "/")

	// Town root is a neutral location — don't infer any role from it.
	// The mayor's actual home is mayor/ (matched below).
	if relPath == "." || relPath == "" {
		return ctx
	}

	if detectTownRole(&ctx, parts) {
		return ctx
	}
	if len(parts) == 0 {
		return ctx
	}
	ctx.Rig = parts[0]
	detectRigRole(&ctx, parts)
	return ctx
}

func detectTownRole(ctx *RoleInfo, parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case "mayor":
		ctx.Role = RoleMayor
	case "deacon":
		return detectDeaconRole(ctx, parts)
	default:
		return false
	}
	return true
}

func detectDeaconRole(ctx *RoleInfo, parts []string) bool {
	if len(parts) >= 3 && parts[1] == "dogs" {
		if parts[2] == "boot" {
			ctx.Role = RoleBoot
		} else {
			ctx.Role = RoleDog
			ctx.Polecat = parts[2]
		}
		return true
	}
	ctx.Role = RoleDeacon
	return true
}

func detectRigRole(ctx *RoleInfo, parts []string) {
	if len(parts) < 2 {
		return
	}
	switch parts[1] {
	case "mayor":
		ctx.Role = RoleMayor
	case "witness":
		ctx.Role = RoleWitness
	case "refinery":
		ctx.Role = RoleRefinery
	case "polecats":
		setWorkerRole(ctx, RolePolecat, parts)
	case "crew":
		setWorkerRole(ctx, RoleCrew, parts)
	}
}

func setWorkerRole(ctx *RoleInfo, role Role, parts []string) {
	if len(parts) < 3 {
		return
	}
	ctx.Role = role
	ctx.Polecat = parts[2]
}

// parseRoleString parses a role string like "mayor", "gastown/witness", or "gastown/polecats/alpha".
func parseRoleString(s string) (Role, string, string) {
	s = strings.TrimSpace(s)

	// Normalize consecutive slashes (e.g. "gamestore//refinery" → "gamestore/refinery")
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	s = strings.TrimSuffix(s, "/")

	if role, ok := simpleRole(s); ok {
		return role, "", ""
	}

	// Compound roles: rig/role or rig/polecats/name or rig/crew/name
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return Role(s), "", ""
	}

	rig := parts[0]
	return compoundRole(s, rig, parts)
}

func simpleRole(s string) (Role, bool) {
	switch s {
	case constants.RoleMayor:
		return RoleMayor, true
	case constants.RoleDeacon:
		return RoleDeacon, true
	case "boot":
		return RoleBoot, true
	case "dog":
		return RoleDog, true
	default:
		return RoleUnknown, false
	}
}

func compoundRole(original, rig string, parts []string) (Role, string, string) {
	switch parts[1] {
	case "boot":
		if rig == "deacon" && len(parts) == 2 {
			return RoleBoot, "", ""
		}
		return Role(original), "", ""
	case constants.RoleWitness:
		return RoleWitness, rig, ""
	case constants.RoleRefinery:
		return RoleRefinery, rig, ""
	case "polecats":
		return workerRole(RolePolecat, rig, parts)
	case constants.RoleCrew:
		return workerRole(RoleCrew, rig, parts)
	default:
		return RolePolecat, rig, parts[1]
	}
}

func workerRole(role Role, rig string, parts []string) (Role, string, string) {
	name := ""
	if len(parts) >= 3 {
		name = parts[2]
	}
	return role, rig, name
}

// ActorString returns the actor identity string for beads attribution.
// Format matches beads created_by convention:
//   - Simple roles: "mayor", "deacon"
//   - Dog roles: "deacon-boot" (hyphenated, matching BD_ACTOR)
//   - Rig-specific: "gastown/witness", "gastown/refinery"
//   - Workers: "gastown/crew/max", "gastown/polecats/Toast"
func (info RoleInfo) ActorString() string {
	switch info.Role {
	case RoleMayor:
		return "mayor"
	case RoleDeacon:
		return "deacon"
	case RoleBoot:
		return "deacon-boot"
	case RoleWitness, RoleRefinery:
		return roleActorString(info, string(info.Role))
	case RolePolecat:
		return workerActorString(info, "polecat", "polecats")
	case RoleCrew:
		return workerActorString(info, "crew", "crew")
	default:
		return string(info.Role)
	}
}

func roleActorString(info RoleInfo, role string) string {
	if info.Rig != "" {
		return fmt.Sprintf("%s/%s", info.Rig, role)
	}
	return role
}

func workerActorString(info RoleInfo, fallback, path string) string {
	if info.Rig != "" && info.Polecat != "" {
		return fmt.Sprintf("%s/%s/%s", info.Rig, path, info.Polecat)
	}
	return fallback
}

// getRoleHome returns the canonical home directory for a role.
func getRoleHome(role Role, rig, polecat, townRoot string) string {
	switch role {
	case RoleMayor:
		return filepath.Join(townRoot, "mayor")
	case RoleDeacon:
		return filepath.Join(townRoot, "deacon")
	case RoleBoot:
		return filepath.Join(townRoot, "deacon", "dogs", "boot")
	case RoleWitness:
		return rigHome(townRoot, rig, "witness")
	case RoleRefinery:
		return rigHome(townRoot, rig, "refinery", "rig")
	case RolePolecat:
		return workerHome(townRoot, rig, polecat, "polecats")
	case RoleCrew:
		return workerHome(townRoot, rig, polecat, "crew")
	case RoleDog:
		return dogHome(townRoot, polecat)
	default:
		return ""
	}
}

func rigHome(townRoot, rig string, parts ...string) string {
	if rig == "" {
		return ""
	}
	return filepath.Join(append([]string{townRoot, rig}, parts...)...)
}

func workerHome(townRoot, rig, worker, directory string) string {
	if rig == "" || worker == "" {
		return ""
	}
	return filepath.Join(townRoot, rig, directory, worker)
}

func dogHome(townRoot, dog string) string {
	if dog == "" {
		return ""
	}
	return filepath.Join(townRoot, "deacon", "dogs", dog)
}

func runRoleShow(_ *cobra.Command, _ []string) error {
	info, err := GetRole()
	if err != nil {
		return err
	}

	// Header
	fmt.Printf("%s\n", style.Bold.Render(string(info.Role)))
	fmt.Printf("Source: %s\n", info.Source)

	if info.Home != "" {
		fmt.Printf("Home: %s\n", info.Home)
	}

	if info.Rig != "" {
		fmt.Printf("Rig: %s\n", info.Rig)
	}

	if info.Polecat != "" {
		fmt.Printf("Worker: %s\n", info.Polecat)
	}

	// Show mismatch warning
	if info.Mismatch {
		fmt.Println()
		fmt.Printf("%s\n", style.Bold.Render("⚠️  ROLE MISMATCH"))
		fmt.Printf("  GT_ROLE=%s (authoritative)\n", info.EnvRole)
		fmt.Printf("  cwd suggests: %s\n", info.CwdRole)
		fmt.Println()
		fmt.Println("The GT_ROLE env var takes precedence, but you may be in the wrong directory.")
		fmt.Printf("Expected home: %s\n", info.Home)
	}

	return nil
}

func runRoleHome(cmd *cobra.Command, args []string) error {
	rigOverride := commandStringFlag(cmd, "rig")
	polecatOverride := commandStringFlag(cmd, "polecat")
	cwd, townRoot, err := roleCommandContext()
	if err != nil {
		return err
	}
	if err := validateRoleHomeOverrides(rigOverride, polecatOverride); err != nil {
		return err
	}

	// Start with current role detection (from env vars or cwd)
	info, err := GetRole()
	if err != nil {
		return err
	}
	role := info.Role
	rig := info.Rig
	polecat := info.Polecat

	role, rig, polecat = applyRoleHomeOverrides(role, rig, polecat, args, rigOverride, polecatOverride)

	home := getRoleHome(role, rig, polecat, townRoot)
	if home == "" {
		return fmt.Errorf("cannot determine home for role %s (rig=%q, polecat=%q)", role, rig, polecat)
	}

	warnOutsideRoleHome(cwd, home)

	fmt.Println(home)
	return nil
}

func roleCommandContext() (cwd, townRoot string, err error) {
	cwd, err = os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("getting current directory: %w", err)
	}
	townRoot, err = workspace.FindFromCwd()
	if err != nil {
		return "", "", fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return "", "", fmt.Errorf("not in a Gas Town workspace")
	}
	return cwd, townRoot, nil
}

func validateRoleHomeOverrides(rigOverride, polecatOverride string) error {
	if polecatOverride != "" && rigOverride == "" {
		return fmt.Errorf("--polecat requires --rig to be specified")
	}
	return nil
}

func applyRoleHomeOverrides(role Role, rig, polecat string, args []string, rigOverride, polecatOverride string) (Role, string, string) {
	if len(args) > 0 {
		role, _, _ = parseRoleString(args[0])
	}
	if rigOverride != "" {
		rig = rigOverride
	}
	if polecatOverride != "" {
		polecat = polecatOverride
	}
	return role, rig, polecat
}

func warnOutsideRoleHome(cwd, home string) {
	if home != cwd && !strings.HasPrefix(cwd, home) {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: cwd (%s) is not within role home (%s)\n", cwd, home)
	}
}

func runRoleDetect(_ *cobra.Command, _ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding workspace: %w", err)
	}
	if townRoot == "" {
		return fmt.Errorf("not in a Gas Town workspace")
	}

	ctx := detectRole(cwd, townRoot)

	fmt.Printf("%s (from cwd)\n", style.Bold.Render(string(ctx.Role)))
	fmt.Printf("Directory: %s\n", cwd)

	if ctx.Rig != "" {
		fmt.Printf("Rig: %s\n", ctx.Rig)
	}
	if ctx.Polecat != "" {
		fmt.Printf("Worker: %s\n", ctx.Polecat)
	}

	// Check if env var disagrees
	envRole := os.Getenv(EnvGTRole)
	if envRole != "" {
		parsedRole, _, _ := parseRoleString(envRole)
		if parsedRole != ctx.Role {
			fmt.Println()
			fmt.Printf("%s\n", style.Bold.Render("⚠️  Mismatch with $GT_ROLE"))
			fmt.Printf("  $GT_ROLE=%s\n", envRole)
			fmt.Println("  The env var takes precedence in normal operation.")
		}
	}

	return nil
}

func runRoleList(_ *cobra.Command, _ []string) error {
	roles := []struct {
		name Role
		desc string
	}{
		{RoleMayor, "Global coordinator at mayor/"},
		{RoleDeacon, "Background supervisor daemon"},
		{RoleWitness, "Per-rig polecat lifecycle manager"},
		{RoleRefinery, "Per-rig merge queue processor"},
		{RolePolecat, "Worker with persistent identity, ephemeral sessions"},
		{RoleCrew, "Persistent worker with own worktree"},
	}

	fmt.Println("Available roles:")
	fmt.Println()
	for _, r := range roles {
		fmt.Printf("  %-10s  %s\n", style.Bold.Render(string(r.name)), r.desc)
	}
	return nil
}

func runRoleEnv(_ *cobra.Command, _ []string) error {
	cwd, townRoot, err := roleCommandContext()
	if err != nil {
		return err
	}

	// Get current role (read-only - from env vars or cwd)
	info, err := GetRole()
	if err != nil {
		return err
	}

	home := getRoleHome(info.Role, info.Rig, info.Polecat, townRoot)
	if home == "" {
		return fmt.Errorf("cannot determine home for role %s (rig=%q, polecat=%q)", info.Role, info.Rig, info.Polecat)
	}

	warnRoleEnv(info.EnvIncomplete, cwd, home)

	// Get canonical env vars from shared source of truth
	envVars := config.AgentEnv(config.AgentEnvConfig{
		Role:      string(info.Role),
		Rig:       info.Rig,
		AgentName: info.Polecat,
		TownRoot:  townRoot,
	})
	envVars[EnvGTRoleHome] = home

	printRoleEnv(envVars)
	return nil
}

func warnRoleEnv(incomplete bool, cwd, home string) {
	if incomplete {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: env vars incomplete, filled from cwd\n")
	}
	warnOutsideRoleHome(cwd, home)
}

func printRoleEnv(envVars map[string]string) {
	keys := make([]string, 0, len(envVars))
	for k := range envVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		printRoleEnvEntry(k, envVars[k])
	}
}

func printRoleEnvEntry(key, value string) {
	if runtime.GOOS == "windows" {
		fmt.Printf("$env:%s=%s\n", key, value)
		return
	}
	fmt.Printf("export %s=%s\n", key, value)
}

func runRoleDef(_ *cobra.Command, args []string) error {
	roleName := args[0]
	if err := validateRoleName(roleName); err != nil {
		return err
	}

	townRoot, rigPath := roleDefinitionPaths()

	def, err := config.LoadRoleDefinition(townRoot, rigPath, roleName)
	if err != nil {
		return fmt.Errorf("loading role definition: %w", err)
	}
	printRoleDefinition(def)
	return nil
}

func validateRoleName(roleName string) error {
	for _, role := range config.AllRoles() {
		if role == roleName {
			return nil
		}
	}
	return fmt.Errorf("unknown role %q - valid roles: %s", roleName, strings.Join(config.AllRoles(), ", "))
}

func roleDefinitionPaths() (townRoot, rigPath string) {
	townRoot, _ = workspace.FindFromCwd()
	if townRoot == "" {
		return townRoot, ""
	}
	if rigInfo, err := GetRole(); err == nil && rigInfo.Rig != "" {
		rigPath = filepath.Join(townRoot, rigInfo.Rig)
	}
	return townRoot, rigPath
}

func printRoleDefinition(def *config.RoleDefinition) {
	fmt.Printf("%s %s\n", style.Bold.Render("Role:"), def.Role)
	fmt.Printf("%s %s\n", style.Bold.Render("Scope:"), def.Scope)
	fmt.Println()
	printRoleSessionConfig(def)
	printRoleEnvironment(def.Env)
	printRoleHealthConfig(def)
	printRolePrompts(def)
}

func printRoleSessionConfig(def *config.RoleDefinition) {
	fmt.Println(style.Bold.Render("[session]"))
	fmt.Printf("  pattern        = %q\n", def.Session.Pattern)
	fmt.Printf("  work_dir       = %q\n", def.Session.WorkDir)
	fmt.Printf("  needs_pre_sync = %v\n", def.Session.NeedsPreSync)
	if def.Session.StartCommand != "" {
		fmt.Printf("  start_command  = %q\n", def.Session.StartCommand)
	}
	fmt.Println()
}

func printRoleEnvironment(env map[string]string) {
	if len(env) == 0 {
		return
	}
	fmt.Println(style.Bold.Render("[env]"))
	envKeys := make([]string, 0, len(env))
	for k := range env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		fmt.Printf("  %s = %q\n", k, env[k])
	}
	fmt.Println()
}

func printRoleHealthConfig(def *config.RoleDefinition) {
	fmt.Println(style.Bold.Render("[health]"))
	fmt.Printf("  ping_timeout         = %q\n", def.Health.PingTimeout.String())
	fmt.Printf("  consecutive_failures = %d\n", def.Health.ConsecutiveFailures)
	fmt.Printf("  kill_cooldown        = %q\n", def.Health.KillCooldown.String())
	fmt.Printf("  stuck_threshold      = %q\n", def.Health.StuckThreshold.String())
	if def.Health.HungSessionThreshold.Duration != 0 {
		fmt.Printf("  hung_session_threshold = %q\n", def.Health.HungSessionThreshold.String())
	}
	fmt.Println()
}

func printRolePrompts(def *config.RoleDefinition) {
	if def.Nudge != "" {
		fmt.Printf("%s %s\n", style.Bold.Render("Nudge:"), def.Nudge)
	}
	if def.PromptTemplate != "" {
		fmt.Printf("%s %s\n", style.Bold.Render("Template:"), def.PromptTemplate)
	}
}
