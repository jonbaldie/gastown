//go:build !windows

package worker

import "syscall"

func terminateServePID(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

func killServePID(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}

func terminateServeGroup(pid int) error {
	pgid, err := syscall.Getpgid(pid)
	if err == nil && pgid == pid {
		return syscall.Kill(-pgid, syscall.SIGTERM)
	}
	return terminateServePID(pid)
}

func killServeGroup(pid int) error {
	pgid, err := syscall.Getpgid(pid)
	if err == nil && pgid == pid {
		return syscall.Kill(-pgid, syscall.SIGKILL)
	}
	return killServePID(pid)
}
