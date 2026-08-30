package beads

import (
	"path/filepath"
	"sync"

	beadsdk "github.com/jonbaldie/beads"
)

// Beads wraps bd CLI operations for a working directory.
// When store is non-nil, methods with in-process implementations use the
// beadsdk.Storage directly instead of shelling out to the bd CLI. Callers are
// responsible for closing the store.
type Beads struct {
	workDir    string
	beadsDir   string // Optional BEADS_DIR override for cross-database access
	isolated   bool   // If true, suppress inherited beads env vars (for test isolation)
	serverPort int    // If set, pass --server-port to bd init and GT_DOLT_PORT to env

	// store is an optional in-process beadsdk.Storage. When set, methods bypass
	// the bd subprocess and use the store directly.
	store beadsdk.Storage

	// Lazy-cached town root for routing resolution.
	townRoot     string
	townRootOnce sync.Once

	// noRoute disables prefix-based routing for this Beads instance.
	noRoute bool
}

// ForAgentBead returns a Beads wrapper rooted at the town database. Agent
// beads carry rig-prefixed IDs but are stored with other town-level agents.
func (b *Beads) ForAgentBead() *Beads {
	townRoot := b.getTownRoot()
	if townRoot == "" {
		return b
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")
	return &Beads{
		workDir:    townRoot,
		beadsDir:   townBeadsDir,
		isolated:   b.isolated,
		serverPort: b.serverPort,
		store:      b.store,
		townRoot:   townRoot,
		noRoute:    true,
	}
}

func (b *Beads) AgentBeadTarget() *Beads {
	if b.noRoute {
		return b
	}
	return b.ForAgentBead()
}

// getTownRoot returns the Gas Town root directory, using lazy caching.
func (b *Beads) getTownRoot() string {
	b.townRootOnce.Do(func() {
		b.townRoot = FindTownRoot(b.workDir)
	})
	return b.townRoot
}
