package mail

import "testing"

// TestRigScopedRoleNameAliasesToTheRole records why an Agent must never be
// named after a Role. It is the reason crew.validateCrewName and
// polecat.reservedPolecatName reject these names.
//
// Address normalization strips the "crew" and "polecats" segment, so a worker
// address "<rig>/crew/<name>" becomes the identity "<rig>/<name>". That is the
// same identity a Rig-scoped Role already has. For the Mayor and the Deacon the
// collision goes further, because normalization also resolves "<rig>/mayor" to
// the Town Mayor (gt-te23), and BeadsMessage.ToMessage normalizes a second time
// on read.
//
// No parser can separate the two: the collision is in the address space itself.
// If this test ever goes red because normalization changed, the reserved-name
// checks may be able to relax; until then they are the only defence.
func TestRigScopedRoleNameAliasesToTheRole(t *testing.T) {
	tests := []struct {
		worker string
		role   string
	}{
		{"gastown/crew/mayor", "mayor/"},
		{"gastown/crew/deacon", "deacon/"},
		{"gastown/crew/witness", "gastown/witness"},
		{"gastown/crew/refinery", "gastown/refinery"},
		{"gastown/polecats/mayor", "mayor/"},
	}

	for _, tt := range tests {
		t.Run(tt.worker, func(t *testing.T) {
			// The second normalization is the one ToMessage applies on read.
			routed := AddressToIdentity(identityToAddress(AddressToIdentity(tt.worker)))
			want := AddressToIdentity(tt.role)
			if routed != want {
				t.Fatalf("expected %q to alias to the identity of %q (%q), got %q",
					tt.worker, tt.role, want, routed)
			}
		})
	}
}

// TestRigScopedRoleAliasIsIntentional pins the two normalization rules the
// collision comes from, so that removing the reserved-name checks stays a
// deliberate act rather than an accident.
func TestRigScopedRoleAliasIsIntentional(t *testing.T) {
	if got := normalizeAddress("gastown/mayor"); got != "mayor/" {
		t.Fatalf(`normalizeAddress("gastown/mayor") = %q, want "mayor/"`, got)
	}
	if got := normalizeAddress("gastown/crew/mayor"); got != "gastown/mayor" {
		t.Fatalf(`normalizeAddress("gastown/crew/mayor") = %q, want "gastown/mayor"`, got)
	}
}
