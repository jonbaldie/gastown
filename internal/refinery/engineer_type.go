package refinery

import (
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/rig"
)

// Engineer is the merge queue processor that polls for ready merge-requests
// and processes them according to the merge queue design.
type Engineer struct {
	rig                   *rig.Rig
	beads                 *beads.Beads
	git                   *git.Git
	config                *MergeQueueConfig
	prProvider            PRProvider // VCS-specific PR operations (nil when MergeStrategy != "pr")
	workDir               string
	output                io.Writer    // Output destination for user-facing messages
	router                *mail.Router // Mail router for sending protocol messages
	mergeSlotEnsureExists func() (string, error)
	mergeSlotAcquire      func(holder string, addWaiter bool) (*beads.MergeSlotStatus, error)
	mergeSlotRelease      func(holder string) error
	mergeSlotMaxRetries   int           // Max retries for slot acquisition (0 = no retry)
	mergeSlotRetryBackoff time.Duration // Initial backoff between retries
	mergeSlotSeq          atomic.Uint64 // Unique merge slot holder IDs.
	testAllowSyntheticMRs bool          // Test-only: legacy merge-mechanics tests use synthetic MRs without beads.
}

// NewEngineer creates a new Engineer for the given rig.
func NewEngineer(r *rig.Rig) *Engineer {
	cfg := DefaultMergeQueueConfig()

	// Determine the git working directory for refinery operations.
	// Prefer refinery/rig worktree, fall back to mayor/rig (legacy architecture).
	// Using rig.Path directly would find town's .git with rig-named remotes instead of "origin".
	gitDir := filepath.Join(r.Path, "refinery", "rig")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		gitDir = filepath.Join(r.Path, "mayor", "rig")
	}
	beadsClient := beads.New(r.Path)

	return &Engineer{
		rig:        r,
		beads:      beadsClient,
		git:        git.NewGit(gitDir),
		config:     cfg,
		prProvider: nil,
		workDir:    gitDir,
		output:     os.Stdout,
		router:     mail.NewRouter(r.Path),
		mergeSlotEnsureExists: func() (string, error) {
			return beadsClient.MergeSlotEnsureExists()
		},
		mergeSlotAcquire: func(holder string, addWaiter bool) (*beads.MergeSlotStatus, error) {
			return beadsClient.MergeSlotAcquire(holder, addWaiter)
		},
		mergeSlotRelease: func(holder string) error {
			return beadsClient.MergeSlotRelease(holder)
		},
		mergeSlotMaxRetries:   10,
		mergeSlotRetryBackoff: 500 * time.Millisecond,
		mergeSlotSeq:          atomic.Uint64{},
		testAllowSyntheticMRs: false,
	}
}
