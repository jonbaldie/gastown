package instructions

import "strings"

type pairNames struct {
	canonical string
	alias     string
}

type provisionPlan struct {
	canonicalName string
	aliasName     string
	canonicalBody string
	writeBody     bool
	writeAlias    bool
	remove        []string
}

func planProvision(snap dirSnap, content, skipIfContains string) provisionPlan {
	names := provisionPairNames(snap)

	body, writeBody := canonicalBody(snap, names, content, skipIfContains)
	plan := provisionPlan{
		canonicalName: names.canonical,
		aliasName:     names.alias,
		canonicalBody: body,
		writeBody:     writeBody,
	}
	planAliasChanges(&plan, snap, names)
	planCanonicalChanges(&plan, snap, names)
	return plan
}

func provisionPairNames(snap dirSnap) pairNames {
	if hasConstitution(snap) {
		return pairNames{canonical: LocalCanonicalFile, alias: LocalAliasFile}
	}
	return pairNames{canonical: CanonicalFile, alias: AliasFile}
}

func planAliasChanges(plan *provisionPlan, snap dirSnap, names pairNames) {
	alias := snap.Entry(names.alias)
	if !symlinkPointsAt(alias, names.canonical) {
		plan.writeAlias = true
		if alias.exists {
			plan.remove = append(plan.remove, names.alias)
		}
	}
}

func planCanonicalChanges(plan *provisionPlan, snap dirSnap, names pairNames) {
	if !plan.writeBody {
		return
	}
	if snap.Entry(names.canonical).symlink {
		plan.remove = appendUnique(plan.remove, names.canonical)
	}
	if names.canonical == CanonicalFile && snap.claude.regular && !isConstitution(snap.claude) {
		plan.remove = appendUnique(plan.remove, AliasFile)
		plan.writeAlias = true
	}
	if names.canonical == LocalCanonicalFile && snap.claudeLocal.regular {
		plan.remove = appendUnique(plan.remove, LocalAliasFile)
		plan.writeAlias = true
	}
}

func canonicalBody(snap dirSnap, names pairNames, content, skipIfContains string) (string, bool) {
	current := snap.Entry(names.canonical)
	if current.regular {
		if skipIfContains != "" && strings.Contains(current.content, skipIfContains) {
			return current.content, false
		}
		if current.content == content {
			return current.content, false
		}
		return content, true
	}

	if migrated, ok := migrateOverlay(snap, names, skipIfContains); ok {
		if skipIfContains != "" {
			return migrated, true
		}
		return content, true
	}

	return content, true
}

func migrateOverlay(snap dirSnap, names pairNames, skipIfContains string) (string, bool) {
	switch names.canonical {
	case CanonicalFile:
		return migrateTownOverlay(snap, skipIfContains)
	case LocalCanonicalFile:
		return migrateOverlayEntry(snap.claudeLocal, skipIfContains)
	default:
		return "", false
	}
}

func migrateTownOverlay(snap dirSnap, skipIfContains string) (string, bool) {
	if !isConstitution(snap.claude) {
		if migrated, ok := migrateOverlayEntry(snap.claude, skipIfContains); ok {
			return migrated, true
		}
	}
	if symlinkPointsAt(snap.agents, AliasFile) && isGasTownOverlayEntry(snap.claude) {
		return snap.claude.content, true
	}
	return "", false
}

func migrateOverlayEntry(entry fsEntry, skipIfContains string) (string, bool) {
	if !isGasTownOverlayEntry(entry) {
		return "", false
	}
	if skipIfContains != "" && !strings.Contains(entry.content, skipIfContains) {
		return "", false
	}
	return entry.content, true
}

func isGasTownOverlayEntry(entry fsEntry) bool {
	return entry.regular && IsGasTownOverlay(entry.content)
}

func appendUnique(items []string, name string) []string {
	for _, item := range items {
		if item == name {
			return items
		}
	}
	return append(items, name)
}

func planNoop(plan provisionPlan) bool {
	return !plan.writeBody && !plan.writeAlias
}

// TownPairValid reports whether dir has AGENTS.md as a regular file and
// CLAUDE.md as a symlink to it.
func TownPairValid(dir string) bool {
	snap := snapshot(dir)
	return snap.agents.regular && symlinkPointsAt(snap.claude, CanonicalFile)
}
