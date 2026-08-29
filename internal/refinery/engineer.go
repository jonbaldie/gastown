// Package refinery provides the merge queue processing agent.
package refinery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/util"
)

// shortSHA returns at most 8 characters of a SHA for display.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// DefaultStaleClaimTimeout is the default duration after which a claimed MR
// is considered abandoned and eligible for re-claim. This is conservative
// to avoid re-claiming MRs that are legitimately processing long test suites.
// Can be overridden per-rig via MergeQueueConfig.StaleClaimTimeout.
const DefaultStaleClaimTimeout = 30 * time.Minute

// isClaimStale checks if a claimed MR should be considered abandoned based on
// its UpdatedAt timestamp and configured timeout. Returns true if the claim
// is stale (eligible for re-claim), false if the claim is recent or the
// timestamp is invalid/missing.
func isClaimStale(updatedAt string, timeout time.Duration) (stale bool, parseErr error) {
	if updatedAt == "" {
		return false, nil // No timestamp - assume claim is valid
	}
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return false, err // Caller should log the parse error
	}
	return time.Since(t) >= timeout, nil
}

// GateConfig defines a single quality gate command.
// GatePhase controls when a gate runs in the merge pipeline.
type GatePhase string

const (
	// GatePhasePreMerge runs the gate before the squash merge (default).
	// The gate validates the source branch on the target baseline.
	GatePhasePreMerge GatePhase = "pre-merge"

	// GatePhasePostSquash runs the gate after the squash merge but before push.
	// The gate validates the actual combined code, catching issues that only
	// manifest in the merged result (broken imports, boot failures, missing
	// templates). On failure, the merge is reset.
	GatePhasePostSquash GatePhase = "post-squash"
)

type GateConfig struct {
	// Cmd is the shell command to execute.
	Cmd string `json:"cmd"`

	// Timeout is the maximum time the gate command may run.
	// Zero means no timeout (inherits context deadline).
	Timeout time.Duration `json:"timeout"`

	// Phase controls when this gate runs: "pre-merge" (default) or "post-squash".
	// Pre-merge gates run before the squash merge on the source branch.
	// Post-squash gates run after the squash merge on the combined result,
	// before pushing. On post-squash failure, the merge is reset.
	Phase GatePhase `json:"phase"`
}

// GateResult holds the outcome of a single gate execution.
type GateResult struct {
	Name    string
	Success bool
	Error   string
	Elapsed time.Duration
}

// MergeQueueConfig holds configuration for the merge queue processor.
//
// Note: Integration branch gating (polecat/refinery enabled flags) is handled at
// MR creation time via config.MergeQueueConfig and formula injection, not here.
// The Engineer's job is to merge whatever target the MR specifies — it doesn't
// need to know whether integration branches are enabled.
type MergeQueueConfig struct {
	// Enabled controls whether the merge queue is active.
	Enabled bool `json:"enabled"`

	// OnConflict is the strategy for handling conflicts: "assign_back" or "auto_rebase".
	OnConflict string `json:"on_conflict"`

	// RunTests controls whether to run tests before merging.
	RunTests bool `json:"run_tests"`

	// TestCommand is the command to run for testing.
	TestCommand string `json:"test_command"`

	// DeleteMergedBranches controls whether to delete branches after merge.
	DeleteMergedBranches bool `json:"delete_merged_branches"`

	// RetryFlakyTests is the number of times to retry flaky tests.
	RetryFlakyTests int `json:"retry_flaky_tests"`

	// PollInterval is how often to check for new MRs.
	PollInterval time.Duration `json:"poll_interval"`

	// MaxConcurrent is the maximum number of MRs to process concurrently.
	MaxConcurrent int `json:"max_concurrent"`

	// StaleClaimTimeout is how long a claimed MR can go without updates before
	// being considered abandoned and eligible for re-claim. This handles the
	// case where a refinery crashes mid-merge, leaving an MR permanently claimed.
	// Set conservatively to avoid re-claiming MRs with long-running test suites.
	// NOTE: Only one refinery instance runs per rig (enforced by ErrAlreadyRunning
	// in manager.go), so concurrent re-claim is not a concern in practice.
	StaleClaimTimeout time.Duration `json:"stale_claim_timeout"`

	// Gates defines named quality gate commands to run before merging.
	// When non-empty, gates replace the legacy RunTests/TestCommand path.
	// Each gate runs as a shell command with an optional per-gate timeout.
	Gates map[string]*GateConfig `json:"gates"`

	// GatesParallel controls whether gates run concurrently.
	// When true, all gates start simultaneously; any failure = overall failure.
	GatesParallel bool `json:"gates_parallel"`

	// StaleClaimWarningAfter is how long a claimed MR can sit without updates
	// before it triggers a "warning" severity anomaly.
	StaleClaimWarningAfter time.Duration `json:"stale_claim_warning_after"`

	// StaleClaimCriticalAfter is how long a claimed MR can sit without updates
	// before it triggers a "critical" severity anomaly.
	StaleClaimCriticalAfter time.Duration `json:"stale_claim_critical_after"`

	// MaxRetryCount is the maximum number of conflict resolution retries
	// before escalation to Mayor.
	MaxRetryCount int `json:"max_retry_count"`

	// AutoPush controls whether the refinery pushes to origin after merging.
	// When false, the refinery merges locally but does not push — the user
	// or a separate process handles pushing. Useful to avoid triggering
	// CI/CD builds (e.g. Vercel) on every merge.
	AutoPush bool `json:"auto_push"`

	// MergeStrategy controls how the refinery lands work: "direct" (default)
	// does local merge + git push; "pr" uses the VCS provider's merge API
	// which respects branch protection/restriction rules.
	MergeStrategy string `json:"merge_strategy,omitempty"`

	// VCSProvider selects the VCS platform for PR operations when
	// MergeStrategy="pr". Valid values: "github" (default), "bitbucket".
	VCSProvider string `json:"vcs_provider,omitempty"`

	// RequireReview controls whether the refinery requires at least one approving
	// review before merging a PR. Only meaningful when MergeStrategy="pr".
	// Nil defaults to false (no review required).
	RequireReview *bool `json:"require_review,omitempty"`

	// Batch holds configuration for the batch-then-bisect merge queue.
	// When nil or MaxBatchSize <= 1, batching is disabled and MRs process sequentially.
	Batch *BatchConfig `json:"batch,omitempty"`
}

// DefaultMergeQueueConfig returns sensible defaults for merge queue configuration.
func DefaultMergeQueueConfig() *MergeQueueConfig {
	return &MergeQueueConfig{
		Enabled:                 true,
		OnConflict:              "assign_back",
		RunTests:                true,
		TestCommand:             "",
		DeleteMergedBranches:    true,
		GatesParallel:           true, // gt-8b2i: run gates concurrently (~2x speedup)
		RetryFlakyTests:         1,
		PollInterval:            30 * time.Second,
		MaxConcurrent:           1,
		StaleClaimTimeout:       DefaultStaleClaimTimeout,
		StaleClaimWarningAfter:  2 * time.Hour,
		StaleClaimCriticalAfter: 6 * time.Hour,
		MaxRetryCount:           5,
		AutoPush:                true,
	}
}

// MRInfo holds merge request information for display and processing.
// This replaces mrqueue.MR after the mrqueue package removal.
type MRInfo struct {
	ID              string     // Bead ID (e.g., "gt-abc123")
	Branch          string     // Source branch (e.g., "polecat/nux")
	Target          string     // Target branch (e.g., "main")
	SourceIssue     string     // The work item being merged
	Worker          string     // Who did the work
	Rig             string     // Which rig
	Title           string     // MR title
	Priority        int        // Priority (lower = higher priority)
	AgentBead       string     // Agent bead ID that created this MR
	CommitSHA       string     // Source branch tip submitted to the queue
	PRURL           string     // Recorded pull request URL, if available
	PRNumber        int        // Recorded pull request number, if available
	RetryCount      int        // Conflict retry count
	ConflictTaskID  string     // Open conflict-resolution task for this MR (if any)
	ConvoyID        string     // Parent convoy ID if part of a convoy
	ConvoyCreatedAt *time.Time // Convoy creation time
	CreatedAt       time.Time  // MR creation time
	BlockedBy       string     // Task ID blocking this MR

	// Pre-verification fields (Phase 3: polecat-owned rebasing)
	// When set, the refinery can skip gates if VerifiedBase matches target HEAD.
	PreVerified     bool      // Polecat ran full gates after rebasing onto target
	PreVerifiedAt   time.Time // When verification completed
	PreVerifiedBase string    // Target branch SHA at verification time

	// Raw data for agent-side queue health analysis (ZFC: agent decides, Go transports)
	UpdatedAt          time.Time // When the MR was last updated
	Assignee           string    // Who claimed this MR (empty = unclaimed)
	BranchExistsLocal  bool      // Whether the MR branch exists locally
	BranchExistsRemote bool      // Whether the MR branch exists in remote tracking refs
}

// MRAnomaly represents an MR queue health problem that can stall processing.
type MRAnomaly struct {
	ID       string        `json:"id"`
	Branch   string        `json:"branch"`
	Type     string        `json:"type"` // stale-claim | orphaned-branch
	Assignee string        `json:"assignee,omitempty"`
	Age      time.Duration `json:"age,omitempty"`
	Detail   string        `json:"detail"`
}

// errMergeSlotTimeout is returned by acquireMainPushSlot when retries are
// exhausted due to slot contention. Infrastructure errors (beads down,
// permission errors) return a different error so callers can distinguish
// transient contention from real failures that need operator attention.
var errMergeSlotTimeout = errors.New("merge slot contention timeout")

// mergeSlotSeq is a package-level counter for unique merge slot holder IDs.
// Using time.Now().UnixNano() alone is insufficient on Windows where timer
// resolution can cause identical timestamps across concurrent goroutines.
var mergeSlotSeq uint64

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
		rig:     r,
		beads:   beadsClient,
		git:     git.NewGit(gitDir),
		config:  cfg,
		workDir: gitDir,
		output:  os.Stdout,
		router:  mail.NewRouter(r.Path),
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
	}
}

// SetOutput sets the output writer for user-facing messages.
// This is useful for testing or redirecting output.
func (e *Engineer) SetOutput(w io.Writer) {
	e.output = w
}

// LoadConfig loads merge queue configuration from the rig's config.json.
type mergeQueueCoreRaw struct {
	Enabled              *bool   `json:"enabled"`
	OnConflict           *string `json:"on_conflict"`
	RunTests             *bool   `json:"run_tests"`
	TestCommand          *string `json:"test_command"`
	DeleteMergedBranches *bool   `json:"delete_merged_branches"`
	RetryFlakyTests      *int    `json:"retry_flaky_tests"`
	PollInterval         *string `json:"poll_interval"`
	MaxConcurrent        *int    `json:"max_concurrent"`
	StaleClaimTimeout    *string `json:"stale_claim_timeout"`
}

type mergeQueueAdvancedRaw struct {
	Gates         map[string]*gateConfigRaw `json:"gates"`
	GatesParallel *bool                     `json:"gates_parallel"`
	AutoPush      *bool                     `json:"auto_push"`
	MergeStrategy *string                   `json:"merge_strategy"`
	VCSProvider   *string                   `json:"vcs_provider"`
	RequireReview *bool                     `json:"require_review"`
}

// LoadConfig loads merge queue configuration from the rig's config.json.
func (e *Engineer) LoadConfig() error {
	rawQueue, err := readMergeQueueConfig(filepath.Join(e.rig.Path, "config.json"))
	if err != nil {
		return err
	}
	if rawQueue == nil {
		return nil
	}
	if err := e.applyMergeQueueConfig(rawQueue); err != nil {
		return err
	}
	if e.config.MergeStrategy == "pr" {
		if err := e.initPRProvider(); err != nil {
			return fmt.Errorf("initializing PR provider: %w", err)
		}
	}
	return nil
}

func readMergeQueueConfig(configPath string) (json.RawMessage, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var rawConfig struct {
		MergeQueue json.RawMessage `json:"merge_queue"`
	}
	if err := json.Unmarshal(data, &rawConfig); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return rawConfig.MergeQueue, nil
}

func (e *Engineer) applyMergeQueueConfig(rawQueue json.RawMessage) error {
	core, advanced, err := parseMergeQueueRaw(rawQueue)
	if err != nil {
		return err
	}
	if err := applyMergeQueueCore(e.config, core); err != nil {
		return err
	}
	return applyMergeQueueAdvanced(e.config, advanced)
}

func parseMergeQueueRaw(rawQueue json.RawMessage) (mergeQueueCoreRaw, mergeQueueAdvancedRaw, error) {
	var core mergeQueueCoreRaw
	if err := json.Unmarshal(rawQueue, &core); err != nil {
		return mergeQueueCoreRaw{}, mergeQueueAdvancedRaw{}, fmt.Errorf("parsing merge_queue config: %w", err)
	}
	var advanced mergeQueueAdvancedRaw
	if err := json.Unmarshal(rawQueue, &advanced); err != nil {
		return mergeQueueCoreRaw{}, mergeQueueAdvancedRaw{}, fmt.Errorf("parsing merge_queue config: %w", err)
	}
	return core, advanced, nil
}

