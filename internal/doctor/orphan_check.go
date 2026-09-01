package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/process"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// OrphanSessionCheck detects orphaned tmux sessions that don't match
// the expected Gas Town session naming patterns.
type OrphanSessionCheck struct {
	FixableCheck
	sessionLister  SessionLister
	orphanSessions []string // Cached during Run for use in Fix
}

// SessionLister abstracts tmux session listing for testing.
type SessionLister interface {
	ListSessions() ([]string, error)
}

type realSessionLister struct {
	t *tmux.Tmux
}

func (r *realSessionLister) ListSessions() ([]string, error) {
	return r.t.ListSessions()
}

// NewOrphanSessionCheck creates a new orphan session check.
func NewOrphanSessionCheck() *OrphanSessionCheck {
	return &OrphanSessionCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "orphan-sessions",
				CheckDescription: "Detect orphaned tmux sessions",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// NewOrphanSessionCheckWithSessionLister creates a check with a custom session lister (for testing).
func NewOrphanSessionCheckWithSessionLister(lister SessionLister) *OrphanSessionCheck {
	check := NewOrphanSessionCheck()
	check.sessionLister = lister
	return check
}

// Run checks for orphaned Gas Town tmux sessions.
func (c *OrphanSessionCheck) Run(ctx *CheckContext) *CheckResult {
	lister := c.getSessionLister()

	sessions, err := lister.ListSessions()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not list tmux sessions",
			Details: []string{err.Error()},
		}
	}

	if len(sessions) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No tmux sessions found",
		}
	}

	validRigs := c.getValidRigs(ctx.TownRoot)
	orphans, validCount := c.classifySessions(sessions, validRigs)

	c.orphanSessions = orphans
	return c.sessionResult(validCount, orphans)
}

func (c *OrphanSessionCheck) getSessionLister() SessionLister {
	if c.sessionLister != nil {
		return c.sessionLister
	}
	return &realSessionLister{t: tmux.NewTmux()}
}

func (c *OrphanSessionCheck) classifySessions(sessions, validRigs []string) ([]string, int) {
	mayorSession := session.MayorSessionName()
	deaconSession := session.DeaconSessionName()
	var orphans []string
	validCount := 0
	for _, sess := range sessions {
		valid, considered := c.classifySession(sess, validRigs, mayorSession, deaconSession)
		if !considered {
			continue
		}
		if valid {
			validCount++
		} else {
			orphans = append(orphans, sess)
		}
	}
	return orphans, validCount
}

func (c *OrphanSessionCheck) classifySession(sess string, validRigs []string, mayorSession, deaconSession string) (bool, bool) {
	if sess == "" {
		return false, false
	}
	if _, err := session.ParseSessionName(sess); err != nil {
		return false, false
	}
	return c.isValidSession(sess, validRigs, mayorSession, deaconSession), true
}

func (c *OrphanSessionCheck) sessionResult(validCount int, orphans []string) *CheckResult {

	if len(orphans) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d Gas Town sessions are valid", validCount),
		}
	}

	details := make([]string, len(orphans))
	for i, session := range orphans {
		details[i] = fmt.Sprintf("Orphan: %s", session)
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Found %d orphaned session(s)", len(orphans)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to kill orphaned sessions",
	}
}

// Fix kills all orphaned sessions, except crew sessions which are protected.
func (c *OrphanSessionCheck) Fix(_ *CheckContext) error {
	if len(c.orphanSessions) == 0 {
		return nil
	}

	t := tmux.NewTmux()
	var lastErr error

	for _, sess := range c.orphanSessions {
		if err := fixOrphanSession(t, sess); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

func fixOrphanSession(t *tmux.Tmux, sess string) error {
	// SAFEGUARD: Never auto-kill crew sessions. Crew workers are human-managed.
	if isCrewSession(sess) {
		return nil
	}
	_ = events.LogFeed(events.TypeSessionDeath, sess,
		events.SessionDeathPayload(sess, "unknown", "orphan cleanup", "gt doctor"))
	return t.KillSessionWithProcesses(sess)
}

// isCrewSession returns true if the session name matches the crew pattern.
// Crew sessions are gt-<rig>-crew-<name> and are protected from auto-cleanup.
func isCrewSession(sess string) bool {
	identity, err := session.ParseSessionName(sess)
	if err != nil {
		return false
	}
	return identity.Role == session.RoleCrew
}

// getValidRigs returns a list of valid rig names from the workspace.
func (c *OrphanSessionCheck) getValidRigs(townRoot string) []string {
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if _, err := os.Stat(rigsPath); err != nil {
		return nil
	}

	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return nil
	}
	var rigs []string
	for _, entry := range entries {
		if isValidRigEntry(townRoot, entry) {
			rigs = append(rigs, entry.Name())
		}
	}
	return rigs
}

func isValidRigEntry(townRoot string, entry os.DirEntry) bool {
	if !entry.IsDir() || entry.Name() == "mayor" || entry.Name() == ".beads" || strings.HasPrefix(entry.Name(), ".") {
		return false
	}
	polecatsDir := filepath.Join(townRoot, entry.Name(), "polecats")
	if _, err := os.Stat(polecatsDir); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(townRoot, entry.Name(), "crew"))
	return err == nil
}

