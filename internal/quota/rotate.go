package quota

import (
	"fmt"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/util"
)

// RotateResult holds the result of rotating a single session.
type RotateResult struct {
	Session        string `json:"session"`                   // tmux session name
	OldAccount     string `json:"old_account,omitempty"`     // previous account handle
	NewAccount     string `json:"new_account,omitempty"`     // new account handle
	Rotated        bool   `json:"rotated"`                   // whether rotation occurred
	ResumedSession string `json:"resumed_session,omitempty"` // session ID that was resumed (empty if fresh start)
	KeychainSwap   bool   `json:"keychain_swap,omitempty"`   // whether keychain was swapped
	Error          string `json:"error,omitempty"`           // error message if rotation failed
}

// RotatePlan describes what the rotator will do.
type RotatePlan struct {
	// LimitedSessions are sessions detected as hard rate-limited.
	LimitedSessions []ScanResult

	// NearLimitSessions are sessions approaching their rate limit.
	// Only populated when PlanOpts.IncludeNearLimit is true.
	NearLimitSessions []ScanResult `json:"near_limit_sessions,omitempty"`

	// AvailableAccounts are accounts that can be rotated to.
	AvailableAccounts []string

	// Assignments maps session -> new account handle.
	Assignments map[string]string

	// ConfigDirSwaps maps config_dir -> new account handle.
	// One keychain swap per config dir, not per session.
	// All sessions sharing a config dir get the same assignment.
	ConfigDirSwaps map[string]string

	// SkippedAccounts maps handle -> reason for accounts that were
	// available by quota status but had invalid/expired tokens.
	SkippedAccounts map[string]string `json:"skipped_accounts,omitempty"`
}

// PlanOpts configures the rotation planning behavior.
type PlanOpts struct {
	// FromAccount targets all sessions using this account regardless of
	// rate-limit status (preemptive rotation). Empty string = default behavior.
	FromAccount string

	// IncludeNearLimit includes sessions approaching their rate limit
	// (not just hard-limited sessions) as rotation candidates.
	IncludeNearLimit bool
}

type rotationConfigDirInfo struct {
	configDir     string
	accountHandle string
}

// PlanRotation scans for limited sessions and plans account assignments.
// The opts parameter controls targeting behavior:
//   - opts.FromAccount: targets all sessions using that account regardless of limit status
//   - opts.IncludeNearLimit: also targets sessions approaching their limit
//
// Returns a plan that can be reviewed before execution.
func PlanRotation(scanner *Scanner, mgr *Manager, acctCfg *config.AccountsConfig, opts PlanOpts) (*RotatePlan, error) {
	results, err := scanner.ScanAll()
	if err != nil {
		return nil, fmt.Errorf("scanning sessions: %w", err)
	}

	state, err := mgr.Load()
	if err != nil {
		return nil, fmt.Errorf("loading quota state: %w", err)
	}
	mgr.EnsureAccountsTracked(state, acctCfg.Accounts)

	// Auto-clear accounts whose reset time has passed so they
	// become available for rotation.
	mgr.ClearExpired(state)

	limitedSessions, nearLimitSessions := selectRotationTargets(results, opts)
	targetSessions := combineRotationTargets(limitedSessions, nearLimitSessions, opts.IncludeNearLimit)

	// Available accounts come from persisted state only — NOT from scan
	// detections. Stale sessions (e.g., parked rigs with old rate-limit
	// messages still in the pane) would otherwise mark their accounts as
	// limited, shrinking the available pool and blocking rotation of
	// sessions that actually need it.
	//
	// The caller persists confirmed rate-limit state after execution.
	available, skipped := validRotationAccounts(mgr.AvailableAccounts(state), opts.FromAccount, acctCfg)
	uniqueConfigDirs := collectRotationConfigDirs(targetSessions, acctCfg)
	configDirSwaps := assignRotationConfigDirs(uniqueConfigDirs, available)
	assignments := expandRotationAssignments(targetSessions, acctCfg, configDirSwaps)

	return &RotatePlan{
		LimitedSessions:   limitedSessions,
		NearLimitSessions: nearLimitSessions,
		AvailableAccounts: available,
		Assignments:       assignments,
		ConfigDirSwaps:    configDirSwaps,
		SkippedAccounts:   skipped,
	}, nil
}

func selectRotationTargets(results []ScanResult, opts PlanOpts) ([]ScanResult, []ScanResult) {
	var limitedSessions []ScanResult
	var nearLimitSessions []ScanResult
	for _, result := range results {
		if opts.FromAccount != "" {
			if result.AccountHandle == opts.FromAccount {
				limitedSessions = append(limitedSessions, result)
			}
			continue
		}
		if result.RateLimited {
			limitedSessions = append(limitedSessions, result)
		} else if result.NearLimit {
			nearLimitSessions = append(nearLimitSessions, result)
		}
	}
	return limitedSessions, nearLimitSessions
}

func combineRotationTargets(limited, nearLimit []ScanResult, includeNearLimit bool) []ScanResult {
	if !includeNearLimit {
		return limited
	}
	return append(limited, nearLimit...)
}

func validRotationAccounts(available []string, fromAccount string, acctCfg *config.AccountsConfig) ([]string, map[string]string) {
	skipped := make(map[string]string)
	var valid []string
	for _, handle := range available {
		if handle == fromAccount {
			continue
		}
		acct, ok := acctCfg.Accounts[handle]
		if !ok {
			continue
		}
		if err := ValidateKeychainToken(util.ExpandHome(acct.ConfigDir)); err != nil {
			skipped[handle] = err.Error()
			continue
		}
		valid = append(valid, handle)
	}
	return valid, skipped
}

func collectRotationConfigDirs(targetSessions []ScanResult, acctCfg *config.AccountsConfig) map[string]*rotationConfigDirInfo {
	unique := make(map[string]*rotationConfigDirInfo)
	for _, result := range targetSessions {
		configDir, ok := rotationConfigDir(result, acctCfg)
		if !ok {
			continue
		}
		if _, exists := unique[configDir]; !exists {
			unique[configDir] = &rotationConfigDirInfo{
				configDir:     configDir,
				accountHandle: result.AccountHandle,
			}
		}
	}
	return unique
}

func rotationConfigDir(result ScanResult, acctCfg *config.AccountsConfig) (string, bool) {
	if result.AccountHandle != "" {
		acct, ok := acctCfg.Accounts[result.AccountHandle]
		if !ok {
			return "", false
		}
		return util.ExpandHome(acct.ConfigDir), true
	}
	if result.ConfigDir == "" {
		return "", false
	}
	return result.ConfigDir, true
}

func assignRotationConfigDirs(unique map[string]*rotationConfigDirInfo, available []string) map[string]string {
	swaps := make(map[string]string)
	availIdx := 0
	for configDir, info := range unique {
		if availIdx >= len(available) {
			break
		}
		candidate := available[availIdx]
		if candidate == info.accountHandle {
			availIdx++
			if availIdx >= len(available) {
				break
			}
			candidate = available[availIdx]
		}
		swaps[configDir] = candidate
		availIdx++
	}
	return swaps
}

func expandRotationAssignments(targetSessions []ScanResult, acctCfg *config.AccountsConfig, swaps map[string]string) map[string]string {
	assignments := make(map[string]string)
	for _, result := range targetSessions {
		configDir, ok := rotationConfigDir(result, acctCfg)
		if !ok {
			continue
		}
		if newAccount, ok := swaps[configDir]; ok {
			assignments[result.Session] = newAccount
		}
	}
	return assignments
}
