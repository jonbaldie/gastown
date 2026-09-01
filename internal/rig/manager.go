package rig

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gofrs/flock"
	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/hooks"
	"github.com/jonbaldie/gastown/internal/templates/commands"
	"github.com/jonbaldie/gastown/internal/util"
)

// Common errors
var (
	ErrRigNotFound = errors.New("rig not found")
	ErrRigExists   = errors.New("rig already exists")
)

// reservedRigNames are names that cannot be used for rigs because they
// collide with town-level infrastructure. "hq" is special-cased by
// EnsureMetadata and dolt routing as the town-level beads alias.
var reservedRigNames = []string{"hq"}

// SanitizeName converts a folder name into a legal rig name.
// Hyphen, dot, and space become underscore. This matches one-repo quick-add.
func SanitizeName(name string) string {
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, ".", "_")
	name = strings.ReplaceAll(name, " ", "_")
	return name
}

// IsReservedName reports whether name is reserved for town-level infrastructure.
func IsReservedName(name string) bool {
	for _, reserved := range reservedRigNames {
		if strings.EqualFold(name, reserved) {
			return true
		}
	}
	return false
}

// wrapCloneError wraps clone errors with helpful suggestions.
// Detects common auth failures and suggests SSH as an alternative.
func wrapCloneError(err error, gitURL string) error {
	errStr := err.Error()

	// Check for GitHub password auth failure
	if strings.Contains(errStr, "Password authentication is not supported") ||
		strings.Contains(errStr, "Authentication failed") {
		// Check if they used HTTPS
		if strings.HasPrefix(gitURL, "https://") {
			// Try to suggest the SSH equivalent
			sshURL := convertToSSH(gitURL)
			if sshURL != "" {
				return fmt.Errorf("creating bare repo: %w\n\nHint: GitHub no longer supports password authentication.\nTry using SSH instead:\n  gt rig add <name> %s", err, sshURL)
			}
			return fmt.Errorf("creating bare repo: %w\n\nHint: GitHub no longer supports password authentication.\nTry using an SSH URL (git@github.com:owner/repo.git) or a personal access token.", err)
		}
	}

	return fmt.Errorf("creating bare repo: %w", err)
}

// convertToSSH converts an HTTPS GitHub/GitLab URL to SSH format.
// Returns empty string if conversion is not possible.
func convertToSSH(httpsURL string) string {
	// Handle GitHub: https://github.com/owner/repo.git -> git@github.com:owner/repo.git
	if strings.HasPrefix(httpsURL, "https://github.com/") {
		path := strings.TrimPrefix(httpsURL, "https://github.com/")
		if !strings.HasSuffix(path, ".git") {
			path += ".git"
		}
		return "git@github.com:" + path
	}

	// Handle GitLab: https://gitlab.com/owner/repo.git -> git@gitlab.com:owner/repo.git
	if strings.HasPrefix(httpsURL, "https://gitlab.com/") {
		path := strings.TrimPrefix(httpsURL, "https://gitlab.com/")
		if !strings.HasSuffix(path, ".git") {
			path += ".git"
		}
		return "git@gitlab.com:" + path
	}

	// Handle Bitbucket: https://bitbucket.org/workspace/repo.git -> git@bitbucket.org:workspace/repo.git
	if strings.HasPrefix(httpsURL, "https://bitbucket.org/") {
		path := strings.TrimPrefix(httpsURL, "https://bitbucket.org/")
		if !strings.HasSuffix(path, ".git") {
			path += ".git"
		}
		return "git@bitbucket.org:" + path
	}

	return ""
}

// RigConfig represents the rig-level configuration (config.json at rig root).
type RigConfig struct {
	Type          string       `json:"type"`                     // "rig"
	Version       int          `json:"version"`                  // schema version
	Name          string       `json:"name"`                     // rig name
	GitURL        string       `json:"git_url"`                  // repository URL (fetch/pull)
	PushURL       string       `json:"push_url,omitempty"`       // optional push URL (fork for read-only upstreams)
	UpstreamURL   string       `json:"upstream_url,omitempty"`   // optional upstream URL (for fork workflows)
	LocalRepo     string       `json:"local_repo,omitempty"`     // optional local reference repo
	DefaultBranch string       `json:"default_branch,omitempty"` // main, master, etc.
	CreatedAt     time.Time    `json:"created_at"`               // when rig was created
	Beads         *BeadsConfig `json:"beads,omitempty"`

	// Persistent polecat pool configuration.
	// PolecatPoolSize is the number of persistent polecats to create with pool init.
	// PolecatNames optionally specifies fixed names (overrides theme-based naming).
	PolecatPoolSize int      `json:"polecat_pool_size,omitempty"`
	PolecatNames    []string `json:"polecat_names,omitempty"`
}

// BeadsConfig represents beads configuration for the rig.
type BeadsConfig struct {
	Prefix string `json:"prefix"` // issue prefix (e.g., "gt")
}

// CurrentRigConfigVersion is the current schema version.
const CurrentRigConfigVersion = 1

// Manager handles rig discovery, loading, and creation.
type Manager struct {
	townRoot string
	config   *config.RigsConfig
	git      *git.Git
}

// NewManager creates a new rig manager.
func NewManager(townRoot string, rigsConfig *config.RigsConfig, g *git.Git) *Manager {
	return &Manager{
		townRoot: townRoot,
		config:   rigsConfig,
		git:      g,
	}
}

// DiscoverRigs returns all rigs registered in the workspace.
// Rigs that fail to load are logged to stderr and skipped; partial results are returned.
func (m *Manager) DiscoverRigs() ([]*Rig, error) {
	var rigs []*Rig

	for name, entry := range m.config.Rigs {
		rig, err := m.loadRig(name, entry)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load rig %q: %v\n", name, err)
			continue
		}
		rigs = append(rigs, rig)
	}

	slices.SortFunc(rigs, func(a, b *Rig) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return rigs, nil
}

// GetRig returns a specific rig by name.
func (m *Manager) GetRig(name string) (*Rig, error) {
	entry, ok := m.config.Rigs[name]
	if !ok {
		return nil, ErrRigNotFound
	}

	return m.loadRig(name, entry)
}

// RigExists checks if a rig is registered.
func (m *Manager) RigExists(name string) bool {
	_, ok := m.config.Rigs[name]
	return ok
}

// UsedNamepoolThemes returns the namepool themes currently in use by existing rigs.
// It checks each rig's settings/config.json for an explicit namepool.style.
// If no setting is configured, calls the fallbackTheme function to get the default theme.
func (m *Manager) UsedNamepoolThemes(fallbackTheme func(rigName string) string) []string {
	var themes []string
	for name := range m.config.Rigs {
		rigPath := filepath.Join(m.townRoot, name)
		settingsPath := filepath.Join(rigPath, "settings", "config.json")
		if settings, err := config.LoadRigSettings(settingsPath); err == nil && settings.Namepool != nil && settings.Namepool.Style != "" {
			themes = append(themes, settings.Namepool.Style)
		} else {
			themes = append(themes, fallbackTheme(name))
		}
	}
	return themes
}

// loadRig loads rig details from the filesystem.
func (m *Manager) loadRig(name string, entry config.RigEntry) (*Rig, error) {
	rigPath := filepath.Join(m.townRoot, name)
	if err := validateRigDirectory(rigPath); err != nil {
		return nil, fmt.Errorf("rig directory: %w", err)
	}
	rig := &Rig{
		Name:      name,
		Path:      rigPath,
		GitURL:    entry.GitURL,
		PushURL:   strings.TrimSpace(entry.PushURL),
		LocalRepo: entry.LocalRepo,
		Config:    entry.BeadsConfig,
	}

	rig.Polecats = scanRigWorkers(filepath.Join(rigPath, "polecats"))
	rig.Crew = scanRigWorkers(filepath.Join(rigPath, "crew"))
	rig.HasWitness = directoryExists(filepath.Join(rigPath, "witness"))
	rig.HasRefinery = pathExists(filepath.Join(rigPath, "refinery", "rig"))
	rig.HasMayor = pathExists(filepath.Join(rigPath, "mayor", "rig"))
	return rig, nil
}

func validateRigDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", path)
	}
	return nil
}

func scanRigWorkers(path string) []string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	workers := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			workers = append(workers, entry.Name())
		}
	}
	return workers
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// AddRigOptions configures rig creation.
type AddRigOptions struct {
	Name           string   // Rig name (directory name)
	GitURL         string   // Repository URL (fetch/pull)
	PushURL        string   // Optional push URL (fork for read-only upstreams)
	UpstreamURL    string   // Optional upstream URL (for fork workflows)
	BeadsPrefix    string   // Beads issue prefix (defaults to derived from name)
	LocalRepo      string   // Optional local repo for reference clones
	DefaultBranch  string   // Default branch (defaults to auto-detected from remote)
	SkipDoltCheck  bool     // Skip Dolt server availability check (for tests with mocked beads)
	CloneFilter    string   // Git clone filter spec (e.g. "blob:none", "tree:0") for partial clones
	SparseCheckout []string // Sparse checkout paths (cone mode); empty means no sparse checkout
	ImportBeads    bool     // Explicit consent to activate tracked Beads data and executable hooks
}

func resolveLocalRepo(path, gitURL string) (string, string) {
	if path == "" {
		return "", ""
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Sprintf("local repo path invalid: %v", err)
	}

	absPath, err = filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Sprintf("local repo path invalid: %v", err)
	}

	repoGit := git.NewGit(absPath)
	if !git.IsRepo(repoGit) {
		return "", fmt.Sprintf("local repo is not a git repository: %s", absPath)
	}

	origin, err := git.ConfiguredRemoteURL(repoGit, "origin")
	if err != nil {
		if errors.Is(err, git.ErrRemoteNotConfigured) {
			return absPath, "local repo has no origin; using it anyway"
		}
		return "", fmt.Sprintf("local repo origin: %v", err)
	}
	if origin != gitURL {
		return "", fmt.Sprintf("local repo origin %q does not match %q", origin, gitURL)
	}

	return absPath, ""
}

