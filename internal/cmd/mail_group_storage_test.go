package cmd

import (
	"strings"
	"testing"

	"github.com/jonbaldie/gastown/internal/beads"
)

// TestMemberPatternSurvivesGroupBeadRoundTrip is the property the member
// validator exists to hold: every pattern the validator accepts must come back
// unchanged, and alone, from the group bead it is written into.
//
// A group bead description is line oriented and unescaped. Every member goes on
// one "members:" line joined by commas, and the parser splits on newlines, then
// on commas, then trims. A member that carries a newline, a comma, or leading
// space is therefore read back as a different member, as several members, or as
// a different field altogether. The validator is the only boundary between a
// user supplied pattern and that format, so the property is checked here.
func TestMemberPatternSurvivesGroupBeadRoundTrip(t *testing.T) {
	accepted := []string{
		"gastown/crew/max",
		"mayor/",
		"deacon/",
		"*/witness",
		"gastown/*",
		"gastown/crew/*",
		"@town",
		"@crew",
		"ops-team",
		"all_witnesses",
	}

	for _, pattern := range accepted {
		t.Run(pattern, func(t *testing.T) {
			if !isValidMemberPattern(pattern) {
				t.Fatalf("isValidMemberPattern(%q) = false, want true", pattern)
			}

			in := &beads.GroupFields{
				Name:      "leads",
				Members:   []string{pattern},
				CreatedBy: "mayor/",
				CreatedAt: "2026-01-01T00:00:00Z",
			}
			out := beads.ParseGroupFields(beads.FormatGroupDescription("Group: leads", in))

			if out.Name != in.Name {
				t.Errorf("name changed: got %q, want %q", out.Name, in.Name)
			}
			if len(out.Members) != 1 || out.Members[0] != pattern {
				t.Errorf("members changed: got %q, want [%q]", out.Members, pattern)
			}
			if out.CreatedBy != in.CreatedBy {
				t.Errorf("created_by changed: got %q, want %q", out.CreatedBy, in.CreatedBy)
			}
		})
	}
}

// TestMemberPatternRejectsUnstorablePatterns pins the specific characters that
// break the storage format. Each of these was accepted before, and each one
// changed the stored group in a way the user never asked for and never saw.
func TestMemberPatternRejectsUnstorablePatterns(t *testing.T) {
	for what, pattern := range unstorablePatterns {
		t.Run(what, func(t *testing.T) {
			if isValidMemberPattern(pattern) {
				t.Fatalf("isValidMemberPattern(%q) = true, want false", pattern)
			}
		})
	}
}

// unstorablePatterns are member patterns that were all accepted before, and
// each one changed the stored group in a way the user never asked for and
// never saw.
var unstorablePatterns = map[string]string{
	"field injection":         "gastown/x\nname: evil",
	"member injection":        "gastown/x\nmembers: gastown/crew/mallory",
	"carriage return":         "gastown/x\rname: evil",
	"comma splits the member": "gastown/x,gastown/y",
	"leading space is lost":   " gastown/x",
	"trailing space is lost":  "gastown/x ",
	"tab":                     "gastown/x\tfoo",
	"at pattern with newline": "@town\nname: evil",
	"group name with newline": "ops\nname: evil",
	"null empties the list":   "null",
}

// TestGroupCommandsRejectUnstorablePatterns proves the guard reaches a user.
//
// The validators are unexported, so the tests above can only show the rule is
// correct, not that it is wired in. These call the real RunE of `gt mail group
// create` and `gt mail group add`, which validate before they look for a town,
// so the rejection is observable without a Beads database.
func TestGroupCommandsRejectUnstorablePatterns(t *testing.T) {
	for what, pattern := range unstorablePatterns {
		t.Run("create/"+what, func(t *testing.T) {
			err := runGroupCreate(groupCreateCmd, []string{"leads", pattern})
			if err == nil {
				t.Fatalf("gt mail group create accepted member %q", pattern)
			}
			if !strings.Contains(err.Error(), "invalid member pattern") {
				t.Fatalf("gt mail group create rejected %q for the wrong reason: %v", pattern, err)
			}
		})
		t.Run("add/"+what, func(t *testing.T) {
			err := runGroupAdd(groupAddCmd, []string{"leads", pattern})
			if err == nil {
				t.Fatalf("gt mail group add accepted member %q", pattern)
			}
			if !strings.Contains(err.Error(), "invalid member pattern") {
				t.Fatalf("gt mail group add rejected %q for the wrong reason: %v", pattern, err)
			}
		})
	}
}

// TestGroupCreateRejectsUnstorableGroupName covers the other field on the same
// line-oriented description. A group named "null" reads back with an empty
// name, so it is filed under "" and can never be found again.
func TestGroupCreateRejectsUnstorableGroupName(t *testing.T) {
	err := runGroupCreate(groupCreateCmd, []string{"null", "mayor/"})
	if err == nil {
		t.Fatal("gt mail group create accepted the group name \"null\"")
	}
	if !strings.Contains(err.Error(), "invalid group name") {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}
}

// TestGroupCommandsAcceptStorablePatterns is the negative control: a valid
// member must get past validation. It cannot get further without a town, so the
// test asserts only that the failure is no longer a validation failure.
func TestGroupCommandsAcceptStorablePatterns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, pattern := range []string{"gastown/crew/max", "mayor/", "@town", "ops-team"} {
		t.Run(pattern, func(t *testing.T) {
			err := runGroupAdd(groupAddCmd, []string{"leads", pattern})
			if err != nil && strings.Contains(err.Error(), "invalid member pattern") {
				t.Fatalf("gt mail group add rejected valid member %q: %v", pattern, err)
			}
		})
	}
}
