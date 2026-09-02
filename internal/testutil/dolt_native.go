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

	data, err := prepareNativeDoltData()
	if err != nil {
		return nil, err
	}

	// --name/--email keep init independent of ~/.dolt. CI installs Dolt but
	// never sets user.name/user.email, and writing those globally from a
	// temp dir can create .dolt before init.
	initOut, err := initializeNativeDolt(dolt, data.dataDir)
	if err != nil {
		_ = data.logFile.Close()
		_ = os.RemoveAll(data.dataDir)
		return nil, fmt.Errorf("dolt init in %s: %w\n%s", data.dataDir, err, initOut)
	}

	cmd, err := launchNativeDolt(dolt, data.dataDir, data.port, data.logFile)
	if err != nil {
		_ = data.logFile.Close()
		_ = os.RemoveAll(data.dataDir)
		return nil, fmt.Errorf("starting dolt sql-server: %w", err)
	}
	_ = data.logFile.Close()

	srv := &nativeDoltServer{cmd: cmd, dataDir: data.dataDir, port: data.port}
	if err := waitForNativeDolt(srv, data.logPath); err != nil {
		_ = stopNativeDoltSQLServer(srv)
		return nil, err
	}
	return srv, nil
}

type nativeDoltData struct {
	dataDir string
	port    string
	logPath string
	logFile *os.File
}

func prepareNativeDoltData() (nativeDoltData, error) {
	dataDir, err := os.MkdirTemp("", "gt-dolt-test-*")
	if err != nil {
		return nativeDoltData{}, fmt.Errorf("mkdir dolt data dir: %w", err)
	}

	port, err := freeTCPPort()
	if err != nil {
		_ = os.RemoveAll(dataDir)
		return nativeDoltData{}, fmt.Errorf("allocate dolt port: %w", err)
	}

	logPath := filepath.Join(dataDir, "sql-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		_ = os.RemoveAll(dataDir)
		return nativeDoltData{}, fmt.Errorf("create dolt log: %w", err)
	}
	return nativeDoltData{
		dataDir: dataDir,
		port:    port,
		logPath: logPath,
		logFile: logFile,
	}, nil
}

func initializeNativeDolt(dolt, dataDir string) ([]byte, error) {
	initCmd := exec.Command(dolt, "init", "--name", "gt-test", "--email", "gt-test@localhost") //nolint:gosec
	initCmd.Dir = dataDir
	return initCmd.CombinedOutput()
}

func launchNativeDolt(dolt, dataDir, port string, logFile *os.File) (*exec.Cmd, error) {
	cmd := exec.Command(dolt, "sql-server", "--host", "127.0.0.1", "--port", port) //nolint:gosec
	cmd.Dir = dataDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func waitForNativeDolt(srv *nativeDoltServer, logPath string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if portListening("127.0.0.1", srv.port) {
			return nil
		}
		if srv.cmd.ProcessState != nil && srv.cmd.ProcessState.Exited() {
			return fmt.Errorf("dolt sql-server exited during startup; see %s", logPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("dolt sql-server did not accept connections on port %s within 15s; see %s", srv.port, logPath)
}

func stopNativeDoltSQLServer(srv *nativeDoltServer) error {
	if srv == nil {
		return nil
	}
	if srv.cmd == nil || srv.cmd.Process == nil {
		return os.RemoveAll(srv.dataDir)
	}

	pgid, pgErr := syscall.Getpgid(srv.cmd.Process.Pid)
	signalNativeDolt(srv.cmd, pgid, pgErr)
	waitNativeDolt(srv.cmd, pgid, pgErr)
	return os.RemoveAll(srv.dataDir)
}

func signalNativeDolt(cmd *exec.Cmd, pgid int, pgErr error) {
	if pgErr == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
}

func waitNativeDolt(cmd *exec.Cmd, pgid int, pgErr error) {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if pgErr == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = cmd.Process.Kill()
		}
		<-done
	}
}
