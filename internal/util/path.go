package util

import (
	"os"
	"strings"
)

// ExpandHome expands a leading ~/ to the user's home directory.
// Returns the path unchanged if it doesn't start with ~/ or if
// the home directory cannot be determined.
func ExpandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return path
	}
	return home + path[1:]
}
