package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/jonbaldie/gastown/internal/formula"
)

// OverlayHealthCheck verifies that formula overlay files reference valid step IDs.
// It scans overlay files at both town-level and rig-level, loads the referenced
// formula from the embedded binary, and checks that every step_id in the overlay
// matches a real step in the formula. Fix mode removes stale step-override entries.
type OverlayHealthCheck struct {
	FixableCheck
}

// NewOverlayHealthCheck creates a new overlay health check.
func NewOverlayHealthCheck() *OverlayHealthCheck {
	return &OverlayHealthCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "overlay-health",
				CheckDescription: "Check formula overlay step IDs are valid",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// overlayFile represents a discovered overlay file with its parsed contents.
type overlayFile struct {
	Path        string
	FormulaName string
	Overlay     *formula.FormulaOverlay
	ParseErr    error    // non-nil if TOML parsing failed
	StaleIDs    []string // step IDs that don't match any formula step
}

// Run checks all formula overlay files for stale step IDs and malformed TOML.
func (c *OverlayHealthCheck) Run(ctx *CheckContext) *CheckResult {
	files := c.scanOverlays(ctx.TownRoot)

	if len(files) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "no overlay files found",
		}
	}

	var malformed, stale, ok int
	var details []string

	for _, f := range files {
		if f.ParseErr != nil {
			malformed++
			details = append(details, fmt.Sprintf("%s: malformed TOML: %v", f.Path, f.ParseErr))
			continue
		}
		if len(f.StaleIDs) > 0 {
			stale++
			details = append(details, fmt.Sprintf("%s: stale step IDs: %s",
				f.Path, strings.Join(f.StaleIDs, ", ")))
			continue
		}
		ok++
	}

	if malformed > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: fmt.Sprintf("%d malformed overlay(s)", malformed),
			Details: details,
			FixHint: "Fix malformed TOML in overlay files manually",
		}
	}

	if stale > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d overlay(s) with stale step IDs", stale),
			Details: details,
			FixHint: "Run 'gt doctor --fix' to remove stale step overrides",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("%d overlay(s) healthy", ok),
	}
}

// Fix removes stale step-override entries from overlay files.
// Malformed TOML files are left untouched (require manual intervention).
func (c *OverlayHealthCheck) Fix(ctx *CheckContext) error {
	files := c.scanOverlays(ctx.TownRoot)

	for _, f := range files {
		if err := fixOverlayFile(f); err != nil {
			return err
		}
	}

	return nil
}

func fixOverlayFile(f overlayFile) error {
	if f.ParseErr != nil || len(f.StaleIDs) == 0 {
		return nil
	}
	kept := keptOverlayOverrides(f)
	if len(kept) == 0 {
		return removeEmptyOverlay(f.Path)
	}
	f.Overlay.StepOverrides = kept
	if err := writeOverlayFile(f.Path, f.Overlay); err != nil {
		return fmt.Errorf("writing overlay %s: %w", f.Path, err)
	}
	return nil
}

func keptOverlayOverrides(f overlayFile) []formula.StepOverride {
	staleSet := make(map[string]bool, len(f.StaleIDs))
	for _, id := range f.StaleIDs {
		staleSet[id] = true
	}
	kept := make([]formula.StepOverride, 0, len(f.Overlay.StepOverrides))
	for _, so := range f.Overlay.StepOverrides {
		if !staleSet[so.StepID] {
			kept = append(kept, so)
		}
	}
	return kept
}

func removeEmptyOverlay(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing empty overlay %s: %w", path, err)
	}
	return nil
}

// scanOverlays discovers and validates all overlay files in the workspace.
func (c *OverlayHealthCheck) scanOverlays(townRoot string) []overlayFile {
	var results []overlayFile

	// Scan town-level overlays.
	townDir := filepath.Join(townRoot, "formula-overlays")
	results = append(results, scanOverlayDir(townDir)...)

	// Scan rig-level overlays by reading rigs.json.
	rigNames := loadRigNames(filepath.Join(townRoot, "mayor", "rigs.json"))
	for rigName := range rigNames {
		rigDir := filepath.Join(townRoot, rigName, "formula-overlays")
		results = append(results, scanOverlayDir(rigDir)...)
	}

	return results
}

// scanOverlayDir reads all .toml files in a formula-overlays directory,
// parses each one, and validates step IDs against the embedded formula.
func scanOverlayDir(dir string) []overlayFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // directory doesn't exist — that's fine
	}

	var results []overlayFile
	for _, entry := range entries {
		if !isOverlayFile(entry) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		formulaName := strings.TrimSuffix(entry.Name(), ".toml")
		results = append(results, parseOverlayFile(path, formulaName))
	}

	return results
}

func isOverlayFile(entry os.DirEntry) bool {
	return !entry.IsDir() && strings.HasSuffix(entry.Name(), ".toml")
}

func parseOverlayFile(path, formulaName string) overlayFile {
	of := overlayFile{Path: path, FormulaName: formulaName}
	data, err := os.ReadFile(path) //nolint:gosec // G304: trusted overlay directory
	if err != nil {
		of.ParseErr = err
		return of
	}
	var overlay formula.FormulaOverlay
	if _, err := toml.Decode(string(data), &overlay); err != nil {
		of.ParseErr = fmt.Errorf("parsing TOML: %w", err)
		return of
	}
	of.Overlay = &overlay
	embeddedContent, err := formula.GetEmbeddedFormulaContent(formulaName)
	if err != nil {
		for _, so := range overlay.StepOverrides {
			of.StaleIDs = append(of.StaleIDs, so.StepID)
		}
		return of
	}
	f, err := formula.Parse(embeddedContent)
	if err != nil {
		return of
	}
	of.StaleIDs = staleOverlayIDs(overlay, f.GetAllIDs())
	return of
}

func staleOverlayIDs(overlay formula.FormulaOverlay, valid []string) []string {
	validIDs := make(map[string]bool, len(valid))
	for _, id := range valid {
		validIDs[id] = true
	}
	var stale []string
	for _, so := range overlay.StepOverrides {
		if !validIDs[so.StepID] {
			stale = append(stale, so.StepID)
		}
	}
	return stale
}

// writeOverlayFile encodes a FormulaOverlay back to TOML and writes it to disk.
func writeOverlayFile(path string, overlay *formula.FormulaOverlay) error {
	var buf strings.Builder
	encoder := toml.NewEncoder(&buf)
	if err := encoder.Encode(overlay); err != nil {
		return fmt.Errorf("encoding TOML: %w", err)
	}
	return os.WriteFile(path, []byte(buf.String()), 0644) //nolint:gosec // G306: overlay files are not sensitive
}
