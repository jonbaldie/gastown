package beads

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSchemaBootstrapTimeoutHonorsOperatorBudget(t *testing.T) {
	t.Setenv(EnvSchemaBootstrapTimeout, "90s")
	got := SchemaBootstrapTimeout()
	if got != 90*time.Second {
		t.Fatalf("SchemaBootstrapTimeout() = %s, want 90s so an operator can set the migration time budget", got)
	}
}

func TestSchemaBootstrapTimeoutRejectsInvalidBudget(t *testing.T) {
	t.Setenv(EnvSchemaBootstrapTimeout, "nope")
	got := SchemaBootstrapTimeout()
	if got != DefaultSchemaBootstrapTimeout {
		t.Fatalf("SchemaBootstrapTimeout() = %s, want default %s for invalid override", got, DefaultSchemaBootstrapTimeout)
	}
}

func TestSchemaBootstrapReportsStageAndBlocksReadyUntilComplete(t *testing.T) {
	dir := t.TempDir()
	if SchemaBootstrapReady(dir) {
		t.Fatal("missing bootstrap state must not report a usable Town")
	}

	if err := MarkSchemaBootstrapStage(dir, SchemaBootstrapStageOpening, ""); err != nil {
		t.Fatalf("mark opening: %v", err)
	}
	state, err := LoadSchemaBootstrapState(dir)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.Stage != string(SchemaBootstrapStageOpening) {
		t.Fatalf("stage = %q, want %q", state.Stage, SchemaBootstrapStageOpening)
	}
	if SchemaBootstrapReady(dir) {
		t.Fatal("in-progress bootstrap must not report a usable Town")
	}

	if err := MarkSchemaBootstrapTimedOut(dir, "opening_store"); err != nil {
		t.Fatalf("mark timeout: %v", err)
	}
	state, err = LoadSchemaBootstrapState(dir)
	if err != nil {
		t.Fatalf("load timed-out state: %v", err)
	}
	if !state.TimedOut || !SchemaBootstrapRetryable(state) {
		t.Fatalf("timed-out bootstrap must be retryable; state=%+v retryable=%v", state, SchemaBootstrapRetryable(state))
	}
	if SchemaBootstrapReady(dir) {
		t.Fatal("timed-out bootstrap must not report a usable Town")
	}

	if err := MarkSchemaBootstrapStage(dir, SchemaBootstrapStageComplete, ""); err != nil {
		t.Fatalf("mark complete: %v", err)
	}
	if !SchemaBootstrapReady(dir) {
		t.Fatal("completed bootstrap must report ready")
	}
}

func TestSchemaBootstrapStateFileLivesInBeadsDir(t *testing.T) {
	dir := t.TempDir()
	if err := MarkSchemaBootstrapStage(dir, SchemaBootstrapStageMigrating, "running"); err != nil {
		t.Fatalf("mark migrating: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, schemaBootstrapStateFile)); err != nil {
		t.Fatalf("state file: %v", err)
	}
}