func applyMergeQueueCore(cfg *MergeQueueConfig, raw mergeQueueCoreRaw) error {
	applyMergeQueueCoreValues(cfg, raw)
	return applyMergeQueueDurations(cfg, raw)
}

func applyMergeQueueCoreValues(cfg *MergeQueueConfig, raw mergeQueueCoreRaw) {
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.OnConflict != nil {
		cfg.OnConflict = *raw.OnConflict
	}
	if raw.RunTests != nil {
		cfg.RunTests = *raw.RunTests
	}
	if raw.TestCommand != nil {
		cfg.TestCommand = *raw.TestCommand
	}
	if raw.DeleteMergedBranches != nil {
		cfg.DeleteMergedBranches = *raw.DeleteMergedBranches
	}
	if raw.RetryFlakyTests != nil {
		cfg.RetryFlakyTests = *raw.RetryFlakyTests
	}
	if raw.MaxConcurrent != nil {
		cfg.MaxConcurrent = *raw.MaxConcurrent
	}
}

func applyMergeQueueDurations(cfg *MergeQueueConfig, raw mergeQueueCoreRaw) error {
	if raw.PollInterval != nil {
		dur, err := time.ParseDuration(*raw.PollInterval)
		if err != nil {
			return fmt.Errorf("invalid poll_interval %q: %w", *raw.PollInterval, err)
		}
		cfg.PollInterval = dur
	}
	if raw.StaleClaimTimeout != nil {
		dur, err := time.ParseDuration(*raw.StaleClaimTimeout)
		if err != nil {
			return fmt.Errorf("invalid stale_claim_timeout %q: %w", *raw.StaleClaimTimeout, err)
		}
		if dur <= 0 {
			return fmt.Errorf("stale_claim_timeout must be positive, got %v", dur)
		}
		cfg.StaleClaimTimeout = dur
	}
	return nil
}

func applyMergeQueueAdvanced(cfg *MergeQueueConfig, raw mergeQueueAdvancedRaw) error {
	if raw.Gates != nil {
		gates, err := parseGateConfigs(raw.Gates)
		if err != nil {
			return err
		}
		cfg.Gates = gates
	}
	if raw.GatesParallel != nil {
		cfg.GatesParallel = *raw.GatesParallel
	}
	if raw.AutoPush != nil {
		cfg.AutoPush = *raw.AutoPush
	}
	if raw.MergeStrategy != nil {
		cfg.MergeStrategy = *raw.MergeStrategy
	}
	if raw.VCSProvider != nil {
		cfg.VCSProvider = *raw.VCSProvider
	}
	if raw.RequireReview != nil {
		cfg.RequireReview = raw.RequireReview
	}
	return nil
}

func parseGateConfigs(rawGates map[string]*gateConfigRaw) (map[string]*GateConfig, error) {
	gates := make(map[string]*GateConfig, len(rawGates))
	for name, raw := range rawGates {
		gate, err := parseGateConfig(name, raw)
		if err != nil {
			return nil, err
		}
		gates[name] = gate
	}
	return gates, nil
}

func parseGateConfig(name string, raw *gateConfigRaw) (*GateConfig, error) {
	gate := &GateConfig{Cmd: raw.Cmd}
	if raw.Timeout != "" {
		dur, err := time.ParseDuration(raw.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout for gate %q: %w", name, err)
		}
		if dur <= 0 {
			return nil, fmt.Errorf("gate %q timeout must be positive, got %v", name, dur)
		}
		gate.Timeout = dur
	}
	switch raw.Phase {
	case "", "pre-merge":
		gate.Phase = GatePhasePreMerge
	case "post-squash":
		gate.Phase = GatePhasePostSquash
	default:
		return nil, fmt.Errorf("gate %q has invalid phase %q: must be \"pre-merge\" or \"post-squash\"", name, raw.Phase)
	}
	return gate, nil
}

// initPRProvider creates the appropriate PRProvider based on vcs_provider config.
// Defaults to GitHub when vcs_provider is empty or "github".
func (e *Engineer) initPRProvider() error {
	switch e.config.VCSProvider {
	case "", "github":
		e.prProvider = newGitHubPRProvider(e.git)
	case "bitbucket":
		p, err := newBitbucketPRProvider(e.git)
		if err != nil {
			return err
		}
		e.prProvider = p
	default:
		return fmt.Errorf("unknown vcs_provider %q (supported: github, bitbucket)", e.config.VCSProvider)
	}
	return nil
}

// gateConfigRaw is the JSON-friendly representation of a gate config
// with timeout as a string duration.
type gateConfigRaw struct {
	Cmd     string `json:"cmd"`
	Timeout string `json:"timeout"`
	Phase   string `json:"phase"`
}

// Config returns the current merge queue configuration.
func (e *Engineer) Config() *MergeQueueConfig {
	return e.config
}

// ProcessResult contains the result of processing a merge request.
type ProcessResult struct {
	Success        bool
	MergeCommit    string
	Error          string
	Conflict       bool
	TestsFailed    bool
	SlotTimeout    bool // Merge slot contention timeout (distinct from build/test failure)
	BranchNotFound bool // Source branch no longer exists (e.g. cleaned up after cherry-pick)
	NoMerge        bool // MR/source is intentionally not merge-eligible, not a build failure
	NeedsApproval  bool // PR exists but lacks required approving review (merge_strategy=pr)
}

// doMerge performs the actual git merge operation.
func (e *Engineer) doMerge(ctx context.Context, mr *MRInfo, skipGates ...bool) ProcessResult {
	if mr == nil {
		return ProcessResult{Success: false, Error: "merge request is missing"}
	}
	branch, target := mr.Branch, mr.Target

	if eligibility := e.recheckMRStillMergeable(mr, target); !eligibility.Success {
		if eligibility.NoMerge {
			_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s is not merge-eligible — skipping merge: %s\n", mr.ID, eligibility.Error)
		}
		return eligibility
	}

	mergeRef, preparation := e.prepareMerge(mr, branch, target)
	if !preparation.Success {
		return preparation
	}

	shouldSkipGates := skipMergeGates(skipGates)
	if result := e.runConfiguredMergeGates(ctx, shouldSkipGates); !result.Success {
		return result
	}

	mergeCommit, mergeResult := e.executeMerge(ctx, mr, branch, target, mergeRef, shouldSkipGates)
	if !mergeResult.Success {
		return mergeResult
	}

	if result := e.publishMergeResult(ctx, mr, target, mergeCommit); !result.Success {
		return result
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] Successfully merged: %s\n", shortSHA(mergeCommit))
	return ProcessResult{
		Success:     true,
		MergeCommit: mergeCommit,
	}
}

func skipMergeGates(skipGates []bool) bool {
	return len(skipGates) > 0 && skipGates[0]
}

func (e *Engineer) executeMerge(ctx context.Context, mr *MRInfo, branch, target, mergeRef string, skipGates bool) (string, ProcessResult) {
	if e.config.MergeStrategy == "pr" {
		result := e.doMergePR(ctx, mr)
		return result.MergeCommit, result
	}
	return e.mergeTarget(ctx, mr, branch, target, mergeRef, skipGates)
}

func (e *Engineer) publishMergeResult(ctx context.Context, mr *MRInfo, target, mergeCommit string) ProcessResult {
	if !e.config.AutoPush {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Auto-push disabled, skipping push to origin/%s\n", target)
		return ProcessResult{Success: true}
	}
	return e.pushMergeResult(ctx, mr, target, mergeCommit)
}

func (e *Engineer) prepareMerge(mr *MRInfo, branch, target string) (string, ProcessResult) {
	mergeRef, result := e.validateMergeSource(mr, branch)
	if !result.Success {
		return "", result
	}
	if result := e.checkoutMergeTarget(target, mergeRef); !result.Success {
		return "", result
	}
	if result := e.pushSubmoduleChanges(mr, target, mergeRef); !result.Success {
		return "", result
	}
	return mergeRef, ProcessResult{Success: true}
}

func (e *Engineer) validateMergeSource(mr *MRInfo, branch string) (string, ProcessResult) {
	_, _ = fmt.Fprintf(e.output, "[Engineer] Checking local branch %s...\n", branch)
	exists, err := e.git.BranchExists(branch)
	if err != nil {
		return "", ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("failed to check branch %s: %v", branch, err),
		}
	}
	if !exists {
		return "", ProcessResult{
			Success:        false,
			BranchNotFound: true,
			Error:          fmt.Sprintf("branch %s not found locally", branch),
		}
	}
	mergeRef, err := e.submittedBranchHead(mr)
	if err != nil {
		return "", ProcessResult{Success: false, Error: err.Error()}
	}
	return mergeRef, ProcessResult{Success: true}
}

func (e *Engineer) checkoutMergeTarget(target, mergeRef string) ProcessResult {
	_, _ = fmt.Fprintf(e.output, "[Engineer] Checking out target branch %s...\n", target)
	if err := e.git.Checkout(target); err != nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("failed to checkout target %s: %v", target, err),
		}
	}
	if err := e.git.Pull("origin", target); err != nil {
		// Pull might fail if nothing to pull, that's ok
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: pull from origin/%s: %v (continuing)\n", target, err)
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] Checking for conflicts...\n")
	conflicts, err := e.git.CheckConflicts(mergeRef, target)
	if err != nil {
		return ProcessResult{
			Success:  false,
			Conflict: true,
			Error:    fmt.Sprintf("conflict check failed: %v", err),
		}
	}
	if len(conflicts) > 0 {
		return ProcessResult{
			Success:  false,
			Conflict: true,
			Error:    fmt.Sprintf("merge conflicts in: %v", conflicts),
		}
	}
	return ProcessResult{Success: true}
}

func (e *Engineer) pushSubmoduleChanges(mr *MRInfo, target, mergeRef string) ProcessResult {
	subChanges, err := e.git.SubmoduleChanges(target, mergeRef)
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not check submodule changes: %v\n", err)
	}
	if len(subChanges) == 0 {
		return ProcessResult{Success: true}
	}

	// Ensure submodules are initialized in the refinery worktree.
	// Use mayor/rig as reference to avoid re-fetching from remote.
	mayorRig := filepath.Join(e.rig.Path, "mayor", "rig")
	if initErr := git.InitSubmodules(e.git.WorkDir(), mayorRig); initErr != nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("failed to init submodules in refinery worktree: %v", initErr),
		}
	}
	for _, sc := range subChanges {
		if sc.NewSHA == "" {
			continue // Submodule removed, nothing to push
		}
		if eligibility := e.recheckMRStillMergeable(mr, target); !eligibility.Success {
			return eligibility
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] Pushing submodule %s (commit %s)...\n", sc.Path, shortSHA(sc.NewSHA))
		if pushErr := e.git.PushSubmoduleCommit(sc.Path, sc.NewSHA, "origin"); pushErr != nil {
			return ProcessResult{
				Success: false,
				Error:   fmt.Sprintf("failed to push submodule %s: %v", sc.Path, pushErr),
			}
		}
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Pushed %d submodule(s)\n", len(subChanges))
	return ProcessResult{Success: true}
}

func (e *Engineer) runConfiguredMergeGates(ctx context.Context, shouldSkip bool) ProcessResult {
	if shouldSkip {
		_, _ = fmt.Fprintln(e.output, "[Engineer] Skipping gates (pre-verified by polecat)")
		return ProcessResult{Success: true}
	}
	if len(e.config.Gates) > 0 {
		return e.runGates(ctx)
	}
	if !e.config.RunTests || e.config.TestCommand == "" {
		return ProcessResult{Success: true}
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] Running tests: %s\n", e.config.TestCommand)
	result := e.runTests(ctx)
	if !result.Success {
		return ProcessResult{
			Success:     false,
			TestsFailed: true,
			Error:       result.Error,
		}
	}
	_, _ = fmt.Fprintln(e.output, "[Engineer] Tests passed")
	return ProcessResult{Success: true}
}

func (e *Engineer) mergeTarget(ctx context.Context, mr *MRInfo, branch, target, mergeRef string, skipGates bool) (string, ProcessResult) {
	mergeMsg := e.mergeMessage(branch, target, mr.SourceIssue)
	_, _ = fmt.Fprintf(e.output, "[Engineer] Merging with message: %s\n", strings.TrimSpace(mergeMsg))
	if err := e.git.MergeNoFF(mergeRef, mergeMsg); err != nil {
		conflicts, conflictErr := e.git.GetConflictingFiles()
		if conflictErr == nil && len(conflicts) > 0 {
			_ = e.git.AbortMerge()
			return "", ProcessResult{
				Success:  false,
				Conflict: true,
				Error:    "merge conflict during actual merge",
			}
		}
		_ = e.git.AbortMerge()
		return "", ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("merge failed: %v", err),
		}
	}

	if !skipGates {
		postResult := e.runGatesForPhase(ctx, GatePhasePostSquash)
		if !postResult.Success {
			if resetErr := e.git.ResetHard("origin/" + target); resetErr != nil {
				_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after post-squash gate failure: %v\n", target, resetErr)
			}
			return "", postResult
		}
	}

	mergeCommit, err := e.git.Rev("HEAD")
	if err != nil {
		return "", ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("failed to get merge commit SHA: %v", err),
		}
	}
	return mergeCommit, ProcessResult{Success: true}
}

