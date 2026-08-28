package beads

import (
	"fmt"
	"os"
)

// buildRoutingEnv builds an environment that lets bd select routes by prefix.
// It is retained as test support for the routing environment contract.
func (b *Beads) buildRoutingEnv() []string {
	if b.isolated {
		env := filterBeadsEnv(os.Environ())
		if b.serverPort > 0 {
			env = stripEnvPrefixes(env, "GT_DOLT_PORT=", "BEADS_DOLT_SERVER_PORT=", "BEADS_DOLT_PORT=", "BEADS_DOLT_AUTO_START=")
			env = append(env, fmt.Sprintf("GT_DOLT_PORT=%d", b.serverPort))
			env = append(env, fmt.Sprintf("BEADS_DOLT_SERVER_PORT=%d", b.serverPort))
			env = append(env, fmt.Sprintf("BEADS_DOLT_PORT=%d", b.serverPort))
			env = append(env, "BEADS_DOLT_AUTO_START=0")
		}
		return SuppressBDSideEffects(env)
	}
	return BuildRoutingBDEnv(os.Environ(), b.getResolvedBeadsDir())
}
