package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/nudge"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/telemetry"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
)

// ErrUnknownList indicates a mailing list name was not found in configuration.
var ErrUnknownList = errors.New("unknown mailing list")

// ErrUnknownQueue indicates a queue name was not found in configuration.
var ErrUnknownQueue = errors.New("unknown queue")

// ErrUnknownAnnounce indicates an announce channel name was not found in configuration.
var ErrUnknownAnnounce = errors.New("unknown announce channel")

// Router handles message delivery via beads.
// It routes messages to the correct beads database based on address:
// - Town-level (mayor/, deacon/) -> {townRoot}/.beads
// - Rig-level (rig/polecat) -> {townRoot}/{rig}/.beads
type Router struct {
	workDir  string // fallback directory to run bd commands in
	townRoot string // town root directory (e.g., ~/gt)
	tmux     *tmux.Tmux

	notifyWg sync.WaitGroup // tracks in-flight async notifications
}

// NewRouter creates a new mail router.
// workDir should be a directory containing a .beads database.
// The town root is auto-detected from workDir if possible.
func NewRouter(workDir string) *Router {
	// Try to detect town root from workDir
	townRoot := detectTownRoot(workDir)

	return &Router{
		workDir:  workDir,
		townRoot: townRoot,
		tmux:     tmux.NewTmux(),
	}
}

// NewRouterWithTownRoot creates a router with an explicit town root.
func NewRouterWithTownRoot(workDir, townRoot string) *Router {
	return &Router{
		workDir:  workDir,
		townRoot: townRoot,
		tmux:     tmux.NewTmux(),
	}
}

// WaitPendingNotifications blocks until all in-flight async notifications
// have completed. CLI commands should call this before exiting to avoid
// losing notifications that are still being delivered.
func WaitPendingNotifications(r *Router) {
	r.notifyWg.Wait()
}

// isListAddress returns true if the address uses list:name syntax.
func isListAddress(address string) bool {
	return strings.HasPrefix(address, "list:")
}

// parseListName extracts the list name from a list:name address.
func parseListName(address string) string {
	return strings.TrimPrefix(address, "list:")
}

// isQueueAddress returns true if the address uses queue:name syntax.
func isQueueAddress(address string) bool {
	return strings.HasPrefix(address, "queue:")
}

// parseQueueName extracts the queue name from a queue:name address.
func parseQueueName(address string) string {
	return strings.TrimPrefix(address, "queue:")
}

// isAnnounceAddress returns true if the address uses announce:name syntax.
func isAnnounceAddress(address string) bool {
	return strings.HasPrefix(address, "announce:")
}

// parseAnnounceName extracts the announce channel name from an announce:name address.
func parseAnnounceName(address string) string {
	return strings.TrimPrefix(address, "announce:")
}

// isChannelAddress returns true if the address uses channel:name syntax (beads-native channels).
func isChannelAddress(address string) bool {
	return strings.HasPrefix(address, "channel:")
}

// parseChannelName extracts the channel name from a channel:name address.
func parseChannelName(address string) string {
	return strings.TrimPrefix(address, "channel:")
}

// expandFromConfig is a generic helper for config-based expansion.
// It loads the messaging config and calls the getter to extract the desired value.
// This consolidates the common pattern of: check townRoot, load config, lookup in map.
func expandFromConfig[T any](r *Router, name string, getter func(*config.MessagingConfig) (T, bool), errType error) (T, error) {
	var zero T
	if r.townRoot == "" {
		return zero, fmt.Errorf("%w: %s (no town root)", errType, name)
	}

	configPath := config.MessagingConfigPath(r.townRoot)
	cfg, err := config.LoadMessagingConfig(configPath)
	if err != nil {
		return zero, fmt.Errorf("loading messaging config: %w", err)
	}

	result, ok := getter(cfg)
	if !ok {
		return zero, fmt.Errorf("%w: %s", errType, name)
	}

	return result, nil
}

// expandList returns the recipients for a mailing list.
// Returns ErrUnknownList if the list is not found.
func expandList(r *Router, listName string) ([]string, error) {
	recipients, err := expandFromConfig(r, listName, func(cfg *config.MessagingConfig) ([]string, bool) {
		r, ok := cfg.Lists[listName]
		return r, ok
	}, ErrUnknownList)
	if err != nil {
		return nil, err
	}

	if len(recipients) == 0 {
		return nil, fmt.Errorf("%w: %s (empty list)", ErrUnknownList, listName)
	}

	return recipients, nil
}

// expandQueue returns the QueueConfig for a queue name.
// Returns ErrUnknownQueue if the queue is not found.
func expandQueue(r *Router, queueName string) (*config.QueueConfig, error) {
	return expandFromConfig(r, queueName, func(cfg *config.MessagingConfig) (*config.QueueConfig, bool) {
		qc, ok := cfg.Queues[queueName]
		if !ok {
			return nil, false
		}
		return &qc, true
	}, ErrUnknownQueue)
}

// expandAnnounce returns the AnnounceConfig for an announce channel name.
// Returns ErrUnknownAnnounce if the channel is not found.
func expandAnnounce(r *Router, announceName string) (*config.AnnounceConfig, error) {
	return expandFromConfig(r, announceName, func(cfg *config.MessagingConfig) (*config.AnnounceConfig, bool) {
		ac, ok := cfg.Announces[announceName]
		if !ok {
			return nil, false
		}
		return &ac, true
	}, ErrUnknownAnnounce)
}

// detectTownRoot finds the town root directory.
//
// Uses workspace.Find which correctly handles nested workspaces by always
// searching to the filesystem root and returning the outermost workspace.
// Falls back to GT_TOWN_ROOT/GT_ROOT env vars when workspace.Find cannot
// locate a workspace (e.g., running from outside any workspace).
func detectTownRoot(startDir string) string {
	// workspace.Find handles nested workspaces correctly: it always searches
	// to the filesystem root and returns the outermost mayor/town.json match.
	townRoot, err := workspace.Find(startDir)
	if err == nil && townRoot != "" {
		return townRoot
	}

	// Fallback: try GT_TOWN_ROOT or GT_ROOT env vars when workspace detection
	// fails (e.g., running from outside any workspace directory).
	for _, envName := range []string{"GT_TOWN_ROOT", "GT_ROOT"} {
		if envRoot := os.Getenv(envName); envRoot != "" {
			if ok, _ := workspace.IsWorkspace(envRoot); ok {
				return envRoot
			}
		}
	}
	return ""
}

// resolveBeadsDir returns the correct .beads directory for mail delivery.
//
// All mail uses town beads ({townRoot}/.beads). Rig-level beads ({rig}/.beads)
// are for project issues only, not mail.
func resolveBeadsDir(r *Router) string {
	// If no town root, fall back to workDir's .beads
	if r.townRoot == "" {
		return filepath.Join(r.workDir, ".beads")
	}

	// All mail uses town-level beads
	return filepath.Join(r.townRoot, ".beads")
}

func ensureCustomTypes(beadsDir string) error {
	if err := beads.EnsureCustomTypes(beadsDir); err != nil {
		return fmt.Errorf("ensuring custom types: %w", err)
	}
	return nil
}

func buildLabels(msg *Message) []string {
	var labels []string
	labels = append(labels, "gt:message")
	if msg.Type == TypeEscalation {
		labels = append(labels, "gt:escalation")
	}
	labels = append(labels, "from:"+msg.From)
	labels = append(labels, "msg-type:"+string(msg.Type))
	labels = append(labels, DeliverySendLabels()...)
	if msg.ThreadID != "" {
		labels = append(labels, "thread:"+msg.ThreadID)
	}
	if msg.ReplyTo != "" {
		labels = append(labels, "reply-to:"+msg.ReplyTo)
	}
	for _, cc := range msg.CC {
		ccIdentity := AddressToIdentity(cc)
		labels = append(labels, "cc:"+ccIdentity)
	}
	return labels
}

// isTownLevelAddress returns true if the address is for a town-level agent or the overseer.
func isTownLevelAddress(address string) bool {
	addr := strings.TrimSuffix(address, "/")
	return addr == constants.RoleMayor || addr == constants.RoleDeacon || addr == "overseer"
}

// isGroupAddress returns true if the address is a @group address.
// Group addresses start with @ and resolve to multiple recipients.
func isGroupAddress(address string) bool {
	return strings.HasPrefix(address, "@")
}

// GroupType represents the type of group address.
type GroupType string

const (
	GroupTypeRig      GroupType = "rig"      // @rig/<rigname> - all agents in a rig
	GroupTypeTown     GroupType = "town"     // @town - all town-level agents
	GroupTypeRole     GroupType = "role"     // @witnesses, @dogs, etc. - all agents of a role
	GroupTypeRigRole  GroupType = "rig-role" // @crew/<rigname>, @polecats/<rigname> - role in a rig
	GroupTypeOverseer GroupType = "overseer" // @overseer - human operator
)

