package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/session"
)

func findClaudeSettingsFiles(townRoot string) []staleSettingsInfo {
	files := findTownRootStaleSettings(townRoot)
	files = appendTownRoleSettings(files, townRoot, "mayor", "hq-mayor")
	files = appendTownRoleSettings(files, townRoot, "deacon", "hq-deacon")
	return append(files, findRigClaudeSettingsFiles(townRoot)...)
}

func findTownRootStaleSettings(townRoot string) []staleSettingsInfo {
	var files []staleSettingsInfo
	staleTownRootSettings := filepath.Join(townRoot, ".claude", "settings.json")
	if fileExists(staleTownRootSettings) {
		files = append(files, staleSettingsInfo{
			path:          staleTownRootSettings,
			agentType:     "mayor",
			sessionName:   "hq-mayor",
			wrongLocation: true,
			gitStatus:     gitSettingsFileStatus(staleTownRootSettings),
			missing:       []string{"stale settings.json at town root (should not exist)"},
		})
	}
	staleTownRootLocal := filepath.Join(townRoot, ".claude", "settings.local.json")
	if fileExists(staleTownRootLocal) {
		files = append(files, staleSettingsInfo{
			path:          staleTownRootLocal,
			agentType:     "mayor",
			sessionName:   "hq-mayor",
			wrongLocation: true,
			gitStatus:     gitSettingsFileStatus(staleTownRootLocal),
			missing:       []string{"stale settings.local.json at town root (should not exist)"},
		})
	}
	staleTownRootCLAUDEmd := filepath.Join(townRoot, "CLAUDE.md")
	if fileExists(staleTownRootCLAUDEmd) && !isIdentityAnchor(staleTownRootCLAUDEmd) {
		files = append(files, staleSettingsInfo{
			path:          staleTownRootCLAUDEmd,
			agentType:     "mayor",
			sessionName:   "hq-mayor",
			wrongLocation: true,
			gitStatus:     gitSettingsFileStatus(staleTownRootCLAUDEmd),
			missing:       []string{"should be at mayor/CLAUDE.md, not town root"},
		})
	}
	return files
}

func appendTownRoleSettings(files []staleSettingsInfo, townRoot, role, sessionName string) []staleSettingsInfo {
	staleLocal := filepath.Join(townRoot, role, ".claude", "settings.local.json")
	if fileExists(staleLocal) {
		files = append(files, staleSettingsInfo{
			path:          staleLocal,
			agentType:     role,
			sessionName:   sessionName,
			wrongLocation: true,
			missing:       []string{"stale settings.local.json (should be settings.json)"},
		})
	}
	settings := filepath.Join(townRoot, role, ".claude", "settings.json")
	if fileExists(settings) {
		return append(files, staleSettingsInfo{path: settings, agentType: role, sessionName: sessionName})
	}
	if dirExists(filepath.Join(townRoot, role)) {
		return append(files, staleSettingsInfo{path: settings, agentType: role, sessionName: sessionName, missingFile: true})
	}
	return files
}

func findRigClaudeSettingsFiles(townRoot string) []staleSettingsInfo {
	var files []staleSettingsInfo
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return files
	}
	for _, entry := range entries {
		if !entry.IsDir() || isSkippedClaudeSettingsRig(entry.Name()) {
			continue
		}
		files = append(files, findOneRigClaudeSettings(townRoot, entry.Name())...)
	}
	return files
}

func isSkippedClaudeSettingsRig(name string) bool {
	return name == "mayor" || name == "deacon" || name == "daemon" || name == ".git" || name == "docs" || name[0] == '.'
}

func findOneRigClaudeSettings(townRoot, rigName string) []staleSettingsInfo {
	rigPath := filepath.Join(townRoot, rigName)
	files := appendPatrolRoleSettings(nil, rigPath, rigName, "witness", session.WitnessSessionName(session.PrefixFor(rigName)))
	files = appendPatrolRoleSettings(files, rigPath, rigName, "refinery", session.RefinerySessionName(session.PrefixFor(rigName)))
	files = appendCrewClaudeSettings(files, rigPath, rigName)
	files = appendPolecatClaudeSettings(files, rigPath, rigName)
	return appendRigRootStaleSettings(files, rigPath, rigName)
}

