package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
)

// determineRigBeadsPath returns the correct route path for a rig based on its actual layout.
// Uses ResolveBeadsDir to follow any redirects (e.g., rig/.beads/redirect -> mayor/rig/.beads).
// Falls back to the default mayor layout path if the resolved path is invalid or escapes the town root.
func determineRigBeadsPath(townRoot, rigName string) string {
	defaultPath := rigName + "/mayor/rig"
	rigPath := filepath.Join(townRoot, rigName)
	resolved := beads.ResolveBeadsDir(rigPath)

	rel, err := filepath.Rel(townRoot, resolved)
	if err != nil {
		return defaultPath
	}

	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return defaultPath
	}
	return strings.TrimSuffix(rel, "/.beads")
}

func hasRealBeadsDir(targetPath string) bool {
	beadsPath := filepath.Join(targetPath, ".beads")
	_, err := os.Stat(beadsPath)
	return err == nil
}

func isRedirectDependent(townRoot, routePath string) bool {
	fullPath := filepath.Join(townRoot, routePath)
	redirectPath := filepath.Join(fullPath, ".beads", "redirect")
	_, err := os.Stat(redirectPath)
	return err == nil
}

type routesScan struct {
	ctx                *CheckContext
	routes             []beads.Route
	routeByPrefix      map[string]string
	routeByPath        map[string]string
	details            []string
	missingTownRoute   bool
	missingConvoyRoute bool
	missingRigs        []string
	invalidRoutes      []string
	suboptimalRoutes   []string
}

func runRoutesCheck(c *RoutesCheck, ctx *CheckContext) *CheckResult {
	scan, early := prepareRoutesScan(ctx)
	if early != nil {
		early.Name = c.Name()
		return early
	}
	if result := scanRigsOrValidate(c, scan); result != nil {
		return result
	}
	return routesScanResult(c, scan)
}

func prepareRoutesScan(ctx *CheckContext) (*routesScan, *CheckResult) {
	beadsDir := filepath.Join(ctx.TownRoot, ".beads")
	routesPath := filepath.Join(beadsDir, beads.RoutesFileName)
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return nil, &CheckResult{
			Status:  StatusWarning,
			Message: "No .beads directory at town root",
			FixHint: "Run 'bd init' to initialize beads",
		}
	}
	if _, err := os.Stat(routesPath); os.IsNotExist(err) {
		return nil, &CheckResult{
			Status:  StatusWarning,
			Message: "No routes.jsonl file (prefix routing not configured)",
			FixHint: "Run 'gt doctor --fix' to create routes.jsonl",
		}
	}
	routes, err := beads.LoadRoutes(beadsDir)
	if err != nil {
		return nil, &CheckResult{
			Status:  StatusError,
			Message: fmt.Sprintf("Failed to load routes.jsonl: %v", err),
		}
	}
	return newRoutesScan(ctx, routes), nil
}

func newRoutesScan(ctx *CheckContext, routes []beads.Route) *routesScan {
	scan := &routesScan{
		ctx:           ctx,
		routes:        routes,
		routeByPrefix: make(map[string]string),
		routeByPath:   make(map[string]string),
	}
	for _, r := range routes {
		scan.routeByPrefix[r.Prefix] = r.Path
		scan.routeByPath[r.Path] = r.Prefix
	}
	if _, hasTownRoute := scan.routeByPrefix["hq-"]; !hasTownRoute {
		scan.missingTownRoute = true
		scan.details = append(scan.details, "Town root route (hq- -> .) is missing")
	}
	if _, hasConvoyRoute := scan.routeByPrefix["hq-cv-"]; !hasConvoyRoute {
		scan.missingConvoyRoute = true
		scan.details = append(scan.details, "Convoy route (hq-cv- -> .) is missing")
	}
	return scan
}

func scanRigsOrValidate(c *RoutesCheck, scan *routesScan) *CheckResult {
	rigsPath := filepath.Join(scan.ctx.TownRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		if scan.missingTownRoute || scan.missingConvoyRoute {
			return &CheckResult{
				Name:    c.Name(),
				Status:  StatusWarning,
				Message: "Required town routes are missing",
				Details: scan.details,
				FixHint: "Run 'gt doctor --fix' to add missing routes",
			}
		}
		return checkRoutesValid(c, scan.ctx, scan.routes)
	}
	scanRigRoutes(scan, rigsConfig)
	scanInvalidRoutes(scan)
	return nil
}

func scanRigRoutes(scan *routesScan, rigsConfig *config.RigsConfig) {
	for rigName, rigEntry := range rigsConfig.Rigs {
		expectedPath := determineRigBeadsPath(scan.ctx.TownRoot, rigName)
		if _, hasRoute := scan.routeByPath[expectedPath]; hasRoute {
			continue
		}
		noteMissingOrSuboptimalRigRoute(scan, rigName, rigPrefix(rigEntry), expectedPath)
	}
}

