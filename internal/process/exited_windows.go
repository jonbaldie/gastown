//go:build windows

package process

// Exited reports whether a process is no longer active.
func Exited(pid int) bool { return !Alive(pid) }
