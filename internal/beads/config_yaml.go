package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnsureConfigYAML ensures config.yaml has both prefix keys set for the given
// beads namespace. Existing non-prefix settings are preserved.
func EnsureConfigYAML(beadsDir, prefix string) error {
	return ensureConfigYAML(beadsDir, prefix, false)
}

// EnsureConfigYAMLValue ensures config.yaml contains key: value while preserving
// existing unrelated settings. It is used for install-time settings that must be
// present before older bd binaries can safely open the Dolt schema.
func EnsureConfigYAMLValue(beadsDir, key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("empty config key")
	}
	wantLine := key + ": " + value
	configPath := filepath.Join(beadsDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return os.WriteFile(configPath, []byte(wantLine+"\n"), 0644)
	}
	if err != nil {
		return err
	}

	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	newContent := updatedConfigYAMLValue(content, key, value)
	if newContent == content {
		return nil
	}
	return os.WriteFile(configPath, []byte(newContent), 0644)
}

func updatedConfigYAMLValue(content, key, value string) string {
	wantLine := key + ": " + value
	lines := strings.Split(content, "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), key+":") {
			lines[i] = wantLine
			found = true
		}
	}
	if !found {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, wantLine)
	}

	newContent := strings.Join(lines, "\n")
	if !strings.HasSuffix(newContent, "\n") {
		newContent += "\n"
	}
	return newContent
}

// EnsureConfigYAMLIfMissing creates config.yaml with the required defaults when
// it is missing. Existing files are left untouched.
func EnsureConfigYAMLIfMissing(beadsDir, prefix string) error {
	return ensureConfigYAML(beadsDir, prefix, true)
}

// EnsureConfigYAMLFromMetadataIfMissing creates config.yaml when missing using
// metadata-derived defaults for prefix when available.
func EnsureConfigYAMLFromMetadataIfMissing(beadsDir, fallbackPrefix string) error {
	prefix := ConfigDefaultsFromMetadata(beadsDir, fallbackPrefix)
	return ensureConfigYAML(beadsDir, prefix, true)
}

// ConfigDefaultsFromMetadata derives config.yaml defaults from metadata.json.
// Falls back to fallbackPrefix when fields are absent.
func ConfigDefaultsFromMetadata(beadsDir, fallbackPrefix string) string {
	prefix := strings.TrimSpace(strings.TrimSuffix(fallbackPrefix, "-"))
	if prefix == "" {
		prefix = fallbackPrefix
	}

	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		return prefix
	}

	var meta map[string]interface{}
	if err := json.Unmarshal(data, &meta); err != nil {
		return prefix
	}

	if derived := firstString(meta, "issue_prefix", "issue-prefix", "prefix"); derived != "" {
		prefix = strings.TrimSpace(strings.TrimSuffix(derived, "-"))
	} else if doltDB := firstString(meta, "dolt_database"); doltDB != "" {
		prefix = normalizeDoltDatabasePrefix(doltDB)
	}

	return prefix
}

func firstString(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s != "" {
			return s
		}
	}
	return ""
}

func normalizeDoltDatabasePrefix(dbName string) string {
	name := strings.TrimSpace(strings.TrimSuffix(dbName, "-"))
	if strings.HasPrefix(name, "beads_") {
		trimmed := strings.TrimPrefix(name, "beads_")
		if trimmed != "" {
			return trimmed
		}
	}
	return name
}

// ConfigYAMLDisablesAutoExport reports whether config.yaml content explicitly
// disables bd's post-run auto-export. Comments do not count as configuration.
func ConfigYAMLDisablesAutoExport(content string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "export.auto:") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "export.auto:"))
			value = strings.Trim(value, `"'`)
			return strings.EqualFold(value, "false")
		}
	}
	return false
}

func ensureConfigYAML(beadsDir, prefix string, onlyIfMissing bool) error {
	configPath := filepath.Join(beadsDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return os.WriteFile(configPath, []byte(configYAMLDefaultsContent(prefix)), 0644)
	}
	if err != nil {
		return err
	}
	if onlyIfMissing {
		return nil
	}

	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	newContent := configYAMLDefaultsUpdated(content, prefix)
	if newContent == content {
		return nil
	}

	return os.WriteFile(configPath, []byte(newContent), 0644)
}

type configYAMLValue struct{ key, value string }

func configYAMLDefaults(prefix string) []configYAMLValue {
	return []configYAMLValue{
		{"prefix", prefix},
		{"issue-prefix", prefix},
		{"dolt.idle-timeout", "\"0\""},
		{"export.auto", "\"false\""},
	}
}

func configYAMLDefaultsContent(prefix string) string {
	var lines []string
	for _, setting := range configYAMLDefaults(prefix) {
		lines = append(lines, setting.key+": "+setting.value)
	}
	return strings.Join(lines, "\n") + "\n"
}

func configYAMLDefaultsUpdated(content, prefix string) string {
	for _, setting := range configYAMLDefaults(prefix) {
		content = updatedConfigYAMLValue(content, setting.key, setting.value)
	}
	return content
}
