package doctor

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/gastown/internal/formula"
)

// FormulaCheck verifies that embedded formulas are up-to-date.
// It detects outdated formulas (binary updated), missing formulas (user deleted),
// and modified formulas (user customized). Can auto-fix outdated and missing.
type FormulaCheck struct {
	FixableCheck
}

// NewFormulaCheck creates a new formula check.
func NewFormulaCheck() *FormulaCheck {
	return &FormulaCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "formulas",
				CheckDescription: "Check embedded formulas are up-to-date",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

// Run checks if formulas need updating.
func (c *FormulaCheck) Run(ctx *CheckContext) *CheckResult {
	report, err := formula.CheckFormulaHealth(ctx.TownRoot)
	if err != nil {
		return formulaCheckErrorResult(c, err)
	}

	if formulasHealthy(report) {
		return formulaHealthyResult(c, report)
	}
	return formulaHealthResult(c, report)
}

func formulaCheckErrorResult(c *FormulaCheck, err error) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("Could not check formulas: %v", err),
	}
}

func formulasHealthy(report *formula.HealthReport) bool {
	return report.Outdated == 0 && report.Missing == 0 && report.Modified == 0 && report.New == 0 && report.Untracked == 0
}

func formulaHealthyResult(c *FormulaCheck, report *formula.HealthReport) *CheckResult {
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: fmt.Sprintf("%d formulas up-to-date", report.OK),
	}
}

func formulaHealthResult(c *FormulaCheck, report *formula.HealthReport) *CheckResult {
	var details []string
	details, needsFix := formulaDetails(report)

	result := &CheckResult{
		Name:    c.Name(),
		Status:  formulaStatus(needsFix),
		Message: formulaMessage(report),
		Details: details,
	}

	if needsFix {
		result.FixHint = "Run 'gt doctor --fix' to update formulas"
	}

	return result
}

func formulaDetails(report *formula.HealthReport) ([]string, bool) {
	var details []string
	var needsFix bool
	for _, status := range report.Formulas {
		detail, fix := formulaDetail(status)
		if detail != "" {
			details = append(details, detail)
		}
		needsFix = needsFix || fix
	}
	return details, needsFix
}

func formulaDetail(status formula.FormulaStatus) (string, bool) {
	switch status.Status {
	case "outdated":
		return fmt.Sprintf("  %s: update available", status.Name), true
	case "missing":
		return fmt.Sprintf("  %s: missing (will reinstall)", status.Name), true
	case "modified":
		return fmt.Sprintf("  %s: locally modified (skipping)", status.Name), false
	case "new":
		return fmt.Sprintf("  %s: new formula available", status.Name), true
	case "untracked":
		return fmt.Sprintf("  %s: untracked (will update)", status.Name), true
	default:
		return "", false
	}
}

func formulaStatus(needsFix bool) CheckStatus {
	if needsFix {
		return StatusWarning
	}
	return StatusOK
}

func formulaMessage(report *formula.HealthReport) string {
	var parts []string
	appendFormulaCount(&parts, "outdated", report.Outdated)
	appendFormulaCount(&parts, "missing", report.Missing)
	appendFormulaCount(&parts, "new", report.New)
	appendFormulaCount(&parts, "untracked", report.Untracked)
	appendFormulaCount(&parts, "modified", report.Modified)
	return fmt.Sprintf("Formulas: %s", strings.Join(parts, ", "))
}

func appendFormulaCount(parts *[]string, label string, count int) {
	if count > 0 {
		*parts = append(*parts, fmt.Sprintf("%d %s", count, label))
	}
}

// Fix updates outdated and missing formulas.
func (c *FormulaCheck) Fix(ctx *CheckContext) error {
	updated, skipped, reinstalled, err := formula.UpdateFormulas(ctx.TownRoot)
	if err != nil {
		return err
	}

	// Log what was done (caller will re-run check to show new status)
	if updated > 0 || reinstalled > 0 || skipped > 0 {
		// The doctor framework will re-run the check after fix
		// so we don't need to log here
		_ = updated
		_ = reinstalled
		_ = skipped
	}

	return nil
}
