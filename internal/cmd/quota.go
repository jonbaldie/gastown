package cmd

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/quota"
	"github.com/jonbaldie/gastown/internal/style"
	ttmux "github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/util"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// quotaLogger adapts style.PrintWarning to the quota.Logger interface.
type quotaLogger struct{}

func (quotaLogger) Warn(format string, args ...interface{}) {
	style.PrintWarning(format, args...)
}

var quotaCmd = &cobra.Command{
	Use:     "quota",
	GroupID: GroupServices,
	Short:   "Manage account quota rotation",
	RunE:    requireSubcommand,
	Long: `Manage Claude Code account quota rotation for Gas Town.

When sessions hit rate limits, quota commands help detect blocked sessions
and rotate them to available accounts from the pool.

Commands:
  gt quota status            Show account quota status
  gt quota scan              Detect rate-limited sessions
  gt quota rotate            Swap blocked sessions to available accounts
  gt quota clear             Mark account(s) as available again`,
}

var quotaStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show account quota status",
	Long: `Show the quota status of all registered accounts.

Displays which accounts are available, rate-limited, or in cooldown,
along with timestamps for limit detection and estimated reset times.

Examples:
  gt quota status           # Text output
  gt quota status --json    # JSON output`,
	RunE: runQuotaStatus,
}

// QuotaStatusItem represents an account in status output.
type QuotaStatusItem struct {
	Handle    string `json:"handle"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	LimitedAt string `json:"limited_at,omitempty"`
	ResetsAt  string `json:"resets_at,omitempty"`
	LastUsed  string `json:"last_used,omitempty"`
	IsDefault bool   `json:"is_default"`
}

func runQuotaStatus(cmd *cobra.Command, _ []string) error {
	jsonOutput := commandBoolFlag(cmd, "json")
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	// Load accounts
	accountsPath := constants.MayorAccountsPath(townRoot)
	acctCfg, err := config.LoadAccountsConfig(accountsPath)
	if err != nil {
		fmt.Println("No accounts configured.")
		fmt.Println("\nTo add an account:")
		fmt.Println("  gt account add <handle>")
		return nil
	}

	if len(acctCfg.Accounts) == 0 {
		fmt.Println("No accounts configured.")
		return nil
	}

	// Load quota state
	mgr := quota.NewManager(townRoot)
	state, err := mgr.Load()
	if err != nil {
		return fmt.Errorf("loading quota state: %w", err)
	}

	// Ensure all accounts are tracked
	mgr.EnsureAccountsTracked(state, acctCfg.Accounts)

	// Auto-clear accounts whose reset time has passed
	if cleared := mgr.ClearExpired(state); cleared > 0 {
		if err := mgr.Save(state); err != nil {
			style.PrintWarning("could not persist expired account clearance: %v", err)
		}
	}

	if jsonOutput {
		return printQuotaStatusJSON(acctCfg, state)
	}
	return printQuotaStatusText(acctCfg, state)
}

func printQuotaStatusJSON(acctCfg *config.AccountsConfig, state *config.QuotaState) error {
	var items []QuotaStatusItem
	for _, handle := range slices.Sorted(maps.Keys(acctCfg.Accounts)) {
		acct := acctCfg.Accounts[handle]
		qs := state.Accounts[handle]
		status := string(qs.Status)
		if status == "" {
			status = string(config.QuotaStatusAvailable)
		}
		items = append(items, QuotaStatusItem{
			Handle:    handle,
			Email:     acct.Email,
			Status:    status,
			LimitedAt: qs.LimitedAt,
			ResetsAt:  qs.ResetsAt,
			LastUsed:  qs.LastUsed,
			IsDefault: handle == acctCfg.Default,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(items)
}

func printQuotaStatusText(acctCfg *config.AccountsConfig, state *config.QuotaState) error {
	available := 0
	limited := 0

	fmt.Println(style.Bold.Render("Account Quota Status"))
	fmt.Println()

	for _, handle := range slices.Sorted(maps.Keys(acctCfg.Accounts)) {
		acct := acctCfg.Accounts[handle]
		qs := state.Accounts[handle]
		status := qs.Status
		if status == "" {
			status = config.QuotaStatusAvailable
		}

		// Handle marker and default indicator
		marker := " "
		if handle == acctCfg.Default {
			marker = "*"
		}

		// Status badge
		var badge string
		switch status {
		case config.QuotaStatusAvailable:
			badge = style.Success.Render("available")
			available++
		case config.QuotaStatusLimited:
			badge = style.Error.Render("limited")
			limited++
			if qs.ResetsAt != "" {
				badge += style.Dim.Render(" (resets " + qs.ResetsAt + ")")
			}
		case config.QuotaStatusCooldown:
			badge = style.Warning.Render("cooldown")
			limited++
		default:
			badge = style.Dim.Render("unknown")
		}

		email := ""
		if acct.Email != "" {
			email = style.Dim.Render(" <" + acct.Email + ">")
		}

		fmt.Printf(" %s %-12s %s%s\n", marker, handle, badge, email)
	}

	fmt.Println()
	fmt.Printf(" %s %d available, %d limited\n",
		style.Info.Render("Summary:"), available, limited)

	return nil
}

var quotaScanCmd = &cobra.Command{
	Use:   "scan",
	Short: "Detect rate-limited sessions",
	Long: `Scan all Gas Town tmux sessions for rate-limit indicators.

Captures recent pane output from each session and checks for rate-limit
messages. Reports which sessions are blocked and which account they use.

Use --update to automatically update quota state with detected limits.

Examples:
  gt quota scan              # Report rate-limited sessions
  gt quota scan --update     # Report and update quota state
  gt quota scan --json       # JSON output`,
	RunE: runQuotaScan,
}