func (e *Engineer) mergeMessage(branch, target, sourceIssue string) string {
	mergeMsg, err := e.git.GetBranchCommitMessage(branch)
	if err == nil {
		return mergeMsg
	}
	mergeMsg = fmt.Sprintf("Merge %s into %s", branch, target)
	if sourceIssue != "" {
		mergeMsg = fmt.Sprintf("Merge %s into %s (%s)", branch, target, sourceIssue)
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not get original commit message: %v\n", err)
	return mergeMsg
}

func (e *Engineer) pushMergeResult(ctx context.Context, mr *MRInfo, target, mergeCommit string) ProcessResult {
	pushHolder, slotErr := e.acquirePushSlot(ctx, target)
	if slotErr != nil {
		if resetErr := e.git.ResetHard("origin/" + target); resetErr != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after slot failure: %v\n", target, resetErr)
		}
		return ProcessResult{
			Success:     false,
			SlotTimeout: errors.Is(slotErr, errMergeSlotTimeout),
			Error:       fmt.Sprintf("failed to acquire merge slot before push: %v", slotErr),
		}
	}
	if pushHolder != "" {
		defer e.releasePushSlot(pushHolder)
	}

	if eligibility := e.recheckMRStillMergeable(mr, target); !eligibility.Success {
		if resetErr := e.git.ResetHard("origin/" + target); resetErr != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after pre-push eligibility failure: %v\n", target, resetErr)
		}
		return eligibility
	}
	if err := e.pushAndVerifyMerge(target, mergeCommit); err != nil {
		if resetErr := e.git.ResetHard("origin/" + target); resetErr != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to reset %s after push failure: %v\n", target, resetErr)
		}
		return ProcessResult{Success: false, Error: err.Error()}
	}
	return ProcessResult{Success: true}
}

func (e *Engineer) acquirePushSlot(ctx context.Context, target string) (string, error) {
	if target != e.rig.DefaultBranch() {
		return "", nil
	}
	return e.acquireMainPushSlot(ctx)
}

func (e *Engineer) releasePushSlot(holder string) {
	if releaseErr := e.mergeSlotRelease(holder); releaseErr != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to release merge slot for push (%s): %v\n", holder, releaseErr)
	}
}

func (e *Engineer) pushAndVerifyMerge(target, mergeCommit string) error {
	_, _ = fmt.Fprintf(e.output, "[Engineer] Pushing to origin/%s...\n", target)
	if err := e.git.Push("origin", target, false); err != nil {
		return fmt.Errorf("failed to push to origin: %w", err)
	}
	if err := e.git.VerifyPushedCommit("origin", target, mergeCommit); err != nil {
		return err
	}
	return nil
}

// doMergePR handles merging via the VCS provider's PR merge API (merge_strategy=pr).
// This respects branch protection/restriction rules including required reviews.
// The VCS provider (GitHub, Bitbucket) is selected via vcs_provider config.
// Called from doMerge after quality gates have passed.
//
//nolint:unparam // ctx is reserved for future use when git methods accept context
func (e *Engineer) doMergePR(ctx context.Context, mr *MRInfo) ProcessResult {
	_ = ctx
	if result := validatePRMergeRequest(mr); !result.Success {
		return result
	}
	branch, target := mr.Branch, mr.Target
	provider := configuredVCSProvider(e.config.VCSProvider)
	_, _ = fmt.Fprintf(e.output, "[Engineer] Using PR merge strategy (vcs_provider=%s)\n", provider)

	if e.prProvider == nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("no PR provider configured for vcs_provider=%s", provider),
		}
	}
	pr, result := e.findPRForMerge(mr, branch)
	if !result.Success {
		return result
	}
	if result := e.checkPRApproval(pr); !result.Success {
		return result
	}
	pr, result = e.preparePRMerge(mr, branch, target)
	if !result.Success {
		return result
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] Merging PR #%d via %s API (merge)...\n", pr.Number, provider)
	mergeCommit, err := e.prProvider.MergePR(pr, "merge")
	if err != nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("PR merge failed for PR #%d: %v", pr.Number, err),
		}
	}
	mergeCommit = e.syncPRMergeTarget(target, mergeCommit)
	if err := e.git.VerifyPushedCommit("origin", target, mergeCommit); err != nil {
		return ProcessResult{Success: false, Error: err.Error()}
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] Successfully merged PR #%d: %s\n", pr.Number, shortSHA(mergeCommit))
	return ProcessResult{
		Success:     true,
		MergeCommit: mergeCommit,
	}
}

func validatePRMergeRequest(mr *MRInfo) ProcessResult {
	if mr == nil {
		return ProcessResult{Success: false, Error: "merge request is missing"}
	}
	return ProcessResult{Success: true}
}

func configuredVCSProvider(provider string) string {
	if provider == "" {
		return "github"
	}
	return provider
}

func (e *Engineer) findPRForMerge(mr *MRInfo, branch string) (*git.PullRequestInfo, ProcessResult) {
	pr, err := e.prProvider.FindPullRequest(branch, mr.PRURL, mr.PRNumber, mr.CommitSHA)
	if err != nil {
		return nil, ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("failed to find PR for branch %s: %v", branch, err),
		}
	}
	if pr == nil {
		return nil, ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("no open PR found for branch %s — merge_strategy=pr requires a PR", branch),
		}
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Found PR #%d for branch %s\n", pr.Number, branch)
	if strings.TrimSpace(mr.CommitSHA) != "" {
		if err := requirePullRequestHead(pr, mr.CommitSHA); err != nil {
			return nil, ProcessResult{Success: false, Error: err.Error()}
		}
	}
	return pr, ProcessResult{Success: true}
}

func (e *Engineer) checkPRApproval(pr *git.PullRequestInfo) ProcessResult {
	requireReview := e.config.RequireReview != nil && *e.config.RequireReview
	if !requireReview {
		return ProcessResult{Success: true}
	}
	approved, err := e.prProvider.IsPRApproved(pr)
	if err != nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("failed to check PR #%d approval status: %v", pr.Number, err),
		}
	}
	if !approved {
		_, _ = fmt.Fprintf(e.output, "[Engineer] PR #%d awaiting human approval — deferring merge\n", pr.Number)
		return ProcessResult{
			Success:       false,
			NeedsApproval: true,
			Error:         fmt.Sprintf("PR #%d requires approving review before merge", pr.Number),
		}
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] PR #%d has approving review\n", pr.Number)
	return ProcessResult{Success: true}
}

func (e *Engineer) refreshPRForMerge(mr *MRInfo, branch string) (*git.PullRequestInfo, ProcessResult) {
	pr, err := e.prProvider.FindPullRequest(branch, mr.PRURL, mr.PRNumber, mr.CommitSHA)
	if err != nil {
		return nil, ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("failed to refresh PR for branch %s: %v", branch, err),
		}
	}
	if pr == nil {
		return nil, ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("no open PR found for branch %s — merge_strategy=pr requires a PR", branch),
		}
	}
	if err := requirePullRequestHead(pr, mr.CommitSHA); err != nil {
		return nil, ProcessResult{Success: false, Error: err.Error()}
	}
	return pr, ProcessResult{Success: true}
}

func (e *Engineer) preparePRMerge(mr *MRInfo, branch, target string) (*git.PullRequestInfo, ProcessResult) {
	if eligibility := e.recheckMRStillMergeable(mr, target); !eligibility.Success {
		return nil, eligibility
	}
	if err := e.ensureMRInfoCommitSHA(mr); err != nil {
		return nil, ProcessResult{Success: false, Error: err.Error()}
	}
	return e.refreshPRForMerge(mr, branch)
}

func (e *Engineer) syncPRMergeTarget(target, mergeCommit string) string {
	if err := e.git.Checkout(target); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to checkout %s after PR merge: %v\n", target, err)
	} else if err := e.git.Pull("origin", target); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to pull %s after PR merge: %v\n", target, err)
	}
	if mergeCommit == "" {
		if sha, err := e.git.Rev("HEAD"); err == nil {
			return sha
		}
	}
	return mergeCommit
}

func mergeIneligibleResult(format string, args ...interface{}) ProcessResult {
	return ProcessResult{
		Success: false,
		NoMerge: true,
		Error:   fmt.Sprintf(format, args...),
	}
}

type mergeRequestRecheckFields struct {
	commitSHA   string
	sourceIssue string
}

func (e *Engineer) recheckMRStillMergeable(mr *MRInfo, target string) ProcessResult {
	if mr == nil {
		return ProcessResult{Success: false, Error: "merge request is missing"}
	}

	sourceIssue := strings.TrimSpace(mr.SourceIssue)
	if sourceIssue == "" {
		if e.isSyntheticMergeMechanicsMR(mr) {
			return ProcessResult{Success: true}
		}
		return e.rejectMRBeforeMerge(mr, "MR has missing source_issue")
	}

	fields, result := e.loadMRRecheckFields(mr, target, sourceIssue)
	if !result.Success {
		return result
	}
	if eligibility := e.recheckMRSourceStillMergeable(mr, fields.sourceIssue); !eligibility.Success {
		return eligibility
	}
	return e.validateMRRecheckCommit(mr, fields.commitSHA)
}

func (e *Engineer) loadMRRecheckFields(mr *MRInfo, target, sourceIssue string) (mergeRequestRecheckFields, ProcessResult) {
	mrID := strings.TrimSpace(mr.ID)
	if mrID == "" || e.isSyntheticMergeMechanicsMR(mr) {
		return mergeRequestRecheckFields{sourceIssue: sourceIssue}, ProcessResult{Success: true}
	}

	mrIssue, result := e.loadMergeRequestForRecheck(mr, mrID)
	if !result.Success {
		return mergeRequestRecheckFields{}, result
	}
	fields := beads.ParseMRFields(mrIssue)
	if fields == nil {
		return mergeRequestRecheckFields{}, e.rejectMRBeforeMerge(mr, "MR has missing merge-request fields")
	}
	if result := e.validateMRRecheckFields(mr, target, sourceIssue, mrID, fields); !result.Success {
		return mergeRequestRecheckFields{}, result
	}
	return mergeRequestRecheckFields{
		commitSHA:   strings.TrimSpace(fields.CommitSHA),
		sourceIssue: fields.SourceIssue,
	}, ProcessResult{Success: true}
}

func (e *Engineer) loadMergeRequestForRecheck(mr *MRInfo, mrID string) (*beads.Issue, ProcessResult) {
	mrIssue, err := e.beads.Show(mrID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, mergeIneligibleResult("MR %s no longer exists", mrID)
		}
		return nil, ProcessResult{Success: false, Error: fmt.Sprintf("pre-push recheck MR %s: %v", mrID, err)}
	}
	if mrIssue == nil {
		return nil, mergeIneligibleResult("MR %s no longer exists", mrID)
	}
	if beads.IssueStatus(strings.TrimSpace(mrIssue.Status)) != beads.StatusOpen {
		return nil, mergeIneligibleResult("MR %s status is %s", mrID, mrIssue.Status)
	}
	if beads.HasLabel(mrIssue, "gt:owned-direct") {
		return nil, e.rejectMRBeforeMerge(mr, "MR is owned-direct")
	}
	return mrIssue, ProcessResult{Success: true}
}

func (e *Engineer) validateMRRecheckFields(mr *MRInfo, target, sourceIssue, mrID string, fields *beads.MRFields) ProcessResult {
	if closeReason := strings.TrimSpace(fields.CloseReason); closeReason != "" {
		return e.validateMRCloseReason(mr, mrID, closeReason)
	}
	if result := validateMRBranchAndTarget(mr, target, fields); !result.Success {
		return e.rejectMRBeforeMerge(mr, result.Error)
	}
	if result := validateMRRigAndSource(e, sourceIssue, fields); !result.Success {
		return e.rejectMRBeforeMerge(mr, result.Error)
	}
	return ProcessResult{Success: true}
}

func validateMRBranchAndTarget(mr *MRInfo, target string, fields *beads.MRFields) ProcessResult {
	if fields.Branch != "" && mr.Branch != "" && fields.Branch != mr.Branch {
		return mergeIneligibleResult("MR branch changed from %s to %s", mr.Branch, fields.Branch)
	}
	if strings.TrimSpace(fields.Target) == "" {
		return mergeIneligibleResult("MR has missing target")
	}
	if fields.Target != target {
		return mergeIneligibleResult("MR target changed from %s to %s", target, fields.Target)
	}
	return ProcessResult{Success: true}
}

func validateMRRigAndSource(e *Engineer, sourceIssue string, fields *beads.MRFields) ProcessResult {
	if fields.Rig != "" && !strings.EqualFold(fields.Rig, e.rig.Name) {
		return mergeIneligibleResult("MR belongs to rig %s", fields.Rig)
	}
	if strings.TrimSpace(fields.SourceIssue) == "" {
		return mergeIneligibleResult("MR has missing source_issue")
	}
	if fields.SourceIssue != sourceIssue {
		return mergeIneligibleResult("MR source_issue changed from %s to %s", sourceIssue, fields.SourceIssue)
	}
	return ProcessResult{Success: true}
}

