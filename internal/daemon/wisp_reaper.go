package daemon

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	agentconfig "github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/dog"
	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/reaper"
	"github.com/jonbaldie/gastown/internal/util"
)

const (
	// defaultWispReaperInterval is the patrol interval. Set to 1h since reaping
	// is cleanup work, not latency-sensitive. Was 30m before Dog-driven refactor.
	defaultWispReaperInterval = 1 * time.Hour
	// Wisps older than this are reaped (closed). Configurable via formula var max_age.
	defaultWispMaxAge = 24 * time.Hour
	// Closed wisps older than this are permanently deleted. Formula var: purge_age.
	defaultWispDeleteAge = 7 * 24 * time.Hour
	// Alert threshold: if open wisp count exceeds this, the Dog should escalate.
	// Shared with `gt reaper run` warning. See reaper.DefaultAlertThreshold.
	wispAlertThreshold = reaper.DefaultAlertThreshold
	// Closed mail older than this is permanently deleted. Formula var: mail_delete_age.
	defaultMailDeleteAge = 7 * 24 * time.Hour
	// Issues stale longer than this are auto-closed. Formula var: stale_issue_age.
	defaultStaleIssueAge = 7 * 24 * time.Hour
)

// WispReaperConfig holds configuration for the wisp_reaper patrol.
type WispReaperConfig struct {
	Enabled      bool     `json:"enabled"`
	DryRun       bool     `json:"dry_run,omitempty"`
	IntervalStr  string   `json:"interval,omitempty"`
	MaxAgeStr    string   `json:"max_age,omitempty"`
	DeleteAgeStr string   `json:"delete_age,omitempty"`
	Databases    []string `json:"databases,omitempty"`
}

// wispReaperInterval returns the configured interval, or the default (1h).
func wispReaperInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.WispReaper.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultWispReaperInterval
}

