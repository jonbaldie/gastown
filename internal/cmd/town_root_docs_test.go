package cmd

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var hardcodedTownRootCommands = regexp.MustCompile(`(?m)(?:^|[` + "`" + `])((?:gt install|cd|rm -rf|bd -C) +~/gt)`)

func TestUserFacingTownRootDocsAreLocationIndependent(t *testing.T) {
	root := findModuleRoot(t)
	files := []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "docs", "INSTALLING.md"),
		filepath.Join(root, "docs", "reference.md"),
		filepath.Join(root, "docs", "WASTELAND.md"),
		filepath.Join(root, "docs", "PI.md"),
	}

	for _, path := range files {
		body := readRepoFile(t, path)
		if !strings.Contains(body, "GT_TOWN_ROOT") {
			t.Errorf("%s must mention GT_TOWN_ROOT so setup works away from the default Town root", path)
		}
		if matches := hardcodedTownRootCommands.FindAllStringSubmatch(body, -1); len(matches) > 0 {
			var found []string
			for _, m := range matches {
				found = append(found, m[1])
			}
			t.Errorf("%s hard-codes Town-root commands %v; use $GT_TOWN_ROOT (default ~/gt)", path, found)
		}
	}
}
