//go:build windows

package session

// ActivateAgentLogging is a no-op on Windows: the detached subprocess relies on
// Unix-specific Setsid / SIGTERM semantics that are not available on Windows.
func ActivateAgentLogging(_, _, _ string) error {
	return nil
}

// DeactivateAgentLogging is a no-op on Windows.
func DeactivateAgentLogging(_ string) {}