func rigPrefix(rigEntry config.RigEntry) string {
	if rigEntry.BeadsConfig != nil && rigEntry.BeadsConfig.Prefix != "" {
		return rigEntry.BeadsConfig.Prefix + "-"
	}
	return ""
}

func noteMissingOrSuboptimalRigRoute(scan *routesScan, rigName, prefix, expectedPath string) {
	if prefix == "" {
		return
	}
	existingPath, found := scan.routeByPrefix[prefix]
	if !found {
		scan.missingRigs = append(scan.missingRigs, rigName)
		scan.details = append(scan.details, fmt.Sprintf("Rig '%s' (prefix: %s) has no routing entry", rigName, prefix))
		return
	}
	if existingPath != expectedPath && isRedirectDependent(scan.ctx.TownRoot, existingPath) {
		scan.suboptimalRoutes = append(scan.suboptimalRoutes, prefix)
		scan.details = append(scan.details, fmt.Sprintf("Route %s -> %s should be %s -> %s (avoids redirect resolution bug)", prefix, existingPath, prefix, expectedPath))
	}
}

func scanInvalidRoutes(scan *routesScan) {
	suboptimalSet := make(map[string]bool, len(scan.suboptimalRoutes))
	for _, p := range scan.suboptimalRoutes {
		suboptimalSet[p] = true
	}
	for _, r := range scan.routes {
		if r.Path == "." || suboptimalSet[r.Prefix] {
			continue
		}
		noteInvalidRoute(scan, r)
	}
}

func noteInvalidRoute(scan *routesScan, r beads.Route) {
	rigPath := filepath.Join(scan.ctx.TownRoot, r.Path)
	if _, err := os.Stat(rigPath); os.IsNotExist(err) {
		scan.invalidRoutes = append(scan.invalidRoutes, r.Prefix)
		scan.details = append(scan.details, fmt.Sprintf("Route %s -> %s: path does not exist", r.Prefix, r.Path))
		return
	}
	beadsPath := filepath.Join(rigPath, ".beads")
	redirectPath := filepath.Join(beadsPath, "redirect")
	_, beadsErr := os.Stat(beadsPath)
	_, redirectErr := os.Stat(redirectPath)
	if os.IsNotExist(beadsErr) && os.IsNotExist(redirectErr) {
		scan.invalidRoutes = append(scan.invalidRoutes, r.Prefix)
		scan.details = append(scan.details, fmt.Sprintf("Route %s -> %s: no .beads directory", r.Prefix, r.Path))
	}
}

func routesScanResult(c *RoutesCheck, scan *routesScan) *CheckResult {
	if !scan.hasIssues() {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("Routes configured correctly (%d routes)", len(scan.routes)),
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: strings.Join(scan.issueMessages(), ", "),
		Details: scan.details,
		FixHint: "Run 'gt doctor --fix' to fix routing issues",
	}
}

func (scan *routesScan) hasIssues() bool {
	return scan.missingTownRoute || scan.missingConvoyRoute ||
		len(scan.missingRigs) > 0 || len(scan.invalidRoutes) > 0 || len(scan.suboptimalRoutes) > 0
}

func (scan *routesScan) issueMessages() []string {
	var parts []string
	parts = appendMissingRouteMessages(parts, scan.missingTownRoute, scan.missingConvoyRoute)
	return appendRouteCountMessages(parts, scan)
}

func appendMissingRouteMessages(parts []string, missingTown, missingConvoy bool) []string {
	if missingTown {
		parts = append(parts, "town root route missing")
	}
	if missingConvoy {
		parts = append(parts, "convoy route missing")
	}
	return parts
}

func appendRouteCountMessages(parts []string, scan *routesScan) []string {
	if len(scan.missingRigs) > 0 {
		parts = append(parts, fmt.Sprintf("%d rig(s) missing routes", len(scan.missingRigs)))
	}
	if len(scan.invalidRoutes) > 0 {
		parts = append(parts, fmt.Sprintf("%d invalid route(s)", len(scan.invalidRoutes)))
	}
	if len(scan.suboptimalRoutes) > 0 {
		parts = append(parts, fmt.Sprintf("%d route(s) using redirect instead of canonical path", len(scan.suboptimalRoutes)))
	}
	return parts
}

func checkRoutesValid(c *RoutesCheck, ctx *CheckContext, routes []beads.Route) *CheckResult {
	var details []string
	invalidCount := 0
	for _, r := range routes {
		if r.Path == "." {
			continue
		}
		rigPath := filepath.Join(ctx.TownRoot, r.Path)
		if _, err := os.Stat(rigPath); os.IsNotExist(err) {
			invalidCount++
			details = append(details, fmt.Sprintf("Route %s -> %s: path does not exist", r.Prefix, r.Path))
		}
	}
	if invalidCount > 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d invalid route(s) in routes.jsonl", invalidCount),
			Details: details,
			FixHint: "Remove invalid routes or recreate the missing rigs",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("Routes configured correctly (%d routes)", len(routes)),
	}
}