func appendPatrolRoleSettings(files []staleSettingsInfo, rigPath, rigName, role, sessionName string) []staleSettingsInfo {
	roleDir := filepath.Join(rigPath, role)
	if !dirExists(roleDir) {
		return files
	}
	files = appendCorrectOrMissingSettings(files, roleDir, role, rigName, sessionName)
	files = appendStaleParentLocal(files, roleDir, role, rigName, sessionName,
		fmt.Sprintf("stale settings.local.json (settings now in %s/.claude/settings.json)", role))
	return appendUntrackedStaleSettings(files, filepath.Join(roleDir, "rig", ".claude"), role, rigName, sessionName,
		fmt.Sprintf("stale settings in workdir (settings now in %s/.claude/settings.json)", role))
}

func appendCrewClaudeSettings(files []staleSettingsInfo, rigPath, rigName string) []staleSettingsInfo {
	crewDir := filepath.Join(rigPath, "crew")
	if !dirExists(crewDir) {
		return files
	}
	files = appendCorrectOrMissingSettings(files, crewDir, "crew", rigName, "")
	files = appendStaleParentLocal(files, crewDir, "crew", rigName, "",
		"stale settings.local.json in parent (settings now in crew/.claude/settings.json)")
	crewEntries, _ := os.ReadDir(crewDir)
	for _, crewEntry := range crewEntries {
		if !crewEntry.IsDir() || crewEntry.Name() == ".claude" {
			continue
		}
		sessionName := session.CrewSessionName(session.PrefixFor(rigName), crewEntry.Name())
		files = appendUntrackedStaleSettings(files, filepath.Join(crewDir, crewEntry.Name(), ".claude"), "crew", rigName, sessionName,
			"stale settings in workdir (settings now in crew/.claude/settings.json)")
	}
	return files
}

func appendPolecatClaudeSettings(files []staleSettingsInfo, rigPath, rigName string) []staleSettingsInfo {
	polecatsDir := filepath.Join(rigPath, "polecats")
	if !dirExists(polecatsDir) {
		return files
	}
	files = appendCorrectOrMissingSettings(files, polecatsDir, "polecat", rigName, "")
	files = appendStaleParentLocal(files, polecatsDir, "polecat", rigName, "",
		"stale settings.local.json in parent (settings now in polecats/.claude/settings.json)")
	polecatEntries, _ := os.ReadDir(polecatsDir)
	for _, pcEntry := range polecatEntries {
		if !pcEntry.IsDir() || pcEntry.Name() == ".claude" {
			continue
		}
		files = appendPolecatMemberStaleSettings(files, polecatsDir, rigName, pcEntry.Name())
	}
	return files
}

func appendPolecatMemberStaleSettings(files []staleSettingsInfo, polecatsDir, rigName, polecatName string) []staleSettingsInfo {
	sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
	for _, staleFile := range []string{"settings.json", "settings.local.json"} {
		stalePath := filepath.Join(polecatsDir, polecatName, ".claude", staleFile)
		if fileExists(stalePath) {
			files = append(files, staleSettingsInfo{
				path:          stalePath,
				agentType:     "polecat",
				rigName:       rigName,
				sessionName:   sessionName,
				wrongLocation: true,
				missing:       []string{"stale settings in intermediate dir (settings now in polecats/.claude/settings.json)"},
			})
		}
	}
	return appendUntrackedStaleSettings(files, filepath.Join(polecatsDir, polecatName, rigName, ".claude"), "polecat", rigName, sessionName,
		"stale settings in workdir (settings now in polecats/.claude/settings.json)")
}

func appendRigRootStaleSettings(files []staleSettingsInfo, rigPath, rigName string) []staleSettingsInfo {
	return appendUntrackedStaleSettings(files, filepath.Join(rigPath, ".claude"), "rig-root", rigName, "",
		"legacy rig-root settings (superseded by per-role files)")
}

func appendCorrectOrMissingSettings(files []staleSettingsInfo, parentDir, agentType, rigName, sessionName string) []staleSettingsInfo {
	correct := filepath.Join(parentDir, ".claude", "settings.json")
	info := staleSettingsInfo{path: correct, agentType: agentType, rigName: rigName, sessionName: sessionName}
	if !fileExists(correct) {
		info.missingFile = true
	}
	return append(files, info)
}