func (e *Engineer) validateMRCloseReason(mr *MRInfo, mrID, closeReason string) ProcessResult {
	if strings.EqualFold(closeReason, string(CloseReasonMerged)) {
		if err := e.closeMRWithReason(mr, string(CloseReasonMerged)); err != nil {
			return ProcessResult{Success: false, Error: fmt.Sprintf("failed to close already-merged MR %s: %v", mrID, err)}
		}
		return mergeIneligibleResult("MR close_reason is %s", closeReason)
	}
	return e.rejectMRBeforeMerge(mr, fmt.Sprintf("MR close_reason is %s", closeReason))
}

func (e *Engineer) validateMRRecheckCommit(mr *MRInfo, fieldCommit string) ProcessResult {
	if strings.TrimSpace(mr.ID) == "" || e.isSyntheticMergeMechanicsMR(mr) {
		return ProcessResult{Success: true}
	}
	if fieldCommit == "" {
		return e.rejectMRBeforeMerge(mr, "MR has missing commit_sha")
	}
	if mr.CommitSHA == "" {
		mr.CommitSHA = fieldCommit
		return ProcessResult{Success: true}
	}
	if fieldCommit != strings.TrimSpace(mr.CommitSHA) {
		return e.rejectMRBeforeMerge(mr, fmt.Sprintf("MR commit_sha changed from %s to %s", shortSHA(mr.CommitSHA), shortSHA(fieldCommit)))
	}
	return ProcessResult{Success: true}
}

func (e *Engineer) isSyntheticMergeMechanicsMR(mr *MRInfo) bool {
	return e.testAllowSyntheticMRs && mr != nil && strings.HasPrefix(strings.TrimSpace(mr.ID), "mr-") && strings.TrimSpace(mr.SourceIssue) == ""
}

func (e *Engineer) rejectMRBeforeMerge(mr *MRInfo, reason string) ProcessResult {
	if err := e.closeIneligibleMR(mr, reason); err != nil {
		mrID := "<missing>"
		if mr != nil && mr.ID != "" {
			mrID = mr.ID
		}
		return ProcessResult{Success: false, Error: fmt.Sprintf("failed to close ineligible MR %s: %v", mrID, err)}
	}
	return mergeIneligibleResult("%s", reason)
}

func (e *Engineer) recheckMRSourceStillMergeable(mr *MRInfo, sourceIssue string) ProcessResult {
	issue, err := e.beads.Show(sourceIssue)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return e.rejectMRBeforeMerge(mr, fmt.Sprintf("source_issue %s is missing", sourceIssue))
		}
		return ProcessResult{Success: false, Error: fmt.Sprintf("pre-push recheck source_issue %s: %v", sourceIssue, err)}
	}
	return e.validateSourceIssue(mr, sourceIssue, issue)
}

func (e *Engineer) validateSourceIssue(mr *MRInfo, sourceIssue string, issue *beads.Issue) ProcessResult {
	if issue == nil {
		return e.rejectMRBeforeMerge(mr, fmt.Sprintf("source_issue %s is missing", sourceIssue))
	}
	if beads.IssueStatus(issue.Status).IsTerminal() {
		return e.rejectMRBeforeMerge(mr, fmt.Sprintf("source_issue %s status is %s", sourceIssue, issue.Status))
	}
	if reason := beads.ConcreteWorkIssueRejectReason(issue); reason != "" {
		return e.rejectMRBeforeMerge(mr, fmt.Sprintf("source_issue %s is not concrete (%s)", sourceIssue, reason))
	}
	if unchecked := beads.HasUncheckedCriteria(issue); unchecked > 0 {
		return e.rejectMRBeforeMerge(mr, fmt.Sprintf("source_issue %s has %d unchecked acceptance criteria", sourceIssue, unchecked))
	}
	return e.validateSourceAttachments(mr, sourceIssue, issue)
}

func (e *Engineer) validateSourceAttachments(mr *MRInfo, sourceIssue string, issue *beads.Issue) ProcessResult {
	if af := beads.ParseAttachmentFields(issue); af != nil {
		switch {
		case af.NoMerge:
			return e.rejectMRBeforeMerge(mr, fmt.Sprintf("source_issue %s has no_merge=true", sourceIssue))
		case af.ReviewOnly:
			return e.rejectMRBeforeMerge(mr, fmt.Sprintf("source_issue %s has review_only=true", sourceIssue))
		case strings.EqualFold(strings.TrimSpace(af.MergeStrategy), "local"):
			return e.rejectMRBeforeMerge(mr, fmt.Sprintf("source_issue %s has merge_strategy=local", sourceIssue))
		}
	}
	return ProcessResult{Success: true}
}

func (e *Engineer) acquireMainPushSlot(ctx context.Context) (string, error) {
	slotID, err := e.mergeSlotEnsureExists()
	if err != nil {
		return "", fmt.Errorf("ensure merge slot exists: %w", err)
	}

	seq := atomic.AddUint64(&mergeSlotSeq, 1)
	holder := fmt.Sprintf("%s/refinery/push/%d-%d", e.rig.Name, time.Now().UnixNano(), seq)

	// The conflict-resolution path holds the slot with holder "rigName/refinery".
	// Both push and conflict-resolution run in the same single-threaded refinery
	// agent, so if our own rig holds the slot for conflict resolution, we can
	// safely proceed without re-acquiring — no concurrent push is possible.
	selfConflictHolder := e.rig.Name + "/refinery"

	backoff := e.mergeSlotRetryBackoff
	if backoff == 0 {
		backoff = 500 * time.Millisecond
	}

	for attempt := 0; attempt <= e.mergeSlotMaxRetries; attempt++ {
		if err := e.waitForMainPushSlotRetry(ctx, attempt, &backoff); err != nil {
			return "", err
		}

		acquired, retry, err := e.tryAcquireMainPushSlot(slotID, holder, selfConflictHolder)
		if err != nil {
			return "", err
		}
		if !retry {
			return acquired, nil
		}
	}

	return "", fmt.Errorf("merge slot %s: %w after %d retries", slotID, errMergeSlotTimeout, e.mergeSlotMaxRetries)
}

func (e *Engineer) waitForMainPushSlotRetry(ctx context.Context, attempt int, backoff *time.Duration) error {
	if attempt == 0 {
		return nil
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Merge slot held, retrying in %v (attempt %d/%d)...\n", *backoff, attempt, e.mergeSlotMaxRetries)
	select {
	case <-time.After(*backoff):
	case <-ctx.Done():
		return ctx.Err()
	}
	*backoff = min(*backoff*2, 10*time.Second)
	return nil
}

func (e *Engineer) tryAcquireMainPushSlot(slotID, holder, selfConflictHolder string) (string, bool, error) {
	status, err := e.mergeSlotAcquire(holder, false)
	if err != nil {
		return "", false, fmt.Errorf("acquire merge slot %s (%s): %w", slotID, holder, err)
	}
	if status == nil {
		return "", false, fmt.Errorf("acquire merge slot %s (%s): empty status", slotID, holder)
	}
	if status.Available || status.Holder == holder {
		return holder, false, nil
	}
	// Slot held by our own conflict-resolution path — safe to proceed.
	if status.Holder == selfConflictHolder {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Merge slot held by conflict-resolution path, proceeding\n")
		return "", false, nil // No holder to release — conflict-resolution owns the slot
	}
	return "", true, nil
}

// ValidateTestCommand validates that a test command is safe to execute.
// TestCommand comes from the rig's operator-controlled config.json, not from
// user input or PR branches. This validation provides defense-in-depth for the
// trusted infrastructure config path.
func ValidateTestCommand(cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("test command must not be empty")
	}
	return nil
}

// runTests runs the configured test command and returns the result.
func (e *Engineer) runTests(ctx context.Context) ProcessResult {
	if err := ValidateTestCommand(e.config.TestCommand); err != nil {
		return ProcessResult{
			Success: false,
			Error:   fmt.Sprintf("invalid test command: %v", err),
		}
	}

	// Run the test command with retries for flaky tests
	maxRetries := e.config.RetryFlakyTests
	if maxRetries < 1 {
		maxRetries = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Retrying tests (attempt %d/%d)...\n", attempt, maxRetries)
		}

		// Trust boundary: TestCommand comes from rig's config.json (operator-controlled
		// infrastructure config), not from PR branches or user input. Shell execution
		// is intentional for flexibility (pipes, env vars, etc).
		_, _ = fmt.Fprintf(e.output, "[Engineer] Executing test command: %s\n", e.config.TestCommand)
		cmd := exec.CommandContext(ctx, "sh", "-c", e.config.TestCommand) //nolint:gosec // G204: TestCommand is from trusted rig config
		util.SetDetachedProcessGroup(cmd)
		cmd.Dir = e.workDir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		if err == nil {
			return ProcessResult{Success: true}
		}
		lastErr = err

		// Check if context was canceled
		if ctx.Err() != nil {
			return ProcessResult{
				Success: false,
				Error:   "test run canceled",
			}
		}
	}

	return ProcessResult{
		Success:     false,
		TestsFailed: true,
		Error:       fmt.Sprintf("tests failed after %d attempts: %v", maxRetries, lastErr),
	}
}

