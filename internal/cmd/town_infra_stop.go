package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/worker"
)

// stopTownDoltAndWorker stops this town's Dolt SQL server and
// `gt worker serve --town <town>` process. Other towns are left running.
func stopTownDoltAndWorker(townRoot string) error {
	if townRoot == "" {
		return nil
	}
	return errors.Join(stopTownDoltServer(townRoot), stopTownWorkerServe(townRoot))
}

func stopTownDoltServer(townRoot string) error {
	cfg := doltserver.DefaultConfig(townRoot)
	if cfg.IsRemote() {
		fmt.Printf("  %s Dolt is remote (%s) — not stopped\n",
			style.Dim.Render("○"), cfg.HostPort())
		return nil
	}
	var errs []error

	idleMonitors := doltserver.FindIdleMonitorProcesses(townRoot)
	if len(idleMonitors) > 0 {
		stopped := stopIdleMonitors(idleMonitors)
		if stopped > 0 {
			fmt.Printf("  %s Dolt idle-monitors stopped (%d)\n",
				style.Bold.Render("✓"), stopped)
		}
	}

	errs = append(errs, stopDoltDataDir(cfg, townRoot)...)

	errs = append(errs, cleanupDoltProcesses(townRoot)...)

	running, pid, err := doltserver.IsRunning(townRoot)
	if err != nil {
		fmt.Printf("  %s Could not verify Dolt stop: %v\n",
			style.Bold.Render("⚠"), err)
		return errors.Join(append(errs, fmt.Errorf("verifying Dolt stop: %w", err))...)
	}
	if running {
		fmt.Printf("  %s Dolt still running after stop (PID %d)\n",
			style.Bold.Render("⚠"), pid)
		errs = append(errs, fmt.Errorf("Dolt still running after stop (PID %d)", pid))
	}
	return errors.Join(errs...)
}

func stopDoltDataDir(cfg *doltserver.Config, townRoot string) []error {
	if _, err := os.Stat(cfg.DataDir); err != nil {
		return nil
	}

	running, pid, err := doltserver.IsRunning(townRoot)
	if err != nil {
		fmt.Printf("  %s Dolt status check failed: %v\n",
			style.Bold.Render("⚠"), err)
		return []error{fmt.Errorf("checking Dolt status: %w", err)}
	}
	if !running {
		fmt.Printf("  %s Dolt not running\n", style.Dim.Render("○"))
		return nil
	}
	if err := doltserver.Stop(townRoot); err != nil {
		fmt.Printf("  %s Failed to stop Dolt (PID %d): %v\n",
			style.Bold.Render("✗"), pid, err)
		return []error{fmt.Errorf("stopping Dolt PID %d: %w", pid, err)}
	}
	fmt.Printf("  %s Dolt stopped (was PID %d)\n",
		style.Bold.Render("✓"), pid)
	return nil
}

func cleanupDoltProcesses(townRoot string) []error {
	var errs []error
	if err := doltserver.KillImposters(townRoot); err != nil {
		fmt.Printf("  %s Dolt imposter cleanup failed: %v\n",
			style.Bold.Render("⚠"), err)
		errs = append(errs, fmt.Errorf("cleaning Dolt imposters: %w", err))
	}
	if pids := findOrphanDoltServers(townRoot); len(pids) > 0 {
		if stopped := stopOrphanDoltServers(pids); stopped > 0 {
			fmt.Printf("  %s Stopped %d orphan Dolt server(s)\n",
				style.Bold.Render("✓"), stopped)
		}
	}
	return errs
}

func stopTownWorkerServe(townRoot string) error {
	pids := worker.FindServePIDs(townRoot)
	if len(pids) == 0 {
		fmt.Printf("  %s Worker serve not running\n", style.Dim.Render("○"))
		_ = os.Remove(worker.SocketPath(townRoot))
		_ = os.Remove(worker.PortPath(townRoot))
		return nil
	}
	stopped, stopErr := worker.StopServeAndWait(townRoot)
	if leftover := worker.FindServePIDs(townRoot); len(leftover) > 0 {
		fmt.Printf("  %s Worker serve still running (PIDs %v)\n",
			style.Bold.Render("⚠"), leftover)
		return fmt.Errorf("worker serve still running (PIDs %v)", leftover)
	}
	if stopErr != nil {
		fmt.Printf("  %s Worker serve teardown incomplete: %v\n", style.Bold.Render("✗"), stopErr)
		return stopErr
	}
	if stopped > 0 {
		fmt.Printf("  %s Worker serve stopped (%d, town %s)\n",
			style.Bold.Render("✓"), stopped, filepath.Base(townRoot))
		return nil
	}
	fmt.Printf("  %s Worker serve not running\n", style.Dim.Render("○"))
	return nil
}

func verifyTownInfraStopped(townRoot string) {
	running, pid, err := doltserver.IsRunning(townRoot)
	switch {
	case err != nil:
		fmt.Printf("  %s Could not verify Dolt: %v\n", style.Bold.Render("⚠"), err)
	case running:
		fmt.Printf("  %s Dolt still running (PID %d)\n", style.Bold.Render("⚠"), pid)
	default:
		fmt.Printf("  %s Dolt not running\n", style.Bold.Render("✓"))
	}
	if leftover := worker.FindServePIDs(townRoot); len(leftover) > 0 {
		fmt.Printf("  %s Worker serve still running (PIDs %v)\n",
			style.Bold.Render("⚠"), leftover)
		return
	}
	fmt.Printf("  %s Worker serve not running\n", style.Bold.Render("✓"))
}
