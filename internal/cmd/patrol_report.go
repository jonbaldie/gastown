package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/deacon"
	"github.com/jonbaldie/gastown/internal/formula"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

var patrolReportCmd = &cobra.Command{
	Use:   "report",
	Short: "Close patrol cycle with summary and start next cycle",
	Long: `Close the current patrol cycle, recording a summary of observations,
then automatically start a new patrol cycle.

This replaces the old squash+new pattern with a single command that:
  1. Closes the current patrol root wisp with the summary
  2. Creates a new patrol wisp for the next cycle

The summary is stored on the patrol root wisp for audit purposes.
The --steps flag records which patrol steps were executed vs skipped,
making shortcutting visible in the ledger.

Examples:
  gt patrol report --summary "All clear, no issues" --steps "heartbeat:OK,inbox-check:OK,health-scan:OK"
  gt patrol report --summary "Dolt latency elevated, filed escalation"`,
	RunE: runPatrolReport,
}

func init() {
	patrolReportCmd.Flags().String("summary", "", "Brief summary of patrol observations (required)")
	patrolReportCmd.Flags().String("steps", "", "Step audit: comma-separated step:STATUS pairs (e.g., heartbeat:OK,inbox-check:OK)")
	_ = patrolReportCmd.MarkFlagRequired("summary")
}

func runPatrolReport(cmd *cobra.Command, _ []string) error {
	summary := commandStringFlag(cmd, "summary")
	steps := commandStringFlag(cmd, "steps")
	roleInfo, err := GetRole()
	if err != nil {
		return fmt.Errorf("detecting role: %w", err)
	}

	cfg, err := patrolConfigForRole(roleInfo.Role, roleInfo)
	if err != nil {
		return err
	}

	patrolID, _, hasPatrol, findErr := findActivePatrol(cfg)
	if findErr != nil {
		return fmt.Errorf("finding active patrol: %w", findErr)
	}
	if !hasPatrol {
		return fmt.Errorf("no active patrol found for %s", cfg.RoleName)
	}

	b := patrolReportBeads(cfg)
	stepAudit := buildStepAudit(cfg.PatrolMolName, steps)
	updatePatrolReportSummary(b, patrolID, summary, stepAudit)
	fmt.Println(stepAudit)

	if err := closePatrolReportCycle(b, patrolID, summary); err != nil {
		return err
	}
	return startNextPatrolReport(cfg, summary)
}

func patrolReportBeads(cfg PatrolConfig) *beads.Beads {
	if cfg.Beads != nil {
		return cfg.Beads
	}
	return beads.New(cfg.BeadsDir)
}

func updatePatrolReportSummary(b *beads.Beads, patrolID, summary, stepAudit string) {
	desc := fmt.Sprintf("Patrol report: %s\n\n%s", summary, stepAudit)
	if err := b.Update(patrolID, beads.UpdateOptions{Description: &desc}); err != nil {
		style.PrintWarning("could not update patrol summary: %v", err)
	}
}

func closePatrolReportCycle(b *beads.Beads, patrolID, summary string) error {
	closed, err := forceCloseDescendants(b, patrolID)
	if err != nil {
		return fmt.Errorf("closing descendants of patrol %s (closed %d): %w", patrolID, closed, err)
	}
	if err := b.ForceCloseWithReason("patrol cycle complete: "+summary, patrolID); err != nil {
		return fmt.Errorf("closing patrol %s: %w", patrolID, err)
	}
	fmt.Printf("%s Closed patrol %s\n", style.Success.Render("✓"), patrolID)
	return nil
}

func startNextPatrolReport(cfg PatrolConfig, summary string) error {
	newPatrolID, err := autoSpawnPatrol(cfg)
	if err != nil {
		if newPatrolID != "" {
			fmt.Fprintf(os.Stderr, "warning: %s\n", err.Error())
			fmt.Printf("New patrol: %s\n", newPatrolID)
			return nil
		}
		return fmt.Errorf("starting next patrol cycle: %w", err)
	}

	fmt.Printf("%s Started new patrol: %s\n", style.Success.Render("✓"), newPatrolID)
	if cfg.RoleName == "deacon" {
		stampDeaconHeartbeatOnReport(cfg.BeadsDir, summary)
	}
	return nil
}

func stampDeaconHeartbeatOnReport(townRoot, summary string) {
	paused, _, err := deacon.IsPaused(townRoot)
	if err != nil {
		style.PrintWarning("not stamping deacon heartbeat: pause state unreadable: %v", err)
		return
	}
	if paused {
		return
	}

	action := "patrol report"
	if summary = strings.TrimSpace(summary); summary != "" {
		action += ": " + summary
	}
	if err := syncDeaconHeartbeatStores(townRoot, action); err != nil {
		style.PrintWarning("could not stamp deacon heartbeat: %v", err)
	}
}

// buildStepAudit builds a step checklist from the formula's steps and the
// reported step results. Format:
//
//	Steps: heartbeat OK | inbox-check OK | orphan-cleanup SKIP | ... (14/25)
//
// If stepsFlag is empty, returns a line indicating the audit was not reported.
func buildStepAudit(formulaName string, stepsFlag string) string {
	content, err := formula.GetEmbeddedFormulaContent(formulaName)
	if err != nil {
		return unvalidatedStepAudit(stepsFlag, "formula not found")
	}

	f, err := formula.Parse(content)
	if err != nil {
		return unvalidatedStepAudit(stepsFlag, "formula parse error")
	}

	allStepIDs := f.GetAllIDs()
	return formatStepAudit(allStepIDs, stepsFlag)
}

func unvalidatedStepAudit(stepsFlag, reason string) string {
	if stepsFlag == "" {
		return "Steps: NOT REPORTED (" + reason + ")"
	}
	return fmt.Sprintf("Steps: %s (unvalidated — %s)", stepsFlag, reason)
}

func formatStepAudit(allStepIDs []string, stepsFlag string) string {
	if len(allStepIDs) == 0 {
		return ""
	}
	if stepsFlag == "" {
		return fmt.Sprintf("Steps: NOT REPORTED (?/%d)", len(allStepIDs))
	}

	reported := parseStepResults(stepsFlag)
	parts := make([]string, 0, len(allStepIDs))
	okCount := 0
	for _, stepID := range allStepIDs {
		status := reported[stepID]
		if status == "" {
			status = "SKIP"
		}
		if status == "OK" {
			okCount++
		}
		parts = append(parts, stepID+" "+status)
	}

	return fmt.Sprintf("Steps: %s (%d/%d)", strings.Join(parts, " | "), okCount, len(allStepIDs))
}

// parseStepResults parses a comma-separated string of step:STATUS pairs.
// Returns a map of step ID to uppercase status.
// Example input: "heartbeat:OK,inbox-check:OK,orphan-cleanup:SKIP"
func parseStepResults(stepsFlag string) map[string]string {
	results := make(map[string]string)
	for _, entry := range strings.Split(stepsFlag, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) == 2 {
			results[strings.TrimSpace(parts[0])] = strings.ToUpper(strings.TrimSpace(parts[1]))
		}
	}
	return results
}