func runQuotaScan(cmd *cobra.Command, _ []string) error {
	jsonOutput := commandBoolFlag(cmd, "json")
	update := commandBoolFlag(cmd, "update")
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	// Load accounts config
	accountsPath := constants.MayorAccountsPath(townRoot)
	acctCfg, loadErr := config.LoadAccountsConfig(accountsPath)
	// acctCfg can be nil if no accounts configured — scan still works

	// Create scanner
	t := ttmux.NewTmux()
	scanner, err := quota.NewScanner(t, nil, acctCfg)
	if err != nil {
		return fmt.Errorf("creating scanner: %w", err)
	}

	results, err := scanner.ScanAll()
	if err != nil {
		return fmt.Errorf("scanning sessions: %w", err)
	}

	// Optionally update quota state
	if update && loadErr == nil && acctCfg != nil {
		if err := updateQuotaState(townRoot, results, acctCfg); err != nil {
			return fmt.Errorf("updating quota state: %w", err)
		}
	}

	if jsonOutput {
		return printScanJSON(results)
	}
	return printScanText(results)
}

func updateQuotaState(townRoot string, results []quota.ScanResult, acctCfg *config.AccountsConfig) error {
	mgr := quota.NewManager(townRoot)
	return mgr.WithLock(func() error {
		state, err := mgr.Load()
		if err != nil {
			return err
		}
		mgr.EnsureAccountsTracked(state, acctCfg.Accounts)

		now := time.Now().UTC().Format(time.RFC3339)
		for _, r := range results {
			if r.RateLimited && r.AccountHandle != "" {
				existing := state.Accounts[r.AccountHandle]
				state.Accounts[r.AccountHandle] = config.AccountQuotaState{
					Status:    config.QuotaStatusLimited,
					LimitedAt: now,
					ResetsAt:  r.ResetsAt,
					LastUsed:  existing.LastUsed,
				}
			}
		}

		return mgr.SaveUnlocked(state)
	})
}

func printScanJSON(results []quota.ScanResult) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

func printScanText(results []quota.ScanResult) error {
	limited := 0
	nearLimit := 0

	for _, r := range results {
		limitedDelta, nearDelta := printScanResult(r)
		limited += limitedDelta
		nearLimit += nearDelta
	}
	printScanSummary(len(results), limited, nearLimit)
	return nil
}

func printScanResult(result quota.ScanResult) (limited, nearLimit int) {
	account := result.AccountHandle
	if account == "" {
		account = "(unknown)"
	}
	if result.RateLimited {
		resets := ""
		if result.ResetsAt != "" {
			resets = style.Dim.Render(" resets " + result.ResetsAt)
		}
		fmt.Printf(" %s %-25s %s %s%s\n", style.Error.Render("!"), result.Session, style.Dim.Render("account:"), account, resets)
		return 1, 0
	}
	if !result.NearLimit {
		return 0, 0
	}
	detail := ""
	if result.MatchedLine != "" {
		detail = style.Dim.Render(fmt.Sprintf(" (%s)", result.MatchedLine))
	}
	fmt.Printf(" %s %-25s %s %s%s\n", style.Warning.Render("~"), result.Session, style.Dim.Render("account:"), account, detail)
	return 0, 1
}

func printScanSummary(total, limited, nearLimit int) {
	if limited == 0 && nearLimit == 0 {
		fmt.Printf(" %s No rate-limited sessions detected (%d scanned)\n",
			style.SuccessPrefix, total)
	} else {
		fmt.Println()
		parts := []string{}
		if limited > 0 {
			parts = append(parts, fmt.Sprintf("%d limited", limited))
		}
		if nearLimit > 0 {
			parts = append(parts, fmt.Sprintf("%d near-limit", nearLimit))
		}
		fmt.Printf(" %s %s of %d sessions\n",
			style.Warning.Render("Summary:"), strings.Join(parts, ", "), total)
	}
}

