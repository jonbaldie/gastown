package beads

import (
	"context"
	"os/exec"
	"path/filepath"

	beadsdk "github.com/jonbaldie/beads"
	agentconfig "github.com/jonbaldie/gastown/internal/config"
)

// Authority is the Beads routing module. Callers ask for a Session for a
// bead ID; prefix routes, redirects, and fallbacks stay inside.
type Authority struct {
	townRoot    string
	fallbackDir string
}

// NewAuthority builds an Authority rooted at a town directory.
// The fallback Beads directory is <townRoot>/.beads.
func NewAuthority(townRoot string) *Authority {
	fallback := ""
	if townRoot != "" {
		fallback = filepath.Join(townRoot, ".beads")
	}
	return &Authority{townRoot: townRoot, fallbackDir: fallback}
}

// NewAuthorityFromBeadsDir builds an Authority from a caller's Beads directory.
// Town routes are discovered by walking up from that directory. If no town is
// found, ForBead falls back to beadsDir.
func NewAuthorityFromBeadsDir(beadsDir string) *Authority {
	townRoot := ""
	if beadsDir != "" {
		townRoot = FindTownRoot(filepath.Dir(beadsDir))
	}
	return &Authority{townRoot: townRoot, fallbackDir: beadsDir}
}

// ForBead returns a Session bound to the database that owns beadID.
func (a *Authority) ForBead(beadID string) Session {
	if a == nil {
		return Session{beadID: beadID}
	}
	s := a.fallbackSession(beadID)
	prefix := ExtractPrefix(beadID)
	if prefix == "" {
		return s
	}

	routesDir, routes, err := a.routesFor(s.beadsDir)
	if err != nil || len(routes) == 0 {
		return s
	}
	route, found := routeForPrefix(routes, prefix)
	if !found {
		return s
	}
	return a.routedSession(beadID, routesDir, route)
}

func (a *Authority) fallbackSession(beadID string) Session {
	return Session{
		beadID:   beadID,
		townRoot: a.townRoot,
		beadsDir: a.fallbackDir,
		workDir:  parentDir(a.fallbackDir),
	}
}

func (a *Authority) routesFor(fallback string) (string, []Route, error) {
	routesDir := a.routeDirectory(fallback)
	routes, err := LoadRoutes(routesDir)
	if err == nil && len(routes) > 0 {
		return routesDir, routes, nil
	}
	return a.fallbackRoutes(routesDir, routes, err, fallback)
}

func (a *Authority) routeDirectory(fallback string) string {
	if a.townRoot == "" {
		return fallback
	}
	return filepath.Join(a.townRoot, ".beads")
}

func (a *Authority) fallbackRoutes(routesDir string, routes []Route, loadErr error, fallback string) (string, []Route, error) {
	townRoot := a.routeTownRoot(fallback)
	if townRoot == "" {
		return routesDir, routes, loadErr
	}
	townBeads := filepath.Join(townRoot, ".beads")
	if townBeads == routesDir {
		return routesDir, routes, loadErr
	}
	routes, err := LoadRoutes(townBeads)
	return townBeads, routes, err
}

func (a *Authority) routeTownRoot(fallback string) string {
	if a.townRoot != "" || fallback == "" {
		return a.townRoot
	}
	return FindTownRoot(filepath.Dir(fallback))
}

func routeForPrefix(routes []Route, prefix string) (Route, bool) {
	for _, route := range routes {
		if route.Prefix == prefix {
			return route, true
		}
	}
	return Route{}, false
}

func (a *Authority) routedSession(beadID, routesDir string, route Route) Session {
	townRoot := a.townRoot
	if townRoot == "" {
		townRoot = filepath.Dir(routesDir)
	}
	beadsDir := ResolveBeadsDir(routesDir)
	if route.Path != "." {
		beadsDir = ResolveBeadsDir(filepath.Join(townRoot, route.Path))
	}
	return Session{
		beadID:   beadID,
		townRoot: townRoot,
		beadsDir: beadsDir,
		workDir:  parentDir(beadsDir),
		routed:   true,
	}
}

