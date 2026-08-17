package now

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// ResolveTownRoot returns --town, else $GT_TOWN_ROOT, else ~/gt.
func ResolveTownRoot(flag string) (string, error) {
	raw := strings.TrimSpace(flag)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("GT_TOWN_ROOT"))
	}
	if raw == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("getting home directory: %w", err)
		}
		raw = filepath.Join(home, "gt")
	}
	return expandPath(raw)
}

// SanitizeRigName converts a directory name into a valid rig name.
func SanitizeRigName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	replacer := strings.NewReplacer("-", "_", ".", "_", " ", "_", "/", "_", "\\", "_")
	name = replacer.Replace(name)
	name = strings.Trim(name, "_")
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func expandPath(path string) (string, error) {
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("getting home directory: %w", err)
		}
		if path == "~" {
			path = home
		} else if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}
	return abs, nil
}
