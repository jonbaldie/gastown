// Package mail provides address resolution for beads-native messaging.
// This module implements the resolution order:
// 1. Explicit prefix (group:, queue:, channel:, list:, announce:)
// 2. Starts with '@' → special pattern (@town, @crew, @rig/X, @role/X)
// 3. Contains '/' → agent address or pattern (validated against known agents)
// 4. Otherwise → lookup by name: group → queue → channel
// 5. If conflict, require prefix (group:X, queue:X, channel:X)
package mail

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/constants"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
)

// ErrUnknownRecipient indicates the address does not match any known agent.
// Callers should NOT fall back to legacy routing on this error — the address
// is definitively invalid, not just unresolvable by the new resolver.
var ErrUnknownRecipient = errors.New("unknown recipient")

// RecipientType indicates the type of resolved recipient.
type RecipientType string

const (
	RecipientAgent   RecipientType = "agent"   // Direct to agent(s)
	RecipientQueue   RecipientType = "queue"   // Single message, workers claim
	RecipientChannel RecipientType = "channel" // Broadcast, retained
)

// Recipient represents a resolved message recipient.
type Recipient struct {
	Address      string        // The resolved address (e.g., "gastown/crew/max")
	Type         RecipientType // Type of recipient (agent, queue, channel)
	OriginalName string        // Original name before resolution (for queues/channels)
}

// Resolver handles address resolution for beads-native messaging.
type Resolver struct {
	beads    *beads.Beads
	townRoot string
}

// NewResolver creates a new address resolver.
func NewResolver(b *beads.Beads, townRoot string) *Resolver {
	return &Resolver{
		beads:    b,
		townRoot: townRoot,
	}
}

// Resolve resolves an address to a list of recipients.
// Resolution order:
// 1. Contains '/' → agent address or pattern (direct delivery)
// 2. Starts with '@' → special pattern (@town, @crew, etc.)
// 3. Starts with explicit prefix → use that type (group:, queue:, channel:)
// 4. Otherwise → lookup by name: group → queue → channel
func (r *Resolver) Resolve(address string) ([]Recipient, error) {
	return r.resolveWithVisited(address, make(map[string]bool))
}

// resolveWithVisited resolves an address while threading cycle detection state.
func (r *Resolver) resolveWithVisited(address string, visited map[string]bool) ([]Recipient, error) {
	// 1. Explicit prefix takes precedence
	if strings.HasPrefix(address, "group:") {
		name := strings.TrimPrefix(address, "group:")
		return r.resolveBeadsGroupWithVisited(name, visited)
	}
	if strings.HasPrefix(address, "queue:") {
		name := strings.TrimPrefix(address, "queue:")
		return r.resolveQueue(name)
	}
	if strings.HasPrefix(address, "channel:") {
		name := strings.TrimPrefix(address, "channel:")
		return r.resolveChannel(name)
	}

	// Legacy prefixes (list:, announce:) - pass through
	if strings.HasPrefix(address, "list:") || strings.HasPrefix(address, "announce:") {
		// These are handled by existing router logic
		return []Recipient{{Address: address, Type: RecipientAgent}}, nil
	}

	// 2. Starts with '@' → special pattern (check before '/' since @rig/X contains '/')
	if strings.HasPrefix(address, "@") {
		return r.resolveAtPatternWithVisited(address, visited)
	}

	// 3. Contains '/' → agent address or pattern
	if strings.Contains(address, "/") {
		return r.resolveAgentAddress(address)
	}

	// 4. Name lookup: group → queue → channel
	return r.resolveByNameWithVisited(address, visited)
}

// resolveAgentAddress handles addresses containing '/'.
// These are either direct addresses or patterns.
func (r *Resolver) resolveAgentAddress(address string) ([]Recipient, error) {
	// Check for wildcard patterns
	if strings.Contains(address, "*") {
		return r.resolvePattern(address)
	}

	// Validate that the address refers to a known agent before accepting.
	// Without this check, typos like "laser/mayor" (instead of "mayor/")
	// silently deliver to a dead inbox with no error.
	// See: https://github.com/steveyegge/gastown/issues/2038
	if err := r.validateAgentAddress(address); err != nil {
		return nil, err
	}

	// Direct address - single recipient
	return []Recipient{{
		Address: address,
		Type:    RecipientAgent,
	}}, nil
}

