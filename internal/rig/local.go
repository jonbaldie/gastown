package rig

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/git"
)

type localRigSetup struct {
	absSrc  string
	gitURL  string
	prefix  string
	rigPath string
	srcGit  *git.Git
}

// AddLocalRig registers a local git repository as a rig without a network clone.
// Polecat worktrees later share objects from a local bare clone of srcRepo.
func (m *Manager) AddLocalRig(ctx context.Context, name, srcRepo string) (*Rig, error) {
	setup, err := m.prepareLocalRig(name, srcRepo)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(setup.rigPath, 0755); err != nil {
		return nil, fmt.Errorf("creating rig directory: %w", err)
	}
	success := false
	defer func() {
		if success {
			return
		}
		_ = os.RemoveAll(setup.rigPath)
		if _, ok := m.config.Rigs[name]; ok {
			delete(m.config.Rigs, name)
			_ = config.SaveRigsConfig(filepath.Join(m.townRoot, "mayor", "rigs.json"), m.config)
		}
	}()
	if err := m.saveLocalRigConfig(setup, name); err != nil {
		return nil, err
	}
	if err := m.createLocalRigDirs(setup.rigPath); err != nil {
		return nil, err
	}
	if err := m.cloneLocalRig(ctx, setup); err != nil {
		return nil, err
	}
	if err := m.registerLocalRig(setup, name); err != nil {
		return nil, err
	}

	success = true
	return m.loadRig(name, m.config.Rigs[name])
}

func (m *Manager) prepareLocalRig(name, srcRepo string) (localRigSetup, error) {
	if err := m.validateLocalRigName(name); err != nil {
		return localRigSetup{}, err
	}

	absSrc, err := resolveLocalRepoPath(srcRepo)
	if err != nil {
		return localRigSetup{}, err
	}
	srcGit := git.NewGit(absSrc)
	if err := validateLocalRepo(absSrc, srcGit); err != nil {
		return localRigSetup{}, err
	}

	prefix := deriveBeadsPrefix(name)
	if err := m.checkLocalRigPrefix(prefix, name); err != nil {
		return localRigSetup{}, err
	}
	rigPath := filepath.Join(m.townRoot, name)
	if _, err := os.Stat(rigPath); err == nil {
		return localRigSetup{}, fmt.Errorf("directory already exists: %s", rigPath)
	}
	return localRigSetup{
		absSrc:  absSrc,
		gitURL:  fileURL(absSrc),
		prefix:  prefix,
		rigPath: rigPath,
		srcGit:  srcGit,
	}, nil
}

func (m *Manager) validateLocalRigName(name string) error {
	if m.RigExists(name) {
		return ErrRigExists
	}
	if strings.ContainsAny(name, "-. /\\") {
		return fmt.Errorf("rig name %q contains invalid characters", name)
	}
	for _, reserved := range reservedRigNames {
		if strings.EqualFold(name, reserved) {
			return fmt.Errorf("rig name %q is reserved for town-level infrastructure", reserved)
		}
	}
	return nil
}

func validateLocalRepo(absSrc string, srcGit *git.Git) error {
	if !srcGit.IsRepo() {
		return fmt.Errorf("not a git repository: %s", absSrc)
	}
	empty, err := srcGit.IsEmpty()
	if err != nil {
		return fmt.Errorf("checking repository: %w", err)
	}
	if empty {
		return fmt.Errorf("repository %s is empty (no commits). Push at least one commit before adding it as a rig", absSrc)
	}
	return nil
}