// ForAgentBead returns a Session for an agent bead. Agent beads live in the
// town database even when their ID carries a rig prefix.
func (a *Authority) ForAgentBead(beadID string) Session {
	if a == nil {
		return Session{beadID: beadID}
	}
	townBeads := a.fallbackDir
	workDir := parentDir(townBeads)
	if a.townRoot != "" {
		townBeads = filepath.Join(a.townRoot, ".beads")
		workDir = a.townRoot
	}
	return Session{
		beadID:   beadID,
		townRoot: a.townRoot,
		beadsDir: townBeads,
		workDir:  workDir,
	}
}

// Session is a Beads database binding for one bead ID.
type Session struct {
	beadID   string
	townRoot string
	workDir  string
	beadsDir string
	routed   bool
	store    beadsdk.Storage
}

// BeadID returns the bead this Session was opened for.
func (s Session) BeadID() string { return s.beadID }

// BeadsDir returns the resolved .beads directory for this Session.
func (s Session) BeadsDir() string { return s.beadsDir }

// WorkDir returns the directory callers should use as bd's working directory.
// This is the parent of BeadsDir, after redirects.
func (s Session) WorkDir() string { return s.workDir }

// WithStore binds an in-process beads SDK adapter. Show, Update, Close, and
// Hook then use the store instead of the bd CLI.
func (s Session) WithStore(store beadsdk.Storage) Session {
	s.store = store
	return s
}

func (a *Authority) WithFallback(dir string) *Authority {
	if a == nil {
		return &Authority{fallbackDir: dir}
	}
	cp := *a
	if dir != "" {
		cp.fallbackDir = dir
	}
	return &cp
}

// Client returns a Beads wrapper pinned to this Session's database.
// Routing is already applied, so the client does not re-route by prefix.
func (s Session) Client() *Beads {
	return &Beads{
		workDir:  s.workDir,
		beadsDir: s.beadsDir,
		townRoot: s.townRoot,
		noRoute:  true,
		store:    s.store,
	}
}

// Show returns the bead this Session was opened for.
func (s Session) Show() (*Issue, error) {
	if s.beadID == "" {
		return nil, ErrNotFound
	}
	return s.Client().Show(s.beadID)
}

// Update mutates the bead this Session was opened for.
func (s Session) Update(opts UpdateOptions) error {
	if s.beadID == "" {
		return ErrNotFound
	}
	return s.Client().Update(s.beadID, opts)
}

// Close closes the bead this Session was opened for.
func (s Session) Close(reason string) error {
	if s.beadID == "" {
		return ErrNotFound
	}
	if reason == "" {
		return s.Client().Close(s.beadID)
	}
	return s.Client().CloseWithReason(reason, s.beadID)
}

// Hook sets the bead status to hooked and assigns it to agent.
func (s Session) Hook(agent string) error {
	status := StatusHooked
	return s.Update(UpdateOptions{Status: &status, Assignee: &agent})
}

// Command builds a bd subprocess targeted at this Session's database.
func (s Session) Command(mode SubprocessEnvMode, args ...string) *exec.Cmd {
	return Command(s.workDir, s.beadsDir, mode, args...)
}

// CommandContext builds a context-bound bd subprocess targeted at this Session's database.
func (s Session) CommandContext(ctx context.Context, mode SubprocessEnvMode, args ...string) *exec.Cmd {
	return CommandContext(ctx, s.workDir, s.beadsDir, mode, args...)
}

// Command builds a bd subprocess against the Authority fallback store.
func (a *Authority) Command(mode SubprocessEnvMode, args ...string) *exec.Cmd {
	if a == nil {
		return Command("", "", mode, args...)
	}
	return Command(parentDir(a.fallbackDir), a.fallbackDir, mode, args...)
}

// DoltEndpoint is the host/port chain for this Authority's town.
func (a *Authority) DoltEndpoint() (host string, port int) {
	if a == nil {
		return "", 0
	}
	townRoot := a.townRoot
	if townRoot == "" && a.fallbackDir != "" {
		townRoot = FindTownRoot(filepath.Dir(a.fallbackDir))
	}
	if townRoot == "" {
		return "", 0
	}
	if h, p, ok := agentconfig.ManagedDoltEndpoint(townRoot); ok {
		return h, p
	}
	return agentconfig.ResolveDoltHost(townRoot), agentconfig.ResolveDoltPort(townRoot)
}

func parentDir(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Dir(path)
}
