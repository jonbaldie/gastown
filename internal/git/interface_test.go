package git_test

import (
	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
)

// Compile-time assertion: Checker satisfies BranchChecker.
var _ beads.BranchChecker = git.Checker{}
