// Package connection provides an abstraction for local and remote operations.
// This allows Gas Town to manage rigs on remote machines via SSH using
// the same interface as local operations.
package connection

import (
	"io/fs"
	"time"
)

// FileConnection abstracts filesystem operations for local and remote contexts.
type FileConnection interface {
	// ReadFile reads the named file and returns its contents.
	ReadFile(_ string) ([]byte, error)

	// WriteFile writes data to the named file with the given permissions.
	WriteFile(_ string, _ []byte, _ fs.FileMode) error

	// MkdirAll creates a directory and all parent directories.
	MkdirAll(_ string, _ fs.FileMode) error

	// Remove removes the named file or empty directory.
	Remove(_ string) error

	// RemoveAll removes the named file or directory and any children.
	RemoveAll(_ string) error

	// Stat returns file info for the named file.
	Stat(_ string) (FileInfo, error)

	// Glob returns the names of all files matching the pattern.
	Glob(_ string) ([]string, error)

	// Exists returns true if the path exists.
	Exists(_ string) (bool, error)
}

// ExecConnection abstracts command execution for local and remote contexts.
type ExecConnection interface {
	// Exec runs a command and returns its combined output.
	Exec(_ string, _ ...string) ([]byte, error)

	// ExecDir runs a command in the specified directory.
	ExecDir(_, _ string, _ ...string) ([]byte, error)

	// ExecEnv runs a command with additional environment variables.
	ExecEnv(_ map[string]string, _ string, _ ...string) ([]byte, error)
}

// TmuxConnection abstracts tmux session control for local and remote contexts.
type TmuxConnection interface {
	// TmuxNewSession creates a new tmux session with the given name.
	TmuxNewSession(_, _ string) error

	// TmuxKillSession terminates the named tmux session.
	// Uses KillSessionWithProcesses internally to ensure all descendant processes are killed.
	TmuxKillSession(_ string) error

	// TmuxSendKeys sends keys to the named tmux session.
	TmuxSendKeys(_, _ string) error

	// TmuxCapturePane captures the last N lines from a tmux pane.
	TmuxCapturePane(_ string, _ int) (string, error)

	// TmuxHasSession returns true if the named tmux session exists.
	TmuxHasSession(_ string) (bool, error)

	// TmuxListSessions returns a list of all tmux session names.
	TmuxListSessions() ([]string, error)
}

// Connection abstracts file operations, command execution, and tmux management
// for both local and remote (SSH) execution contexts.
type Connection interface {
	// Name returns a human-readable name for this connection.
	Name() string

	// IsLocal returns true if this is a local connection.
	IsLocal() bool

	FileConnection
	ExecConnection
	TmuxConnection
}

// FileInfo abstracts fs.FileInfo for use over remote connections.
// This is needed because fs.FileInfo contains methods that can't be
// easily serialized over SSH.
type FileInfo interface {
	// Name returns the base name of the file.
	Name() string

	// Size returns the length in bytes.
	Size() int64

	// Mode returns the file mode bits.
	Mode() fs.FileMode

	// ModTime returns the modification time.
	ModTime() time.Time

	// IsDir returns true if this is a directory.
	IsDir() bool
}

// BasicFileInfo is a simple implementation of FileInfo.
type BasicFileInfo struct {
	FileName    string      `json:"name"`
	FileSize    int64       `json:"size"`
	FileMode    fs.FileMode `json:"mode"`
	FileModTime time.Time   `json:"mod_time"`
	FileIsDir   bool        `json:"is_dir"`
}

// Name implements FileInfo.
func (f BasicFileInfo) Name() string { return f.FileName }

// Size implements FileInfo.
func (f BasicFileInfo) Size() int64 { return f.FileSize }

// Mode implements FileInfo.
func (f BasicFileInfo) Mode() fs.FileMode { return f.FileMode }

// ModTime implements FileInfo.
func (f BasicFileInfo) ModTime() time.Time { return f.FileModTime }

// IsDir implements FileInfo.
func (f BasicFileInfo) IsDir() bool { return f.FileIsDir }

// FromOSFileInfo creates a BasicFileInfo from an os.FileInfo.
func FromOSFileInfo(fi fs.FileInfo) BasicFileInfo {
	return BasicFileInfo{
		FileName:    fi.Name(),
		FileSize:    fi.Size(),
		FileMode:    fi.Mode(),
		FileModTime: fi.ModTime(),
		FileIsDir:   fi.IsDir(),
	}
}

// Error types for connection operations.
type (
	// ConnectionError indicates a connection-level failure.
	ConnectionError struct {
		Op      string // Operation that failed (e.g., "connect", "exec")
		Machine string // Machine name or address
		Err     error  // Underlying error
	}

	// NotFoundError indicates a file or resource was not found.
	NotFoundError struct {
		Path string
	}

	// PermissionError indicates an access permission failure.
	PermissionError struct {
		Path string
		Op   string
	}
)

func (e *ConnectionError) Error() string {
	return "connection " + e.Op + " on " + e.Machine + ": " + e.Err.Error()
}

func (e *ConnectionError) Unwrap() error {
	return e.Err
}

func (e *NotFoundError) Error() string {
	return "not found: " + e.Path
}

func (e *PermissionError) Error() string {
	return "permission denied: " + e.Op + " " + e.Path
}
