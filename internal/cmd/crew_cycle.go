package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// cycleCrewSession switches to the next or previous crew session in the same rig.
// direction: 1 for next, -1 for previous
// sessionOverride: if non-empty, use this instead of detecting current session
func cycleCrewSession(direction int, sessionOverride, clientOverride string) error {
	currentSession, err := resolveCurrentSession(sessionOverride)
	if err != nil {
		return fmt.Errorf("not in a tmux session: %w", err)
	}
	if currentSession == "" {
		return fmt.Errorf("not in a tmux session")
	}

	parsed, ok := parseCrewSessionName(currentSession)
	if !ok {
		return nil
	}

	sessions, err := findRigCrewSessions(parsed.prefix)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	return cycleInGroup(direction, currentSession, sessions, clientOverride)
}

func runCrewNext(cmd *cobra.Command, _ []string) error {
	session, err := cmd.Flags().GetString("session")
	if err != nil {
		return err
	}
	return cycleCrewSession(1, session, "")
}

func runCrewPrev(cmd *cobra.Command, _ []string) error {
	session, err := cmd.Flags().GetString("session")
	if err != nil {
		return err
	}
	return cycleCrewSession(-1, session, "")
}
