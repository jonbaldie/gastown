// Package wasteland implements the Wasteland federation protocol for Gas Town.
//
// The Wasteland is a federation of Gas Towns via DoltHub. Each rig has a
// sovereign fork of a shared commons database. Rigs register by writing
// to the commons' rigs table, and contribute wanted work items and
// completions through DoltHub's fork/PR/merge primitives.
//
// See ~/hop/docs/wasteland/design.md for the full design.
package wasteland

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/util"
)

// ErrNotJoined indicates the rig has not joined a wasteland.
var ErrNotJoined = errors.New("rig has not joined a wasteland")

// Config holds the wasteland configuration for a rig.
type Config struct {
	// Upstream is the DoltHub path of the upstream commons (e.g., "steveyegge/wl-commons").
	Upstream string `json:"upstream"`

	// ForkOrg is the DoltHub org where the fork lives (e.g., "alice-dev").
	ForkOrg string `json:"fork_org"`

	// ForkDB is the database name of the fork (e.g., "wl-commons").
	ForkDB string `json:"fork_db"`

	// LocalDir is the absolute path to the local clone of the fork.
	LocalDir string `json:"local_dir"`

	// RigHandle is the rig's handle in the registry.
	RigHandle string `json:"rig_handle"`

	// JoinedAt is when the town joined the wasteland.
	JoinedAt time.Time `json:"joined_at"`
}

// ConfigPath returns the path to the wasteland config file for a town.
func ConfigPath(townRoot string) string {
	return filepath.Join(townRoot, "mayor", "wasteland.json")
}

// LoadConfig loads the wasteland configuration from disk.
func LoadConfig(townRoot string) (*Config, error) {
	data, err := os.ReadFile(ConfigPath(townRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w (run 'gt wl join <upstream>')", ErrNotJoined)
		}
		return nil, fmt.Errorf("reading wasteland config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing wasteland config: %w", err)
	}
	return &cfg, nil
}

// SaveConfig writes the wasteland configuration to disk.
func SaveConfig(townRoot string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling wasteland config: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(ConfigPath(townRoot), data, 0644)
}

// dolthubAPIBase is the DoltHub REST API base URL.
// Var so tests can override it.
var dolthubAPIBase = "https://www.dolthub.com/api/v1alpha1"

// dolthubRemoteBase is the Dolt remote API base URL.
const dolthubRemoteBase = "https://doltremoteapi.dolthub.com"

// ParseUpstream parses an upstream path like "steveyegge/wl-commons" into org and db.
func ParseUpstream(upstream string) (org, db string, err error) {
	parts := strings.SplitN(upstream, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid upstream path %q: expected format 'org/database'", upstream)
	}
	return parts[0], parts[1], nil
}

// ForkDoltHubRepo forks a DoltHub database to the target org.
// Uses the DoltHub fork API endpoint.
func ForkDoltHubRepo(fromOrg, fromDB, toOrg, token string) error {
	body := map[string]string{
		"ownerName":          toOrg,
		"parentOwnerName":    fromOrg,
		"parentDatabaseName": fromDB,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling fork request: %w", err)
	}

	url := dolthubAPIBase + "/fork"
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating fork request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("authorization", "token "+token)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("DoltHub fork API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	// Parse error response
	var errResp struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if decErr := json.NewDecoder(resp.Body).Decode(&errResp); decErr == nil {
		// "already exists" is not an error — fork was already created
		if strings.Contains(strings.ToLower(errResp.Message), "already exists") {
			return nil
		}
		return fmt.Errorf("DoltHub fork API error (HTTP %d): %s", resp.StatusCode, errResp.Message)
	}
	return fmt.Errorf("DoltHub fork API error (HTTP %d)", resp.StatusCode)
}

// CloneLocally clones a DoltHub database to a local directory.
// Returns the absolute path to the clone.
func CloneLocally(org, db, targetDir string) error {
	remoteURL := fmt.Sprintf("%s/%s/%s", dolthubRemoteBase, org, db)

	if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// If directory already exists with a .dolt folder, skip clone
	if _, err := os.Stat(filepath.Join(targetDir, ".dolt")); err == nil {
		return nil
	}

	cmd := exec.Command("dolt", "clone", remoteURL, targetDir)
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("dolt clone %s: %w (%s)", remoteURL, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// RegisterRig inserts a row into the rigs table on the local clone.
// For Phase 1 (wild-west mode), writes directly to main.
func RegisterRig(localDir string, handle, dolthubOrg, displayName, ownerEmail, gtVersion string) error {
	sql := fmt.Sprintf(
		`INSERT INTO rigs (handle, display_name, dolthub_org, owner_email, gt_version, trust_level, registered_at, last_seen) `+
			`VALUES ('%s', '%s', '%s', '%s', '%s', 1, NOW(), NOW()) `+
			`ON DUPLICATE KEY UPDATE last_seen = NOW(), gt_version = '%s'`,
		escapeSQLString(handle),
		escapeSQLString(displayName),
		escapeSQLString(dolthubOrg),
		escapeSQLString(ownerEmail),
		escapeSQLString(gtVersion),
		escapeSQLString(gtVersion),
	)

	cmd := exec.Command("dolt", "sql", "-q", sql)
	cmd.Dir = localDir
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("inserting rig registration: %w (%s)", err, strings.TrimSpace(string(output)))
	}

	// Stage and commit
	addCmd := exec.Command("dolt", "add", ".")
	addCmd.Dir = localDir
	util.SetDetachedProcessGroup(addCmd)
	if output, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("dolt add: %w (%s)", err, strings.TrimSpace(string(output)))
	}

	commitCmd := exec.Command("dolt", "commit", "-m", fmt.Sprintf("Register rig: %s", handle))
	commitCmd.Dir = localDir
	util.SetDetachedProcessGroup(commitCmd)
	output, err = commitCmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "nothing to commit") || strings.Contains(lower, "no changes added") {
			return nil // already registered
		}
		return fmt.Errorf("dolt commit: %w (%s)", err, msg)
	}

	return nil
}