// validateAgentAddress checks that a slash-containing address corresponds to
// a known agent. It checks well-known singletons, agent beads, and workspace
// directories. Returns nil if the agent exists, or an error with suggestions.
// If neither beads nor townRoot is available, validation is skipped (graceful
// degradation) and downstream validation in sendToSingle handles it.
func (r *Resolver) validateAgentAddress(address string) error {
	if r.beads == nil && r.townRoot == "" {
		return nil
	}
	return validateAgentAddressSources(r.beads, r.townRoot, address)
}

func validateAgentAddressSources(b *beads.Beads, townRoot, address string) error {
	if hasUnsafeAddressSegment(address) {
		return unknownRecipientError(address)
	}
	normalized := normalizeAddress(strings.TrimSuffix(address, "/"))
	handled, validDog, err := validateSpecialAddress(normalized, address)
	if err != nil {
		return err
	}
	if handled {
		return nil
	}
	parts := strings.SplitN(normalized, "/", 3)
	if len(parts) < 2 || parts[1] == "" {
		return unknownRecipientError(address)
	}
	if valid, err := validateResolverRigSingleton(townRoot, parts, address); valid || err != nil {
		return err
	}
	if knownAgentAddress(b, townRoot, normalized, parts, validDog) {
		return nil
	}
	return fmt.Errorf("%w: %s (no matching agent or workspace found)", ErrUnknownRecipient, address)
}

func knownAgentAddress(b *beads.Beads, townRoot, normalized string, parts []string, validDog bool) bool {
	return beadsContainAgent(b, normalized) || workspaceContainsAgent(townRoot, parts, validDog)
}

func unknownRecipientError(address string) error {
	return fmt.Errorf("%w: %s", ErrUnknownRecipient, address)
}

func validateSpecialAddress(normalized, address string) (handled, validDog bool, err error) {
	switch normalized {
	case constants.RoleMayor + "/", constants.RoleMayor, constants.RoleDeacon + "/", constants.RoleDeacon, "overseer":
		return true, false, nil
	}
	if _, ok := DogAddressName(normalized); ok {
		return false, true, nil
	}
	if isReservedTownSubpath(normalized) {
		return false, false, unknownRecipientError(address)
	}
	return false, false, nil
}

func validateResolverRigSingleton(townRoot string, parts []string, address string) (bool, error) {
	if len(parts) != 2 || (parts[1] != constants.RoleWitness && parts[1] != constants.RoleRefinery) {
		return false, nil
	}
	if townRoot == "" || dirExistsAt(filepath.Join(townRoot, parts[0])) {
		return true, nil
	}
	return true, unknownRecipientError(address)
}

func beadsContainAgent(b *beads.Beads, normalized string) bool {
	if b == nil {
		return false
	}
	agents, err := b.ListAgentBeads()
	if err != nil {
		return false
	}
	for id := range agents {
		addr := AgentBeadIDToAddress(id)
		if addr != "" && normalizeAddress(addr) == normalized {
			return true
		}
	}
	return false
}

func workspaceContainsAgent(townRoot string, parts []string, validDog bool) bool {
	if townRoot == "" {
		return false
	}
	switch len(parts) {
	case 2:
		return workspaceContainsNamedAgent(townRoot, parts[0], parts[1])
	case 3:
		return workspaceContainsExplicitAgent(townRoot, parts, validDog)
	default:
		return false
	}
}

func workspaceContainsNamedAgent(townRoot, rig, name string) bool {
	if dirExistsAt(filepath.Join(townRoot, rig, name)) {
		return true
	}
	for _, role := range []string{"crew", "polecats"} {
		if dirExistsAt(filepath.Join(townRoot, rig, role, name)) {
			return true
		}
	}
	return false
}

func workspaceContainsExplicitAgent(townRoot string, parts []string, validDog bool) bool {
	validRole := parts[1] == constants.RoleCrew || parts[1] == "polecats" || (validDog && parts[1] == "dogs")
	return validRole && dirExistsAt(filepath.Join(townRoot, parts[0], parts[1], parts[2]))
}

// dirExistsAt returns true if path exists and is a directory.
func dirExistsAt(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// resolvePattern expands a wildcard pattern to matching agents.
// Patterns like "*/witness" or "gastown/*" are expanded.
func (r *Resolver) resolvePattern(pattern string) ([]Recipient, error) {
	if r.beads == nil {
		return nil, fmt.Errorf("beads not available for pattern resolution")
	}

	// Get all agent beads
	agents, err := r.beads.ListAgentBeads()
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}

	var recipients []Recipient
	for id := range agents {
		// Convert bead ID to address and check match
		addr := AgentBeadIDToAddress(id)
		if addr != "" && matchPattern(pattern, addr) {
			recipients = append(recipients, Recipient{
				Address: addr,
				Type:    RecipientAgent,
			})
		}
	}

	if len(recipients) == 0 {
		return nil, fmt.Errorf("no agents match pattern: %s", pattern)
	}

	return recipients, nil
}

