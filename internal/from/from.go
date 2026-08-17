// Package from plans a Gas Town from a parent folder of local Git repositories.
package from

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/workspace"
)

var glueNames = []string{
	"compose.yaml",
	"compose.yml",
	"docker-compose.yaml",
	"docker-compose.yml",
}

// RigAction is what apply should do for one discovered repository.
type RigAction int

const (
	// ActionAdd registers a new Rig.
	ActionAdd RigAction = iota
	// ActionSkip leaves an already-matching Rig in place.
	ActionSkip
)

// Rig is one planned Rig derived from a local Git repository.
type Rig struct {
	SourcePath string
	Name       string
	GitURL     string
	LocalRepo  string
	Branch     string
	Prefix     string
	Action     RigAction
	SkipReason string
}

// Plan is the dry-run and apply input for gt from.
type Plan struct {
	ParentAbs  string
	TownAbs    string
	TownExists bool
	Rigs       []Rig
	Glue       []string
	Skipped    []string
}

// Prepare resolves paths, discovers candidate repositories, and fails before
// any Town writes when the layout is invalid.
func Prepare(parent, town string) (*Plan, error) {
	parentAbs, err := resolvePath(parent)
	if err != nil {
		return nil, fmt.Errorf("resolving parent path: %w", err)
	}
	info, err := os.Stat(parentAbs)
	if err != nil {
		return nil, fmt.Errorf("parent folder: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("parent %s is not a directory", parentAbs)
	}

	townAbs, err := resolveTownPath(parentAbs, town)
	if err != nil {
		return nil, err
	}
	if pathInside(townAbs, parentAbs) {
		return nil, fmt.Errorf("Town path %s is inside parent folder %s; HQ files must not mix with project repositories", townAbs, parentAbs)
	}

	if townRoot, err := workspace.Find(parentAbs); err == nil && townRoot != "" {
		return nil, fmt.Errorf("parent %s is inside Gas Town HQ %s; gt from scans a folder of project repositories, not a Town", parentAbs, townRoot)
	}

	plan := &Plan{
		ParentAbs: parentAbs,
		TownAbs:   townAbs,
	}

	townInfo, townStatErr := os.Stat(townAbs)
	switch {
	case townStatErr == nil:
		if !townInfo.IsDir() {
			return nil, fmt.Errorf("Town path %s exists and is not a directory", townAbs)
		}
		isTown, err := workspace.IsWorkspace(townAbs)
		if err != nil {
			return nil, fmt.Errorf("checking Town path: %w", err)
		}
		if !isTown {
			return nil, fmt.Errorf("path %s exists but is not a Gas Town HQ", townAbs)
		}
		plan.TownExists = true
	case os.IsNotExist(townStatErr):
		// New Town.
	default:
		return nil, fmt.Errorf("checking Town path: %w", townStatErr)
	}

	if err := discover(plan); err != nil {
		return nil, err
	}
	if err := preflight(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func resolveTownPath(parentAbs, town string) (string, error) {
	if strings.TrimSpace(town) == "" {
		return filepath.Join(filepath.Dir(parentAbs), filepath.Base(parentAbs)+".gt"), nil
	}
	abs, err := resolvePath(town)
	if err != nil {
		return "", fmt.Errorf("resolving Town path: %w", err)
	}
	return abs, nil
}

func resolvePath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("getting home directory: %w", err)
		}
		switch {
		case p == "~":
			p = home
		case strings.HasPrefix(p, "~/"):
			p = filepath.Join(home, p[2:])
		default:
			p = filepath.Join(home, p[1:])
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func pathInside(inner, outer string) bool {
	rel, err := filepath.Rel(outer, inner)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..")
}

func discover(plan *Plan) error {
	entries, err := os.ReadDir(plan.ParentAbs)
	if err != nil {
		return fmt.Errorf("reading parent folder: %w", err)
	}

	var children []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			if isGlueName(name) {
				plan.Glue = append(plan.Glue, name)
			}
			continue
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		child := filepath.Join(plan.ParentAbs, name)
		if looksLikeAssembledRig(child) {
			plan.Skipped = append(plan.Skipped, name+" (assembled Rig)")
			continue
		}
		if isGitDirRepo(child) {
			children = append(children, child)
		}
	}

	sources := children
	if len(sources) == 0 && isGitDirRepo(plan.ParentAbs) {
		sources = []string{plan.ParentAbs}
	}
	if len(sources) == 0 {
		return fmt.Errorf("no Git repositories found in %s", plan.ParentAbs)
	}

	existing, err := loadExistingRigs(plan)
	if err != nil {
		return err
	}

	for _, source := range sources {
		planned := planRig(source)
		classifyAgainstExisting(&planned, existing)
		plan.Rigs = append(plan.Rigs, planned)
	}
	return nil
}

func planRig(source string) Rig {
	name := rig.SanitizeName(filepath.Base(source))
	g := git.NewGit(source)
	gitURL := fileURL(source)
	if origin, err := g.ConfiguredRemoteURL("origin"); err == nil && strings.TrimSpace(origin) != "" {
		gitURL = strings.TrimSpace(origin)
	}
	return Rig{
		SourcePath: source,
		Name:       name,
		GitURL:     gitURL,
		LocalRepo:  source,
		Branch:     g.DefaultBranch(),
		Prefix:     rig.DeriveBeadsPrefix(name),
		Action:     ActionAdd,
	}
}

func classifyAgainstExisting(planned *Rig, existing map[string]config.RigEntry) {
	entry, ok := existing[planned.Name]
	if !ok {
		return
	}
	if samePath(entry.LocalRepo, planned.LocalRepo) || entry.GitURL == planned.GitURL {
		planned.Action = ActionSkip
		planned.SkipReason = "already registered"
		return
	}
}

func loadExistingRigs(plan *Plan) (map[string]config.RigEntry, error) {
	if !plan.TownExists {
		return nil, nil
	}
	rigsPath := filepath.Join(plan.TownAbs, "mayor", "rigs.json")
	cfg, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		return nil, fmt.Errorf("loading rigs.json: %w", err)
	}
	if cfg.Rigs == nil {
		return map[string]config.RigEntry{}, nil
	}
	return cfg.Rigs, nil
}

func preflight(plan *Plan) error {
	byName := make(map[string][]string)
	byPrefix := make(map[string][]string)
	for _, r := range plan.Rigs {
		byName[r.Name] = append(byName[r.Name], r.SourcePath)
		byPrefix[r.Prefix] = append(byPrefix[r.Prefix], r.Name)
		if rig.IsReservedName(r.Name) {
			return fmt.Errorf("folder %s sanitizes to reserved Rig name %q", r.SourcePath, r.Name)
		}
	}
	for name, paths := range byName {
		if len(paths) > 1 {
			return fmt.Errorf("folders %s sanitize to the same Rig name %q", strings.Join(paths, " and "), name)
		}
	}
	for prefix, names := range byPrefix {
		if uniqueCount(names) > 1 {
			return fmt.Errorf("Rigs %s would share Beads prefix %q", strings.Join(unique(names), " and "), prefix)
		}
	}

	existing, err := loadExistingRigs(plan)
	if err != nil {
		return err
	}
	for _, r := range plan.Rigs {
		entry, ok := existing[r.Name]
		if !ok {
			continue
		}
		if r.Action == ActionSkip {
			continue
		}
		return fmt.Errorf("Rig %q already exists for a different source (%s)", r.Name, entry.LocalRepo)
	}
	return nil
}

func isGitDirRepo(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && info.IsDir()
}

func looksLikeAssembledRig(path string) bool {
	if info, err := os.Stat(filepath.Join(path, ".repo.git")); err == nil && info.IsDir() {
		return true
	}
	data, err := os.ReadFile(filepath.Join(path, "config.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}
	return cfg.Type == "rig"
}

func isGlueName(name string) bool {
	for _, glue := range glueNames {
		if name == glue {
			return true
		}
	}
	return false
}

func fileURL(abs string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return a == b
}

func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func uniqueCount(values []string) int {
	return len(unique(values))
}
