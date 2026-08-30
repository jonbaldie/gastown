package daemon

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/doltserver"
)

// isRemote returns true when the daemon's Dolt config points to a non-local server.
func (m *DoltServerManager) isRemote() bool {
	if m.config == nil {
		return false
	}
	host := strings.ToLower(m.config.Host)
	switch host {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return false
	}
	// Resolve hostname and check if it points to loopback.
	addrs, err := net.LookupHost(m.config.Host)
	if err != nil {
		return true
	}
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && ip.IsLoopback() {
			return false
		}
	}
	return true
}

// buildDoltSQLCmd constructs a non-interactive dolt sql command that always
// talks to the running SQL server over TCP.
//
// For local servers, this avoids embedded-mode auto-discovery, which can load
// databases relative to cmd.Dir instead of querying the live shared server.
func (m *DoltServerManager) buildDoltSQLCmd(ctx context.Context, args ...string) *exec.Cmd {
	host := m.config.Host
	if host == "" {
		host = "127.0.0.1"
	}
	user := m.config.User
	if user == "" {
		user = "root"
	}

	fullArgs := []string{
		"--host", host,
		"--port", strconv.Itoa(m.config.Port),
		"--user", user,
		"--no-tls",
		"sql",
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.CommandContext(ctx, "dolt", fullArgs...)
	setSysProcAttr(cmd)

	// Always set cmd.Dir to DataDir — even for remote connections (GH#2537).
	// Without this, dolt auto-creates .doltcfg/privileges.db in $CWD,
	// which accumulates stray privilege files that cause "multiple
	// .doltcfg directories detected" or "Access denied" errors.
	cmd.Dir = m.config.DataDir

	// Always set DOLT_CLI_PASSWORD when explicitly configured.
	// For remote checks, preserve inherited credentials if config omits a
	// password. For local checks, keep forcing the empty-password path so
	// inherited shell credentials cannot make a healthy local server look broken.
	// Strip any inherited DOLT_CLI_PASSWORD from os.Environ() first so the
	// single canonical value we append wins unambiguously — duplicate keys in
	// cmd.Env leak credentials into local checks (tests grep cmd.Env directly).
	env := filterEnvKey(os.Environ(), "DOLT_CLI_PASSWORD")
	if m.config.Password != "" {
		cmd.Env = append(env, "DOLT_CLI_PASSWORD="+m.config.Password)
	} else if m.isRemote() {
		if inherited, ok := os.LookupEnv("DOLT_CLI_PASSWORD"); ok {
			cmd.Env = append(env, "DOLT_CLI_PASSWORD="+inherited)
		} else {
			cmd.Env = append(env, "DOLT_CLI_PASSWORD=")
		}
	} else {
		cmd.Env = append(env, "DOLT_CLI_PASSWORD=")
	}

	return cmd
}

// isDoltServerOnPort checks if the configured dolt port is accepting connections.
// More reliable than ps string matching for process identity verification.
func (m *DoltServerManager) isDoltServerOnPort() bool {
	addr := net.JoinHostPort(m.config.Host, strconv.Itoa(m.config.Port))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// getDoltVersion returns the Dolt server version.
func (m *DoltServerManager) getDoltVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dolt", "version")
	setSysProcAttr(cmd)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	// Parse "dolt version X.Y.Z"
	line := strings.TrimSpace(string(output))
	parts := strings.Fields(line)
	if len(parts) >= 3 {
		return parts[2], nil
	}
	return line, nil
}

// listDatabases returns the list of databases in the Dolt server.
// Delegates to doltserver.ListDatabases which caches results and deduplicates
// concurrent queries to avoid the thundering herd problem (GH#2180).
func (m *DoltServerManager) listDatabases() ([]string, error) {
	return doltserver.ListDatabases(m.townRoot)
}
