// Package skills provisions the mattpocock/skills collection into agent workspaces.
//
// Role sessions discover skills from project trees (.agents/skills and
// <agent-config>/skills) and from isolated account config dirs (skills/).
// This package embeds the collection and writes or links it into those paths.
package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

const (
	universalSkillsRel = ".agents/skills"
	userSkillsRel      = "skills"
)

// Names returns the embedded mattpocock skill directory names, sorted.
func Names() []string {
	entries, err := fs.ReadDir(embeddedFS, "embedded")
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// ProvisionFor writes the embedded mattpocock skills into workspacePath so a
// role agent can discover them. Skills land in .agents/skills (universal) and,
// when the agent has a config directory, <configDir>/skills.
func ProvisionFor(workspacePath, agent string) error {
	if workspacePath == "" {
		return fmt.Errorf("workspace path is required")
	}

	universalDir := filepath.Join(workspacePath, filepath.FromSlash(universalSkillsRel))
	if err := writeEmbeddedTree(universalDir); err != nil {
		return err
	}

	if configDir := agentConfigDir(agent); configDir != "" {
		agentDir := filepath.Join(workspacePath, configDir, "skills")
		if err := linkOrWrite(agentDir, universalDir); err != nil {
			return err
		}
	}
	return nil
}

// ProvisionUserDir writes skills into configDir/skills. Used for isolated
// account directories (CLAUDE_CONFIG_DIR and equivalents).
func ProvisionUserDir(configDir string) error {
	if configDir == "" {
		return fmt.Errorf("config dir is required")
	}
	return writeEmbeddedTree(filepath.Join(configDir, userSkillsRel))
}

// MissingFor returns embedded skill names that are not present in the
// workspace's universal skill tree.
func MissingFor(workspacePath, agent string) []string {
	_ = agent
	if workspacePath == "" {
		return append([]string(nil), Names()...)
	}
	root := filepath.Join(workspacePath, filepath.FromSlash(universalSkillsRel))
	var missing []string
	for _, name := range Names() {
		if _, err := os.Stat(filepath.Join(root, name, "SKILL.md")); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

func agentConfigDir(agent string) string {
	preset := config.GetAgentPresetByName(strings.ToLower(agent))
	if preset == nil {
		return ""
	}
	return preset.ConfigDir
}

func writeEmbeddedTree(destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", destDir, err)
	}

	return fs.WalkDir(embeddedFS, "embedded", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("embedded", path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(destDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if _, err := os.Lstat(target); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		data, err := embeddedFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

func linkOrWrite(destDir, sourceDir string) error {
	if destDir == sourceDir {
		return nil
	}
	if info, err := os.Lstat(destDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(destDir)
			if err == nil && sameResolvedPath(destDir, target, sourceDir) {
				return nil
			}
			if err := os.Remove(destDir); err != nil {
				return fmt.Errorf("removing stale skills symlink: %w", err)
			}
		} else if info.IsDir() {
			return writeEmbeddedTree(destDir)
		} else {
			return fmt.Errorf("skills path %s exists and is not a directory", destDir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Dir(destDir), sourceDir)
	if err != nil {
		rel = sourceDir
	}
	if err := os.Symlink(rel, destDir); err != nil {
		return writeEmbeddedTree(destDir)
	}
	return nil
}

func sameResolvedPath(linkPath, linkTarget, want string) bool {
	resolved := linkTarget
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(linkPath), linkTarget)
	}
	absResolved, errA := filepath.Abs(resolved)
	absWant, errB := filepath.Abs(want)
	if errA != nil || errB != nil {
		return filepath.Clean(resolved) == filepath.Clean(want)
	}
	return absResolved == absWant
}