var quotaRotateCmd = &cobra.Command{
	Use:   "rotate",
	Short: "Swap blocked sessions to available accounts",
	Long: `Rotate rate-limited sessions to available accounts.

Scans all sessions for rate limits, plans account assignments using
least-recently-used ordering, and restarts blocked sessions with fresh accounts.

Use --from to preemptively rotate sessions using a specific account before
it hits its rate limit. This is useful for switching idle sessions while
it's not disruptive.

The rotation process:
  1. Scans all Gas Town sessions for rate-limit indicators
  2. Selects available accounts (LRU order)
  3. Swaps macOS Keychain credentials (same config dir preserved)
  4. Restarts blocked sessions via respawn-pane
  5. Sends /resume to recover conversation context

Examples:
  gt quota rotate                    # Rotate all blocked sessions
  gt quota rotate --from work        # Preemptively rotate sessions on 'work' account
  gt quota rotate --from work --idle # Only rotate idle sessions on 'work' account
  gt quota rotate --dry-run          # Show plan without executing
  gt quota rotate --json             # JSON output`,
	RunE: runQuotaRotate,
}

func runQuotaRotate(cmd *cobra.Command, _ []string) error {
	opts := quotaRotateOptionsFromCommand(cmd)
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}
	rotation, err := prepareQuotaRotation(townRoot, opts.fromAccount)
	if err != nil {
		return err
	}
	if handled, err := handleEmptyQuotaRotation(rotation.plan, opts); handled || err != nil {
		return err
	}

	execution, handled, err := prepareQuotaRotationExecution(rotation, opts)
	if handled || err != nil {
		return err
	}
	return runPreparedQuotaRotation(rotation, execution, opts)
}

func runPreparedQuotaRotation(rotation *quotaRotation, execution quotaRotationExecution, opts quotaRotateOptions) error {
	if !opts.jsonOutput {
		printQuotaRotationPlan(rotation, execution.sessions, execution.noConfigDir, execution.unassignable)
	}
	if opts.dryRun {
		return outputQuotaRotationDryRun(rotation.plan, opts.jsonOutput)
	}
	return executeQuotaRotationOutput(rotation, execution.sessions, opts.jsonOutput)
}

func executeQuotaRotationOutput(rotation *quotaRotation, sessions []string, jsonOutput bool) error {
	results := executeQuotaRotation(rotation, sessions, jsonOutput)
	if !jsonOutput {
		return nil
	}
	return encodeQuotaRotateResults(results)
}

type quotaRotationExecution struct {
	sessions     []string
	noConfigDir  int
	unassignable int
}

func prepareQuotaRotationExecution(rotation *quotaRotation, opts quotaRotateOptions) (quotaRotationExecution, bool, error) {
	noConfigDir, unassignable := countUnassignableQuotaSessions(rotation.plan)
	if !opts.idleOnly {
		return quotaRotationExecution{
			sessions:     slices.Sorted(maps.Keys(rotation.plan.Assignments)),
			noConfigDir:  noConfigDir,
			unassignable: unassignable,
		}, false, nil
	}

	filterBusyQuotaSessions(rotation, opts.jsonOutput)
	if len(rotation.plan.Assignments) > 0 {
		return quotaRotationExecution{
			sessions:     slices.Sorted(maps.Keys(rotation.plan.Assignments)),
			noConfigDir:  noConfigDir,
			unassignable: unassignable,
		}, false, nil
	}
	if opts.jsonOutput {
		return quotaRotationExecution{}, true, encodeQuotaRotateResults(nil)
	}
	fmt.Printf("\n %s No idle sessions to rotate\n", style.WarningPrefix)
	return quotaRotationExecution{}, true, nil
}

type quotaRotateOptions struct {
	dryRun      bool
	jsonOutput  bool
	fromAccount string
	idleOnly    bool
}

func quotaRotateOptionsFromCommand(cmd *cobra.Command) quotaRotateOptions {
	return quotaRotateOptions{
		dryRun:      commandBoolFlag(cmd, "dry-run"),
		jsonOutput:  commandBoolFlag(cmd, "json"),
		fromAccount: commandStringFlag(cmd, "from"),
		idleOnly:    commandBoolFlag(cmd, "idle"),
	}
}

type quotaRotation struct {
	t       *ttmux.Tmux
	mgr     *quota.Manager
	acctCfg *config.AccountsConfig
	plan    *quota.RotatePlan
}

