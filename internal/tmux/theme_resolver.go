package tmux

import (
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
)

// ResolveSessionTheme returns the configured tmux theme for a session.
// A nil theme means tmux theming is explicitly disabled.
// crewMember is the crew member name (e.g. "krieger"); pass "" for non-crew roles.
// When non-empty, crew_themes config is checked before role-level fallback.
func ResolveSessionTheme(townRoot, rigName, role, crewMember string) *Theme {
	role = normalizeThemeRole(role)

	if rigTheme := resolveRigSessionTheme(townRoot, rigName, role, crewMember); rigTheme != unresolvedTheme {
		return rigTheme
	}

	if townTheme := resolveTownSessionTheme(townRoot, role, crewMember); townTheme != unresolvedTheme {
		return townTheme
	}

	if themeName, ok := config.BuiltinRoleThemes()[role]; ok {
		if theme := GetThemeByName(themeName); theme != nil {
			return theme
		}
	}

	switch role {
	case constants.RoleMayor:
		theme := MayorTheme()
		return &theme
	case constants.RoleDeacon:
		theme := DeaconTheme()
		return &theme
	case "dog":
		theme := DogTheme()
		return &theme
	default:
		if rigName == "" {
			return nil
		}
		theme := AssignTheme(rigName)
		return &theme
	}
}

var unresolvedTheme = &Theme{Name: "__unresolved__"}

func resolveRigSessionTheme(townRoot, rigName, role, crewMember string) *Theme {
	if townRoot == "" || rigName == "" {
		return unresolvedTheme
	}

	settingsPath := config.RigSettingsPath(filepath.Join(townRoot, rigName))
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil || settings.Theme == nil {
		return unresolvedTheme
	}

	// Per-member theme takes priority over role-level theme.
	if resolved, ok := resolveThemeOverrides(role, crewMember, settings.Theme.CrewThemes, settings.Theme.RoleThemes); ok {
		return resolved
	}

	return resolveThemeConfig(settings.Theme)
}

func resolveTownSessionTheme(townRoot, role, crewMember string) *Theme {
	if townRoot == "" {
		return unresolvedTheme
	}

	mayorCfg, err := config.LoadMayorConfig(filepath.Join(townRoot, "mayor", "config.json"))
	if err != nil || mayorCfg.Theme == nil {
		return unresolvedTheme
	}

	// Per-member theme takes priority over role defaults at town level too.
	if resolved, ok := resolveThemeOverrides(role, crewMember, mayorCfg.Theme.CrewThemes, mayorCfg.Theme.RoleDefaults); ok {
		return resolved
	}

	return resolveThemeValues(mayorCfg.Theme.Disabled, mayorCfg.Theme.Name, mayorCfg.Theme.Custom)
}

func resolveThemeConfig(cfg *config.ThemeConfig) *Theme {
	if cfg == nil {
		return unresolvedTheme
	}
	return resolveThemeValues(cfg.Disabled, cfg.Name, cfg.Custom)
}

func resolveThemeValues(disabled bool, name string, custom *config.CustomTheme) *Theme {
	if disabled {
		return nil
	}
	if custom != nil {
		return customTheme("custom", custom)
	}
	if name != "" {
		if theme := GetThemeByName(name); theme != nil {
			return theme
		}
	}
	return unresolvedTheme
}

func resolveThemeOverrides(role, crewMember string, crewThemes, roleThemes map[string]string) (*Theme, bool) {
	if crewMember != "" {
		if resolved, ok := resolveRoleThemeName(crewThemes[crewMember]); ok {
			return resolved, true
		}
	}
	if resolved, ok := resolveRoleThemeName(roleThemes[role]); ok {
		return resolved, true
	}
	return nil, false
}

func resolveRoleThemeName(name string) (*Theme, bool) {
	if name == "" {
		return nil, false
	}
	if strings.EqualFold(name, "none") {
		return nil, true
	}
	if theme := GetThemeByName(name); theme != nil {
		return theme, true
	}
	return nil, false
}

func customTheme(name string, custom *config.CustomTheme) *Theme {
	if custom == nil {
		return nil
	}
	themeName := name
	if themeName == "" {
		themeName = "custom"
	}
	return &Theme{
		Name: themeName,
		BG:   custom.BG,
		FG:   custom.FG,
	}
}

func normalizeThemeRole(role string) string {
	switch role {
	case "coordinator":
		return constants.RoleMayor
	case "health-check":
		return constants.RoleDeacon
	default:
		return role
	}
}