func resolveLocalRepoPath(srcRepo string) (string, error) {
	absSrc, err := filepath.Abs(srcRepo)
	if err != nil {
		return "", fmt.Errorf("resolving local repo: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(absSrc); err == nil {
		absSrc = resolved
	}
	return absSrc, nil
}

func (m *Manager) saveLocalRigConfig(setup localRigSetup, name string) error {
	rigConfig := &RigConfig{
		Type:      "rig",
		Version:   CurrentRigConfigVersion,
		Name:      name,
		GitURL:    setup.gitURL,
		LocalRepo: setup.absSrc,
		CreatedAt: time.Now(),
		Beads:     &BeadsConfig{Prefix: setup.prefix},
	}
	if branch := setup.srcGit.DefaultBranch(); branch != "" {
		rigConfig.DefaultBranch = branch
	}
	if err := m.saveRigConfig(setup.rigPath, rigConfig); err != nil {
		return fmt.Errorf("saving rig config: %w", err)
	}
	if err := fenceRigBeadsWalkUp(setup.rigPath, setup.prefix); err != nil {
		return err
	}
	return nil
}

func (m *Manager) createLocalRigDirs(rigPath string) error {
	if err := os.MkdirAll(filepath.Join(rigPath, "polecats"), 0755); err != nil {
		return fmt.Errorf("creating polecats dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, "crew"), 0755); err != nil {
		return fmt.Errorf("creating crew dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, "settings"), 0755); err != nil {
		return fmt.Errorf("creating settings dir: %w", err)
	}
	return nil
}

func (m *Manager) cloneLocalRig(ctx context.Context, setup localRigSetup) error {
	bareRepoPath := filepath.Join(setup.rigPath, ".repo.git")
	if err := m.git.CloneBareLocal(ctx, setup.absSrc, bareRepoPath); err != nil {
		return fmt.Errorf("sharing local git objects: %w", err)
	}
	return nil
}

func (m *Manager) registerLocalRig(setup localRigSetup, name string) error {
	if m.config.Rigs == nil {
		m.config.Rigs = make(map[string]config.RigEntry)
	}
	m.config.Rigs[name] = config.RigEntry{
		GitURL:    setup.gitURL,
		LocalRepo: setup.absSrc,
		AddedAt:   time.Now(),
		BeadsConfig: &config.BeadsConfig{
			Prefix: setup.prefix,
		},
	}
	rigsPath := filepath.Join(m.townRoot, "mayor", "rigs.json")
	if err := config.SaveRigsConfig(rigsPath, m.config); err != nil {
		return fmt.Errorf("registering rig in rigs.json: %w", err)
	}

	// Sling resolves rig aliases through town routes.jsonl. The prefix fence
	// stops bd -C walk-up; without this route, ResolveRepoAliasBeadsDir still
	// fails until detached provision finishes InitializeRigBeads.
	if err := m.appendRigRoute(setup.rigPath, name, setup.prefix, RigBeadsInitOptions{RequireDolt: true}); err != nil {
		return err
	}
	return nil
}

// FindByLocalRepo returns the registered rig whose LocalRepo matches srcRepo.
func (m *Manager) FindByLocalRepo(srcRepo string) (string, bool) {
	want, err := canonicalRepoPath(srcRepo)
	if err != nil {
		return "", false
	}
	for name, entry := range m.config.Rigs {
		got, err := canonicalRepoPath(entry.LocalRepo)
		if err != nil {
			continue
		}
		if got == want {
			return name, true
		}
	}
	return "", false
}

func canonicalRepoPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

func (m *Manager) checkLocalRigPrefix(prefix, name string) error {
	if prefix == "" {
		return nil
	}
	if err := beads.CheckPrefixAvailable(m.townRoot, prefix+"-", name); err != nil {
		if errors.Is(err, beads.ErrPrefixInUse) {
			return fmt.Errorf("prefix collision (derived prefix %q): %w; choose a different --name or add the rig with 'gt rig add --prefix'", prefix, err)
		}
		return fmt.Errorf("prefix collision (derived prefix %q): %w", prefix, err)
	}
	for existing, entry := range m.config.Rigs {
		if existing == name || entry.BeadsConfig == nil {
			continue
		}
		if entry.BeadsConfig.Prefix == prefix {
			return fmt.Errorf("prefix collision (derived prefix %q): prefix already in use by rig %q; choose a different --name or add the rig with 'gt rig add --prefix'", prefix, existing)
		}
	}
	return nil
}

func fileURL(absPath string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}
	return u.String()
}

// fenceRigBeadsWalkUp writes a rig-local .beads config so `bd -C <town>/<rig>`
// cannot walk up to town hq-* beads. Full Dolt init happens later in
// InitializeRigBeads once the town Dolt server is running.
func fenceRigBeadsWalkUp(rigPath, prefix string) error {
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := beads.EnsureDir(beadsDir); err != nil {
		return err
	}
	if err := beads.EnsureConfigYAML(beadsDir, prefix); err != nil {
		return fmt.Errorf("writing rig beads prefix fence: %w", err)
	}
	return nil
}
