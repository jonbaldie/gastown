// Package now implements the gt now five-second Mayor start path.
package now

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
)

const (
	MayorAlias   = "now-mayor"
	WorkersAlias = "now-workers"

	DefaultMayorEffort   = "high"
	DefaultWorkersEffort = "low"
)

// MayorRoles receive the Mayor profile.
var MayorRoles = []string{"mayor", "deacon"}

// WorkerRoles receive the worker profile.
var WorkerRoles = []string{"witness", "polecat", "refinery", "crew", "boot", "dog"}

// Profile is a runtime, optional model, and effort parsed from --mayor/--workers.
type Profile struct {
	Runtime string
	Model   string
	Effort  string
}

// ParseProfile parses runtime[:model[:effort]].
// An empty spec returns a profile with only defaultEffort set.
func ParseProfile(spec, defaultEffort string, supportsEffort func(runtime, token string) bool) (Profile, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Profile{Effort: defaultEffort}, nil
	}

	parts := strings.SplitN(spec, ":", 3)
	runtime := strings.TrimSpace(parts[0])
	if runtime == "" {
		return Profile{}, fmt.Errorf("empty runtime in %q", spec)
	}

	profile := Profile{Runtime: runtime, Effort: defaultEffort}
	switch len(parts) {
	case 1:
		return profile, nil
	case 2:
		token := strings.TrimSpace(parts[1])
		if token == "" {
			return Profile{}, fmt.Errorf("empty model or effort in %q", spec)
		}
		if supportsEffort != nil && supportsEffort(runtime, token) {
			profile.Effort = token
			return profile, nil
		}
		profile.Model = token
		return profile, nil
	default:
		model := strings.TrimSpace(parts[1])
		effort := strings.TrimSpace(parts[2])
		if model == "" {
			return Profile{}, fmt.Errorf("empty model in %q", spec)
		}
		if effort == "" {
			return Profile{}, fmt.Errorf("empty effort in %q", spec)
		}
		if strings.Contains(model, ":") {
			return Profile{}, fmt.Errorf("model may not contain ':' (got %q)", spec)
		}
		profile.Model = model
		profile.Effort = effort
		return profile, nil
	}
}

// SupportsEffort reports whether the named runtime accepts the effort token.
func SupportsEffort(runtime, token string) bool {
	if !config.IsKnownPreset(runtime) {
		return config.IsValidEffortLevel(token)
	}
	rc := config.RuntimeConfigFromPreset(config.AgentPreset(runtime))
	return config.RuntimeSupportsEffort(rc, token)
}

// Format returns runtime, runtime:effort, runtime:model, or runtime:model:effort.
func (p Profile) Format() string {
	if p.Runtime == "" {
		return ""
	}
	switch {
	case p.Model != "" && p.Effort != "":
		return p.Runtime + ":" + p.Model + ":" + p.Effort
	case p.Model != "":
		return p.Runtime + ":" + p.Model
	case p.Effort != "":
		return p.Runtime + ":" + p.Effort
	default:
		return p.Runtime
	}
}