func prepareQuotaRotation(townRoot, fromAccount string) (*quotaRotation, error) {

	// Load accounts config (required for rotation)
	accountsPath := constants.MayorAccountsPath(townRoot)
	acctCfg, err := config.LoadAccountsConfig(accountsPath)
	if err != nil {
		return nil, fmt.Errorf("no accounts configured (run 'gt account add' first): %w", err)
	}
	if len(acctCfg.Accounts) < 2 {
		return nil, fmt.Errorf("need at least 2 accounts for rotation (have %d)", len(acctCfg.Accounts))
	}

	// Validate --from account if specified
	if fromAccount != "" {
		if _, ok := acctCfg.Accounts[fromAccount]; !ok {
			return nil, fmt.Errorf("account %q not found (available: %s)",
				fromAccount, strings.Join(accountHandles(acctCfg), ", "))
		}
	}

	// Create scanner and plan rotation
	t := ttmux.NewTmux()
	scanner, err := quota.NewScanner(t, nil, acctCfg)
	if err != nil {
		return nil, fmt.Errorf("creating scanner: %w", err)
	}

	mgr := quota.NewManager(townRoot)
	plan, err := quota.PlanRotation(scanner, mgr, acctCfg, quota.PlanOpts{FromAccount: fromAccount})
	if err != nil {
		return nil, fmt.Errorf("planning rotation: %w", err)
	}
	return &quotaRotation{t: t, mgr: mgr, acctCfg: acctCfg, plan: plan}, nil
}

func handleEmptyQuotaRotation(plan *quota.RotatePlan, opts quotaRotateOptions) (bool, error) {
	if len(plan.LimitedSessions) == 0 {
		if opts.jsonOutput {
			return true, encodeQuotaRotateResults(nil)
		}
		if opts.fromAccount != "" {
			fmt.Printf(" %s No sessions found using account %q\n", style.SuccessPrefix, opts.fromAccount)
		} else {
			fmt.Printf(" %s No rate-limited sessions detected\n", style.SuccessPrefix)
		}
		return true, nil
	}
	if len(plan.Assignments) > 0 {
		return false, nil
	}
	if opts.jsonOutput {
		return true, encodeQuotaRotateResults(nil)
	}
	if opts.fromAccount != "" {
		fmt.Printf(" %s %d session(s) on %q but no available accounts to rotate to\n", style.WarningPrefix, len(plan.LimitedSessions), opts.fromAccount)
	} else {
		fmt.Printf(" %s %d sessions rate-limited but no available accounts to rotate to\n", style.WarningPrefix, len(plan.LimitedSessions))
	}
	if len(plan.SkippedAccounts) > 0 {
		fmt.Println()
		for handle, reason := range plan.SkippedAccounts {
			fmt.Printf(" %s Skipped %s — %s\n", style.WarningPrefix, handle, reason)
		}
	}
	return true, nil
}

func countUnassignableQuotaSessions(plan *quota.RotatePlan) (noConfigDir, unassignable int) {
	for _, result := range plan.LimitedSessions {
		if _, assigned := plan.Assignments[result.Session]; assigned {
			continue
		}
		if result.AccountHandle == "" && result.ConfigDir == "" {
			noConfigDir++
		}
	}
	unassignable = len(plan.LimitedSessions) - len(plan.Assignments) - noConfigDir
	return noConfigDir, unassignable
}

func filterBusyQuotaSessions(rotation *quotaRotation, jsonOutput bool) {
	for session := range rotation.plan.Assignments {
		if rotation.t.IsIdle(session) {
			continue
		}
		if !jsonOutput {
			fmt.Printf(" %s %-25s %s\n", style.Dim.Render("-"), session, style.Dim.Render("skipped (busy)"))
		}
		delete(rotation.plan.Assignments, session)
	}
}

func printQuotaRotationPlan(rotation *quotaRotation, sessions []string, noConfigDir, unassignable int) {
	fmt.Println(style.Bold.Render("Rotation Plan"))
	fmt.Println()
	for _, session := range sessions {
		fmt.Printf(" %s %-25s %s → %s\n", style.ArrowPrefix, session,
			style.Dim.Render(quotaOldAccount(rotation.plan.LimitedSessions, session)),
			style.Success.Render(rotation.plan.Assignments[session]))
	}
	if noConfigDir > 0 {
		fmt.Printf("\n %s %d session(s) skipped (no CLAUDE_CONFIG_DIR)\n", style.WarningPrefix, noConfigDir)
	}
	if unassignable > 0 {
		fmt.Printf(" %s %d session(s) cannot be rotated (not enough available accounts)\n", style.WarningPrefix, unassignable)
	}
	printQuotaSkippedAccounts(rotation.acctCfg, rotation.plan.SkippedAccounts)
}

func quotaOldAccount(results []quota.ScanResult, session string) string {
	for _, result := range results {
		if result.Session == session {
			if result.AccountHandle != "" {
				return result.AccountHandle
			}
			break
		}
	}
	return "(unknown)"
}

