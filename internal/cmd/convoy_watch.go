package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

func init() {
	convoyWatchCmd.Flags().Bool("nudge", false, "Subscribe for nudge notification instead of mail")
	convoyWatchCmd.Flags().String("addr", "", "Address to notify (default: caller's identity)")
	convoyWatchCmd.Flags().Bool("json", false, "Output as JSON")

	convoyUnwatchCmd.Flags().String("addr", "", "Address to remove (default: caller's identity)")

	convoyCmd.AddCommand(convoyWatchCmd)
	convoyCmd.AddCommand(convoyUnwatchCmd)
}

var convoyWatchCmd = &cobra.Command{
	Use:   "watch <convoy-id>",
	Short: "Subscribe to convoy completion notifications",
	Long: `Subscribe to be notified when a convoy completes (all tracked issues close).

By default, sends a mail notification to the caller's identity when the
convoy lands. Use --nudge for lightweight nudge notifications instead.

The watcher list is stored in the convoy's description fields and processed
by notifyConvoyCompletion when the convoy closes.

Examples:
  gt convoy watch hq-cv-abc                    # Mail notification to caller
  gt convoy watch hq-cv-abc --nudge            # Nudge notification to caller
  gt convoy watch hq-cv-abc --addr gastown/crew/mel  # Mail notification to mel
  gt convoy watch hq-cv-abc --nudge --addr mayor/    # Nudge mayor on completion`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runConvoyWatch,
}

var convoyUnwatchCmd = &cobra.Command{
	Use:   "unwatch <convoy-id>",
	Short: "Unsubscribe from convoy completion notifications",
	Long: `Remove yourself (or a specified address) from a convoy's watcher list.

Removes from both mail and nudge watcher lists.

Examples:
  gt convoy unwatch hq-cv-abc                        # Remove caller from watchers
  gt convoy unwatch hq-cv-abc --addr gastown/crew/mel # Remove mel from watchers`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runConvoyUnwatch,
}

type convoyWatchOptions struct {
	nudge bool
	addr  string
	json  bool
}

func readConvoyWatchOptions(cmd *cobra.Command) (convoyWatchOptions, error) {
	addr, err := cmd.Flags().GetString("addr")
	if err != nil {
		return convoyWatchOptions{}, err
	}
	opts := convoyWatchOptions{addr: addr}
	if cmd.Flags().Lookup("nudge") != nil {
		opts.nudge, err = cmd.Flags().GetBool("nudge")
		if err != nil {
			return convoyWatchOptions{}, err
		}
		opts.json, err = cmd.Flags().GetBool("json")
		if err != nil {
			return convoyWatchOptions{}, err
		}
	}
	return opts, nil
}

func runConvoyWatch(cmd *cobra.Command, args []string) error {
	watch, err := loadConvoyWatch(cmd, args[0])
	if err != nil {
		return err
	}

	fields := beads.ParseConvoyFields(&beads.Issue{Description: watch.convoy.Description})
	if fields == nil {
		fields = &beads.ConvoyFields{}
	}

	// Add watcher
	added, watchType := addConvoyWatcher(fields, watch.addr, watch.opts.nudge)

	if !added {
		printWatchResult(watch.convoyID, watch.addr, watchType, "already_watching", watch.opts.json, watch.opts.nudge)
		return nil
	}

	newDesc := beads.SetConvoyFields(&beads.Issue{Description: watch.convoy.Description}, fields)
	if err := updateConvoyDescription(watch.townBeads, watch.convoyID, newDesc); err != nil {
		return fmt.Errorf("updating convoy watchers: %w", err)
	}

	printWatchResult(watch.convoyID, watch.addr, watchType, "subscribed", watch.opts.json, watch.opts.nudge)

	return nil
}

func runConvoyUnwatch(cmd *cobra.Command, args []string) error {
	watch, err := loadConvoyWatch(cmd, args[0])
	if err != nil {
		return err
	}

	fields := beads.ParseConvoyFields(&beads.Issue{Description: watch.convoy.Description})
	if fields == nil {
		fmt.Printf("%s %s is not watching convoy %s\n", style.Dim.Render("○"), watch.addr, watch.convoyID)
		return nil
	}

	removedMail := fields.RemoveWatcher(watch.addr)
	removedNudge := fields.RemoveNudgeWatcher(watch.addr)

	if !removedMail && !removedNudge {
		fmt.Printf("%s %s is not watching convoy %s\n", style.Dim.Render("○"), watch.addr, watch.convoyID)
		return nil
	}

	newDesc := beads.SetConvoyFields(&beads.Issue{Description: watch.convoy.Description}, fields)
	if err := updateConvoyDescription(watch.townBeads, watch.convoyID, newDesc); err != nil {
		return fmt.Errorf("updating convoy watchers: %w", err)
	}

	var types []string
	if removedMail {
		types = append(types, "mail")
	}
	if removedNudge {
		types = append(types, "nudge")
	}
	fmt.Printf("🔕 %s unsubscribed from convoy %s (%s)\n", watch.addr, watch.convoyID, strings.Join(types, "+"))

	return nil
}

