// Package session provides polecat session lifecycle management.
package session

import (
	"fmt"
	"strings"
)

// matchPrefix finds the prefix in a session name suffix using the registry.
// It tries registered prefixes longest first.
func (r *PrefixRegistry) matchPrefix(session string) (prefix, rest string, matched bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.sortedPrefixes() {
		candidate := p + "-"
		if strings.HasPrefix(session, candidate) {
			return p, session[len(candidate):], true
		}
	}
	return "", "", false
}

// Role represents the type of Gas Town agent.
type Role string

const (
	RoleMayor    Role = "mayor"
	RoleDeacon   Role = "deacon"
	RoleOverseer Role = "overseer"
	RoleWitness  Role = "witness"
	RoleRefinery Role = "refinery"
	RoleCrew     Role = "crew"
	RolePolecat  Role = "polecat"
	RoleDog      Role = "dog"
)

// AgentIdentity represents a parsed Gas Town agent identity.
type AgentIdentity struct {
	Role   Role   // mayor, deacon, witness, refinery, crew, polecat, dog
	Rig    string // rig name (empty for mayor/deacon/dog)
	Name   string // crew/polecat/dog name (empty for mayor/deacon/witness/refinery)
	Prefix string // beads prefix for rig-level agents (e.g., "gt", "bd", "hop")
}

// ParseAddress parses a mail-style address into an AgentIdentity.
func ParseAddress(address string) (*AgentIdentity, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, fmt.Errorf("empty address")
	}
	if identity, handled, err := parseTownAddress(address); handled {
		return identity, err
	}
	address = strings.TrimSuffix(address, "/")
	parts := strings.Split(address, "/")
	if !validAddressParts(parts) {
		return nil, fmt.Errorf("invalid address %q", address)
	}
	identity, ok := parseRigAddress(parts)
	if !ok {
		return nil, fmt.Errorf("invalid address %q", address)
	}
	return identity, nil
}

func parseTownAddress(address string) (*AgentIdentity, bool, error) {
	switch address {
	case string(RoleMayor), string(RoleMayor) + "/":
		return &AgentIdentity{Role: RoleMayor}, true, nil
	case string(RoleDeacon), string(RoleDeacon) + "/":
		return &AgentIdentity{Role: RoleDeacon}, true, nil
	case string(RoleOverseer):
		return nil, true, fmt.Errorf("overseer has no session")
	default:
		return nil, false, nil
	}
}