// ParsedGroup represents a parsed @group address.
type ParsedGroup struct {
	Type     GroupType
	RoleType string // witness, crew, polecat, dog, etc.
	Rig      string // rig name for rig-scoped groups
	Original string // original @group string
}

// parseGroupAddress parses a @group address into its components.
// Returns nil if the address is not a valid group address.
//
// Supported patterns:
//   - @rig/<rigname>: All agents in a rig
//   - @town: All town-level agents (mayor, deacon)
//   - @witnesses: All witnesses across rigs
//   - @crew/<rigname>: Crew workers in a specific rig
//   - @polecats/<rigname>: Polecats in a specific rig
//   - @dogs: All Deacon dogs
//   - @overseer: Human operator (special case)
func parseGroupAddress(address string) *ParsedGroup {
	if !isGroupAddress(address) {
		return nil
	}

	// Remove @ prefix
	group := strings.TrimPrefix(address, "@")
	if parsed := parseSpecialGroup(group, address); parsed != nil {
		return parsed
	}
	return parseScopedGroup(group, address)
}

func parseSpecialGroup(group, address string) *ParsedGroup {
	switch group {
	case "overseer":
		return &ParsedGroup{Type: GroupTypeOverseer, Original: address}
	case "town":
		return &ParsedGroup{Type: GroupTypeTown, Original: address}
	case "witnesses":
		return &ParsedGroup{Type: GroupTypeRole, RoleType: constants.RoleWitness, Original: address}
	case "dogs":
		return &ParsedGroup{Type: GroupTypeRole, RoleType: "dog", Original: address}
	case "refineries":
		return &ParsedGroup{Type: GroupTypeRole, RoleType: constants.RoleRefinery, Original: address}
	case "deacons":
		return &ParsedGroup{Type: GroupTypeRole, RoleType: constants.RoleDeacon, Original: address}
	}
	return nil
}

func parseScopedGroup(group, address string) *ParsedGroup {
	// Parse patterns with slashes: @rig/<name>, @crew/<rig>, @polecats/<rig>
	parts := strings.SplitN(group, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return nil // Invalid format
	}

	prefix, qualifier := parts[0], parts[1]
	switch prefix {
	case "rig":
		return &ParsedGroup{Type: GroupTypeRig, Rig: qualifier, Original: address}
	case constants.RoleCrew:
		return &ParsedGroup{Type: GroupTypeRigRole, RoleType: constants.RoleCrew, Rig: qualifier, Original: address}
	case "polecats":
		return &ParsedGroup{Type: GroupTypeRigRole, RoleType: constants.RolePolecat, Rig: qualifier, Original: address}
	default:
		return nil // Unknown group type
	}
}

// agentBead represents an agent bead as returned by bd list --label=gt:agent.
type agentBead struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	CreatedBy   string   `json:"created_by"`
	Type        string   `json:"issue_type"`
	Labels      []string `json:"labels"`
}

// agentBeadToAddress converts an agent bead to a mail address.
// Handles multiple ID formats:
//   - hq-mayor → mayor/
//   - hq-deacon → deacon/
//   - gt-gastown-crew-max → gastown/max (legacy)
//   - ppf-pyspark_pipeline_framework-polecat-Toast → pyspark_pipeline_framework/Toast (rig prefix)
func agentBeadToAddress(bead *agentBead) string {
	if bead == nil {
		return ""
	}

	id := bead.ID
	if addr := dogAddressFromAgentBeadID(id); addr != "" {
		return addr
	}
	if isDogAgentBeadIDWithoutName(id) {
		return ""
	}

	if strings.HasPrefix(id, "hq-") {
		return hqAgentAddress(bead)
	}

	if !strings.HasPrefix(id, "gt-") {
		// For rig-prefixed IDs, extract rig and role from description
		return parseRigAgentAddress(bead)
	}
	return legacyAgentAddress(strings.TrimPrefix(id, "gt-"))
}

func hqAgentAddress(bead *agentBead) string {
	switch bead.ID {
	case "hq-mayor":
		return "mayor/"
	case "hq-deacon":
		return "deacon/"
	default:
		// For other hq- agents, fall back to description parsing
		return parseAgentAddressFromDescription(bead.Description)
	}
}

func legacyAgentAddress(rest string) string {
	// Agent bead IDs include the role explicitly: gt-<rig>-<role>[-<name>]
	// Scan from right for known role markers to handle hyphenated rig names.
	parts := strings.Split(rest, "-")

	if len(parts) == 1 {
		// Town-level: gt-mayor, gt-deacon
		return parts[0] + "/"
	}

	// Scan from right for known role markers
	for i := len(parts) - 1; i >= 1; i-- {
		if address := legacyAddressAtMarker(parts, i); address != "" {
			return address
		}
	}

	// Fallback: assume first part is rig, rest is role/name
	if len(parts) == 2 {
		return parts[0] + "/" + parts[1]
	}
	return ""
}

func legacyAddressAtMarker(parts []string, index int) string {
	switch parts[index] {
	case constants.RoleWitness, constants.RoleRefinery:
		// Singleton role: rig is everything before the role
		rig := strings.Join(parts[:index], "-")
		return rig + "/" + parts[index]
	case constants.RoleCrew, constants.RolePolecat:
		// Named role: rig is before role, name is after (skip role in address)
		rig := strings.Join(parts[:index], "-")
		if index+1 < len(parts) {
			name := strings.Join(parts[index+1:], "-")
			return rig + "/" + name
		}
		return rig + "/"
	case "dog":
		// Town-level named: gt-dog-alpha
		return dogAddressFromParts(parts, index)
	default:
		return ""
	}
}

// parseRigAgentAddress extracts address from a rig-prefixed agent bead.
// ID format: <prefix>-<rig>-<role>[-<name>]
// Examples:
//   - ppf-pyspark_pipeline_framework-witness → pyspark_pipeline_framework/witness
//   - ppf-pyspark_pipeline_framework-polecat-Toast → pyspark_pipeline_framework/Toast
//   - bd-beads-crew-beavis → beads/beavis
func parseRigAgentAddress(bead *agentBead) string {
	metadata := parseAgentAddressMetadata(bead.Description)
	if !metadata.hasRigAndRole() {
		// Fallback: parse from bead ID by scanning for known role markers.
		// ID format: <prefix>-<rig>-<role>[-<name>]
		// Known rig-level roles: crew, polecat, witness, refinery
		return parseRigAgentAddressFromID(bead.ID)
	}

	// For singleton roles (witness, refinery), address is rig/role
	if isSingletonRole(metadata.roleType) {
		return metadata.rig + "/" + metadata.roleType
	}

	// For named roles (crew, polecat), extract name from ID
	// ID pattern: <prefix>-<rig>-<role>-<name>
	// Find the role in the ID and take everything after it as the name
	if name := namedAgentName(bead.ID, metadata.roleType); name != "" {
		return metadata.rig + "/" + name
	}

	// Fallback: return rig/roleType (may not be correct for all cases)
	return metadata.rig + "/" + metadata.roleType
}

type agentAddressMetadata struct {
	location string
	roleType string
	rig      string
}

func parseAgentAddressMetadata(desc string) agentAddressMetadata {
	var metadata agentAddressMetadata
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "location:"):
			metadata.location = strings.TrimSpace(strings.TrimPrefix(line, "location:"))
		case strings.HasPrefix(line, "role_type:"):
			metadata.roleType = strings.TrimSpace(strings.TrimPrefix(line, "role_type:"))
		case strings.HasPrefix(line, "rig:"):
			metadata.rig = strings.TrimSpace(strings.TrimPrefix(line, "rig:"))
		}
	}
	return metadata
}

func (m agentAddressMetadata) hasRigAndRole() bool {
	return m.rig != "" && m.rig != "null" && m.roleType != "" && m.roleType != "null"
}

func isSingletonRole(roleType string) bool {
	return roleType == constants.RoleWitness || roleType == constants.RoleRefinery
}

func namedAgentName(id, roleType string) string {
	roleMarker := "-" + roleType + "-"
	idx := strings.Index(id, roleMarker)
	if idx < 0 {
		return ""
	}
	return id[idx+len(roleMarker):]
}

// parseRigAgentAddressFromID extracts a mail address from a rig-prefixed bead ID
// when the description metadata is missing. Scans for known role markers in the ID
// to determine the rig name and agent name.
//
// ID format: <prefix>-<rig>-<role>[-<name>]
//
// Singleton roles (witness, refinery) must NOT have a name segment — IDs like
// "bd-beads-witness-extra" are malformed and return "".
//
// Keep role lists in sync with beads.RigLevelRoles and beads.NamedRoles.
func parseRigAgentAddressFromID(id string) string {
	// Singleton roles: no name segment allowed
	singletonRoles := []string{constants.RoleWitness, constants.RoleRefinery}
	// Named roles: require a name segment
	namedRoles := []string{constants.RoleCrew, constants.RolePolecat}

	for _, role := range namedRoles {
		if address := namedRigAgentAddress(id, role); address != "" {
			return address
		}
	}

	for _, role := range singletonRoles {
		if address := singletonRigAgentAddress(id, role); address != "" {
			return address
		}
	}

	return ""
}

