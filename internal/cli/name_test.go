package cli

import "testing"

func TestName_DefaultIsGt(t *testing.T) {
	t.Setenv("GT_COMMAND", "")

	got := Name()
	if got != "gt" {
		t.Errorf("Name() = %q, want %q", got, "gt")
	}
}

func TestName_RespectsGT_COMMAND(t *testing.T) {
	t.Setenv("GT_COMMAND", "gastown")

	got := Name()
	if got != "gastown" {
		t.Errorf("Name() = %q, want %q", got, "gastown")
	}
}

func TestName_ReflectsEnvironment(t *testing.T) {
	t.Setenv("GT_COMMAND", "first")

	first := Name()
	if first != "first" {
		t.Fatalf("Name() = %q, want %q", first, "first")
	}

	t.Setenv("GT_COMMAND", "second")
	second := Name()
	if second != "second" {
		t.Errorf("Name() returned %q after env change, want %q", second, "second")
	}
}
