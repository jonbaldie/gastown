package doctor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/instructions"
	"github.com/jonbaldie/gastown/internal/runtime"
	"github.com/jonbaldie/gastown/internal/templates"
)

func runPrimingCheck(c *PrimingCheck, ctx *CheckContext) *CheckResult {
	c.issues = nil
	var details []string
	details = append(details, collectSystemPrimingIssues(c)...)
	details = append(details, collectTownRootPrimingIssues(c, ctx)...)
	details = append(details, collectTownAgentPrimingIssues(c, ctx)...)
	details = appendIssueDetails(details, c, checkRigPriming(ctx.TownRoot))
	if len(c.issues) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Priming subsystem is correctly configured",
		}
	}
	return primingCheckErrorResult(c, details)
}

func collectSystemPrimingIssues(c *PrimingCheck) []string {
	if err := exec.Command("which", "gt").Run(); err == nil {
		return nil
	}
	c.issues = append(c.issues, primingIssue{
		location:    "system",
		issueType:   "gt_not_in_path",
		description: "gt binary not found in PATH",
		fixable:     false,
	})
	return []string{"gt binary not found in PATH"}
}

func collectTownRootPrimingIssues(c *PrimingCheck, ctx *CheckContext) []string {
	townRootAgents := filepath.Join(ctx.TownRoot, "AGENTS.md")
	townRootClaude := filepath.Join(ctx.TownRoot, "CLAUDE.md")
	if fileExists(townRootAgents) || fileExists(townRootClaude) {
		return nil
	}
	c.issues = append(c.issues, primingIssue{
		location:    "town-root",
		issueType:   "missing_town_agents_md",
		description: "Missing AGENTS.md at town root (identity anchor for Mayor/Deacon)",
		fixable:     true,
	})
	return []string{"town-root: Missing AGENTS.md identity anchor"}
}

func collectTownAgentPrimingIssues(c *PrimingCheck, ctx *CheckContext) []string {
	var details []string
	details = appendIssueDetails(details, c, checkAgentPriming(ctx.TownRoot, "mayor", "mayor", ""))
	details = append(details, collectStaleMayorInstructionIssues(c, ctx)...)
	deaconPath := filepath.Join(ctx.TownRoot, "deacon")
	if dirExists(deaconPath) {
		details = appendIssueDetails(details, c, checkAgentPriming(ctx.TownRoot, "deacon", "deacon", ""))
	}
	return details
}

func collectStaleMayorInstructionIssues(c *PrimingCheck, ctx *CheckContext) []string {
	var details []string
	mayorDir := filepath.Join(ctx.TownRoot, "mayor")
	for _, filename := range []string{"CLAUDE.md", "AGENTS.md"} {
		if !fileExists(filepath.Join(mayorDir, filename)) {
			continue
		}
		issue := primingIssue{
			location:    "mayor",
			issueType:   "stale_intermediate_instructions_md",
			description: fmt.Sprintf("Stale %s at intermediate directory (no longer needed)", filename),
			fixable:     true,
		}
		c.issues = append(c.issues, issue)
		details = append(details, fmt.Sprintf("%s: %s", issue.location, issue.description))
	}
	return details
}

func appendIssueDetails(details []string, c *PrimingCheck, issues []primingIssue) []string {
	for _, issue := range issues {
		details = append(details, fmt.Sprintf("%s: %s", issue.location, issue.description))
	}
	c.issues = append(c.issues, issues...)
	return details
}

func primingCheckErrorResult(c *PrimingCheck, details []string) *CheckResult {
	fixableCount := 0
	for _, issue := range c.issues {
		if issue.fixable {
			fixableCount++
		}
	}
	fixHint := ""
	if fixableCount > 0 {
		fixHint = fmt.Sprintf("Run 'gt doctor --fix' to fix %d issue(s)", fixableCount)
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("Found %d priming issue(s)", len(c.issues)),
		Details: details,
		FixHint: fixHint,
	}
}

func checkAgentPriming(townRoot, agentDir, agentType, rigName string) []primingIssue {
	var issues []primingIssue
	agentPath := filepath.Join(townRoot, agentDir)
	issues = append(issues, checkAgentPrimeHook(agentPath, agentDir, agentType, rigName)...)
	return append(issues, checkAgentClaudeMdSize(agentPath, agentDir)...)
}

func checkAgentPrimeHook(agentPath, agentDir, agentType, rigName string) []primingIssue {
	settingsPath := filepath.Join(agentPath, ".claude", "settings.json")
	if !fileExists(settingsPath) {
		return nil
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil
	}
	if hasGtPrimeHook(settings) {
		return nil
	}
	return []primingIssue{{
		location:    agentDir,
		issueType:   "no_prime_hook",
		description: "SessionStart hook missing 'gt prime'",
		fixable:     true,
		agentType:   agentType,
		rigName:     rigName,
	}}
}