// runGate executes a single quality gate command and returns the result.
func (e *Engineer) runGate(ctx context.Context, name string, gate *GateConfig) GateResult {
	start := time.Now()

	if strings.TrimSpace(gate.Cmd) == "" {
		return GateResult{
			Name:    name,
			Success: false,
			Error:   "gate command is empty",
			Elapsed: time.Since(start),
		}
	}

	// Apply per-gate timeout if configured
	gateCtx := ctx
	if gate.Timeout > 0 {
		var cancel context.CancelFunc
		gateCtx, cancel = context.WithTimeout(ctx, gate.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(gateCtx, "sh", "-c", gate.Cmd) //nolint:gosec // G204: Gate commands are from trusted rig config
	util.SetDetachedProcessGroup(cmd)
	cmd.Dir = e.workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	elapsed := time.Since(start)

	if err == nil {
		return GateResult{
			Name:    name,
			Success: true,
			Elapsed: elapsed,
		}
	}

	errMsg := fmt.Sprintf("%v", err)
	if gateCtx.Err() == context.DeadlineExceeded {
		errMsg = fmt.Sprintf("timed out after %v", gate.Timeout)
	}
	if stderrStr := strings.TrimSpace(stderr.String()); stderrStr != "" {
		// Cap stderr to avoid huge error messages
		if len(stderrStr) > 500 {
			stderrStr = stderrStr[:500] + "..."
		}
		errMsg = fmt.Sprintf("%s: %s", errMsg, stderrStr)
	}

	return GateResult{
		Name:    name,
		Success: false,
		Error:   errMsg,
		Elapsed: elapsed,
	}
}

// runGates executes all pre-merge gates (backward-compatible entry point).
func (e *Engineer) runGates(ctx context.Context) ProcessResult {
	return e.runGatesForPhase(ctx, GatePhasePreMerge)
}

// runGatesForPhase executes gates matching the given phase.
// Gates run in parallel if GatesParallel is true; otherwise sequentially.
// Any single gate failure means overall failure.
func (e *Engineer) runGatesForPhase(ctx context.Context, phase GatePhase) ProcessResult {
	gates := filterGatesForPhase(e.config.Gates, phase)
	if len(gates) == 0 {
		return ProcessResult{Success: true}
	}

	names := sortedGateNames(gates)

	parallel := e.config.GatesParallel && phase == GatePhasePreMerge // post-squash always sequential
	_, _ = fmt.Fprintf(e.output, "[Engineer] Running %d %s gate(s) (parallel=%v)\n", len(names), phase, parallel)

	var results []GateResult
	if parallel {
		results = e.runGatesParallel(ctx, names, gates)
	} else {
		results = e.runGatesSequential(ctx, names, gates)
	}

	failures := e.reportGateResults(results)

	if len(failures) > 0 {
		return ProcessResult{
			Success:     false,
			TestsFailed: true,
			Error:       fmt.Sprintf("quality gates failed: %s", strings.Join(failures, "; ")),
		}
	}

	_, _ = fmt.Fprintln(e.output, "[Engineer] All quality gates passed")
	return ProcessResult{Success: true}
}

func filterGatesForPhase(gates map[string]*GateConfig, phase GatePhase) map[string]*GateConfig {
	filtered := make(map[string]*GateConfig)
	for name, gate := range gates {
		gatePhase := gate.Phase
		if gatePhase == "" {
			gatePhase = GatePhasePreMerge
		}
		if gatePhase == phase {
			filtered[name] = gate
		}
	}
	return filtered
}

func sortedGateNames(gates map[string]*GateConfig) []string {
	names := make([]string, 0, len(gates))
	for name := range gates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (e *Engineer) runGatesParallel(ctx context.Context, names []string, gates map[string]*GateConfig) []GateResult {
	results := make([]GateResult, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(idx int, gateName string) {
			defer wg.Done()
			_, _ = fmt.Fprintf(e.output, "[Engineer] Gate %q: starting (%s)\n", gateName, gates[gateName].Cmd)
			results[idx] = e.runGate(ctx, gateName, gates[gateName])
		}(i, name)
	}
	wg.Wait()
	return results
}

func (e *Engineer) runGatesSequential(ctx context.Context, names []string, gates map[string]*GateConfig) []GateResult {
	var results []GateResult
	for _, name := range names {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Gate %q: starting (%s)\n", name, gates[name].Cmd)
		result := e.runGate(ctx, name, gates[name])
		results = append(results, result)
		if !result.Success {
			// Sequential mode: stop on first failure
			break
		}
	}
	return results
}

func (e *Engineer) reportGateResults(results []GateResult) []string {
	var failures []string
	for _, result := range results {
		if result.Success {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Gate %q: passed (%v)\n", result.Name, result.Elapsed.Truncate(time.Millisecond))
			continue
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] Gate %q: FAILED (%v) - %s\n", result.Name, result.Elapsed.Truncate(time.Millisecond), result.Error)
		failures = append(failures, fmt.Sprintf("%s: %s", result.Name, result.Error))
	}
	return failures
}

// ProcessMRInfo processes a merge request from MRInfo.
func (e *Engineer) ProcessMRInfo(ctx context.Context, mr *MRInfo) ProcessResult {
	// MR fields are directly on the struct
	_, _ = fmt.Fprintln(e.output, "[Engineer] Processing MR:")
	_, _ = fmt.Fprintf(e.output, "  Branch: %s\n", mr.Branch)
	_, _ = fmt.Fprintf(e.output, "  Target: %s\n", mr.Target)
	_, _ = fmt.Fprintf(e.output, "  Worker: %s\n", mr.Worker)
	_, _ = fmt.Fprintf(e.output, "  Source: %s\n", mr.SourceIssue)

	// Phase 3: Check pre-verification fast-path.
	// If the polecat already rebased onto the target and ran gates, and the target
	// hasn't moved since, we can skip running gates entirely (~5s merge).
	skipGates := false
	if mr.PreVerified && mr.PreVerifiedBase != "" {
		_, _ = fmt.Fprintf(e.output, "  Pre-verified: yes (base=%s)\n", mr.PreVerifiedBase[:min(8, len(mr.PreVerifiedBase))])
		// Check if target HEAD still matches the verified base
		targetHead, err := e.git.Rev("origin/" + mr.Target)
		if err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not resolve origin/%s HEAD: %v (falling through to normal gates)\n", mr.Target, err)
		} else if targetHead == mr.PreVerifiedBase {
			_, _ = fmt.Fprintln(e.output, "[Engineer] Pre-verification valid — target unchanged, skipping gates (fast-path)")
			skipGates = true
		} else {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Pre-verification stale — target moved (%s → %s), running gates normally\n",
				mr.PreVerifiedBase[:min(8, len(mr.PreVerifiedBase))], targetHead[:min(8, len(targetHead))])
		}
	}

	// Use the shared merge logic
	return e.doMerge(ctx, mr, skipGates)
}

// HandleMRInfoSuccess handles a successful merge from MRInfo.
func (e *Engineer) HandleMRInfoSuccess(mr *MRInfo, result ProcessResult) bool {
	workBeadID := resolveMergedWorkBead(e.beads.ForAgentBead(), mergedWorkBeadCloseRequest{
		MRID:        mr.ID,
		Branch:      mr.Branch,
		SourceIssue: mr.SourceIssue,
		AgentBead:   mr.AgentBead,
	})

	e.releaseMergeSlotAfterSuccess()
	if !e.completeMRInfoMerge(mr, result) {
		return false
	}

	e.finishMergedWork(mr, result, workBeadID)
	e.deleteMergedSourceBranch(mr)

	// 3. Check and auto-close completed convoys
	// After closing a source issue, its parent convoy may now be complete.
	// Run convoy check to auto-close and notify subscribers.
	e.postMergeConvoyCheck(mr)

	e.nudgeMayorAfterMerge(mr)

	// 5. Log success
	_, _ = fmt.Fprintf(e.output, "[Engineer] ✓ Merged: %s (commit: %s)\n", mr.ID, result.MergeCommit)
	return true
}

func (e *Engineer) releaseMergeSlotAfterSuccess() {
	// Best-effort: slot release failures are always non-fatal.
	// Slot may not have been held (optional acquisition) or may have expired.
	holder := e.rig.Name + "/refinery"
	if err := e.mergeSlotRelease(holder); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Note: merge slot release: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Released merge slot\n")
}

func (e *Engineer) completeMRInfoMerge(mr *MRInfo, result ProcessResult) bool {
	if err := e.verifyMRInfoPostMergeProof(mr); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Post-merge proof failed for %s: %v\n", mr.ID, err)
		return false
	}
	if mr.ID == "" || e.isSyntheticMergeMechanicsMR(mr) {
		return true
	}
	if err := e.closeMRWithReason(mr, string(CloseReasonMerged), result.MergeCommit); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Post-merge cleanup failed for %s: %v\n", mr.ID, err)
		return false
	}
	return true
}

func (e *Engineer) finishMergedWork(mr *MRInfo, result ProcessResult, workBeadID string) {
	closeMergedWorkBead(e.beads, nil, e.output, mergedWorkBeadCloseRequest{
		MRID:        mr.ID,
		Target:      mr.Target,
		SourceIssue: workBeadID,
		MergeCommit: result.MergeCommit,
	})
	e.closeSupersededConflictArtifacts(mr)
}

func (e *Engineer) deleteMergedSourceBranch(mr *MRInfo) {
	isPolecat := strings.HasPrefix(mr.Branch, "polecat/")
	if mr.Branch == "" || (!e.config.DeleteMergedBranches && !isPolecat) {
		return
	}
	expectedHead := strings.TrimSpace(mr.CommitSHA)
	if !e.deleteMergedRemoteBranch(mr, isPolecat, expectedHead) {
		return
	}
	if err := e.deleteLocalBranchIfAt(mr.Branch, expectedHead); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to delete local branch %s: %v\n", mr.Branch, err)
		return
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Deleted local branch: %s\n", mr.Branch)
}

func (e *Engineer) deleteMergedRemoteBranch(mr *MRInfo, isPolecat bool, expectedHead string) bool {
	if !isPolecat {
		return true
	}
	// Non-polecat branches may belong to contributor forks with open upstream PRs;
	// deleting them from origin causes GitHub to auto-close those PRs. (GH#2669)
	if e.git.HasOpenPullRequest(git.PullRequestRef{URL: mr.PRURL, Number: mr.PRNumber, Branch: mr.Branch, HeadSHA: expectedHead}) {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Skipping remote branch delete for %s: open PR exists (gas-fk4)\n", mr.Branch)
		return true
	}
	if err := e.git.DeleteRemoteBranchIfAt("origin", mr.Branch, expectedHead); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to delete remote branch %s: %v\n", mr.Branch, err)
		return false
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Deleted remote branch: %s\n", mr.Branch)
	return true
}

func (e *Engineer) nudgeMayorAfterMerge(mr *MRInfo) {
	// Use nudge (not mail) to avoid permanent Dolt commits for routine signals (GH#2434).
	nudgeMsg := fmt.Sprintf("MERGED: %s issue=%s branch=%s", mr.ID, mr.SourceIssue, mr.Branch)
	nudgeCmd := exec.Command("gt", "nudge", "mayor/", nudgeMsg)
	util.SetDetachedProcessGroup(nudgeCmd)
	nudgeCmd.Dir = e.workDir
	if err := nudgeCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge mayor about merge: %v\n", err)
	}
}

func (e *Engineer) ensureMRInfoCommitSHA(mr *MRInfo) error {
	if mr == nil {
		return fmt.Errorf("merge request is missing")
	}
	if strings.TrimSpace(mr.CommitSHA) != "" {
		return nil
	}
	if !e.isSyntheticMergeMechanicsMR(mr) {
		return fmt.Errorf("missing submitted commit_sha")
	}
	if e.git == nil {
		return fmt.Errorf("missing submitted commit_sha and git client is missing")
	}
	branch := strings.TrimSpace(mr.Branch)
	if branch == "" {
		return fmt.Errorf("missing submitted commit_sha and source branch")
	}
	sha, err := e.git.Rev(branch)
	if err != nil {
		return fmt.Errorf("resolve submitted head for %s: %w", branch, err)
	}
	mr.CommitSHA = strings.TrimSpace(sha)
	return nil
}

func (e *Engineer) submittedBranchHead(mr *MRInfo) (string, error) {
	if err := e.ensureMRInfoCommitSHA(mr); err != nil {
		return "", err
	}
	if e.git == nil {
		return "", fmt.Errorf("git client is missing")
	}
	branch := strings.TrimSpace(mr.Branch)
	if branch == "" {
		return "", fmt.Errorf("missing source branch")
	}
	commit := strings.TrimSpace(mr.CommitSHA)
	localHead, err := e.git.Rev("refs/heads/" + branch + "^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve source branch %s: %w", branch, err)
	}
	localHead = strings.TrimSpace(localHead)
	if localHead != commit {
		return "", fmt.Errorf("source branch %s changed from submitted head %s to %s", branch, shortSHA(commit), shortSHA(localHead))
	}
	return commit, nil
}

func (e *Engineer) deleteLocalBranchIfAt(branch, expectedHead string) error {
	branch = strings.TrimSpace(branch)
	expectedHead = strings.TrimSpace(expectedHead)
	if branch == "" {
		return fmt.Errorf("missing source branch")
	}
	if expectedHead == "" {
		return fmt.Errorf("missing submitted commit_sha")
	}
	localHead, err := e.git.Rev("refs/heads/" + branch + "^{commit}")
	if err != nil {
		return fmt.Errorf("resolve local branch head: %w", err)
	}
	if strings.TrimSpace(localHead) != expectedHead {
		return fmt.Errorf("local branch head changed from submitted %s to %s", shortSHA(expectedHead), shortSHA(localHead))
	}
	return e.git.DeleteBranch(branch, false)
}

func requirePullRequestHead(pr *git.PullRequestInfo, expectedHead string) error {
	expectedHead = strings.TrimSpace(expectedHead)
	if expectedHead == "" {
		return fmt.Errorf("missing submitted commit_sha")
	}
	if pr == nil {
		return fmt.Errorf("pull request is missing")
	}
	actualHead := strings.TrimSpace(pr.HeadSHA)
	if actualHead == "" {
		return fmt.Errorf("PR #%d head SHA is missing", pr.Number)
	}
	if actualHead != expectedHead {
		return fmt.Errorf("PR #%d head changed from submitted %s to %s", pr.Number, shortSHA(expectedHead), shortSHA(actualHead))
	}
	return nil
}

func (e *Engineer) verifyMRInfoPostMergeProof(mr *MRInfo) error {
	if mr == nil {
		return fmt.Errorf("merge request is missing")
	}
	if e.git == nil {
		return fmt.Errorf("git client is missing")
	}
	target := strings.TrimSpace(mr.Target)
	if target == "" {
		return fmt.Errorf("missing target branch")
	}
	if source := strings.TrimSpace(mr.Branch); source != "" && source == target {
		return fmt.Errorf("source branch %s matches target branch", source)
	}
	commit := strings.TrimSpace(mr.CommitSHA)
	if commit == "" {
		return fmt.Errorf("missing submitted commit_sha")
	}
	if err := e.git.VerifyPushedCommitReachableFromPushTarget("origin", target, commit); err != nil {
		return fmt.Errorf("target %s does not contain submitted head %s: %w", target, commit, err)
	}
	return nil
}

// HandleMRInfoFailure handles a failed merge from MRInfo.
// For conflicts, creates a resolution task and blocks the MR until resolved.
// For slot timeouts, the MR stays in queue for automatic retry without notifying polecats.
// This enables non-blocking delegation: the queue continues to the next MR.
func (e *Engineer) HandleMRInfoFailure(mr *MRInfo, result ProcessResult) {
	if e.handleEarlyMRFailure(mr, result) {
		return
	}

	failureType := mrFailureType(result)
	e.nudgeMergeFailure(mr, result, failureType)
	e.handleConflictFailure(mr, result)
	e.logMRFailure(mr, result)
}

func (e *Engineer) handleEarlyMRFailure(mr *MRInfo, result ProcessResult) bool {
	if result.SlotTimeout {
		_, _ = fmt.Fprintf(e.output, "[Engineer] ✗ Slot timeout: %s - %s\n", mr.ID, result.Error)
		_, _ = fmt.Fprintln(e.output, "[Engineer] MR remains in queue for automatic retry (slot contention)")
		return true
	}
	if result.NoMerge {
		reason := strings.TrimSpace(result.Error)
		if reason == "" {
			reason = "merge request is not merge-eligible"
		}
		_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: %s, dequeued\n", mr.ID, reason)
		if closeErr := e.closeIneligibleMR(mr, reason); closeErr != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to close ineligible MR %s: %v\n", mr.ID, closeErr)
		}
		return true
	}
	if result.NeedsApproval {
		_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: PR awaiting human approval, will retry next poll\n", mr.ID)
		return true
	}
	if result.BranchNotFound {
		e.nudgeMayorForMissingBranch(mr)
		return true
	}
	return false
}