// AddRig creates a new rig as a container with clones for each agent.
// The rig structure is:
//
//	<name>/                    # Container (NOT a git clone)
//	├── config.json            # Rig configuration
//	├── .beads/                # Rig-level issue tracking
//	├── refinery/rig/          # Canonical main clone
//	├── mayor/rig/             # Mayor's working clone
//	├── witness/               # Witness agent (no clone)
//	├── polecats/              # Worker directories (empty)
//	└── crew/<crew>/           # Default human workspace
func (m *Manager) AddRig(opts AddRigOptions) (*Rig, error) {
	prepared, err := prepareAddRig(m, opts)
	if err != nil {
		return nil, err
	}
	opts = prepared.options
	if prepared.warning != "" {
		fmt.Printf("  Warning: %s\n", prepared.warning)
	}
	ownershipStamp, rigConfig, err := initializeRigAddPath(prepared.rigPath, opts, prepared.localRepo, m.saveRigConfig)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			removeRigPathIfOwned(prepared.rigPath, ownershipStamp)
		}
	}()
	if err := provisionRigAddition(m, prepared, &opts, rigConfig); err != nil {
		return nil, err
	}
	runNewRigBestEffortSetup(
		func() error { return initAgentBeads(prepared.rigPath, opts.Name, opts.BeadsPrefix) },
		func() error { return m.seedPatrolMolecules(prepared.rigPath) },
		func() error { return createPluginDirectories(m.townRoot, prepared.rigPath) },
	)
	if err := registerNewRig(m.townRoot, m.config, opts, prepared.localRepo, func() error {
		return verifyRigIdentity(m.townRoot, prepared.rigPath, opts.Name)
	}); err != nil {
		return nil, err
	}
	success = true
	_ = clearAddOwnershipStamp(prepared.rigPath)
	return m.loadRig(opts.Name, m.config.Rigs[opts.Name])
}

type preparedRigAdd struct {
	options            AddRigOptions
	rigPath            string
	localRepo          string
	warning            string
	userProvidedPrefix bool
}

func initializeRigAddPath(
	rigPath string,
	opts AddRigOptions,
	localRepo string,
	save func(string, *RigConfig) error,
) (string, *RigConfig, error) {
	if err := os.MkdirAll(rigPath, 0755); err != nil {
		return "", nil, fmt.Errorf("creating rig directory: %w", err)
	}
	stamp, err := newAddOwnershipStamp()
	if err != nil {
		_ = os.RemoveAll(rigPath)
		return "", nil, fmt.Errorf("generating ownership stamp: %w", err)
	}
	if err := writeAddOwnershipStamp(rigPath, stamp); err != nil {
		_ = os.RemoveAll(rigPath)
		return "", nil, fmt.Errorf("writing ownership stamp: %w", err)
	}
	cfg := &RigConfig{
		Type:        "rig",
		Version:     CurrentRigConfigVersion,
		Name:        opts.Name,
		GitURL:      opts.GitURL,
		PushURL:     opts.PushURL,
		UpstreamURL: opts.UpstreamURL,
		LocalRepo:   localRepo,
		CreatedAt:   time.Now(),
		Beads:       &BeadsConfig{Prefix: opts.BeadsPrefix},
	}
	if err := save(rigPath, cfg); err != nil {
		return "", nil, fmt.Errorf("saving rig config: %w", err)
	}
	return stamp, cfg, nil
}