// isValidSession checks if a session name matches expected Gas Town patterns.
// Valid patterns:
//   - hq-mayor (headquarters mayor session)
//   - hq-deacon (headquarters deacon session)
//   - gt-boot (boot watchdog session)
//   - gt-<rig>-witness
//   - gt-<rig>-refinery
//   - gt-<rig>-<polecat> (where polecat is any name)
//
// Note: We can't verify polecat names without reading state, so we're permissive.
func (c *OrphanSessionCheck) isValidSession(sess string, validRigs []string, mayorSession, deaconSession string) bool {
	if isAlwaysValidSession(sess, mayorSession, deaconSession) {
		return true
	}

	// For rig-specific sessions, extract rig name using canonical parser
	identity, err := session.ParseSessionName(sess)
	if err != nil {
		return false
	}

	rigName := identity.Rig
	if rigName == "" {
		// Town-level session - not orphaned
		return true
	}

	return rigExistsForSession(identity, validRigs)
}

func isAlwaysValidSession(sess, mayorSession, deaconSession string) bool {
	return (mayorSession != "" && sess == mayorSession) ||
		(deaconSession != "" && sess == deaconSession) ||
		sess == session.BootSessionName()
}

func rigExistsForSession(identity *session.AgentIdentity, validRigs []string) bool {
	// For polecats, ParseSessionName assumes rig = everything except last segment,
	// but polecat names can contain hyphens. If the initial parse does not match,
	// check whether a registered rig is the session prefix.
	rigName := identity.Rig
	for _, r := range validRigs {
		if r == rigName {
			return true
		}
	}

	if identity.Role == session.RolePolecat {
		for _, r := range validRigs {
			if session.PrefixFor(r) == identity.Prefix {
				return true
			}
		}
	}
	return false
}

// OrphanProcessCheck detects runtime processes that are not
// running inside a tmux session. These may be user's personal sessions
// or legitimately orphaned processes from crashed Gas Town sessions.
// This check is informational only - it does not auto-fix since we cannot
// distinguish user sessions from orphaned Gas Town processes.
type OrphanProcessCheck struct {
	BaseCheck
}

// NewOrphanProcessCheck creates a new orphan process check.
func NewOrphanProcessCheck() *OrphanProcessCheck {
	return &OrphanProcessCheck{
		BaseCheck: BaseCheck{
			CheckName:        "orphan-processes",
			CheckDescription: "Detect runtime processes outside tmux",
			CheckCategory:    CategoryCleanup,
		},
	}
}

// Run checks for runtime processes running outside tmux.
func (c *OrphanProcessCheck) Run(_ *CheckContext) *CheckResult {
	// Get list of tmux session PIDs
	tmuxPIDs, err := c.getTmuxSessionPIDs()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not get tmux session info",
			Details: []string{err.Error()},
		}
	}

	// Find runtime processes
	runtimeProcs, err := c.findRuntimeProcesses()
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not list runtime processes",
			Details: []string{err.Error()},
		}
	}

	if len(runtimeProcs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No runtime processes found",
		}
	}

	// Check which runtime processes are outside tmux
	var outsideTmux []processInfo
	var insideTmux int

	for _, proc := range runtimeProcs {
		if c.isOrphanProcess(proc, tmuxPIDs) {
			outsideTmux = append(outsideTmux, proc)
		} else {
			insideTmux++
		}
	}

	if len(outsideTmux) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d runtime processes are inside tmux", insideTmux),
		}
	}

	details := make([]string, 0, len(outsideTmux)+2)
	details = append(details, "These may be your personal sessions or orphaned Gas Town processes.")
	details = append(details, "Verify these are expected before manually killing any:")
	for _, proc := range outsideTmux {
		details = append(details, fmt.Sprintf("  PID %d: %s (parent: %d)", proc.pid, proc.cmd, proc.ppid))
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Found %d runtime process(es) running outside tmux", len(outsideTmux)),
		Details: details,
	}
}

