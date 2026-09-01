package doctor

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jonbaldie/gastown/internal/ui"
)

// Doctor manages and executes health checks.
type Doctor struct {
	checks []Check
}

// NewDoctor creates a new Doctor with no registered checks.
func NewDoctor() *Doctor {
	return &Doctor{
		checks: make([]Check, 0),
	}
}

// Register adds a check to the doctor's check list.
func (d *Doctor) Register(check Check) {
	d.checks = append(d.checks, check)
}

// RegisterAll adds multiple checks to the doctor's check list.
func (d *Doctor) RegisterAll(checks ...Check) {
	d.checks = append(d.checks, checks...)
}

// Checks returns the list of registered checks.
func (d *Doctor) Checks() []Check {
	return d.checks
}

// categoryGetter interface for checks that provide a category
type categoryGetter interface {
	Category() string
}

// Run executes all registered checks and returns a report.
func (d *Doctor) Run(ctx *CheckContext) *Report {
	return d.RunStreaming(ctx, nil, 0)
}

// RunStreaming executes all registered checks with optional real-time output.
// If w is non-nil, prints each check name as it starts and result when done.
// If slowThreshold > 0, shows hourglass icon for slow checks.
func (d *Doctor) RunStreaming(ctx *CheckContext, w io.Writer, slowThreshold time.Duration) *Report {
	report := NewReport()

	for _, check := range d.checks {
		streamCheckStart(w, check)
		result := d.runTimedCheck(check, ctx)
		streamCheckResult(w, result, slowThreshold, report)

		report.Add(result)
	}

	return report
}

// Fix runs all checks with auto-fix enabled where possible.
// It first runs the check, then if it fails and can be fixed, attempts the fix.
func (d *Doctor) Fix(ctx *CheckContext) *Report {
	return d.FixStreaming(ctx, nil, 0)
}

// safeFixCheck calls check.Fix() with panic recovery. If the Fix method panics
// (e.g., due to a Dolt nil pointer dereference propagating in-process — GH#1769),
// the panic is caught and returned as an error instead of crashing gt doctor.
func safeFixCheck(check Check, ctx *CheckContext) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("fix panicked: %v", r)
		}
	}()
	return check.Fix(ctx)
}

func streamCheckStart(w io.Writer, check Check) {
	if w != nil {
		fmt.Fprintf(w, "  %s  %s...", ui.RenderMuted("○"), check.Name())
	}
}

func (d *Doctor) runTimedCheck(check Check, ctx *CheckContext) *CheckResult {
	start := time.Now()
	result := check.Run(ctx)
	result.Elapsed = time.Since(start)
	setCheckMetadata(check, result)
	return result
}

func setCheckMetadata(check Check, result *CheckResult) {
	if result.Name == "" {
		result.Name = check.Name()
	}
	if cg, ok := check.(categoryGetter); ok && result.Category == "" {
		result.Category = cg.Category()
	}
}

func streamCheckResult(w io.Writer, result *CheckResult, slowThreshold time.Duration, report *Report) {
	if w == nil {
		return
	}
	statusIcon := statusIcon(result.Status)
	isSlow := slowThreshold > 0 && result.Elapsed >= slowThreshold
	slowIndicator := "  "
	if isSlow {
		report.Summary.Slow++
		slowIndicator = "⏳"
	}
	fmt.Fprintf(w, "\r  %s%s%s", statusIcon, slowIndicator, result.Name)
	writeResultMessage(w, result, isSlow)
	fmt.Fprintln(w)
}

func statusIcon(status CheckStatus) string {
	switch status {
	case StatusOK:
		return ui.RenderPassIcon()
	case StatusWarning:
		return ui.RenderWarnIcon()
	case StatusError:
		return ui.RenderFailIcon()
	default:
		return ""
	}
}

func writeResultMessage(w io.Writer, result *CheckResult, isSlow bool) {
	if result.Message != "" {
		fmt.Fprintf(w, "%s", ui.RenderMuted(" "+result.Message))
	}
	if isSlow {
		fmt.Fprintf(w, "%s", ui.RenderMuted(" ("+formatDuration(result.Elapsed)+")"))
	}
}