func validAddressParts(parts []string) bool {
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, `\`) {
			return false
		}
	}
	return true
}

func parseRigAddress(parts []string) (*AgentIdentity, bool) {
	rig := parts[0]
	prefix := PrefixFor(rig)
	switch len(parts) {
	case 2:
		return parseShortRigAddress(rig, prefix, parts[1])
	case 3:
		return parseLongRigAddress(rig, prefix, parts[1], parts[2])
	default:
		return nil, false
	}
}

func parseShortRigAddress(rig, prefix, name string) (*AgentIdentity, bool) {
	switch name {
	case string(RoleWitness):
		return &AgentIdentity{Role: RoleWitness, Rig: rig, Prefix: prefix}, true
	case string(RoleRefinery):
		return &AgentIdentity{Role: RoleRefinery, Rig: rig, Prefix: prefix}, true
	case string(RoleCrew), "polecats", "dogs":
		return nil, false
	default:
		return &AgentIdentity{Role: RolePolecat, Rig: rig, Name: name, Prefix: prefix}, true
	}
}

func parseLongRigAddress(rig, prefix, role, name string) (*AgentIdentity, bool) {
	switch role {
	case string(RoleCrew):
		return &AgentIdentity{Role: RoleCrew, Rig: rig, Name: name, Prefix: prefix}, true
	case "polecats":
		return &AgentIdentity{Role: RolePolecat, Rig: rig, Name: name, Prefix: prefix}, true
	case "dogs":
		if rig == string(RoleDeacon) && safeDogName(name) {
			return &AgentIdentity{Role: RoleDog, Name: name}, true
		}
		return nil, false
	default:
		return nil, false
	}
}

func safeDogName(name string) bool {
	return name != "" &&
		name != "." &&
		name != ".." &&
		!strings.ContainsAny(name, `/\\`) &&
		!strings.Contains(name, "..")
}

// ParseSessionName parses a tmux session name into an AgentIdentity.
// Uses the default PrefixRegistry to resolve rig-level prefixes to rig names.
//
// Session name formats:
//   - hq-mayor → Role: mayor (town-level, one per machine)
//   - hq-deacon → Role: deacon (town-level, one per machine)
//   - hq-boot → Role: deacon, Name: boot (boot watchdog)
//   - <prefix>-witness → Role: witness (e.g., gt-witness for gastown)
//   - <prefix>-refinery → Role: refinery (e.g., gt-refinery for gastown)
//   - <prefix>-crew-<name> → Role: crew (e.g., gt-crew-max for gastown)
//   - <prefix>-polecat_crew-<name> → Role: polecat named crew-<name>
//   - <prefix>-<name> → Role: polecat (e.g., gt-furiosa for gastown)
//
// The prefix is the rig's beads prefix (e.g., "gt" for gastown, "dolt" for beads).
// The rig name is resolved from the default PrefixRegistry. If the prefix is
// not in the registry, the prefix itself is used as the rig name.
func ParseSessionName(session string) (*AgentIdentity, error) {
	return ParseSessionNameWithRegistry(session, DefaultRegistry())
}

// ParseSessionNameWithRegistry parses a tmux session name using a specific registry.
// If registry is nil, an empty registry is used (prefix will not resolve to rig name).
func ParseSessionNameWithRegistry(session string, registry *PrefixRegistry) (*AgentIdentity, error) {
	if registry == nil {
		registry = NewPrefixRegistry()
	}
	if identity, handled, err := parseTownSessionName(session); handled {
		return identity, err
	}
	prefix, rest, _ := registry.MatchPrefix(session)
	if prefix == "" || rest == "" {
		return nil, fmt.Errorf("invalid session name %q: cannot determine prefix", session)
	}
	return parseRigSessionName(session, registry.RigForPrefix(prefix), prefix, rest)
}

func parseTownSessionName(session string) (*AgentIdentity, bool, error) {
	if !strings.HasPrefix(session, HQPrefix) {
		return nil, false, nil
	}
	suffix := strings.TrimPrefix(session, HQPrefix)
	switch suffix {
	case string(RoleMayor):
		return &AgentIdentity{Role: RoleMayor}, true, nil
	case string(RoleDeacon):
		return &AgentIdentity{Role: RoleDeacon}, true, nil
	case "boot":
		return &AgentIdentity{Role: RoleDeacon, Name: "boot"}, true, nil
	case string(RoleOverseer):
		return &AgentIdentity{Role: RoleOverseer}, true, nil
	}
	if !strings.HasPrefix(suffix, "dog-") {
		return nil, false, nil
	}
	name := strings.TrimPrefix(suffix, "dog-")
	if name == "" {
		return nil, true, fmt.Errorf("invalid session name %q: empty dog name", session)
	}
	return &AgentIdentity{Role: RoleDog, Name: name}, true, nil
}

func parseRigSessionName(session, rig, prefix, rest string) (*AgentIdentity, error) {
	identity := &AgentIdentity{Rig: rig, Prefix: prefix}
	switch {
	case rest == string(RoleWitness):
		identity.Role = RoleWitness
	case rest == string(RoleRefinery):
		identity.Role = RoleRefinery
	case strings.HasPrefix(rest, "polecat_crew-"):
		identity.Role, identity.Name = RolePolecat, strings.TrimPrefix(rest, "polecat_")
	case strings.HasPrefix(rest, "crew-"):
		identity.Role, identity.Name = RoleCrew, strings.TrimPrefix(rest, "crew-")
		if identity.Name == "" {
			return nil, fmt.Errorf("invalid session name %q: empty crew name", session)
		}
	default:
		identity.Role, identity.Name = RolePolecat, rest
	}
	return identity, nil
}

// SessionName returns the tmux session name for this identity.
func (a *AgentIdentity) SessionName() string {
	if name, ok := a.townSessionName(); ok {
		return name
	}
	switch a.Role {
	case RoleWitness:
		return WitnessSessionName(a.prefix())
	case RoleRefinery:
		return RefinerySessionName(a.prefix())
	case RoleCrew:
		return CrewSessionName(a.prefix(), a.Name)
	case RolePolecat:
		return PolecatSessionName(a.prefix(), a.Name)
	case RoleDog:
		return DogSessionName(a.Name)
	default:
		return ""
	}
}

func (a *AgentIdentity) townSessionName() (string, bool) {
	switch a.Role {
	case RoleMayor:
		return MayorSessionName(), true
	case RoleDeacon:
		if a.Name == "boot" {
			return BootSessionName(), true
		}
		return DeaconSessionName(), true
	case RoleOverseer:
		return OverseerSessionName(), true
	default:
		return "", false
	}
}

// prefix returns the rig prefix, falling back to registry lookup or DefaultPrefix.
func (a *AgentIdentity) prefix() string {
	if a.Prefix != "" {
		return a.Prefix
	}
	if a.Rig != "" {
		return PrefixFor(a.Rig)
	}
	return DefaultPrefix
}

// BeaconAddress returns a human-readable, non-path-like address for use in
// startup beacons. Unlike Address(), this format prevents LLMs from
// misinterpreting the recipient as a filesystem path.
// Examples:
//   - mayor → "mayor"
//   - deacon → "deacon"
//   - witness → "witness (rig: gastown)"
//   - crew → "crew max (rig: gastown)"
//   - polecat → "polecat Toast (rig: gastown)"
func (a *AgentIdentity) BeaconAddress() string {
	switch a.Role {
	case RoleMayor:
		return "mayor"
	case RoleDeacon:
		return "deacon"
	case RoleOverseer:
		return "overseer"
	case RoleWitness:
		return BeaconRecipient("witness", "", a.Rig)
	case RoleRefinery:
		return BeaconRecipient("refinery", "", a.Rig)
	case RoleCrew:
		return BeaconRecipient("crew", a.Name, a.Rig)
	case RolePolecat:
		return BeaconRecipient("polecat", a.Name, a.Rig)
	case RoleDog:
		return BeaconRecipient("dog", a.Name, "")
	default:
		return ""
	}
}

// Address returns the mail-style address for this identity.
// Examples:
//   - mayor → "mayor"
//   - deacon → "deacon"
//   - witness → "gastown/witness"
//   - refinery → "gastown/refinery"
//   - crew → "gastown/crew/max"
//   - polecat → "gastown/polecats/Toast"
func (a *AgentIdentity) Address() string {
	switch a.Role {
	case RoleMayor:
		return "mayor"
	case RoleDeacon:
		return "deacon"
	case RoleOverseer:
		return "overseer"
	case RoleWitness:
		return fmt.Sprintf("%s/witness", a.Rig)
	case RoleRefinery:
		return fmt.Sprintf("%s/refinery", a.Rig)
	case RoleCrew:
		return fmt.Sprintf("%s/crew/%s", a.Rig, a.Name)
	case RolePolecat:
		return fmt.Sprintf("%s/polecats/%s", a.Rig, a.Name)
	case RoleDog:
		return fmt.Sprintf("deacon/dogs/%s", a.Name)
	default:
		return ""
	}
}

// GTRole returns the GT_ROLE environment variable format.
// This is the same as Address() for most roles, except boot
// which is a deacon variant with its own role identity.
func (a *AgentIdentity) GTRole() string {
	if a.Role == RoleDeacon && a.Name == "boot" {
		return "boot"
	}
	return a.Address()
}
