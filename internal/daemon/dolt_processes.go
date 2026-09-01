package daemon

import (
	"os"

	"github.com/jonbaldie/gastown/internal/doltserver"
)

func uniqueDoltServerPIDs(listeners []doltserver.DoltListener) []int {
	seen := make(map[int]bool, len(listeners))
	pids := make([]int, 0, len(listeners))
	for _, listener := range listeners {
		if seen[listener.PID] {
			continue
		}
		seen[listener.PID] = true
		pids = append(pids, listener.PID)
	}
	return pids
}

func signalDoltServers(pids []int, force bool) {
	for _, pid := range pids {
		process, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if force {
			_ = sendKillSignal(process)
			continue
		}
		_ = sendTermSignal(process)
	}
}

func forceStopRemainingDoltServers() {
	for _, listener := range doltserver.FindAllDoltListeners() {
		process, err := os.FindProcess(listener.PID)
		if err == nil {
			_ = sendKillSignal(process)
		}
	}
}

func maxDoltServersKilled(killed int) int {
	if killed < 0 {
		return 0
	}
	return killed
}