func checkAgentClaudeMdSize(agentPath, agentDir string) []primingIssue {
	claudeMdPath := filepath.Join(agentPath, "CLAUDE.md")
	if !fileExists(claudeMdPath) {
		return nil
	}
	lines := countFileLines(claudeMdPath)
	if lines <= 30 {
		return nil
	}
	return []primingIssue{{
		location:    agentDir,
		issueType:   "large_claude_md",
		description: fmt.Sprintf("CLAUDE.md has %d lines (should be <30 for bootstrap pointer)", lines),
		fixable:     false,
	}}
}

func checkRigPriming(townRoot string) []primingIssue {
	var issues []primingIssue
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return issues
	}
	for _, entry := range entries {
		if !entry.IsDir() || isSkippedPrimingRig(entry.Name()) {
			continue
		}
		issues = append(issues, checkOneRigPriming(townRoot, entry.Name())...)
	}
	return issues
}

func isSkippedPrimingRig(name string) bool {
	return name == "mayor" || name == "deacon" || name == "daemon" || name == "docs" || name[0] == '.'
}

func checkOneRigPriming(townRoot, rigName string) []primingIssue {
	rigPath := filepath.Join(townRoot, rigName)
	if !dirExists(filepath.Join(rigPath, ".beads")) {
		return nil
	}
	var issues []primingIssue
	issues = append(issues, checkRigPrimeMd(rigPath, rigName)...)
	issues = append(issues, checkStaleRoleInstructionFiles(rigPath, rigName)...)
	issues = append(issues, checkRigRolePriming(townRoot, rigName, "witness")...)
	issues = append(issues, checkRigRolePriming(townRoot, rigName, "refinery")...)
	issues = append(issues, checkCrewPrimeMd(rigPath, rigName)...)
	return append(issues, checkPolecatPriming(rigPath, rigName)...)
}

func checkRigPrimeMd(rigPath, rigName string) []primingIssue {
	primeMdPath := filepath.Join(beads.ResolveBeadsDir(rigPath), "PRIME.md")
	if fileExists(primeMdPath) {
		return nil
	}
	return []primingIssue{{
		location:    rigName,
		issueType:   "missing_prime_md",
		description: "Missing .beads/PRIME.md (Gas Town context fallback)",
		fixable:     true,
	}}
}

func checkStaleRoleInstructionFiles(rigPath, rigName string) []primingIssue {
	var issues []primingIssue
	for _, role := range []string{"refinery", "witness", "crew", "polecats"} {
		agentPath := filepath.Join(rigPath, role)
		if !dirExists(agentPath) {
			continue
		}
		for _, filename := range []string{"CLAUDE.md", "AGENTS.md"} {
			if !fileExists(filepath.Join(agentPath, filename)) {
				continue
			}
			issues = append(issues, primingIssue{
				location:    fmt.Sprintf("%s/%s", rigName, role),
				issueType:   "stale_intermediate_instructions_md",
				description: fmt.Sprintf("Stale %s at intermediate directory (no longer needed)", filename),
				fixable:     true,
			})
		}
	}
	return issues
}

func checkRigRolePriming(townRoot, rigName, role string) []primingIssue {
	if !dirExists(filepath.Join(townRoot, rigName, role)) {
		return nil
	}
	return checkAgentPriming(townRoot, filepath.Join(rigName, role), role, rigName)
}

func checkCrewPrimeMd(rigPath, rigName string) []primingIssue {
	crewDir := filepath.Join(rigPath, "crew")
	if !dirExists(crewDir) {
		return nil
	}
	var issues []primingIssue
	crewEntries, _ := os.ReadDir(crewDir)
	for _, crewEntry := range crewEntries {
		if !crewEntry.IsDir() || crewEntry.Name() == ".claude" {
			continue
		}
		primeMdPath := filepath.Join(beads.ResolveBeadsDir(filepath.Join(crewDir, crewEntry.Name())), "PRIME.md")
		if fileExists(primeMdPath) {
			continue
		}
		issues = append(issues, primingIssue{
			location:    fmt.Sprintf("%s/crew/%s", rigName, crewEntry.Name()),
			issueType:   "missing_prime_md",
			description: "Missing PRIME.md (Gas Town context fallback)",
			fixable:     true,
		})
	}
	return issues
}

func checkPolecatPriming(rigPath, rigName string) []primingIssue {
	polecatsDir := filepath.Join(rigPath, "polecats")
	if !dirExists(polecatsDir) {
		return nil
	}
	var issues []primingIssue
	pcEntries, _ := os.ReadDir(polecatsDir)
	for _, pcEntry := range pcEntries {
		if !pcEntry.IsDir() || pcEntry.Name() == ".claude" {
			continue
		}
		issues = append(issues, checkOnePolecatPriming(polecatsDir, rigName, pcEntry.Name())...)
	}
	return issues
}