// resolveAtPatternWithVisited handles @-prefixed patterns with cycle detection.
func (r *Resolver) resolveAtPatternWithVisited(address string, visited map[string]bool) ([]Recipient, error) {
	// First check if this is a beads-native group (if beads available)
	if r.beads != nil {
		groupName := strings.TrimPrefix(address, "@")
		_, fields, err := r.beads.LookupGroupByName(groupName)
		if err != nil && !errors.Is(err, beads.ErrNotFound) {
			return nil, err
		}
		if err == nil && fields != nil {
			// Found a beads-native group - expand its members
			return r.expandGroupMembersWithVisited(fields, visited)
		}
	}

	// Fall back to built-in patterns (handled by existing router)
	// Return as-is for router to handle
	return []Recipient{{Address: address, Type: RecipientAgent}}, nil
}

// resolveByNameWithVisited looks up a name with cycle detection.
func (r *Resolver) resolveByNameWithVisited(name string, visited map[string]bool) ([]Recipient, error) {
	return resolveByName(r, name, visited)
}

func resolveByName(r *Resolver, name string, visited map[string]bool) ([]Recipient, error) {
	matches, err := lookupNameMatches(r.beads, r.townRoot, name)
	if err != nil {
		return nil, err
	}
	return resolveNameMatch(r, name, visited, matches)
}

type nameMatches struct {
	group   *beads.GroupFields
	queue   bool
	channel bool
}

func lookupNameMatches(b *beads.Beads, townRoot, name string) (nameMatches, error) {
	matches, err := lookupBeadsNameMatches(b, name)
	if err != nil {
		return nameMatches{}, err
	}
	if townRoot == "" {
		return matches, nil
	}
	return addConfigNameMatches(townRoot, name, matches), nil
}

func lookupBeadsNameMatches(b *beads.Beads, name string) (nameMatches, error) {
	if b == nil {
		return nameMatches{}, nil
	}
	var matches nameMatches
	_, fields, err := b.LookupGroupByName(name)
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return nameMatches{}, err
	}
	if err == nil && fields != nil {
		matches.group = fields
	}
	_, queueFields, err := b.LookupQueueByName(name)
	if err != nil {
		return nameMatches{}, err
	}
	matches.queue = queueFields != nil
	_, channelFields, err := b.LookupChannelByName(name)
	if err != nil {
		return nameMatches{}, err
	}
	matches.channel = channelFields != nil
	return matches, nil
}

func addConfigNameMatches(townRoot, name string, matches nameMatches) nameMatches {
	cfg, err := config.LoadMessagingConfig(config.MessagingConfigPath(townRoot))
	if err != nil || cfg == nil {
		return matches
	}
	if _, ok := cfg.Queues[name]; ok {
		matches.queue = true
	}
	if _, ok := cfg.Announces[name]; ok {
		matches.channel = true
	}
	return matches
}

func resolveNameMatch(r *Resolver, name string, visited map[string]bool, matches nameMatches) ([]Recipient, error) {
	conflictCount := countNameMatches(matches)
	if conflictCount == 0 {
		return nil, fmt.Errorf("unknown address: %s (not a group, queue, or channel)", name)
	}
	if conflictCount > 1 {
		return nil, ambiguousNameError(name, matches)
	}
	if matches.group != nil {
		return r.expandGroupMembersWithVisited(matches.group, visited)
	}
	if matches.queue {
		return r.resolveQueue(name)
	}
	return r.resolveChannel(name)
}

func countNameMatches(matches nameMatches) int {
	count := 0
	if matches.group != nil {
		count++
	}
	if matches.queue {
		count++
	}
	if matches.channel {
		count++
	}
	return count
}

func ambiguousNameError(name string, matches nameMatches) error {
	var types []string
	if matches.group != nil {
		types = append(types, "group:"+name)
	}
	if matches.queue {
		types = append(types, "queue:"+name)
	}
	if matches.channel {
		types = append(types, "channel:"+name)
	}
	return fmt.Errorf("ambiguous address %q: matches multiple types. Use explicit prefix: %s", name, strings.Join(types, ", "))
}

