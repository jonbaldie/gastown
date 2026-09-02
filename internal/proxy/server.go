package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config holds configuration for the proxy server.
type Config struct {
	ListenAddr      string
	AllowedCommands []string
	// AllowedSubcommands maps each allowed command ("gt", "bd") to the set of
	// subcommands that polecats may invoke. If a command has an entry here,
	// argv[1] must appear in its list; absent argv[1] → 403.
	// If a command has NO entry, subcommands are unrestricted for that command
	// (safe for single-subcommand tools, but not intended for gt/bd).
	AllowedSubcommands map[string][]string
	// TownRoot is the path to the Gas Town root directory (e.g. ~/gt).
	// Populated from the GT_TOWN env var or ~/gt by default.
	TownRoot string
	// Logger is the structured logger to use. nil uses slog.Default().
	Logger *slog.Logger
	// ExtraSANIPs are additional IP addresses to embed in the server cert as IP SANs.
	// Merged with auto-detected local interface IPs by Start().
	ExtraSANIPs []net.IP
	// ExtraSANHosts are additional DNS names to embed in the server cert as DNS SANs.
	// Merged with the default "gt-proxy-server" DNS SAN by Start().
	ExtraSANHosts []string
	// AdminListenAddr is the address for the local admin HTTP server (no TLS).
	// The admin server exposes management endpoints for operators running on the same host.
	// If empty, no admin server is started. Recommended: "127.0.0.1:0" or "127.0.0.1:9877".
	AdminListenAddr string
	// MaxConcurrentExec caps the number of exec subprocesses that may run
	// concurrently across all clients. 0 uses the default (32).
	MaxConcurrentExec int
	// ExecRateLimit is the sustained request rate per client (identified by
	// mTLS cert CN) in requests per second. 0 uses the default (10 req/s).
	ExecRateLimit float64
	// ExecRateBurst is the maximum burst size for the per-client rate limiter.
	// 0 uses the default (20).
	ExecRateBurst int
	// ExecTimeout is the maximum duration a single exec subprocess may run.
	// 0 uses the default (60s). Use a negative value to disable the timeout.
	ExecTimeout time.Duration
}

// Server is an mTLS HTTP proxy server.
type Server struct {
	cfg           Config
	security      *serverSecurity
	allowed       map[string]bool
	allowedSubs   map[string]map[string]bool
	resolvedPaths map[string]string
	log           *slog.Logger

	// execSem is a semaphore limiting global concurrent exec subprocesses.
	execSem chan struct{}
	// execTimeout is the per-command deadline; derived from Config.ExecTimeout.
	execTimeout time.Duration
	// RateLimiters holds a *rate.Limiter per client identity (cert CN).
	RateLimiters sync.Map
	rateLimit    float64
	rateBurst    int

	lnMu    sync.Mutex
	ln      net.Listener
	adminLn net.Listener
}

type serverSecurity struct {
	ca       *CA
	denyList *DenyList
}

