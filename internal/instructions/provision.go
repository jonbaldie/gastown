package instructions

import (
	"fmt"
	"os"
	"path/filepath"
)

// Provision writes Gas Town instruction text into dir.
//
// The canonical file is written first. The Claude alias is then a symlink to
// that file. skipIfContains, when non-empty, keeps an existing canonical file
// that already contains that marker.
func Provision(dir, content, skipIfContains string) (bool, error) {
	if dir == "" {
		return false, fmt.Errorf("instruction directory is empty")
	}
	snap := snapshot(dir)
	plan := planProvision(snap, content, skipIfContains)
	if planNoop(plan) {
		return false, nil
	}
	if err := applyPlan(dir, plan); err != nil {
		return false, err
	}
	return true, nil
}

// CanonicalName returns the canonical instruction file name for dir.
func CanonicalName(dir string) string {
	if snapshot(dir).agentsLocal.regular {
		return LocalCanonicalFile
	}
	return CanonicalFile
}

func applyPlan(dir string, plan provisionPlan) error {
	for _, name := range plan.remove {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", name, err)
		}
	}

	if plan.writeBody {
		path := filepath.Join(dir, plan.canonicalName)
		if err := os.WriteFile(path, []byte(plan.canonicalBody), 0644); err != nil {
			return fmt.Errorf("writing %s: %w", plan.canonicalName, err)
		}
	}

	if plan.writeAlias {
		if err := os.Symlink(plan.canonicalName, filepath.Join(dir, plan.aliasName)); err != nil {
			return fmt.Errorf("linking %s: %w", plan.aliasName, err)
		}
	}

	if plan.writeGemini {
		if err := os.Symlink(plan.canonicalName, filepath.Join(dir, GeminiAliasFile)); err != nil {
			return fmt.Errorf("linking %s: %w", GeminiAliasFile, err)
		}
	}

	return nil
}
