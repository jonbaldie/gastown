package crew

import (
	"errors"
	"testing"
)

// reservedNames are the infrastructure Role names a new crew worker must not be
// given. Mixed case is included because the rule ignores case.
var reservedNames = []string{
	"mayor", "deacon", "witness", "refinery",
	"crew", "polecats", "overseer", "boot", "dog",
	"Mayor", "WITNESS",
}

// TestValidateNewCrewNameRejectsReservedNames locks down the addressing collision.
//
// A crew worker address normalizes to "<rig>/<name>", so a worker named after a
// Role resolves to that Role instead: "gastown/crew/mayor" becomes
// "gastown/mayor" and then "mayor/", and the worker's mail is delivered to the
// Town Mayor with no error reported to the sender. Polecats have always
// rejected these names; crew workers must too.
func TestValidateNewCrewNameRejectsReservedNames(t *testing.T) {
	for _, name := range reservedNames {
		t.Run(name, func(t *testing.T) {
			err := validateNewCrewName(name)
			if err == nil {
				t.Fatalf("validateNewCrewName(%q) accepted a reserved name", name)
			}
			if !errors.Is(err, ErrInvalidCrewName) {
				t.Fatalf("validateNewCrewName(%q) = %v, want ErrInvalidCrewName", name, err)
			}
		})
	}
}

// TestValidateCrewNameKeepsReservedNamesManageable is the other half of the
// rule. The reserved-name check guards creation only. A worker created before
// the check existed must still be reachable by Get, Stop, and Remove, which all
// gate on validateCrewName, otherwise the fix strands the very state it exists
// to prevent.
func TestValidateCrewNameKeepsReservedNamesManageable(t *testing.T) {
	for _, name := range reservedNames {
		t.Run(name, func(t *testing.T) {
			if err := validateCrewName(name); err != nil {
				t.Fatalf("validateCrewName(%q) = %v, want nil so the worker stays removable", name, err)
			}
		})
	}
}

// TestValidateNewCrewNameAcceptsOrdinaryNames guards against the reserved-name
// check rejecting names that merely contain a Role name.
func TestValidateNewCrewNameAcceptsOrdinaryNames(t *testing.T) {
	for _, name := range []string{"max", "dom", "mayoral", "witnesses", "crew2", "doghouse"} {
		t.Run(name, func(t *testing.T) {
			if err := validateNewCrewName(name); err != nil {
				t.Fatalf("validateNewCrewName(%q) = %v, want nil", name, err)
			}
		})
	}
}
