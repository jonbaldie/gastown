package cmd

import (
	"github.com/spf13/cobra"
)

func runCrewAt(cmd *cobra.Command, args []string) error {
	return runCrewAtWithRetry(cmd, args, false)
}