// FixStreaming runs all checks with auto-fix and optional real-time output.
// If w is non-nil, prints each check name as it starts and result when done.
// If slowThreshold > 0, shows hourglass icon for slow checks.
func (d *Doctor) FixStreaming(ctx *CheckContext, w io.Writer, slowThreshold time.Duration) *Report {
	report := NewReport()

	for _, check := range d.checks {
		streamCheckStart(w, check)
		result, committedFixingLine := d.runTimedFix(check, ctx, w)
		streamFixResult(w, result, slowThreshold, report, committedFixingLine)

		report.Add(result)
	}

	return report
}

func (d *Doctor) runTimedFix(check Check, ctx *CheckContext, w io.Writer) (*CheckResult, bool) {
	start := time.Now()
	result := check.Run(ctx)
	setCheckMetadata(check, result)
	committedFixingLine := false
	if result.Status != StatusOK && check.CanFix() {
		result, committedFixingLine = d.applyFix(check, ctx, w, result)
	}
	result.Elapsed = time.Since(start)
	return result, committedFixingLine
}

func (d *Doctor) applyFix(check Check, ctx *CheckContext, w io.Writer, result *CheckResult) (*CheckResult, bool) {
	committedFixingLine := false
	if w != nil {
		fmt.Fprintf(w, "\r  %s %s%s\n", ui.RenderFixIcon(), check.Name(), ui.RenderMuted(" (fixing)..."))
		committedFixingLine = true
	}

	err := safeFixCheck(check, ctx)
	if err == nil {
		result = d.runTimedCheckWithoutTiming(check, ctx)
		if result.Status == StatusOK {
			result.Message += " (fixed)"
			result.Fixed = true
		}
		return result, committedFixingLine
	}
	if errors.Is(err, ErrSkippedNoStart) {
		result.Details = append(result.Details, "Skipped: --no-start suppresses startup")
	} else {
		result.Details = append(result.Details, "Fix failed: "+err.Error())
	}
	return result, committedFixingLine
}

func (d *Doctor) runTimedCheckWithoutTiming(check Check, ctx *CheckContext) *CheckResult {
	result := check.Run(ctx)
	setCheckMetadata(check, result)
	return result
}

func streamFixResult(w io.Writer, result *CheckResult, slowThreshold time.Duration, report *Report, committedFixingLine bool) {
	if w == nil {
		return
	}
	status := statusIcon(result.Status)
	if result.Fixed {
		status = ui.RenderFixIcon()
	}
	isSlow := slowThreshold > 0 && result.Elapsed >= slowThreshold
	slowIndicator := "  "
	if result.Fixed {
		slowIndicator = " "
	}
	if isSlow {
		report.Summary.Slow++
		slowIndicator = "⏳"
	}
	linePrefix := "\r  "
	if committedFixingLine {
		linePrefix = "  "
	}
	fmt.Fprintf(w, "%s%s%s%s", linePrefix, status, slowIndicator, result.Name)
	writeResultMessage(w, result, isSlow)
	fmt.Fprintln(w)
}

// BaseCheck provides a base implementation for checks that don't support auto-fix.
// Embed this in custom checks to get default CanFix() and Fix() implementations.
type BaseCheck struct {
	CheckName        string
	CheckDescription string
	CheckCategory    string // Category for grouping (e.g., CategoryCore)
}

// Category returns the check's category for grouping in output.
func (b *BaseCheck) Category() string {
	return b.CheckCategory
}

// Name returns the check name.
func (b *BaseCheck) Name() string {
	return b.CheckName
}

// Description returns the check description.
func (b *BaseCheck) Description() string {
	return b.CheckDescription
}

// CanFix returns false by default.
func (b *BaseCheck) CanFix() bool {
	return false
}

// Fix returns an error indicating this check cannot be auto-fixed.
func (b *BaseCheck) Fix(_ *CheckContext) error {
	return ErrCannotFix
}

// FixableCheck provides a base implementation for checks that support auto-fix.
// Embed this and override CanFix() to return true, and implement Fix().
type FixableCheck struct {
	BaseCheck
}

// CanFix returns true for fixable checks.
func (f *FixableCheck) CanFix() bool {
	return true
}
