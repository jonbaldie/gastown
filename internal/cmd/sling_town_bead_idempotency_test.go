package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/sling"
)

// TestEnsureBeadInTargetRigTownBeadRetryDoesNotBurnIDs is the tight loop for
// "gt sling hq-ni1 demo" not being idempotent: the first attempt moves the
// town bead (closing it) before the target-rig dispatch check, then a retry
// of the surviving hq-* ID mints another closed orphan.
func TestEnsureBeadInTargetRigTownBeadRetryDoesNotBurnIDs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows: shell stub uses POSIX env logging")
	}

	townRoot, createdPath := setupTownBeadRetryFixture(t, false)

	firstID, firstErr := ensureBeadInTargetRig("hq-ni1", "demo", townRoot, false)
	if firstErr != nil {
		t.Fatalf("expected town bead to land in the target rig before dispatch, got: %v", firstErr)
	}
	if firstID != "dm-landed1" {
		t.Fatalf("landed ID = %q, want dm-landed1 (rig prefix)", firstID)
	}
	created := readCreatedIDs(t, createdPath)

	retryID, retryErr := ensureBeadInTargetRig("hq-ni1", "demo", townRoot, false)
	if retryErr != nil {
		t.Fatalf("retry of the original town bead ID should follow the move, got: %v", retryErr)
	}
	if retryID != firstID {
		t.Fatalf("retry ID = %q, want %q", retryID, firstID)
	}

	created = readCreatedIDs(t, createdPath)
	var townCreates []string
	for _, id := range created {
		if isTownWorkBead(id) {
			townCreates = append(townCreates, id)
		}
	}
	if len(townCreates) > 0 {
		t.Fatalf("town-bead sling minted hq-* orphans %v (firstErr=%v firstID=%q); retries must not burn town IDs", townCreates, firstErr, firstID)
	}
	if len(created) > 1 {
		t.Fatalf("retry burned extra IDs %v; expected at most one landing in the target rig", created)
	}
}

// TestRunTownSlingTownBeadRetryDoesNotTreatMovedSourceAsClosed covers the
// user-visible retry: `gt sling hq-ni1 demo` again after the first attempt
// closed hq-ni1. That retry currently fails on the old ID ("already closed")
// instead of following the move or leaving the source open.
func TestRunTownSlingTownBeadRetryDoesNotTreatMovedSourceAsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows: shell stub uses POSIX env logging")
	}

	townRoot, createdPath := setupTownBeadRetryFixture(t, true)

	intent := sling.Intent{
		BeadID:  "hq-ni1",
		RigName: "demo",
		IntentExecutionOptions: sling.IntentExecutionOptions{
			TownRoot:    townRoot,
			BeadsDir:    filepath.Join(townRoot, ".beads"),
			HookRawBead: true,
			NoConvoy:    true,
			NoBoot:      true,
		},
	}
	_, firstErr := runTownSling(context.Background(), intent)
	_, retryErr := runTownSling(context.Background(), intent)
	if retryErr != nil && (strings.Contains(retryErr.Error(), "is closed") || strings.Contains(retryErr.Error(), "already closed")) {
		t.Fatalf("retry failed on the old ID after the move committed: %v (firstErr=%v)", retryErr, firstErr)
	}

	created := readCreatedIDs(t, createdPath)
	var townCreates []string
	for _, id := range created {
		if isTownWorkBead(id) {
			townCreates = append(townCreates, id)
		}
	}
	if len(townCreates) > 0 {
		t.Fatalf("executeSling minted hq-* orphans %v; town-bead sling must land in the target rig or not move", townCreates)
	}
	if len(created) > 1 {
		t.Fatalf("retry burned extra IDs %v; expected at most one landing in the target rig", created)
	}
}