func checkOnePolecatPriming(polecatsDir, rigName, polecatName string) []primingIssue {
	var issues []primingIssue
	polecatDir := filepath.Join(polecatsDir, polecatName)
	if dirExists(filepath.Join(polecatDir, ".beads")) {
		issues = append(issues, primingIssue{
			location:    fmt.Sprintf("%s/polecats/%s", rigName, polecatName),
			issueType:   "orphaned_beads_dir",
			description: "Orphaned .beads directory at wrong level (should be in worktree)",
			fixable:     true,
		})
	}
	polecatWorktree := filepath.Join(polecatDir, rigName)
	if !dirExists(polecatWorktree) {
		return issues
	}
	if fileExists(filepath.Join(beads.ResolveBeadsDir(polecatWorktree), "PRIME.md")) {
		return issues
	}
	return append(issues, primingIssue{
		location:    fmt.Sprintf("%s/polecats/%s/%s", rigName, polecatName, rigName),
		issueType:   "missing_prime_md",
		description: "Missing PRIME.md (Gas Town context fallback)",
		fixable:     true,
	})
}

func hasGtPrimeHook(settings map[string]any) bool {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return false
	}
	hookList, ok := hooks["SessionStart"].([]any)
	if !ok {
		return false
	}
	for _, hook := range hookList {
		if hookCommandHasPattern(hook, "gt prime") {
			return true
		}
	}
	return false
}

func countFileLines(path string) int {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		count++
	}
	return count
}

func fixPrimingIssues(c *PrimingCheck, ctx *CheckContext) error {
	var errors []string
	for _, issue := range c.issues {
		if !issue.fixable {
			continue
		}
		if err := fixOnePrimingIssue(ctx, issue); err != "" {
			errors = append(errors, err)
		}
	}
	if len(errors) > 0 {
		return fmt.Errorf("%s", strings.Join(errors, "; "))
	}
	return nil
}

func fixOnePrimingIssue(ctx *CheckContext, issue primingIssue) string {
	switch issue.issueType {
	case "no_prime_hook":
		return fixNoPrimeHook(ctx, issue)
	case "missing_town_agents_md":
		return fixMissingTownAgentsMD(ctx)
	case "orphaned_beads_dir":
		return fixOrphanedBeadsDir(ctx, issue)
	case "missing_prime_md":
		return fixMissingPrimeMD(ctx, issue)
	case "stale_intermediate_instructions_md":
		return fixStaleIntermediateInstructions(ctx, issue)
	default:
		return ""
	}
}

func fixNoPrimeHook(ctx *CheckContext, issue primingIssue) string {
	settingsPath := filepath.Join(ctx.TownRoot, issue.location, ".claude", "settings.json")
	if err := os.Remove(settingsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Sprintf("%s: failed to delete stale settings: %v", issue.location, err)
	}
	settingsDir := filepath.Join(ctx.TownRoot, issue.location)
	rigPath := ""
	if issue.rigName != "" {
		rigPath = filepath.Join(ctx.TownRoot, issue.rigName)
	}
	runtimeConfig := config.ResolveRoleAgentConfig(issue.agentType, ctx.TownRoot, rigPath)
	if err := runtime.EnsureSettingsForRole(settingsDir, settingsDir, issue.agentType, runtimeConfig); err != nil {
		return fmt.Sprintf("%s: failed to recreate settings: %v", issue.location, err)
	}
	return ""
}

func fixMissingTownAgentsMD(ctx *CheckContext) string {
	if _, err := instructions.Provision(ctx.TownRoot, templates.TownRootAgentsMD(), "# Gas Town"); err != nil {
		return fmt.Sprintf("town-root identity pair: %v", err)
	}
	return ""
}

func fixOrphanedBeadsDir(ctx *CheckContext, issue primingIssue) string {
	orphanedPath := filepath.Join(ctx.TownRoot, issue.location, ".beads")
	if err := os.RemoveAll(orphanedPath); err != nil {
		return fmt.Sprintf("%s: failed to remove orphaned .beads: %v", issue.location, err)
	}
	return ""
}

func fixMissingPrimeMD(ctx *CheckContext, issue primingIssue) string {
	worktreePath := filepath.Join(ctx.TownRoot, issue.location)
	if err := beads.ProvisionPrimeMDForWorktree(worktreePath); err != nil {
		return fmt.Sprintf("%s: %v", issue.location, err)
	}
	return ""
}

func fixStaleIntermediateInstructions(ctx *CheckContext, issue primingIssue) string {
	agentPath := filepath.Join(ctx.TownRoot, issue.location)
	for _, filename := range []string{"CLAUDE.md", "AGENTS.md"} {
		filePath := filepath.Join(agentPath, filename)
		if !fileExists(filePath) {
			continue
		}
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			return fmt.Sprintf("%s: failed to remove %s: %v", issue.location, filename, err)
		}
	}
	return ""
}
