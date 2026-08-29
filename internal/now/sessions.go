package now

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/deacon"
	"github.com/jonbaldie/gastown/internal/formula"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/mayor"
	"github.com/jonbaldie/gastown/internal/refinery"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/skills"
	"github.com/jonbaldie/gastown/internal/templates"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/util"
	"github.com/jonbaldie/gastown/internal/witness"
	"github.com/jonbaldie/gastown/internal/workspace"
)

func startSessions(ctx context.Context, townRoot string, mayorChanged bool, opts Options, hooks Hooks) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureNowDaemon(townRoot, hooks); err != nil {
		return err
	}
	if err := startMayorDeaconSessions(ctx, townRoot, mayorChanged && opts.MayorSpec != ""); err != nil {
		return err
	}
	if opts.RestartWorkers {
		return restartWorkers(townRoot)
	}
	return nil
}

func ensureNowDaemon(townRoot string, hooks Hooks) error {
	if err := config.EnsureDaemonPatrolConfig(townRoot); err != nil {
		return fmt.Errorf("ensuring daemon config: %w", err)
	}
	if hooks.EnsureDaemon == nil {
		return nil
	}
	if err := hooks.EnsureDaemon(townRoot); err != nil {
		return fmt.Errorf("starting daemon: %w", err)
	}
	return nil
}

func startMayorDeaconSessions(ctx context.Context, townRoot string, restartMayorRoles bool) error {
	var wg sync.WaitGroup
	var firstErr error
	var mu sync.Mutex
	setErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		mgr := mayor.NewManager(townRoot)
		setErr(reuseOrStartSession(ctx, mayor.SessionName(), restartMayorRoles, mgr.Stop, func() error {
			return mgr.StartImmediate("")
		}, mayor.ErrNotRunning, mayor.ErrAlreadyRunning, mayor.ErrACPActive))
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		mgr := deacon.NewManager(townRoot)
		setErr(reuseOrStartSession(ctx, deacon.SessionName(), restartMayorRoles, mgr.Stop, func() error {
			return mgr.StartImmediate("")
		}, deacon.ErrNotRunning, deacon.ErrAlreadyRunning))
	}()
	wg.Wait()
	return firstErr
}

func reuseOrStartSession(ctx context.Context, name string, restart bool, stop, start func() error, skipStop error, skipStart ...error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	live, err := sessionLive(name)
	if err != nil {
		return fmt.Errorf("checking %s session: %w", name, err)
	}
	live, err = maybeRestartSession(live, restart, name, stop, skipStop)
	if err != nil {
		return err
	}
	if live {
		return nil
	}
	if err := start(); err != nil && !ignoreStartConflict(err, skipStart...) {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	return nil
}

func maybeRestartSession(live, restart bool, name string, stop func() error, skipStop error) (bool, error) {
	if !live || !restart {
		return live, nil
	}
	if err := stop(); err != nil && !errors.Is(err, skipStop) {
		return live, fmt.Errorf("stopping %s: %w", name, err)
	}
	return false, nil
}

func requireLiveSession(name string) error {
	live, err := sessionLive(name)
	if err != nil {
		return fmt.Errorf("Mayor session is not running: %w", err)
	}
	if !live {
		return fmt.Errorf("Mayor session is not running")
	}
	return nil
}

func sessionLive(name string) (bool, error) {
	tm := tmux.NewTmux()
	has, err := tm.HasSession(name)
	if err != nil || !has {
		return false, err
	}
	dead, err := tm.IsPaneDead(name)
	if err != nil {
		return false, err
	}
	return !dead, nil
}

func ignoreStartConflict(err error, skipStart ...error) bool {
	if errors.Is(err, tmux.ErrSessionExists) {
		return true
	}
	for _, skip := range skipStart {
		if errors.Is(err, skip) {
			return true
		}
	}
	return false
}

func restartWorkers(townRoot string) error {
	rigsConfig, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading rigs for --restart-workers: %w", err)
	}
	mgr := rig.NewManager(townRoot, rigsConfig, git.NewGit(townRoot))
	var errs []error
	for name := range rigsConfig.Rigs {
		errs = append(errs, restartRigWorkers(mgr, name)...)
	}
	return errors.Join(errs...)
}

