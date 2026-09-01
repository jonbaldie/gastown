package formula

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Formulas live in internal/formula/formulas/ (source of truth).
// They are embedded into the binary and provisioned to .beads/formulas/ at install time.

//go:embed formulas/*.formula.toml
var formulasFS embed.FS

// InstalledRecord tracks which formulas were installed and their checksums.
// Stored in .beads/formulas/.installed.json
type InstalledRecord struct {
	Formulas map[string]string `json:"formulas"` // filename -> sha256 at install time
}

// FormulaStatus represents the status of a single formula during health check.
type FormulaStatus struct {
	Name          string
	Status        string // "ok", "outdated", "modified", "missing", "new", "untracked"
	EmbeddedHash  string // hash computed from embedded content
	InstalledHash string // hash we installed (from .installed.json)
	CurrentHash   string // hash of current file on disk
}

// HealthReport contains the results of checking formula health.
type HealthReport struct {
	Formulas []FormulaStatus
	// Counts
	OK        int
	Outdated  int // embedded changed, user hasn't modified
	Modified  int // user modified the file (tracked in .installed.json)
	Missing   int // file was deleted
	New       int // new formula not yet installed
	Untracked int // file exists but not in .installed.json (safe to update)
	Error     int // file could not be read (e.g. permission denied)
}

// ResolveFormulaContent resolves formula content using the three-tier precedence
// defined in docs/design/formula-resolution.md: rig > town > embedded.
//
// Tier 1 (rig): townRoot/rigName/.beads/formulas/<name>.formula.toml
// Tier 2 (town): townRoot/.beads/formulas/<name>.formula.toml
// Tier 3 (embedded): compiled into the binary
//
// Either townRoot or rigName may be empty; those tiers are skipped.
func ResolveFormulaContent(name, townRoot, rigName string) ([]byte, error) {
	filename := name
	if !hasFormulaSuffix(filename) {
		filename = filename + ".formula.toml"
	}

	// Tier 1: rig-level (most specific)
	if townRoot != "" && rigName != "" {
		path := filepath.Join(townRoot, rigName, ".beads", "formulas", filename)
		if content, err := os.ReadFile(path); err == nil {
			return content, nil
		}
	}

	// Tier 2: town-level
	if townRoot != "" {
		path := filepath.Join(townRoot, ".beads", "formulas", filename)
		if content, err := os.ReadFile(path); err == nil {
			return content, nil
		}
	}

	// Tier 3: embedded (system fallback)
	return GetEmbeddedFormulaContent(name)
}

// GetEmbeddedFormulaContent returns the raw content of an embedded formula by name.
// The name can be with or without the .formula.toml suffix.
// Returns the content bytes, or an error if the formula is not found.
func GetEmbeddedFormulaContent(name string) ([]byte, error) {
	// Normalize: ensure the filename has the correct suffix
	filename := name
	if !hasFormulaSuffix(filename) {
		filename = filename + ".formula.toml"
	}
	content, err := formulasFS.ReadFile("formulas/" + filename)
	if err != nil {
		return nil, fmt.Errorf("embedded formula %q not found: %w", name, err)
	}
	return content, nil
}

// hasFormulaSuffix checks if a name already has a formula file suffix.
func hasFormulaSuffix(name string) bool {
	return len(name) > len(".formula.toml") &&
		name[len(name)-len(".formula.toml"):] == ".formula.toml"
}

// computeHash computes SHA256 hash of data.
func computeHash(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// getEmbeddedFormulas returns a map of filename -> sha256 for all embedded formulas.
func getEmbeddedFormulas() (map[string]string, error) {
	entries, err := formulasFS.ReadDir("formulas")
	if err != nil {
		return nil, fmt.Errorf("reading formulas directory: %w", err)
	}

	result := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := formulasFS.ReadFile("formulas/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		result[entry.Name()] = computeHash(content)
	}
	return result, nil
}

// loadInstalledRecord loads the installed record from disk.
func loadInstalledRecord(formulasDir string) (*InstalledRecord, error) {
	path := filepath.Join(formulasDir, ".installed.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &InstalledRecord{Formulas: make(map[string]string)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading installed record: %w", err)
	}
	var r InstalledRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing installed record: %w", err)
	}
	if r.Formulas == nil {
		r.Formulas = make(map[string]string)
	}
	return &r, nil
}

