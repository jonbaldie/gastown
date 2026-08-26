//go:build windows

package worker

import (
	"os"
)

func terminateServePID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

func killServePID(pid int) error {
	return terminateServePID(pid)
}

func terminateServeGroup(pid int) error { return terminateServePID(pid) }

func killServeGroup(pid int) error { return killServePID(pid) }
