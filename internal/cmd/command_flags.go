package cmd

import "github.com/spf13/cobra"

func commandStringFlag(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}

func commandStringAliasFlag(cmd *cobra.Command, primary, alias string) string {
	if cmd.Flags().Changed(alias) {
		return commandStringFlag(cmd, alias)
	}
	return commandStringFlag(cmd, primary)
}

func commandBoolFlag(cmd *cobra.Command, name string) bool {
	value, _ := cmd.Flags().GetBool(name)
	return value
}

func commandIntFlag(cmd *cobra.Command, name string) int {
	value, _ := cmd.Flags().GetInt(name)
	return value
}

func commandStringArrayFlag(cmd *cobra.Command, name string) []string {
	value, _ := cmd.Flags().GetStringArray(name)
	return value
}
