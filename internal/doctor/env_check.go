package doctor

import (
	"fmt"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// SessionEnvReader abstracts tmux session environment reads for testing.
type SessionEnvReader interface {
	ListSessions() ([]string, error)
	GetAllEnvironment(_ string) (map[string]string, error)
}

// SessionEnvWriter abstracts tmux session environment writes for testing.
type SessionEnvWriter interface {
	SetEnvironment(_, _, _ string) error
}

// SessionEnvAccessor combines read and write access to tmux session environments.
type SessionEnvAccessor interface {
	SessionEnvReader
	SessionEnvWriter
}

// tmuxEnvReaderWriter wraps real tmux operations for both reading and writing.
type tmuxEnvReaderWriter struct {
	t *tmux.Tmux
}

func (r *tmuxEnvReaderWriter) ListSessions() ([]string, error) {
	return r.t.ListSessions()
}

func (r *tmuxEnvReaderWriter) GetAllEnvironment(session string) (map[string]string, error) {
	return r.t.GetAllEnvironment(session)
}

func (r *tmuxEnvReaderWriter) SetEnvironment(session, key, value string) error {
	return r.t.SetEnvironment(session, key, value)
}

// EnvVarsCheck verifies that tmux session environment variables match expected values.
type EnvVarsCheck struct {
	FixableCheck
	dependencies envVarsDependencies
}

type envVarsDependencies struct {
	reader   SessionEnvReader   // nil means use real tmux
	accessor SessionEnvAccessor // non-nil when Fix() support is needed
}

// NewEnvVarsCheck creates a new env vars check.
func NewEnvVarsCheck() *EnvVarsCheck {
	return &EnvVarsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "env-vars",
				CheckDescription: "Verify tmux session environment variables match expected values",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// NewEnvVarsCheckWithReader creates a check with a custom reader (for testing Run()).
func NewEnvVarsCheckWithReader(reader SessionEnvReader) *EnvVarsCheck {
	c := NewEnvVarsCheck()
	c.dependencies.reader = reader
	return c
}

// NewEnvVarsCheckWithAccessor creates a check with a custom accessor (for testing Fix()).
func NewEnvVarsCheckWithAccessor(accessor SessionEnvAccessor) *EnvVarsCheck {
	c := NewEnvVarsCheck()
	c.dependencies.accessor = accessor
	c.dependencies.reader = accessor
	return c
}

// Run checks environment variables for all Gas Town sessions.
func (c *EnvVarsCheck) Run(ctx *CheckContext) *CheckResult {
	reader := c.sessionEnvReader()

	sessions, err := reader.ListSessions()
	if err != nil {
		// No tmux server - treat as success (valid when Gas Town is down)
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No tmux sessions running",
		}
	}

	gtSessions := knownGasTownSessions(sessions)
	if len(gtSessions) == 0 {
		// No Gas Town sessions - treat as success (valid when Gas Town is down)
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No Gas Town sessions running",
		}
	}

	details := c.inspectSessions(reader, gtSessions, ctx)
	return c.envResult(details)
}

type envCheckDetails struct {
	mismatches       []string
	beadsDirWarnings []string
	checkedCount     int
}

func (c *EnvVarsCheck) sessionEnvReader() SessionEnvReader {
	if c.dependencies.reader != nil {
		return c.dependencies.reader
	}
	return &tmuxEnvReaderWriter{t: tmux.NewTmux()}
}

func (c *EnvVarsCheck) inspectSessions(reader SessionEnvReader, sessions []string, ctx *CheckContext) envCheckDetails {
	var details envCheckDetails
	for _, sess := range sessions {
		mismatches, beadsWarning, checked := inspectSessionEnvironment(reader, sess, ctx)
		details.mismatches = append(details.mismatches, mismatches...)
		if beadsWarning != "" {
			details.beadsDirWarnings = append(details.beadsDirWarnings, beadsWarning)
		}
		if checked {
			details.checkedCount++
		}
	}
	return details
}

func inspectSessionEnvironment(reader SessionEnvReader, sess string, ctx *CheckContext) ([]string, string, bool) {
	identity, err := session.ParseSessionName(sess)
	if err != nil {
		return nil, "", false
	}
	actual, err := reader.GetAllEnvironment(sess)
	if err != nil {
		return []string{fmt.Sprintf("%s: could not read env vars: %v", sess, err)}, "", false
	}
	mismatches := compareSessionEnvironment(sess, expectedSessionEnvironment(ctx, identity), actual)
	return mismatches, beadsDirWarning(sess, actual), true
}

