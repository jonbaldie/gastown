package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/polecat"
	"github.com/jonbaldie/gastown/internal/rig"
)

func stubUncommittedWorkCheckDeps(
	t *testing.T,
	listFn func(*rig.Rig) ([]*polecat.Polecat, error),
	checkFn func(string) (*git.UncommittedWorkStatus, error),
	isTTYFn func() bool,
	promptFn func(string) bool,
) {
	t.Helper()

	oldList := listPolecatsForWorkCheck
	oldCheck := checkPolecatWorkStatus
	oldIsTTY := isStdinTerminal
	oldPrompt := promptYesNoUnsafeProceed

	listPolecatsForWorkCheck = listFn
	checkPolecatWorkStatus = checkFn
	isStdinTerminal = isTTYFn
	promptYesNoUnsafeProceed = promptFn

	t.Cleanup(func() {
		listPolecatsForWorkCheck = oldList
		checkPolecatWorkStatus = oldCheck
		isStdinTerminal = oldIsTTY
		promptYesNoUnsafeProceed = oldPrompt
	})
}

func testRig() *rig.Rig {
	return &rig.Rig{
		Name: "testrig",
		Path: "/tmp/testrig",
	}
}

func TestCheckUncommittedWork_ListErrorBlocksWithoutForce(t *testing.T) {
	stubUncommittedWorkCheckDeps(
		t,
		func(*rig.Rig) ([]*polecat.Polecat, error) {
			return nil, errors.New("list failed")
		},
		func(string) (*git.UncommittedWorkStatus, error) {
			t.Fatalf("check should not be called when list fails")
			return nil, nil
		},
		func() bool { return false },
		func(string) bool {
			t.Fatalf("prompt should not be called without --force")
			return false
		},
	)

	var proceed bool
	output := captureStdout(t, func() {
		proceed = checkUncommittedWork(testRig(), "testrig", "stop", false)
	})

	if proceed {
		t.Fatal("expected proceed=false when polecat listing fails without --force")
	}
	if !strings.Contains(output, "Could not check polecats for uncommitted work") {
		t.Fatalf("expected list-error warning, got: %q", output)
	}
	if !strings.Contains(output, "--force") || !strings.Contains(output, "--nuclear") {
		t.Fatalf("expected override hint, got: %q", output)
	}
}

func TestCheckUncommittedWork_ListErrorForceTTYPrompts(t *testing.T) {
	stubUncommittedWorkCheckDeps(
		t,
		func(*rig.Rig) ([]*polecat.Polecat, error) {
			return nil, errors.New("list failed")
		},
		func(string) (*git.UncommittedWorkStatus, error) {
			t.Fatalf("check should not be called when list fails")
			return nil, nil
		},
		func() bool { return true },
		func(question string) bool {
			if question != "Proceed anyway?" {
				t.Fatalf("unexpected prompt question: %q", question)
			}
			return true
		},
	)

	proceed := checkUncommittedWork(testRig(), "testrig", "shutdown", true)
	if !proceed {
		t.Fatal("expected proceed=true after force+TTY confirmation")
	}
}

func TestCheckUncommittedWork_PolecatStatusErrorBlocks(t *testing.T) {
	stubUncommittedWorkCheckDeps(
		t,
		func(*rig.Rig) ([]*polecat.Polecat, error) {
			return []*polecat.Polecat{
				{Name: "alpha", ClonePath: "/tmp/alpha"},
			}, nil
		},
		func(string) (*git.UncommittedWorkStatus, error) {
			return nil, errors.New("git status failed")
		},
		func() bool { return false },
		func(string) bool {
			t.Fatalf("prompt should not be called without --force")
			return false
		},
	)

	var proceed bool
	output := captureStdout(t, func() {
		proceed = checkUncommittedWork(testRig(), "testrig", "restart", false)
	})

	if proceed {
		t.Fatal("expected proceed=false when polecat status check fails")
	}
	if !strings.Contains(output, "Could not verify uncommitted work for") {
		t.Fatalf("expected status-check error warning, got: %q", output)
	}
	if !strings.Contains(output, "alpha") {
		t.Fatalf("expected polecat name in warning, got: %q", output)
	}
	if !strings.Contains(output, "git status failed") {
		t.Fatalf("expected status error in warning, got: %q", output)
	}
}