func namedRigAgentAddress(id, role string) string {
	marker := "-" + role + "-"
	idx := strings.Index(id, marker)
	if idx < 0 {
		return ""
	}
	// Everything between prefix- and -role- is the rig name.
	// The prefix ends at the first hyphen: <prefix>-<rig>-...
	// But prefix could be multi-char (bd, gt, ppf), so we find
	// the rig as the substring between the first hyphen and the role marker.
	firstHyphen := strings.Index(id, "-")
	if firstHyphen < 0 || firstHyphen >= idx {
		return ""
	}
	rig := id[firstHyphen+1 : idx]
	if rig == "" {
		return ""
	}
	name := id[idx+len(marker):]
	if name == "" {
		return ""
	}
	// Named role (crew, polecat): address is rig/name
	return rig + "/" + name
}

func singletonRigAgentAddress(id, role string) string {
	// Singleton roles match only at end of ID: <prefix>-<rig>-<role>
	// Reject if a name segment follows (e.g. -witness-extra is malformed).
	marker := "-" + role + "-"
	if strings.Contains(id, marker) {
		return ""
	}

	suffix := "-" + role
	if !strings.HasSuffix(id, suffix) {
		return ""
	}
	// Find rig between first hyphen and the suffix
	firstHyphen := strings.Index(id, "-")
	if firstHyphen < 0 {
		return ""
	}
	suffixStart := len(id) - len(suffix)
	if firstHyphen >= suffixStart {
		return ""
	}
	rig := id[firstHyphen+1 : suffixStart]
	if rig == "" {
		return ""
	}
	return rig + "/" + role
}

// parseAgentAddressFromDescription extracts agent address from description metadata.
// Looks for "location: X" first (explicit address), then falls back to
// "role_type: X" and "rig: Y" patterns in the description.
func parseAgentAddressFromDescription(desc string) string {
	metadata := parseAgentAddressMetadata(desc)

	// Explicit location takes priority (used by dogs and other agents
	// whose address can't be derived from role_type + rig alone)
	if metadata.location != "" && metadata.location != "null" {
		return metadata.location
	}

	// Handle null values from description
	rig := metadata.rig
	if rig == "null" || rig == "" {
		rig = ""
	}
	roleType := metadata.roleType
	if roleType == "null" || roleType == "" {
		return ""
	}

	// Town-level agents (no rig)
	if rig == "" {
		return roleType + "/"
	}

	// Rig-level agents: rig/name (role_type is the agent name for crew/polecat)
	return rig + "/" + roleType
}

// ResolveGroupAddress resolves a @group address to individual recipient addresses.
// Returns the list of resolved addresses and any error.
// This is the public entry point for group resolution.
func (r *Router) ResolveGroupAddress(address string) ([]string, error) {
	group := parseGroupAddress(address)
	if group == nil {
		return nil, fmt.Errorf("invalid group address: %s", address)
	}
	return resolveGroup(r, group)
}

// resolveGroup resolves a @group address to individual recipient addresses.
// Returns the list of resolved addresses and any error.
func resolveGroup(r *Router, group *ParsedGroup) ([]string, error) {
	if group == nil {
		return nil, errors.New("nil group")
	}

	switch group.Type {
	case GroupTypeOverseer:
		return resolveOverseer(r)
	case GroupTypeTown:
		return resolveTownAgents(r)
	case GroupTypeRole:
		return resolveAgentsByRole(r, group.RoleType, "")
	case GroupTypeRig:
		return resolveAgentsByRig(r, group.Rig)
	case GroupTypeRigRole:
		return resolveAgentsByRole(r, group.RoleType, group.Rig)
	default:
		return nil, fmt.Errorf("unknown group type: %s", group.Type)
	}
}

// resolveOverseer resolves @overseer to the human operator's address.
// Loads the overseer config and returns "overseer" as the address.
func resolveOverseer(r *Router) ([]string, error) {
	if r.townRoot == "" {
		return nil, errors.New("town root not set, cannot resolve @overseer")
	}

	// Load overseer config to verify it exists
	configPath := config.OverseerConfigPath(r.townRoot)
	_, err := config.LoadOverseerConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolving @overseer: %w", err)
	}

	// Return the overseer address
	return []string{"overseer"}, nil
}

// resolveTownAgents resolves @town to all town-level agents (mayor, deacon).
func resolveTownAgents(r *Router) ([]string, error) {
	// Town-level agents have rig=null in their description
	agents := queryAgents(r, "rig: null")

	var addresses []string
	for _, agent := range agents {
		if addr := agentBeadToAddress(agent); addr != "" {
			addresses = append(addresses, addr)
		}
	}

	return addresses, nil
}

// resolveAgentsByRole resolves agents by their role_type.
// If rig is non-empty, also filters by rig.
func resolveAgentsByRole(r *Router, roleType, rig string) ([]string, error) {
	// Build query filter
	query := "role_type: " + roleType
	agents := queryAgents(r, query)

	var addresses []string
	for _, agent := range agents {
		// Filter by rig if specified
		if rig != "" {
			// Check if agent's description contains matching rig
			if !strings.Contains(agent.Description, "rig: "+rig) {
				continue
			}
		}
		if addr := agentBeadToAddress(agent); addr != "" {
			addresses = append(addresses, addr)
		}
	}

	return addresses, nil
}

// resolveAgentsByRig resolves @rig/<rigname> to all agents in that rig.
func resolveAgentsByRig(r *Router, rig string) ([]string, error) {
	// Query for agents with matching rig in description
	query := "rig: " + rig
	agents := queryAgents(r, query)

	var addresses []string
	for _, agent := range agents {
		if addr := agentBeadToAddress(agent); addr != "" {
			addresses = append(addresses, addr)
		}
	}

	return addresses, nil
}

// queryAgents queries agent beads using bd list with description filtering.
// Searches both town-level and rig-level beads to find all agents.
func queryAgents(r *Router, descContains string) []*agentBead {
	var allAgents []*agentBead

	// Query town-level beads
	townBeadsDir := resolveBeadsDir(r)
	townAgents, err := queryAgentsInDir(townBeadsDir, descContains)
	if err != nil {
		// Don't fail yet - rig beads might still have results
		townAgents = nil
	}
	allAgents = append(allAgents, townAgents...)

	// Also query rig-level beads via routes.jsonl
	if r.townRoot != "" {
		routesDir := filepath.Join(r.townRoot, ".beads")
		routes, routeErr := beads.LoadRoutes(routesDir)
		if routeErr == nil {
			for _, route := range routes {
				// Skip hq- routes (town-level, already queried)
				if strings.HasPrefix(route.Prefix, "hq-") {
					continue
				}
				rigBeadsDir := filepath.Join(r.townRoot, route.Path, ".beads")
				rigAgents, rigErr := queryAgentsInDir(rigBeadsDir, descContains)
				if rigErr != nil {
					continue // Skip rigs with errors
				}
				allAgents = append(allAgents, rigAgents...)
			}
		}
	}

	// Deduplicate by ID
	seen := make(map[string]bool)
	var unique []*agentBead
	for _, agent := range allAgents {
		if !seen[agent.ID] {
			seen[agent.ID] = true
			unique = append(unique, agent)
		}
	}

	return unique
}

// queryAgentsInDir queries agent beads in a specific beads directory with optional description filtering.
// Queries both the issues and wisps tables, merging results.
func queryAgentsInDir(beadsDir, descContains string) ([]*agentBead, error) {
	args := agentListArgs(descContains)

	ctx, cancel := bdReadCtx()
	defer cancel()

	// Query issues table (backward compat during migration)
	stdout, issuesErr := runBdCommand(ctx, args, filepath.Dir(beadsDir), beadsDir)

	// Also query wisps table for migrated agent beads (best-effort)
	wispCtx, wispCancel := bdReadCtx()
	defer wispCancel()
	wispOut, _ := runBdCommand(wispCtx, []string{"mol", "wisp", "list", "--json"}, filepath.Dir(beadsDir), beadsDir)

	// Merge results: collect agent beads from both sources. Wisps are the primary
	// source after migration; issue rows fill in agents not yet migrated.
	agents, seenIDs := parseWispAgents(wispOut)

	// Then issues (backward compat, skip duplicates)
	if len(stdout) > 0 {
		agents = appendIssueAgents(agents, seenIDs, stdout)
	} else if issuesErr != nil && len(agents) == 0 {
		return nil, fmt.Errorf("querying agents in %s: %w", beadsDir, issuesErr)
	}

	// Filter for active agents (closed/deleted agents are inactive)
	return activeAgentBeads(agents), nil
}

func agentListArgs(descContains string) []string {
	args := []string{"list", "--label=gt:agent", "--json", "--flat", "--limit=0"}
	if descContains != "" {
		args = append(args, "--desc-contains="+descContains)
	}
	return args
}

