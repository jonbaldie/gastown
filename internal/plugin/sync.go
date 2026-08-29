package plugin

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SyncResult records the outcome of a plugin sync operation.
type SyncResult struct {
	Copied  []string // plugin names that were copied/updated
	Removed []string // plugin names that were removed (clean mode)
	Skipped []string // plugin names that were already up-to-date
	Errors  []string // errors encountered
}

// SyncPlugins copies plugin directories from source to target.
// If clean is true, removes plugins from target that don't exist in source.
func SyncPlugins(sourceDir, targetDir string, clean bool) (*SyncResult, error) {
	if err := preparePluginSyncDirs(sourceDir, targetDir); err != nil {
		return nil, err
	}
	srcEntries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("reading source directory: %w", err)
	}
	result := &SyncResult{}
	srcPlugins := syncPluginEntries(sourceDir, targetDir, srcEntries, result)
	if clean {
		removeStalePlugins(targetDir, srcPlugins, result)
	}
	return result, nil
}

func preparePluginSyncDirs(sourceDir, targetDir string) error {
	srcInfo, err := os.Stat(sourceDir)
	if err != nil {
		return fmt.Errorf("source directory %s: %w", sourceDir, err)
	}
	if !srcInfo.IsDir() {
		return fmt.Errorf("source is not a directory: %s", sourceDir)
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("creating target directory: %w", err)
	}
	return nil
}

func syncPluginEntries(sourceDir, targetDir string, srcEntries []os.DirEntry, result *SyncResult) map[string]bool {
	srcPlugins := make(map[string]bool)
	for _, entry := range srcEntries {
		if !isPluginDir(sourceDir, entry) {
			continue
		}
		srcPlugins[entry.Name()] = true
		syncOnePlugin(sourceDir, targetDir, entry.Name(), result)
	}
	return srcPlugins
}

func isPluginDir(parent string, entry os.DirEntry) bool {
	if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
		return false
	}
	_, err := os.Stat(filepath.Join(parent, entry.Name(), "plugin.md"))
	return err == nil
}

func syncOnePlugin(sourceDir, targetDir, name string, result *SyncResult) {
	srcPluginDir := filepath.Join(sourceDir, name)
	dstPluginDir := filepath.Join(targetDir, name)
	if dirsMatch(srcPluginDir, dstPluginDir) {
		result.Skipped = append(result.Skipped, name)
		return
	}
	if err := copyDir(srcPluginDir, dstPluginDir); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", name, err))
		return
	}
	result.Copied = append(result.Copied, name)
}

func removeStalePlugins(targetDir string, srcPlugins map[string]bool, result *SyncResult) {
	dstEntries, err := os.ReadDir(targetDir)
	if err != nil {
		return
	}
	for _, entry := range dstEntries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || srcPlugins[entry.Name()] {
			continue
		}
		dstPath := filepath.Join(targetDir, entry.Name())
		if err := os.RemoveAll(dstPath); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("removing %s: %v", entry.Name(), err))
			continue
		}
		result.Removed = append(result.Removed, entry.Name())
	}
}

// dirsMatch checks if two plugin directories have identical contents.
func dirsMatch(src, dst string) bool {
	srcHash := DirHash(src)
	dstHash := DirHash(dst)
	return srcHash != "" && srcHash == dstHash
}

// DirHash computes a content hash of all files in a directory.
func DirHash(dir string) string {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(rel))
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: walking trusted plugin directory
		if err != nil {
			return err
		}
		h.Write(data)
		return nil
	})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// copyDir recursively copies a directory, replacing the destination atomically.
// It copies to a temp directory in the same parent, then swaps via rename.
func copyDir(src, dst string) error {
	tmpDir, err := os.MkdirTemp(filepath.Dir(dst), ".plugin-sync-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	// Clean up temp dir on failure; on success it's been renamed away.
	defer os.RemoveAll(tmpDir)

	if err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		tmpPath := filepath.Join(tmpDir, rel)
		if d.IsDir() {
			return os.MkdirAll(tmpPath, 0755)
		}
		return copyFile(path, tmpPath)
	}); err != nil {
		return err
	}

	// Atomic swap: remove old dst, rename temp into place.
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("removing old destination: %w", err)
	}
	return os.Rename(tmpDir, dst)
}

func copyFile(src, dst string) error {
	srcFile, err := os.Open(src) //nolint:gosec // G304: path is from trusted plugin directory
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode()) //nolint:gosec // G304: path is from trusted plugin directory
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

