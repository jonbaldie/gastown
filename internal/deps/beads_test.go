package deps

import (
	"runtime/debug"
	"testing"
)

func TestParseBeadsVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"bd version 0.55.4 (dev: main@3e1378e122c6)", "0.55.4"},
		{"bd version 0.55.4", "0.55.4"},
		{"bd version 1.2.3", "1.2.3"},
		{"bd version 10.20.30 (release)", "10.20.30"},
		{"some other output", ""},
		{"", ""},
	}

	for _, tt := range tests {
		result := parseBeadsVersion(tt.input)
		if result != tt.expected {
			t.Errorf("parseBeadsVersion(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestBeadsVersionFromBuildInfo(t *testing.T) {
	tests := []struct {
		name     string
		info     *debug.BuildInfo
		expected string
	}{
		{
			name:     "tagged fork binary",
			info:     &debug.BuildInfo{Main: debug.Module{Path: beadsMainPackage, Version: "v1.3.0"}},
			expected: "1.3.0",
		},
		{
			name:     "pseudo-version fork binary",
			info:     &debug.BuildInfo{Main: debug.Module{Path: beadsMainPackage, Version: "v1.2.2-0.20260817230026-3e7110daa8e3"}},
			expected: "1.2.2",
		},
		{
			name: "development binary falls back",
			info: &debug.BuildInfo{Main: debug.Module{Path: beadsMainPackage, Version: "(devel)"}},
		},
		{
			name: "different binary falls back",
			info: &debug.BuildInfo{Main: debug.Module{Path: "example.com/not-bd", Version: "v1.3.0"}},
		},
		{name: "missing build info falls back"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := beadsVersionFromBuildInfo(tt.info); got != tt.expected {
				t.Fatalf("beadsVersionFromBuildInfo() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b     string
		expected int
	}{
		{"0.55.4", "0.55.4", 0},
		{"0.55.4", "0.54.0", 1},
		{"0.54.0", "0.55.4", -1},
		{"1.0.0", "0.99.99", 1},
		{"0.55.5", "0.55.4", 1},
		{"0.55.4", "0.55.5", -1},
	}

	for _, tt := range tests {
		result := CompareVersions(tt.a, tt.b)
		if result != tt.expected {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, result, tt.expected)
		}
	}
}

func TestCheckBeads(t *testing.T) {
	// This test depends on whether bd is installed in the test environment
	status, version := CheckBeads()

	// We expect bd to be installed in dev environment
	if status == BeadsNotFound {
		t.Skip("bd not installed, skipping integration test")
	}

	if status == BeadsOK && version == "" {
		t.Error("CheckBeads returned BeadsOK but empty version")
	}

	t.Logf("CheckBeads: status=%d, version=%s", status, version)
}
