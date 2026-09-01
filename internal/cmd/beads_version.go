// Package cmd provides CLI commands for the gt tool.
package cmd

import (
	"fmt"
	"sync"

	"github.com/jonbaldie/gastown/internal/deps"
)

var beadsVersionCheck = sync.OnceValue(func() error {
	status, version := deps.CheckBeads()
	switch status {
	case deps.BeadsOK:
		return nil
	case deps.BeadsUnknown:
		return fmt.Errorf("beads (bd) version could not be determined\n\nTry reinstalling: go install %s", deps.BeadsInstallPath)
	case deps.BeadsNotFound:
		return fmt.Errorf("beads (bd) not found in PATH\n\nInstall with: go install %s", deps.BeadsInstallPath)
	case deps.BeadsTooOld:
		return fmt.Errorf("beads %s is required, but %s is installed\n\nUpgrade: go install %s",
			deps.MinBeadsVersion, version, deps.BeadsInstallPath)
	default:
		return nil
	}
})

// CheckBeadsVersion verifies that the installed beads version meets the minimum requirement.
// Returns nil if the version is sufficient, or an error with details if not.
// The check is performed only once per process execution.
func CheckBeadsVersion() error {
	return beadsVersionCheck()
}