// PushToOrigin pushes the local clone to origin main.
func PushToOrigin(localDir string) error {
	cmd := exec.Command("dolt", "push", "origin", "main")
	cmd.Dir = localDir
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("dolt push: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// AddUpstreamRemote adds the upstream commons as a remote named "upstream".
func AddUpstreamRemote(localDir, upstreamOrg, upstreamDB string) error {
	url := fmt.Sprintf("%s/%s/%s", dolthubRemoteBase, upstreamOrg, upstreamDB)

	// Check if upstream remote already exists
	checkCmd := exec.Command("dolt", "remote", "-v")
	checkCmd.Dir = localDir
	util.SetDetachedProcessGroup(checkCmd)
	output, err := checkCmd.CombinedOutput()
	if err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "upstream") {
				return nil // already exists
			}
		}
	}

	cmd := exec.Command("dolt", "remote", "add", "upstream", url)
	cmd.Dir = localDir
	util.SetDetachedProcessGroup(cmd)
	output, err = cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if strings.Contains(strings.ToLower(msg), "already exists") {
			return nil
		}
		return fmt.Errorf("dolt remote add upstream: %w (%s)", err, msg)
	}
	return nil
}

// WastelandDir returns the directory where wasteland data is stored for a town.
func WastelandDir(townRoot string) string {
	return filepath.Join(townRoot, ".wasteland")
}

// LocalCloneDir returns the local clone directory for a specific wasteland commons.
func LocalCloneDir(townRoot, upstreamOrg, upstreamDB string) string {
	return filepath.Join(WastelandDir(townRoot), upstreamOrg, upstreamDB)
}

// ResolveDBName returns the Dolt database name for the wasteland.
// It derives the name from the config's ForkDB field (replacing hyphens with
// underscores, since Dolt maps database directory names that way).
// Falls back to "wl_commons" if no config is found.
func ResolveDBName(townRoot string) string {
	cfg, err := LoadConfig(townRoot)
	if err != nil || cfg.ForkDB == "" {
		return "wl_commons"
	}
	// Dolt maps directory names to database names by replacing hyphens with underscores.
	return strings.ReplaceAll(cfg.ForkDB, "-", "_")
}

// escapeSQLString escapes backslashes and single quotes for SQL string literals.
func escapeSQLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "'", "''")
}

// DoltHubAPI abstracts DoltHub REST API operations.
type DoltHubAPI interface {
	ForkRepo(_, _, _, _ string) error
}

// DoltCLI abstracts dolt CLI subprocess operations.
type DoltCLI interface {
	Clone(_, _, _ string) error
	RegisterRig(_, _, _, _, _, _ string) error
	Push(_ string) error
	AddUpstreamRemote(_, _, _ string) error
}

// ConfigStore abstracts wasteland config persistence.
type ConfigStore interface {
	Load(_ string) (*Config, error)
	Save(_ string, _ *Config) error
}

// Service coordinates wasteland operations with injectable dependencies.
type Service struct {
	API        DoltHubAPI
	CLI        DoltCLI
	Config     ConfigStore
	OnProgress func(step string) // optional callback for progress reporting
}

