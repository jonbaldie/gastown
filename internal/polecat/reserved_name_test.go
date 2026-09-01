package polecat

import (
	"strings"
	"testing"

	"github.com/jonbaldie/gastown/internal/rig"
)

func TestAddWithOptionsRejectsReservedNameBeforeCreatingFiles(t *testing.T) {
	mgr := &Manager{rig: &rig.Rig{Path: t.TempDir()}}
	for _, name := range []string{"mayor", "WITNESS", "crew", "dog"} {
		t.Run(name, func(t *testing.T) {
			_, err := AddWithOptions(mgr, name, AddOptions{})
			if err == nil {
				t.Fatalf("AddWithOptions(%q) succeeded, want reserved-name error", name)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("AddWithOptions(%q) error = %v, want reserved-name error", name, err)
			}
		})
	}
}

func TestValidateNewPolecatNameAllowsEscapedCrewNamespace(t *testing.T) {
	if err := ValidateNewPolecatName("crew-max"); err != nil {
		t.Fatalf("ValidateNewPolecatName(\"crew-max\") error = %v, want nil", err)
	}
}
