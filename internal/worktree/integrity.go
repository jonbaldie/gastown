package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrIntegrityViolation classifies corrupted worktree metadata. Command paths
// that depend on hook/worktree state should fail closed when this is returned.
var ErrIntegrityViolation = errors.New("worktree integrity violation")

// IntegrityOptions controls worktree validation.
type IntegrityOptions struct {
	// TownRoot bounds the upward search for .git metadata. Empty means search to
	// the filesystem root.
	TownRoot string

	// Require reports a missing .git marker as an integrity violation. This is
	// appropriate for agent worktree roles such as polecats, crew, refinery, and
	// witness.
	Require bool
}

// Validate checks the nearest worktree metadata for path. It accepts regular
// clones (.git directory) and validates linked worktree .git files by ensuring
// they are well formed and point at usable git metadata.
func Validate(path string, opts IntegrityOptions) error {
	path, err := validationPath(path)
	if err != nil {
		return err
	}

	marker, found, err := findGitMarker(path, opts.TownRoot)
	if err != nil {
		return err
	}
	if !found {
		return missingMarkerError(path, opts.Require)
	}

	return validateMarker(marker)
}

func validationPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("%w: get cwd: %v", ErrIntegrityViolation, err)
	}
	return cwd, nil
}

func missingMarkerError(path string, require bool) error {
	if !require {
		return nil
	}
	return fmt.Errorf("%w: missing .git metadata under %s", ErrIntegrityViolation, path)
}

func validateMarker(marker string) error {
	info, err := os.Stat(marker)
	if err != nil {
		return fmt.Errorf("%w: cannot stat %s: %v", ErrIntegrityViolation, marker, err)
	}
	if info.IsDir() {
		return validateGitDir(marker, marker)
	}

	content, err := os.ReadFile(marker)
	if err != nil {
		return fmt.Errorf("%w: cannot read %s: %v", ErrIntegrityViolation, marker, err)
	}

	target, err := resolveGitDirTarget(marker, content)
	if err != nil {
		return err
	}
	return validateGitDir(target, marker)
}

func resolveGitDirTarget(marker string, content []byte) (string, error) {
	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir: ") {
		return "", fmt.Errorf("%w: malformed .git file at %s", ErrIntegrityViolation, marker)
	}

	target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir: "))
	if target == "" {
		return "", fmt.Errorf("%w: empty gitdir target in %s", ErrIntegrityViolation, marker)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Clean(filepath.Join(filepath.Dir(marker), target))
	}

	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("%w: gitdir target missing for %s: %s", ErrIntegrityViolation, marker, target)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: gitdir target is not a directory for %s: %s", ErrIntegrityViolation, marker, target)
	}
	return target, nil
}

func validateGitDir(gitdir, marker string) error {
	if _, err := os.Stat(filepath.Join(gitdir, "HEAD")); err != nil {
		return fmt.Errorf("%w: gitdir metadata incomplete for %s: missing HEAD in %s", ErrIntegrityViolation, marker, gitdir)
	}
	return nil
}

func findGitMarker(path, townRoot string) (string, bool, error) {
	path, stop, err := searchBounds(path, townRoot)
	if err != nil {
		return "", false, err
	}

	for {
		marker, found, err := markerAt(path)
		if err != nil || found {
			return marker, found, err
		}

		if atSearchStop(path, stop) {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}

	return "", false, nil
}

func searchBounds(path, townRoot string) (string, string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve path %s: %v", ErrIntegrityViolation, path, err)
	}
	if townRoot == "" {
		return path, "", nil
	}
	stop, err := filepath.Abs(townRoot)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve town root %s: %v", ErrIntegrityViolation, townRoot, err)
	}
	return path, stop, nil
}

func markerAt(path string) (string, bool, error) {
	marker := filepath.Join(path, ".git")
	_, err := os.Lstat(marker)
	if err == nil {
		return marker, true, nil
	}
	if os.IsNotExist(err) {
		return "", false, nil
	}
	return "", false, fmt.Errorf("%w: cannot inspect %s: %v", ErrIntegrityViolation, marker, err)
}

func atSearchStop(path, stop string) bool {
	return stop != "" && path == stop
}