func parseWispAgents(output []byte) ([]*agentBead, map[string]bool) {
	seenIDs := make(map[string]bool)
	if len(output) == 0 {
		return nil, seenIDs
	}

	var wispAgents []*agentBead
	if json.Unmarshal(output, &wispAgents) != nil {
		return nil, seenIDs
	}
	var agents []*agentBead
	for _, agent := range wispAgents {
		if isAgentBeadEntry(agent) {
			seenIDs[agent.ID] = true
			agents = append(agents, agent)
		}
	}
	return agents, seenIDs
}

func appendIssueAgents(agents []*agentBead, seenIDs map[string]bool, output []byte) []*agentBead {
	var issueAgents []*agentBead
	if json.Unmarshal(output, &issueAgents) != nil {
		return agents
	}
	for _, agent := range issueAgents {
		if !seenIDs[agent.ID] {
			agents = append(agents, agent)
		}
	}
	return agents
}

func activeAgentBeads(agents []*agentBead) []*agentBead {
	var active []*agentBead
	for _, agent := range agents {
		if isActiveAgentStatus(agent.Status) {
			active = append(active, agent)
		}
	}
	return active
}

func isActiveAgentStatus(status string) bool {
	switch status {
	case "open", "in_progress", "hooked", "pinned":
		return true
	default:
		return false
	}
}

// isAgentBeadEntry checks if an agentBead entry is an actual agent bead.
func isAgentBeadEntry(a *agentBead) bool {
	if a.Type == "agent" {
		return true
	}
	for _, l := range a.Labels {
		if l == "gt:agent" {
			return true
		}
	}
	return false
}

// queryAgentsFromDir queries agent beads from a specific beads directory.
func queryAgentsFromDir(beadsDir string) ([]*agentBead, error) {
	return queryAgentsInDir(beadsDir, "")
}

// shouldBeWisp determines if a message should be stored as a wisp.
// Returns true if:
// - Message.Wisp is explicitly set
// - Subject matches lifecycle message patterns (POLECAT_*, NUDGE, etc.)
func shouldBeWisp(msg *Message) bool {
	if msg.Wisp {
		return true
	}
	// Auto-detect protocol/lifecycle messages by subject prefix
	subjectLower := strings.ToLower(msg.Subject)
	wispPrefixes := []string{
		"polecat_started",
		"polecat_done",
		"work_done",
		"start_work",
		"nudge",
		"lifecycle:",
		"merged",
		"merge_ready",
		"merge_failed",
	}
	for _, prefix := range wispPrefixes {
		if strings.HasPrefix(subjectLower, prefix) {
			return true
		}
	}
	return false
}

// Send delivers a message via beads message.
// Routes the message to the correct beads database based on recipient address.
// Supports fan-out for:
// - Mailing lists (list:name) - fans out to all list members
// - @group addresses - resolves and fans out to matching agents
// Supports single-copy delivery for:
// - Queues (queue:name) - stores single message for worker claiming
// - Announces (announce:name) - bulletin board, no claiming, retention-limited
func (r *Router) Send(msg *Message) error {
	// Check for mailing list address
	if isListAddress(msg.To) {
		return sendToList(r, msg)
	}

	// Check for queue address - single message for claiming
	if isQueueAddress(msg.To) {
		return sendToQueue(r, msg)
	}

	// Check for announce address - bulletin board (single copy, no claiming)
	if isAnnounceAddress(msg.To) {
		return sendToAnnounce(r, msg)
	}

	// Check for beads-native channel address - broadcast with retention
	if isChannelAddress(msg.To) {
		return sendToChannel(r, msg)
	}

	// Check for @group address - resolve and fan-out
	if isGroupAddress(msg.To) {
		return sendToGroup(r, msg)
	}

	// Single recipient - send directly
	return sendToSingle(r, msg)
}

// sendToGroup resolves a @group address and sends individual messages to each member.
func sendToGroup(r *Router, msg *Message) error {
	group := parseGroupAddress(msg.To)
	if group == nil {
		return fmt.Errorf("invalid group address: %s", msg.To)
	}

	recipients, err := resolveGroup(r, group)
	if err != nil {
		return fmt.Errorf("resolving group %s: %w", msg.To, err)
	}

	if len(recipients) == 0 {
		return fmt.Errorf("no recipients found for group: %s", msg.To)
	}

	// Fan-out: send a copy to each recipient
	var errs []string
	for _, recipient := range recipients {
		// Create a copy of the message for this recipient
		msgCopy := *msg
		msgCopy.To = recipient
		msgCopy.ID = "" // Each fan-out copy gets its own ID from bd create

		if err := sendToSingle(r, &msgCopy); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", recipient, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("some group sends failed: %s", strings.Join(errs, "; "))
	}

	return nil
}

// validateRecipient checks that the recipient identity corresponds to an existing agent.
// Returns an error if the recipient is invalid or doesn't exist.
// Queries agents from town-level beads AND all rig-level beads via routes.jsonl.
func validateRecipient(r *Router, identity string) error {
	if hasUnsafeAddressSegment(identity) {
		return fmt.Errorf("no agent found")
	}

	if handled, err := validateKnownRecipient(r.townRoot, identity); handled {
		return err
	}

	// Query agents from town-level beads
	if queryContainsRecipient(r, identity) {
		return nil
	}

	return validateRecipientFromSources(r, identity)
}

func validateKnownRecipient(townRoot, identity string) (bool, error) {
	if handled, err := validateTownRecipient(identity); handled {
		return true, err
	}
	return validateRigSingleton(townRoot, identity)
}

func validateTownRecipient(identity string) (bool, error) {
	// Overseer is the human operator, not an agent bead.
	if identity == "overseer" {
		return true, nil
	}
	// Well-known town-level singletons always valid.
	switch identity {
	case "mayor", "mayor/", "deacon", "deacon/":
		return true, nil
	}
	if _, ok := DogAddressName(identity); !ok && isReservedTownSubpath(identity) {
		return true, fmt.Errorf("no agent found")
	}
	return false, nil
}

func validateRigSingleton(townRoot, identity string) (bool, error) {
	// Well-known rig-level singletons are valid without an active session, but
	// the containing rig must exist so typos do not queue mail to a dead inbox.
	parts := strings.SplitN(identity, "/", 3)
	if len(parts) == 2 && (parts[1] == "witness" || parts[1] == "refinery") {
		if townRoot == "" || dirExists(filepath.Join(townRoot, parts[0])) {
			return true, nil
		}
		return true, fmt.Errorf("no agent found")
	}
	return false, nil
}

func validateRecipientFromSources(r *Router, identity string) error {
	// Query agents from rig-level beads via routes.jsonl.
	found, routeQueryErr := queryRoutesForRecipient(r, identity)
	if found {
		return nil
	}

	// Fall back to workspace directory validation. Agent beads may be missing
	// (e.g., Dolt DB reset) even though the agent's workspace directory exists.
	if r.townRoot != "" && validateAgentWorkspace(r, identity) {
		return nil
	}
	if routeQueryErr != nil {
		return routeQueryErr
	}
	return fmt.Errorf("no agent found")
}

func queryContainsRecipient(r *Router, identity string) bool {
	for _, agent := range queryAgents(r, "") {
		if agentBeadToAddress(agent) == identity {
			return true
		}
	}
	return false
}

func queryRoutesForRecipient(r *Router, identity string) (bool, error) {
	if r.townRoot == "" {
		return false, nil
	}

	routes, err := beads.LoadRoutes(filepath.Join(r.townRoot, ".beads"))
	if err != nil {
		return false, nil
	}
	var queryErrors []string
	for _, route := range routes {
		if strings.HasPrefix(route.Prefix, "hq-") {
			continue
		}
		found, err := queryRouteForRecipient(r, identity, route)
		if found {
			return true, nil
		}
		if err != nil {
			queryErrors = append(queryErrors, fmt.Sprintf("%s: %v", route.Path, err))
		}
	}
	if len(queryErrors) > 0 {
		return false, fmt.Errorf("no agent found (query errors: %s)", strings.Join(queryErrors, "; "))
	}
	return false, nil
}

func queryRouteForRecipient(r *Router, identity string, route beads.Route) (bool, error) {
	rigBeadsDir := filepath.Join(r.townRoot, route.Path, ".beads")
	agents, err := queryAgentsFromDir(rigBeadsDir)
	if err != nil {
		return false, err
	}
	for _, agent := range agents {
		if agentBeadToAddress(agent) == identity {
			return true, nil
		}
	}
	return false, nil
}

// validateAgentWorkspace checks if an agent's workspace directory exists on disk.
// Used as a fallback when the agent isn't found in the bead registry.
func validateAgentWorkspace(r *Router, identity string) bool {
	if _, ok := DogAddressName(identity); !ok && isReservedTownSubpath(identity) {
		return false
	}

	parts := strings.Split(identity, "/")
	switch len(parts) {
	case 1:
		return townWorkspaceExists(r.townRoot, parts[0])
	case 2:
		return rigWorkspaceExists(r.townRoot, parts[0], parts[1])
	case 3:
		return explicitWorkspaceExists(r.townRoot, identity, parts)
	}

	return false
}