func provisionRigAddition(m *Manager, prepared preparedRigAdd, opts *AddRigOptions, cfg *RigConfig) error {
	fmt.Printf("  Cloning repository (this may take a moment)...\n")
	bareRepoPath := filepath.Join(prepared.rigPath, ".repo.git")
	bareGit, defaultBranch, err := setupBareRigRepository(m.git, *opts, prepared.localRepo, bareRepoPath)
	if err != nil {
		return err
	}
	cfg.DefaultBranch = defaultBranch
	if err := m.saveRigConfig(prepared.rigPath, cfg); err != nil {
		return fmt.Errorf("updating rig config with default branch: %w", err)
	}
	mayorRigPath, err := createMayorRigClone(m.git, *opts, prepared.rigPath, bareRepoPath, defaultBranch)
	if err != nil {
		return err
	}
	if err := importTrackedRigBeads(m.townRoot, mayorRigPath, *opts, prepared.userProvidedPrefix, func(prefix string) error {
		opts.BeadsPrefix = prefix
		cfg.Beads.Prefix = prefix
		if err := m.saveRigConfig(prepared.rigPath, cfg); err != nil {
			return fmt.Errorf("updating rig config with detected prefix: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := provisionNewRigBeads(m.townRoot, prepared.rigPath, opts.Name, func() error {
		return m.InitializeRigBeads(prepared.rigPath, opts.Name, opts.BeadsPrefix, RigBeadsInitOptions{SkipDoltCheck: opts.SkipDoltCheck})
	}); err != nil {
		return err
	}
	if err := createNewRigWorkspaces(m.townRoot, prepared.rigPath, bareGit, defaultBranch); err != nil {
		return err
	}
	return prepareNewRigRouting(m.townRoot, prepared.rigPath, opts.Name, opts.BeadsPrefix)
}

func cloneBareRig(g *git.Git, opts AddRigOptions, localRepo, bareRepoPath string) error {
	branch := opts.DefaultBranch
	if opts.CloneFilter != "" && localRepo != "" {
		if err := git.CloneBarePartialWithReferenceAndBranch(g, opts.GitURL, bareRepoPath, opts.CloneFilter, localRepo, branch); err == nil {
			return nil
		} else {
			fmt.Printf("  Warning: could not use local repo reference with filter: %v\n", err)
		}
		_ = os.RemoveAll(bareRepoPath)
		return git.CloneBarePartialWithBranch(g, opts.GitURL, bareRepoPath, opts.CloneFilter, branch)
	}
	if opts.CloneFilter != "" {
		return git.CloneBarePartialWithBranch(g, opts.GitURL, bareRepoPath, opts.CloneFilter, branch)
	}
	if localRepo == "" {
		return git.CloneBareWithBranch(g, opts.GitURL, bareRepoPath, branch)
	}
	if err := git.CloneBareWithReferenceAndBranch(g, opts.GitURL, bareRepoPath, localRepo, branch); err == nil {
		return nil
	} else {
		fmt.Printf("  Warning: could not use local repo reference: %v\n", err)
	}
	_ = os.RemoveAll(bareRepoPath)
	return git.CloneBareWithBranch(g, opts.GitURL, bareRepoPath, branch)
}

func setupBareRigRepository(g *git.Git, opts AddRigOptions, localRepo, bareRepoPath string) (*git.Git, string, error) {
	if err := cloneBareRig(g, opts, localRepo, bareRepoPath); err != nil {
		if hasRefs, refsErr := git.RemoteHasRefs(g, opts.GitURL); refsErr == nil && !hasRefs {
			return nil, "", emptyRigRepositoryError(opts.GitURL)
		}
		return nil, "", wrapCloneError(err, opts.GitURL)
	}
	printBareCloneSuccess(opts.CloneFilter)
	bareGit := git.NewGitWithDir(bareRepoPath, "")
	if err := validateBareRigRepository(g, bareGit, opts.GitURL); err != nil {
		return nil, "", err
	}
	if err := configureRigRemotes(bareGit, opts); err != nil {
		return nil, "", err
	}
	branch, err := resolveRigDefaultBranch(bareGit, opts.DefaultBranch)
	if err != nil {
		return nil, "", err
	}
	return bareGit, branch, nil
}

func emptyRigRepositoryError(gitURL string) error {
	return fmt.Errorf("repository %s is empty (no commits). Push at least one commit before adding it as a rig", gitURL)
}

func printBareCloneSuccess(filter string) {
	if filter == "" {
		fmt.Printf("   ✓ Created shared bare repo\n")
		return
	}
	fmt.Printf("   ✓ Created shared bare repo (partial: --filter=%s)\n", filter)
}

func validateBareRigRepository(g, bareGit *git.Git, gitURL string) error {
	empty, err := git.IsEmpty(bareGit)
	if err != nil {
		return fmt.Errorf("checking if repository is empty: %w", err)
	}
	if !empty {
		return nil
	}
	hasRefs, err := git.RemoteHasRefs(g, gitURL)
	if err != nil {
		return fmt.Errorf("checking if repository is empty: %w", err)
	}
	if !hasRefs {
		return emptyRigRepositoryError(gitURL)
	}
	return fmt.Errorf("repository %s has refs, but no default branch could be cloned. Ensure the remote HEAD points to a branch, or pass --branch <branch>", gitURL)
}

func configureRigRemotes(bareGit *git.Git, opts AddRigOptions) error {
	if opts.PushURL != "" {
		if err := git.ConfigurePushURL(bareGit, "origin", opts.PushURL); err != nil {
			return fmt.Errorf("configuring push URL: %w", err)
		}
		fmt.Printf("   ✓ Configured push URL (fork: %s)\n", util.RedactURL(opts.PushURL))
	}
	if opts.UpstreamURL != "" {
		if err := git.AddUpstreamRemote(bareGit, opts.UpstreamURL); err != nil {
			return fmt.Errorf("configuring upstream remote: %w", err)
		}
		fmt.Printf("   ✓ Configured upstream remote: %s\n", util.RedactURL(opts.UpstreamURL))
	}
	return nil
}

func resolveRigDefaultBranch(bareGit *git.Git, requested string) (string, error) {
	if requested == "" {
		return git.DefaultBranch(bareGit), nil
	}
	ref := fmt.Sprintf("origin/%s", requested)
	if exists, _ := git.RefExists(bareGit, ref); exists {
		return requested, nil
	}
	if err := git.FetchBranchShallow(bareGit, "origin", requested); err != nil {
		return "", fmt.Errorf("branch %q does not exist on remote or could not be fetched: %w", requested, err)
	}
	return requested, nil
}

func createMayorRigClone(g *git.Git, opts AddRigOptions, rigPath, bareRepoPath, defaultBranch string) (string, error) {
	fmt.Printf("  Creating mayor clone...\n")
	mayorRigPath := filepath.Join(rigPath, "mayor", "rig")
	if err := os.MkdirAll(filepath.Dir(mayorRigPath), 0755); err != nil {
		return "", fmt.Errorf("creating mayor dir: %w", err)
	}
	if err := cloneMayorRig(g, opts, mayorRigPath, bareRepoPath, defaultBranch); err != nil {
		return "", err
	}
	if len(opts.SparseCheckout) > 0 {
		if err := git.InitSparseCheckout(mayorRigPath, opts.SparseCheckout); err != nil {
			return "", fmt.Errorf("initializing sparse checkout for mayor: %w", err)
		}
		fmt.Printf("   ✓ Configured sparse checkout: %v\n", opts.SparseCheckout)
	}
	if err := configureMayorRemotes(git.NewGitWithDir("", mayorRigPath), opts); err != nil {
		return "", err
	}
	fmt.Printf("   ✓ Created mayor clone\n")
	return mayorRigPath, nil
}

func cloneMayorRig(g *git.Git, opts AddRigOptions, mayorRigPath, bareRepoPath, defaultBranch string) error {
	if opts.CloneFilter != "" {
		if err := git.CloneBranchPartialWithReference(g, opts.GitURL, mayorRigPath, defaultBranch, opts.CloneFilter, bareRepoPath); err == nil {
			return nil
		} else {
			fmt.Printf("  Warning: could not use bare repo as reference with filter: %v\n", err)
		}
		_ = os.RemoveAll(mayorRigPath)
		if err := git.CloneBranchPartial(g, opts.GitURL, mayorRigPath, defaultBranch, opts.CloneFilter); err != nil {
			return fmt.Errorf("cloning for mayor: %w", err)
		}
		return nil
	}
	if err := git.CloneBranchWithReference(g, opts.GitURL, mayorRigPath, defaultBranch, bareRepoPath); err == nil {
		return nil
	} else {
		fmt.Printf("  Warning: could not use bare repo as reference: %v\n", err)
	}
	_ = os.RemoveAll(mayorRigPath)
	if err := git.CloneBranch(g, opts.GitURL, mayorRigPath, defaultBranch); err != nil {
		return fmt.Errorf("cloning for mayor: %w", err)
	}
	return nil
}

func configureMayorRemotes(mayorGit *git.Git, opts AddRigOptions) error {
	if opts.PushURL != "" {
		if err := git.ConfigurePushURL(mayorGit, "origin", opts.PushURL); err != nil {
			return fmt.Errorf("configuring mayor push URL: %w", err)
		}
	}
	if opts.UpstreamURL != "" {
		if err := git.AddUpstreamRemote(mayorGit, opts.UpstreamURL); err != nil {
			return fmt.Errorf("configuring mayor upstream remote: %w", err)
		}
	}
	return nil
}

func importTrackedRigBeads(townRoot, mayorRigPath string, opts AddRigOptions, userProvidedPrefix bool, adoptPrefix func(string) error) error {
	inspection, err := InspectTrackedBeadsImport(mayorRigPath)
	if err != nil {
		return fmt.Errorf("inspecting tracked Beads import: %w", err)
	}
	if err := RequireTrackedBeadsImportConsent(inspection, opts.ImportBeads); err != nil {
		return err
	}
	if inspection.RequiresConsent() {
		fmt.Printf("  Importing %d bead(s) from %s (%v); executable hooks: %v\n",
			inspection.BeadCount, inspection.Source, inspection.JSONLFiles, inspection.ExecutableHooks)
	}
	sourceBeadsDir := filepath.Join(mayorRigPath, ".beads")
	if !pathExists(sourceBeadsDir) {
		return nil
	}
	_ = os.Remove(filepath.Join(sourceBeadsDir, "redirect"))
	sourceConfig := filepath.Join(sourceBeadsDir, "config.yaml")
	prefix, err := adoptTrackedBeadsPrefix(sourceConfig, opts.BeadsPrefix, userProvidedPrefix, adoptPrefix)
	if err != nil {
		return err
	}
	if bdDatabaseExists(sourceBeadsDir) {
		return nil
	}
	initializeTrackedBeadsDatabase(townRoot, mayorRigPath, sourceBeadsDir, sourceConfig, opts.Name, prefix)
	return nil
}

func adoptTrackedBeadsPrefix(configPath, configured string, userProvided bool, adopt func(string) error) (string, error) {
	source := detectBeadsPrefixFromConfig(configPath)
	if source == "" {
		fmt.Printf("  Using prefix '%s' for tracked beads (no existing issues to detect from)\n", configured)
		return configured, nil
	}
	fmt.Printf("  Detected existing beads prefix '%s' from source repo\n", source)
	if userProvided && strings.TrimSuffix(configured, "-") != strings.TrimSuffix(source, "-") {
		return "", fmt.Errorf("prefix mismatch: source repo uses '%s' but --prefix '%s' was provided; use --prefix %s to match existing issues", source, configured, source)
	}
	if err := adopt(source); err != nil {
		return "", err
	}
	return source, nil
}

func initializeTrackedBeadsDatabase(townRoot, mayorRigPath, beadsDir, configPath, rigName, prefix string) {
	args := trackedBeadsInitArgs(townRoot, configPath, rigName, prefix)
	cmd := beads.Spawn(args...)
	cmd.Dir = mayorRigPath
	cmd.Env = bdSubprocessEnv(beadsDir, rigName)
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("  Warning: Could not init bd database: %v (%s)\n", err, strings.TrimSpace(string(output)))
	}
	if err := dropRigOrphanDBs(townRoot, prefix, rigName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: orphan database cleanup: %v\n", err)
	}
}

func trackedBeadsInitArgs(townRoot, configPath, rigName, prefix string) []string {
	args := []string{"init"}
	if prefix != "" {
		args = append(args, "--prefix", prefix)
	}
	if rigName != "" {
		args = append(args, "--database", rigName)
	}
	args = append(args, "--server", "--server-port", strconv.Itoa(bdInitServerPort(townRoot)))
	if beadsConfigHasSyncRemote(configPath) {
		args = append(args, "--reinit-local", "--discard-remote", "--destroy-token=DESTROY-"+prefix)
	}
	return args
}

func provisionNewRigBeads(townRoot, rigPath, rigName string, initialize func() error) error {
	if err := initialize(); err != nil {
		return err
	}
	setupRigDoltHubRemote(townRoot, rigName)
	if err := beads.ProvisionPrimeMD(beads.ResolveBeadsDir(rigPath)); err != nil {
		fmt.Printf("  Warning: Could not provision PRIME.md: %v\n", err)
	}
	return nil
}

func setupRigDoltHubRemote(townRoot, rigName string) {
	token := doltserver.DoltHubToken()
	org := doltserver.DoltHubOrg()
	if token == "" || org == "" {
		return
	}
	dbName := "beads_" + rigName
	dbDir := doltserver.RigDatabaseDir(townRoot, dbName)
	fmt.Printf("  Setting up DoltHub remote for %s/%s...\n", org, doltserver.DoltHubRepoName(dbName))
	if err := doltserver.SetupDoltHubRemote(dbDir, org, dbName, token); err != nil {
		fmt.Printf("  Warning: DoltHub remote setup failed: %v\n", err)
		fmt.Printf("  You can set up the remote manually later with 'gt dolt sync'.\n")
		return
	}
	fmt.Printf("   ✓ DoltHub remote configured and initial push complete\n")
}

func createNewRigWorkspaces(townRoot, rigPath string, bareGit *git.Git, defaultBranch string) error {
	if err := createRefineryWorkspace(rigPath, bareGit, defaultBranch); err != nil {
		return err
	}
	crewPath := filepath.Join(rigPath, "crew")
	if err := os.MkdirAll(crewPath, 0755); err != nil {
		return fmt.Errorf("creating crew dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(crewPath, "README.md"), []byte(crewDirectoryReadme), 0644); err != nil {
		return fmt.Errorf("creating crew README: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, "witness"), 0755); err != nil {
		return fmt.Errorf("creating witness dir: %w", err)
	}
	polecatsPath := filepath.Join(rigPath, "polecats")
	if err := os.MkdirAll(polecatsPath, 0755); err != nil {
		return fmt.Errorf("creating polecats dir: %w", err)
	}
	scaffoldPolecatWorkspace(townRoot, polecatsPath)
	return nil
}

func createRefineryWorkspace(rigPath string, bareGit *git.Git, defaultBranch string) error {
	fmt.Printf("  Creating refinery worktree...\n")
	refineryPath := filepath.Join(rigPath, "refinery", "rig")
	if err := os.MkdirAll(filepath.Dir(refineryPath), 0755); err != nil {
		return fmt.Errorf("creating refinery dir: %w", err)
	}
	if err := git.WorktreeAddExisting(bareGit, refineryPath, defaultBranch); err != nil {
		return fmt.Errorf("creating refinery worktree: %w", err)
	}
	if err := git.ConfigureHooksPath(git.NewGit(refineryPath)); err != nil {
		return fmt.Errorf("configuring hooks for refinery: %w", err)
	}
	fmt.Printf("   ✓ Created refinery worktree\n")
	if err := Provision(rigPath, refineryPath, "refinery"); err != nil {
		fmt.Printf("  Warning: Could not provision refinery workspace: %v\n", err)
	}
	return nil
}

func prepareNewRigRouting(townRoot, rigPath, rigName, prefix string) error {
	if prefix != "" {
		routePath := rigName
		if pathExists(filepath.Join(rigPath, "mayor", "rig", ".beads")) {
			routePath = rigName + "/mayor/rig"
		}
		if err := beads.AppendRoute(townRoot, beads.Route{Prefix: prefix + "-", Path: routePath}); err != nil {
			fmt.Printf("  Warning: Could not update routes.jsonl: %v\n", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(rigPath, constants.DirSettings), 0755); err != nil {
		return fmt.Errorf("creating settings dir: %w", err)
	}
	return nil
}

const crewDirectoryReadme = `# Crew Directory

This directory contains crew worker workspaces.

## Adding a Crew Member

` + "```bash" + `
gt crew add <name>    # Creates crew/<name>/ with a git clone
` + "```" + `

## Crew vs Polecats

- **Crew**: Persistent, user-managed workspaces (never auto-garbage-collected)
- **Polecats**: Transient, witness-managed workers (cleaned up after work completes)

Use crew for your own workspace. Polecats are for batch work dispatch.
`

func runNewRigBestEffortSetup(createAgents, seedPatrols, createPlugins func() error) {
	steps := []struct {
		label string
		run   func() error
	}{
		{"create agent beads", createAgents},
		{"seed patrol molecules", seedPatrols},
		{"create plugin directories", createPlugins},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: Could not %s: %v\n", step.label, err)
		}
	}
}

func registerNewRig(townRoot string, rigsConfig *config.RigsConfig, opts AddRigOptions, localRepo string, verifyIdentity func() error) error {
	rigsConfig.Rigs[opts.Name] = config.RigEntry{
		GitURL:      opts.GitURL,
		PushURL:     opts.PushURL,
		UpstreamURL: opts.UpstreamURL,
		LocalRepo:   localRepo,
		AddedAt:     time.Now(),
		BeadsConfig: &config.BeadsConfig{Prefix: opts.BeadsPrefix},
	}
	if err := verifyIdentity(); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠ Identity verification warning: %v\n", err)
		fmt.Fprintf(os.Stderr, "  Run 'gt doctor --fix' to repair if needed.\n")
	}
	if err := config.SaveRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"), rigsConfig); err != nil {
		return fmt.Errorf("registering rig in rigs.json: %w", err)
	}
	return nil
}

func prepareAddRig(m *Manager, opts AddRigOptions) (preparedRigAdd, error) {
	if m.RigExists(opts.Name) {
		return preparedRigAdd{}, ErrRigExists
	}
	if err := validateNewRigName(opts.Name); err != nil {
		return preparedRigAdd{}, err
	}
	if err := requireAddRigDolt(m.townRoot, opts.SkipDoltCheck); err != nil {
		return preparedRigAdd{}, err
	}
	rigPath := filepath.Join(m.townRoot, opts.Name)
	if pathExists(rigPath) {
		return preparedRigAdd{}, fmt.Errorf("directory already exists: %s\n\nTo adopt an existing directory, use --adopt:\n  gt rig add %s --adopt", rigPath, opts.Name)
	}
	userProvidedPrefix := opts.BeadsPrefix != ""
	opts.BeadsPrefix = strings.TrimSuffix(opts.BeadsPrefix, "-")
	if opts.BeadsPrefix == "" {
		opts.BeadsPrefix = deriveBeadsPrefix(opts.Name)
	}
	if err := beads.CheckPrefixAvailable(m.townRoot, opts.BeadsPrefix+"-", opts.Name); err != nil {
		return preparedRigAdd{}, prefixCollisionError(opts.BeadsPrefix, err)
	}
	localRepo, warning := resolveLocalRepo(opts.LocalRepo, opts.GitURL)
	return preparedRigAdd{opts, rigPath, localRepo, warning, userProvidedPrefix}, nil
}

func validateNewRigName(name string) error {
	if strings.ContainsAny(name, "-. /\\") {
		sanitized := strings.NewReplacer("-", "_", ".", "_", " ", "_", "/", "_", "\\", "_").Replace(name)
		sanitized = strings.ToLower(strings.TrimLeft(sanitized, "_"))
		return fmt.Errorf("rig name %q contains invalid characters; hyphens, dots, spaces, and path separators are not allowed. Try %q instead (underscores are allowed)", name, sanitized)
	}
	if IsReservedName(name) {
		return fmt.Errorf("rig name %q is reserved for town-level infrastructure", name)
	}
	return nil
}

func requireAddRigDolt(townRoot string, skip bool) error {
	if skip {
		return nil
	}
	running, _, err := doltserver.IsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking Dolt server: %w", err)
	}
	if !running {
		return fmt.Errorf("Dolt server is not running (required for beads init); start it with 'gt up' or 'gt dolt start'")
	}
	return nil
}

func prefixCollisionError(prefix string, err error) error {
	if errors.Is(err, beads.ErrPrefixInUse) {
		return fmt.Errorf("prefix collision (derived prefix %q): %w; use --prefix to specify a different prefix", prefix, err)
	}
	return fmt.Errorf("prefix collision (derived prefix %q): %w", prefix, err)
}

// addOwnershipStampFile marks which AddRig invocation currently owns the path.
const addOwnershipStampFile = ".gt-add-owner"

func newAddOwnershipStamp() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func writeAddOwnershipStamp(rigPath, stamp string) error {
	return os.WriteFile(filepath.Join(rigPath, addOwnershipStampFile), []byte(stamp), 0644)
}

func readAddOwnershipStamp(rigPath string) string {
	data, err := os.ReadFile(filepath.Join(rigPath, addOwnershipStampFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func clearAddOwnershipStamp(rigPath string) error {
	return os.Remove(filepath.Join(rigPath, addOwnershipStampFile))
}

// removeRigPathIfOwned only removes a path if this AddRig invocation still owns
// it. Missing stamps are only removed when the directory is empty, which avoids
// deleting a later successful re-add that already cleared its own stamp.
func removeRigPathIfOwned(rigPath, expectedStamp string) {
	if expectedStamp == "" {
		_ = os.RemoveAll(rigPath)
		return
	}

	onDisk := readAddOwnershipStamp(rigPath)
	if onDisk == expectedStamp {
		_ = os.RemoveAll(rigPath)
		return
	}

	if onDisk == "" {
		if entries, err := os.ReadDir(rigPath); err == nil && len(entries) == 0 {
			_ = os.RemoveAll(rigPath)
			return
		}
		fmt.Fprintf(os.Stderr,
			"  Warning: skipping rollback of %s because ownership stamp is missing and directory is non-empty (gh#3683 protection)\n",
			rigPath)
		return
	}

	fmt.Fprintf(os.Stderr,
		"  Warning: skipping rollback of %s because another rig add now owns this path (gh#3683 protection)\n",
		rigPath)
}

// verifyRigIdentity checks that metadata.json points to the correct Dolt database
// for this rig. This catches identity mismatches early — before polecats are spawned
// and get stuck in retry loops. (gas-tc4)
func verifyRigIdentity(townRoot, rigPath, rigName string) error {
	resolvedBeadsDir := beads.ResolveBeadsDir(rigPath)
	metadataPath := filepath.Join(resolvedBeadsDir, "metadata.json")

	data, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No metadata.json yet — will be created later
		}
		return fmt.Errorf("reading metadata.json: %w", err)
	}

	var metadata struct {
		DoltDatabase string `json:"dolt_database"`
		DoltMode     string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return fmt.Errorf("parsing metadata.json: %w", err)
	}

	if metadata.DoltMode != "server" {
		return nil // Not using server mode, skip check
	}

	// Verify the database name matches what we expect.
	// The database should be named after the rig (e.g., "gastown") not after
	// a bd init artifact (e.g., "beads_gt") or a stale value from another rig.
	if metadata.DoltDatabase != "" && metadata.DoltDatabase != rigName {
		fmt.Fprintf(os.Stderr, "   ⚠ metadata.json has dolt_database=%q (expected %q) — attempting repair\n",
			metadata.DoltDatabase, rigName)
		if repairErr := doltserver.EnsureMetadata(townRoot, rigName); repairErr != nil {
			return fmt.Errorf("metadata.json has dolt_database=%q (expected %q) and auto-repair failed: %w",
				metadata.DoltDatabase, rigName, repairErr)
		}
		fmt.Printf("   ✓ Repaired metadata.json identity (was %q, now %q)\n", metadata.DoltDatabase, rigName)
	}

	return nil
}

// saveRigConfig writes the rig configuration to config.json.
func (m *Manager) saveRigConfig(rigPath string, cfg *RigConfig) error {
	configPath := filepath.Join(rigPath, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

// LoadRigConfig reads the rig configuration from config.json.
func LoadRigConfig(rigPath string) (*RigConfig, error) {
	configPath := filepath.Join(rigPath, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg RigConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	warnDeprecatedRigConfigKeys(data, configPath)
	return &cfg, nil
}

// warnDeprecatedRigConfigKeys detects merge_queue keys in rig root config.json
// that are silently ignored by json.Unmarshal (RigConfig has no merge_queue field).
// Without this warning, users can set merge_queue.target_branch believing it
// controls MR targets, while gt mq submit / gt done actually use default_branch.
func warnDeprecatedRigConfigKeys(data []byte, path string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	if mq, ok := raw["merge_queue"]; ok {
		var mqMap map[string]json.RawMessage
		if json.Unmarshal(mq, &mqMap) == nil {
			if _, has := mqMap["target_branch"]; has {
				fmt.Fprintf(os.Stderr, "WARNING: %s: merge_queue.target_branch is deprecated and ignored — set default_branch instead\n", path)
			}
		}
	}
}

// dropRigOrphanDBs drops orphan Dolt databases that bd init creates as a side
// effect of `bd init --prefix <prefix>`. The orphan name depends on bd version:
//
//	bd >= 0.62: "<prefix>"        (e.g. "ma" for prefix=ma)
//	bd <  0.62: "beads_<prefix>"  (e.g. "beads_ma")
//
// Either form must be removed when it differs from <rigName>, otherwise beads
// created from the rig can silently land in the orphan DB while the mayor
// reads from <rigName> — causing the silent data split documented in gh#3562.
//
// On entry the orphan is freshly created by bd init and contains only schema
// tables, so it is safe to force-drop. If the candidate looks like a real rig
// database (matches rigName, "hq", or doesn't exist at all) it is skipped.
//
// Returns an error only if at least one orphan candidate exists on disk and
// cannot be removed — callers in AddRig treat that as fatal so the user is not
// left with a silently-split rig.
func dropRigOrphanDBs(townRoot, prefix, rigName string) error {
	if prefix == "" || rigName == "" {
		return nil
	}
	candidates := []string{prefix, "beads_" + prefix}
	var failures []string
	for _, name := range candidates {
		if failure := removeRigOrphanCandidate(townRoot, name, rigName); failure != "" {
			failures = append(failures, failure)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("orphan database(s) for rig %q (prefix %q) could not be removed: %s — run `gt dolt cleanup --force` to resolve",
			rigName, prefix, strings.Join(failures, "; "))
	}
	return nil
}

func removeRigOrphanCandidate(townRoot, name, rigName string) string {
	if name == rigName || name == "hq" || !doltserver.DatabaseExists(townRoot, name) {
		return ""
	}
	if err := doltserver.RemoveDatabase(townRoot, name, true); err != nil {
		return fmt.Sprintf("%s: %v", name, err)
	}
	// Re-check: RemoveDatabase may report success but leave files behind
	// in pathological cases (read-only server, partial DROP).
	if doltserver.DatabaseExists(townRoot, name) {
		return fmt.Sprintf("%s: still present after RemoveDatabase", name)
	}
	return ""
}

// RigBeadsInitOptions controls InitializeRigBeads.
type RigBeadsInitOptions struct {
	// SkipDoltCheck skips creating the server-side Dolt database. Tests use this
	// when a fake bd is on PATH.
	SkipDoltCheck bool
	// Quiet suppresses progress prints. Warnings still go to stderr.
	Quiet bool
}

func printfBeads(opts RigBeadsInitOptions, format string, args ...any) {
	if opts.Quiet {
		return
	}
	fmt.Printf(format, args...)
}

func warnBeads(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}

func rigBeadsInitialized(rigPath string) bool {
	_, err := os.Stat(filepath.Join(beads.ResolveBeadsDir(rigPath), "metadata.json"))
	return err == nil
}

// InitializeRigBeads creates the rig Dolt database, local .beads, issue prefix,
// town route, and PRIME.md, so `bd -C <town>/<rig> create` files a
// rig-prefixed bead instead of hq-*.
func (m *Manager) InitializeRigBeads(rigPath, name, prefix string, opts RigBeadsInitOptions) error {
	return initializeRigBeads(m, rigPath, name, prefix, opts)
}

func initializeRigBeads(m *Manager, rigPath, name, prefix string, opts RigBeadsInitOptions) error {
	if err := checkRigBeadsPrefix(m.townRoot, name, prefix); err != nil {
		return err
	}
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := beads.EnsureDir(beadsDir); err != nil {
		return err
	}
	fl := flock.New(filepath.Join(beadsDir, "init.lock"))
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("locking rig beads init for %s: %w", name, err)
	}
	defer func() { _ = fl.Unlock() }()
	return initializeRigBeadsLocked(m, rigPath, name, prefix, opts)
}

func initializeRigBeadsLocked(m *Manager, rigPath, name, prefix string, opts RigBeadsInitOptions) error {
	already := rigBeadsInitialized(rigPath)
	if already {
		return m.appendRigRoute(rigPath, name, prefix)
	}
	if err := ensureRigDoltDatabase(m.townRoot, name, already, opts); err != nil {
		return err
	}
	if err := initializeNewRigBeads(m, rigPath, name, prefix, already, opts); err != nil {
		return err
	}
	resolvedBeadsDir := configureRigBeads(rigPath, prefix)
	if err := dropRigOrphanDBs(m.townRoot, prefix, name); err != nil {
		return fmt.Errorf("rig init left a duplicate Dolt database: %w", err)
	}
	return finalizeRigBeads(m, rigPath, name, prefix, resolvedBeadsDir)
}

func finalizeRigBeads(m *Manager, rigPath, name, prefix, resolvedBeadsDir string) error {
	if err := beads.ProvisionPrimeMD(resolvedBeadsDir); err != nil {
		warnBeads("  Warning: Could not provision PRIME.md: %v\n", err)
	}
	if err := m.appendRigRoute(rigPath, name, prefix); err != nil {
		return err
	}
	if err := m.initAgentBeads(rigPath, name, prefix); err != nil {
		warnBeads("  Warning: Could not create agent beads: %v\n", err)
	}
	return nil
}

func checkRigBeadsPrefix(townRoot, name, prefix string) error {
	if prefix == "" {
		return nil
	}
	if err := beads.CheckPrefixAvailable(townRoot, prefix+"-", name); err != nil {
		if errors.Is(err, beads.ErrPrefixInUse) {
			return fmt.Errorf("prefix collision (derived prefix %q): %w; use --prefix to specify a different prefix", prefix, err)
		}
		return fmt.Errorf("prefix collision (derived prefix %q): %w", prefix, err)
	}
	return nil
}

func ensureRigDoltDatabase(townRoot, name string, already bool, opts RigBeadsInitOptions) error {
	if already || opts.SkipDoltCheck {
		return nil
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		return nil
	}
	if _, _, err := doltserver.InitRig(townRoot, name); err != nil {
		warnBeads("  Warning: Could not create rig database: %v\n", err)
	}
	return nil
}

func initializeNewRigBeads(m *Manager, rigPath, name, prefix string, already bool, opts RigBeadsInitOptions) error {
	if already {
		return nil
	}
	printfBeads(opts, "  Initializing beads database...\n")
	if err := m.InitBeads(rigPath, prefix, name); err != nil {
		return fmt.Errorf("initializing beads: %w", err)
	}
	printfBeads(opts, "   ✓ Initialized beads (prefix: %s)\n", prefix)
	if err := doltserver.EnsureMetadata(m.townRoot, name); err != nil {
		warnBeads("  Warning: Could not set Dolt server metadata: %v\n", err)
		warnBeads("  Run 'gt doctor --fix' to repair, or it will self-heal on next daemon start.\n")
	}
	return nil
}

func configureRigBeads(rigPath, prefix string) string {
	rigRootBeadsDir := filepath.Join(rigPath, ".beads")
	resolvedBeadsDir := beads.ResolveBeadsDir(rigRootBeadsDir)
	if _, err := os.Stat(filepath.Join(rigRootBeadsDir, "redirect")); os.IsNotExist(err) {
		if err := beads.EnsureConfigYAMLValue(resolvedBeadsDir, "issue-prefix", prefix); err != nil {
			warnBeads("  Warning: Could not set issue-prefix in config.yaml: %v\n", err)
		}
		_ = beads.EnsureConfigYAMLValue(resolvedBeadsDir, "types.custom", constants.BeadsCustomTypes)
		_ = beads.EnsureConfigYAMLValue(resolvedBeadsDir, "types.infra", constants.BeadsInfraTypes)
		if err := beads.EnsureConfigYAMLValue(resolvedBeadsDir, "beads.role", "maintainer"); err != nil {
			warnBeads("  Warning: Could not set beads.role in config.yaml: %v\n", err)
		}
	}
	if err := beads.EnsureDoltConfigValue(resolvedBeadsDir, "issue_prefix", prefix); err != nil {
		warnBeads("  Warning: Could not set issue_prefix in rig database: %v\n", err)
	}
	_ = beads.EnsureDoltConfigValue(resolvedBeadsDir, "types.custom", constants.BeadsCustomTypes)
	_ = beads.EnsureDoltConfigValue(resolvedBeadsDir, "types.infra", constants.BeadsInfraTypes)
	return resolvedBeadsDir
}

func (m *Manager) appendRigRoute(rigPath, name, prefix string) error {
	return appendRigRoute(m.townRoot, rigPath, name, prefix)
}

func appendRigRoute(townRoot, rigPath, name, prefix string) error {
	if prefix == "" {
		return nil
	}
	routePath := name
	if _, err := os.Stat(filepath.Join(rigPath, "mayor", "rig", ".beads")); err == nil {
		routePath = name + "/mayor/rig"
	}
	route := beads.Route{
		Prefix: prefix + "-",
		Path:   routePath,
	}
	if err := beads.AppendRoute(townRoot, route); err != nil {
		warnBeads("  Warning: Could not update routes.jsonl: %v\n", err)
	}
	return nil
}

// InitBeads initializes the beads database at rig level.
// The project's .beads/config.yaml determines sync-branch settings.
// Use `bd doctor --fix` in the project to configure sync-branch if needed.
// TODO(bd-yaml): beads config should migrate to JSON (see beads issue)
//
// rigName is the rig's database name (e.g. "gastown"). When non-empty and
// different from the database that `bd init --prefix` creates (named "<prefix>"
// on bd >= 0.62 or "beads_<prefix>" on older bd), InitBeads drops the orphan
// to prevent the silent data split documented in gh#3562.
func (m *Manager) InitBeads(rigPath, prefix, rigName string) error {
	return initBeads(m.townRoot, rigPath, prefix, rigName)
}

func initBeads(townRoot, rigPath, prefix, rigName string) error {
	if !isValidBeadsPrefix(prefix) {
		return fmt.Errorf("invalid beads prefix %q: must be alphanumeric with optional hyphens, start with letter, max 20 chars", prefix)
	}
	beadsDir := filepath.Join(rigPath, ".beads")
	redirected, err := createTrackedBeadsRedirect(rigPath, beadsDir)
	if err != nil || redirected {
		return err
	}
	if err := beads.EnsureDir(beadsDir); err != nil {
		return err
	}
	filteredEnv := bdSubprocessEnv(beadsDir, rigName)
	cmd := beads.Spawn(rigBeadsInitArgs(townRoot, prefix, rigName)...)
	cmd.Dir = rigPath
	cmd.Env = filteredEnv
	_, bdInitErr := cmd.CombinedOutput()
	if err := beads.EnsureDir(beadsDir); err != nil {
		return err
	}
	if bdInitErr == nil {
		if err := configureInitializedRigBeads(townRoot, rigPath, prefix, rigName, filteredEnv); err != nil {
			return err
		}
	}
	return completeRigBeadsFiles(townRoot, rigPath, beadsDir, prefix, rigName, filteredEnv)
}

func completeRigBeadsFiles(townRoot, rigPath, beadsDir, prefix, rigName string, env []string) error {
	if err := beads.EnsureConfigYAML(beadsDir, prefix); err != nil {
		return fmt.Errorf("ensuring config.yaml: %w", err)
	}
	if rigName != "" {
		if err := doltserver.EnsureMetadataForBeadsDir(townRoot, beadsDir, rigName, rigName); err != nil {
			return fmt.Errorf("ensuring metadata.json: %w", err)
		}
	}
	migrateCmd := beads.Spawn("migrate", "--update-repo-id")
	migrateCmd.Dir = rigPath
	migrateCmd.Env = env
	// Ignore errors - fingerprint is optional for functionality
	_, _ = migrateCmd.CombinedOutput()

	// NOTE: We intentionally do NOT create routes.jsonl in rig beads.
	// bd's routing walks up to find town root (via mayor/town.json) and uses
	// town-level routes.jsonl for prefix-based routing. Rig-level routes.jsonl
	// would prevent this walk-up and break cross-rig routing.
	return nil
}

func createTrackedBeadsRedirect(rigPath, beadsDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(rigPath, "mayor", "rig", ".beads")); err != nil {
		return false, nil
	}
	if err := beads.EnsureDir(beadsDir); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		return false, fmt.Errorf("creating redirect file: %w", err)
	}
	return true, nil
}

func rigBeadsInitArgs(townRoot, prefix, rigName string) []string {
	args := []string{"init"}
	if prefix != "" {
		args = append(args, "--prefix", prefix)
	}
	if rigName != "" {
		args = append(args, "--database", rigName)
	}
	return append(args, "--server", "--server-port", strconv.Itoa(bdInitServerPort(townRoot)), "--force")
}

func configureInitializedRigBeads(townRoot, rigPath, prefix, rigName string, env []string) error {
	for _, cfg := range []struct{ key, value string }{
		{"types.custom", constants.BeadsCustomTypes},
		{"types.infra", constants.BeadsInfraTypes},
	} {
		cmd := beads.Spawn("config", "set", cfg.key, cfg.value)
		cmd.Dir, cmd.Env = rigPath, env
		_, _ = cmd.CombinedOutput()
	}
	prefixCmd := beads.Spawn("config", "set", "issue_prefix", prefix)
	prefixCmd.Dir, prefixCmd.Env = rigPath, env
	if output, err := prefixCmd.CombinedOutput(); err != nil && !strings.Contains(strings.TrimSpace(string(output)), "cannot be set via") {
		return fmt.Errorf("bd config set issue_prefix failed: %s", strings.TrimSpace(string(output)))
	}
	if rigName != "" {
		if err := dropRigOrphanDBs(townRoot, prefix, rigName); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: orphan database cleanup: %v\n", err)
		}
	}
	return nil
}

// initAgentBeads creates rig-level agent beads for Witness and Refinery.
// These agents use the rig's beads prefix and are stored in rig beads.
//
// Town-level agents (Mayor, Deacon) are created by gt install in town beads.
// Role beads are also created by gt install with hq- prefix.
//
// Rig-level agents (Witness, Refinery) are created here in rig beads with rig prefix.
// Format: <prefix>-<rig>-<role> (e.g., pi-pixelforge-witness)
//
// Agent beads track lifecycle state for ZFC compliance (gt-h3hak, gt-pinkq).
func (m *Manager) initAgentBeads(rigPath, rigName, prefix string) error {
	return initAgentBeads(rigPath, rigName, prefix)
}

func initAgentBeads(rigPath, rigName, prefix string) error {
	// Rig-level agents go in rig beads with rig prefix (per docs/architecture.md).
	// Town-level agents (Mayor, Deacon) are created by gt install in town beads.
	// Use ResolveBeadsDir to follow redirect files for tracked beads.
	rigBeadsDir := beads.ResolveBeadsDir(rigPath)
	bd := beads.NewWithBeadsDir(rigPath, rigBeadsDir)

	// Define rig-level agents to create
	type agentDef struct {
		id       string
		roleType string
		rig      string
		desc     string
	}

	// Create rig-specific agents using rig prefix in rig beads.
	// Format: <prefix>-<rig>-<role> (e.g., pi-pixelforge-witness)
	agents := []agentDef{
		{
			id:       beads.WitnessBeadIDWithPrefix(prefix, rigName),
			roleType: "witness",
			rig:      rigName,
			desc:     fmt.Sprintf("Witness for %s - monitors polecat health and progress.", rigName),
		},
		{
			id:       beads.RefineryBeadIDWithPrefix(prefix, rigName),
			roleType: "refinery",
			rig:      rigName,
			desc:     fmt.Sprintf("Refinery for %s - processes merge queue.", rigName),
		},
	}

	// Note: Mayor and Deacon are now created by gt install in town beads.

	for _, agent := range agents {
		// Check if already exists
		if _, err := bd.Show(agent.id); err == nil {
			continue // Already exists
		}

		// Note: RoleBead field removed - role definitions are now config-based
		fields := &beads.AgentFields{
			RoleType:   agent.roleType,
			Rig:        agent.rig,
			AgentState: "idle",
			HookBead:   "",
		}

		if _, err := bd.CreateAgentBead(agent.id, agent.desc, fields); err != nil {
			return fmt.Errorf("creating %s: %w", agent.id, err)
		}
		fmt.Printf("   ✓ Created agent bead: %s\n", agent.id)
	}

	return nil
}

// ensureGitignoreEntry adds an entry to .gitignore if it doesn't already exist.
func ensureGitignoreEntry(gitignorePath, entry string) error {
	// Read existing content
	content, err := os.ReadFile(gitignorePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Check if entry already exists
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == entry {
			return nil // Already present
		}
	}

	// Append entry
	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644) //nolint:gosec // G302: .gitignore should be readable by git tools
	if err != nil {
		return err
	}
	defer f.Close()

	// Add newline before if file doesn't end with one
	if len(content) > 0 && content[len(content)-1] != '\n' {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(entry + "\n")
	return err
}

// DeriveBeadsPrefix generates a beads prefix from a rig name.
// Examples: "gastown" -> "gt", "my-project" -> "mp", "foo" -> "foo"
func DeriveBeadsPrefix(name string) string {
	return deriveBeadsPrefix(name)
}

func deriveBeadsPrefix(name string) string {
	// Strip path separators — callers should validate names, but be defensive
	name = filepath.Base(name)
	name = strings.TrimLeft(name, "/\\")

	// Remove common suffixes
	name = strings.TrimSuffix(name, "-py")
	name = strings.TrimSuffix(name, "-go")

	// Split on hyphens/underscores
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_'
	})

	// If single part, try camelCase splitting first (e.g., "myProject" -> "my" + "Project"),
	// then fall back to compound word detection (e.g., "gastown" -> "gas" + "town").
	if len(parts) == 1 {
		if camelParts := splitCamelCase(parts[0]); len(camelParts) >= 2 {
			parts = camelParts
		} else {
			parts = splitCompoundWord(parts[0])
		}
	}

	if len(parts) >= 2 {
		// Take first letter of each part: "gas-town" -> "gt"
		prefix := ""
		for _, p := range parts {
			if len(p) > 0 {
				prefix += string(p[0])
			}
		}
		return strings.ToLower(prefix)
	}

	// Single word: use first 2-3 chars
	if len(name) <= 3 {
		return strings.ToLower(name)
	}
	return strings.ToLower(name[:2])
}

