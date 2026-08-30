// gt-proxy-server is the mTLS proxy server for sandboxed polecat execution.
// It runs on the host and allows containers to call gt/bd and access git repos
// via authenticated, authorized HTTP endpoints.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jonbaldie/gastown/internal/proxy"
	"github.com/jonbaldie/gastown/internal/util"
)

// defaultAllowedSubcmds lists the safe subcommands for gt and bd.
// Dangerous subcommands (e.g. gt polecat, gt rig, gt admin, gt nuke) are excluded.
const defaultAllowedSubcmds = "" +
	"gt:prime,hook,done,mail,nudge,mol,status,handoff,version,convoy,sling;" +
	"bd:create,update,close,show,list,ready,dep,export,prime,stats,blocked,doctor"

// processExit is the executable's replaceable process-termination boundary.
var processExit = os.Exit

func main() {
	if err := run(); err != nil {
		slog.Error("proxy server error", "err", err)
		processExit(1)
	}
}

type proxyServerOptions struct {
	configFile     string
	listen         string
	adminListen    string
	caDir          string
	allowedCmds    string
	allowedSubcmds string
	townRoot       string
	explicitFlags  map[string]bool
}

func parseProxyServerOptions() proxyServerOptions {
	var (
		configFile     = flag.String("config", "", "path to config file (default: ~/gt/.runtime/proxy/config.json)")
		listen         = flag.String("listen", "0.0.0.0:9876", "address to listen on")
		adminListen    = flag.String("admin-listen", "127.0.0.1:9877", "address for local admin HTTP server (use empty string to disable)")
		caDir          = flag.String("ca-dir", "", "directory for CA cert/key (default: ~/gt/.runtime/ca)")
		allowedCmds    = flag.String("allowed-cmds", "gt,bd", "comma-separated list of allowed commands")
		allowedSubcmds = flag.String("allowed-subcmds", discoverAllowedSubcmds(),
			`semicolon-separated list of "cmd:sub1,sub2,..." subcommand allowlists`)
		townRoot = flag.String("town-root", "", "Gas Town root directory (default: $GT_TOWN or ~/gt)")
	)
	flag.Parse()
	explicitFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { explicitFlags[f.Name] = true })
	return proxyServerOptions{
		configFile:     *configFile,
		listen:         *listen,
		adminListen:    *adminListen,
		caDir:          *caDir,
		allowedCmds:    *allowedCmds,
		allowedSubcmds: *allowedSubcmds,
		townRoot:       *townRoot,
		explicitFlags:  explicitFlags,
	}
}

func run() error {
	opts := parseProxyServerOptions()

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	// Determine config file path and load it.
	cfgPath := proxyServerConfigPath(opts.configFile, home)
	fileCfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}

	mergeProxyServerConfig(&opts, fileCfg)
	applyProxyServerDefaults(&opts, home)
	ca, err := proxy.LoadOrGenerateCA(opts.caDir)
	if err != nil {
		return err
	}
	slog.Info("CA loaded", "dir", opts.caDir)
	srv, err := proxy.New(proxyServerConfig(opts, fileCfg), ca)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return srv.Start(ctx)
}

func proxyServerConfigPath(configFile, home string) string {
	if configFile != "" {
		return configFile
	}
	return filepath.Join(home, "gt", ".runtime", "proxy", "config.json")
}

func mergeProxyServerConfig(o *proxyServerOptions, fileCfg ProxyConfig) {
	mergeProxyServerNetwork(o, fileCfg)
	mergeProxyServerPaths(o, fileCfg)
	mergeProxyServerAllowlists(o, fileCfg)
}

func mergeProxyServerNetwork(o *proxyServerOptions, fileCfg ProxyConfig) {
	if !o.explicitFlags["listen"] && fileCfg.ListenAddr != "" {
		o.listen = fileCfg.ListenAddr
	}
	if !o.explicitFlags["admin-listen"] && fileCfg.AdminListenAddr != "" {
		o.adminListen = fileCfg.AdminListenAddr
	}
}

