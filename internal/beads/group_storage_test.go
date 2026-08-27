package beads

import "testing"

func TestValidateGroupStorage(t *testing.T) {
	validMembers := []string{"gastown/crew/max", "*/witness", "@town"}
	if err := ValidateGroupStorage("ops-team", validMembers); err != nil {
		t.Fatalf("ValidateGroupStorage() error = %v", err)
	}

	for _, members := range [][]string{
		{"null"},
		{"gastown/crew/max,gastown/crew/dom"},
		{" gastown/crew/max"},
		{"gastown/crew/max "},
		{"gastown/crew/max\nname: wrong"},
		{"gastown/crew/max\t"},
	} {
		if err := ValidateGroupStorage("ops-team", members); err == nil {
			t.Fatalf("ValidateGroupStorage(%q, %q) = nil, want error", "ops-team", members)
		}
	}
	if err := ValidateGroupStorage("null", validMembers); err == nil {
		t.Fatal("ValidateGroupStorage(\"null\", members) = nil, want error")
	}
	for _, fields := range []*GroupFields{
		{Name: "ops-team", CreatedBy: "null"},
		{Name: "ops-team", CreatedAt: "created\nby: wrong"},
	} {
		if err := validateGroupFieldsStorage(fields); err == nil {
			t.Fatalf("validateGroupFieldsStorage(%+v) = nil, want error", fields)
		}
	}
}

func TestGroupWritersRejectUnstorableValuesBeforeRunningBD(t *testing.T) {
	b := &Beads{}
	if _, err := b.CreateGroupBead("null", &GroupFields{Members: []string{"mayor/"}}); err == nil {
		t.Fatal("CreateGroupBead accepted an unstorable group name")
	}
	if _, err := b.UpdateGroupMembers("ops-team", []string{"null"}); err == nil {
		t.Fatal("UpdateGroupMembers accepted an unstorable member")
	}
	if _, err := b.AddGroupMember("ops-team", "gastown/crew/max,gas/crew/dom"); err == nil {
		t.Fatal("AddGroupMember accepted an unstorable member")
	}
	if _, err := b.RemoveGroupMember("null", "mayor/"); err == nil {
		t.Fatal("RemoveGroupMember accepted an unstorable group name")
	}
	if _, err := b.CreateGroupBead("ops-team", &GroupFields{CreatedBy: "mayor\nname: wrong"}); err == nil {
		t.Fatal("CreateGroupBead accepted unstorable metadata")
	}
}
