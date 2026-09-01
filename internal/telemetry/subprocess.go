package telemetry

import (
	"os"
	"strings"
)

// buildGTResourceAttrs builds the OTEL_RESOURCE_ATTRIBUTES value from GT context
// vars present in the current process environment.
// Returns "" when no GT vars are found.
func buildGTResourceAttrs() string {
	var attrs []string
	attrs = appendEnvAttr(attrs, "GT_ROLE", "gt.role")
	attrs = appendEnvAttr(attrs, "GT_RIG", "gt.rig")
	attrs = appendEnvAttr(attrs, "BD_ACTOR", "gt.actor")
	attrs = appendAgentAttr(attrs)
	attrs = appendEnvAttr(attrs, "GT_SESSION", "gt.session")
	attrs = appendEnvAttr(attrs, "GT_RUN", "gt.run_id")
	// Work context — set by gt prime via injectWorkContext; identifies the rig,
	// bead, and molecule the agent is currently processing.
	attrs = appendEnvAttr(attrs, "GT_WORK_RIG", "gt.work_rig")
	attrs = appendEnvAttr(attrs, "GT_WORK_BEAD", "gt.work_bead")
	attrs = appendEnvAttr(attrs, "GT_WORK_MOL", "gt.work_mol")
	return strings.Join(attrs, ",")
}

func appendEnvAttr(attrs []string, envName, attrName string) []string {
	if value := os.Getenv(envName); value != "" {
		return append(attrs, attrName+"="+value)
	}
	return attrs
}

func appendAgentAttr(attrs []string) []string {
	if value := os.Getenv("GT_POLECAT"); value != "" {
		return append(attrs, "gt.agent="+value)
	}
	if value := os.Getenv("GT_CREW"); value != "" {
		return append(attrs, "gt.agent="+value)
	}
	return attrs
}

// SetProcessOTELAttrs sets OTEL-related variables in the current process
// environment so that all bd subprocesses spawned via exec.Command inherit
// them automatically — no per-call injection needed.
//
// Sets:
//   - OTEL_RESOURCE_ATTRIBUTES — GT context labels (gt.role, gt.rig, …)
//   - BD_OTEL_METRICS_URL      — bd's own metrics var (mirrors GT_OTEL_METRICS_URL)
//   - BD_OTEL_LOGS_URL         — bd's own logs var   (mirrors GT_OTEL_LOGS_URL)
//
// Called once at gt startup (Execute) when telemetry is active.
// No-op when GT_OTEL_METRICS_URL is not set.
func SetProcessOTELAttrs() {
	metricsURL := os.Getenv(EnvMetricsURL)
	if metricsURL == "" {
		return
	}
	if attrs := buildGTResourceAttrs(); attrs != "" {
		_ = os.Setenv("OTEL_RESOURCE_ATTRIBUTES", attrs)
	}
	// Mirror GT vars into bd's own var names so bd subprocesses
	// emit their metrics to the same VictoriaMetrics instance.
	_ = os.Setenv("BD_OTEL_METRICS_URL", metricsURL)
	if logsURL := os.Getenv(EnvLogsURL); logsURL != "" {
		_ = os.Setenv("BD_OTEL_LOGS_URL", logsURL)
	}
}

// OTELEnvForSubprocess returns OTEL environment variables to inject into bd
// subprocesses when cmd.Env is built explicitly (overriding os.Environ).
//
// Complements SetProcessOTELAttrs for callers that construct cmd.Env manually
// (beads.go run, mail/bd.go runBdCommand) so the vars aren't lost when the
// explicit env slice is built from scratch instead of os.Environ().
//
// Returns nil when GT telemetry is not active (GT_OTEL_METRICS_URL not set).
func OTELEnvForSubprocess() []string {
	metricsURL := os.Getenv(EnvMetricsURL)
	if metricsURL == "" {
		return nil
	}
	var env []string
	if attrs := buildGTResourceAttrs(); attrs != "" {
		env = append(env, "OTEL_RESOURCE_ATTRIBUTES="+attrs)
	}
	env = append(env, "BD_OTEL_METRICS_URL="+metricsURL)
	if logsURL := os.Getenv(EnvLogsURL); logsURL != "" {
		env = append(env, "BD_OTEL_LOGS_URL="+logsURL)
	}
	if runID := os.Getenv("GT_RUN"); runID != "" {
		env = append(env, "GT_RUN="+runID)
	}
	return env
}