func appendStaleParentLocal(files []staleSettingsInfo, parentDir, agentType, rigName, sessionName, missing string) []staleSettingsInfo {
	staleLocal := filepath.Join(parentDir, ".claude", "settings.local.json")
	if !fileExists(staleLocal) {
		return files
	}
	return append(files, staleSettingsInfo{
		path:          staleLocal,
		agentType:     agentType,
		rigName:       rigName,
		sessionName:   sessionName,
		wrongLocation: true,
		missing:       []string{missing},
	})
}

func appendUntrackedStaleSettings(files []staleSettingsInfo, dir, agentType, rigName, sessionName, missing string) []staleSettingsInfo {
	for _, staleFile := range []string{"settings.json", "settings.local.json"} {
		stalePath := filepath.Join(dir, staleFile)
		if !fileExists(stalePath) {
			continue
		}
		gs := gitSettingsFileStatus(stalePath)
		if gs == gitStatusTrackedClean || gs == gitStatusTrackedModified {
			continue
		}
		info := staleSettingsInfo{
			path:          stalePath,
			agentType:     agentType,
			rigName:       rigName,
			sessionName:   sessionName,
			wrongLocation: true,
			missing:       []string{missing},
		}
		if agentType == "rig-root" {
			info.gitStatus = gs
		}
		files = append(files, info)
	}
	return files
}

func expectedStopPattern(agentType string) string {
	switch agentType {
	case "polecat", "polecats":
		return "polecat-stop-check"
	default:
		return "costs record"
	}
}

func checkClaudeSettings(path, agentType string) []string {
	var missing []string
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{"unreadable"}
	}
	var actual map[string]any
	if err := json.Unmarshal(data, &actual); err != nil {
		return []string{"invalid JSON"}
	}
	if _, ok := actual["enabledPlugins"]; !ok {
		missing = append(missing, "enabledPlugins")
	}
	hooks, ok := actual["hooks"].(map[string]any)
	if !ok {
		return append(missing, "hooks")
	}
	if !hookHasPattern(hooks, "SessionStart", "prime --hook") {
		missing = append(missing, "SessionStart hook (prime --hook)")
	}
	expected := expectedStopPattern(agentType)
	if !hookHasPattern(hooks, "Stop", expected) {
		missing = append(missing, fmt.Sprintf("Stop hook (%s)", expected))
	}
	return missing
}

func gitSettingsFileStatus(filePath string) gitFileStatus {
	dir := filepath.Dir(filePath)
	fileName := filepath.Base(filePath)
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--git-dir")
	if err := cmd.Run(); err != nil {
		return gitStatusUnknown
	}
	cmd = exec.Command("git", "-C", dir, "ls-files", fileName)
	output, err := cmd.Output()
	if err != nil {
		return gitStatusUnknown
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		cmd = exec.Command("git", "-C", dir, "check-ignore", "-q", fileName)
		if err := cmd.Run(); err == nil {
			return gitStatusIgnored
		}
		return gitStatusUntracked
	}
	cmd = exec.Command("git", "-C", dir, "diff", "--quiet", fileName)
	if err := cmd.Run(); err != nil {
		return gitStatusTrackedModified
	}
	cmd = exec.Command("git", "-C", dir, "diff", "--cached", "--quiet", fileName)
	if err := cmd.Run(); err != nil {
		return gitStatusTrackedModified
	}
	return gitStatusTrackedClean
}

func hookHasPattern(hooks map[string]any, hookName, pattern string) bool {
	hookList, ok := hooks[hookName].([]any)
	if !ok {
		return false
	}
	for _, hook := range hookList {
		if hookCommandHasPattern(hook, pattern) {
			return true
		}
	}
	return false
}

func hookCommandHasPattern(hook any, pattern string) bool {
	hookMap, ok := hook.(map[string]any)
	if !ok {
		return false
	}
	innerHooks, ok := hookMap["hooks"].([]any)
	if !ok {
		return false
	}
	for _, inner := range innerHooks {
		innerMap, ok := inner.(map[string]any)
		if !ok {
			continue
		}
		cmd, ok := innerMap["command"].(string)
		if ok && strings.Contains(cmd, pattern) {
			return true
		}
	}
	return false
}
