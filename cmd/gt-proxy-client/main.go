// gt-proxy-client is the pass-through binary installed in containers as both `gt` and `bd`.
// When GT_PROXY_URL, GT_PROXY_CERT, GT_PROXY_KEY, and GT_PROXY_CA are all set, it forwards
// os.Args[1:] to the proxy server over mTLS and proxies the response.
// Otherwise it execs the real binary at /usr/local/bin/gt.real (or the path in GT_REAL_BIN).
package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type execRequest struct {
	Argv []string `json:"argv"`
}

type execResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// processExit is the executable's replaceable process-termination boundary.
var processExit = os.Exit

func main() {
	if err := run(); err != nil {
		code := 1
		var exitErr *exitCodeError
		if errors.As(err, &exitErr) {
			code = exitErr.code
		} else {
			fmt.Fprintf(os.Stderr, "gt-proxy-client: %v\n", err)
		}
		processExit(code)
	}
}

type exitCodeError struct {
	code int
}

func (e *exitCodeError) Error() string { return "" }

func run() error {
	// Required environment variables:
	//   GT_PROXY_URL  — proxy base URL (e.g. https://172.17.0.1:9876)
	//   GT_PROXY_CERT — path to PEM client cert (issued by proxy CA)
	//   GT_PROXY_KEY  — path to PEM client private key
	//   GT_PROXY_CA   — path to PEM proxy CA cert (used to verify server cert)
	// Optional:
	//   GT_REAL_BIN   — fallback binary path (default /usr/local/bin/gt.real)
	proxyURL := os.Getenv("GT_PROXY_URL")
	certFile := os.Getenv("GT_PROXY_CERT")
	keyFile := os.Getenv("GT_PROXY_KEY")
	// GT_PROXY_CA is the CA cert for the proxy's server TLS cert.
	// This is the same CA cert as GIT_SSL_CAINFO (which git uses to trust the proxy),
	// but passed separately so the Go HTTP client can also trust the proxy server cert.
	caFile := os.Getenv("GT_PROXY_CA")

	if proxyURL == "" || certFile == "" || keyFile == "" || caFile == "" {
		// One or more proxy env vars unset — not in sandboxed mode, exec the real binary silently.
		return execReal()
	}

	httpClient, err := newProxyClient(certFile, keyFile, caFile)
	if err != nil {
		return err
	}

	// Determine argv: prepend the binary name so the server knows which tool we are.
	argv := os.Args // os.Args[0] is the binary path; the server needs the tool name as argv[0].
	// Replace argv[0] with the tool name (gt or bd) based on the binary name.
	toolName := toolNameFromArg0(os.Args[0])
	argv = append([]string{toolName}, os.Args[1:]...)

	result, err := executeRemote(httpClient, proxyURL, argv)
	if err != nil {
		return err
	}
	return writeExecResult(result)
}

func writeExecResult(result execResponse) error {
	if result.Stdout != "" {
		_, _ = fmt.Fprint(os.Stdout, result.Stdout)
	}
	if result.Stderr != "" {
		_, _ = fmt.Fprint(os.Stderr, result.Stderr)
	}
	if result.ExitCode != 0 {
		return &exitCodeError{code: result.ExitCode}
	}
	return nil
}

func newProxyClient(certFile, keyFile, caFile string) (*http.Client, error) {
	clientCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}

	caPEM, err := os.ReadFile(caFile) //nolint:gosec // caFile is from trusted env var GT_PROXY_CA
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid CA PEM")
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		MinVersion:   tls.VersionTLS12,
		RootCAs:      pool,
	}
	return &http.Client{
		Timeout:   5 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

func executeRemote(client *http.Client, proxyURL string, argv []string) (execResponse, error) {
	body, err := json.Marshal(execRequest{Argv: argv})
	if err != nil {
		return execResponse{}, fmt.Errorf("encode request: %w", err)
	}
	resp, err := client.Post(proxyURL+"/v1/exec", "application/json", bytes.NewReader(body)) //nolint:gosec // proxyURL is from trusted env var GT_PROXY_URL
	if err != nil {
		return execResponse{}, fmt.Errorf("proxy request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close on response body
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return execResponse{}, fmt.Errorf("server error %d: %s", resp.StatusCode, msg)
	}
	var result execResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return execResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}

// toolNameFromArg0 extracts "gt" or "bd" from the argv[0] binary path.
func toolNameFromArg0(arg0 string) string {
	return filepath.Base(arg0)
}

// execReal replaces the current process with the real binary.
func execReal() error {
	realBin := os.Getenv("GT_REAL_BIN")
	if realBin == "" {
		realBin = "/usr/local/bin/gt.real"
	}
	if err := syscall.Exec(realBin, os.Args, os.Environ()); err != nil { //nolint:gosec // realBin is from GT_REAL_BIN or hardcoded default
		return fmt.Errorf("exec %s: %w", realBin, err)
	}
	return nil
}
