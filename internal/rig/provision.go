package rig

import (
	"path/filepath"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
)

// Provision makes a Worker workspace ready. Callers supply the rig path,
// the work directory, and the Role. The six steps run in one order:
// shared beads redirect, PRIME.md, overlay, local git exclude, runtime
// settings, setup hooks. Ignore patterns stay in .git/info/exclude so they
// do not reach the tracked Rig tree.
func Provision(rigPath, workDir, role string) error {
	townRoot := provisionTownRoot(rigPath)

	warnProvisionFailure("could not set up beads redirect", func() error {
		return beads.SetupRedirect(townRoot, workDir)
	})
	warnProvisionFailure("could not provision PRIME.md", func() error {
		return beads.ProvisionPrimeMDForWorktree(workDir)
	})
	warnProvisionFailure("could not copy overlay files", func() error {
		return CopyOverlay(rigPath, workDir)
	})
	warnProvisionFailure("could not update local git excludes", func() error {
		return EnsureLocalExcludePatterns(workDir)
	})

	settingsDir := provisionSettingsDir(role, rigPath, workDir)
	rc := config.ResolveRoleAgentConfig(role, townRoot, rigPath)
	warnProvisionFailure("could not install runtime settings", func() error {
		return runtime.EnsureSettingsForRole(settingsDir, workDir, role, rc)
	})
	warnProvisionFailure("could not run setup hooks", func() error {
		return RunSetupHooks(rigPath, workDir)
	})
	return nil
}

func provisionTownRoot(rigPath string) string {
	townRoot, err := workspace.Find(rigPath)
	if err == nil && townRoot != "" {
		return townRoot
	}
	return filepath.Dir(rigPath)
}

func provisionSettingsDir(role, rigPath, workDir string) string {
	settingsDir := config.RoleSettingsDir(role, rigPath)
	if settingsDir == "" {
		return workDir
	}
	return settingsDir
}

func warnProvisionFailure(message string, action func() error) {
	if err := action(); err != nil {
		style.PrintWarning("%s: %v", message, err)
	}
}