// splitCompoundWord attempts to split a compound word into its components.
// Common suffixes like "town", "ville", "port" are detected to split
// compound names (e.g., "gastown" -> ["gas", "town"]).
func splitCompoundWord(word string) []string {
	word = strings.ToLower(word)

	// Common suffixes for compound place names
	suffixes := []string{"town", "ville", "port", "place", "land", "field", "wood", "ford"}

	for _, suffix := range suffixes {
		if strings.HasSuffix(word, suffix) && len(word) > len(suffix) {
			prefix := word[:len(word)-len(suffix)]
			if len(prefix) > 0 {
				return []string{prefix, suffix}
			}
		}
	}

	return []string{word}
}

// splitCamelCase splits a camelCase or PascalCase string into its word parts.
// Examples: "myProject" -> ["my", "Project"], "gasStation" -> ["gas", "Station"],
// "HTMLParser" -> ["HTML", "Parser"].
func splitCamelCase(s string) []string {
	if s == "" {
		return nil
	}
	var parts []string
	start := 0
	runes := []rune(s)
	runeCount := len(runes)
	for i := 1; i < runeCount; i++ {
		// Split when transitioning from lower to upper: "myProject" at 'P'
		if unicode.IsLower(runes[i-1]) && unicode.IsUpper(runes[i]) {
			parts = append(parts, string(runes[start:i]))
			start = i
		}
		// Split when transitioning from upper run to upper+lower: "HTMLParser" at 'P'
		if i >= 2 && unicode.IsUpper(runes[i-1]) && unicode.IsUpper(runes[i-2]) && unicode.IsLower(runes[i]) {
			parts = append(parts, string(runes[start:i-1]))
			start = i - 1
		}
	}
	parts = append(parts, string(runes[start:]))
	return parts
}

