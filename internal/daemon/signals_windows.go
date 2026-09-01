//go:build windows

package daemon

import (
	"os"
	"syscall"
)

func daemonSignals() []os.Signal {
	return []os.Signal{
		syscall.SIGINT,
		syscall.SIGTERM,
	}
}

func isLifecycleSignal(_ os.Signal) bool {
	return false
}

func isReloadRestartSignal(_ os.Signal) bool {
	return false
}
