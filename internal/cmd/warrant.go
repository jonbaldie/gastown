package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// Warrant represents a death warrant for an agent
type Warrant struct {
	ID         string     `json:"id"`
	Target     string     `json:"target"` // e.g., "gastown/polecats/alpha", "deacon/dogs/bravo"
	Reason     string     `json:"reason"`
	FiledBy    string     `json:"filed_by"`
	FiledAt    time.Time  `json:"filed_at"`
	Executed   bool       `json:"executed,omitempty"`
	ExecutedAt *time.Time `json:"executed_at,omitempty"`
}

var warrantCmd = &cobra.Command{
	Use:   "warrant",
	Short: "Manage death warrants for stuck agents",
	Long: `Manage death warrants for agents that need termination.

Death warrants are filed when an agent is stuck, unresponsive, or needs
forced termination. Boot handles warrant execution during triage cycles.

The warrant system provides a controlled way to terminate agents:
1. Deacon/Witness files a warrant with a reason
2. Boot picks up the warrant during triage
3. Boot executes the warrant (terminates session, updates state)
4. Warrant is marked as executed

Warrants are stored in ~/gt/warrants/ as JSON files.`,
}

var warrantFileCmd = &cobra.Command{
	Use:   "file <target>",
	Short: "File a death warrant for an agent",
	Long: `File a death warrant for an agent that needs termination.

The target should be an agent path like:
  - gastown/polecats/alpha
  - deacon/dogs/bravo
  - beads/polecats/charlie

Examples:
  gt warrant file gastown/polecats/alpha --reason "Zombie: no session, idle >10m"
  gt warrant file deacon/dogs/bravo --reason "Stuck: working on task for >2h"`,
	Args: cobra.ExactArgs(1),
	RunE: runWarrantFile,
}

var warrantListCmd = &cobra.Command{
	Use:   "list",
	Short: "List pending warrants",
	Long: `List all pending (unexecuted) warrants.

Use --all to include executed warrants.

Examples:
  gt warrant list
  gt warrant list --all`,
	RunE: runWarrantList,
}

var warrantExecuteCmd = &cobra.Command{
	Use:   "execute <target>",
	Short: "Execute a warrant (terminate agent)",
	Long: `Execute a pending warrant for the specified target.

This will:
1. Find the warrant for the target
2. Terminate the agent's tmux session (if exists)
3. Mark the warrant as executed

Use --force to execute even if no warrant exists.

Examples:
  gt warrant execute gastown/polecats/alpha
  gt warrant execute deacon/dogs/bravo --force`,
	Args: cobra.ExactArgs(1),
	RunE: runWarrantExecute,
}

func init() {
	// File flags
	warrantFileCmd.Flags().StringP("reason", "r", "", "Reason for the warrant (required unless --stdin)")
	warrantFileCmd.Flags().Bool("stdin", false, "Read reason from stdin (avoids shell quoting issues)")

	// List flags
	warrantListCmd.Flags().BoolP("all", "a", false, "Include executed warrants")

	// Execute flags
	warrantExecuteCmd.Flags().BoolP("force", "f", false, "Execute even without a warrant")

	warrantCmd.AddCommand(warrantFileCmd)
	warrantCmd.AddCommand(warrantListCmd)
	warrantCmd.AddCommand(warrantExecuteCmd)

	rootCmd.AddCommand(warrantCmd)
}

// getWarrantDir returns the warrants directory path
func getWarrantDir() (string, error) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return "", fmt.Errorf("finding town root: %w", err)
	}
	return filepath.Join(townRoot, "warrants"), nil
}

// warrantFilePath returns the path for a warrant file
func warrantFilePath(dir, target string) string {
	// Replace / with _ for filename safety
	safe := strings.ReplaceAll(target, "/", "_")
	return filepath.Join(dir, safe+".warrant.json")
}

func runWarrantFile(cmd *cobra.Command, args []string) error {
	reason, err := warrantReason(cmd)
	if err != nil {
		return err
	}
	target := args[0]

	warrantDir, err := getWarrantDir()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(warrantDir, 0755); err != nil {
		return fmt.Errorf("creating warrants directory: %w", err)
	}

	warrantPath := warrantFilePath(warrantDir, target)
	if existing, ok := readPendingWarrant(warrantPath); ok {
		printExistingWarrant(target, existing)
		return nil
	}

	warrant := buildWarrant(target, reason)
	if err := writeWarrant(warrantPath, warrant); err != nil {
		return err
	}

	fmt.Printf("✓ Filed death warrant for %s\n", style.Bold.Render(target))
	fmt.Printf("  Reason: %s\n", reason)
	fmt.Printf("  ID: %s\n", warrant.ID)

	return nil
}