func fixRoutes(_ *RoutesCheck, ctx *CheckContext) error {
	beadsDir := filepath.Join(ctx.TownRoot, ".beads")
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return fmt.Errorf(".beads directory does not exist; run 'bd init' first")
	}
	routes, routeMap := loadRoutesForFix(beadsDir)
	modified := ensureTownRoutes(&routes, routeMap)
	if applyRigRouteFixes(ctx, &routes, routeMap) {
		modified = true
	} else if !modified {
		return nil
	}
	return writeRoutesIfNeeded(beadsDir, routes, modified)
}

func loadRoutesForFix(beadsDir string) ([]beads.Route, map[string]int) {
	routes, err := beads.LoadRoutes(beadsDir)
	if err != nil {
		routes = []beads.Route{}
	}
	routeMap := make(map[string]int)
	for i, r := range routes {
		routeMap[r.Prefix] = i
	}
	return routes, routeMap
}

func ensureTownRoutes(routes *[]beads.Route, routeMap map[string]int) bool {
	modified := false
	if _, exists := routeMap["hq-"]; !exists {
		routeMap["hq-"] = len(*routes)
		*routes = append(*routes, beads.Route{Prefix: "hq-", Path: "."})
		modified = true
	}
	if _, exists := routeMap["hq-cv-"]; !exists {
		routeMap["hq-cv-"] = len(*routes)
		*routes = append(*routes, beads.Route{Prefix: "hq-cv-", Path: "."})
		modified = true
	}
	return modified
}

func applyRigRouteFixes(ctx *CheckContext, routes *[]beads.Route, routeMap map[string]int) bool {
	rigsPath := filepath.Join(ctx.TownRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsPath)
	if err != nil {
		return false
	}
	prefixCount := countRigPrefixes(rigsConfig)
	modified := false
	for rigName, rigEntry := range rigsConfig.Rigs {
		if applyOneRigRouteFix(ctx, routes, routeMap, prefixCount, rigName, rigEntry) {
			modified = true
		}
	}
	return modified
}

func countRigPrefixes(rigsConfig *config.RigsConfig) map[string]int {
	prefixCount := make(map[string]int)
	for _, rigEntry := range rigsConfig.Rigs {
		if prefix := rigPrefix(rigEntry); prefix != "" {
			prefixCount[prefix]++
		}
	}
	return prefixCount
}

func applyOneRigRouteFix(ctx *CheckContext, routes *[]beads.Route, routeMap, prefixCount map[string]int, rigName string, rigEntry config.RigEntry) bool {
	prefix := rigPrefix(rigEntry)
	if prefix == "" {
		return false
	}
	if prefixCount[prefix] > 1 {
		fmt.Fprintf(os.Stderr, "Warning: skipping route fix for duplicate prefix %s (%d rigs share it)\n",
			prefix, prefixCount[prefix])
		return false
	}
	rigRoutePath := determineRigBeadsPath(ctx.TownRoot, rigName)
	canonicalPath := filepath.Join(ctx.TownRoot, rigRoutePath)
	if idx, exists := routeMap[prefix]; exists {
		return rewriteRedirectRoute(ctx, *routes, idx, prefix, rigRoutePath, canonicalPath)
	}
	return addMissingRigRoute(routes, routeMap, prefix, rigRoutePath, canonicalPath)
}

func rewriteRedirectRoute(ctx *CheckContext, routes []beads.Route, idx int, prefix, rigRoutePath, canonicalPath string) bool {
	if routes[idx].Path == rigRoutePath || !isRedirectDependent(ctx.TownRoot, routes[idx].Path) {
		return false
	}
	if hasRealBeadsDir(canonicalPath) {
		routes[idx].Path = rigRoutePath
		return true
	}
	fmt.Fprintf(os.Stderr, "Warning: cannot rewrite route %s -> %s to %s (canonical path has no .beads directory)\n",
		prefix, routes[idx].Path, rigRoutePath)
	return false
}

func addMissingRigRoute(routes *[]beads.Route, routeMap map[string]int, prefix, rigRoutePath, canonicalPath string) bool {
	if !hasRealBeadsDir(canonicalPath) {
		return false
	}
	routeMap[prefix] = len(*routes)
	*routes = append(*routes, beads.Route{Prefix: prefix, Path: rigRoutePath})
	return true
}

func writeRoutesIfNeeded(beadsDir string, routes []beads.Route, modified bool) error {
	if !modified {
		return nil
	}
	return beads.WriteRoutes(beadsDir, routes)
}