func expectedSessionEnvironment(ctx *CheckContext, identity *session.AgentIdentity) map[string]string {
	return config.AgentEnv(config.AgentEnvConfig{
		Role:      sessionRole(identity),
		Rig:       identity.Rig,
		AgentName: identity.Name,
		TownRoot:  ctx.TownRoot,
	})
}

func sessionRole(identity *session.AgentIdentity) string {
	if identity.Role == session.RoleDeacon && identity.Name == "boot" {
		return "boot"
	}
	return string(identity.Role)
}

func compareSessionEnvironment(sess string, expected, actual map[string]string) []string {
	var mismatches []string
	for key, expectedVal := range expected {
		actualVal, exists := actual[key]
		if !exists && expectedVal != "" {
			mismatches = append(mismatches, fmt.Sprintf("%s: missing %s (expected %q)", sess, key, expectedVal))
		} else if exists && actualVal != expectedVal {
			mismatches = append(mismatches, fmt.Sprintf("%s: %s=%q (expected %q)", sess, key, actualVal, expectedVal))
		}
	}
	return mismatches
}

func beadsDirWarning(sess string, actual map[string]string) string {
	if beadsDir := actual["BEADS_DIR"]; beadsDir != "" {
		return fmt.Sprintf("%s: BEADS_DIR=%q (breaks prefix routing)", sess, beadsDir)
	}
	return ""
}

func (c *EnvVarsCheck) envResult(details envCheckDetails) *CheckResult {

	// Check for BEADS_DIR issues first (higher priority warning)
	if len(details.beadsDirWarnings) > 0 {
		resultDetails := details.beadsDirWarnings
		if len(details.mismatches) > 0 {
			resultDetails = append(resultDetails, "", "Other env var issues:")
			resultDetails = append(resultDetails, details.mismatches...)
		}
		resultDetails = append(resultDetails,
			"",
			"BEADS_DIR overrides prefix-based routing and breaks multi-rig lookups.",
		)
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("Found BEADS_DIR set in %d session(s)", len(details.beadsDirWarnings)),
			Details: resultDetails,
			FixHint: "Remove BEADS_DIR from session environment: gt shutdown && gt up",
		}
	}

	if len(details.mismatches) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d session(s) have correct environment variables", details.checkedCount),
		}
	}

	// Add explanation about needing restart
	resultDetails := append(details.mismatches,
		"",
		"Note: Mismatched session env vars won't affect running Claude until sessions restart.",
	)

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Found %d env var mismatch(es) across %d session(s)", len(details.mismatches), details.checkedCount),
		Details: resultDetails,
		FixHint: "Run 'gt doctor --fix' to apply missing env vars in-place, or 'gt shutdown && gt up' to restart",
	}
}

// Fix applies missing or incorrect env vars to all Gas Town tmux sessions in-place.
// The running Claude process is unaffected (it already has env vars from startup);
// this updates the tmux session store so future processes and gt doctor agree.
func (c *EnvVarsCheck) Fix(ctx *CheckContext) error {
	accessor := c.sessionEnvAccessor()

	sessions, err := accessor.ListSessions()
	if err != nil {
		// No tmux server — nothing to fix.
		return nil
	}

	for _, sess := range knownGasTownSessions(sessions) {
		fixSessionEnvironment(accessor, sess, ctx)
	}
	return nil
}

func (c *EnvVarsCheck) sessionEnvAccessor() SessionEnvAccessor {
	if c.dependencies.accessor != nil {
		return c.dependencies.accessor
	}
	return &tmuxEnvReaderWriter{t: tmux.NewTmux()}
}

func fixSessionEnvironment(accessor SessionEnvAccessor, sess string, ctx *CheckContext) {
	identity, err := session.ParseSessionName(sess)
	if err != nil {
		return
	}
	expected := expectedSessionEnvironment(ctx, identity)
	actual, err := accessor.GetAllEnvironment(sess)
	if err != nil {
		return
	}
	for key, expectedVal := range expected {
		if actualVal, exists := actual[key]; !exists || actualVal != expectedVal {
			_ = accessor.SetEnvironment(sess, key, expectedVal)
		}
	}
}
