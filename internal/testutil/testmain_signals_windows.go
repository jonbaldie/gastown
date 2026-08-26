//go:build windows

package testutil

import "os"

func testMainTerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
