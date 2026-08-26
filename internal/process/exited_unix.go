//go:build !windows

package process

import (
	"os/exec"
	"strconv"
	"strings"
)

// Exited reports whether a process has exited, including a zombie that is
// awaiting reaping by its parent. A zombie cannot keep files, sockets, or a
// Town alive, so lifecycle teardown must treat it as exited.
func Exited(pid int) bool {
	if !Alive(pid) {
		return true
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if err != nil {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
