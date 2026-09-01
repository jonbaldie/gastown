package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/constants"
	"github.com/jonbaldie/gastown/internal/events"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/telemetry"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

type nudgeRun struct {
	target   string
	message  string
	sender   string
	townRoot string
	t        *tmux.Tmux
}

func runNudge(_ *cobra.Command, args []string) (retErr error) {
	defer func() {
		target := ""
		if len(args) > 0 {
			target = args[0]
		}
		telemetry.RecordNudge(context.Background(), target, retErr)
	}()
	if err := validateNudgeFlags(); err != nil {
		return err
	}
	if skipIfFreshNudge() {
		return nil
	}
	r, err := beginNudgeRun(args)
	if err != nil {
		return err
	}
	if strings.HasPrefix(r.target, "channel:") {
		return runNudgeChannel(strings.TrimPrefix(r.target, "channel:"), r.message, r.sender)
	}
	if skipNudgeDND(r) {
		return nil
	}
	return dispatchNudgeRun(r)
}

func validateNudgeFlags() error {
	if !validNudgeModes[nudgeState().mode] {
		return fmt.Errorf("invalid --mode %q: must be one of immediate, queue, wait-idle", nudgeState().mode)
	}
	if !validNudgePriorities[nudgeState().priority] {
		return fmt.Errorf("invalid --priority %q: must be one of normal, urgent, system", nudgeState().priority)
	}
	return nil
}

func skipIfFreshNudge() bool {
	if !nudgeState().ifFresh {
		return false
	}
	sessionName := tmux.CurrentSessionName()
	if sessionName == "" {
		return false
	}
	created, err := tmux.NewTmux().GetSessionCreatedUnix(sessionName)
	if err != nil || created <= 0 {
		return false
	}
	return time.Since(time.Unix(created, 0)) > ifFreshMaxAge
}

func beginNudgeRun(args []string) (*nudgeRun, error) {
	message, err := readNudgeMessage(args)
	if err != nil {
		return nil, err
	}
	townRoot, _ := workspace.FindFromCwd()
	if townRoot != "" {
		_ = session.InitRegistry(townRoot)
	}
	return &nudgeRun{
		target:   strings.TrimSuffix(args[0], "/"),
		message:  message,
		sender:   nudgeSenderName(),
		townRoot: townRoot,
		t:        tmux.NewTmux(),
	}, nil
}

