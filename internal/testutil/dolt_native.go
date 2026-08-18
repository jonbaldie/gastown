//go:build integration && !windows

package testutil

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type nativeDoltServer struct {
	cmd     *exec.Cmd
	dataDir string
	port    string
}

func lookPathDolt() string {
	p, err := exec.LookPath("dolt")
	if err != nil {
		return ""
	}
	return p
}

func envDoltPort() string {
	if p := os.Getenv("GT_DOLT_PORT"); p != "" {
		return p
	}
	return os.Getenv("BEADS_DOLT_PORT")
}

func portListening(host, port string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func freeTCPPort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	return port, err
}

func startNativeDoltSQLServer() (*nativeDoltServer, error) {
	dolt := lookPathDolt()
	if dolt == "" {
		return nil, fmt.Errorf("dolt binary not found")
	}

	dataDir, err := os.MkdirTemp("", "gt-dolt-test-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir dolt data dir: %w", err)
	}

	port, err := freeTCPPort()
	if err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, fmt.Errorf("allocate dolt port: %w", err)
	}

	logPath := filepath.Join(dataDir, "sql-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, fmt.Errorf("create dolt log: %w", err)
	}

	cmd := exec.Command(dolt, "sql-server", "--host", "127.0.0.1", "--port", port) //nolint:gosec
	cmd.Dir = dataDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = os.RemoveAll(dataDir)
		return nil, fmt.Errorf("starting dolt sql-server: %w", err)
	}
	_ = logFile.Close()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if portListening("127.0.0.1", port) {
			return &nativeDoltServer{cmd: cmd, dataDir: dataDir, port: port}, nil
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			_ = logFile.Close()
			_ = os.RemoveAll(dataDir)
			return nil, fmt.Errorf("dolt sql-server exited during startup; see %s", logPath)
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = stopNativeDoltSQLServer(&nativeDoltServer{cmd: cmd, dataDir: dataDir})
	return nil, fmt.Errorf("dolt sql-server did not accept connections on port %s within 15s; see %s", port, logPath)
}

func stopNativeDoltSQLServer(srv *nativeDoltServer) error {
	if srv == nil || srv.cmd == nil || srv.cmd.Process == nil {
		if srv != nil && srv.dataDir != "" {
			return os.RemoveAll(srv.dataDir)
		}
		return nil
	}

	pgid, pgErr := syscall.Getpgid(srv.cmd.Process.Pid)
	if pgErr == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = srv.cmd.Process.Signal(syscall.SIGTERM)
	}

	done := make(chan struct{})
	go func() {
		_ = srv.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if pgErr == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = srv.cmd.Process.Kill()
		}
		<-done
	}

	return os.RemoveAll(srv.dataDir)
}
