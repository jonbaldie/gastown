package protocol

import (
	"testing"
	"time"
)

// TestFixNeededPayloadRoundTripLosesMultilineError is a regression test for
// a bug found via CGPT metamorphic testing (see BUGS-FOUND.md).
//
// FIX_NEEDED payloads with a multi-line Error field (e.g. real git/test
// stderr output, which is virtually always multi-line) used to be silently
// truncated to their first line when the message was parsed back, because
// parseField scans the body line-by-line for a "Key: value" prefix and the
// Error field was written before other fields with no escaping/framing.
// Worse, a later line inside the Error text that happened to look like
// "Key: value" for a subsequent field (e.g. "Attempt-Number: 999" appearing
// in test output) was misread as that field, silently corrupting protocol
// state. The fix writes Error last and parses it with parseErrorField,
// which captures to the end of the body instead of stopping at the first
// line. The same bug applied to ParseMergeFailedPayload; see
// TestMergeFailedPayloadRoundTripPreservesMultilineError below.
func TestFixNeededPayloadRoundTripLosesMultilineError(t *testing.T) {
	original := FixNeededPayload{
		Branch:        "feature/x",
		Issue:         "gt-123",
		Polecat:       "dave",
		Rig:           "laneassist",
		FailedAt:      time.Now().Truncate(time.Second),
		FailureType:   "tests",
		Error:         "FAIL: TestFoo\n--- FAIL: TestFoo (0.00s)\nAttempt-Number: 999\nmore output",
		TargetBranch:  "main",
		AttemptNumber: 1,
		MRBeadID:      "gt-mr-42",
	}

	body := formatFixNeededBody(original)
	parsed, err := ParseFixNeededPayload(body)
	if err != nil {
		t.Fatalf("ParseFixNeededPayload: %v", err)
	}

	if parsed.Error != original.Error {
		t.Errorf("round trip lost data:\n  original.Error = %q\n  parsed.Error   = %q", original.Error, parsed.Error)
	}
	if parsed.AttemptNumber != original.AttemptNumber {
		t.Errorf("a line inside the Error field (%q) was misparsed as Attempt-Number: got %d, want %d",
			"Attempt-Number: 999", parsed.AttemptNumber, original.AttemptNumber)
	}
	if parsed.MRBeadID != original.MRBeadID {
		t.Errorf("MR-Bead-ID corrupted by reordering: got %q, want %q", parsed.MRBeadID, original.MRBeadID)
	}
	if parsed.Branch != original.Branch || parsed.Rig != original.Rig || parsed.Polecat != original.Polecat {
		t.Errorf("required fields corrupted: got Branch=%q Rig=%q Polecat=%q, want Branch=%q Rig=%q Polecat=%q",
			parsed.Branch, parsed.Rig, parsed.Polecat, original.Branch, original.Rig, original.Polecat)
	}
}

// TestMergeFailedPayloadRoundTripPreservesMultilineError is the
// MERGE_FAILED counterpart to the FIX_NEEDED test above: same parseField
// helper, same shape of free-text Error field, same fix (parseErrorField).
func TestMergeFailedPayloadRoundTripPreservesMultilineError(t *testing.T) {
	original := MergeFailedPayload{
		Branch:       "feature/y",
		Issue:        "gt-456",
		Polecat:      "hilde",
		Rig:          "laneassist",
		FailedAt:     time.Now().Truncate(time.Second),
		FailureType:  "merge-conflict",
		Error:        "CONFLICT (content): Merge conflict in foo.go\nRig: not-the-real-rig\nAutomatic merge failed",
		TargetBranch: "main",
	}

	body := formatMergeFailedBody(original)
	parsed, err := ParseMergeFailedPayload(body)
	if err != nil {
		t.Fatalf("ParseMergeFailedPayload: %v", err)
	}

	if parsed.Error != original.Error {
		t.Errorf("round trip lost data:\n  original.Error = %q\n  parsed.Error   = %q", original.Error, parsed.Error)
	}
	if parsed.Rig != original.Rig {
		t.Errorf("a line inside the Error field was misparsed as Rig: got %q, want %q", parsed.Rig, original.Rig)
	}
}

// TestFixNeededPayloadOptionalFieldNotShadowedByErrorText is a regression
// test for a gap the code-review Spec pass caught in the fix above:
// MR-Bead-ID is written conditionally (only when non-empty), so when it's
// absent there is no real "MR-Bead-ID: " line earlier in the body to win
// parseField's first-match scan. Without the "stop at Error:" boundary in
// parseField, a lookalike line inside the free-text Error section (e.g.
// build output that happens to print "MR-Bead-ID: <something>") would still
// be picked up as if it were the real field.
func TestFixNeededPayloadOptionalFieldNotShadowedByErrorText(t *testing.T) {
	original := FixNeededPayload{
		Branch:      "feature/x",
		Polecat:     "dave",
		Rig:         "laneassist",
		FailureType: "tests",
		Error:       "build log:\nMR-Bead-ID: bogus-from-log\nmore output",
		// MRBeadID intentionally left empty: formatFixNeededBody omits the
		// real "MR-Bead-ID: " line entirely in this case.
	}

	body := formatFixNeededBody(original)
	parsed, err := ParseFixNeededPayload(body)
	if err != nil {
		t.Fatalf("ParseFixNeededPayload: %v", err)
	}

	if parsed.MRBeadID != "" {
		t.Errorf("MR-Bead-ID shadowed by a lookalike line inside Error text: got %q, want empty", parsed.MRBeadID)
	}
	if parsed.Error != original.Error {
		t.Errorf("round trip lost data:\n  original.Error = %q\n  parsed.Error   = %q", original.Error, parsed.Error)
	}
}

// TestParseErrorFieldEdgeCases exercises parseErrorField directly against
// shapes that are easy to get wrong: an empty Error, one ending in its own
// trailing newline, and a body with no Error field at all.
func TestParseErrorFieldEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"absent", "Branch: x\nRig: y\n", ""},
		{"empty", "Branch: x\nError: \n", ""},
		{"single line", "Branch: x\nError: boom\n", "boom"},
		{"trailing newline in value", "Branch: x\nError: boom\n\n", "boom\n"},
		{"multi line", "Branch: x\nError: line1\nline2\nline3\n", "line1\nline2\nline3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseErrorField(tc.body); got != tc.want {
				t.Errorf("parseErrorField(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}
