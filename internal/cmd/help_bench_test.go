package cmd

import (
	"testing"
)

// benchHelpText is a realistic full-tree help render (Long description +
// UsageString) captured from the real rootCmd, exercising the group
// headers, section headers, command lines, and flag lines that
// colorizeHelpOutput's regexes match against.
func benchHelpText(b *testing.B) string {
	b.Helper()
	rootCmd.InitDefaultHelpFlag()
	usage := rootCmd.UsageString()
	long := rootCmd.Long
	if long == "" {
		long = rootCmd.Short
	}
	return long + "\n\n" + usage
}

func BenchmarkColorizeHelpOutput(b *testing.B) {
	text := benchHelpText(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		colorizeHelpOutput(text)
	}
}
