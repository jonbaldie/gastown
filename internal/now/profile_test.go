package now

import "testing"

func TestParseProfileTwoTokenKnownEffortIsEffort(t *testing.T) {
	profile, err := ParseProfile("cursor:xhigh", DefaultMayorEffort)
	if err != nil {
		t.Fatalf("ParseProfile: %v", err)
	}
	if profile.Runtime != "cursor" || profile.Model != "" || profile.Effort != "xhigh" {
		t.Fatalf("got %+v, want runtime=cursor effort=xhigh", profile)
	}
}

func TestParseProfileTwoTokenUnknownIsModel(t *testing.T) {
	profile, err := ParseProfile("cursor:grok-4.6", DefaultMayorEffort)
	if err != nil {
		t.Fatalf("ParseProfile: %v", err)
	}
	if profile.Model != "grok-4.6" || profile.Effort != DefaultMayorEffort {
		t.Fatalf("got %+v, want model grok-4.6 and default effort", profile)
	}
}

func TestParseProfileSlashInModel(t *testing.T) {
	profile, err := ParseProfile("cursor:org/model:high", DefaultMayorEffort)
	if err != nil {
		t.Fatalf("ParseProfile: %v", err)
	}
	if profile.Runtime != "cursor" || profile.Model != "org/model" || profile.Effort != "high" {
		t.Fatalf("got %+v, want cursor org/model high", profile)
	}
}

// TestParseProfileRejectsUnknownDefaultEffort guards the Format()/ParseProfile
// round-trip: Format() encodes a Model-less profile as "runtime:effort", and
// ParseProfile only reads that second token back as an effort when it is a
// recognized level. An unrecognized defaultEffort would otherwise silently
// round-trip into a Model on reparse. Found by fuzzing (ParseProfile("0", "0")).
func TestParseProfileRejectsUnknownDefaultEffort(t *testing.T) {
	if _, err := ParseProfile("0", "0"); err == nil {
		t.Fatal("ParseProfile(\"0\", \"0\") succeeded with an unrecognized default effort")
	}
	if _, err := ParseProfile("cursor", "not-a-real-effort"); err == nil {
		t.Fatal("ParseProfile with unrecognized defaultEffort succeeded")
	}
}

// TestParseProfileFormatRoundTrips checks that every successfully parsed
// profile survives a Format() -> ParseProfile() round trip.
func TestParseProfileFormatRoundTrips(t *testing.T) {
	specs := []string{"", "cursor", "cursor:high", "cursor:grok-4.6", "cursor:grok-4.6:high", "cursor:org/model:xhigh"}
	for _, spec := range specs {
		profile, err := ParseProfile(spec, DefaultMayorEffort)
		if err != nil {
			t.Fatalf("ParseProfile(%q): %v", spec, err)
		}
		reparsed, err := ParseProfile(profile.Format(), DefaultMayorEffort)
		if err != nil {
			t.Fatalf("ParseProfile(%q) -> Format() = %q -> reparse error: %v", spec, profile.Format(), err)
		}
		if reparsed != profile {
			t.Fatalf("round-trip mismatch for %q: %+v -> %q -> %+v", spec, profile, profile.Format(), reparsed)
		}
	}
}
