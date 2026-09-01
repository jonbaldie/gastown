package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

type agentStateOptions struct {
	set  []string
	incr string
	del  []string
	json bool
}

const agentStateLongDescription = `Get or set label-based operational state on agent beads.

Agent beads store operational state (like idle cycle counts) as labels.
This command provides a convenient interface for reading and modifying
these labels without affecting other bead properties.

LABEL FORMAT:
Labels are stored as key:value pairs (e.g., idle:3, backoff:2m).

OPERATIONS:
  Get all labels (default):
    gt agents state <agent-bead>

  Set a label:
    gt agents state <agent-bead> --set idle=0
    gt agents state <agent-bead> --set idle=0 --set backoff=30s

  Increment a numeric label:
    gt agents state <agent-bead> --incr idle
    (Creates label with value 1 if not present)

  Delete a label:
    gt agents state <agent-bead> --del idle

COMMON LABELS:
  idle:<n>           - Consecutive idle patrol cycles
  backoff:<duration> - Current backoff interval
  last_activity:<ts> - Last activity timestamp

EXAMPLES:
  # Check current idle count
  gt agents state gt-gastown-witness

  # Reset idle counter after finding work
  gt agents state gt-gastown-witness --set idle=0

  # Increment idle counter on timeout
  gt agents state gt-gastown-witness --incr idle

  # Get state as JSON
  gt agents state gt-gastown-witness --json`

func newAgentStateCommand() *cobra.Command {
	opts := &agentStateOptions{}
	cmd := &cobra.Command{
		Use:   "state <agent-bead>",
		Short: "Get or set operational state on agent beads",
		Long:  agentStateLongDescription,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runAgentState(opts, args)
		},
	}
	cmd.Flags().StringArrayVar(&opts.set, "set", nil,
		"Set label value (format: key=value, repeatable)")
	cmd.Flags().StringVar(&opts.incr, "incr", "",
		"Increment numeric label (creates with value 1 if missing)")
	cmd.Flags().StringArrayVar(&opts.del, "del", nil,
		"Delete label (repeatable)")
	cmd.Flags().BoolVar(&opts.json, "json", false,
		"Output as JSON")
	return cmd
}

func init() {
	agentsCmd.AddCommand(newAgentStateCommand())
}

// agentStateResult holds the state query result.
type agentStateResult struct {
	AgentBead string            `json:"agent_bead"`
	Labels    map[string]string `json:"labels"`
}

func runAgentState(opts *agentStateOptions, args []string) error {
	agentBead := args[0]

	beadsDir, err := resolveAgentTrackingBeadsDir()
	if err != nil {
		return fmt.Errorf("not in a beads workspace: %w", err)
	}

	// Determine operation mode
	hasSet := len(opts.set) > 0
	hasIncr := opts.incr != ""
	hasDel := len(opts.del) > 0

	if hasSet || hasIncr || hasDel {
		// Modification mode
		return modifyAgentState(agentBead, beadsDir, opts)
	}

	// Query mode
	return queryAgentState(agentBead, beadsDir, opts.json)
}

// queryAgentState retrieves and displays labels from an agent bead.
func queryAgentState(agentBead, beadsDir string, outputJSON bool) error {
	labels, err := getAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	result := &agentStateResult{
		AgentBead: agentBead,
		Labels:    labels,
	}

	if outputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	// Human-readable output
	fmt.Printf("%s Agent: %s\n\n", style.Bold.Render("📊"), agentBead)

	if len(labels) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(no operational state labels)"))
		return nil
	}

	for key, value := range labels {
		fmt.Printf("  %s: %s\n", key, value)
	}

	return nil
}

// modifyAgentState modifies labels on an agent bead.
// Uses read-modify-write pattern: read current labels, apply changes, write back all.
func modifyAgentState(agentBead, beadsDir string, opts *agentStateOptions) error {
	// Read current labels
	labels, err := getAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	// Also get non-state labels (ones without : separator) to preserve them
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	if err := applyAgentStateOperations(labels, opts); err != nil {
		return err
	}

	finalLabels := buildAgentStateLabels(allLabels, labels)
	if err := updateAgentStateLabels(agentBead, beadsDir, finalLabels); err != nil {
		return err
	}
	fmt.Printf("%s Updated agent state for %s\n", style.Bold.Render("✓"), agentBead)

	return nil
}

