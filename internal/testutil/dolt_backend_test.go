package testutil

import "testing"

func TestSelectSharedDoltBackend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		port      string
		reachable bool
		doltBin   string
		dockerOK  bool
		want      string
	}{
		{name: "attach live port", port: "3307", reachable: true, doltBin: "/usr/bin/dolt", dockerOK: true, want: "attached"},
		{name: "ignore dead port and use native", port: "3307", reachable: false, doltBin: "/usr/bin/dolt", dockerOK: true, want: "native"},
		{name: "native over docker", port: "", reachable: false, doltBin: "/usr/bin/dolt", dockerOK: true, want: "native"},
		{name: "docker when no dolt", port: "", reachable: false, doltBin: "", dockerOK: true, want: "docker"},
		{name: "none", port: "", reachable: false, doltBin: "", dockerOK: false, want: "none"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := selectSharedDoltBackend(tt.port, tt.reachable, tt.doltBin, tt.dockerOK)
			if got != tt.want {
				t.Fatalf("selectSharedDoltBackend(...) = %q, want %q", got, tt.want)
			}
		})
	}
}