type processInfo struct {
	pid  int
	ppid int
	cmd  string
}

// getTmuxSessionPIDs returns PIDs of all tmux server processes and pane shell PIDs.
func (c *OrphanProcessCheck) getTmuxSessionPIDs() (map[int]bool, error) { //nolint:unparam // error return kept for future use
	// Get tmux server PID and all pane PIDs
	pids := make(map[int]bool)

	// Find tmux server processes using ps instead of pgrep.
	// pgrep -x tmux is unreliable on macOS - it often misses the actual server.
	// We use ps with awk to find processes where comm is exactly "tmux" or starts with "tmux:".
	// On Linux, tmux servers show as "tmux: server" in the comm field.
	out, err := exec.Command("sh", "-c", `ps ax -o pid,comm | awk '$2 == "tmux" || $2 ~ /\/tmux$/ || $2 ~ /^tmux:/ { print $1 }'`).Output()
	if err != nil {
		// No tmux server running
		return pids, nil
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil {
			pids[pid] = true
		}
	}

	// Also get shell PIDs inside tmux panes
	t := tmux.NewTmux()
	sessions, _ := t.ListSessions()
	for _, session := range sessions {
		// Get pane PIDs for this session
		out, err := tmux.BuildCommand("list-panes", "-t", session, "-F", "#{pane_pid}").Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			var pid int
			if _, err := fmt.Sscanf(line, "%d", &pid); err == nil {
				pids[pid] = true
			}
		}
	}

	return pids, nil
}

// argvHasFlag reports whether argv (full command line) contains a standalone flag token.
func argvHasFlag(args, flag string) bool {
	for _, tok := range strings.Fields(args) {
		if tok == flag || strings.HasPrefix(tok, flag+"=") {
			return true
		}
	}
	return false
}

// gasTownRuntimeYOLO returns true when argv matches a Gas Town-managed agent (YOLO / auto-approve),
// excluding personal interactive sessions that omit these flags.
func gasTownRuntimeYOLO(cmdName, args string) bool {
	cmdName = strings.ToLower(filepath.Base(cmdName))
	switch cmdName {
	case "claude", "claude-code", "codex":
		return strings.Contains(args, "--dangerously-skip-permissions")
	case "cursor-agent":
		return argvHasFlag(args, "-f")
	case "agent":
		// Install may symlink cursor-agent as "agent"; require -f plus a Cursor-specific token.
		return argvHasFlag(args, "-f") &&
			(strings.Contains(args, "--resume") || argvHasFlag(args, "-p") || strings.Contains(args, "--print"))
	case "copilot":
		return strings.Contains(args, "--yolo")
	default:
		return false
	}
}

// findRuntimeProcesses finds Gas Town agent processes by per-provider YOLO / launch signatures
// in argv (not comm name alone): claude/codex --dangerously-skip-permissions, cursor-agent -f,
// copilot --yolo, etc.
func (c *OrphanProcessCheck) findRuntimeProcesses() ([]processInfo, error) {
	table, err := process.Capture()
	if err != nil {
		return nil, err
	}

	var procs []processInfo
	for _, p := range table.All() {
		if !gasTownRuntimeYOLO(p.Name, p.Args) {
			continue
		}
		procs = append(procs, processInfo{
			pid:  p.PID,
			ppid: p.PPID,
			cmd:  p.Args,
		})
	}
	return procs, nil
}

// isOrphanProcess checks if a runtime process is orphaned.
// A process is orphaned if its parent (or ancestor) is not a tmux session.
func (c *OrphanProcessCheck) isOrphanProcess(proc processInfo, tmuxPIDs map[int]bool) bool {
	table, err := process.Capture()
	if err != nil {
		return true
	}
	currentPPID := proc.ppid
	visited := make(map[int]bool)

	for currentPPID > 1 && !visited[currentPPID] {
		visited[currentPPID] = true

		if tmuxPIDs[currentPPID] {
			return false
		}

		p, ok := table.Lookup(currentPPID)
		if !ok {
			break
		}
		currentPPID = p.PPID
	}

	return true
}
