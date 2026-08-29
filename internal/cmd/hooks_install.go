package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/hooks"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var hooksInstallCmd = &cobra.Command{
	Use:   "install <hook-name>",
	Short: "Install a hook from the registry",
	Long: `Install a hook from the registry to worktrees.

By default, installs to the current worktree. Use --role to install
to all worktrees of a specific role in the current rig.

Examples:
  gt hooks install pr-workflow-guard              # Install to current worktree
  gt hooks install pr-workflow-guard --role crew  # Install to all crew in current rig
  gt hooks install session-prime --role crew --all-rigs  # Install to all crew everywhere
  gt hooks install pr-workflow-guard --dry-run    # Preview what would be installed`,
	Args: cobra.ExactArgs(1),
	RunE: runHooksInstall,
}

func init() {
	hooksCmd.AddCommand(hooksInstallCmd)
	hooksInstallCmd.Flags().String("role", "", "Install to all worktrees of this role (crew, polecat, witness, refinery)")
	hooksInstallCmd.Flags().Bool("all-rigs", false, "Install across all rigs (requires --role)")
	hooksInstallCmd.Flags().Bool("dry-run", false, "Preview changes without writing files")
	hooksInstallCmd.Flags().Bool("force", false, "Install even if hook is disabled in registry")
}

func runHooksInstall(cmd *cobra.Command, args []string) error {
	role := commandStringFlag(cmd, "role")
	allRigs := commandBoolFlag(cmd, "all-rigs")
	dryRun := commandBoolFlag(cmd, "dry-run")
	force := commandBoolFlag(cmd, "force")
	hookName := args[0]

	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	registry, err := LoadRegistry(townRoot)
	if err != nil {
		return err
	}
	hookDef, err := findHookToInstall(registry, hookName, force)
	if err != nil {
		return err
	}

	targets, err := determineTargets(townRoot, role, allRigs, hookDef.Roles)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		targets, err = currentHooksInstallTarget(townRoot, role)
		if err != nil {
			return err
		}
	}

	summary := installHooksToTargets(targets, hookDef, dryRun)
	printHooksInstallSummary(hookName, summary, dryRun)
	return hooksInstallSummaryError(summary)
}

func findHookToInstall(registry *HookRegistry, hookName string, force bool) (HookDefinition, error) {
	hookDef, ok := registry.Hooks[hookName]
	if !ok {
		return HookDefinition{}, fmt.Errorf("hook %q not found in registry", hookName)
	}
	if hookDef.Enabled || force {
		if !hookDef.Enabled {
			fmt.Printf("%s Hook %q is disabled in registry, installing with --force.\n",
				style.Warning.Render("Warning:"), hookName)
		}
		return hookDef, nil
	}
	return HookDefinition{}, fmt.Errorf("hook %q is disabled in registry; use --force to install anyway", hookName)
}