// wispReaperMaxAge returns the configured max age, or the default (24h).
func wispReaperMaxAge(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.MaxAgeStr != "" {
			if d, err := time.ParseDuration(config.Patrols.WispReaper.MaxAgeStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultWispMaxAge
}

// wispDeleteAge returns the configured delete age, or the default (7 days).
func wispDeleteAge(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.WispReaper != nil {
		if config.Patrols.WispReaper.DeleteAgeStr != "" {
			if d, err := time.ParseDuration(config.Patrols.WispReaper.DeleteAgeStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultWispDeleteAge
}

// reapWisps is the thin orchestrator for the wisp_reaper patrol.
// It pours a mol-dog-reaper molecule, then dispatches a Dog to execute it.
// The Dog reads the formula steps and calls `gt reaper` CLI helpers.
// Falls back to inline execution if Dog dispatch fails.
func (d *Daemon) reapWisps() {
	if !d.isPatrolActive("wisp_reaper") {
		return
	}

	if err := dog.RequireDispatchAllowed(d.config.TownRoot); err != nil {
		d.logger.Printf("wisp_reaper: guardian blocked dog dispatch: %v", err)
		return
	}

	config := d.patrolConfig.Patrols.WispReaper
	maxAge := wispReaperMaxAge(d.patrolConfig)
	deleteAge := wispDeleteAge(d.patrolConfig)

	vars := map[string]string{
		"max_age":         maxAge.String(),
		"purge_age":       deleteAge.String(),
		"stale_issue_age": defaultStaleIssueAge.String(),
		"mail_delete_age": defaultMailDeleteAge.String(),
		"alert_threshold": fmt.Sprintf("%d", wispAlertThreshold),
	}

	if config.DryRun {
		vars["dry_run"] = "true"
	}
	if len(config.Databases) > 0 {
		vars["databases"] = strings.Join(config.Databases, ",")
	}

	// Pour the molecule for observability tracking.
	mol := d.pourDogMolecule(constants.MolDogReaper, vars)
	defer mol.Close()

	if config.DryRun {
		d.logger.Printf("wisp_reaper: DRY RUN — reporting only, no changes will be made")
	}

	// Try dispatching to a Dog for formula-driven execution.
	if err := d.dispatchReaperDog(vars); err != nil {
		if actErr := dog.RequireActivationAllowed(d.config.TownRoot); actErr != nil {
			d.logger.Printf("wisp_reaper: Dog dispatch failed (%v); inline fallback blocked by guardian: %v", err, actErr)
			mol.FailStep("dispatch", actErr.Error())
			return
		}
		d.logger.Printf("wisp_reaper: Dog dispatch failed (%v), running inline fallback", err)
		d.reapWispsInline(config, maxAge, deleteAge, mol)
		return
	}

	d.logger.Printf("wisp_reaper: dispatched to Dog for formula-driven execution")
}

// dispatchReaperDog dispatches the mol-dog-reaper formula to a Dog via gt sling.
func (d *Daemon) dispatchReaperDog(vars map[string]string) error {
	args := []string{"sling", constants.MolDogReaper, "deacon/dogs"}
	for k, v := range vars {
		args = append(args, "--var", fmt.Sprintf("%s=%s", k, v))
	}

	cmd := exec.Command(d.gtPath, args...) //nolint:gosec // G204: d.gtPath resolved at daemon init via LookPath
	cmd.Dir = d.config.TownRoot
	// gt sling performs writes, so use mutation routing env: it preserves PATH
	// while stripping stale bd target selectors and derived Beads endpoint aliases.
	cmd.Env = bdMutationRoutingEnv(d.config.TownRoot)
	util.SetDetachedProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gt sling: %w", err)
	}
	return nil
}

type inlineReapStats struct {
	totalReaped         int
	totalMoleculeSteps  int
	totalOpen           int
	totalPurged         int
	totalMailPurged     int
	totalPluginClosed   int
	totalDispatchClosed int
	totalAutoClosed     int
}

// reapWispsInline is the fallback that runs the reaper cycle inline when
// Dog dispatch is unavailable. Delegates to the reaper package for SQL execution.
func (d *Daemon) reapWispsInline(config *WispReaperConfig, maxAge, deleteAge time.Duration, mol *dogMol) {
	databases := config.Databases
	host := d.doltServerHost()
	if len(databases) == 0 {
		databases = reaper.DiscoverDatabases(host, d.doltServerPort())
	}
	if len(databases) == 0 {
		d.logger.Printf("wisp_reaper: no databases to reap")
		mol.FailStep("scan", "no databases found")
		return
	}
	d.logger.Printf("wisp_reaper: scanning %d databases (inline fallback)", len(databases))
	mol.CloseStep("scan")

	port := d.doltServerPort()
	dryRun := config.DryRun
	stats := &inlineReapStats{}

	// Step 2: Reap
	reapErrors := d.reapInlineWisps(databases, host, port, maxAge, dryRun, stats)
	if reapErrors > 0 {
		mol.FailStep("reap", fmt.Sprintf("%d databases had reap errors", reapErrors))
	} else {
		mol.CloseStep("reap")
	}

	// Step 3: Purge
	purgeErrors := d.purgeInlineWisps(databases, host, port, deleteAge, dryRun, stats)
	if purgeErrors > 0 {
		mol.FailStep("purge", fmt.Sprintf("%d databases had purge errors", purgeErrors))
	} else {
		mol.CloseStep("purge")
	}

	// Step 3b: Close plugin receipts (fast-track — 1h instead of 7d stale age)
	pluginReceiptAge := 1 * time.Hour
	d.closeInlinePluginReceipts(databases, host, port, pluginReceiptAge, dryRun, stats)

	// Step 3c: Close plugin dispatch mails (daemon→dog instruction beads that are never closed)
	pluginDispatchAge := 1 * time.Hour
	d.closeInlinePluginDispatches(databases, host, port, pluginDispatchAge, dryRun, stats)

	// Step 4: Auto-close
	autoCloseErrors := d.autoCloseInlineIssues(databases, host, port, dryRun, stats)
	if autoCloseErrors > 0 {
		mol.FailStep("auto-close", fmt.Sprintf("%d databases had auto-close errors", autoCloseErrors))
	} else {
		mol.CloseStep("auto-close")
	}

	// Step 5: Report
	d.reportInlineReap(stats, len(databases), dryRun, mol)
}

func (d *Daemon) reapInlineWisps(databases []string, host string, port int, maxAge time.Duration, dryRun bool, stats *inlineReapStats) int {
	errors := 0
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := reaper.OpenDB(host, port, dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: connect error: %v", dbName, err)
			errors++
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			d.logger.Printf("wisp_reaper: %s: skipped (no reaper schema)", dbName)
			db.Close()
			continue
		}
		result, err := reaper.Reap(db, dbName, maxAge, dryRun)
		db.Close()
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: reap error: %v", dbName, err)
			errors++
			continue
		}
		stats.totalReaped += result.Reaped
		stats.totalMoleculeSteps += result.MoleculeStepsClosed
		stats.totalOpen += result.OpenRemain
		if result.Reaped > 0 || result.MoleculeStepsClosed > 0 {
			d.logInlineReapResult(dbName, result.Reaped, result.MoleculeStepsClosed, result.OpenRemain)
		}
	}
	return errors
}

func (d *Daemon) logInlineReapResult(dbName string, reaped, moleculeSteps, openRemain int) {
	summary := fmt.Sprintf("wisp_reaper: %s: reaped %d stale wisps", dbName, reaped)
	if moleculeSteps > 0 {
		summary += fmt.Sprintf(", closed %d molecule steps", moleculeSteps)
	}
	d.logger.Printf("%s, %d open remain", summary, openRemain)
}

func (d *Daemon) purgeInlineWisps(databases []string, host string, port int, deleteAge time.Duration, dryRun bool, stats *inlineReapStats) int {
	errors := 0
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := reaper.OpenDB(host, port, dbName, 30*time.Second, 30*time.Second)
		if err != nil {
			errors++
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			db.Close()
			continue
		}
		result, err := reaper.Purge(db, dbName, deleteAge, defaultMailDeleteAge, dryRun)
		db.Close()
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: purge error: %v", dbName, err)
			errors++
			continue
		}
		stats.totalPurged += result.WispsPurged
		stats.totalMailPurged += result.MailPurged
		for _, anomaly := range result.Anomalies {
			d.logger.Printf("wisp_reaper: %s: ANOMALY: %s", dbName, anomaly.Message)
		}
	}
	return errors
}