func townWorkspaceExists(townRoot, name string) bool {
	// Town-level singleton: "mayor", "deacon".
	return dirExists(filepath.Join(townRoot, strings.TrimSuffix(name, "/")))
}

func rigWorkspaceExists(townRoot, rig, name string) bool {
	// Singleton role: gastown/witness, gastown/refinery.
	if dirExists(filepath.Join(townRoot, rig, name)) {
		return true
	}
	// Named role (identity normalized away crew/polecats): check both.
	for _, role := range []string{"crew", "polecats"} {
		if dirExists(filepath.Join(townRoot, rig, role, name)) {
			return true
		}
	}
	return false
}

func explicitWorkspaceExists(townRoot, identity string, parts []string) bool {
	// Explicit role paths: rig/crew/<name> or rig/polecats/<name>.
	if parts[1] == "crew" || parts[1] == "polecats" {
		return dirExists(filepath.Join(townRoot, parts[0], parts[1], parts[2]))
	}
	// Dog addresses: deacon/dogs/<name>.
	_, isDog := DogAddressName(identity)
	return isDog && dirExists(filepath.Join(townRoot, parts[0], parts[1], parts[2]))
}

// dirExists returns true if the path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// resolveCrewShorthand expands "crew/name" or "polecats/name" shorthand addresses
// to fully-qualified "rig/name" form by scanning the town filesystem.
//
// When gt agents displays crew workers, it shows them as "crew/bob" (without rig).
// This function enables "gt mail send crew/bob" to work by finding the rig.
//
// Returns the normalized identity if exactly one rig contains the crew member,
// or the original identity unchanged if zero or multiple rigs match (to let
// validation fail with an informative error).
func resolveCrewShorthand(r *Router, identity string) string {
	if r.townRoot == "" {
		return identity
	}

	roleDir, name, ok := crewShorthandParts(identity)
	if !ok || realRigDirectory(r.townRoot, roleDir) {
		return identity
	}
	if match, ok := findCrewShorthand(r.townRoot, roleDir, name); ok {
		return match // Unambiguous: expand to rig/name
	}
	return identity // Ambiguous or not found: let validation handle it
}

func crewShorthandParts(identity string) (roleDir, name string, ok bool) {
	parts := strings.Split(identity, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	roleDir, name = parts[0], parts[1]
	if roleDir != constants.RoleCrew && roleDir != "polecats" {
		return "", "", false
	}
	return roleDir, name, true
}

func realRigDirectory(townRoot, roleDir string) bool {
	fi, err := os.Stat(filepath.Join(townRoot, roleDir))
	return err == nil && fi.IsDir()
}

func findCrewShorthand(townRoot, roleDir, name string) (string, bool) {
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return "", false
	}
	var match string
	matches := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agentDir := filepath.Join(townRoot, entry.Name(), roleDir, name)
		if fi, err := os.Stat(agentDir); err == nil && fi.IsDir() {
			match = entry.Name() + "/" + name
			matches++
		}
	}
	return match, matches == 1
}

// sendToSingle sends a message to a single recipient.
func sendToSingle(r *Router, msg *Message) error {
	// Ensure message has an ID for in-memory tracking (notifications, logging).
	// We no longer pass --id to bd create; bd auto-generates the correct prefix.
	if msg.ID == "" {
		msg.ID = GenerateID()
	}

	// Validate message before sending
	if err := msg.Validate(); err != nil {
		return fmt.Errorf("invalid message: %w", err)
	}

	// Convert addresses to beads identities
	toIdentity := AddressToIdentity(msg.To)
	// Expand crew/polecats shorthand (e.g., "crew/bob" → "pata/bob")
	toIdentity = resolveCrewShorthand(r, toIdentity)

	// Validate recipient exists
	if err := validateRecipient(r, toIdentity); err != nil {
		return fmt.Errorf("invalid recipient %q: %w", msg.To, err)
	}

	// Build labels for type, from/thread/reply-to/cc
	labels := buildLabels(msg)
	args := singleMessageArgs(msg, toIdentity, labels, shouldBeWisp(msg))
	if err := sendSingleMessage(r, msg, args); err != nil {
		return err
	}

	// Notify recipient if they have an active session (best-effort notification).
	// Skip when the caller explicitly suppressed notification (--no-notify)
	// or for self-mail (handoffs to future-self don't need present-self notified).
	// Notification is async: the durable write is complete, so the caller
	// doesn't block on idle probing (up to 1s per recipient in fan-out).
	// Callers that exit soon after Send should call WaitPendingNotifications.
	if !msg.SuppressNotify && !isSelfMail(msg.From, msg.To) {
		msgCopy := *msg // copy to avoid data race if caller mutates msg
		r.notifyWg.Add(1)
		go func() {
			defer r.notifyWg.Done()
			notifyRecipient(r, &msgCopy) //nolint:errcheck
		}()
	}

	return nil
}

func singleMessageArgs(msg *Message, toIdentity string, labels []string, ephemeral bool) []string {
	// Build command: bd create --assignee=<recipient> -d <body> --labels=gt:message,... -- <subject>
	// Flags go first, then -- to end flag parsing, then the positional subject.
	// This prevents subjects like "--help" from being parsed as flags (see web/api.go).
	// Let bd auto-generate the ID with the correct database prefix.
	args := []string{"create",
		"--assignee", toIdentity,
		"-d", msg.Body,
		"--priority", fmt.Sprintf("%d", PriorityToBeads(msg.Priority)),
	}
	if len(labels) > 0 {
		args = append(args, "--labels", strings.Join(labels, ","))
	}
	args = append(args, "--actor", msg.From)
	// Do NOT pass --id to bd create. The msg.ID (msg-xxx prefix) is for
	// in-memory tracking only. bd auto-generates IDs with the correct
	// database prefix (e.g., hq-wisp-xxx).
	if ephemeral {
		args = append(args, "--ephemeral")
	}
	return append(args, "--", msg.Subject)
}

func sendSingleMessage(r *Router, msg *Message, args []string) error {
	beadsDir := resolveBeadsDir(r)
	if err := ensureCustomTypes(beadsDir); err != nil {
		return err
	}
	ctx, cancel := bdWriteCtx()
	defer cancel()
	_, err := runBdCommand(ctx, args, filepath.Dir(beadsDir), beadsDir)
	telemetry.RecordMailMessage(context.Background(), "send", telemetry.MailMessageInfo{
		ID:       msg.ID,
		From:     msg.From,
		To:       msg.To,
		Subject:  msg.Subject,
		Body:     msg.Body,
		ThreadID: msg.ThreadID,
		Priority: string(msg.Priority),
		MsgType:  string(msg.Type),
	}, err)
	if err != nil {
		return fmt.Errorf("sending message: %w", err)
	}
	return nil
}

// sendToList expands a mailing list and sends individual copies to each recipient.
// Each recipient gets their own message copy with the same content.
// Collects all delivery errors and reports partial failures.
func sendToList(r *Router, msg *Message) error {
	listName := parseListName(msg.To)
	recipients, err := expandList(r, listName)
	if err != nil {
		return err
	}

	// Fan-out: send a copy to each recipient, collecting all errors
	var errs []string
	for _, recipient := range recipients {
		// Create a copy of the message for this recipient
		msgCopy := *msg
		msgCopy.To = recipient
		msgCopy.ID = "" // Each fan-out copy gets its own ID from bd create

		if err := r.Send(&msgCopy); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", recipient, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("sending to list %s: some deliveries failed: %s", listName, strings.Join(errs, "; "))
	}

	return nil
}

// ExpandListAddress expands a list:name address to its recipients.
// Returns ErrUnknownList if the list is not found.
// This is exported for use by commands that want to show fan-out details.
func (r *Router) ExpandListAddress(address string) ([]string, error) {
	if !isListAddress(address) {
		return nil, fmt.Errorf("not a list address: %s", address)
	}
	return expandList(r, parseListName(address))
}