func applyAgentStateOperations(labels map[string]string, opts *agentStateOptions) error {
	if opts.incr != "" {
		incrementAgentStateLabel(labels, opts.incr)
	}
	if err := setAgentStateLabels(labels, opts.set); err != nil {
		return err
	}
	deleteAgentStateLabels(labels, opts.del)
	return nil
}

func incrementAgentStateLabel(labels map[string]string, key string) {
	currentValue := 0
	if valStr, ok := labels[key]; ok {
		if value, err := strconv.Atoi(valStr); err == nil {
			currentValue = value
		}
	}
	labels[key] = strconv.Itoa(currentValue + 1)
}

func setAgentStateLabels(labels map[string]string, operations []string) error {
	for _, operation := range operations {
		parts := strings.SplitN(operation, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid set format: %s (expected key=value)", operation)
		}
		labels[parts[0]] = parts[1]
	}
	return nil
}

func deleteAgentStateLabels(labels map[string]string, keys []string) {
	for _, key := range keys {
		delete(labels, key)
	}
}

func buildAgentStateLabels(allLabels []string, labels map[string]string) []string {
	finalLabels := make([]string, 0, len(allLabels)+len(labels))
	for _, label := range allLabels {
		if !strings.Contains(label, ":") {
			finalLabels = append(finalLabels, label)
		}
	}
	for key, value := range labels {
		finalLabels = append(finalLabels, key+":"+value)
	}
	return finalLabels
}

func updateAgentStateLabels(agentBead, beadsDir string, labels []string) error {
	args := []string{"update", agentBead}
	for _, label := range labels {
		args = append(args, "--set-labels="+label)
	}
	if len(labels) == 0 {
		args = append(args, "--set-labels=")
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()
	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.MutationPinned, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return formatAgentStateUpdateError(err, stderr.String())
	}
	return nil
}

func formatAgentStateUpdateError(err error, stderr string) error {
	errMsg := strings.TrimSpace(stderr)
	if errMsg != "" {
		return fmt.Errorf("%s", errMsg)
	}
	return fmt.Errorf("updating agent state: %w", err)
}

// getAgentLabels retrieves state labels from an agent bead.
// Returns only labels in key:value format, parsed into a map.
// State labels are those with a : separator (e.g., idle:3, backoff:2m).
func getAgentLabels(agentBead, beadsDir string) (map[string]string, error) {
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return nil, err
	}

	// Parse state labels (those with : separator) into key:value map
	labels := make(map[string]string)
	for _, label := range allLabels {
		parts := strings.SplitN(label, ":", 2)
		if len(parts) == 2 {
			labels[parts[0]] = parts[1]
		}
	}

	return labels, nil
}

// bdCallTimeout is the per-call timeout for bd subprocess invocations in agent-bead
// helpers. bd commands should be fast against a local Dolt server, but can hang
// indefinitely if Dolt is unresponsive (e.g., connection pool exhausted). A 30s
// ceiling prevents await-event/await-signal from stalling past the patrol timeout.
const bdCallTimeout = 30 * time.Second

// getAllAgentLabels retrieves all labels (including non-state) from an agent bead.
func getAllAgentLabels(agentBead, beadsDir string) ([]string, error) {
	args := []string{"show", agentBead, "--json"}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.ReadOnlyPinned, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if strings.Contains(errMsg, "not found") {
			return nil, fmt.Errorf("agent bead not found: %s", agentBead)
		}
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return nil, fmt.Errorf("querying agent bead: %w", err)
	}

	return parseAgentBeadLabels(stdout.Bytes(), stderr.Bytes(), agentBead)
}

// parseAgentBeadLabels parses the JSON output from bd show --json and extracts labels.
// This is separated from getAllAgentLabels to enable unit testing.
func parseAgentBeadLabels(stdout, stderr []byte, agentBead string) ([]string, error) {
	// Check for empty stdout before parsing - can happen with daemon mismatch
	// or other errors that don't set exit code
	if len(stdout) == 0 {
		errMsg := strings.TrimSpace(string(stderr))
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return nil, fmt.Errorf("agent bead query returned no output: %s", agentBead)
	}

	// Parse JSON output - bd show --json returns an array
	var issues []struct {
		Labels []string `json:"labels"`
	}

	if err := json.Unmarshal(stdout, &issues); err != nil {
		return nil, fmt.Errorf("parsing agent bead response: %w", err)
	}

	if len(issues) == 0 {
		return nil, fmt.Errorf("agent bead not found: %s", agentBead)
	}

	return issues[0].Labels, nil
}