// saveInstalledRecord saves the installed record to disk.
func saveInstalledRecord(formulasDir string, record *InstalledRecord) error {
	path := filepath.Join(formulasDir, ".installed.json")
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding installed record: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// computeFileHash computes SHA256 hash of a file.
func computeFileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return computeHash(data), nil
}

// ProvisionFormulas creates the .beads/formulas/ directory with embedded formulas.
// This is called during gt install for fresh installations.
// If a formula already exists, it is skipped (no overwrite).
// Returns the number of formulas provisioned.
func ProvisionFormulas(beadsPath string) (int, error) {
	embedded, err := getEmbeddedFormulas()
	if err != nil {
		return 0, err
	}

	entries, err := formulasFS.ReadDir("formulas")
	if err != nil {
		return 0, fmt.Errorf("reading formulas directory: %w", err)
	}

	// Create .beads/formulas/ directory
	formulasDir := filepath.Join(beadsPath, ".beads", "formulas")
	if err := os.MkdirAll(formulasDir, 0755); err != nil {
		return 0, fmt.Errorf("creating formulas directory: %w", err)
	}

	// Load existing installed record (or create new)
	installed, err := loadInstalledRecord(formulasDir)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, entry := range entries {
		installedOne, err := provisionMissingFormula(entry, formulasDir, embedded, installed)
		if err != nil {
			return count, err
		}
		if installedOne {
			count++
		}
	}

	// Save updated installed record
	if err := saveInstalledRecord(formulasDir, installed); err != nil {
		return count, fmt.Errorf("saving installed record: %w", err)
	}

	return count, nil
}

// CheckFormulaHealth checks the status of all formulas.
// Returns a report of which formulas are ok, outdated, modified, or missing.
func CheckFormulaHealth(beadsPath string) (*HealthReport, error) {
	embedded, err := getEmbeddedFormulas()
	if err != nil {
		return nil, err
	}

	formulasDir := filepath.Join(beadsPath, ".beads", "formulas")
	installed, err := loadInstalledRecord(formulasDir)
	if err != nil {
		return nil, err
	}

	report := &HealthReport{}

	for filename, embeddedHash := range embedded {
		installedHash, wasInstalled := installed.Formulas[filename]
		status := FormulaStatus{
			Name:          filename,
			EmbeddedHash:  embeddedHash,
			InstalledHash: installedHash,
		}
		classifyFormulaFile(&status, report, filepath.Join(formulasDir, filename), wasInstalled, installedHash, embeddedHash)
		report.Formulas = append(report.Formulas, status)
	}

	return report, nil
}

// UpdateFormulas updates formulas that are safe to update (outdated, missing, or untracked).
// Skips user-modified formulas (tracked files that user changed).
// Returns counts of updated, skipped (modified), and reinstalled (missing).
func UpdateFormulas(beadsPath string) (updated, skipped, reinstalled int, err error) {
	embedded, err := getEmbeddedFormulas()
	if err != nil {
		return 0, 0, 0, err
	}

	formulasDir := filepath.Join(beadsPath, ".beads", "formulas")
	if err := os.MkdirAll(formulasDir, 0755); err != nil {
		return 0, 0, 0, fmt.Errorf("creating formulas directory: %w", err)
	}

	installed, err := loadInstalledRecord(formulasDir)
	if err != nil {
		return 0, 0, 0, err
	}

	updated, skipped, reinstalled, err = applyFormulaUpdates(embedded, formulasDir, installed)
	if err != nil {
		return updated, skipped, reinstalled, err
	}
	if err := saveInstalledRecord(formulasDir, installed); err != nil {
		return updated, skipped, reinstalled, fmt.Errorf("saving installed record: %w", err)
	}
	return updated, skipped, reinstalled, nil
}