// sendToQueue delivers a message to a queue for worker claiming.
// Unlike sendToList, this creates a SINGLE message (no fan-out).
// The message is stored in town-level beads with queue metadata.
// Workers claim messages using bd update --claimed-by.
func sendToQueue(r *Router, msg *Message) error {
	queueName := parseQueueName(msg.To)

	// Validate queue exists in messaging config
	_, err := expandQueue(r, queueName)
	if err != nil {
		return err
	}

	// Build labels for type, from/thread/reply-to/cc plus queue metadata
	var labels []string
	labels = append(labels, "gt:message")
	labels = append(labels, "from:"+msg.From)
	labels = append(labels, "queue:"+queueName)
	labels = append(labels, DeliverySendLabels()...)
	if msg.ThreadID != "" {
		labels = append(labels, "thread:"+msg.ThreadID)
	}
	if msg.ReplyTo != "" {
		labels = append(labels, "reply-to:"+msg.ReplyTo)
	}
	for _, cc := range msg.CC {
		ccIdentity := AddressToIdentity(cc)
		labels = append(labels, "cc:"+ccIdentity)
	}

	// Build command: bd create --assignee=queue:<name> -d <body> ... -- <subject>
	// Flags go first, then -- to end flag parsing, then the positional subject.
	// This prevents subjects like "--help" from being parsed as flags.
	// Use queue:<name> as assignee so inbox queries can filter by queue
	args := []string{"create",
		"--assignee", msg.To, // queue:name
		"-d", msg.Body,
	}

	// Add priority flag
	beadsPriority := PriorityToBeads(msg.Priority)
	args = append(args, "--priority", fmt.Sprintf("%d", beadsPriority))

	// Add labels (includes queue name for filtering)
	if len(labels) > 0 {
		args = append(args, "--labels", strings.Join(labels, ","))
	}

	// Add actor for attribution (sender identity)
	args = append(args, "--actor", msg.From)

	// Queue messages are never ephemeral - they need to persist until claimed
	// (deliberately not checking shouldBeWisp)

	// End flag parsing, then subject as positional argument
	args = append(args, "--", msg.Subject)

	// Queue messages go to town-level beads (shared location)
	beadsDir := resolveBeadsDir(r)
	if err := ensureCustomTypes(beadsDir); err != nil {
		return err
	}
	ctx, cancel := bdWriteCtx()
	defer cancel()
	_, err = runBdCommand(ctx, args, filepath.Dir(beadsDir), beadsDir)
	if err != nil {
		return fmt.Errorf("sending to queue %s: %w", queueName, err)
	}

	// No notification for queue messages - workers poll or check on their own schedule

	return nil
}

// sendToAnnounce delivers a message to an announce channel (bulletin board).
// Unlike sendToQueue, no claiming is supported - messages persist until retention limit.
// ONE copy is stored in town-level beads with announce_channel metadata.
func sendToAnnounce(r *Router, msg *Message) error {
	announceName := parseAnnounceName(msg.To)

	// Validate announce channel exists and get config
	announceCfg, err := expandAnnounce(r, announceName)
	if err != nil {
		return err
	}

	// Apply retention pruning BEFORE creating new message
	if announceCfg.RetainCount > 0 {
		if err := pruneAnnounce(r, announceName, announceCfg.RetainCount); err != nil {
			// Log but don't fail - pruning is best-effort
			// The new message should still be created
			_ = err
		}
	}

	labels := announceMessageLabels(msg, announceName)
	args := announceMessageArgs(msg, labels)
	return sendAnnounceMessage(r, announceName, args)
}

func announceMessageLabels(msg *Message, announceName string) []string {
	// delivery:pending is intentionally omitted for announce messages — broadcast
	// messages have no single recipient to ack against. Subscriber fan-out copies
	// go through sendToSingle which adds delivery tracking.
	labels := []string{"gt:message", "from:" + msg.From, "announce:" + announceName}
	if msg.ThreadID != "" {
		labels = append(labels, "thread:"+msg.ThreadID)
	}
	if msg.ReplyTo != "" {
		labels = append(labels, "reply-to:"+msg.ReplyTo)
	}
	for _, cc := range msg.CC {
		labels = append(labels, "cc:"+AddressToIdentity(cc))
	}
	return labels
}

func announceMessageArgs(msg *Message, labels []string) []string {
	// Flags go first, then -- to end flag parsing, then the positional subject.
	// This prevents subjects like "--help" from being parsed as flags.
	args := []string{
		"create",
		"--assignee", msg.To, // announce:name
		"-d", msg.Body,
		"--priority", fmt.Sprintf("%d", PriorityToBeads(msg.Priority)),
	}
	if len(labels) > 0 {
		args = append(args, "--labels", strings.Join(labels, ","))
	}
	args = append(args, "--actor", msg.From)
	// Announce messages are never ephemeral — they need to persist for readers.
	return append(args, "--", msg.Subject)
}

func sendAnnounceMessage(r *Router, announceName string, args []string) error {
	// Announce messages go to town-level beads (shared location).
	beadsDir := resolveBeadsDir(r)
	if err := ensureCustomTypes(beadsDir); err != nil {
		return err
	}
	ctx, cancel := bdWriteCtx()
	defer cancel()
	if _, err := runBdCommand(ctx, args, filepath.Dir(beadsDir), beadsDir); err != nil {
		return fmt.Errorf("sending to announce %s: %w", announceName, err)
	}
	// No notification for announce messages — readers poll or check on their own schedule.
	return nil
}

func channelMessageLabels(msg *Message, channelName string) []string {
	labels := []string{
		"gt:message",
		"from:" + msg.From,
		"channel:" + channelName,
	}
	if msg.ThreadID != "" {
		labels = append(labels, "thread:"+msg.ThreadID)
	}
	if msg.ReplyTo != "" {
		labels = append(labels, "reply-to:"+msg.ReplyTo)
	}
	for _, cc := range msg.CC {
		labels = append(labels, "cc:"+AddressToIdentity(cc))
	}
	return labels
}

func channelMessageArgs(msg *Message, labels []string) []string {
	args := []string{
		"create",
		"--assignee", msg.To, // channel:name
		"-d", msg.Body,
		"--priority", fmt.Sprintf("%d", PriorityToBeads(msg.Priority)),
	}
	if len(labels) > 0 {
		args = append(args, "--labels", strings.Join(labels, ","))
	}
	args = append(args, "--actor", msg.From)
	// Channel messages are never ephemeral — they persist according to the
	// channel's retention policy.
	return append(args, "--", msg.Subject)
}

func sendChannelMessage(r *Router, channelName string, args []string) error {
	beadsDir := resolveBeadsDir(r)
	if err := ensureCustomTypes(beadsDir); err != nil {
		return err
	}
	ctx, cancel := bdWriteCtx()
	defer cancel()
	if _, err := runBdCommand(ctx, args, filepath.Dir(beadsDir), beadsDir); err != nil {
		return fmt.Errorf("sending to channel %s: %w", channelName, err)
	}
	return nil
}

func fanOutChannelMessage(r *Router, msg *Message, channelName string, subscribers []string) error {
	if len(subscribers) == 0 {
		return nil
	}

	var errs []string
	for _, subscriber := range subscribers {
		// Skip self-delivery (don't notify the sender).
		if isSelfMail(msg.From, subscriber) {
			continue
		}

		msgCopy := *msg
		msgCopy.To = subscriber
		msgCopy.ID = "" // Each fan-out copy gets its own ID from bd create.
		msgCopy.Subject = fmt.Sprintf("[channel:%s] %s", channelName, msg.Subject)

		if err := sendToSingle(r, &msgCopy); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", subscriber, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("channel %s: some subscriber deliveries failed: %s", channelName, strings.Join(errs, "; "))
	}
	return nil
}

// sendToChannel delivers a message to a beads-native channel.
// Creates a message with channel:<name> label for channel queries.
// Also fans out delivery to each subscriber's inbox.
// Retention is enforced by the channel's EnforceChannelRetention after message creation.
func sendToChannel(r *Router, msg *Message) error {
	channelName := parseChannelName(msg.To)

	// Validate channel exists as a beads-native channel
	if r.townRoot == "" {
		return fmt.Errorf("town root not set, cannot send to channel: %s", channelName)
	}
	b := beads.New(r.townRoot)
	_, fields, err := b.GetChannelBead(channelName)
	if err != nil {
		return fmt.Errorf("getting channel %s: %w", channelName, err)
	}
	if fields == nil {
		return fmt.Errorf("channel not found: %s", channelName)
	}
	if fields.Status == beads.ChannelStatusClosed {
		return fmt.Errorf("channel %s is closed", channelName)
	}

	labels := channelMessageLabels(msg, channelName)
	args := channelMessageArgs(msg, labels)
	if err := sendChannelMessage(r, channelName, args); err != nil {
		return err
	}

	// Enforce channel retention policy (on-write cleanup)
	_ = b.EnforceChannelRetention(channelName)

	return fanOutChannelMessage(r, msg, channelName, fields.Subscribers)
}

