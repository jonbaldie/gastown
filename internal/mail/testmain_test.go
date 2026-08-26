package mail

import (
	"testing"

	"github.com/jonbaldie/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.RunTestMain(m, testutil.TerminateDoltContainer)
}