func printQuotaSkippedAccounts(acctCfg *config.AccountsConfig, skipped map[string]string) {
	if len(skipped) == 0 {
		return
	}
	fmt.Println()
	for handle, reason := range skipped {
		acct := acctCfg.Accounts[handle]
		fmt.Printf(" %s Skipped %s — %s\n", style.WarningPrefix, handle, reason)
		fmt.Printf("   Run: claude /login  (in CLAUDE_CONFIG_DIR=%s)\n", acct.ConfigDir)
	}
}

func outputQuotaRotationDryRun(plan *quota.RotatePlan, jsonOutput bool) error {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	}
	fmt.Println()
	fmt.Println(style.Dim.Render(" (dry run — no changes made)"))
	return nil
}

func executeQuotaRotation(rotation *quotaRotation, sessions []string, jsonOutput bool) []quota.RotateResult {
	if !jsonOutput {
		fmt.Println()
	}
	swappedConfigDirs := make(map[string]*quota.KeychainCredential)
	results := make([]quota.RotateResult, 0, len(sessions))
	for _, session := range sessions {
		result := executeKeychainRotation(rotation.t, rotation.mgr, rotation.acctCfg, session, rotation.plan.Assignments[session], swappedConfigDirs)
		results = append(results, result)
		if !jsonOutput {
			printQuotaRotationResult(result)
		}
	}
	return results
}

func printQuotaRotationResult(result quota.RotateResult) {
	if result.Rotated {
		suffix := ""
		if result.ResumedSession != "" {
			suffix = style.Dim.Render(" (resumed)")
		}
		if result.KeychainSwap {
			suffix += style.Dim.Render(" [keychain]")
		}
		fmt.Printf(" %s %s → %s%s\n", style.SuccessPrefix, result.Session, result.NewAccount, suffix)
		return
	}
	if result.Error != "" {
		fmt.Printf(" %s %s: %s\n", style.ErrorPrefix, result.Session, result.Error)
	}
}

func encodeQuotaRotateResults(results []quota.RotateResult) error {
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(results)
}

var quotaClearCmd = &cobra.Command{
	Use:   "clear [handle...]",
	Short: "Mark account(s) as available again",
	Long: `Clear the rate-limited status for one or more accounts, marking them available.

When no handles are specified, all limited accounts are cleared.

Examples:
  gt quota clear              # Clear all limited accounts
  gt quota clear work         # Clear a specific account
  gt quota clear work personal`,
	RunE: runQuotaClear,
}

func runQuotaClear(_ *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	mgr := quota.NewManager(townRoot)

	if len(args) == 0 {
		return clearAllQuotaAccounts(mgr)
	}
	return clearNamedQuotaAccounts(mgr, args)
}

func clearAllQuotaAccounts(mgr *quota.Manager) error {
	state, err := mgr.Load()
	if err != nil {
		return fmt.Errorf("loading quota state: %w", err)
	}
	cleared := 0
	for handle, acctState := range state.Accounts {
		if !isQuotaUnavailable(acctState.Status) {
			continue
		}
		if err := mgr.MarkAvailable(handle); err != nil {
			return fmt.Errorf("clearing %s: %w", handle, err)
		}
		fmt.Printf(" %s %s → available\n", style.SuccessPrefix, handle)
		cleared++
	}
	if cleared == 0 {
		fmt.Printf(" %s No limited accounts to clear\n", style.SuccessPrefix)
	}
	return nil
}

func isQuotaUnavailable(status config.AccountQuotaStatus) bool {
	return status == config.QuotaStatusLimited || status == config.QuotaStatusCooldown
}

func clearNamedQuotaAccounts(mgr *quota.Manager, handles []string) error {
	for _, handle := range handles {
		if err := mgr.MarkAvailable(handle); err != nil {
			return fmt.Errorf("clearing %s: %w", handle, err)
		}
		fmt.Printf(" %s %s → available\n", style.SuccessPrefix, handle)
	}
	return nil
}

// accountHandles returns sorted account handle names for error messages.
func accountHandles(acctCfg *config.AccountsConfig) []string {
	handles := make([]string, 0, len(acctCfg.Accounts))
	for h := range acctCfg.Accounts {
		handles = append(handles, h)
	}
	slices.Sort(handles)
	return handles
}