// resolveBeadsGroupWithVisited resolves a beads-native group with cycle detection.
func (r *Resolver) resolveBeadsGroupWithVisited(name string, visited map[string]bool) ([]Recipient, error) {
	if r.beads == nil {
		return nil, fmt.Errorf("beads not available")
	}

	_, fields, err := r.beads.LookupGroupByName(name)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, fmt.Errorf("group not found: %s", name)
		}
		return nil, err
	}

	return r.expandGroupMembersWithVisited(fields, visited)
}

// expandGroupMembersWithVisited expands group members with cycle detection.
func (r *Resolver) expandGroupMembersWithVisited(fields *beads.GroupFields, visited map[string]bool) ([]Recipient, error) {
	if fields == nil {
		return nil, nil
	}

	// Mark this group as visited for cycle detection
	if fields.Name != "" {
		if visited[fields.Name] {
			// Cycle detected - skip silently (as per design: "silent skip with warning")
			return nil, nil
		}
		visited[fields.Name] = true
	}

	seen := make(map[string]bool)
	var recipients []Recipient

	for _, member := range fields.Members {
		// Recursively resolve each member
		resolved, err := r.resolveMemberWithVisited(member, visited)
		if err != nil {
			// Log warning but continue with other members
			continue
		}

		for _, rec := range resolved {
			// Deduplicate
			if !seen[rec.Address] {
				seen[rec.Address] = true
				recipients = append(recipients, rec)
			}
		}
	}

	return recipients, nil
}

// resolveMemberWithVisited resolves a single group member with cycle detection.
func (r *Resolver) resolveMemberWithVisited(member string, visited map[string]bool) ([]Recipient, error) {
	// Check if this is a nested group reference
	if r.beads != nil && !strings.Contains(member, "/") && !strings.HasPrefix(member, "@") {
		_, fields, err := r.beads.LookupGroupByName(member)
		if err == nil && fields != nil {
			return r.expandGroupMembersWithVisited(fields, visited)
		}
	}

	// Otherwise resolve with the same visited map to maintain cycle detection
	return r.resolveWithVisited(member, visited)
}

// resolveQueue returns a queue recipient.
func (r *Resolver) resolveQueue(name string) ([]Recipient, error) {
	return []Recipient{{
		Address:      "queue:" + name,
		Type:         RecipientQueue,
		OriginalName: name,
	}}, nil
}

// resolveChannel returns a channel recipient.
func (r *Resolver) resolveChannel(name string) ([]Recipient, error) {
	return []Recipient{{
		Address:      "channel:" + name,
		Type:         RecipientChannel,
		OriginalName: name,
	}}, nil
}

// AgentBeadIDToAddress converts an agent bead ID to a mail address.
// Accepts any beads prefix that ParseAgentBeadID understands:
//   - hq-mayor → mayor/
//   - bd-mayor → mayor/
//   - gt-gastown-crew-max → gastown/crew/max
//   - ff-witness → ff/witness (collapsed prefix==rig)
func AgentBeadIDToAddress(id string) string {
	parsed, ok := beads.ParseAgentBeadID(id)
	if !ok {
		return ""
	}

	if parsed.Role == constants.RoleMayor || parsed.Role == constants.RoleDeacon {
		return parsed.Role + "/"
	}
	if parsed.Role == "dog" {
		return DogAddress(parsed.Name)
	}
	return scopedAgentAddress(parsed.Rig, parsed.Role, parsed.Name)
}

func scopedAgentAddress(rig, role, name string) string {
	switch role {
	case constants.RoleWitness, constants.RoleRefinery:
		if rig == "" {
			return ""
		}
		return rig + "/" + role
	case constants.RoleCrew:
		if rig == "" || name == "" {
			return ""
		}
		return rig + "/crew/" + name
	case constants.RolePolecat:
		if rig == "" || name == "" {
			return ""
		}
		return rig + "/polecats/" + name
	default:
		return ""
	}
}

// matchPattern checks if an address matches a wildcard pattern.
// '*' matches any single path segment (no slashes).
func matchPattern(pattern, address string) bool {
	patternParts := strings.Split(pattern, "/")
	addressParts := strings.Split(address, "/")

	if len(patternParts) != len(addressParts) {
		return false
	}

	for i, p := range patternParts {
		if p == "*" {
			continue // Wildcard matches anything
		}
		if p != addressParts[i] {
			return false
		}
	}

	return true
}
