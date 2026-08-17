package now

import (
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
)

// AliasConfig builds the now-mayor / now-workers runtime alias.
func AliasConfig(profile Profile) *config.RuntimeConfig {
	rc := config.RuntimeConfigFromPreset(config.AgentPreset(profile.Runtime))
	if rc == nil {
		rc = &config.RuntimeConfig{}
	}
	rc.Provider = profile.Runtime
	if profile.Model != "" {
		rc.Args = withModel(rc.Args, profile.Model)
	}
	return rc
}

// MixAssignments returns mix specs for a mayor or worker profile.
func MixAssignments(alias string, roles []string, effort string) []config.MixAssignment {
	assignments := make([]config.MixAssignment, 0, len(roles))
	for _, role := range roles {
		assignments = append(assignments, config.MixAssignment{
			Kind:   config.MixKindRole,
			Name:   role,
			Agent:  alias,
			Effort: effort,
		})
	}
	return assignments
}

func withModel(args []string, model string) []string {
	out := make([]string, 0, len(args)+2)
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "--model" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "--model=") {
			continue
		}
		out = append(out, arg)
	}
	return append(out, "--model", model)
}