func (e *Engineer) nudgeMayorForMissingBranch(mr *MRInfo) {
	_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s: branch %s not found on remote — escalating to mayor (possible work loss)\n", mr.ID, mr.Branch)
	mayorMsg := fmt.Sprintf("BRANCH_MISSING: MR %s branch=%s issue=%s worker=%s — branch not on origin, work may be lost; re-dispatch if needed",
		mr.ID, mr.Branch, mr.SourceIssue, mr.Worker)
	mayorCmd := exec.Command("gt", "nudge", "mayor/", mayorMsg)
	mayorCmd.Dir = e.workDir
	if err := mayorCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge mayor about missing branch: %v\n", err)
	}
}

func mrFailureType(result ProcessResult) string {
	if result.Conflict {
		return "conflict"
	}
	if result.TestsFailed {
		return "tests"
	}
	return "build"
}

func (e *Engineer) nudgeMergeFailure(mr *MRInfo, result ProcessResult, failureType string) {
	polecatName := strings.TrimPrefix(mr.Worker, "polecats/")
	nudgeTarget := fmt.Sprintf("%s/%s", e.rig.Name, polecatName)
	nudgeMsg := fmt.Sprintf("MERGE_FAILED: branch=%s issue=%s type=%s error=%s — fix and resubmit with 'gt done'",
		mr.Branch, mr.SourceIssue, failureType, result.Error)
	nudgeCmd := exec.Command("gt", "nudge", nudgeTarget, nudgeMsg)
	util.SetDetachedProcessGroup(nudgeCmd)
	nudgeCmd.Dir = e.workDir
	if err := nudgeCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge %s about merge failure: %v\n", polecatName, err)
	} else {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Nudged %s about merge failure (%s)\n", polecatName, failureType)
	}

	// Nudge mayor about merge failure so dispatcher can unblock or reassign dependent work.
	mayorMsg := fmt.Sprintf("MERGE_FAILED: %s issue=%s branch=%s type=%s", mr.ID, mr.SourceIssue, mr.Branch, failureType)
	mayorCmd := exec.Command("gt", "nudge", "mayor/", mayorMsg)
	util.SetDetachedProcessGroup(mayorCmd)
	mayorCmd.Dir = e.workDir
	if err := mayorCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge mayor about merge failure: %v\n", err)
	}
}

func (e *Engineer) handleConflictFailure(mr *MRInfo, result ProcessResult) {
	if !result.Conflict {
		return
	}
	retryCount := mr.RetryCount + 1
	conflictSHA, revErr := e.git.Rev("origin/" + mr.Target)
	if revErr != nil {
		conflictSHA = "unknown-sha"
	}
	taskID, err := e.createConflictResolutionTaskForMR(mr, result)
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to create conflict resolution task: %v\n", err)
		return
	}
	if taskID == "" {
		return
	}
	e.blockMROnConflictTask(mr, taskID, retryCount, conflictSHA)
}

func (e *Engineer) blockMROnConflictTask(mr *MRInfo, taskID string, retryCount int, conflictSHA string) {
	// Block the MR on the conflict resolution task; closing the task re-enters the queue.
	if err := e.beads.AddDependency(mr.ID, taskID); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to block MR on task: %v\n", err)
		return
	}
	if err := e.recordConflictTaskOnMR(mr, taskID, retryCount, conflictSHA); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to record conflict task on MR %s: %v\n", mr.ID, err)
	} else {
		mr.ConflictTaskID = taskID
		mr.RetryCount = retryCount
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s blocked on conflict task %s (non-blocking delegation)\n", mr.ID, taskID)
}

func (e *Engineer) logMRFailure(mr *MRInfo, result ProcessResult) {

	// Log the failure - MR stays in queue but may be blocked
	_, _ = fmt.Fprintf(e.output, "[Engineer] ✗ Failed: %s - %s\n", mr.ID, result.Error)
	if mr.BlockedBy != "" {
		_, _ = fmt.Fprintln(e.output, "[Engineer] MR blocked pending conflict resolution - queue continues to next MR")
	} else {
		_, _ = fmt.Fprintln(e.output, "[Engineer] MR remains in queue for retry")
	}
}

func (e *Engineer) closeIneligibleMR(mr *MRInfo, reason string) error {
	return e.closeMRWithReason(mr, "rejected: "+reason)
}

func (e *Engineer) closeMRWithReason(mr *MRInfo, closeReason string, mergeCommit ...string) error {
	if mr == nil || strings.TrimSpace(mr.ID) == "" {
		return nil
	}
	var commit string
	if len(mergeCommit) > 0 {
		commit = mergeCommit[0]
	}
	var expected *MergeRequest
	if normalizedMRCloseReason(closeReason) == string(CloseReasonMerged) {
		expected = mergeRequestFromMRInfo(mr)
	}
	result, err := closeTerminalMR(e.beads, mr.ID, terminalMRCloseOptions{
		Reason:        closeReason,
		MergeCommit:   commit,
		AgentBeadHint: mr.AgentBead,
		MissingOK:     true,
		ExpectedMR:    expected,
	})
	if err != nil {
		return err
	}
	if result.Closed {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Closed MR bead: %s (%s)\n", mr.ID, closeReason)
	}
	if result.AgentActiveMRClearErr != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to clear agent bead %s active_mr: %v\n", result.AgentBead, result.AgentActiveMRClearErr)
	}
	return nil
}

func mergeRequestFromMRInfo(mr *MRInfo) *MergeRequest {
	if mr == nil {
		return nil
	}
	return &MergeRequest{
		ID:           mr.ID,
		Branch:       mr.Branch,
		Worker:       mr.Worker,
		AgentBead:    mr.AgentBead,
		IssueID:      mr.SourceIssue,
		TargetBranch: mr.Target,
		CommitSHA:    mr.CommitSHA,
		PRURL:        mr.PRURL,
		PRNumber:     mr.PRNumber,
	}
}

func normalizedMRCloseReason(closeReason string) string {
	closeReason = strings.TrimSpace(closeReason)
	lower := strings.ToLower(closeReason)
	if strings.HasPrefix(lower, "rejected:") {
		return string(CloseReasonRejected)
	}
	if strings.HasPrefix(lower, "superseded") {
		return string(CloseReasonSuperseded)
	}
	if strings.HasPrefix(lower, "conflict") {
		return string(CloseReasonConflict)
	}
	return closeReason
}

// createConflictResolutionTaskForMR creates a dispatchable task for resolving merge conflicts.
// This task will be picked up by bd ready and can be slung to a fresh polecat (spawned on demand).
// Returns the created task's ID for blocking the MR until resolution.
//
// Task format:
//
//	Title: Resolve merge conflicts: <original-issue-title>
//	Type: task
//	Priority: inherit from original (ZFC: agent decides boost strategy)
//	Parent: original MR bead
//	Description: metadata including branch, conflict SHA, etc.
//
// Merge Slot Integration:
// Before creating a conflict resolution task, we acquire the merge-slot for this rig.
// This serializes conflict resolution - only one polecat can resolve conflicts at a time.
// If the slot is already held, we skip creating the task and let the MR stay in queue.
// When the current resolution completes and merges, the slot is released.
func (e *Engineer) createConflictResolutionTaskForMR(mr *MRInfo, _ ProcessResult) (string, error) { // result unused but kept for future merge diagnostics
	_, slotHolder, deferred := e.acquireConflictResolutionSlot(mr)
	if deferred {
		return "", nil
	}

	mainSHA := e.conflictResolutionMainSHA(mr)
	originalTitle := e.conflictResolutionOriginalTitle(mr)
	retryCount := mr.RetryCount + 1
	description := buildConflictResolutionDescription(mr, mainSHA, retryCount)
	task, err := e.beads.Create(beads.CreateOptions{
		Title:       fmt.Sprintf("Resolve merge conflicts: %s", originalTitle),
		Labels:      []string{"gt:task"},
		Priority:    mr.Priority,
		Description: description,
		Actor:       e.rig.Name + "/refinery",
		Rig:         e.rig.Name, // Ensure task lands in the rig's database (gt-7y7)
	})
	if err != nil {
		e.releaseConflictResolutionSlot(slotHolder)
		return "", fmt.Errorf("creating conflict resolution task: %w", err)
	}

	// gt-gpy: Validate task bead landed in the rig's database (warning only).
	townRoot := filepath.Dir(e.rig.Path)
	if prefixErr := beads.ValidateRigPrefix(townRoot, e.rig.Name, task.ID); prefixErr != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] WARNING: conflict task prefix mismatch: %v\n", prefixErr)
	}

	// The conflict task's ID is returned so the MR can be blocked on it.
	// When the task closes, the MR unblocks and re-enters the ready queue.

	_, _ = fmt.Fprintf(e.output, "[Engineer] Created conflict resolution task: %s (P%d)\n", task.ID, task.Priority)

	return task.ID, nil
}

func (e *Engineer) acquireConflictResolutionSlot(mr *MRInfo) (string, string, bool) {
	slotID, err := e.mergeSlotEnsureExists()
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not ensure merge slot: %v\n", err)
		return "", "", false
	}
	holder := e.rig.Name + "/refinery"
	status, err := e.mergeSlotAcquire(holder, false)
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not acquire merge slot: %v\n", err)
		return slotID, "", false
	}
	if status == nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: merge slot returned nil status\n")
		return slotID, "", false
	}
	if !status.Available && status.Holder != "" && status.Holder != holder {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Merge slot held by %s - deferring conflict resolution\n", status.Holder)
		_, _ = fmt.Fprintf(e.output, "[Engineer] MR %s will retry after current resolution completes\n", mr.ID)
		return slotID, "", true
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Acquired merge slot: %s\n", slotID)
	return slotID, holder, false
}

func (e *Engineer) releaseConflictResolutionSlot(slotHolder string) {
	if slotHolder != "" {
		_ = e.mergeSlotRelease(slotHolder)
	}
}

func (e *Engineer) conflictResolutionMainSHA(mr *MRInfo) string {
	mainSHA, err := e.git.Rev("origin/" + mr.Target)
	if err != nil {
		return "unknown-sha"
	}
	return mainSHA
}

func (e *Engineer) conflictResolutionOriginalTitle(mr *MRInfo) string {
	if mr.SourceIssue != "" {
		if sourceIssue, err := e.beads.Show(mr.SourceIssue); err == nil && sourceIssue != nil {
			return sourceIssue.Title
		}
	}
	return mr.SourceIssue
}

func buildConflictResolutionDescription(mr *MRInfo, mainSHA string, retryCount int) string {
	return fmt.Sprintf(`Resolve merge conflicts for branch %s

## Metadata
- Original MR: %s
- Branch: %s
- Conflict with: %s@%s
- Original issue: %s
- Retry count: %d

## Instructions
1. Check out the branch: git checkout %s
2. Merge target without rewriting branch history: git merge --no-ff origin/%s
3. Resolve conflicts in your editor
4. Complete the merge: git add . && git commit
5. Push the resolved branch: git push origin %s
6. Close this task: bd close <this-task-id>

The Refinery will automatically retry the merge after you push.`,
		mr.Branch,
		mr.ID,
		mr.Branch,
		mr.Target, shortSHA(mainSHA),
		mr.SourceIssue,
		retryCount,
		mr.Branch,
		mr.Target,
		mr.Branch,
	)
}

func (e *Engineer) recordConflictTaskOnMR(mr *MRInfo, taskID string, retryCount int, conflictSHA string) error {
	mrBead, err := e.beads.Show(mr.ID)
	if err != nil {
		return err
	}
	mrFields := beads.ParseMRFields(mrBead)
	if mrFields == nil {
		mrFields = &beads.MRFields{}
	}
	mrFields.ConflictTaskID = taskID
	mrFields.RetryCount = retryCount
	mrFields.LastConflictSHA = conflictSHA
	newDesc := beads.SetMRFields(mrBead, mrFields)
	return e.beads.Update(mr.ID, beads.UpdateOptions{Description: &newDesc})
}

// closeSupersededConflictArtifacts closes conflict-resolution tasks made moot
// by a successful land of the source issue (hq-jnap). Two cases:
//  1. The merged MR's own conflict task is still open — the conflict was
//     resolved out-of-band (force-push) without `bd close`, so the task rots.
//  2. Another open MR carries the same source issue (a re-land) — its conflict
//     task is now pointless because the content is on the target branch.
//
// Superseded sibling MRs are closed only when their conflict task verifies it
// belongs to that MR/source issue; this avoids unblocking stale duplicate MRs.
// All operations are best-effort; failures are logged and don't affect the merge.
func (e *Engineer) closeSupersededConflictArtifacts(merged *MRInfo) {
	e.closeConflictTaskIfOpen(conflictTaskIDForMR(merged), merged.ID, merged.ID, merged.SourceIssue)

	if merged.SourceIssue == "" {
		return
	}
	all, err := e.ListAllOpenMRs()
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: conflict-artifact sweep skipped (list MRs): %v\n", err)
		return
	}
	for _, other := range all {
		if other.ID == merged.ID || other.SourceIssue != merged.SourceIssue {
			continue
		}
		if !e.closeConflictTaskIfOpen(conflictTaskIDForMR(other), other.ID, merged.ID, merged.SourceIssue) {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Note: open MR %s shares source issue %s just merged via %s, but had no verified conflict task to close\n",
				other.ID, merged.SourceIssue, merged.ID)
			continue
		}
		reason := fmt.Sprintf("superseded by %s", merged.ID)
		if err := e.closeMRWithReason(other, reason); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to close superseded MR %s: %v\n", other.ID, err)
		} else {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Closed superseded MR %s: %s\n", other.ID, reason)
		}
	}
}