// executeKeychainRotation performs context-preserving rotation for a single session.
// Instead of changing CLAUDE_CONFIG_DIR (which destroys context), it swaps the
// macOS Keychain OAuth token from an available account into the rate-limited
// account's keychain entry, then respawns with the SAME config dir so /resume works.
//
// swappedConfigDirs tracks which config dirs have already been swapped in this
// rotation batch — multiple sessions sharing a config dir only need one swap.
func executeKeychainRotation(
	t *ttmux.Tmux,
	mgr *quota.Manager,
	acctCfg *config.AccountsConfig,
	session, newAccount string,
	swappedConfigDirs map[string]*quota.KeychainCredential,
) quota.RotateResult {
	result := quota.RotateResult{
		Session:    session,
		NewAccount: newAccount,
	}

	// Read the session's current CLAUDE_CONFIG_DIR, falling back to ~/.claude.
	currentConfigDir, err := quotaCurrentConfigDir(t, session)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.OldAccount = quotaOldAccountForConfig(acctCfg, currentConfigDir)

	sourceConfigDir, err := quotaSourceConfigDir(acctCfg, newAccount)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if !swapQuotaCredentials(currentConfigDir, sourceConfigDir, session, swappedConfigDirs, &result) {
		return result
	}

	// ContinueSession omits the beacon prompt and adds --continue, so the
	// agent silently resumes where it left off without a fresh handoff cycle.
	restartCmd, err := quotaRestartCommand(session, currentConfigDir, newAccount)
	if err != nil {
		// Session types that can't be restarted (e.g., hq-boot/deacon) still
		// benefit from the keychain swap above — mark as rotated without restart.
		result.Rotated = true
		result.Error = fmt.Sprintf("keychain swapped but could not restart: %v", err)
		return result
	}
	if err := respawnQuotaSession(t, session, restartCmd, newAccount); err != nil {
		result.Error = err.Error()
		return result
	}

	// Context recovery is handled by --continue in the restart command.
	result.ResumedSession = "continue"
	if err := updateQuotaRotationState(mgr, currentConfigDir, newAccount); err != nil {
		style.PrintWarning("could not update LastUsed for %s: %v", newAccount, err)
	}

	result.Rotated = true
	return result
}

func quotaCurrentConfigDir(t *ttmux.Tmux, session string) (string, error) {
	currentConfigDir, envErr := t.GetEnvironment(session, "CLAUDE_CONFIG_DIR")
	if envErr == nil && strings.TrimSpace(currentConfigDir) != "" {
		return currentConfigDir, nil
	}
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		return "", fmt.Errorf("reading CLAUDE_CONFIG_DIR: %v", envErr)
	}
	return home + "/.claude", nil
}

func quotaOldAccountForConfig(acctCfg *config.AccountsConfig, currentConfigDir string) string {
	for handle, acct := range acctCfg.Accounts {
		if acct.ConfigDir == currentConfigDir || util.ExpandHome(acct.ConfigDir) == currentConfigDir {
			return handle
		}
	}
	return ""
}

func quotaSourceConfigDir(acctCfg *config.AccountsConfig, newAccount string) (string, error) {
	newAcct, ok := acctCfg.Accounts[newAccount]
	if !ok {
		return "", fmt.Errorf("account %q not found in config", newAccount)
	}
	return util.ExpandHome(newAcct.ConfigDir), nil
}

func swapQuotaCredentials(
	currentConfigDir, sourceConfigDir, session string,
	swappedConfigDirs map[string]*quota.KeychainCredential,
	result *quota.RotateResult,
) bool {
	if _, alreadySwapped := swappedConfigDirs[currentConfigDir]; alreadySwapped {
		return true
	}
	backup, err := quota.SwapKeychainCredential(currentConfigDir, sourceConfigDir)
	if err != nil {
		result.Error = fmt.Sprintf("keychain swap failed: %v", err)
		return false
	}
	swappedConfigDirs[currentConfigDir] = backup

	// Also swap the oauthAccount in .claude.json so Claude Code identifies
	// as the new account (correct accountUuid/organizationUuid for rate limits).
	if _, err := quota.SwapOAuthAccount(currentConfigDir, sourceConfigDir); err != nil {
		style.PrintWarning("could not swap oauthAccount for %s: %v", session, err)
	}
	result.KeychainSwap = true
	return true
}

func quotaRestartCommand(session, currentConfigDir, newAccount string) (string, error) {
	restartCmd, err := buildRestartCommandWithOpts(session, buildRestartCommandOpts{
		ContinueSession: true,
	})
	if err != nil {
		return "", err
	}
	return config.PrependEnv(restartCmd, map[string]string{
		"CLAUDE_CONFIG_DIR": currentConfigDir,
		"GT_QUOTA_ACCOUNT":  newAccount,
	}), nil
}