func (d *Daemon) closeInlinePluginReceipts(databases []string, host string, port int, age time.Duration, dryRun bool, stats *inlineReapStats) {
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := reaper.OpenDB(host, port, dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			db.Close()
			continue
		}
		result, err := reaper.ClosePluginReceipts(db, dbName, age, dryRun)
		db.Close()
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: plugin receipt close error: %v", dbName, err)
			continue
		}
		stats.totalPluginClosed += result.Closed
		if result.Closed > 0 {
			d.logger.Printf("wisp_reaper: %s: closed %d plugin receipts", dbName, result.Closed)
		}
	}
}

func (d *Daemon) closeInlinePluginDispatches(databases []string, host string, port int, age time.Duration, dryRun bool, stats *inlineReapStats) {
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := reaper.OpenDB(host, port, dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			db.Close()
			continue
		}
		result, err := reaper.ClosePluginDispatches(db, dbName, age, dryRun)
		db.Close()
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: plugin dispatch close error: %v", dbName, err)
			continue
		}
		stats.totalDispatchClosed += result.Closed
		if result.Closed > 0 {
			d.logger.Printf("wisp_reaper: %s: closed %d plugin dispatches", dbName, result.Closed)
		}
	}
}

func (d *Daemon) autoCloseInlineIssues(databases []string, host string, port int, dryRun bool, stats *inlineReapStats) int {
	errors := 0
	for _, dbName := range databases {
		if err := reaper.ValidateDBName(dbName); err != nil {
			continue
		}
		db, err := reaper.OpenDB(host, port, dbName, 10*time.Second, 10*time.Second)
		if err != nil {
			errors++
			continue
		}
		if ok, _ := reaper.HasReaperSchema(db); !ok {
			db.Close()
			continue
		}
		result, err := reaper.AutoClose(db, dbName, defaultStaleIssueAge, dryRun)
		db.Close()
		if err != nil {
			d.logger.Printf("wisp_reaper: %s: auto-close error: %v", dbName, err)
			errors++
			continue
		}
		stats.totalAutoClosed += result.Closed
	}
	return errors
}

func (d *Daemon) reportInlineReap(stats *inlineReapStats, databaseCount int, dryRun bool, mol *dogMol) {
	if stats.totalOpen > wispAlertThreshold {
		d.logger.Printf("wisp_reaper: WARNING: %d open wisps exceed threshold %d — investigate wisp lifecycle",
			stats.totalOpen, wispAlertThreshold)
	}
	summary := fmt.Sprintf("wisp_reaper: cycle complete — reaped=%d", stats.totalReaped)
	if stats.totalMoleculeSteps > 0 {
		summary += fmt.Sprintf(" molecule_steps_closed=%d", stats.totalMoleculeSteps)
	}
	summary += fmt.Sprintf(" purged=%d mail_purged=%d plugin_closed=%d dispatch_closed=%d auto_closed=%d open=%d databases=%d dryRun=%v",
		stats.totalPurged, stats.totalMailPurged, stats.totalPluginClosed, stats.totalDispatchClosed, stats.totalAutoClosed, stats.totalOpen, databaseCount, dryRun)
	d.logger.Printf("%s", summary)
	mol.CloseStep("report")
}

// doltServerPort returns the configured Dolt server port.
func (d *Daemon) doltServerPort() int {
	if d.doltServer != nil {
		return d.doltServer.config.Port
	}
	if port := agentconfig.ResolveDoltPort(d.config.TownRoot); port > 0 {
		return port
	}
	return doltserver.DefaultPort
}

func (d *Daemon) doltServerHost() string {
	if d.doltServer != nil && d.doltServer.config.Host != "" {
		return d.doltServer.config.Host
	}
	if host := agentconfig.ResolveDoltHost(d.config.TownRoot); host != "" {
		return host
	}
	if cfg := doltserver.DefaultConfig(d.config.TownRoot); cfg.Host != "" {
		return cfg.Host
	}
	return "127.0.0.1"
}
