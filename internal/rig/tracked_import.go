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
	beadsDir := filepath.Join(sourceRoot, ".beads")
	info, err := os.Lstat(beadsDir)
	if err != nil && !os.IsNotExist(err) {
		return TrackedBeadsImport{}, err
	}
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return TrackedBeadsImport{}, fmt.Errorf("refusing symlinked Beads directory %s", beadsDir)
	}
	if err == nil && info.IsDir() {
		if err := filepath.WalkDir(beadsDir, func(path string, d fs.DirEntry, walkErr error) error {
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
				count, err := countJSONLRecords(path)
				if err != nil {
					return err
				}
				if count > 0 {
					rel, _ := filepath.Rel(sourceRoot, path)
					imp.JSONLFiles = append(imp.JSONLFiles, filepath.ToSlash(rel))
					imp.BeadCount += count
				}
			}
			if isExecutableHook(path, d) {
				rel, _ := filepath.Rel(sourceRoot, path)
				imp.ExecutableHooks = append(imp.ExecutableHooks, filepath.ToSlash(rel))
			}
			return nil
		}); err != nil {
			return TrackedBeadsImport{}, err
		}
	}

	for _, hooksDir := range []string{
		filepath.Join(sourceRoot, ".git", "hooks"),
		filepath.Join(sourceRoot, ".beads", "hooks"),
	} {
		info, err := os.Lstat(hooksDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return TrackedBeadsImport{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return TrackedBeadsImport{}, fmt.Errorf("refusing symlinked hooks directory %s", hooksDir)
		}
		entries, err := os.ReadDir(hooksDir)
		if err != nil {
			return TrackedBeadsImport{}, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return TrackedBeadsImport{}, fmt.Errorf("refusing symlinked hook %s", filepath.Join(hooksDir, entry.Name()))
			}
			path := filepath.Join(hooksDir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if isExecutableFile(path, info) && !strings.HasSuffix(entry.Name(), ".sample") {
				rel, _ := filepath.Rel(sourceRoot, path)
				if !containsString(imp.ExecutableHooks, filepath.ToSlash(rel)) {
					imp.ExecutableHooks = append(imp.ExecutableHooks, filepath.ToSlash(rel))
				}
			}
		}
	}
	return imp, nil
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