func respawnQuotaSession(t *ttmux.Tmux, session, restartCmd, newAccount string) error {
	pane, err := t.GetPaneID(session)
	if err != nil {
		return fmt.Errorf("getting pane: %v", err)
	}

	// Keep the pane alive while its process is replaced.
	if err := t.SetRemainOnExit(pane, true); err != nil {
		style.PrintWarning("could not set remain-on-exit for %s: %v", session, err)
	}
	if err := t.KillPaneProcesses(pane); err != nil {
		style.PrintWarning("could not kill pane processes for %s: %v", session, err)
	}
	if err := t.ClearHistory(pane); err != nil {
		style.PrintWarning("could not clear history for %s: %v", session, err)
	}
	if err := t.RespawnPane(pane, restartCmd); err != nil {
		return fmt.Errorf("respawning pane: %v", err)
	}
	// The shell export in restartCmd only affects the process environment;
	// this sets the session value used by the scanner's GetEnvironment call.
	if err := t.SetEnvironment(session, "GT_QUOTA_ACCOUNT", newAccount); err != nil {
		style.PrintWarning("could not set GT_QUOTA_ACCOUNT for %s: %v", session, err)
	}
	return nil
}

func updateQuotaRotationState(mgr *quota.Manager, currentConfigDir, newAccount string) error {
	return mgr.WithLock(func() error {
		state, err := mgr.Load()
		if err != nil {
			return err
		}
		existing := state.Accounts[newAccount]
		existing.LastUsed = time.Now().UTC().Format(time.RFC3339)
		state.Accounts[newAccount] = existing

		// Record the swap mapping so SyncSwappedTokens can propagate
		// fresh tokens if the source account re-authenticates later.
		quota.RecordSwap(state, currentConfigDir, newAccount)
		return mgr.SaveUnlocked(state)
	})
}

var quotaWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Monitor sessions and rotate proactively before hard 429",
	Long: `Continuously monitor sessions for approaching rate limits and rotate proactively.

Polls all Gas Town sessions on the specified interval, checking for both
hard rate limits and near-limit warning signals via pane pattern matching.

When a session is detected as approaching its limit, rotation is triggered
before the hard 429 hits.

Examples:
  gt quota watch                      # Watch with default 5m interval
  gt quota watch --interval 2m        # Custom interval
  gt quota watch --dry-run            # Show detections without rotating`,
	RunE: runQuotaWatch,
}

func runQuotaWatch(cmd *cobra.Command, _ []string) error {
	interval, dryRun, err := quotaWatchOptions(cmd)
	if err != nil {
		return err
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}

	acctCfg, err := loadQuotaWatchAccounts(townRoot)
	if err != nil {
		return err
	}

	printQuotaWatchHeader(interval, dryRun)
	return runQuotaWatchLoop(townRoot, acctCfg, interval, dryRun)
}

func quotaWatchOptions(cmd *cobra.Command) (time.Duration, bool, error) {
	interval, err := cmd.Flags().GetDuration("interval")
	if err != nil {
		return 0, false, fmt.Errorf("reading --interval: %w", err)
	}
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return 0, false, fmt.Errorf("reading --dry-run: %w", err)
	}
	return interval, dryRun, nil
}

func loadQuotaWatchAccounts(townRoot string) (*config.AccountsConfig, error) {
	accountsPath := constants.MayorAccountsPath(townRoot)
	acctCfg, err := config.LoadAccountsConfig(accountsPath)
	if err != nil {
		return nil, fmt.Errorf("no accounts configured: %w", err)
	}
	if len(acctCfg.Accounts) < 2 {
		return nil, fmt.Errorf("need at least 2 accounts for rotation (have %d)", len(acctCfg.Accounts))
	}
	return acctCfg, nil
}

func printQuotaWatchHeader(interval time.Duration, dryRun bool) {
	fmt.Printf(" %s Watching for near-limit signals (interval: %s)\n",
		style.Info.Render("Watch:"), interval)
	if dryRun {
		fmt.Println(style.Dim.Render(" (dry run — detections only, no rotation)"))
	}
	fmt.Println()
}

func runQuotaWatchLoop(townRoot string, acctCfg *config.AccountsConfig, interval time.Duration, dryRun bool) error {
	// Handle graceful shutdown on SIGTERM/SIGINT.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run immediately on start, then on each tick
	for {
		runWatchCycle(townRoot, acctCfg, dryRun)

		select {
		case <-sigCh:
			fmt.Printf("\n %s Shutting down watch\n", style.Info.Render("Watch:"))
			return nil
		case <-ticker.C:
		}
	}
}

func runWatchCycle(townRoot string, acctCfg *config.AccountsConfig, dryRun bool) {
	t := ttmux.NewTmux()
	scanner, ok := prepareQuotaWatchScanner(t, acctCfg)
	if !ok {
		return
	}

	mgr := quota.NewManager(townRoot)
	syncQuotaWatchTokens(mgr, acctCfg)

	plan, err := quota.PlanRotation(scanner, mgr, acctCfg, quota.PlanOpts{IncludeNearLimit: true})
	if err != nil {
		style.PrintWarning("planning rotation: %v", err)
		return
	}

	now := time.Now().Format("15:04:05")
	if !printQuotaWatchFindings(plan, now) {
		return
	}
	if dryRun || len(plan.Assignments) == 0 {
		return
	}
	executeQuotaWatchRotation(t, mgr, acctCfg, plan, now)
}