type loadConvoyWatchResult struct {
	opts      convoyWatchOptions
	convoyID  string
	addr      string
	townBeads string
	convoy    *convoyForWatch
}

func loadConvoyWatch(cmd *cobra.Command, rawID string) (loadConvoyWatchResult, error) {
	opts, err := readConvoyWatchOptions(cmd)
	if err != nil {
		return loadConvoyWatchResult{}, err
	}
	convoyID, err := resolveWatchedConvoy(rawID)
	if err != nil {
		return loadConvoyWatchResult{}, err
	}
	addr := watcherAddress(opts.addr)
	if addr == "" {
		return loadConvoyWatchResult{}, fmt.Errorf("could not determine caller identity; use --addr to specify")
	}
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return loadConvoyWatchResult{}, err
	}
	convoy, err := getConvoyForWatch(townBeads, convoyID)
	if err != nil {
		return loadConvoyWatchResult{}, err
	}
	return loadConvoyWatchResult{opts: opts, convoyID: convoyID, addr: addr, townBeads: townBeads, convoy: convoy}, nil
}

func resolveWatchedConvoy(convoyID string) (string, error) {
	n, err := strconv.Atoi(convoyID)
	if err != nil || n <= 0 {
		return convoyID, nil
	}
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return "", err
	}
	return resolveConvoyNumber(townBeads, n)
}

func watcherAddress(addr string) string {
	if addr != "" {
		return addr
	}
	return detectSender()
}

func addConvoyWatcher(fields *beads.ConvoyFields, addr string, nudge bool) (bool, string) {
	if nudge {
		return fields.AddNudgeWatcher(addr), "nudge"
	}
	return fields.AddWatcher(addr), "mail"
}

func printWatchResult(convoyID, addr, watchType, status string, jsonOutput, nudge bool) {
	if jsonOutput {
		out, _ := json.Marshal(map[string]interface{}{
			"convoy_id":  convoyID,
			"address":    addr,
			"watch_type": watchType,
			"status":     status,
		})
		fmt.Println(string(out))
		return
	}
	if status == "already_watching" {
		fmt.Printf("%s %s is already watching convoy %s (%s)\n", style.Dim.Render("○"), addr, convoyID, watchType)
		return
	}
	emoji := "📬"
	if nudge {
		emoji = "🔔"
	}
	fmt.Printf("%s %s subscribed to convoy %s (%s notification)\n", emoji, addr, convoyID, watchType)
}

// convoyForWatch is a minimal convoy struct for watch operations.
type convoyForWatch struct {
	ID          string
	Title       string
	Status      string
	Type        string
	Description string
}

// getConvoyForWatch fetches and validates a convoy for watch/unwatch operations.
func getConvoyForWatch(townBeads, convoyID string) (*convoyForWatch, error) {
	showCmd := beads.Spawn("show", convoyID, "--json")
	showCmd.Dir = townBeads
	var stdout bytes.Buffer
	showCmd.Stdout = &stdout

	if err := showCmd.Run(); err != nil {
		return nil, fmt.Errorf("convoy '%s' not found", convoyID)
	}

	var convoys []struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Status      string   `json:"status"`
		Type        string   `json:"issue_type"`
		Description string   `json:"description"`
		Labels      []string `json:"labels"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &convoys); err != nil {
		return nil, fmt.Errorf("parsing convoy data: %w", err)
	}

	if len(convoys) == 0 {
		return nil, fmt.Errorf("convoy '%s' not found", convoyID)
	}

	c := convoys[0]
	if !isConvoyIssue(c.Type, c.Labels) {
		return nil, fmt.Errorf("'%s' is not a convoy (type: %s)", convoyID, c.Type)
	}

	return &convoyForWatch{
		ID:          c.ID,
		Title:       c.Title,
		Status:      c.Status,
		Type:        c.Type,
		Description: c.Description,
	}, nil
}

// updateConvoyDescription updates a convoy's description via bd update.
func updateConvoyDescription(townBeads, convoyID, newDesc string) error {
	updateCmd := beads.Spawn("update", convoyID, "--description", newDesc)
	updateCmd.Dir = townBeads
	var stderr bytes.Buffer
	updateCmd.Stderr = &stderr

	if err := updateCmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("bd update: %s", errMsg)
		}
		return fmt.Errorf("bd update: %w", err)
	}
	return nil
}