func conflictTaskIDForMR(mr *MRInfo) string {
	if mr == nil {
		return ""
	}
	if mr.ConflictTaskID != "" {
		return mr.ConflictTaskID
	}
	return mr.BlockedBy
}

// closeConflictTaskIfOpen closes a conflict-resolution task if it is still open.
func (e *Engineer) closeConflictTaskIfOpen(taskID, taskMRID, landedMRID, sourceIssue string) bool {
	if taskID == "" {
		return false
	}
	task, err := e.beads.Show(taskID)
	if err != nil || task == nil {
		return false
	}
	if !isConflictTaskForMR(task, taskMRID, sourceIssue) {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: refusing to close unverified conflict task %s for MR %s\n", taskID, taskMRID)
		return false
	}
	if task.Status == string(beads.StatusClosed) {
		return true
	}
	reason := fmt.Sprintf("conflict moot: %s landed (MR %s)", sourceIssue, landedMRID)
	if err := e.beads.CloseWithReason(reason, taskID); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to close moot conflict task %s: %v\n", taskID, err)
		return false
	} else {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Closed moot conflict task: %s (%s)\n", taskID, reason)
	}
	return true
}

func isConflictTaskForMR(task *beads.Issue, mrID, sourceIssue string) bool {
	if task == nil || task.Description == "" || mrID == "" {
		return false
	}
	metadata := conflictTaskMetadata(task.Description)
	if metadata["Original MR"] != mrID {
		return false
	}
	return sourceIssue == "" || metadata["Original issue"] == sourceIssue
}

func conflictTaskMetadata(description string) map[string]string {
	metadata := make(map[string]string)
	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

// issueToMRInfo converts a beads issue (with parsed MR fields) into an MRInfo.
// Shared by ListReadyMRs, ListBlockedMRs, and ListAllOpenMRs.
func issueToMRInfo(issue *beads.Issue, fields *beads.MRFields) *MRInfo {
	// Parse convoy created_at if present
	var convoyCreatedAt *time.Time
	if fields.ConvoyCreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, fields.ConvoyCreatedAt); err == nil {
			convoyCreatedAt = &t
		}
	}

	// Parse issue timestamps
	var createdAt, updatedAt time.Time
	if issue.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, issue.CreatedAt); err == nil {
			createdAt = t
		}
	}
	if issue.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, issue.UpdatedAt); err == nil {
			updatedAt = t
		}
	}

	// Parse pre-verification timestamp if present
	var preVerifiedAt time.Time
	if fields.PreVerifiedAt != "" {
		if t, err := time.Parse(time.RFC3339, fields.PreVerifiedAt); err == nil {
			preVerifiedAt = t
		}
	}

	return &MRInfo{
		ID:              issue.ID,
		Branch:          fields.Branch,
		Target:          fields.Target,
		SourceIssue:     fields.SourceIssue,
		Worker:          fields.Worker,
		Rig:             fields.Rig,
		Title:           issue.Title,
		Priority:        issue.Priority,
		AgentBead:       fields.AgentBead,
		CommitSHA:       fields.CommitSHA,
		PRURL:           fields.PRURL,
		PRNumber:        fields.PRNumber,
		RetryCount:      fields.RetryCount,
		ConflictTaskID:  fields.ConflictTaskID,
		ConvoyID:        fields.ConvoyID,
		ConvoyCreatedAt: convoyCreatedAt,
		PreVerified:     fields.PreVerified,
		PreVerifiedAt:   preVerifiedAt,
		PreVerifiedBase: fields.PreVerifiedBase,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
		Assignee:        issue.Assignee,
	}
}

// firstOpenBlocker returns the first unresolved blocker ID for an issue.
func (e *Engineer) firstOpenBlocker(issue *beads.Issue) string {
	return beads.FirstUnresolvedBlockerID(issue)
}

// ListReadyMRs returns MRs that are ready for processing:
// - Not claimed by another worker (checked via assignee field)
// - Not blocked by unresolved dependencies
// Sorted by priority (highest first).
//
// Uses bd list instead of bd ready because MRs are ephemeral beads and
// bd ready filters out ephemeral issues (see gt-t5t6y). This matches the
// pattern used by ListBlockedMRs and ListAllOpenMRs.
func (e *Engineer) ListReadyMRs() ([]*MRInfo, error) {
	// Query beads for all open merge-request issues.
	// Cannot use ReadyWithType here because bd ready excludes ephemeral beads,
	// and MRs are ephemeral by design. Use List + manual blocker check instead.
	issues, err := e.beads.ListMergeRequests(beads.ListOptions{
		Status:   "open",
		Label:    "gt:merge-request",
		Priority: -1, // No priority filter
		Rig:      e.rig.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("querying beads for merge-requests: %w", err)
	}
	return e.readyMRInfos(issues), nil
}

func (e *Engineer) readyMRInfos(issues []*beads.Issue) []*MRInfo {
	var mrs []*MRInfo
	for _, issue := range issues {
		if mr := e.readyMRInfo(issue); mr != nil {
			mrs = append(mrs, mr)
		}
	}
	return mrs
}

func (e *Engineer) readyMRInfo(issue *beads.Issue) *MRInfo {
	if issue.Status != "open" || beads.HasUnresolvedBlockers(issue) {
		return nil
	}
	if beads.HasLabel(issue, "gt:owned-direct") {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Skipping MR %s: owned+direct convoy (belt-and-suspenders)\n", issue.ID)
		return nil
	}
	fields := beads.ParseMRFields(issue)
	if fields == nil || (fields.Rig != "" && !strings.EqualFold(fields.Rig, e.rig.Name)) {
		return nil
	}
	if !e.readyMRClaimEligible(issue) {
		return nil
	}
	return issueToMRInfo(issue, fields)
}

func (e *Engineer) readyMRClaimEligible(issue *beads.Issue) bool {
	if issue.Assignee == "" {
		return true
	}
	stale, parseErr := isClaimStale(issue.UpdatedAt, e.config.StaleClaimTimeout)
	if parseErr != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not parse UpdatedAt for %s: %v (treating claim as valid)\n",
			issue.ID, parseErr)
	}
	if !stale {
		return false
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Stale claim detected: %s (assignee: %s, updated: %s) — eligible for re-claim\n",
		issue.ID, issue.Assignee, issue.UpdatedAt)
	return true
}

