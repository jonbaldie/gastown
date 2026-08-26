//go:build windows

package tmux

import "os"

func tmuxTestTerminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