func applyFormulaUpdates(embedded map[string]string, formulasDir string, installed *InstalledRecord) (updated, skipped, reinstalled int, err error) {
	for filename, embeddedHash := range embedded {
		u, s, r, err := applyOneFormulaUpdate(filename, embeddedHash, formulasDir, installed)
		if err != nil {
			return updated, skipped, reinstalled, err
		}
		updated += u
		skipped += s
		reinstalled += r
	}
	return updated, skipped, reinstalled, nil
}

func applyOneFormulaUpdate(filename, embeddedHash, formulasDir string, installed *InstalledRecord) (updated, skipped, reinstalled int, err error) {
	installedHash, wasInstalled := installed.Formulas[filename]
	destPath := filepath.Join(formulasDir, filename)
	currentHash, fileErr := computeFileHash(destPath)
	decision := decideFormulaUpdate(fileErr, wasInstalled, currentHash, embeddedHash, installedHash)
	if decision.isModified {
		return 0, 1, 0, nil
	}
	if !decision.shouldInstall {
		return 0, 0, 0, nil
	}
	if err := writeFormulaFile(filename, destPath, embeddedHash, installed); err != nil {
		return 0, 0, 0, err
	}
	if decision.isMissing {
		return 0, 0, 1, nil
	}
	return 1, 0, 0, nil
}

func provisionMissingFormula(entry fs.DirEntry, formulasDir string, embedded map[string]string, installed *InstalledRecord) (bool, error) {
	if entry.IsDir() {
		return false, nil
	}
	destPath := filepath.Join(formulasDir, entry.Name())
	if _, err := os.Stat(destPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("checking %s: %w", entry.Name(), err)
	}
	content, err := formulasFS.ReadFile("formulas/" + entry.Name())
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", entry.Name(), err)
	}
	if err := os.WriteFile(destPath, content, 0644); err != nil {
		return false, fmt.Errorf("writing %s: %w", entry.Name(), err)
	}
	if hash, ok := embedded[entry.Name()]; ok {
		installed.Formulas[entry.Name()] = hash
	}
	return true, nil
}

func classifyFormulaFile(status *FormulaStatus, report *HealthReport, destPath string, wasInstalled bool, installedHash, embeddedHash string) {
	currentHash, err := computeFileHash(destPath)
	if os.IsNotExist(err) {
		classifyMissingFormula(status, report, wasInstalled)
		return
	}
	if err != nil {
		status.Status = "error"
		report.Error++
		return
	}
	status.CurrentHash = currentHash
	classifyExistingFormula(status, report, wasInstalled, currentHash, installedHash, embeddedHash)
}

func classifyMissingFormula(status *FormulaStatus, report *HealthReport, wasInstalled bool) {
	if wasInstalled {
		status.Status = "missing"
		report.Missing++
		return
	}
	status.Status = "new"
	report.New++
}

func classifyExistingFormula(status *FormulaStatus, report *HealthReport, wasInstalled bool, currentHash, installedHash, embeddedHash string) {
	switch {
	case currentHash == embeddedHash:
		status.Status = "ok"
		report.OK++
	case wasInstalled && currentHash == installedHash:
		status.Status = "outdated"
		report.Outdated++
	case wasInstalled:
		status.Status = "modified"
		report.Modified++
	default:
		status.Status = "untracked"
		report.Untracked++
	}
}

type formulaUpdateDecision struct {
	shouldInstall bool
	isMissing     bool
	isModified    bool
}

func decideFormulaUpdate(fileErr error, wasInstalled bool, currentHash, embeddedHash, installedHash string) formulaUpdateDecision {
	if os.IsNotExist(fileErr) {
		return formulaUpdateDecision{shouldInstall: true, isMissing: wasInstalled}
	}
	if fileErr != nil || currentHash == embeddedHash {
		return formulaUpdateDecision{}
	}
	if wasInstalled && currentHash != installedHash {
		return formulaUpdateDecision{isModified: true}
	}
	return formulaUpdateDecision{shouldInstall: true}
}

func writeFormulaFile(filename, destPath, embeddedHash string, installed *InstalledRecord) error {
	content, err := formulasFS.ReadFile("formulas/" + filename)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filename, err)
	}
	if err := os.WriteFile(destPath, content, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", filename, err)
	}
	installed.Formulas[filename] = embeddedHash
	return nil
}
