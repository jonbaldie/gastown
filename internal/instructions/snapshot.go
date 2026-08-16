package instructions

import (
	"os"
	"os/exec"
	"path/filepath"
)

type fsEntry struct {
	exists  bool
	symlink bool
	regular bool
	target  string
	content string
	tracked bool
}

type dirSnap struct {
	dir         string
	agents      fsEntry
	claude      fsEntry
	agentsLocal fsEntry
	claudeLocal fsEntry
	gemini      fsEntry
}

func snapshot(dir string) dirSnap {
	return dirSnap{
		dir:         dir,
		agents:      readEntry(dir, CanonicalFile),
		claude:      readEntry(dir, AliasFile),
		agentsLocal: readEntry(dir, LocalCanonicalFile),
		claudeLocal: readEntry(dir, LocalAliasFile),
		gemini:      readEntry(dir, GeminiAliasFile),
	}
}

func readEntry(dir, name string) fsEntry {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return fsEntry{}
	}
	entry := fsEntry{exists: true, tracked: isTracked(dir, name)}
	if info.Mode()&os.ModeSymlink != 0 {
		entry.symlink = true
		if target, err := os.Readlink(path); err == nil {
			entry.target = target
		}
		return entry
	}
	if info.Mode().IsRegular() {
		entry.regular = true
		if data, err := os.ReadFile(path); err == nil {
			entry.content = string(data)
		}
	}
	return entry
}

func isTracked(dir, name string) bool {
	cmd := exec.Command("git", "-C", dir, "ls-files", "--error-unmatch", "--", name)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

func (s dirSnap) entry(name string) fsEntry {
	switch name {
	case CanonicalFile:
		return s.agents
	case AliasFile:
		return s.claude
	case LocalCanonicalFile:
		return s.agentsLocal
	case LocalAliasFile:
		return s.claudeLocal
	case GeminiAliasFile:
		return s.gemini
	default:
		return fsEntry{}
	}
}

func isConstitution(entry fsEntry) bool {
	if !entry.regular {
		return false
	}
	if entry.tracked {
		return true
	}
	return !IsGasTownOverlay(entry.content)
}

func hasConstitution(s dirSnap) bool {
	return isConstitution(s.agents) || isConstitution(s.claude)
}

func symlinkPointsAt(entry fsEntry, target string) bool {
	if !entry.symlink {
		return false
	}
	return filepath.Clean(entry.target) == target || filepath.Base(filepath.Clean(entry.target)) == target
}