// FindGastownSource locates the gastown source repo's plugins directory.
// Search order:
//  1. Walk up from CWD for a gastown go.mod with plugins/
//  2. <townRoot>/gastown/crew/den/plugins/
//  3. <townRoot>/gastown/plugins/
func FindGastownSource(townRoot string) (string, error) {
	if cwd, err := os.Getwd(); err == nil {
		if src := findSourceFromDir(cwd); src != "" {
			return src, nil
		}
	}

	candidates := []string{
		filepath.Join(townRoot, "gastown", "crew", "den", "plugins"),
		filepath.Join(townRoot, "gastown", "plugins"),
	}
	for _, candidate := range candidates {
		if hasPlugins(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("could not locate gastown plugin source; use --source to specify")
}

func findSourceFromDir(dir string) string {
	current := dir
	for {
		pluginsDir := filepath.Join(current, "plugins")
		goMod := filepath.Join(current, "go.mod")
		if hasPlugins(pluginsDir) {
			if isGastownModule(goMod) {
				return pluginsDir
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

// isGastownModule checks if a go.mod file declares a gastown module path.
// Matches "module .../gastown" on the module directive line to avoid
// false-positives from comments or dependency names.
func isGastownModule(goModPath string) bool {
	f, err := os.Open(goModPath) //nolint:gosec // G304: path from traversal
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.HasSuffix(line, "/gastown") || line == "module gastown"
		}
	}
	return false
}

func hasPlugins(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			if _, err := os.Stat(filepath.Join(dir, entry.Name(), "plugin.md")); err == nil {
				return true
			}
		}
	}
	return false
}

// DriftReport describes differences between source and runtime plugins.
type DriftReport struct {
	Source  string       `json:"source"`
	Target  string       `json:"target"`
	Drifted []DriftEntry `json:"drifted,omitempty"`
	Missing []string     `json:"missing,omitempty"` // in source but not target
	Extra   []string     `json:"extra,omitempty"`   // in target but not source
}

// DriftEntry describes a single plugin that differs between source and runtime.
type DriftEntry struct {
	Name       string `json:"name"`
	SourceHash string `json:"source_hash"`
	TargetHash string `json:"target_hash"`
}

// DetectDrift compares plugin directories between source and target.
func DetectDrift(sourceDir, targetDir string) (*DriftReport, error) {
	srcEntries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("reading source: %w", err)
	}
	report := &DriftReport{
		Source: sourceDir,
		Target: targetDir,
	}
	tgtPlugins := listDirNames(targetDir)
	compareSourcePlugins(sourceDir, targetDir, srcEntries, tgtPlugins, report)
	appendExtraPlugins(targetDir, tgtPlugins, report)
	return report, nil
}

func listDirNames(dir string) map[string]bool {
	names := make(map[string]bool)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return names
	}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			names[entry.Name()] = true
		}
	}
	return names
}

func compareSourcePlugins(sourceDir, targetDir string, srcEntries []os.DirEntry, tgtPlugins map[string]bool, report *DriftReport) {
	for _, entry := range srcEntries {
		if !isPluginDir(sourceDir, entry) {
			continue
		}
		if !tgtPlugins[entry.Name()] {
			report.Missing = append(report.Missing, entry.Name())
			continue
		}
		delete(tgtPlugins, entry.Name())
		recordPluginDrift(sourceDir, targetDir, entry.Name(), report)
	}
}

func recordPluginDrift(sourceDir, targetDir, name string, report *DriftReport) {
	srcHash := DirHash(filepath.Join(sourceDir, name))
	dstHash := DirHash(filepath.Join(targetDir, name))
	if srcHash == dstHash {
		return
	}
	report.Drifted = append(report.Drifted, DriftEntry{
		Name:       name,
		SourceHash: srcHash,
		TargetHash: dstHash,
	})
}

func appendExtraPlugins(targetDir string, tgtPlugins map[string]bool, report *DriftReport) {
	for name := range tgtPlugins {
		if _, err := os.Stat(filepath.Join(targetDir, name, "plugin.md")); err == nil {
			report.Extra = append(report.Extra, name)
		}
	}
}

// HasDrift returns true if the report indicates any differences.
func (r *DriftReport) HasDrift() bool {
	return len(r.Drifted) > 0 || len(r.Missing) > 0
}
