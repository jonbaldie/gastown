//go:build !windows

package tmux

import (
	"os"
	"syscall"
)

func tmuxTestTerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
