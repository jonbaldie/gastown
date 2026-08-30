package mail

import beadsdk "github.com/jonbaldie/beads"

import (
	"path/filepath"

	"github.com/jonbaldie/gastown/internal/beads"
)

// Mailbox manages messages for an identity via beads.
// When store is non-nil, beads-mode methods use the in-process beadsdk.Storage
// directly instead of shelling out to the bd CLI.
type Mailbox struct {
	identity string // beads identity (e.g., "gastown/polecats/Toast")
	workDir  string // directory to run bd commands in
	beadsDir string // explicit .beads directory path (set via BEADS_DIR)
	path     string // for legacy JSONL mode (crew workers)
	legacy   bool   // true = use JSONL files, false = use beads

	// store is an optional in-process beadsdk.Storage. When set, beads-mode
	// methods bypass the bd subprocess and use the store directly.
	// Callers are responsible for closing the store.
	store beadsdk.Storage
}

// NewMailbox creates a mailbox for the given JSONL path (legacy mode).
// Used by crew workers that have local JSONL inboxes.
func NewMailbox(path string) *Mailbox {
	return &Mailbox{
		path:   filepath.Join(path, "inbox.jsonl"),
		legacy: true,
	}
}

// NewMailboxBeads creates a mailbox backed by beads.
func NewMailboxBeads(identity, workDir string) *Mailbox {
	return &Mailbox{
		identity: identity,
		workDir:  workDir,
		legacy:   false,
	}
}

// NewMailboxFromAddress creates a beads-backed mailbox from a GGT address.
// Follows .beads/redirect for crew workers and polecats using shared beads.
func NewMailboxFromAddress(address, workDir string) *Mailbox {
	beadsDir := beads.ResolveBeadsDir(workDir)
	return &Mailbox{
		identity: AddressToIdentity(address),
		workDir:  workDir,
		beadsDir: beadsDir,
		legacy:   false,
	}
}

// NewMailboxWithBeadsDir creates a mailbox with an explicit beads directory.
func NewMailboxWithBeadsDir(address, workDir, beadsDir string) *Mailbox {
	return &Mailbox{
		identity: AddressToIdentity(address),
		workDir:  workDir,
		beadsDir: beadsDir,
		legacy:   false,
	}
}

// SetStore configures an in-process beadsdk.Storage for this Mailbox.
func (m *Mailbox) SetStore(store beadsdk.Storage) {
	m.store = store
}

// Store returns the in-process beadsdk.Storage, or nil if not set.
func (m *Mailbox) Store() beadsdk.Storage {
	return m.store
}
