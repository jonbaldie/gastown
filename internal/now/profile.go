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
// A two-token spec treats a globally known effort level as effort even when
// the runtime does not accept it; validateProfile rejects that later.
//
// defaultEffort must be a recognized effort level: Format() encodes a
// Model-less profile as "runtime:effort", and re-parsing that string only
// recognizes the second token as an effort when it is a known level. An
// unrecognized defaultEffort would silently round-trip into a Model instead.
func ParseProfile(spec, defaultEffort string) (Profile, error) {
	if !config.IsValidEffortLevel(defaultEffort) {
		return Profile{}, fmt.Errorf("invalid default effort %q", defaultEffort)
	}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Profile{Effort: defaultEffort}, nil
	}

	profile, parts, err := newProfile(spec, defaultEffort)
	if err != nil {
		return Profile{}, err
	}
	switch len(parts) {
	case 1:
		return profile, nil
	case 2:
		return parseProfileToken(profile, spec, parts[1])
	default:
		return parseProfileModelAndEffort(profile, spec, parts[1], parts[2])
	}
}

func newProfile(spec, defaultEffort string) (Profile, []string, error) {
	parts := strings.SplitN(spec, ":", 3)
	runtime := strings.TrimSpace(parts[0])
	if runtime == "" {
		return Profile{}, nil, fmt.Errorf("empty runtime in %q", spec)
	}
	return Profile{Runtime: runtime, Effort: defaultEffort}, parts, nil
}

func parseProfileToken(profile Profile, spec, token string) (Profile, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Profile{}, fmt.Errorf("empty model or effort in %q", spec)
	}
	if config.IsValidEffortLevel(token) {
		profile.Effort = token
	} else {
		profile.Model = token
	}
	return profile, nil
}

func parseProfileModelAndEffort(profile Profile, spec, model, effort string) (Profile, error) {
	model, effort = strings.TrimSpace(model), strings.TrimSpace(effort)
	if model == "" {
		return Profile{}, fmt.Errorf("empty model in %q", spec)
	}
	if effort == "" {
		return Profile{}, fmt.Errorf("empty effort in %q", spec)
	}
	if strings.Contains(model, ":") {
		return Profile{}, fmt.Errorf("model may not contain ':' (got %q)", spec)
	}
	profile.Model, profile.Effort = model, effort
	return profile, nil
}

// SupportsEffort reports whether the named runtime accepts the effort token.
func SupportsEffort(runtime, token string) bool {
	if !config.IsKnownPreset(runtime) {
		return config.IsValidEffortLevel(token)
	}
	rc := config.RuntimeConfigFromPreset(config.AgentPreset(runtime))
	return config.RuntimeSupportsEffort(rc, token)
}

// ResolveProfiles parses, detects, and validates --mayor and --workers specs.
func ResolveProfiles(mayorSpec, workersSpec string) (Profile, Profile, error) {
	mayorProfile, err := ParseProfile(mayorSpec, DefaultMayorEffort)
	if err != nil {
		return Profile{}, Profile{}, fmt.Errorf("parsing --mayor: %w", err)
	}
	workersProfile, err := ParseProfile(workersSpec, DefaultWorkersEffort)
	if err != nil {
		return Profile{}, Profile{}, fmt.Errorf("parsing --workers: %w", err)
	}
	if err := fillRuntimes(&mayorProfile, &workersProfile); err != nil {
		return Profile{}, Profile{}, err
	}
	if err := validateProfile(mayorProfile, "--mayor"); err != nil {
		return Profile{}, Profile{}, err
	}
	if err := validateProfile(workersProfile, "--workers"); err != nil {
		return Profile{}, Profile{}, err
	}
	return mayorProfile, workersProfile, nil
}

func fillRuntimes(mayorProfile, workersProfile *Profile) error {
	detected := ""
	fill := func(profile *Profile) error {
		if profile.Runtime != "" {
			return nil
		}
		if detected == "" {
			var err error
			detected, err = DetectRuntime()
			if err != nil {
				return err
			}
		}
		profile.Runtime = detected
		return nil
	}
	if err := fill(mayorProfile); err != nil {
		return err
	}
	return fill(workersProfile)
}

func validateProfile(profile Profile, flag string) error {
	if profile.Runtime == "" {
		return fmt.Errorf("%s: missing runtime", flag)
	}
	if !config.IsKnownPreset(profile.Runtime) {
		return fmt.Errorf("%s: unknown runtime %q", flag, profile.Runtime)
	}
	if !RuntimePresent(profile.Runtime) {
		return fmt.Errorf("%s: %s not found on PATH", flag, RuntimeCommand(profile.Runtime))
	}
	if !SupportsEffort(profile.Runtime, profile.Effort) {
		return fmt.Errorf("%s: invalid effort %q for runtime %s", flag, profile.Effort, profile.Runtime)
	}
	return nil
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
