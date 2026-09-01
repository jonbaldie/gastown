package session

import (
	"path/filepath"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
)

// ResolveWindowTint resolves the window tint style for a session.
// Resolution order mirrors status bar theming:
//  1. Per-rig role tint (rig/settings/config.json → theme.window_tint.role_tints)
//  2. Global role tint (mayor/config.json → theme.window_tint.role_tints)
//  3. Per-rig window tint (rig/settings/config.json → theme.window_tint.name/custom)
//  4. Global window tint (mayor/config.json → theme.window_tint.name/custom)
//  5. Fallback: disabled (nil) — no window tinting by default
//
// Returns nil if window tinting is disabled or not configured.
// Returns a WindowStyle if window tinting is enabled.
func ResolveWindowTint(rig, role string) *tmux.WindowStyle {
	townRoot, _ := workspace.FindFromCwd()
	rigWindowTint := loadRigWindowTint(townRoot, rig)
	globalWindowTint := loadGlobalWindowTint(townRoot)

	// If the rig has its own window_tint config, it's the final word.
	// The rig either specifies exact colors or returns nil (inherit from status bar).
	// This prevents global role_tints from overriding rig-level intent — e.g.,
	// a rig with crew_themes wants window tint to match per-member status bar colors,
	// not a global role-level default.
	if style, handled := resolveWindowTint(rigWindowTint, role); handled {
		return style
	}

	if style, handled := resolveWindowTint(globalWindowTint, role); handled {
		return style
	}
	return nil
}

func loadRigWindowTint(townRoot, rig string) *config.WindowTint {
	if townRoot == "" || rig == "" {
		return nil
	}
	settingsPath := filepath.Join(townRoot, rig, "settings", "config.json")
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil || settings.Theme == nil {
		return nil
	}
	return settings.Theme.WindowTint
}

func loadGlobalWindowTint(townRoot string) *config.WindowTint {
	if townRoot == "" {
		return nil
	}
	mayorConfigPath := filepath.Join(townRoot, "mayor", "config.json")
	mayorCfg, err := config.LoadMayorConfig(mayorConfigPath)
	if err != nil || mayorCfg.Theme == nil {
		return nil
	}
	return mayorCfg.Theme.WindowTint
}

func resolveWindowTint(windowTint *config.WindowTint, role string) (*tmux.WindowStyle, bool) {
	if windowTint == nil {
		return nil, false
	}
	if windowTint.Enabled != nil && !*windowTint.Enabled {
		return nil, true
	}
	if style := resolveWindowRoleTint(windowTint, role); style != nil {
		return style, true
	}
	if windowTint.Custom != nil {
		return &tmux.WindowStyle{BG: windowTint.Custom.BG, FG: windowTint.Custom.FG}, true
	}
	if style := resolveWindowNamedTint(windowTint.Name); style != nil {
		return style, true
	}
	return nil, true
}

func resolveWindowRoleTint(windowTint *config.WindowTint, role string) *tmux.WindowStyle {
	if windowTint.RoleTints == nil {
		return nil
	}
	themeName, ok := windowTint.RoleTints[role]
	if !ok {
		return nil
	}
	return windowStyleForTheme(themeName)
}

func resolveWindowNamedTint(name string) *tmux.WindowStyle {
	if name == "" {
		return nil
	}
	return windowStyleForTheme(name)
}

func windowStyleForTheme(name string) *tmux.WindowStyle {
	theme := tmux.GetThemeByName(name)
	if theme == nil {
		return nil
	}
	return &tmux.WindowStyle{BG: theme.BG, FG: theme.FG}
}

// DefaultTintFactor is the default darkening factor for window backgrounds
// when inheriting from the status bar theme.
const DefaultTintFactor = 0.4

// ResolveTintFactor returns the tint_factor from the most specific config level.
// Falls back to DefaultTintFactor if not configured.
func ResolveTintFactor(rig string) float64 {
	townRoot, _ := workspace.FindFromCwd()

	if windowTint := loadRigWindowTint(townRoot, rig); windowTint != nil && windowTint.TintFactor != nil {
		return *windowTint.TintFactor
	}

	if windowTint := loadGlobalWindowTint(townRoot); windowTint != nil && windowTint.TintFactor != nil {
		return *windowTint.TintFactor
	}

	return DefaultTintFactor
}

// IsWindowTintEnabled checks if window tinting is enabled at any config level.
// Returns true if enabled explicitly; false if disabled or not configured.
func IsWindowTintEnabled(rig string) bool {
	townRoot, _ := workspace.FindFromCwd()

	if windowTint := loadRigWindowTint(townRoot, rig); windowTint != nil && windowTint.Enabled != nil {
		return *windowTint.Enabled
	}

	if windowTint := loadGlobalWindowTint(townRoot); windowTint != nil && windowTint.Enabled != nil {
		return *windowTint.Enabled
	}

	return false
}