// detectBeadsPrefixFromConfig reads the issue prefix from a beads config.yaml file.
// Returns empty string if the file doesn't exist or doesn't contain a prefix.
//
// beadsPrefixRegexp validates beads prefix format: alphanumeric, may contain hyphens,
// must start with letter, max 20 chars. Prevents shell injection via config files.
var beadsPrefixRegexp = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,19}$`)

// isValidBeadsPrefix checks if a prefix is safe for use in shell commands.
// Prefixes must be alphanumeric (with optional hyphens), start with a letter,
// and be at most 20 characters. This prevents command injection from
// malicious config files.
func isValidBeadsPrefix(prefix string) bool {
	return beadsPrefixRegexp.MatchString(prefix)
}

func bdSubprocessEnv(beadsDir, database string) []string {
	base := os.Environ()
	if townRoot := beads.FindTownRoot(filepath.Dir(beads.ResolveBeadsDir(beadsDir))); townRoot != "" {
		base = config.NormalizeConfiguredDoltEnv(base, townRoot)
	}
	env := beads.BuildMutationPinnedBDEnv(base, beadsDir)
	if database != "" {
		env = beads.StripEnvKey(env, "BEADS_DOLT_SERVER_DATABASE")
		env = append(env, "BEADS_DOLT_SERVER_DATABASE="+database)
	}
	return env
}

func bdInitServerPort(townRoot string) int {
	if port := config.ResolveConfiguredDoltPort(townRoot); port > 0 {
		return port
	}
	return doltserver.DefaultPort
}

// isStandardBeadHash checks if a string looks like a standard 5-char bead hash.
// Regular bead IDs use a 5-character base32-encoded hash (e.g., "mawit", "z0ixd").
// This distinguishes regular issues from agent beads (suffix like "witness")
// and merge requests (10-char suffix).
func isStandardBeadHash(s string) bool {
	if len(s) != 5 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// bdDatabaseExists checks if a beads directory has an initialized database
// that is actually usable (not just tracked metadata from another workspace).
//
// For Dolt server mode, metadata.json may be tracked in git with dolt_database
// pointing to a database that doesn't exist on this Dolt server. In that case,
// we need to run bd init to create the server-side database.
func bdDatabaseExists(beadsDir string) bool {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return false
	}

	// Parse metadata to check if the referenced Dolt database actually exists.
	var meta struct {
		DoltMode     string `json:"dolt_mode"`
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return true // Can't parse — assume it exists (backward compat)
	}

	// For server mode, verify the database exists in .dolt-data/.
	// metadata.json may be tracked in git from another workspace where
	// the Dolt server had this database, but this is a fresh server.
	if meta.DoltMode == "server" && meta.DoltDatabase != "" {
		// Walk up from beadsDir to find the town root (.dolt-data lives there).
		townRoot := beads.FindTownRoot(filepath.Dir(beadsDir))
		if townRoot == "" {
			return true // Can't find town root — assume it exists
		}
		dbDir := filepath.Join(townRoot, ".dolt-data", meta.DoltDatabase)
		if _, err := os.Stat(dbDir); os.IsNotExist(err) {
			return false // Database doesn't exist on this server
		}
	}

	return true
}

// When adding a rig from a source repo that has .beads/ tracked in git (like a project
// that already uses beads for issue tracking), we need to use that project's existing
// prefix instead of generating a new one. Otherwise, the rig would have a mismatched
// prefix and routing would fail to find the existing issues.
func detectBeadsPrefixFromConfig(configPath string) string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}

	// Parse YAML-style config (simple line-by-line parsing)
	// Looking for "issue-prefix: <value>" or "prefix: <value>"
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Check for issue-prefix or prefix key
		for _, key := range []string{"issue-prefix:", "prefix:"} {
			if strings.HasPrefix(line, key) {
				value := strings.TrimSpace(strings.TrimPrefix(line, key))
				// Remove quotes if present
				value = strings.Trim(value, `"'`)
				if value != "" && isValidBeadsPrefix(value) {
					return strings.TrimSuffix(value, "-")
				}
			}
		}
	}

	return ""
}

