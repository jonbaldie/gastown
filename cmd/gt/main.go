// gt is the Gas Town CLI for managing multi-agent workspaces.
package main

import (
	"os"

	"github.com/jonbaldie/gastown/internal/cmd"
)

func main() {
	// Keep process termination at the executable boundary while allowing the
	// command result to be exercised without invoking the process exit primitive.
	exit := os.Exit
	exit(cmd.Execute())
}
