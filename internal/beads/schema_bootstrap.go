package beads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultSchemaBootstrapTimeout is the migration time budget when the operator
// has not set GT_BEADS_SCHEMA_BOOTSTRAP_TIMEOUT.
const DefaultSchemaBootstrapTimeout = time.Minute

// EnvSchemaBootstrapTimeout is the operator override for the schema bootstrap
// migration time budget. Value is a Go duration, for example "90s" or "2m".
const EnvSchemaBootstrapTimeout = "GT_BEADS_SCHEMA_BOOTSTRAP_TIMEOUT"

const schemaBootstrapStateFile = "schema-bootstrap.json"

// SchemaBootstrapStage is a named step in Beads schema bootstrap.
type SchemaBootstrapStage string

const (
	SchemaBootstrapStageOpening       SchemaBootstrapStage = "opening_store"
	SchemaBootstrapStageMigrating     SchemaBootstrapStage = "migrating_schema"
	SchemaBootstrapStageSettingPrefix SchemaBootstrapStage = "setting_prefix"
	SchemaBootstrapStageComplete      SchemaBootstrapStage = "complete"
	SchemaBootstrapStageFailed        SchemaBootstrapStage = "failed"
)

// SchemaBootstrapState records the migration stage that is running or failed.
type SchemaBootstrapState struct {
	Stage     string `json:"stage"`
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

// SchemaBootstrapTimeout returns the operator-configurable migration time budget.
func SchemaBootstrapTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv(EnvSchemaBootstrapTimeout)); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil && d > 0 {
			return d
		}
	}
	return DefaultSchemaBootstrapTimeout
}

func schemaBootstrapStatePath(beadsDir string) string {
	return filepath.Join(beadsDir, schemaBootstrapStateFile)
}

// LoadSchemaBootstrapState returns the persisted bootstrap stage, or nil if none.
func LoadSchemaBootstrapState(beadsDir string) (*SchemaBootstrapState, error) {
	data, err := os.ReadFile(schemaBootstrapStatePath(beadsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state SchemaBootstrapState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func saveSchemaBootstrapState(beadsDir string, state SchemaBootstrapState) error {
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if state.StartedAt == "" {
		if existing, err := LoadSchemaBootstrapState(beadsDir); err == nil && existing != nil && existing.StartedAt != "" && existing.Stage != string(SchemaBootstrapStageComplete) {
			state.StartedAt = existing.StartedAt
		} else {
			state.StartedAt = now
		}
	}
	state.UpdatedAt = now
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(schemaBootstrapStatePath(beadsDir), data, 0o600)
}

// MarkSchemaBootstrapStage records the migration stage that is running or failed.
func MarkSchemaBootstrapStage(beadsDir string, stage SchemaBootstrapStage, errMsg string) error {
	state := SchemaBootstrapState{
		Stage:    string(stage),
		Error:    errMsg,
		TimedOut: false,
	}
	if stage == SchemaBootstrapStageComplete {
		state.Error = ""
	}
	return saveSchemaBootstrapState(beadsDir, state)
}

// MarkSchemaBootstrapTimedOut records a timed-out stage so a later retry can resume.
func MarkSchemaBootstrapTimedOut(beadsDir, stage string) error {
	if strings.TrimSpace(stage) == "" {
		stage = string(SchemaBootstrapStageFailed)
	}
	return saveSchemaBootstrapState(beadsDir, SchemaBootstrapState{
		Stage:    stage,
		Error:    "timed out",
		TimedOut: true,
	})
}

// SchemaBootstrapReady reports whether required schema work has completed.
// A missing state file is not ready: bootstrap must record completion before
// the Town is treated as usable.
func SchemaBootstrapReady(beadsDir string) bool {
	state, err := LoadSchemaBootstrapState(beadsDir)
	if err != nil || state == nil {
		return false
	}
	return state.Stage == string(SchemaBootstrapStageComplete) && !state.TimedOut
}

// SchemaBootstrapRetryable reports whether a timed-out or failed bootstrap
// can be retried safely. Completion is not retryable because it already finished.
func SchemaBootstrapRetryable(state *SchemaBootstrapState) bool {
	if state == nil {
		return true
	}
	if state.Stage == string(SchemaBootstrapStageComplete) && !state.TimedOut {
		return false
	}
	return state.TimedOut || state.Stage == string(SchemaBootstrapStageFailed) || state.Stage != string(SchemaBootstrapStageComplete)
}
