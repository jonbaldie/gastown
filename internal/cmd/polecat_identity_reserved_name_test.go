package cmd

import (
	"strings"
	"testing"
)

func TestPolecatIdentityCommandsRejectReservedNamesBeforeTownLookup(t *testing.T) {
	if err := runPolecatIdentityAdd(polecatIdentityAddCmd, []string{"missing-rig", "mayor"}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("identity add error = %v, want reserved-name error", err)
	}
	if err := runPolecatIdentityRename(polecatIdentityRenameCmd, []string{"missing-rig", "old", "WITNESS"}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("identity rename error = %v, want reserved-name error", err)
	}
}
