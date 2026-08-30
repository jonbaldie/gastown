package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func sendDoltEscalationAlert(m *DoltServerManager, restartCount int) {
	subject := fmt.Sprintf("ESCALATION: Dolt server crash-looping (%d restarts)", restartCount)
	body := fmt.Sprintf(`The Dolt server has restarted %d times within %v and has been capped.

The daemon will NOT restart it again until the backoff window expires or the issue is resolved.

Possible causes:
- Bad configuration
- Corrupt data directory
- Disk full
- Port conflict

Data dir: %s
Log file: %s
Host: %s:%d

Action needed: Investigate and fix the root cause, then restart the daemon or the Dolt server manually.`,
		restartCount, m.config.RestartWindow,
		m.config.DataDir, m.config.LogFile,
		m.config.Host, m.config.Port)

	townRoot := m.townRoot
	logger := m.logger
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "gt", "mail", "send", "mayor/", "-s", subject, "-m", body) //nolint:gosec // G204: args are constructed internally
		setSysProcAttr(cmd)
		cmd.Dir = townRoot
		cmd.Env = os.Environ()

		if err := cmd.Run(); err != nil {
			logger("Warning: failed to send escalation mail to mayor: %v", err)
		} else {
			logger("Sent escalation mail to mayor about Dolt server crash-loop")
		}

		// Also notify all witnesses so they can react to degraded Dolt state
		sendDoltAlertToWitnesses(townRoot, subject, body, logger)
	}()
}

func sendDoltCrashAlert(m *DoltServerManager, deadPID int) {
	subject := "ALERT: Dolt server crashed"
	body := fmt.Sprintf(`The Dolt server (PID %d) was found dead. The daemon is restarting it.

Data dir: %s
Log file: %s
Host: %s:%d

Check the log file for crash details. If crashes recur, the daemon will escalate after %d restarts in %v.`,
		deadPID,
		m.config.DataDir, m.config.LogFile,
		m.config.Host, m.config.Port,
		m.config.MaxRestartsInWindow, m.config.RestartWindow)

	townRoot := m.townRoot
	logger := m.logger
	go func() {
		sendDoltAlertMail(townRoot, "mayor/", subject, body, logger)
		sendDoltAlertToWitnesses(townRoot, subject, body, logger)
	}()
}

func sendDoltUnhealthyAlert(m *DoltServerManager, healthErr error) {
	subject := "ALERT: Dolt server unhealthy"
	body := fmt.Sprintf(`The Dolt server is running but failing health checks. The daemon is restarting it.

Health check error: %v

Data dir: %s
Log file: %s
Host: %s:%d

This may indicate high load, connection exhaustion, or internal server errors.`,
		healthErr,
		m.config.DataDir, m.config.LogFile,
		m.config.Host, m.config.Port)

	townRoot := m.townRoot
	logger := m.logger
	go func() {
		sendDoltAlertMail(townRoot, "mayor/", subject, body, logger)
		sendDoltAlertToWitnesses(townRoot, subject, body, logger)
	}()
}

func sendDoltReadOnlyAlert(m *DoltServerManager, readOnlyErr error) {
	subject := "ALERT: Dolt server entered READ-ONLY mode"
	body := fmt.Sprintf(`The Dolt server is running but has entered read-only mode.
All write operations (beads create, update, close) will fail until the server is restarted.

The daemon is restarting the server automatically.

Error: %v

Data dir: %s
Log file: %s
Host: %s:%d

This typically occurs under heavy concurrent write load when multiple agents
contend for the storage manifest. If it recurs frequently, consider reducing
concurrent polecat count or staggering write-heavy operations.`,
		readOnlyErr,
		m.config.DataDir, m.config.LogFile,
		m.config.Host, m.config.Port)

	townRoot := m.townRoot
	logger := m.logger
	go func() {
		sendDoltAlertMail(townRoot, "mayor/", subject, body, logger)
		sendDoltAlertToWitnesses(townRoot, subject, body, logger)
	}()
}

// sendDoltAlertMail sends a Dolt alert mail to a specific recipient.
func sendDoltAlertMail(townRoot, recipient, subject, body string, logger func(format string, v ...interface{})) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gt", "mail", "send", recipient, "-s", subject, "-m", body) //nolint:gosec // G204: args are constructed internally
	setSysProcAttr(cmd)
	cmd.Dir = townRoot
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		logger("Warning: failed to send Dolt alert to %s: %v", recipient, err)
	}
}

// sendDoltAlertToWitnesses sends a Dolt alert to all rig witnesses.
// Discovers rigs from mayor/rigs.json and sends to each <rig>/witness.
func sendDoltAlertToWitnesses(townRoot, subject, body string, logger func(format string, v ...interface{})) {
	rigsPath := filepath.Join(townRoot, "mayor", "rigs.json")
	data, err := os.ReadFile(rigsPath)
	if err != nil {
		return // No rigs.json, nothing to notify
	}

	var parsed struct {
		Rigs map[string]interface{} `json:"rigs"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return
	}

	for rigName := range parsed.Rigs {
		recipient := rigName + "/witness"
		sendDoltAlertMail(townRoot, recipient, subject, body, logger)
	}
}