// New creates a new Server with the given config and CA.
// It logs a warning if AllowedCommands is empty, since no commands would be
// permitted — a safe default but almost certainly a misconfiguration.
// Any AllowedCommands entries containing "/" or "\" are rejected and removed.
// Returns an error if Config.TownRoot is empty or not an absolute path.
func New(cfg Config, ca *CA) (*Server, error) {
	if err := validateServerConfig(cfg); err != nil {
		return nil, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	allowed, resolvedPaths := resolveAllowedCommands(cfg.AllowedCommands, logger)
	if len(allowed) == 0 {
		logger.Warn("AllowedCommands is empty — all exec requests will be denied")
	}
	settings := execSettings(cfg)
	return &Server{
		cfg:           cfg,
		security:      &serverSecurity{ca: ca, denyList: NewDenyList()},
		allowed:       allowed,
		allowedSubs:   subcommandAllowlist(cfg.AllowedSubcommands),
		resolvedPaths: resolvedPaths,
		log:           logger,
		execSem:       make(chan struct{}, settings.maxConcurrent),
		execTimeout:   settings.timeout,
		rateLimit:     settings.rateLimit,
		rateBurst:     settings.rateBurst,
	}, nil
}

func validateServerConfig(cfg Config) error {
	if cfg.TownRoot == "" {
		return fmt.Errorf("Config.TownRoot must be non-empty")
	}
	if !filepath.IsAbs(cfg.TownRoot) {
		return fmt.Errorf("Config.TownRoot must be an absolute path, got %q", cfg.TownRoot)
	}
	return nil
}

func resolveAllowedCommands(commands []string, logger *slog.Logger) (map[string]bool, map[string]string) {
	allowed := make(map[string]bool, len(commands))
	for _, command := range commands {
		if strings.ContainsAny(command, `/\`) {
			logger.Error("AllowedCommands entry contains path separator — ignoring", "entry", command)
			continue
		}
		allowed[command] = true
	}
	resolved := make(map[string]string, len(allowed))
	for command := range allowed {
		path, err := exec.LookPath(command)
		if err != nil {
			logger.Error("command not found in PATH — removing from allowlist", "cmd", command)
			delete(allowed, command)
			continue
		}
		resolved[command] = path
	}
	return allowed, resolved
}

func subcommandAllowlist(configured map[string][]string) map[string]map[string]bool {
	allowed := make(map[string]map[string]bool, len(configured))
	for command, subcommands := range configured {
		allowed[command] = make(map[string]bool, len(subcommands))
		for _, subcommand := range subcommands {
			allowed[command][subcommand] = true
		}
	}
	return allowed
}

type execLimits struct {
	maxConcurrent int
	rateLimit     float64
	rateBurst     int
	timeout       time.Duration
}

func execSettings(cfg Config) execLimits {
	settings := execLimits{
		maxConcurrent: cfg.MaxConcurrentExec,
		rateLimit:     cfg.ExecRateLimit,
		rateBurst:     cfg.ExecRateBurst,
		timeout:       cfg.ExecTimeout,
	}
	if settings.maxConcurrent <= 0 {
		settings.maxConcurrent = 32
	}
	if settings.rateLimit <= 0 {
		settings.rateLimit = 10
	}
	if settings.rateBurst <= 0 {
		settings.rateBurst = 20
	}
	if settings.timeout == 0 {
		settings.timeout = 60 * time.Second
	}
	return settings
}

// Addr returns the address the server is listening on.
// Valid only after Start() has progressed past the listen call (i.e. after
// the first request is handled, or after waitForServer returns in tests).
func (s *Server) Addr() net.Addr {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// AdminAddr returns the address the admin server is listening on.
// Returns nil if no admin server was configured or if Start() has not yet bound
// the admin listener.
func (s *Server) AdminAddr() net.Addr {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	if s.adminLn == nil {
		return nil
	}
	return s.adminLn.Addr()
}

// DenyCert adds a certificate serial number to the server's deny list.
// Any active or future TLS connection presenting a cert with this serial will be
// rejected at the TLS handshake. This method is safe for concurrent use.
func (s *Server) DenyCert(serial *big.Int) {
	s.security.denyList.Deny(serial)
	s.log.Info("certificate denied", "serial", serial.Text(16))
}

// Start begins listening and serving. Blocks until ctx is canceled.
func (s *Server) Start(ctx context.Context) error {
	tlsCfg := serverTLSConfig(s.security)
	if err := installServerCertificate(s, tlsCfg); err != nil {
		return err
	}
	srv := mainHTTPServer(s, tlsCfg)
	errCh := make(chan error, 1)
	listener, err := startMainListener(s, srv, errCh)
	if err != nil {
		return err
	}
	s.lnMu.Lock()
	s.ln = listener
	s.lnMu.Unlock()
	adminSrv, err := startAdminServer(s, srv)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return shutdownProxyServers(srv, adminSrv)
	case err := <-errCh:
		return err
	}
}

func serverTLSConfig(security *serverSecurity) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(security.ca.Cert)
	dl := security.denyList
	return &tls.Config{
		ClientAuth:             tls.RequireAndVerifyClientCert,
		ClientCAs:              pool,
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(_ [][]byte, verifiedChains [][]*x509.Certificate) error {
			if len(verifiedChains) > 0 && len(verifiedChains[0]) > 0 {
				leaf := verifiedChains[0][0]
				if dl.IsDenied(leaf.SerialNumber) {
					return fmt.Errorf("certificate serial %s has been revoked", leaf.SerialNumber.Text(16))
				}
			}
			return nil
		},
	}
}

func mainHTTPServer(s *Server, tlsCfg *tls.Config) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/exec", s.handleExec)
	mux.HandleFunc("/v1/git/", s.handleGit)
	return &http.Server{
		Addr:         s.cfg.ListenAddr,
		Handler:      mux,
		TLSConfig:    tlsCfg,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
}

func installServerCertificate(s *Server, tlsCfg *tls.Config) error {
	ips := append(serverListenIPs(s.cfg.ListenAddr), s.cfg.ExtraSANIPs...)
	certPEM, keyPEM, err := s.security.ca.IssueServer("gt-proxy-server", ips, s.cfg.ExtraSANHosts, 365*24*time.Hour)
	if err != nil {
		return fmt.Errorf("issue server cert: %w", err)
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("load server cert: %w", err)
	}
	tlsCfg.Certificates = []tls.Certificate{tlsCert}
	return nil
}

func startMainListener(s *Server, srv *http.Server, errCh chan<- error) (net.Listener, error) {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	go func() {
		s.log.Info("gt-proxy-server: listening", "addr", ln.Addr(), "tls", "mTLS")
		if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	return ln, nil
}

func startAdminServer(s *Server, mainServer *http.Server) (*http.Server, error) {
	if s.cfg.AdminListenAddr == "" {
		return nil, nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/admin/deny-cert", s.handleDenyCert)
	mux.HandleFunc("/v1/admin/issue-cert", s.handleIssueCert)
	adminSrv := &http.Server{Addr: s.cfg.AdminListenAddr, Handler: mux, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	adminLn, err := net.Listen("tcp", s.cfg.AdminListenAddr)
	if err != nil {
		_ = mainServer.Shutdown(context.Background())
		return nil, fmt.Errorf("admin listen: %w", err)
	}
	s.lnMu.Lock()
	s.adminLn = adminLn
	s.lnMu.Unlock()
	s.log.Info("gt-proxy-server: admin listening", "addr", adminLn.Addr())
	go func() {
		if err := adminSrv.Serve(adminLn); err != nil && err != http.ErrServerClosed {
			s.log.Error("admin server error", "err", err)
		}
	}()
	return adminSrv, nil
}

func shutdownProxyServers(mainServer, adminServer *http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if adminServer != nil {
		_ = adminServer.Shutdown(shutdownCtx)
	}
	return mainServer.Shutdown(shutdownCtx)
}

// serverListenIPs returns the IP addresses that should be included as IP SANs in the
// server certificate. It parses the host portion of listenAddr and:
//   - If it is a specific non-loopback IP, returns [that IP, 127.0.0.1, ::1].
//   - If it is 0.0.0.0 or :: (unspecified), enumerates all non-loopback, non-link-local
//     IPv4 and IPv6 interface addresses and prepends 127.0.0.1 and ::1.
//   - Returns [127.0.0.1, ::1] at minimum on any parse or enumeration error.
func serverListenIPs(listenAddr string) []net.IP {
	loopbacks := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return loopbacks
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return loopbacks
	}
	if ip.IsUnspecified() {
		return append(loopbacks, localInterfaceIPs()...)
	}
	if ip.IsLoopback() {
		return loopbacks
	}
	return append([]net.IP{ip}, loopbacks...)
}

func localInterfaceIPs() []net.IP {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			if ip := usableInterfaceIP(address); ip != nil {
				ips = append(ips, ip)
			}
		}
	}
	return ips
}

func usableInterfaceIP(address net.Addr) net.IP {
	var ip net.IP
	switch value := address.(type) {
	case *net.IPNet:
		ip = value.IP
	case *net.IPAddr:
		ip = value.IP
	}
	if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return nil
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4
	}
	return ip
}

// issueCertRequest is the JSON body for POST /v1/admin/issue-cert.
type issueCertRequest struct {
	// Rig is the rig name (e.g. "MyRig").
	Rig string `json:"rig"`
	// Name is the polecat name (e.g. "rust").
	Name string `json:"name"`
	// TTL is the certificate validity duration (e.g. "720h"). Defaults to 720h (30 days).
	TTL string `json:"ttl"`
}

// issueCertResponse is the JSON response for POST /v1/admin/issue-cert.
type issueCertResponse struct {
	CN        string `json:"cn"`
	Cert      string `json:"cert"`
	Key       string `json:"key"`
	CA        string `json:"ca"`
	Serial    string `json:"serial"`
	ExpiresAt string `json:"expires_at"`
}

// handleIssueCert handles POST /v1/admin/issue-cert on the local admin server.
// It issues a new polecat client certificate signed by the server's CA.
func (s *Server) handleIssueCert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, ttl, err := readIssueCertRequest(w, r)
	if err != nil {
		return
	}
	response, issueErr := issuePolecatCertificate(s.security.ca, req, ttl)
	if issueErr != nil {
		http.Error(w, issueErr.message, issueErr.status)
		return
	}
	s.log.Info("cert issued via admin API", "cn", response.CN, "serial", response.Serial)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func readIssueCertRequest(w http.ResponseWriter, r *http.Request) (issueCertRequest, time.Duration, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req issueCertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return req, 0, err
	}
	if req.Rig == "" {
		http.Error(w, "bad request: rig is required", http.StatusBadRequest)
		return req, 0, fmt.Errorf("rig is required")
	}
	if req.Name == "" {
		http.Error(w, "bad request: name is required", http.StatusBadRequest)
		return req, 0, fmt.Errorf("name is required")
	}
	ttl, err := issueCertTTL(req.TTL)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
	}
	return req, ttl, err
}

func issueCertTTL(value string) (time.Duration, error) {
	if value == "" {
		return 720 * time.Hour, nil
	}
	ttl, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid ttl: %w", err)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("ttl must be positive")
	}
	return ttl, nil
}

type certificateIssueError struct {
	message string
	status  int
}

func (e *certificateIssueError) Error() string { return e.message }

func issuePolecatCertificate(ca *CA, req issueCertRequest, ttl time.Duration) (issueCertResponse, *certificateIssueError) {
	cn := "gt-" + req.Rig + "-" + req.Name
	certPEM, keyPEM, err := ca.IssuePolecat(cn, ttl)
	if err != nil {
		return issueCertResponse{}, &certificateIssueError{"failed to issue certificate: " + err.Error(), http.StatusBadRequest}
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return issueCertResponse{}, &certificateIssueError{"internal error: failed to decode issued certificate PEM", http.StatusInternalServerError}
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return issueCertResponse{}, &certificateIssueError{"internal error: " + err.Error(), http.StatusInternalServerError}
	}
	return issueCertResponse{
		CN: cn, Cert: string(certPEM), Key: string(keyPEM), CA: string(ca.CertPEM),
		Serial: leaf.SerialNumber.Text(16), ExpiresAt: leaf.NotAfter.UTC().Format(time.RFC3339),
	}, nil
}

// denyCertRequest is the JSON body for POST /v1/admin/deny-cert.
type denyCertRequest struct {
	// Serial is the certificate serial number in lowercase hexadecimal (no "0x" prefix).
	Serial string `json:"serial"`
}

// handleDenyCert handles POST /v1/admin/deny-cert on the local admin server.
// It adds the given certificate serial number to the server's deny list so that
// any subsequent TLS handshake presenting that certificate is rejected.
//
// The admin server is local-only (bound to 127.0.0.1), so no additional
// authentication is required beyond having local access to the host.
func (s *Server) handleDenyCert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KiB is ample for a serial
	var req denyCertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Serial == "" {
		http.Error(w, "bad request: serial is required", http.StatusBadRequest)
		return
	}

	serial := new(big.Int)
	if _, ok := serial.SetString(req.Serial, 16); !ok {
		http.Error(w, "bad request: serial must be lowercase hex (no 0x prefix)", http.StatusBadRequest)
		return
	}

	s.security.denyList.Deny(serial)
	s.log.Info("cert revoked via admin API", "serial", req.Serial)
	w.WriteHeader(http.StatusNoContent)
}

// minimalEnv returns a minimal environment for git and gt/bd subprocesses,
// containing only HOME and PATH to avoid leaking server credentials.
// GIT_EXEC_PATH is intentionally omitted: the git binary resolves it
// automatically from its own installation path, so passing HOME and PATH
// is sufficient for git subcommands to locate git-core helpers.
func minimalEnv() []string {
	env := []string{}
	for _, key := range []string{"HOME", "PATH"} {
		if val := os.Getenv(key); val != "" {
			env = append(env, key+"="+val)
		}
	}
	return env
}