// beadsConfigHasSyncRemote reports whether the given beads config.yaml contains
// a non-empty sync.remote entry. bd init blocks waiting for interactive
// confirmation when it detects this, so callers must pass --reinit-local
// --discard-remote --destroy-token to suppress the prompt. (GH #3873)
func beadsConfigHasSyncRemote(configPath string) bool {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "sync.remote:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "sync.remote:"))
			return strings.Trim(value, `"'`) != ""
		}
	}
	return false
}

// RemoveRig unregisters a rig (does not delete files).
func (m *Manager) RemoveRig(name string) error {
	if !m.RigExists(name) {
		return ErrRigNotFound
	}

	delete(m.config.Rigs, name)
	return nil
}

// ListRigNames returns the names of all registered rigs.
// RegisterRigOptions contains options for registering an existing rig directory.
type RegisterRigOptions struct {
	Name        string // Rig name (directory name)
	GitURL      string // Override git URL (auto-detected from origin if empty)
	PushURL     string // Override push URL (auto-detected from existing config/remotes if empty)
	UpstreamURL string // Upstream repository URL (for fork workflows)
	BeadsPrefix string // Beads issue prefix (defaults to derived from name or existing config)
	Force       bool   // Register even if directory structure looks incomplete
}

// RegisterRigResult contains the result of registering a rig.
type RegisterRigResult struct {
	Name          string // Rig name
	GitURL        string // Detected or provided git URL
	BeadsPrefix   string // Detected or derived beads prefix
	FromConfig    bool   // True if values were read from existing config.json
	DefaultBranch string // Default branch from existing config (if any)
}