// Join orchestrates the wasteland join workflow: fork -> clone -> add upstream -> register -> push -> save config.
// Returns the saved Config on success, or the existing Config if already joined.
func (s *Service) Join(upstream, forkOrg, token, handle, displayName, ownerEmail, gtVersion, townRoot string) (*Config, error) {
	upstreamOrg, upstreamDB, err := ParseUpstream(upstream)
	if err != nil {
		return nil, err
	}

	if existing, joined, err := existingJoin(s.Config, townRoot, upstream); err != nil {
		return nil, err
	} else if joined {
		return existing, nil
	}

	localDir := LocalCloneDir(townRoot, upstreamOrg, upstreamDB)
	progress := s.OnProgress
	if progress == nil {
		progress = func(string) {}
	}

	if err := s.runJoinSteps(progress, joinInputs{
		upstreamOrg: upstreamOrg,
		upstreamDB:  upstreamDB,
		forkOrg:     forkOrg,
		token:       token,
		handle:      handle,
		displayName: displayName,
		ownerEmail:  ownerEmail,
		gtVersion:   gtVersion,
		localDir:    localDir,
	}); err != nil {
		return nil, err
	}

	cfg := &Config{
		Upstream:  upstream,
		ForkOrg:   forkOrg,
		ForkDB:    upstreamDB,
		LocalDir:  localDir,
		RigHandle: handle,
		JoinedAt:  time.Now(),
	}
	if err := s.Config.Save(townRoot, cfg); err != nil {
		return nil, fmt.Errorf("saving wasteland config: %w", err)
	}

	return cfg, nil
}

func existingJoin(config ConfigStore, townRoot, upstream string) (*Config, bool, error) {
	existing, err := config.Load(townRoot)
	if err == nil {
		if existing.Upstream != upstream {
			return nil, false, fmt.Errorf("already joined to %s; run gt wl leave first", existing.Upstream)
		}
		return existing, true, nil
	}
	if errors.Is(err, ErrNotJoined) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("loading wasteland config: %w", err)
}

type joinStep struct {
	message string
	name    string
	run     func() error
}

type joinInputs struct {
	upstreamOrg string
	upstreamDB  string
	forkOrg     string
	token       string
	handle      string
	displayName string
	ownerEmail  string
	gtVersion   string
	localDir    string
}

func (s *Service) runJoinSteps(progress func(string), inputs joinInputs) error {
	steps := []joinStep{
		{
			message: "Forking commons...",
			name:    "forking commons",
			run: func() error {
				return s.API.ForkRepo(inputs.upstreamOrg, inputs.upstreamDB, inputs.forkOrg, inputs.token)
			},
		},
		{
			message: "Cloning fork locally...",
			name:    "cloning fork",
			run:     func() error { return s.CLI.Clone(inputs.forkOrg, inputs.upstreamDB, inputs.localDir) },
		},
		{
			message: "Adding upstream remote...",
			name:    "adding upstream remote",
			run:     func() error { return s.CLI.AddUpstreamRemote(inputs.localDir, inputs.upstreamOrg, inputs.upstreamDB) },
		},
		{
			message: "Registering rig...",
			name:    "registering rig",
			run: func() error {
				return s.CLI.RegisterRig(inputs.localDir, inputs.handle, inputs.forkOrg, inputs.displayName, inputs.ownerEmail, inputs.gtVersion)
			},
		},
		{
			message: "Pushing to fork...",
			name:    "pushing to fork",
			run:     func() error { return s.CLI.Push(inputs.localDir) },
		},
	}
	for _, step := range steps {
		progress(step.message)
		if err := step.run(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

// httpDoltHubAPI implements DoltHubAPI using the real DoltHub REST API.
type httpDoltHubAPI struct{}

func (h *httpDoltHubAPI) ForkRepo(fromOrg, fromDB, toOrg, token string) error {
	return ForkDoltHubRepo(fromOrg, fromDB, toOrg, token)
}

// execDoltCLI implements DoltCLI using real dolt subprocess calls.
type execDoltCLI struct{}

func (e *execDoltCLI) Clone(org, db, targetDir string) error {
	return CloneLocally(org, db, targetDir)
}
func (e *execDoltCLI) RegisterRig(localDir, handle, dolthubOrg, displayName, ownerEmail, gtVersion string) error {
	return RegisterRig(localDir, handle, dolthubOrg, displayName, ownerEmail, gtVersion)
}
func (e *execDoltCLI) Push(localDir string) error {
	return PushToOrigin(localDir)
}
func (e *execDoltCLI) AddUpstreamRemote(localDir, upstreamOrg, upstreamDB string) error {
	return AddUpstreamRemote(localDir, upstreamOrg, upstreamDB)
}

// fileConfigStore implements ConfigStore using filesystem persistence.
type fileConfigStore struct{}

func (f *fileConfigStore) Load(townRoot string) (*Config, error) {
	return LoadConfig(townRoot)
}
func (f *fileConfigStore) Save(townRoot string, cfg *Config) error {
	return SaveConfig(townRoot, cfg)
}

// NewService creates a Service with real (production) dependencies.
func NewService() *Service {
	return &Service{
		API:    &httpDoltHubAPI{},
		CLI:    &execDoltCLI{},
		Config: &fileConfigStore{},
	}
}