func warrantReason(cmd *cobra.Command) (string, error) {
	reason := commandStringFlag(cmd, "reason")
	if commandBoolFlag(cmd, "stdin") {
		if reason != "" {
			return "", fmt.Errorf("cannot use --stdin with --reason/-r")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		reason = strings.TrimRight(string(data), "\n")
	}
	if reason == "" {
		return "", fmt.Errorf("required flag \"reason\" not set (use --reason/-r or --stdin)")
	}
	return reason, nil
}

func readPendingWarrant(path string) (*Warrant, bool) {
	if _, err := os.Stat(path); err != nil {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var warrant Warrant
	if err := json.Unmarshal(data, &warrant); err != nil || warrant.Executed {
		return nil, false
	}
	return &warrant, true
}

func printExistingWarrant(target string, warrant *Warrant) {
	fmt.Printf("Warrant already exists for %s\n", target)
	fmt.Printf("  Reason: %s\n", warrant.Reason)
	fmt.Printf("  Filed: %s\n", warrant.FiledAt.Format(time.RFC3339))
}

func buildWarrant(target, reason string) Warrant {
	filedBy := os.Getenv("BD_ACTOR")
	if filedBy == "" {
		filedBy = "unknown"
	}
	now := time.Now()
	return Warrant{
		ID:      fmt.Sprintf("warrant-%d", now.UnixMilli()),
		Target:  target,
		Reason:  reason,
		FiledBy: filedBy,
		FiledAt: now,
	}
}

func writeWarrant(path string, warrant Warrant) error {
	data, err := json.MarshalIndent(warrant, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling warrant: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing warrant: %w", err)
	}
	return nil
}

func runWarrantList(cmd *cobra.Command, _ []string) error {
	showAll := commandBoolFlag(cmd, "all")
	warrantDir, err := getWarrantDir()
	if err != nil {
		return err
	}

	warrants, err := loadWarrants(warrantDir, showAll)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No warrants filed")
			return nil
		}
		return err
	}

	if len(warrants) == 0 {
		if showAll {
			fmt.Println("No warrants found")
		} else {
			fmt.Println("No pending warrants")
		}
		return nil
	}

	printWarrants(warrants)

	return nil
}

func loadWarrants(warrantDir string, showAll bool) ([]Warrant, error) {
	entries, err := os.ReadDir(warrantDir)
	if err != nil {
		return nil, fmt.Errorf("reading warrants directory: %w", err)
	}
	var warrants []Warrant
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".warrant.json") {
			continue
		}
		warrant, ok := loadWarrantFile(filepath.Join(warrantDir, entry.Name()))
		if ok && (showAll || !warrant.Executed) {
			warrants = append(warrants, warrant)
		}
	}
	return warrants, nil
}

func loadWarrantFile(path string) (Warrant, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Warrant{}, false
	}
	var warrant Warrant
	if err := json.Unmarshal(data, &warrant); err != nil {
		return Warrant{}, false
	}
	return warrant, true
}

func printWarrants(warrants []Warrant) {
	fmt.Println(style.Bold.Render("Death Warrants"))
	fmt.Println()
	for _, warrant := range warrants {
		printWarrant(warrant)
	}
}

func printWarrant(w Warrant) {
	status := "⚠️  PENDING"
	if w.Executed {
		status = "✓ EXECUTED"
	}
	fmt.Printf("  %s %s\n", status, style.Bold.Render(w.Target))
	fmt.Printf("     Reason: %s\n", w.Reason)
	fmt.Printf("     Filed: %s by %s\n", w.FiledAt.Format("2006-01-02 15:04"), w.FiledBy)
	if w.Executed && w.ExecutedAt != nil {
		fmt.Printf("     Executed: %s\n", w.ExecutedAt.Format("2006-01-02 15:04"))
	}
	fmt.Println()
}

func runWarrantExecute(cmd *cobra.Command, args []string) error {
	force := commandBoolFlag(cmd, "force")
	target := args[0]

	warrantDir, err := getWarrantDir()
	if err != nil {
		return err
	}

	warrantPath := warrantFilePath(warrantDir, target)
	warrant := loadWarrant(warrantPath)

	if warrant == nil && !force {
		return fmt.Errorf("no warrant found for %s (use --force to execute anyway)", target)
	}

	if warrant != nil && warrant.Executed {
		fmt.Printf("Warrant for %s already executed at %s\n", target, warrant.ExecutedAt.Format(time.RFC3339))
		return nil
	}

	tm := tmux.NewTmux()

	if warrant != nil {
		if err := executeOneWarrant(warrant, warrantPath, tm); err != nil {
			return fmt.Errorf("executing warrant: %w", err)
		}
	} else {
		if err := executeForcedWarrant(target, tm); err != nil {
			return err
		}
	}

	fmt.Printf("✓ Warrant executed for %s\n", style.Bold.Render(target))
	return nil
}

