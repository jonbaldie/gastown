package session

import (
	"time"

	"github.com/jonbaldie/gastown/internal/config"
	"github.com/jonbaldie/gastown/internal/tmux"
)

// TmuxOps is the tmux seam used by StartSession and StopSession.
// Production code passes *tmux.Tmux. Tests pass a stand-in.
type TmuxOps interface {
	HasSession(_ string) (bool, error)
	IsAgentAlive(_ string) bool
	KillSessionWithProcesses(_ string) error
	NewSessionWithCommandAndEnv(_, _, _ string, _ map[string]string) error
	SetRemainOnExit(_ string, _ bool) error
	SetEnvironment(_, _, _ string) error
	GetPaneID(_ string) (string, error)
	GetPanePID(_ string) (string, error)
	ConfigureGasTownSession(_ string, _ *tmux.Theme, _, _, _ string) error
	WaitForCommand(_ string, _ []string, _ time.Duration) error
	SetAutoRespawnHook(_ string) error
	AcceptStartupDialogs(_ string) error
	CheckStartupBlocked(_ string) error
	WaitForRuntimeReady(_ string, _ *config.RuntimeConfig, _ time.Duration) error
	CheckSessionHealth(_ string, _ time.Duration) tmux.ZombieStatus
	SendKeysRaw(_, _ string) error
}
