package formula

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// OverrideMode specifies how a step override is applied.
type OverrideMode string

const (
	// ModeReplace swaps the step description entirely.
	ModeReplace OverrideMode = "replace"
	// ModeAppend adds text after the existing step description.
	ModeAppend OverrideMode = "append"
	// ModeSkip removes the step from the formula.
	ModeSkip OverrideMode = "skip"
)

// StepOverride describes a single step override from an overlay file.
type StepOverride struct {
	StepID      string       `toml:"step_id"`
	Mode        OverrideMode `toml:"mode"`
	Description string       `toml:"description"`
}

// FormulaOverlay holds the parsed overlay for a formula.
type FormulaOverlay struct {
	StepOverrides []StepOverride `toml:"step-overrides"`
}

// LoadFormulaOverlay reads overlay files for the given formula name.
//
// It checks two locations:
//   - Town-level: <townRoot>/formula-overlays/<formulaName>.toml
//   - Rig-level:  <townRoot>/<rigName>/formula-overlays/<formulaName>.toml
//
// If a rig-level overlay exists, it takes full precedence (not merged with town).
// If neither file exists, returns nil with no error.
func LoadFormulaOverlay(formulaName, townRoot, rigName string) (*FormulaOverlay, error) {
	rigPath := filepath.Join(townRoot, rigName, "formula-overlays", formulaName+".toml")
	townPath := filepath.Join(townRoot, "formula-overlays", formulaName+".toml")

	// Rig-level takes full precedence.
	if overlay, err := loadOverlayFile(rigPath); err != nil {
		return nil, fmt.Errorf("loading rig overlay %s: %w", rigPath, err)
	} else if overlay != nil {
		return overlay, nil
	}

	// Fall back to town-level.
	if overlay, err := loadOverlayFile(townPath); err != nil {
		return nil, fmt.Errorf("loading town overlay %s: %w", townPath, err)
	} else if overlay != nil {
		return overlay, nil
	}

	return nil, nil
}

// loadOverlayFile reads and parses a single overlay TOML file.
// Returns (nil, nil) if the file does not exist.
func loadOverlayFile(path string) (*FormulaOverlay, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from trusted overlay directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var overlay FormulaOverlay
	if _, err := toml.Decode(string(data), &overlay); err != nil {
		return nil, fmt.Errorf("parsing overlay TOML: %w", err)
	}

	// Validate overrides.
	for i, so := range overlay.StepOverrides {
		if so.StepID == "" {
			return nil, fmt.Errorf("step-overrides[%d]: step_id is required", i)
		}
		switch so.Mode {
		case ModeReplace, ModeAppend, ModeSkip:
			// valid
		default:
			return nil, fmt.Errorf("step-overrides[%d] (step_id=%q): invalid mode %q (must be replace, append, or skip)", i, so.StepID, so.Mode)
		}
	}

	return &overlay, nil
}

// ApplyOverlays modifies formula steps in place according to the overlay.
// Returns a list of warnings for step IDs in the overlay that do not match
// any formula step (stale overrides).
func ApplyOverlays(f *Formula, overlay *FormulaOverlay) []string {
	if overlay == nil || len(overlay.StepOverrides) == 0 {
		return nil
	}

	var warnings []string

	for _, override := range overlay.StepOverrides {
		idx := formulaStepIndex(f, override.StepID)
		if idx == -1 {
			warnings = append(warnings, fmt.Sprintf("overlay references unknown step %q (stale override)", override.StepID))
			continue
		}
		applyStepOverride(f, idx, override)
	}
	return warnings
}

func formulaStepIndex(f *Formula, stepID string) int {
	for i := range f.Steps {
		if f.Steps[i].ID == stepID {
			return i
		}
	}
	return -1
}

func applyStepOverride(f *Formula, index int, override StepOverride) {
	switch override.Mode {
	case ModeReplace:
		f.Steps[index].Description = override.Description
	case ModeAppend:
		f.Steps[index].Description += "\n" + override.Description
	case ModeSkip:
		skipFormulaStep(f, index)
	}
}

func skipFormulaStep(f *Formula, index int) {
	removedID, removedNeeds := f.Steps[index].ID, f.Steps[index].Needs
	f.Steps = append(f.Steps[:index], f.Steps[index+1:]...)
	for i := range f.Steps {
		f.Steps[i].Needs = rewriteSkippedNeeds(f.Steps[i].Needs, removedID, removedNeeds)
	}
}

// rewriteSkippedNeeds replaces every occurrence of removedID with replacement.
func rewriteSkippedNeeds(needs []string, removedID string, replacement []string) []string {
	if len(needs) == 0 {
		return needs
	}
	out := make([]string, 0, len(needs))
	for _, need := range needs {
		if need == removedID {
			out = append(out, replacement...)
			continue
		}
		out = append(out, need)
	}
	return out
}