func readNudgeMessage(args []string) (string, error) {
	if nudgeState().stdin {
		if nudgeState().message != "" {
			return "", fmt.Errorf("cannot use --stdin with --message/-m")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		nudgeState().message = strings.TrimRight(string(data), "\n")
	}
	if nudgeState().message != "" {
		return nudgeState().message, nil
	}
	if len(args) >= 2 {
		return args[1], nil
	}
	return "", fmt.Errorf("message required: use -m flag or provide as second argument")
}

func nudgeSenderName() string {
	roleInfo, err := GetRole()
	if err != nil {
		return "unknown"
	}
	switch roleInfo.Role {
	case RoleMayor:
		return constants.RoleMayor
	case RoleCrew:
		return fmt.Sprintf("%s/crew/%s", roleInfo.Rig, roleInfo.Polecat)
	case RolePolecat:
		return fmt.Sprintf("%s/%s", roleInfo.Rig, roleInfo.Polecat)
	case RoleWitness:
		return fmt.Sprintf("%s/witness", roleInfo.Rig)
	case RoleRefinery:
		return fmt.Sprintf("%s/refinery", roleInfo.Rig)
	case RoleDeacon:
		return constants.RoleDeacon
	default:
		return string(roleInfo.Role)
	}
}

func skipNudgeDND(r *nudgeRun) bool {
	if r.townRoot == "" || nudgeState().force {
		return false
	}
	shouldSend, level, _ := shouldNudgeTarget(r.townRoot, r.target, nudgeState().force)
	if shouldSend {
		return false
	}
	fmt.Printf("%s Target has DND enabled (%s) - nudge skipped\n", style.Dim.Render("○"), level)
	fmt.Printf("  Use %s to override\n", style.Bold.Render("--force"))
	return true
}

func dispatchNudgeRun(r *nudgeRun) error {
	if err := expandNudgeRoleShortcut(r); err != nil {
		return err
	}
	if r.target == constants.RoleDeacon {
		return nudgeDeacon(r)
	}
	if dogName, ok := mail.DogAddressName(r.target); ok {
		return nudgeDog(r, dogName)
	}
	if strings.HasPrefix(r.target, constants.RoleMayor+"/") || strings.HasPrefix(r.target, constants.RoleDeacon+"/") {
		return fmt.Errorf("invalid town target %q", r.target)
	}
	if strings.Contains(r.target, "/") {
		return nudgeAddressTarget(r)
	}
	return nudgeRawSession(r)
}

func expandNudgeRoleShortcut(r *nudgeRun) error {
	switch r.target {
	case constants.RoleMayor:
		r.target = session.MayorSessionName()
		return nil
	case constants.RoleWitness, constants.RoleRefinery:
		roleInfo, err := GetRole()
		if err != nil {
			return fmt.Errorf("cannot determine rig for %s shortcut: %w", r.target, err)
		}
		if roleInfo.Rig == "" {
			return fmt.Errorf("cannot determine rig for %s shortcut (not in a rig context)", r.target)
		}
		rigPrefix := session.PrefixFor(roleInfo.Rig)
		if r.target == constants.RoleWitness {
			r.target = session.WitnessSessionName(rigPrefix)
		} else {
			r.target = session.RefinerySessionName(rigPrefix)
		}
	}
	return nil
}

func nudgeDeacon(r *nudgeRun) error {
	deaconSession := session.DeaconSessionName()
	hasACP := hasACPSessionByName(r.townRoot, deaconSession)
	exists := false
	if !hasACP {
		exists, _ = nudgeTargetExists(r.t, r.townRoot, deaconSession)
	}
	if !hasACP && !exists {
		fmt.Printf("%s Deacon not running, nudge skipped\n", style.Dim.Render("○"))
		return nil
	}
	if err := deliverNudge(r.t, deaconSession, r.message, r.sender); err != nil {
		return fmt.Errorf("nudging deacon: %w", err)
	}
	fmt.Printf("%s Nudged deacon (%s)\n", style.Bold.Render("✓"), nudgeState().mode)
	logNudgeDelivery(r.sender, constants.RoleDeacon, r.message, "")
	return nil
}

func nudgeDog(r *nudgeRun, dogName string) error {
	sessionName := session.DogSessionName(dogName)
	if err := requireNudgeSession(r, sessionName, "checking dog session"); err != nil {
		return err
	}
	if err := deliverNudge(r.t, sessionName, r.message, r.sender); err != nil {
		return fmt.Errorf("nudging dog: %w", err)
	}
	fmt.Printf("%s Nudged %s (%s)\n", style.Bold.Render("✓"), r.target, nudgeState().mode)
	logNudgeDelivery(r.sender, r.target, r.message, "")
	return nil
}

func nudgeAddressTarget(r *nudgeRun) error {
	rigName, polecatName, err := parseAddress(r.target)
	if err != nil {
		return err
	}
	sessionName, err := resolveNudgeAddressSession(r.t, rigName, polecatName)
	if err != nil {
		return err
	}
	if err := requireNudgeSession(r, sessionName, "checking session"); err != nil {
		return err
	}
	if err := deliverNudge(r.t, sessionName, r.message, r.sender); err != nil {
		return fmt.Errorf("nudging session: %w", err)
	}
	fmt.Printf("%s Nudged %s/%s (%s)\n", style.Bold.Render("✓"), rigName, polecatName, nudgeState().mode)
	logNudgeDelivery(r.sender, r.target, r.message, rigName)
	return nil
}

func resolveNudgeAddressSession(t *tmux.Tmux, rigName, polecatName string) (string, error) {
	if strings.HasPrefix(polecatName, "crew/") {
		return crewSessionName(rigName, strings.TrimPrefix(polecatName, "crew/")), nil
	}
	if strings.HasPrefix(polecatName, "polecats/") {
		mgr, _, err := getSessionManager(rigName)
		if err != nil {
			return "", err
		}
		return mgr.SessionName(strings.TrimPrefix(polecatName, "polecats/")), nil
	}
	crewSession := crewSessionName(rigName, polecatName)
	if exists, _ := t.HasSession(crewSession); exists {
		return crewSession, nil
	}
	mgr, _, err := getSessionManager(rigName)
	if err != nil {
		return "", err
	}
	return mgr.SessionName(polecatName), nil
}

func nudgeRawSession(r *nudgeRun) error {
	if !hasACPSessionByName(r.townRoot, r.target) {
		exists, err := nudgeTargetExists(r.t, r.townRoot, r.target)
		if err != nil {
			return fmt.Errorf("checking session: %w", err)
		}
		if !exists {
			return fmt.Errorf("session %q not found", r.target)
		}
	}
	if err := deliverNudge(r.t, r.target, r.message, r.sender); err != nil {
		return fmt.Errorf("nudging session: %w", err)
	}
	fmt.Printf("✓ Nudged %s (%s)\n", r.target, nudgeState().mode)
	logNudgeDelivery(r.sender, r.target, r.message, "")
	return nil
}

func requireNudgeSession(r *nudgeRun, sessionName, checkVerb string) error {
	if nudgeState().mode == NudgeModeImmediate || hasACPSessionByName(r.townRoot, sessionName) {
		return nil
	}
	exists, err := nudgeTargetExists(r.t, r.townRoot, sessionName)
	if err != nil {
		return fmt.Errorf("%s: %w", checkVerb, err)
	}
	if !exists {
		return fmt.Errorf("session %q not found (cannot queue nudge for nonexistent session)", sessionName)
	}
	return nil
}

func logNudgeDelivery(sender, logTarget, message, rigName string) {
	if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
		_ = LogNudge(townRoot, logTarget, message)
	}
	_ = events.LogFeed(events.TypeNudge, sender, events.NudgePayload(rigName, logTarget, message))
}
