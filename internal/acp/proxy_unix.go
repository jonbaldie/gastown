//go:build unix

package acp

import (
	"os"
	"syscall"
	"time"

	"github.com/jonbaldie/gastown/internal/util"
)

// signalsToHandle returns the signals that Forward() should listen for.
// On Unix, we handle both SIGTERM and SIGINT for graceful shutdown.
func signalsToHandle() []os.Signal {
	return []os.Signal{syscall.SIGTERM, syscall.SIGINT}
}

// setupProcessGroup configures the command to run in its own process group.
// This allows us to terminate the agent and all its children on shutdown.
func (p *Proxy) setupProcessGroup() {
	util.SetProcessGroup(p.cmd)
}

// isProcessAlive checks if the agent process is still running.
// On Unix, we use signal 0 to check process liveness.
func (p *Proxy) isProcessAlive() bool {
	if p.cmd == nil || p.cmd.Process == nil {
		return false
	}
	err := p.cmd.Process.Signal(syscall.Signal(0))
	return err == nil
}

// terminateProcess gracefully terminates the agent process.
// On Unix, we send SIGTERM to the process group, then SIGKILL after 2 seconds
// if the process hasn't exited.
func (p *Proxy) terminateProcess() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	process := p.cmd.Process
	debugLog(p.townRoot, "[Proxy] Shutdown: sending SIGTERM to agent process (pid=%d)", process.Pid)
	processGroupID := signalProcessOrGroup(process.Pid, syscall.SIGTERM)
	time.AfterFunc(2*time.Second, func() {
		if p.cmd.ProcessState == nil || !p.cmd.ProcessState.Exited() {
			forceKillProcessOrGroup(process.Pid, processGroupID)
		}
	})
}

func signalProcessOrGroup(pid int, signal syscall.Signal) int {
	processGroupID, err := syscall.Getpgid(pid)
	if err != nil || processGroupID == syscall.Getpgrp() {
		_ = syscall.Kill(pid, signal)
		return processGroupID
	}
	_ = syscall.Kill(-processGroupID, signal)
	return processGroupID
}

func forceKillProcessOrGroup(pid, processGroupID int) {
	if processGroupID == 0 {
		processGroupID, _ = syscall.Getpgid(pid)
	}
	if processGroupID > 0 && processGroupID != syscall.Getpgrp() {
		_ = syscall.Kill(-processGroupID, syscall.SIGKILL)
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
