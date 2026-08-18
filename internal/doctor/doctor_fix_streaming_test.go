package doctor

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// renderCarriageReturns simulates a dumb TTY: \r rewinds the current line,
// \n commits it. Used to recover what a user would actually see.
func renderCarriageReturns(s string) string {
	var lines []string
	var cur []byte
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\r':
			cur = cur[:0]
		case '\n':
			lines = append(lines, string(cur))
			cur = cur[:0]
		default:
			cur = append(cur, s[i])
		}
	}
	if len(cur) > 0 {
		lines = append(lines, string(cur))
	}
	return strings.Join(lines, "\n")
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

// captureStdoutAndWriter runs fn with os.Stdout and the returned writer as the
// same pipe, matching `gt doctor --fix` (FixStreaming writes to stdout, and
// some Check.Fix methods also fmt.Printf to stdout).
func captureStdoutAndWriter(t *testing.T, fn func(w io.Writer)) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()

	done := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(r)
		done <- data
	}()

	fn(w)
	_ = w.Close()
	return string(<-done)
}

func renderedFixStreamingOutput(t *testing.T, raw string) string {
	t.Helper()
	return strings.TrimSpace(stripANSI(renderCarriageReturns(raw)))
}

// TestDoctorFixStreaming_ClaudeSettingsPrintsPostFixStatus is the regression
// for: `gt doctor --fix` prints the pre-fix claude-settings failure even after
// the files are repaired; the next `gt doctor` run is green.
func TestDoctorFixStreaming_ClaudeSettingsPrintsPostFixStatus(t *testing.T) {
	tmpDir := t.TempDir()
	mayorSettings := filepath.Join(tmpDir, "mayor", ".claude", "settings.json")
	createStaleSettings(t, mayorSettings, "PATH")

	d := NewDoctor()
	d.Register(NewClaudeSettingsCheck())
	ctx := &CheckContext{TownRoot: tmpDir}

	var report *Report
	raw := captureStdoutAndWriter(t, func(w io.Writer) {
		report = d.FixStreaming(ctx, w, 0)
	})
	rendered := renderedFixStreamingOutput(t, raw)
	t.Logf("rendered stdout:\n%s", rendered)

	next := NewClaudeSettingsCheck().Run(ctx)
	if next.Status != StatusOK {
		t.Fatalf("fixture invalid: next Run must be green; got %s: %s %v",
			next.Status, next.Message, next.Details)
	}

	if len(report.Checks) != 1 {
		t.Fatalf("expected 1 check result, got %d", len(report.Checks))
	}
	got := report.Checks[0]
	if got.Status != StatusOK || !got.Fixed {
		t.Errorf("report status=%s fixed=%v message=%q; want OK and fixed",
			got.Status, got.Fixed, got.Message)
	}
	if report.HasErrors() {
		t.Errorf("summary still HasErrors() after a successful fix")
	}

	const preFixMsg = "Found 1 stale Claude config file(s)"
	if strings.Contains(rendered, preFixMsg) {
		t.Errorf("printed pre-fix claude-settings status %q after the fix succeeded:\n%s",
			preFixMsg, rendered)
	}
	if strings.Contains(rendered, "✖") {
		t.Errorf("printed fail icon after a successful claude-settings fix:\n%s", rendered)
	}
	if !strings.Contains(rendered, "(fixed)") {
		t.Errorf("missing post-fix status on screen:\n%s", rendered)
	}
}

// printingFixCheck is the minimised FixStreaming seam: Run fails, Fix succeeds,
// and optionally writes a newline to stdout the way ClaudeSettingsCheck.Fix does.
type printingFixCheck struct {
	FixableCheck
	printDuringFix bool
	fixed          bool
}

func (p *printingFixCheck) Run(ctx *CheckContext) *CheckResult {
	if p.fixed {
		return &CheckResult{
			Name:    p.Name(),
			Status:  StatusOK,
			Message: "All Claude settings.json files are up to date",
		}
	}
	return &CheckResult{
		Name:    p.Name(),
		Status:  StatusError,
		Message: "Found 1 stale Claude config file(s)",
	}
}

func (p *printingFixCheck) Fix(ctx *CheckContext) error {
	if p.printDuringFix {
		_, _ = os.Stdout.WriteString("  Deleted stale: /tmp/settings.json\n")
	}
	p.fixed = true
	return nil
}

func TestDoctorFixStreaming_DoesNotPrintPreFixStatusWhenFixWritesStdout(t *testing.T) {
	for _, printDuringFix := range []bool{false, true} {
		t.Run(fmtPrintDuringFixName(printDuringFix), func(t *testing.T) {
			check := &printingFixCheck{
				FixableCheck:   FixableCheck{BaseCheck: BaseCheck{CheckName: "claude-settings"}},
				printDuringFix: printDuringFix,
			}
			d := NewDoctor()
			d.Register(check)

			raw := captureStdoutAndWriter(t, func(w io.Writer) {
				d.FixStreaming(&CheckContext{TownRoot: t.TempDir()}, w, 0)
			})
			rendered := renderedFixStreamingOutput(t, raw)

			if strings.Contains(rendered, "Found 1 stale Claude config file(s)") {
				t.Errorf("printed pre-fix status after a successful fix:\n%s", rendered)
			}
			if strings.Contains(rendered, "✖") {
				t.Errorf("printed fail icon after a successful fix:\n%s", rendered)
			}
			if !strings.Contains(rendered, "(fixed)") {
				t.Errorf("missing post-fix status:\n%s", rendered)
			}
		})
	}
}

func fmtPrintDuringFixName(printDuringFix bool) string {
	if printDuringFix {
		return "Fix writes newline to stdout"
	}
	return "silent Fix"
}