// RegisterRig registers an existing rig directory with the town.
// Complementary to AddRig: while AddRig creates a new rig from scratch,
// RegisterRig adopts an existing directory structure.
func (m *Manager) RegisterRig(opts RegisterRigOptions) (*RegisterRigResult, error) {
	if m.RigExists(opts.Name) {
		return nil, ErrRigExists
	}
	rigPath, err := validateRigRegistrationPath(m.townRoot, opts.Name)
	if err != nil {
		return nil, err
	}
	result, existingConfig := loadRigRegistration(rigPath, opts)
	if err := resolveRigRegistration(m, rigPath, opts, result); err != nil {
		return nil, err
	}
	pushURL, authoritative := registrationPushURL(opts, existingConfig, detectPushURL(rigPath))
	if err := configureRegistrationRemotes(rigPath, pushURL, authoritative, opts.UpstreamURL); err != nil {
		return nil, err
	}
	syncRegistrationPushURL(rigPath, existingConfig, pushURL, m.saveRigConfig)
	m.config.Rigs[opts.Name] = config.RigEntry{
		GitURL:      result.GitURL,
		PushURL:     pushURL,
		UpstreamURL: opts.UpstreamURL,
		AddedAt:     time.Now(),
		BeadsConfig: &config.BeadsConfig{
			Prefix: result.BeadsPrefix,
		},
	}

	return result, nil
}

func validateRigRegistrationPath(townRoot, name string) (string, error) {
	if strings.ContainsAny(name, "-. /\\") {
		sanitized := strings.NewReplacer("-", "_", ".", "_", " ", "_", "/", "_", "\\", "_").Replace(name)
		sanitized = strings.ToLower(strings.TrimLeft(sanitized, "_"))
		return "", fmt.Errorf("rig name %q contains invalid characters; hyphens, dots, spaces, and path separators are not allowed. Try %q instead (underscores are allowed)", name, sanitized)
	}
	if IsReservedName(name) {
		return "", fmt.Errorf("rig name %q is reserved for town-level infrastructure", name)
	}
	rigPath := filepath.Join(townRoot, name)
	info, err := os.Stat(rigPath)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("directory does not exist: %s", rigPath)
	}
	if err != nil {
		return "", fmt.Errorf("checking directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", rigPath)
	}
	return rigPath, nil
}

func loadRigRegistration(rigPath string, opts RegisterRigOptions) (*RegisterRigResult, *RigConfig) {
	result := &RegisterRigResult{Name: opts.Name}
	cfg, err := LoadRigConfig(rigPath)
	if err != nil || cfg == nil {
		return result, nil
	}
	result.FromConfig = true
	if opts.GitURL == "" {
		result.GitURL = cfg.GitURL
	}
	if opts.BeadsPrefix == "" && cfg.Beads != nil {
		result.BeadsPrefix = cfg.Beads.Prefix
	}
	result.DefaultBranch = cfg.DefaultBranch
	return result, cfg
}

func resolveRigRegistration(m *Manager, rigPath string, opts RegisterRigOptions, result *RegisterRigResult) error {
	if result.GitURL == "" && opts.GitURL == "" {
		detected, err := detectGitURL(rigPath)
		if err != nil && !opts.Force {
			return fmt.Errorf("could not detect git URL (use --url to specify, or --force to skip): %w", err)
		}
		result.GitURL = detected
	}
	if opts.GitURL != "" {
		result.GitURL = opts.GitURL
	}
	return resolveRigRegistrationPrefix(m.townRoot, opts, result)
}

func resolveRigRegistrationPrefix(townRoot string, opts RegisterRigOptions, result *RegisterRigResult) error {
	if result.BeadsPrefix == "" && opts.BeadsPrefix == "" {
		result.BeadsPrefix = deriveBeadsPrefix(opts.Name)
	}
	if opts.BeadsPrefix != "" {
		result.BeadsPrefix = opts.BeadsPrefix
	}
	if err := beads.CheckPrefixAvailable(townRoot, result.BeadsPrefix+"-", opts.Name); err != nil {
		if errors.Is(err, beads.ErrPrefixInUse) {
			return fmt.Errorf("prefix collision (prefix %q): %w; use --prefix to specify a different prefix", result.BeadsPrefix, err)
		}
		return fmt.Errorf("prefix collision (prefix %q): %w", result.BeadsPrefix, err)
	}
	return nil
}

func registrationPushURL(opts RegisterRigOptions, cfg *RigConfig, detected string) (string, bool) {
	if opts.PushURL != "" {
		return opts.PushURL, true
	}
	if cfg != nil && cfg.PushURL != "" {
		return cfg.PushURL, true
	}
	return detected, false
}

func configureRegistrationRemotes(rigPath, pushURL string, authoritative bool, upstreamURL string) error {
	bareRepoPath := filepath.Join(rigPath, ".repo.git")
	mayorRigPath := filepath.Join(rigPath, "mayor", "rig")
	if err := configureRegistrationPushRemotes(bareRepoPath, mayorRigPath, pushURL, authoritative); err != nil {
		return err
	}
	return configureRegistrationUpstreamRemotes(bareRepoPath, mayorRigPath, upstreamURL)
}

func configureRegistrationPushRemotes(bareRepoPath, mayorRigPath, pushURL string, authoritative bool) error {
	if pushURL != "" {
		return applyRegistrationPushURL(bareRepoPath, mayorRigPath, pushURL)
	}
	if !authoritative {
		return nil
	}
	return clearRegistrationPushURL(bareRepoPath, mayorRigPath)
}

func applyRegistrationPushURL(bareRepoPath, mayorRigPath, pushURL string) error {
	if pathExists(bareRepoPath) {
		if err := git.ConfigurePushURL(git.NewGitWithDir(bareRepoPath, ""), "origin", pushURL); err != nil {
			return fmt.Errorf("configuring push URL on bare repo: %w", err)
		}
	}
	if pathExists(mayorRigPath) {
		if err := git.ConfigurePushURL(git.NewGit(mayorRigPath), "origin", pushURL); err != nil {
			return fmt.Errorf("configuring mayor push URL: %w", err)
		}
	}
	return nil
}

