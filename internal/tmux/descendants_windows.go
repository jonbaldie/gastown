//go:build windows

package tmux

import (
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// Windows Toolhelp32 API constants and types for process enumeration.
const (
	thSnapProcess = 0x00000002
	invalidHandle = ^uintptr(0)
)

type processEntry32 struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [260]uint16
}

type windowsProcessInfo struct {
	pid  uint32
	ppid uint32
	name string
}

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	createToolhelp32Snap = kernel32.NewProc("CreateToolhelp32Snapshot")
	process32FirstW      = kernel32.NewProc("Process32FirstW")
	process32NextW       = kernel32.NewProc("Process32NextW")
)

// hasDescendantWithNamesWindows enumerates all processes via the Toolhelp32 API
// (pure Go, no subprocess) and does a BFS from the given parent PID to find any
// descendant whose exe name (sans .exe) matches one of the target names.
func hasDescendantWithNamesWindows(ppidStr string, names []string, depth int) bool {
	const maxDepth = 10
	if depth > maxDepth || len(names) == 0 {
		return false
	}

	parentPid, err := strconv.ParseUint(ppidStr, 10, 32)
	if err != nil {
		return false
	}

	procs, ok := snapshotWindowsProcesses()
	if !ok {
		return false
	}
	return hasMatchingDescendant(procs, uint32(parentPid), normalizedProcessNames(names), depth, maxDepth)
}

func normalizedProcessNames(names []string) map[string]bool {
	nameSet := make(map[string]bool, len(names))
	for _, name := range names {
		nameSet[strings.ToLower(name)] = true
	}
	return nameSet
}

func snapshotWindowsProcesses() ([]windowsProcessInfo, bool) {
	// Take a snapshot of all processes.
	handle, _, _ := createToolhelp32Snap.Call(uintptr(thSnapProcess), 0)
	if handle == invalidHandle {
		return nil, false
	}
	defer syscall.CloseHandle(syscall.Handle(handle))

	var entry processEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	ret, _, _ := process32FirstW.Call(handle, uintptr(unsafe.Pointer(&entry)))
	if ret == 0 {
		return nil, false
	}

	var procs []windowsProcessInfo

	for {
		exeName := syscall.UTF16ToString(entry.ExeFile[:])
		procs = append(procs, windowsProcessInfo{
			pid:  entry.ProcessID,
			ppid: entry.ParentProcessID,
			name: exeName,
		})

		entry.Size = uint32(unsafe.Sizeof(entry))
		ret, _, _ = process32NextW.Call(handle, uintptr(unsafe.Pointer(&entry)))
		if ret == 0 {
			break
		}
	}
	return procs, true
}

func hasMatchingDescendant(procs []windowsProcessInfo, parentPID uint32, nameSet map[string]bool, depth, maxDepth int) bool {
	queue := []uint32{parentPID}
	visited := map[uint32]bool{parentPID: true}
	for d := 0; d <= maxDepth-depth; d++ {
		if len(queue) == 0 {
			break
		}
		var nextQueue []uint32
		for _, qpid := range queue {
			for _, process := range procs {
				if process.ppid != qpid || visited[process.pid] {
					continue
				}
				visited[process.pid] = true
				procName := strings.TrimSuffix(strings.ToLower(process.name), ".exe")
				if nameSet[procName] {
					return true
				}
				nextQueue = append(nextQueue, process.pid)
			}
		}
		queue = nextQueue
	}
	return false
}
