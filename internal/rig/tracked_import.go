package rig

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrTrackedBeadsImportConsent is returned when gt rig add would activate
// tracked Beads data or executable hooks without --import-beads.
var ErrTrackedBeadsImportConsent = errors.New("tracked Beads import requires --import-beads")

// TrackedBeadsImport describes Beads data and executable hooks found in a
// cloned Rig source before activation.
type TrackedBeadsImport struct {
	Source          string
	BeadCount       int
	JSONLFiles      []string
	ExecutableHooks []string
}

func (imp TrackedBeadsImport) RequiresConsent() bool {
	return imp.BeadCount > 0 || len(imp.ExecutableHooks) > 0
}

// InspectTrackedBeadsImport counts tracked Beads JSONL records and executable
// hooks under a cloned Rig source root (typically mayor/rig).
func InspectTrackedBeadsImport(sourceRoot string) (TrackedBeadsImport, error) {
	imp := TrackedBeadsImport{Source: sourceRoot}
	if err := inspectTrackedBeadsDir(sourceRoot, &imp); err != nil {
		return TrackedBeadsImport{}, err
	}
	if err := inspectTrackedHooks(sourceRoot, &imp); err != nil {
		return TrackedBeadsImport{}, err
	}
	return imp, nil
}

func inspectTrackedBeadsDir(sourceRoot string, imp *TrackedBeadsImport) error {
	beadsDir := filepath.Join(sourceRoot, ".beads")
	info, err := os.Lstat(beadsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked Beads directory %s", beadsDir)
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(beadsDir, func(path string, d fs.DirEntry, walkErr error) error {
		return inspectTrackedBeadsPath(sourceRoot, imp, path, d, walkErr)
	})
}

func inspectTrackedBeadsPath(sourceRoot string, imp *TrackedBeadsImport, path string, d fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	if d.Type()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked tracked Beads path %s", path)
	}
	if d.IsDir() {
		return nil
	}
	if strings.HasSuffix(d.Name(), ".jsonl") {
		if err := recordTrackedJSONL(sourceRoot, imp, path); err != nil {
			return err
		}
	}
	recordTrackedHook(sourceRoot, imp, path, d)
	return nil
}

func recordTrackedJSONL(sourceRoot string, imp *TrackedBeadsImport, path string) error {
	count, err := countJSONLRecords(path)
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	rel, _ := filepath.Rel(sourceRoot, path)
	imp.JSONLFiles = append(imp.JSONLFiles, filepath.ToSlash(rel))
	imp.BeadCount += count
	return nil
}

func recordTrackedHook(sourceRoot string, imp *TrackedBeadsImport, path string, d fs.DirEntry) {
	if !isExecutableHook(path, d) {
		return
	}
	rel, _ := filepath.Rel(sourceRoot, path)
	imp.ExecutableHooks = append(imp.ExecutableHooks, filepath.ToSlash(rel))
}

func inspectTrackedHooks(sourceRoot string, imp *TrackedBeadsImport) error {
	for _, hooksDir := range []string{
		filepath.Join(sourceRoot, ".git", "hooks"),
		filepath.Join(sourceRoot, ".beads", "hooks"),
	} {
		if err := inspectTrackedHooksDir(sourceRoot, hooksDir, imp); err != nil {
			return err
		}
	}
	return nil
}

func inspectTrackedHooksDir(sourceRoot, hooksDir string, imp *TrackedBeadsImport) error {
	info, err := os.Lstat(hooksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked hooks directory %s", hooksDir)
	}
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := inspectTrackedHookEntry(sourceRoot, hooksDir, imp, entry); err != nil {
			return err
		}
	}
	return nil
}

func inspectTrackedHookEntry(sourceRoot, hooksDir string, imp *TrackedBeadsImport, entry os.DirEntry) error {
	if entry.IsDir() {
		return nil
	}
	path := filepath.Join(hooksDir, entry.Name())
	if entry.Type()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked hook %s", path)
	}
	info, err := entry.Info()
	if err != nil {
		return nil
	}
	if strings.HasSuffix(entry.Name(), ".sample") || !isExecutableFile(path, info) {
		return nil
	}
	rel, _ := filepath.Rel(sourceRoot, path)
	relPath := filepath.ToSlash(rel)
	if containsString(imp.ExecutableHooks, relPath) {
		return nil
	}
	imp.ExecutableHooks = append(imp.ExecutableHooks, relPath)
	return nil
}

// RequireTrackedBeadsImportConsent rejects silent activation of tracked Beads
// data or executable hooks. A normal Rig with neither requires no consent.
func RequireTrackedBeadsImportConsent(imp TrackedBeadsImport, allowed bool) error {
	if !imp.RequiresConsent() {
		return nil
	}
	if allowed {
		return nil
	}
	return fmt.Errorf("%w: source %s would import %d bead(s) from %v and activate %d executable hook(s) %v",
		ErrTrackedBeadsImportConsent, imp.Source, imp.BeadCount, imp.JSONLFiles, len(imp.ExecutableHooks), imp.ExecutableHooks)
}

func countJSONLRecords(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "{") {
			count++
		}
	}
	return count, scanner.Err()
}

func isExecutableHook(path string, d fs.DirEntry) bool {
	if d.IsDir() {
		return false
	}
	name := d.Name()
	if strings.HasSuffix(name, ".sample") {
		return false
	}
	dir := filepath.Base(filepath.Dir(path))
	if dir != "hooks" {
		return false
	}
	info, err := d.Info()
	if err != nil {
		return false
	}
	return isExecutableFile(path, info)
}

func isExecutableFile(path string, info os.FileInfo) bool {
	if info == nil || info.IsDir() {
		return false
	}
	if info.Mode()&0o111 != 0 {
		return true
	}
	// Windows has no POSIX exec bit; treat non-sample hook files as executable.
	if filepath.Ext(path) == ".exe" || filepath.Ext(path) == ".bat" || filepath.Ext(path) == ".cmd" {
		return true
	}
	return false
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