func currentHooksInstallTarget(townRoot, role string) ([]string, error) {
	if role != "" {
		return nil, fmt.Errorf("no targets found for role %q in workspace", role)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return []string{resolveSettingsTarget(townRoot, cwd)}, nil
}

type hooksInstallSummary struct {
	installed       int
	errors          int
	integrityErrors int
	failedTargets   []string
}

func installHooksToTargets(targets []string, hookDef HookDefinition, dryRun bool) hooksInstallSummary {
	var summary hooksInstallSummary
	for _, target := range targets {
		if err := installHookTo(target, hookDef, dryRun); err != nil {
			label := "install error"
			if hooks.IsSettingsIntegrityError(err) {
				label = "integrity violation"
				summary.integrityErrors++
			}
			fmt.Printf("%s Failed to install to %s (%s): %v\n", style.Error.Render("Error:"), target, label, err)
			summary.errors++
			summary.failedTargets = append(summary.failedTargets, target)
			continue
		}
		summary.installed++
	}
	return summary
}

func printHooksInstallSummary(hookName string, summary hooksInstallSummary, dryRun bool) {
	verb := "Installed"
	label := "Done:"
	if dryRun {
		verb = "Would install"
		label = "Dry run:"
	}
	styleRender := style.Success.Render
	if dryRun {
		styleRender = style.Dim.Render
	}
	fmt.Printf("\n%s %s %q to %d worktree(s)\n", styleRender(label), verb, hookName, summary.installed)
}

func hooksInstallSummaryError(summary hooksInstallSummary) error {
	if summary.errors == 0 {
		return nil
	}
	if summary.integrityErrors > 0 {
		return fmt.Errorf("hook install failed closed: %d integrity violation(s) (%s)",
			summary.integrityErrors, strings.Join(summary.failedTargets, ", "))
	}
	return fmt.Errorf("hook install failed: %d target(s) failed (%s)",
		summary.errors, strings.Join(summary.failedTargets, ", "))
}

// determineTargets finds all worktree paths matching the role criteria.
func determineTargets(townRoot, role string, allRigs bool, allowedRoles []string) ([]string, error) {
	if role == "" {
		return nil, nil // Will use current directory
	}

	if !roleIsAllowed(role, allowedRoles) {
		return nil, fmt.Errorf("hook is not applicable to role %q (allowed: %s)", role, strings.Join(allowedRoles, ", "))
	}

	rigs, err := hooksInstallRigs(townRoot, allRigs)
	if err != nil {
		return nil, err
	}

	var targets []string
	for _, rig := range rigs {
		if target := hooksInstallRoleTarget(townRoot, rig, role); target != "" {
			targets = append(targets, target)
		}
	}

	return targets, nil
}

func roleIsAllowed(role string, allowedRoles []string) bool {
	for _, allowed := range allowedRoles {
		if allowed == role {
			return true
		}
	}
	return false
}

func hooksInstallRigs(townRoot string, allRigs bool) ([]string, error) {
	if allRigs {
		entries, err := os.ReadDir(townRoot)
		if err != nil {
			return nil, err
		}
		var rigs []string
		for _, entry := range entries {
			if isHooksInstallRig(entry.Name(), entry.IsDir()) {
				rigs = append(rigs, entry.Name())
			}
		}
		return rigs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	relPath, err := filepath.Rel(townRoot, cwd)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(relPath, string(filepath.Separator))
	if len(parts) == 0 {
		return nil, nil
	}
	return []string{parts[0]}, nil
}

func isHooksInstallRig(name string, isDir bool) bool {
	return isDir && !strings.HasPrefix(name, ".") && name != "mayor" && name != "deacon" && name != "hooks"
}

func hooksInstallRoleTarget(townRoot, rig, role string) string {
	roleDir := map[string]string{
		constants.RoleCrew:     "crew",
		constants.RolePolecat:  "polecats",
		constants.RoleWitness:  "witness",
		constants.RoleRefinery: "refinery",
	}[role]
	if roleDir == "" {
		return ""
	}
	target := filepath.Join(townRoot, rig, roleDir)
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		return target
	}
	return ""
}

// resolveSettingsTarget resolves a working directory to the appropriate settings
// target directory. For shared-parent roles (crew, polecats, witness, refinery),
// this returns the role parent directory rather than the individual worktree,
// matching the shared settings model used by DiscoverTargets and EnsureSettingsForRole.
func resolveSettingsTarget(townRoot, cwd string) string {
	relPath, err := filepath.Rel(townRoot, cwd)
	if err != nil {
		return cwd
	}
	parts := strings.Split(relPath, string(filepath.Separator))
	if len(parts) < 2 {
		return cwd // At town root or top-level dir (mayor/deacon)
	}
	// parts[0] = rig name (or mayor/deacon), parts[1] = role dir
	roleDir := parts[1]
	switch roleDir {
	case "crew", "polecats", "witness", "refinery":
		return filepath.Join(townRoot, parts[0], roleDir)
	default:
		return cwd
	}
}

// installHookTo installs a hook to a specific worktree.
func installHookTo(worktreePath string, hookDef HookDefinition, dryRun bool) error {
	settingsPath := filepath.Join(worktreePath, ".claude", "settings.json")

	// Load existing settings or create new
	settings, err := hooks.LoadSettings(settingsPath)
	if err != nil {
		return fmt.Errorf("loading existing settings: %w", err)
	}

	if settings.EnabledPlugins == nil {
		settings.EnabledPlugins = make(map[string]bool)
	}

	addHookEntries(&settings.Hooks, hookDef)
	settings.EnabledPlugins["beads@beads-marketplace"] = false

	relPath := hookInstallDisplayPath(worktreePath)

	if dryRun {
		fmt.Printf("  %s %s\n", style.Dim.Render("Would install to:"), relPath)
		return nil
	}
	return writeInstalledHook(settings, settingsPath, relPath)
}

func addHookEntries(config *hooks.HooksConfig, hookDef HookDefinition) {
	for _, matcher := range hookDef.Matchers {
		config.AddEntry(hookDef.Event, hooks.HookEntry{
			Matcher: matcher,
			Hooks:   []hooks.Hook{{Type: "command", Command: hookDef.Command}},
		})
	}
}

func hookInstallDisplayPath(worktreePath string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, worktreePath); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return worktreePath
}

func writeInstalledHook(settings *hooks.SettingsJSON, settingsPath, relPath string) error {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		return fmt.Errorf("creating .claude directory: %w", err)
	}

	data, err := hooks.MarshalSettings(settings)
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		return fmt.Errorf("writing settings: %w", err)
	}

	fmt.Printf("  %s %s\n", style.Success.Render("Installed to:"), relPath)
	return nil
}
