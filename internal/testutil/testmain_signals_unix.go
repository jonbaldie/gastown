//go:build !windows

package testutil

import (
	"os"
	"syscall"
)

func testMainTerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
