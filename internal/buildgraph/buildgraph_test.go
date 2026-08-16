package buildgraph

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHitsReportsOnlyForbiddenPrefixes(t *testing.T) {
	deps := []string{
		"fmt",
		"github.com/dolthub/driver",
		"github.com/steveyegge/beads",
		"github.com/testcontainers/testcontainers-go",
		"github.com/go-rod/rod",
		"github.com/go-rod/rod/lib/launcher",
	}

	got := Hits(deps, ProductionForbiddenPrefixes)
	want := []string{
		"github.com/dolthub/driver",
		"github.com/testcontainers/testcontainers-go",
		"github.com/go-rod/rod",
		"github.com/go-rod/rod/lib/launcher",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Hits() = %q, want %q", got, want)
	}
}

func TestHitsEmptyWhenGraphIsClean(t *testing.T) {
	deps := []string{
		"fmt",
		"github.com/steveyegge/beads",
		"github.com/go-sql-driver/mysql",
	}
	if hits := Hits(deps, ProductionForbiddenPrefixes); len(hits) != 0 {
		t.Fatalf("Hits() = %q, want none", hits)
	}
}

func TestMakefileDefaultsCGOOff(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("CGO_ENABLED ?= 0")) {
		t.Fatal("Makefile must default CGO_ENABLED to 0 so daily builds skip beads' embedded Dolt engine")
	}
}

func TestCmdGTWithoutCGOOmitsForbiddenDeps(t *testing.T) {
	deps := listDeps(t, "0", "./cmd/gt")
	if hits := Hits(deps, ProductionForbiddenPrefixes); len(hits) != 0 {
		t.Fatalf("CGO_ENABLED=0 ./cmd/gt still imports forbidden packages:\n%s", strings.Join(hits, "\n"))
	}
}

func TestCmdGTWithCGOPullsEmbeddedDolt(t *testing.T) {
	deps := listDeps(t, "1", "./cmd/gt")
	if Hits(deps, []string{"github.com/steveyegge/beads/internal/storage/embeddeddolt"}) == nil {
		t.Fatal("CGO_ENABLED=1 ./cmd/gt should import beads embeddeddolt; that is the compile tax CGO_ENABLED=0 removes")
	}
}

func TestTestutilDefaultImportsOmitTestcontainers(t *testing.T) {
	imports := listPackageImports(t, "", "./internal/testutil")
	if hits := Hits(imports, []string{"github.com/testcontainers/"}); len(hits) != 0 {
		t.Fatalf("default ./internal/testutil imports testcontainers:\n%s", strings.Join(hits, "\n"))
	}
}

func TestTestutilIntegrationTagKeepsTestcontainers(t *testing.T) {
	imports := listPackageImports(t, "integration", "./internal/testutil")
	if Hits(imports, []string{"github.com/testcontainers/testcontainers-go"}) == nil {
		t.Fatal("go test -tags=integration must still compile the real testcontainers Dolt helpers")
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func listDeps(t *testing.T, cgo, pkg string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", pkg)
	cmd.Dir = moduleRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgo)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list -deps %s: %v\n%s", pkg, err, ee.Stderr)
		}
		t.Fatal(err)
	}
	return splitNonEmpty(string(out))
}

func listPackageImports(t *testing.T, tags, pkg string) []string {
	t.Helper()
	args := []string{"list", "-f", "{{join .Imports \"\\n\"}}"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = moduleRoot(t)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list %s: %v\n%s", pkg, err, ee.Stderr)
		}
		t.Fatal(err)
	}
	return splitNonEmpty(string(out))
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
