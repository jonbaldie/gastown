package cmd

import (
	"time"

	"github.com/spf13/cobra"
)

func commandStringFlag(cmd *cobra.Command, name string) string {
	if cmd == nil {
		return ""
	}
	value, _ := cmd.Flags().GetString(name)
	return value
}

func commandStringAliasFlag(cmd *cobra.Command, primary, alias string) string {
	if cmd == nil {
		return ""
	}
	if cmd.Flags().Changed(alias) {
		return commandStringFlag(cmd, alias)
	}
	return commandStringFlag(cmd, primary)
}

func commandBoolFlag(cmd *cobra.Command, name string) bool {
	if cmd == nil {
		return false
	}
	value, _ := cmd.Flags().GetBool(name)
	return value
}

func commandIntFlag(cmd *cobra.Command, name string) int {
	if cmd == nil {
		return 0
	}
	value, _ := cmd.Flags().GetInt(name)
	return value
}

func commandDurationFlag(cmd *cobra.Command, name string) time.Duration {
	if cmd == nil {
		return 0
	}
	value, _ := cmd.Flags().GetDuration(name)
	return value
}

func commandFloat64Flag(cmd *cobra.Command, name string) float64 {
	if cmd == nil {
		return 0
	}
	value, _ := cmd.Flags().GetFloat64(name)
	return value
}

func commandStringArrayFlag(cmd *cobra.Command, name string) []string {
	if cmd == nil {
		return nil
	}
	value, _ := cmd.Flags().GetStringArray(name)
	return value
}
