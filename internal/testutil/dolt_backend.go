package testutil

// selectSharedDoltBackend chooses how TestMain should provide a Dolt SQL server.
//
// Preference order:
//  1. attached — GT_DOLT_PORT / BEADS_DOLT_PORT already points at a live server
//  2. native — a dolt binary is on PATH (typical CI and this cloud image)
//  3. docker — testcontainers fallback
//  4. none
//
// Native beats Docker because docker pull + Ryuk + container boot dwarfs
// `dolt sql-server` on localhost, while tests still speak the same SQL protocol.
func selectSharedDoltBackend(existingPort string, portReachable bool, doltBin string, dockerOK bool) string {
	if existingPort != "" && portReachable {
		return "attached"
	}
	if doltBin != "" {
		return "native"
	}
	if dockerOK {
		return "docker"
	}
	return "none"
}