func setupTownBeadRetryFixture(t *testing.T, stubExecuteSling bool) (townRoot, createdPath string) {
	t.Helper()

	townRoot = t.TempDir()
	rigDir := filepath.Join(townRoot, "demo", "mayor", "rig")
	townBeadsDir := filepath.Join(townRoot, ".beads")
	rigBeadsDir := filepath.Join(rigDir, ".beads")
	for _, dir := range []string{townBeadsDir, rigBeadsDir, filepath.Join(townRoot, "mayor", "rig")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"version":1}`), 0644); err != nil {
		t.Fatalf("write town marker: %v", err)
	}
	rigs := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"demo": {
				GitURL:  "git@github.com:test/demo.git",
				AddedAt: time.Now().Truncate(time.Second),
				BeadsConfig: &config.BeadsConfig{
					Repo:   "local",
					Prefix: "dm-",
				},
			},
		},
	}
	if err := config.SaveRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"), rigs); err != nil {
		t.Fatalf("SaveRigsConfig: %v", err)
	}
	// Town route only: GetPrefixForRig("demo") falls back to rigs.json ("dm"),
	// but prefix routing for "dm-x" misses routes and creates in the town DB
	// (hq-*). That is the landing that makes retries mint more hq-* orphans.
	routes := strings.Join([]string{
		`{"prefix":"hq-","path":"."}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(townBeadsDir, "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath := filepath.Join(townRoot, "bd.log")
	createdPath = filepath.Join(townRoot, "created.txt")
	closedDir := filepath.Join(townRoot, "closed")
	if err := os.MkdirAll(closedDir, 0755); err != nil {
		t.Fatalf("mkdir closed: %v", err)
	}

	bdScript := `#!/bin/sh
set -e
if [ "$1" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo 'bd version 1.0.0'
  exit 0
fi
printf '%s|BEADS_DIR=%s|PWD=%s\n' "$*" "${BEADS_DIR:-}" "$(pwd)" >> "${BD_LOG}"
cmd="$1"
shift || true
if [ "$cmd" = "--allow-stale" ]; then
  cmd="$1"
  shift || true
fi
case "$cmd" in
  show)
    id="$1"
    reason_file="${CLOSED_DIR}/${id}"
    if [ -f "$reason_file" ]; then
      reason=$(cat "$reason_file")
      echo "[{\"id\":\"${id}\",\"title\":\"Town-owned issue\",\"status\":\"closed\",\"assignee\":\"\",\"description\":\"\",\"close_reason\":\"${reason}\"}]"
      exit 0
    fi
    if [ "${BEADS_DIR:-}" = "${TARGET_BEADS_DIR}" ]; then
      case "$id" in
        dm-*)
          echo "[{\"id\":\"${id}\",\"title\":\"Town-owned issue\",\"status\":\"open\",\"assignee\":\"\",\"description\":\"\"}]"
          exit 0
          ;;
      esac
      exit 1
    fi
    echo "[{\"id\":\"${id}\",\"title\":\"Town-owned issue\",\"status\":\"open\",\"assignee\":\"\",\"description\":\"\",\"issue_type\":\"task\",\"priority\":2}]"
    exit 0
    ;;
  create)
    n=0
    if [ -f "${CREATED}" ]; then
      n=$(wc -l < "${CREATED}" | tr -d ' ')
    fi
    n=$((n + 1))
    if [ "${BEADS_DIR:-}" = "${TARGET_BEADS_DIR}" ]; then
      id="dm-landed${n}"
    else
      if [ "$n" -eq 1 ]; then
        id="hq-3nn"
      else
        id="hq-73b"
      fi
    fi
    echo "$id" >> "${CREATED}"
    echo "$id"
    exit 0
    ;;
  close)
    id="$1"
    reason="closed"
    while [ $# -gt 0 ]; do
      case "$1" in
        --reason)
          shift
          reason="$1"
          ;;
        --reason=*)
          reason="${1#--reason=}"
          ;;
      esac
      shift || true
    done
    printf '%s\n' "$reason" > "${CLOSED_DIR}/${id}"
    exit 0
    ;;
  update|cook|mol|dep)
    exit 0
    ;;
esac
exit 0
`
	_ = writeBDStub(t, binDir, bdScript, "")

	t.Setenv("BD_LOG", logPath)
	t.Setenv("CREATED", createdPath)
	t.Setenv("CLOSED_DIR", closedDir)
	t.Setenv("TARGET_BEADS_DIR", rigBeadsDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGTRole, "mayor")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("GT_TEST_NO_NUDGE", "1")
	t.Setenv("GT_TEST_SKIP_HOOK_VERIFY", "1")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(filepath.Join(townRoot, "mayor", "rig")); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if stubExecuteSling {
		prevSpawn := spawnPolecatForSling
		prevHook := hookBeadWithRetryWithTownRootFn
		t.Cleanup(func() {
			spawnPolecatForSling = prevSpawn
			hookBeadWithRetryWithTownRootFn = prevHook
		})
		spawnPolecatForSling = func(rigName string, _ SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
			return &SpawnedPolecatInfo{
				RigName:     rigName,
				PolecatName: "toast",
				ClonePath:   filepath.Join(townRoot, "demo", "polecats", "toast"),
				Pane:        "%1",
			}, nil
		}
		hookBeadWithRetryWithTownRootFn = func(string, string, string, string) error {
			return nil
		}
	}

	return townRoot, createdPath
}

func readCreatedIDs(t *testing.T, createdPath string) []string {
	t.Helper()
	data, err := os.ReadFile(createdPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read created IDs: %v", err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids
}

func TestParseMovedToDestination(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{"Moved to hq-3nn", "hq-3nn"},
		{"Moved to dm-landed1", "dm-landed1"},
		{"Moved to dm-landed1 because prefix changed", "dm-landed1"},
		{"closed", ""},
		{"", ""},
		{"Moved to ", ""},
	}
	for _, tt := range tests {
		if got := parseMovedToDestination(tt.reason); got != tt.want {
			t.Errorf("parseMovedToDestination(%q) = %q, want %q", tt.reason, got, tt.want)
		}
	}
}

func TestCheckCrossRigGuardAcceptsRigsJSONPrefixWithoutRoute(t *testing.T) {
	townRoot, _ := setupTownBeadRetryFixture(t, false)
	if err := checkCrossRigGuard("dm-landed1", "demo/polecats/toast", townRoot); err != nil {
		t.Fatalf("checkCrossRigGuard should accept the rigs.json prefix when routes.jsonl is stale, got: %v", err)
	}
}