// pruneAnnounce deletes oldest messages from an announce channel to enforce retention.
// If the channel has >= retainCount messages, deletes the oldest until count < retainCount.
func pruneAnnounce(r *Router, announceName string, retainCount int) error {
	if retainCount <= 0 {
		return nil // No retention limit
	}

	beadsDir := resolveBeadsDir(r)
	if err := ensureCustomTypes(beadsDir); err != nil {
		return err
	}

	// Query existing messages in this announce channel
	// Use bd list with labels filter to find messages with gt:message and announce:<name> labels
	args := []string{"list",
		"--labels=gt:message,announce:" + announceName,
		"--json",
		"--limit=0", // Get all
		"--sort=created",
		"--asc", // Oldest first
	}

	ctx, cancel := bdReadCtx()
	defer cancel()
	stdout, err := runBdCommand(ctx, args, filepath.Dir(beadsDir), beadsDir)
	if err != nil {
		return fmt.Errorf("querying announce messages: %w", err)
	}

	// Parse message list
	var messages []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(stdout, &messages); err != nil {
		return fmt.Errorf("parsing announce messages: %w", err)
	}

	// Calculate how many to delete (we're about to add 1 more)
	// If we have N messages and retainCount is R, we need to keep at most R-1 after pruning
	// so the new message makes it exactly R
	toDelete := len(messages) - (retainCount - 1)
	if toDelete <= 0 {
		return nil // No pruning needed
	}

	// Delete oldest messages
	messageCount := len(messages)
	for i := 0; i < toDelete && i < messageCount; i++ {
		deleteArgs := []string{"close", messages[i].ID, "--reason=retention pruning"}
		// Best-effort deletion - don't fail if one delete fails
		delCtx, delCancel := bdWriteCtx()
		_, _ = runBdCommand(delCtx, deleteArgs, filepath.Dir(beadsDir), beadsDir)
		delCancel()
	}

	return nil
}

// isSelfMail returns true if sender and recipient are the same identity.
// Uses AddressToIdentity for canonical normalization (handles crew/, polecats/ paths).
func isSelfMail(from, to string) bool {
	return AddressToIdentity(from) == AddressToIdentity(to)
}

// GetMailbox returns a Mailbox for the given address.
// Routes to the correct beads database based on the address.
func (r *Router) GetMailbox(address string) (*Mailbox, error) {
	beadsDir := resolveBeadsDir(r)
	workDir := filepath.Dir(beadsDir) // Parent of .beads
	return NewMailboxFromAddress(address, workDir), nil
}

// notifyRecipient notifies a recipient that mail is waiting.
//
// Notification strategy (GH#4607):
//  1. For agent sessions, enqueue a nudge for cooperative delivery at the
//     next turn boundary. Direct tmux NudgeSession submits a new turn and
//     cancels in-flight tool calls; the queued-nudge channel appends instead.
//  2. For the overseer (human operator), always use a visible banner.
//  3. If no town root is available, skip agent notification. The mail bead
//     is already durable. Do not submit a new turn.
//
// After a successful notification, a deferred reply-reminder nudge is also
// enqueued (after a configurable delay, default 30s) to prompt the recipient
// to reply via gt mail send rather than in chat.
//
// Supports mayor/, deacon/, rig/crew/name, rig/polecats/name, and rig/name addresses.
// Respects agent DND/muted state - skips notification if recipient has DND enabled.
func notifyRecipient(r *Router, msg *Message) error {
	sessionIDs := AddressToSessionIDs(msg.To)
	if len(sessionIDs) == 0 {
		return nil
	}
	notification := formatNotificationMessage(msg)
	priority := nudgePriorityForMailPriority(msg.Priority)
	result := notifyLiveSessions(r, msg, sessionIDs, notification, priority)
	if result.notified == 0 && r.townRoot != "" && (result.noServer || len(result.errs) == 0) {
		enqueueHeadlessMail(r, msg, sessionIDs, notification, priority, &result)
	}
	return finishMailNotify(result)
}

type mailNotifyResult struct {
	notified int
	noServer bool
	errs     []error
}

func notifyLiveSessions(r *Router, msg *Message, sessionIDs []string, notification, priority string) mailNotifyResult {
	var result mailNotifyResult
	for _, sessionID := range sessionIDs {
		if isSessionMuted(r, sessionID) {
			continue
		}
		outcome := notifyOneLiveSession(r, msg, sessionID, notification, priority)
		result.notified += outcome.notified
		if outcome.err != nil {
			result.errs = append(result.errs, outcome.err)
		}
		if outcome.noServer {
			result.noServer = true
			break
		}
	}
	return result
}

type sessionNotifyOutcome struct {
	notified int
	noServer bool
	err      error
}

func notifyOneLiveSession(r *Router, msg *Message, sessionID, notification, priority string) sessionNotifyOutcome {
	hasSession, err := r.tmux.HasSession(sessionID)
	if errors.Is(err, tmux.ErrNoServer) {
		return sessionNotifyOutcome{noServer: true}
	}
	if err != nil {
		return sessionNotifyOutcome{err: fmt.Errorf("notify session %s: %w", sessionID, err)}
	}
	if !hasSession {
		return sessionNotifyOutcome{}
	}
	if msg.To == "overseer" {
		return notifyOverseerBanner(r, msg, sessionID)
	}
	return notifyAgentSession(r, msg, sessionID, notification, priority)
}

func notifyOverseerBanner(r *Router, msg *Message, sessionID string) sessionNotifyOutcome {
	if err := r.tmux.SendNotificationBanner(sessionID, msg.From, msg.Subject); err != nil {
		return sessionNotifyOutcome{err: fmt.Errorf("notify session %s: %w", sessionID, err)}
	}
	return sessionNotifyOutcome{notified: 1}
}

func notifyAgentSession(r *Router, msg *Message, sessionID, notification, priority string) sessionNotifyOutcome {
	if r.townRoot == "" {
		// Fail safely: the bead is durable. Never submit a new turn (GH#4607).
		return sessionNotifyOutcome{}
	}
	if err := enqueueMailNotification(r, msg, sessionID, notification, priority); err != nil {
		return sessionNotifyOutcome{err: err}
	}
	return sessionNotifyOutcome{notified: 1}
}

func enqueueHeadlessMail(r *Router, msg *Message, sessionIDs []string, notification, priority string, result *mailNotifyResult) {
	for _, sessionID := range sessionIDs {
		if isSessionMuted(r, sessionID) {
			continue
		}
		if err := enqueueQueuedMail(r.townRoot, sessionID, msg, notification, priority); err != nil {
			result.errs = append(result.errs, err)
			continue
		}
		result.notified++
	}
}

func finishMailNotify(result mailNotifyResult) error {
	if len(result.errs) == 0 {
		return nil
	}
	joined := errors.Join(result.errs...)
	if result.notified > 0 {
		fmt.Fprintf(os.Stderr, "Warning: mail notification partially failed: %v\n", joined)
		return nil
	}
	return fmt.Errorf("mail notification failed: %w", joined)
}

func isSessionMuted(r *Router, sessionID string) bool {
	if r.townRoot == "" || sessionID == "" || sessionID == session.OverseerSessionName() {
		return false
	}
	bd := beads.New(r.townRoot)
	level, err := bd.GetAgentNotificationLevel(sessionID)
	if err != nil {
		return false
	}
	return level == beads.NotifyMuted
}

func nudgeKindForMessage(msg *Message) string {
	if msg.Type == TypeEscalation {
		return "escalation"
	}
	return "mail"
}

func nudgePriorityForMailPriority(priority Priority) string {
	switch priority {
	case PriorityUrgent, PriorityHigh:
		return nudge.PriorityUrgent
	default:
		return nudge.PriorityNormal
	}
}

func formatNotificationMessage(msg *Message) string {
	if msg.Type == TypeEscalation {
		return fmt.Sprintf("🚨 Escalation mail from %s. ID: %s. Severity: %s. Subject: %s. Run 'gt mail read %s' or 'gt escalate ack %s'.", msg.From, msg.ThreadID, prioritySeverityLabel(msg.Priority), msg.Subject, msg.ThreadID, msg.ThreadID)
	}
	return fmt.Sprintf("📬 You have new mail from %s. Subject: %s. Run 'gt mail inbox' to read.", msg.From, msg.Subject)
}

func prioritySeverityLabel(priority Priority) string {
	switch priority {
	case PriorityUrgent:
		return "critical"
	case PriorityHigh:
		return "high"
	case PriorityLow:
		return "low"
	default:
		return "medium"
	}
}

// enqueueMailNotification writes the mail wake-up onto the turn-appending
// nudge queue and schedules the deferred reply reminder.
func enqueueMailNotification(r *Router, msg *Message, sessionID, notification, priority string) error {
	if err := enqueueQueuedMail(r.townRoot, sessionID, msg, notification, priority); err != nil {
		return err
	}
	enqueueReplyReminder(r, msg, sessionID)
	return nil
}

func enqueueQueuedMail(townRoot, sessionID string, msg *Message, notification, priority string) error {
	if err := nudge.Enqueue(townRoot, sessionID, nudge.QueuedNudge{
		Sender:   msg.From,
		Message:  notification,
		Priority: priority,
		Kind:     nudgeKindForMessage(msg),
		ThreadID: msg.ThreadID,
		Severity: prioritySeverityLabel(msg.Priority),
	}); err != nil {
		return fmt.Errorf("enqueue mail notification for session %s: %w", sessionID, err)
	}
	return nil
}