func TestCollectPolecatWorkPreservesProblemsAndErrors(t *testing.T) {
	dirtyStatus := &git.UncommittedWorkStatus{
		HasUncommittedChanges: true,
		ModifiedFiles:         []string{"README.md"},
	}
	checkErr := errors.New("git status failed")
	stubUncommittedWorkCheckDeps(
		t,
		func(*rig.Rig) ([]*polecat.Polecat, error) { return nil, nil },
		func(clonePath string) (*git.UncommittedWorkStatus, error) {
			switch clonePath {
			case "/tmp/dirty":
				return dirtyStatus, nil
			case "/tmp/broken":
				return nil, checkErr
			default:
				t.Fatalf("unexpected clone path: %q", clonePath)
				return nil, nil
			}
		},
		func() bool { return false },
		func(string) bool { return false },
	)

	problems, checkErrors := collectPolecatWork([]*polecat.Polecat{
		{Name: "dirty", ClonePath: "/tmp/dirty"},
		{Name: "broken", ClonePath: "/tmp/broken"},
	})
	if len(problems) != 1 {
		t.Fatalf("problem count = %d, want 1", len(problems))
	}
	if problems[0].name != "dirty" || problems[0].status != dirtyStatus {
		t.Fatalf("problem = %#v, want dirty polecat and original status", problems[0])
	}
	if len(checkErrors) != 1 {
		t.Fatalf("check error count = %d, want 1", len(checkErrors))
	}
	if checkErrors[0].name != "broken" || !errors.Is(checkErrors[0].err, checkErr) {
		t.Fatalf("check error = %#v, want broken polecat and original error", checkErrors[0])
	}
}

func TestCheckUncommittedWork_NoPolecatsProceeds(t *testing.T) {
	stubUncommittedWorkCheckDeps(
		t,
		func(*rig.Rig) ([]*polecat.Polecat, error) { return nil, nil },
		func(string) (*git.UncommittedWorkStatus, error) {
			t.Fatal("status check should not run without polecats")
			return nil, nil
		},
		func() bool { return false },
		func(string) bool { return false },
	)

	if !checkUncommittedWork(testRig(), "testrig", "stop", false) {
		t.Fatal("empty polecat list should be safe")
	}
}

func TestCheckUncommittedWork_DirtyForceNonTTYBlocks(t *testing.T) {
	stubUncommittedWorkCheckDeps(
		t,
		func(*rig.Rig) ([]*polecat.Polecat, error) {
			return []*polecat.Polecat{
				{Name: "alpha", ClonePath: "/tmp/alpha"},
			}, nil
		},
		func(string) (*git.UncommittedWorkStatus, error) {
			return &git.UncommittedWorkStatus{
				HasUncommittedChanges: true,
				ModifiedFiles:         []string{"README.md"},
			}, nil
		},
		func() bool { return false },
		func(string) bool {
			t.Fatalf("prompt should not be called in non-TTY mode")
			return false
		},
	)

	var proceed bool
	output := captureStdout(t, func() {
		proceed = checkUncommittedWork(testRig(), "testrig", "stop", true)
	})

	if proceed {
		t.Fatal("expected proceed=false for force in non-TTY mode")
	}
	if !strings.Contains(output, "--force") || !strings.Contains(output, "interactive terminal") {
		t.Fatalf("expected non-TTY force hint, got: %q", output)
	}
}

func TestCheckUncommittedWork_DirtyForceTTYPrompts(t *testing.T) {
	stubUncommittedWorkCheckDeps(
		t,
		func(*rig.Rig) ([]*polecat.Polecat, error) {
			return []*polecat.Polecat{
				{Name: "alpha", ClonePath: "/tmp/alpha"},
			}, nil
		},
		func(string) (*git.UncommittedWorkStatus, error) {
			return &git.UncommittedWorkStatus{
				HasUncommittedChanges: true,
				ModifiedFiles:         []string{"README.md"},
			}, nil
		},
		func() bool { return true },
		func(question string) bool {
			if question != "Proceed anyway?" {
				t.Fatalf("unexpected prompt question: %q", question)
			}
			return true
		},
	)

	proceed := checkUncommittedWork(testRig(), "testrig", "stop", true)
	if !proceed {
		t.Fatal("expected proceed=true after force+TTY confirmation")
	}
}