func loadWarrant(path string) *Warrant {
	warrant, ok := loadWarrantFile(path)
	if !ok {
		return nil
	}
	return &warrant
}

func executeForcedWarrant(target string, tm *tmux.Tmux) error {
	sessionName, err := targetToSessionName(target)
	if err != nil {
		return fmt.Errorf("determining session name: %w", err)
	}
	has, err := tm.HasSession(sessionName)
	if err != nil {
		return fmt.Errorf("checking session %s: %w", sessionName, err)
	}
	if !has {
		fmt.Printf("  Session %s not found (already dead)\n", sessionName)
		return nil
	}
	if err := tm.KillSessionWithProcesses(sessionName); err != nil {
		return fmt.Errorf("killing session %s: %w", sessionName, err)
	}
	fmt.Printf("✓ Terminated session %s\n", sessionName)
	return nil
}

// executeOneWarrant executes a single pending warrant: checks if the target
// session exists, kills it with full process tree cleanup, and marks the warrant
// as executed on disk. Returns nil on success. On error, the warrant is NOT
// marked as executed so it can be retried on the next triage cycle.
func executeOneWarrant(w *Warrant, warrantPath string, tm *tmux.Tmux) error {
	sessionName, err := targetToSessionName(w.Target)
	if err != nil {
		return fmt.Errorf("invalid target %s: %w", w.Target, err)
	}

	has, err := tm.HasSession(sessionName)
	if err != nil {
		return fmt.Errorf("checking session %s: %w", sessionName, err)
	}

	if has {
		if err := tm.KillSessionWithProcesses(sessionName); err != nil {
			return fmt.Errorf("killing session %s: %w", sessionName, err)
		}
		fmt.Printf("Warrant executed: terminated session %s (%s)\n", sessionName, w.Target)
	} else {
		fmt.Printf("Warrant executed: session %s already dead (%s)\n", sessionName, w.Target)
	}

	now := time.Now()
	w.Executed = true
	w.ExecutedAt = &now
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling warrant: %w", err)
	}
	if err := os.WriteFile(warrantPath, data, 0644); err != nil {
		return fmt.Errorf("writing warrant file: %w", err)
	}

	return nil
}

// targetToSessionName converts a target path to a tmux session name
func targetToSessionName(target string) (string, error) {
	parts := strings.Split(target, "/")
	if name, err, ok := targetSessionSpecialCase(parts); ok {
		return name, err
	}
	return defaultTargetSessionName(target, parts), nil
}

func targetSessionSpecialCase(parts []string) (string, error, bool) {
	if len(parts) == 3 {
		return targetThreePartSpecialCase(parts)
	}
	if len(parts) == 2 {
		return targetTwoPartSpecialCase(parts)
	}
	return "", nil, false
}

func targetThreePartSpecialCase(parts []string) (string, error, bool) {
	switch parts[1] {
	case "polecats":
		return session.PolecatSessionName(session.PrefixFor(parts[0]), parts[2]), nil, true
	case "crew":
		return session.CrewSessionName(session.PrefixFor(parts[0]), parts[2]), nil, true
	case "dogs":
		if parts[0] == "deacon" {
			return fmt.Sprintf("hq-dog-%s", parts[2]), nil, true
		}
	}
	return "", nil, false
}

func targetTwoPartSpecialCase(parts []string) (string, error, bool) {
	switch parts[1] {
	case "witness":
		return session.WitnessSessionName(session.PrefixFor(parts[0])), nil, true
	case "refinery":
		return session.RefinerySessionName(session.PrefixFor(parts[0])), nil, true
	case "dogs":
		if parts[0] == "deacon" {
			return "", fmt.Errorf("invalid target: need dog name (e.g., deacon/dogs/alpha)"), true
		}
	}
	return "", nil, false
}

func defaultTargetSessionName(target string, parts []string) string {
	prefix := session.DefaultPrefix
	if len(parts) > 0 {
		prefix = session.PrefixFor(parts[0])
	}
	return prefix + "-" + strings.ReplaceAll(target, "/", "-")
}