// enqueueReplyReminder queues a deferred nudge reminding the recipient to reply
// via gt mail send rather than in chat. Best-effort: errors are logged, not returned.
//
// Skipped when:
//   - No town root (can't use nudge queue)
//   - Message type is TypeReply (recipient is already replying)
//   - Sender is not a direct mail address that can receive a reply
//   - Configured delay is zero or negative (feature disabled)
func enqueueReplyReminder(r *Router, msg *Message, sessionID string) {
	if r.townRoot == "" {
		return
	}
	if msg.Type == TypeReply {
		return // Already a reply — reminder would be redundant
	}
	if !senderCanReceiveReply(msg.From) {
		return
	}
	delay := config.LoadOperationalConfig(r.townRoot).GetMailConfig().ReplyReminderDelayD()
	if delay <= 0 {
		return // Disabled by config
	}
	reminder := nudge.QueuedNudge{
		Sender:       "system",
		Message:      fmt.Sprintf("Remember to reply to %s (subject: %q) via `gt mail send %s` — not in chat.", msg.From, msg.Subject, msg.From),
		Priority:     nudge.PriorityNormal,
		Kind:         "reply-reminder",
		ThreadID:     msg.ThreadID,
		DeliverAfter: time.Now().Add(delay),
	}
	if err := nudge.Enqueue(r.townRoot, sessionID, reminder); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to enqueue reply reminder for %s: %v\n", sessionID, err)
	}
}

func senderCanReceiveReply(from string) bool {
	if !isReplySenderInput(from) {
		return false
	}

	identity := AddressToIdentity(from)
	if isDirectReplyIdentity(identity) {
		return true
	}
	if !isRoutableReplyIdentity(identity) {
		return false
	}

	return replyAddressParts(strings.Split(identity, "/"))
}

func isReplySenderInput(from string) bool {
	return from != "" && strings.TrimSpace(from) == from && !strings.ContainsAny(from, " \t\r\n")
}

func isDirectReplyIdentity(identity string) bool {
	switch identity {
	case "overseer", "mayor/", "deacon/":
		return true
	default:
		return false
	}
}

func isRoutableReplyIdentity(identity string) bool {
	return identity != "" && !strings.HasPrefix(identity, "@") && !strings.ContainsAny(identity, ":@")
}

func replyAddressParts(parts []string) bool {
	switch len(parts) {
	case 2:
		return twoPartReplyAddress(parts)
	case 3:
		return deaconDogReplyAddress(parts)
	default:
		return false
	}
}

func twoPartReplyAddress(parts []string) bool {
	if !validReplyAddressPart(parts[0]) || !validReplyAddressPart(parts[1]) {
		return false
	}
	if parts[0] == constants.RoleMayor || parts[0] == constants.RoleDeacon {
		return false
	}
	return !isNonReplyAddressRole(parts[1])
}

func isNonReplyAddressRole(role string) bool {
	switch role {
	case constants.RoleCrew, "polecat", "polecats", "dogs":
		return true
	default:
		return false
	}
}

func deaconDogReplyAddress(parts []string) bool {
	return parts[0] == constants.RoleDeacon && parts[1] == "dogs" && validReplyAddressPart(parts[2])
}

func validReplyAddressPart(part string) bool {
	return part != "" && strings.TrimSpace(part) == part && !strings.ContainsAny(part, " \t\r\n:@")
}

// ClearReplyReminders removes any queued reply-reminder nudges for the given
// recipient identity and thread. This is best-effort cleanup after a successful
// reply send so satisfied threads do not keep re-nudging.
func (r *Router) ClearReplyReminders(address, threadID string) error {
	if r.townRoot == "" || threadID == "" {
		return nil
	}

	var firstErr error
	for _, sessionID := range AddressToSessionIDs(address) {
		if _, err := nudge.RemoveKindByThread(r.townRoot, sessionID, "reply-reminder", threadID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// IsRecipientMuted checks if a mail recipient has DND/muted notifications enabled.
// Returns true if the recipient is muted and should not receive tmux nudges.
// Fails open (returns false) if the agent bead cannot be found or the town root is not set.
func (r *Router) IsRecipientMuted(address string) bool {
	if r.townRoot == "" {
		return false
	}
	return isRecipientMuted(r, address)
}

// isRecipientMuted checks if a mail recipient has DND/muted notifications enabled.
// Returns true if the recipient is muted and should not receive tmux nudges.
// Fails open (returns false) if the agent bead cannot be found.
func isRecipientMuted(r *Router, address string) bool {
	agentBeadID := addressToAgentBeadID(address)
	if agentBeadID == "" {
		return false // Can't determine agent bead, allow notification
	}

	bd := beads.New(r.townRoot)
	level, err := bd.GetAgentNotificationLevel(agentBeadID)
	if err != nil {
		return false // Agent bead might not exist, allow notification
	}

	return level == beads.NotifyMuted
}

// addressToAgentBeadID converts a mail address to an agent bead ID for DND lookup.
// Returns empty string if the address cannot be converted.
func addressToAgentBeadID(address string) string {
	if agentBeadID, ok := townAddressToAgentBeadID(address); ok {
		return agentBeadID
	}
	if isReservedTownSubpath(address) {
		return ""
	}

	parts := strings.SplitN(address, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return ""
	}

	return agentBeadIDForTarget(parts[0], parts[1])
}

func townAddressToAgentBeadID(address string) (string, bool) {
	if address == "overseer" {
		return "", true // Overseer is a human, no agent bead.
	}
	if dogName, ok := DogAddressName(address); ok {
		return session.DogSessionName(dogName), true
	}
	switch address {
	case constants.RoleMayor, constants.RoleMayor + "/":
		return session.MayorSessionName(), true
	case constants.RoleDeacon, constants.RoleDeacon + "/":
		return session.DeaconSessionName(), true
	default:
		return "", false
	}
}

func agentBeadIDForTarget(rig, target string) string {
	rigPrefix := session.PrefixFor(rig)
	switch {
	case target == constants.RoleWitness:
		return session.WitnessSessionName(rigPrefix)
	case target == constants.RoleRefinery:
		return session.RefinerySessionName(rigPrefix)
	case strings.HasPrefix(target, "crew/"):
		return session.CrewSessionName(rigPrefix, strings.TrimPrefix(target, "crew/"))
	case strings.HasPrefix(target, "polecat/"):
		return session.PolecatSessionName(rigPrefix, strings.TrimPrefix(target, "polecat/"))
	case strings.HasPrefix(target, "polecats/"):
		return session.PolecatSessionName(rigPrefix, strings.TrimPrefix(target, "polecats/"))
	default:
		return session.PolecatSessionName(rigPrefix, target)
	}
}

// AddressToSessionIDs converts a mail address to possible tmux session IDs.
// Returns multiple candidates since the canonical address format (rig/name)
// doesn't distinguish between crew workers (gt-rig-crew-name) and polecats
// (gt-rig-name). The caller should try each and use the one that exists.
//
// This supersedes the approach in PR #896 which only handled slash-to-dash
// conversion but didn't address the crew/polecat ambiguity.
func AddressToSessionIDs(address string) []string {
	if ids, ok := townAddressToSessionIDs(address); ok {
		return ids
	}
	if isReservedTownSubpath(address) {
		return nil
	}

	// Rig-based address: "rig/target" or "rig/crew/name" or "rig/polecats/name"
	parts := strings.SplitN(address, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return nil
	}

	return rigAddressSessionIDs(parts[0], parts[1])
}

func townAddressToSessionIDs(address string) ([]string, bool) {
	if address == "overseer" {
		return []string{session.OverseerSessionName()}, true
	}
	if dogName, ok := DogAddressName(address); ok {
		return []string{session.DogSessionName(dogName)}, true
	}
	if address == constants.RoleMayor || address == constants.RoleMayor+"/" {
		return []string{session.MayorSessionName()}, true
	}
	if address == constants.RoleDeacon || address == constants.RoleDeacon+"/" {
		return []string{session.DeaconSessionName()}, true
	}
	return nil, false
}

func rigAddressSessionIDs(rig, target string) []string {
	rigPrefix := session.PrefixFor(rig)
	if id, ok := explicitRigSessionID(rigPrefix, target); ok {
		return []string{id}
	}

	// For normalized addresses like "gastown/holden", try both:
	// 1. Crew format: gt-crew-holden
	// 2. Polecat format: gt-holden
	// Return crew first since crew workers are more commonly missed.
	return []string{
		session.CrewSessionName(rigPrefix, target),    // <prefix>-crew-name
		session.PolecatSessionName(rigPrefix, target), // <prefix>-name
	}
}

func explicitRigSessionID(rigPrefix, target string) (string, bool) {
	switch {
	case strings.HasPrefix(target, "crew/"):
		return session.CrewSessionName(rigPrefix, strings.TrimPrefix(target, "crew/")), true
	case strings.HasPrefix(target, "polecat/"):
		return session.PolecatSessionName(rigPrefix, strings.TrimPrefix(target, "polecat/")), true
	case strings.HasPrefix(target, "polecats/"):
		return session.PolecatSessionName(rigPrefix, strings.TrimPrefix(target, "polecats/")), true
	case target == constants.RoleWitness:
		return session.WitnessSessionName(rigPrefix), true
	case target == constants.RoleRefinery:
		return session.RefinerySessionName(rigPrefix), true
	default:
		return "", false
	}
}