func clearRegistrationPushURL(bareRepoPath, mayorRigPath string) error {
	if pathExists(bareRepoPath) {
		if err := git.ClearPushURL(git.NewGitWithDir(bareRepoPath, ""), "origin"); err != nil {
			return fmt.Errorf("clearing stale push URL on bare repo: %w", err)
		}
	}
	if pathExists(mayorRigPath) {
		if err := git.ClearPushURL(git.NewGit(mayorRigPath), "origin"); err != nil {
			return fmt.Errorf("clearing stale mayor push URL: %w", err)
		}
	}
	return nil
}

func configureRegistrationUpstreamRemotes(bareRepoPath, mayorRigPath, upstreamURL string) error {
	if upstreamURL == "" {
		return nil
	}
	if pathExists(bareRepoPath) {
		if err := git.AddUpstreamRemote(git.NewGitWithDir(bareRepoPath, ""), upstreamURL); err != nil {
			return fmt.Errorf("configuring upstream remote on bare repo: %w", err)
		}
	}
	if pathExists(mayorRigPath) {
		if err := git.AddUpstreamRemote(git.NewGit(mayorRigPath), upstreamURL); err != nil {
			return fmt.Errorf("configuring mayor upstream remote: %w", err)
		}
	}
	return nil
}

func syncRegistrationPushURL(rigPath string, cfg *RigConfig, pushURL string, save func(string, *RigConfig) error) {
	if cfg == nil || cfg.PushURL == pushURL {
		return
	}
	cfg.PushURL = pushURL
	if err := save(rigPath, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not update config.json with push URL: %v\n", err)
	}
}

// detectPushURL attempts to detect a custom push URL from an existing repository.
// Returns empty string if push URL matches fetch URL (no custom push URL configured).
func detectPushURL(rigPath string) string {
	// Check bare repo first (polecat-preferred source of truth), then clones.
	// .repo.git is a bare repo and requires NewGitWithDir; the rest are regular clones.
	bareRepoPath := filepath.Join(rigPath, ".repo.git")
	if pushURL := detectPushURLFrom(git.NewGitWithDir(bareRepoPath, "")); pushURL != "" {
		return pushURL
	}

	clonePaths := []string{
		rigPath,
		filepath.Join(rigPath, "mayor", "rig"),
		filepath.Join(rigPath, "refinery", "rig"),
	}
	for _, p := range clonePaths {
		if pushURL := detectPushURLFrom(git.NewGit(p)); pushURL != "" {
			return pushURL
		}
	}
	return ""
}

// detectPushURLFrom checks a single git repo for a custom push URL.
func detectPushURLFrom(g *git.Git) string {
	fetchURL, fetchErr := git.RemoteURL(g, "origin")
	if fetchErr != nil {
		return ""
	}
	pushURL, pushErr := git.GetPushURL(g, "origin")
	if pushErr != nil || pushURL == "" {
		return ""
	}
	if strings.TrimSpace(pushURL) != strings.TrimSpace(fetchURL) {
		return strings.TrimSpace(pushURL)
	}
	return ""
}

// detectGitURL attempts to detect the git remote URL from an existing repository.
// detectGitURL finds the origin remote URL from available clones.
// Note: .repo.git is intentionally not checked here — it's a bare repo shared by worktrees
// and requires NewGitWithDir (not NewGit). detectPushURL checks .repo.git because push URL
// is primarily configured there. For git URL, the clone-based paths are authoritative.
func detectGitURL(rigPath string) (string, error) {
	possiblePaths := []string{
		rigPath,
		filepath.Join(rigPath, "mayor", "rig"),
		filepath.Join(rigPath, "refinery", "rig"),
	}
	for _, p := range possiblePaths {
		g := git.NewGit(p)
		url, err := git.RemoteURL(g, "origin")
		if err == nil && url != "" {
			return strings.TrimSpace(url), nil
		}
	}
	return "", fmt.Errorf("no git repository with origin remote found in %s", rigPath)
}

func (m *Manager) ListRigNames() []string {
	names := make([]string, 0, len(m.config.Rigs))
	for name := range m.config.Rigs {
		names = append(names, name)
	}
	return names
}

// seedPatrolMolecules creates patrol molecule prototypes in the rig's beads database.
// These molecules define the work loops for Deacon, Witness, and Refinery roles.
func (m *Manager) seedPatrolMolecules(rigPath string) error {
	// Use bd command to seed molecules (more reliable than internal API)
	cmd := beads.Spawn("mol", "seed", "--patrol")
	cmd.Dir = rigPath
	if err := cmd.Run(); err != nil {
		// Fallback: bd mol seed might not support --patrol yet
		// Try creating them individually via bd create
		return seedPatrolMoleculesManually(rigPath)
	}
	return nil
}

// seedPatrolMoleculesManually creates patrol molecules using bd create commands.
func seedPatrolMoleculesManually(rigPath string) error {
	// Patrol molecule definitions for seeding
	patrolMols := []struct {
		title string
		desc  string
	}{
		{
			title: "Deacon Patrol",
			desc:  "Mayor's daemon patrol loop for handling callbacks, health checks, and cleanup.",
		},
		{
			title: "Witness Patrol",
			desc:  "Per-rig worker monitor patrol loop with progressive nudging.",
		},
		{
			title: "Refinery Patrol",
			desc:  "Merge queue processor patrol loop with verification gates.",
		},
	}

	for _, mol := range patrolMols {
		// Check if already exists by title
		checkCmd := beads.Spawn("list", "--type=molecule", "--format=json")
		checkCmd.Dir = rigPath
		output, _ := checkCmd.Output()
		if strings.Contains(string(output), mol.title) {
			continue // Already exists
		}

		// Create the molecule
		cmd := beads.Spawn("create", //nolint:gosec // G204: bd is a trusted internal tool
			"--type=molecule",
			"--title="+mol.title,
			"--description="+mol.desc,
			"--priority=2",
		)
		cmd.Dir = rigPath
		if err := cmd.Run(); err != nil {
			// Non-fatal, continue with others
			continue
		}
	}
	return nil
}

// createPluginDirectories creates plugin directories at town and rig levels.
// - ~/gt/plugins/ (town-level, shared across all rigs)
// - <rig>/plugins/ (rig-level, rig-specific plugins)
func createPluginDirectories(townRoot, rigPath string) error {
	// Town-level plugins directory
	townPluginsDir := filepath.Join(townRoot, "plugins")
	if err := os.MkdirAll(townPluginsDir, 0755); err != nil {
		return fmt.Errorf("creating town plugins directory: %w", err)
	}

	// Create a README in town plugins if it doesn't exist
	townReadme := filepath.Join(townPluginsDir, "README.md")
	if _, err := os.Stat(townReadme); os.IsNotExist(err) {
		content := `# Gas Town Plugins

This directory contains town-level plugins that run during Deacon patrol cycles.

## Plugin Structure

Each plugin is a directory containing:
- plugin.md - Plugin definition with TOML frontmatter

## Gate Types

- cooldown: Time since last run (e.g., 24h)
- cron: Schedule-based (e.g., "0 9 * * *")
- condition: Metric threshold
- event: Trigger-based (startup, heartbeat)

See docs/deacon-plugins.md for full documentation.
`
		if writeErr := os.WriteFile(townReadme, []byte(content), 0644); writeErr != nil {
			// Non-fatal
			return nil
		}
	}

	// Rig-level plugins directory
	rigPluginsDir := filepath.Join(rigPath, "plugins")
	if err := os.MkdirAll(rigPluginsDir, 0755); err != nil {
		return fmt.Errorf("creating rig plugins directory: %w", err)
	}

	// Add Gas Town directories and config files to rig .gitignore so they
	// don't pollute the project repo. The rig container is not a git repo
	// itself, but this is a defensive measure against accidental git init
	// or future architecture changes.
	//
	// NOTE: No **/* wildcards — all GT runtime files live inside these
	// directories. Broad patterns like **/*.lock would catch project files
	// (yarn.lock, Cargo.lock, flake.lock, etc).
	gitignorePath := filepath.Join(rigPath, ".gitignore")
	gitignoreEntries := []string{
		// Existing patterns
		"plugins/",
		".repo.git/",
		".land-worktree/",
		// GT infrastructure directories
		".beads/",
		".claude/",
		".archive/",
		".runtime/",
		"crew/",
		"daemon/",
		"mayor/",
		"polecats/",
		"refinery/",
		"settings/",
		"witness/",
		// GT configuration files
		"config.json",
		"state.json",
		"AGENTS.md",
	}
	for _, entry := range gitignoreEntries {
		if err := ensureGitignoreEntry(gitignorePath, entry); err != nil {
			return err
		}
	}
	return nil
}

// scaffoldPolecatWorkspace writes polecat settings and slash-command files
// for the town default agent. Custom aliases inherit the provider's config dir.
func scaffoldPolecatWorkspace(townRoot, polecatsPath string) {
	townSettings, tsErr := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if tsErr != nil {
		townSettings = config.NewTownSettings()
	}
	defaultAgentName := townSettings.DefaultAgent
	if defaultAgentName == "" {
		defaultAgentName = string(config.AgentClaude)
	}
	defaultPreset := config.ResolveAgentPreset(defaultAgentName, townSettings, nil)
	if defaultPreset != nil && defaultPreset.HooksProvider != "" {
		if err := hooks.InstallForRole(defaultPreset.HooksProvider, polecatsPath, polecatsPath, "polecat",
			defaultPreset.HooksDir, defaultPreset.HooksSettingsFile, defaultPreset.HooksUseSettingsDir); err != nil {
			// Non-fatal: session startup will retry via EnsureSettingsForRole
			fmt.Printf("  %s Could not scaffold polecat settings: %v\n", "!", err)
		}
	}
	if err := commands.ProvisionForSettings(polecatsPath, defaultAgentName, townSettings); err != nil {
		// Non-fatal: commands are convenience, not critical
		fmt.Printf("  %s Could not scaffold polecat commands: %v\n", "!", err)
	}
}