func mergeProxyServerPaths(o *proxyServerOptions, fileCfg ProxyConfig) {
	if !o.explicitFlags["ca-dir"] && fileCfg.CADir != "" {
		o.caDir = fileCfg.CADir
	}
	if !o.explicitFlags["town-root"] && fileCfg.TownRoot != "" {
		o.townRoot = fileCfg.TownRoot
	}
}

func mergeProxyServerAllowlists(o *proxyServerOptions, fileCfg ProxyConfig) {
	if !o.explicitFlags["allowed-cmds"] && len(fileCfg.AllowedCommands) > 0 {
		o.allowedCmds = strings.Join(fileCfg.AllowedCommands, ",")
	}
	if !o.explicitFlags["allowed-subcmds"] && len(fileCfg.AllowedSubcommands) > 0 {
		o.allowedSubcmds = buildAllowedSubcmds(fileCfg.AllowedSubcommands)
	}
}

func applyProxyServerDefaults(o *proxyServerOptions, home string) {
	if o.caDir == "" {
		o.caDir = filepath.Join(home, "gt", ".runtime", "ca")
	}
	if o.townRoot != "" {
		return
	}
	o.townRoot = os.Getenv("GT_TOWN")
	if o.townRoot == "" {
		o.townRoot = filepath.Join(home, "gt")
	}
}

func proxyServerConfig(o proxyServerOptions, fileCfg ProxyConfig) proxy.Config {
	return proxy.Config{
		ListenAddr:         o.listen,
		AdminListenAddr:    o.adminListen,
		AllowedCommands:    splitAllowedCommands(o.allowedCmds),
		AllowedSubcommands: parseAllowedSubcmds(o.allowedSubcmds),
		TownRoot:           o.townRoot,
		ExtraSANIPs:        parseExtraSANIPs(fileCfg.ExtraSANIPs),
		ExtraSANHosts:      parseExtraSANHosts(fileCfg.ExtraSANHosts),
	}
}

func splitAllowedCommands(value string) []string {
	cmds := strings.Split(value, ",")
	for i := range cmds {
		cmds[i] = strings.TrimSpace(cmds[i])
	}
	return cmds
}

func parseExtraSANIPs(values []string) []net.IP {
	var ips []net.IP
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		ip := net.ParseIP(value)
		if ip == nil {
			slog.Warn("extra_san_ips: invalid IP address — skipping", "entry", value)
			continue
		}
		ips = append(ips, ip)
	}
	return ips
}

func parseExtraSANHosts(values []string) []string {
	var hosts []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			hosts = append(hosts, value)
		}
	}
	return hosts
}

// discoverAllowedSubcmds calls "gt proxy-subcmds" to auto-discover the allowed
// subcommand list. Falls back to defaultAllowedSubcmds if the command is
// unavailable or returns empty output.
func discoverAllowedSubcmds() string {
	cmd := exec.Command("gt", "proxy-subcmds")
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		slog.Debug("gt proxy-subcmds discovery failed, using built-in default", "err", err)
		return defaultAllowedSubcmds
	}
	result := strings.TrimSpace(string(out))
	if result == "" {
		return defaultAllowedSubcmds
	}
	return result
}

// buildAllowedSubcmds serializes a map[string][]string back into the semicolon-separated
// "cmd:sub1,sub2,..." format expected by parseAllowedSubcmds.
func buildAllowedSubcmds(m map[string][]string) string {
	parts := make([]string, 0, len(m))
	for cmd, subs := range m {
		parts = append(parts, cmd+":"+strings.Join(subs, ","))
	}
	return strings.Join(parts, ";")
}

// parseAllowedSubcmds parses a string of the form
// "gt:prime,hook,done;bd:create,update,close" into a map of command → subcommand set.
func parseAllowedSubcmds(s string) map[string][]string {
	if s == "" {
		return nil
	}
	result := make(map[string][]string)
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, ":")
		if idx < 0 {
			continue
		}
		cmd := strings.TrimSpace(part[:idx])
		subsStr := strings.TrimSpace(part[idx+1:])
		var subs []string
		for _, sub := range strings.Split(subsStr, ",") {
			sub = strings.TrimSpace(sub)
			if sub != "" {
				subs = append(subs, sub)
			}
		}
		if cmd != "" && len(subs) > 0 {
			result[cmd] = subs
		}
	}
	return result
}