// ListBlockedMRs returns MRs that are blocked by open tasks.
// Useful for monitoring/reporting.
//
// This queries beads for blocked merge-request issues.
func (e *Engineer) ListBlockedMRs() ([]*MRInfo, error) {
	// Query all merge-request issues (both ready and blocked)
	issues, err := e.beads.ListMergeRequests(beads.ListOptions{
		Status:   "open",
		Label:    "gt:merge-request",
		Priority: -1, // No priority filter
		Rig:      e.rig.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("querying beads for merge-requests: %w", err)
	}

	// Filter for blocked issues (those with open blockers)
	var mrs []*MRInfo
	for _, issue := range issues {
		if issue.Status != "open" {
			continue
		}

		if !beads.HasUnresolvedBlockers(issue) {
			continue
		}

		blockedBy := e.firstOpenBlocker(issue)

		fields := beads.ParseMRFields(issue)
		if fields == nil {
			continue
		}

		// Filter by rig — wisps are shared across all rigs (GH#2718).
		if fields.Rig != "" && !strings.EqualFold(fields.Rig, e.rig.Name) {
			continue
		}

		mr := issueToMRInfo(issue, fields)
		mr.BlockedBy = blockedBy
		mrs = append(mrs, mr)
	}

	return mrs, nil
}

// ListAllOpenMRs returns all open merge requests with full raw data.
// Unlike ListReadyMRs/ListBlockedMRs, this performs no filtering — it returns
// claimed, unclaimed, blocked, and unblocked MRs. It also checks branch existence
// so agents can detect orphaned MRs. Designed for agent-side queue health analysis
// (ZFC: Go transports data, agent decides what's interesting).
func (e *Engineer) ListAllOpenMRs() ([]*MRInfo, error) {
	issues, err := e.beads.ListMergeRequests(beads.ListOptions{
		Status:   "open",
		Label:    "gt:merge-request",
		Priority: -1,
		Rig:      e.rig.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("querying beads for merge-requests: %w", err)
	}

	var mrs []*MRInfo
	for _, issue := range issues {
		if issue.Status != "open" {
			continue
		}

		fields := beads.ParseMRFields(issue)
		if fields == nil {
			continue
		}

		// Filter by rig — wisps are shared across all rigs (GH#2718).
		if fields.Rig != "" && !strings.EqualFold(fields.Rig, e.rig.Name) {
			continue
		}

		mr := issueToMRInfo(issue, fields)

		// Check branch existence (local + remote tracking refs)
		mr.BranchExistsLocal, _ = e.git.BranchExists(fields.Branch)
		mr.BranchExistsRemote, _ = e.git.RemoteTrackingBranchExists("origin", fields.Branch)
		mr.BlockedBy = e.firstOpenBlocker(issue)

		mrs = append(mrs, mr)
	}

	return mrs, nil
}

// ListQueueAnomalies finds stale claims and orphaned branches in open MRs.
// This gives Witness/Refinery patrols deterministic signals for deadlock risk.
func (e *Engineer) ListQueueAnomalies(now time.Time) ([]*MRAnomaly, error) {
	issues, err := e.beads.ListMergeRequests(beads.ListOptions{
		Status:   "open",
		Label:    "gt:merge-request",
		Priority: -1,
		Rig:      e.rig.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("querying beads for merge-requests: %w", err)
	}

	// Filter by rig — wisps are shared across all rigs (GH#2718).
	filtered := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		fields := beads.ParseMRFields(issue)
		if fields != nil && fields.Rig != "" && !strings.EqualFold(fields.Rig, e.rig.Name) {
			continue
		}
		filtered = append(filtered, issue)
	}

	return detectQueueAnomalies(filtered, now, e.config.StaleClaimWarningAfter, func(branch string) (bool, bool, error) {
		localExists, err := e.git.BranchExists(branch)
		if err != nil {
			return false, false, err
		}
		remoteTrackingExists, err := e.git.RemoteTrackingBranchExists("origin", branch)
		if err != nil {
			return false, false, err
		}
		return localExists, remoteTrackingExists, nil
	}), nil
}

func detectQueueAnomalies(
	issues []*beads.Issue,
	now time.Time,
	warningAfter time.Duration,
	branchExistsFn func(branch string) (localExists bool, remoteTrackingExists bool, err error),
) []*MRAnomaly {
	var anomalies []*MRAnomaly

	for _, issue := range issues {
		anomalies = append(anomalies, queueAnomaliesForIssue(issue, now, warningAfter, branchExistsFn)...)
	}

	return anomalies
}

func queueAnomaliesForIssue(
	issue *beads.Issue,
	now time.Time,
	warningAfter time.Duration,
	branchExistsFn func(branch string) (localExists bool, remoteTrackingExists bool, err error),
) []*MRAnomaly {
	if issue == nil || issue.Status != "open" {
		return nil
	}
	fields := beads.ParseMRFields(issue)
	if fields == nil || fields.Branch == "" {
		return nil
	}

	var anomalies []*MRAnomaly
	if anomaly := staleClaimAnomaly(issue, fields.Branch, now, warningAfter); anomaly != nil {
		anomalies = append(anomalies, anomaly)
	}
	if anomaly := orphanedBranchAnomaly(issue, fields.Branch, branchExistsFn); anomaly != nil {
		anomalies = append(anomalies, anomaly)
	}
	return anomalies
}

func staleClaimAnomaly(issue *beads.Issue, branch string, now time.Time, warningAfter time.Duration) *MRAnomaly {
	if issue.Assignee == "" {
		return nil
	}
	updatedAt, err := time.Parse(time.RFC3339, issue.UpdatedAt)
	if err != nil {
		return nil
	}
	age := now.Sub(updatedAt)
	if age < warningAfter {
		return nil
	}
	return &MRAnomaly{
		ID:       issue.ID,
		Branch:   branch,
		Type:     "stale-claim",
		Assignee: issue.Assignee,
		Age:      age,
		Detail:   "MR is claimed but not progressing",
	}
}

func orphanedBranchAnomaly(
	issue *beads.Issue,
	branch string,
	branchExistsFn func(branch string) (localExists bool, remoteTrackingExists bool, err error),
) *MRAnomaly {
	localExists, remoteTrackingExists, err := branchExistsFn(branch)
	if err != nil || localExists || remoteTrackingExists {
		return nil
	}
	return &MRAnomaly{
		ID:     issue.ID,
		Branch: branch,
		Type:   "orphaned-branch",
		Detail: "MR branch is missing locally and in origin/* tracking refs",
	}
}

// ClaimMR claims an MR for processing by setting the assignee field.
// This replaces mrqueue.Claim() for beads-based MRs.
// The workerID is typically the refinery's identifier (e.g., "gastown/refinery").
func (e *Engineer) ClaimMR(mrID, workerID string) error {
	return e.beads.Update(mrID, beads.UpdateOptions{
		Assignee: &workerID,
	})
}

// ReleaseMR releases a claimed MR back to the queue by clearing the assignee.
// This replaces mrqueue.Release() for beads-based MRs.
func (e *Engineer) ReleaseMR(mrID string) error {
	empty := ""
	return e.beads.Update(mrID, beads.UpdateOptions{
		Assignee: &empty,
	})
}

// postMergeConvoyCheck runs convoy completion checks after a successful merge.
//
// When a source issue is closed by a merge, any convoy tracking that issue may
// now be complete (all tracked issues closed). This method:
//  1. Runs `gt convoy check` to auto-close completed convoys and notify subscribers
//  2. For completed convoys with integration branches (swarms), triggers landing
//  3. Cleans up stale polecat branches from completed work
//
// All operations are best-effort: failures are logged but don't affect merge success.
func (e *Engineer) postMergeConvoyCheck(mr *MRInfo) {
	// Find town root from rig path (rig is at ~/gt/<rigname>, town is ~/gt)
	townRoot := filepath.Dir(e.rig.Path)
	townBeads := filepath.Join(townRoot, ".beads")

	// Quick check: does town-level beads exist?
	if _, err := os.Stat(townBeads); os.IsNotExist(err) {
		return
	}

	// Step 1: Run `gt convoy check` to auto-close completed convoys.
	// This handles cross-rig convoy completion: convoys in town beads (hq-*)
	// tracking issues in rig beads (gt-*) won't auto-close via bd close alone.
	closedConvoys := e.checkAndCloseCompletedConvoys(townRoot, townBeads)

	// Step 2: For each closed convoy, check if it has a swarm with an
	// integration branch that needs landing.
	for _, convoy := range closedConvoys {
		e.landConvoySwarm(townRoot, convoy)
	}

	// Step 3: Notify deacon of convoy-eligible merges for immediate feeding.
	// When the merged MR is part of a convoy, send a structured CONVOY_NEEDS_FEEDING
	// protocol message so the deacon can immediately feed the next ready issue
	// instead of waiting for the next patrol cycle (up to 10 minutes).
	e.notifyDeaconConvoyFeeding(mr)

	// Step 4: Clean up stale branches from completed work.
	// Prune remote tracking refs that no longer exist on origin.
	if e.config.DeleteMergedBranches {
		e.pruneStaleRemoteRefs()
	}
}

// notifyDeaconConvoyFeeding sends a CONVOY_NEEDS_FEEDING protocol message to
// the deacon when the merged MR is part of a convoy. This triggers immediate
// convoy feeding instead of waiting for the next deacon patrol cycle (up to
// 10 minutes). An event is also emitted to wake the deacon from await-signal.
func (e *Engineer) notifyDeaconConvoyFeeding(mr *MRInfo) {
	if mr.ConvoyID == "" {
		return
	}

	// Nudge deacon about convoy feeding instead of sending permanent mail.
	// The deacon discovers convoy state from beads on next patrol cycle;
	// this nudge just accelerates discovery.
	nudgeMsg := fmt.Sprintf("CONVOY_NEEDS_FEEDING: convoy=%s issue=%s", mr.ConvoyID, mr.SourceIssue)
	nudgeCmd := exec.Command("gt", "nudge", "deacon", nudgeMsg)
	util.SetDetachedProcessGroup(nudgeCmd)
	nudgeCmd.Dir = e.workDir
	if err := nudgeCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to nudge deacon about convoy feeding for %s: %v\n", mr.ConvoyID, err)
	} else {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Nudged deacon: CONVOY_NEEDS_FEEDING %s\n", mr.ConvoyID)
	}

	// Emit event to wake deacon from await-signal.
	_ = events.LogFeed(events.TypeMail, e.rig.Name+"/refinery", events.MailPayload("deacon/", "CONVOY_NEEDS_FEEDING "+mr.ConvoyID))
}

// convoyInfo holds minimal info about a closed convoy for post-merge processing.
type convoyInfo struct {
	ID          string
	Title       string
	Description string
}

func refineryHasLabel(labels []string, target string) bool {
	for _, label := range labels {
		if label == target {
			return true
		}
	}
	return false
}

// checkAndCloseCompletedConvoys finds and closes convoys where all tracked issues
// are complete. Returns the list of convoys that were closed.
func (e *Engineer) checkAndCloseCompletedConvoys(townRoot, townBeads string) []convoyInfo {
	townReadEnv := beads.BuildReadOnlyPinnedBDEnv(os.Environ(), townBeads)
	townMutationEnv := beads.BuildMutationPinnedBDEnv(os.Environ(), townBeads)
	routingReadEnv := beads.BuildReadOnlyRoutingBDEnv(os.Environ(), townBeads)

	// List all open issues and filter locally so legacy type=convoy beads remain visible.
	listArgs := beads.InjectFlatForListJSON([]string{"list", "--status=open", "--json", "--limit=0"})
	listArgs = beads.MaybePrependAllowStaleWithEnv(townReadEnv, listArgs)
	listCmd := beads.Command(townBeads, townBeads, beads.ReadOnlyPinned, listArgs...)
	var stdout bytes.Buffer
	listCmd.Stdout = &stdout

	if err := listCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to list convoys: %v\n", err)
		return nil
	}

	var convoys []struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Status      string   `json:"status"`
		Description string   `json:"description"`
		IssueType   string   `json:"issue_type"`
		Labels      []string `json:"labels"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &convoys); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to parse convoy list: %v\n", err)
		return nil
	}

	var closed []convoyInfo

	for _, convoy := range convoys {
		if convoy.IssueType != "convoy" && !refineryHasLabel(convoy.Labels, "gt:convoy") {
			continue
		}
		// Get tracked issues for this convoy via bd dep list
		depArgs := beads.MaybePrependAllowStaleWithEnv(townReadEnv, []string{"dep", "list", convoy.ID, "--direction=down", "--type=tracks", "--json"})
		depCmd := beads.Command(townRoot, townBeads, beads.ReadOnlyPinned, depArgs...)
		var depOut bytes.Buffer
		depCmd.Stdout = &depOut

		if err := depCmd.Run(); err != nil {
			continue
		}

		var deps []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(depOut.Bytes(), &deps); err != nil {
			continue
		}

		// Refresh statuses from home rigs (cross-rig lookup)
		allClosed := true
		for _, dep := range deps {
			// Unwrap external:prefix:id format
			depID := dep.ID
			if strings.HasPrefix(depID, "external:") {
				parts := strings.SplitN(depID, ":", 3)
				if len(parts) == 3 {
					depID = parts[2]
				}
			}

			// Get fresh status from home rig via bd show with routing
			showArgs := beads.MaybePrependAllowStaleWithEnv(routingReadEnv, []string{"show", depID, "--json"})
			showCmd := beads.Command(townRoot, townBeads, beads.ReadOnlyRouting, showArgs...)
			var showOut bytes.Buffer
			showCmd.Stdout = &showOut

			if err := showCmd.Run(); err != nil || showOut.Len() == 0 {
				// Can't verify - treat as open to be safe
				allClosed = false
				break
			}

			var issues []struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(showOut.Bytes(), &issues); err != nil || len(issues) == 0 {
				allClosed = false
				break
			}

			if issues[0].Status != "closed" && issues[0].Status != "tombstone" {
				allClosed = false
				break
			}
		}

		if !allClosed {
			continue
		}

		// All tracked issues are complete - close the convoy
		reason := "All tracked issues completed"
		if len(deps) == 0 {
			reason = "Empty convoy — auto-closed as definitionally complete"
		}

		closeArgs := beads.MaybePrependAllowStaleWithEnv(townMutationEnv, []string{"close", convoy.ID, "-r", reason})
		closeCmd := beads.Command(townBeads, townBeads, beads.MutationPinned, closeArgs...)

		if err := closeCmd.Run(); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to close convoy %s: %v\n", convoy.ID, err)
			continue
		}

		_, _ = fmt.Fprintf(e.output, "[Engineer] Auto-closed convoy %s: %s\n", convoy.ID, convoy.Title)
		closed = append(closed, convoyInfo{
			ID:          convoy.ID,
			Title:       convoy.Title,
			Description: convoy.Description,
		})

		// Send convoy completion notifications (owner + notify addresses)
		e.notifyConvoyCompletion(townRoot, convoy.ID, convoy.Title, convoy.Description)
	}

	return closed
}

// notifyConvoyCompletion sends notifications to convoy owner and notify addresses.
func (e *Engineer) notifyConvoyCompletion(townRoot, convoyID, title, description string) {
	fields, shouldNotify := e.claimConvoyCompletionNotification(townRoot, convoyID, description)
	if !shouldNotify {
		return
	}
	for _, addr := range fields.NotificationAddresses() {
		mailCmd := exec.Command("gt", "mail", "send", addr,
			"-s", fmt.Sprintf("🚚 Convoy landed: %s", title),
			"-m", fmt.Sprintf("Convoy %s has completed.\n\nAll tracked issues are now closed.\n\nClosed by: %s/refinery", convoyID, e.rig.Name),
			"--from", "convoy/"+convoyID,
			"--no-notify")
		util.SetDetachedProcessGroup(mailCmd)
		mailCmd.Dir = townRoot
		if err := mailCmd.Run(); err != nil {
			_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not notify %s: %v\n", addr, err)
		}
	}
}

func (e *Engineer) claimConvoyCompletionNotification(townRoot, convoyID, fallbackDescription string) (*beads.ConvoyFields, bool) {
	townBeads := filepath.Join(townRoot, ".beads")
	description := fallbackDescription

	readEnv := beads.BuildReadOnlyPinnedBDEnv(os.Environ(), townBeads)
	showArgs := beads.MaybePrependAllowStaleWithEnv(readEnv, []string{"show", convoyID, "--json"})
	showCmd := beads.Command(townBeads, townBeads, beads.ReadOnlyPinned, showArgs...)
	var showOut bytes.Buffer
	showCmd.Stdout = &showOut
	if err := showCmd.Run(); err == nil && showOut.Len() > 0 {
		var convoys []struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(showOut.Bytes(), &convoys); err == nil && len(convoys) > 0 {
			description = convoys[0].Description
		}
	}

	fields := beads.ParseConvoyFields(&beads.Issue{Description: description})
	if fields == nil {
		fields = &beads.ConvoyFields{}
	}
	if fields.CompletionNotifiedAt != "" {
		return fields, false
	}

	fields.CompletionNotifiedAt = time.Now().UTC().Format(time.RFC3339)
	newDesc := beads.SetConvoyFields(&beads.Issue{Description: description}, fields)
	mutationEnv := beads.BuildMutationPinnedBDEnv(os.Environ(), townBeads)
	updateArgs := beads.MaybePrependAllowStaleWithEnv(mutationEnv, []string{"update", convoyID, "--description=" + newDesc})
	updateCmd := beads.Command(townBeads, townBeads, beads.MutationPinned, updateArgs...)
	if err := updateCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not record convoy completion notification state for %s: %v\n", convoyID, err)
		return fields, false
	}

	return fields, true
}

// landConvoySwarm checks if a completed convoy has an associated swarm with an
// integration branch, and triggers landing if so.
func (e *Engineer) landConvoySwarm(townRoot string, convoy convoyInfo) {
	// ZFC: Use typed accessor instead of parsing description text
	fields := beads.ParseConvoyFields(&beads.Issue{Description: convoy.Description})
	var moleculeID string
	if fields != nil {
		moleculeID = fields.Molecule
	}

	if moleculeID == "" {
		return // No swarm/molecule associated with this convoy
	}

	// Check if the molecule has an integration branch (swarm/* pattern)
	integrationBranch := fmt.Sprintf("swarm/%s", moleculeID)
	branchExists, err := e.git.BranchExists(integrationBranch)
	if err != nil || !branchExists {
		// Also check remote
		remoteExists, _ := e.git.RemoteTrackingBranchExists("origin", integrationBranch)
		if !remoteExists {
			return // No integration branch to land
		}
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] Landing integration branch %s for convoy %s...\n", integrationBranch, convoy.ID)

	// Use gt swarm land to perform the landing
	landCmd := exec.Command("gt", "swarm", "land", moleculeID)
	util.SetDetachedProcessGroup(landCmd)
	landCmd.Dir = townRoot
	var landOut, landErr bytes.Buffer
	landCmd.Stdout = &landOut
	landCmd.Stderr = &landErr

	if err := landCmd.Run(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to land swarm %s: %v (%s)\n",
			moleculeID, err, strings.TrimSpace(landErr.String()))
		return
	}

	_, _ = fmt.Fprintf(e.output, "[Engineer] ✓ Landed integration branch for convoy %s\n", convoy.ID)
}

// pruneStaleRemoteRefs prunes remote tracking refs that no longer exist on origin.
// This cleans up refs from branches that were deleted on the remote after merge.
func (e *Engineer) pruneStaleRemoteRefs() {
	if err := e.git.FetchPrune("origin"); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to prune stale remote refs: %v\n", err)
	}
}