func prepareQuotaWatchScanner(t *ttmux.Tmux, acctCfg *config.AccountsConfig) (*quota.Scanner, bool) {
	scanner, err := quota.NewScanner(t, nil, acctCfg)
	if err != nil {
		style.PrintWarning("creating scanner: %v", err)
		return nil, false
	}
	// Enable near-limit detection via pane patterns.
	if err := scanner.WithWarningPatterns(nil); err != nil {
		style.PrintWarning("setting warning patterns: %v", err)
		return nil, false
	}
	return scanner, true
}

func syncQuotaWatchTokens(mgr *quota.Manager, acctCfg *config.AccountsConfig) {
	// If a source account re-authenticated since the last rotation, propagate
	// the fresh token to all target keychain entries.
	state, err := mgr.Load()
	if err != nil || len(state.ActiveSwaps) == 0 {
		return
	}
	resolved := quota.ResolveSwapSourceDirs(state.ActiveSwaps, acctCfg.Accounts)
	if n := quota.SyncSwappedTokens(resolved); n > 0 {
		now := time.Now().Format("15:04:05")
		fmt.Printf(" [%s] %s synced %d swapped keychain(s)\n",
			style.Dim.Render(now),
			style.Info.Render("Sync:"),
			n)
	}
}

func printQuotaWatchFindings(plan *quota.RotatePlan, now string) bool {
	if len(plan.LimitedSessions)+len(plan.NearLimitSessions) == 0 {
		fmt.Printf(" [%s] %s\n", style.Dim.Render(now), style.Dim.Render("all clear"))
		return false
	}
	for _, r := range plan.LimitedSessions {
		fmt.Printf(" [%s] %s %-25s %s\n",
			style.Dim.Render(now),
			style.Error.Render("LIMITED"),
			r.Session,
			style.Dim.Render(r.AccountHandle))
	}
	for _, r := range plan.NearLimitSessions {
		detail := ""
		if r.MatchedLine != "" {
			detail = fmt.Sprintf(" (%s)", r.MatchedLine)
		}
		fmt.Printf(" [%s] %s %-25s %s%s\n",
			style.Dim.Render(now),
			style.Warning.Render("NEAR"),
			r.Session,
			style.Dim.Render(r.AccountHandle),
			style.Dim.Render(detail))
	}
	return true
}

func executeQuotaWatchRotation(t *ttmux.Tmux, mgr *quota.Manager, acctCfg *config.AccountsConfig, plan *quota.RotatePlan, now string) {
	swappedConfigDirs := make(map[string]*quota.KeychainCredential)
	for _, session := range slices.Sorted(maps.Keys(plan.Assignments)) {
		newAccount := plan.Assignments[session]
		result := executeKeychainRotation(t, mgr, acctCfg, session, newAccount, swappedConfigDirs)
		printQuotaWatchResult(result, now)
	}
}

func printQuotaWatchResult(result quota.RotateResult, now string) {
	if result.Rotated {
		fmt.Printf(" [%s] %s %s → %s\n",
			style.Dim.Render(now),
			style.SuccessPrefix,
			result.Session,
			style.Success.Render(result.NewAccount))
		return
	}
	if result.Error != "" {
		fmt.Printf(" [%s] %s %s: %s\n",
			style.Dim.Render(now),
			style.ErrorPrefix,
			result.Session,
			result.Error)
	}
}

func init() {
	quotaStatusCmd.Flags().Bool("json", false, "Output as JSON")

	quotaScanCmd.Flags().Bool("json", false, "Output as JSON")
	quotaScanCmd.Flags().Bool("update", false, "Update quota state with detected limits")

	quotaRotateCmd.Flags().Bool("dry-run", false, "Show plan without executing")
	quotaRotateCmd.Flags().Bool("json", false, "Output as JSON")
	quotaRotateCmd.Flags().String("from", "", "Preemptively rotate sessions using this account")
	quotaRotateCmd.Flags().Bool("idle", false, "Only rotate sessions at the idle prompt (skip busy agents)")

	quotaWatchCmd.Flags().Duration("interval", 5*time.Minute, "Poll interval")
	quotaWatchCmd.Flags().Bool("dry-run", false, "Show detections without executing rotation")

	quotaCmd.AddCommand(quotaStatusCmd)
	quotaCmd.AddCommand(quotaScanCmd)
	quotaCmd.AddCommand(quotaRotateCmd)
	quotaCmd.AddCommand(quotaClearCmd)
	quotaCmd.AddCommand(quotaWatchCmd)

	rootCmd.AddCommand(quotaCmd)
}
