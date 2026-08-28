package worker

import "strings"

// DecideAuthorize is fail-closed. Unknown state never allows. A deny
// always includes a reason. A missing town answer is a deny.
func DecideAuthorize(state State, req AuthorizeRequest) AuthorizeDecision {
	if !state.known() {
		return AuthorizeDecision{
			Allowed: false,
			Reason:  "unknown worker state: town will not allow a tool",
		}
	}
	if state == StateStopped || state == StateStopping {
		return AuthorizeDecision{
			Allowed: false,
			Reason:  "session is stopping: town will not allow a tool",
		}
	}
	command := commandFromInput(req.Input)
	if reason := dangerousReason(command); reason != "" {
		return AuthorizeDecision{Allowed: false, Reason: reason}
	}
	return AuthorizeDecision{Allowed: true}
}

func commandFromInput(input map[string]any) string {
	if input == nil {
		return ""
	}
	if v, ok := input["command"].(string); ok {
		return v
	}
	return ""
}

func dangerousReason(command string) string {
	if command == "" {
		return ""
	}
	lower := strings.ToLower(command)
	if reason := matchSudo(lower); reason != "" {
		return reason
	}
	if reason := matchForcePush(lower); reason != "" {
		return reason
	}
	return matchDestructiveGitCommand(lower)
}

func matchDestructiveGitCommand(command string) string {
	if isHardReset(command) {
		return "hard reset discards uncommitted changes"
	}
	if isForcedClean(command) {
		return "git clean -f deletes untracked files"
	}
	return ""
}

func isHardReset(command string) bool {
	return strings.Contains(command, "git") &&
		strings.Contains(command, "reset") &&
		strings.Contains(command, "--hard")
}

func isForcedClean(command string) bool {
	return strings.Contains(command, "git") &&
		strings.Contains(command, "clean") &&
		strings.Contains(command, "-f")
}

func matchSudo(command string) string {
	for _, f := range strings.Fields(command) {
		if f == "sudo" {
			return "agents must never use sudo"
		}
	}
	return ""
}

func matchForcePush(command string) string {
	return forcePushReason(strings.Fields(command))
}

func forcePushReason(fields []string) string {
	for i := range fields {
		if !isGitPushToken(fields, i) {
			continue
		}
		return forcePushFlagReason(fields[i+1:])
	}
	return ""
}

func isGitPushToken(fields []string, index int) bool {
	return index > 0 && fields[index] == "push" && fields[index-1] == "git"
}

func forcePushFlagReason(fields []string) string {
	for _, field := range fields {
		if isAllowedForcePushFlag(field) {
			continue
		}
		if isForcePushFlag(field) {
			return "force push rewrites remote history"
		}
	}
	return ""
}

func isAllowedForcePushFlag(field string) bool {
	return field == "--force-with-lease" || field == "--force-if-includes"
}

func isForcePushFlag(field string) bool {
	return field == "--force" || field == "-f"
}