func restartRigWorkers(mgr *rig.Manager, name string) []error {
	r, err := mgr.GetRig(name)
	if err != nil {
		return []error{fmt.Errorf("loading rig %s: %w", name, err)}
	}
	var errs []error
	errs = append(errs, restartWitness(r, name)...)
	errs = append(errs, restartRefinery(r, name)...)
	return errs
}

func restartWitness(r *rig.Rig, name string) []error {
	wit := witness.NewManager(r)
	if err := wit.Stop(); err != nil && !errors.Is(err, witness.ErrNotRunning) {
		return []error{fmt.Errorf("stopping witness for %s: %w", name, err)}
	}
	if err := wit.Start(false, "", nil); err != nil && !errors.Is(err, witness.ErrAlreadyRunning) {
		return []error{fmt.Errorf("restarting witness for %s: %w", name, err)}
	}
	return nil
}

func restartRefinery(r *rig.Rig, name string) []error {
	ref := refinery.NewManager(r)
	if err := ref.Stop(); err != nil && !errors.Is(err, refinery.ErrNotRunning) {
		return []error{fmt.Errorf("stopping refinery for %s: %w", name, err)}
	}
	if err := ref.Start(false, ""); err != nil && !errors.Is(err, refinery.ErrAlreadyRunning) {
		return []error{fmt.Errorf("restarting refinery for %s: %w", name, err)}
	}
	return nil
}

func startDeferredProvision(executable, townRoot string) error {
	if executable == "" {
		return fmt.Errorf("gt executable path is empty")
	}
	cmd := exec.Command(executable, "now", "--provision-only", "--town", townRoot)
	cmd.Dir = townRoot
	cmd.Env = append(os.Environ(), "GT_TOWN_ROOT="+townRoot)
	cmd.Stdin = nil
	if logFile, err := os.OpenFile(filepath.Join(townRoot, "mayor", "provision.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	}
	util.SetDetachedProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting deferred provision: %w", err)
	}
	return nil
}

func provisionTown(townRoot string, hooks Hooks) error {
	if err := requireTownHQ(townRoot); err != nil {
		return err
	}
	var errs []error
	appendProvisionErr(&errs, wrapProvisionErr(ensureAllLocalRigBeads(townRoot), "initializing rig beads"))
	appendProvisionErr(&errs, provisionFormulasForTown(townRoot))
	appendProvisionErr(&errs, wrapProvisionErr(templates.ProvisionCommands(townRoot), "provisioning slash commands"))
	appendProvisionErr(&errs, wrapProvisionErr(skills.ProvisionFor(townRoot, "claude"), "provisioning skills"))
	appendProvisionErr(&errs, initAgentBeadsForTown(townRoot, hooks))
	appendProvisionErr(&errs, wrapProvisionErr(ensureAllRigAgentBeads(townRoot), "initializing rig agent beads"))
	return errors.Join(errs...)
}

func requireTownHQ(townRoot string) error {
	ok, err := workspace.IsWorkspace(townRoot)
	if err != nil {
		return fmt.Errorf("checking Town HQ: %w", err)
	}
	if !ok {
		return fmt.Errorf("not a Gas Town HQ: %s", townRoot)
	}
	return nil
}

func provisionFormulasForTown(townRoot string) error {
	count, err := formula.ProvisionFormulas(townRoot)
	if err != nil {
		return fmt.Errorf("provisioning formulas: %w", err)
	}
	if count > 0 {
		fmt.Printf("provisioned %d formulas\n", count)
	}
	return nil
}

func initAgentBeadsForTown(townRoot string, hooks Hooks) error {
	if hooks.InitAgentBeads == nil {
		return nil
	}
	if err := hooks.InitAgentBeads(townRoot); err != nil {
		return fmt.Errorf("initializing agent beads: %w", err)
	}
	return nil
}

func wrapProvisionErr(err error, verb string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", verb, err)
}

func appendProvisionErr(errs *[]error, err error) {
	if err != nil {
		*errs = append(*errs, err)
	}
}
