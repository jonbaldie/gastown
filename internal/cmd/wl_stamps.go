package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/wasteland"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var wlStampsCmd = &cobra.Command{
	Use:   "stamps <rig-handle>",
	Short: "Query stamps for a rig",
	Args:  cobra.ExactArgs(1),
	RunE:  runWLStamps,
	Long: `Query the stamps table for a given rig handle.

Shows stamps where the rig is the subject (worker being stamped).
Use --author to filter by who issued the stamp. Use --skill, --type,
and --severity to narrow results.

EXAMPLES:
  gt wl stamps gastown                         # All stamps for gastown
  gt wl stamps gastown --author hop-mayor      # Stamps from a specific validator
  gt wl stamps gastown --skill go              # Filter by skill tag
  gt wl stamps gastown --type boot_block       # Boot block stamps only
  gt wl stamps gastown --severity branch       # Branch-level stamps
  gt wl stamps gastown --limit 10              # Show 10 stamps
  gt wl stamps gastown --json                  # JSON output`,
}

func init() {
	wlStampsCmd.Flags().String("author", "", "Filter by stamper rig handle")
	wlStampsCmd.Flags().String("skill", "", "Filter by skill tag (searches JSON array)")
	wlStampsCmd.Flags().String("type", "", "Filter by context_type (completion, endorsement, boot_block, validation_received, sandboxed_completion)")
	wlStampsCmd.Flags().String("stamp-type", "", "Filter by stamp_type (work, mentoring, peer_review, endorsement, boot_block)")
	wlStampsCmd.Flags().String("cohort", "", "Filter by pilot_cohort (andela-pilot, commbank-pilot, indie)")
	wlStampsCmd.Flags().String("severity", "", "Filter by severity (leaf, branch, root)")
	wlStampsCmd.Flags().Int("limit", 50, "Maximum stamps to display")
	wlStampsCmd.Flags().Bool("json", false, "Output as JSON")

	wlCmd.AddCommand(wlStampsCmd)
}

// StampsFilter holds filter parameters for building a stamps query.
type StampsFilter struct {
	Subject     string
	Author      string
	Skill       string
	ContextType string
	StampType   string
	PilotCohort string
	Severity    string
	Limit       int
}

func buildStampsQuery(f StampsFilter) string {
	var conditions []string

	conditions = append(conditions, fmt.Sprintf("subject = '%s'", doltserver.EscapeSQL(f.Subject)))

	if f.Author != "" {
		conditions = append(conditions, fmt.Sprintf("author = '%s'", doltserver.EscapeSQL(f.Author)))
	}
	if f.ContextType != "" {
		conditions = append(conditions, fmt.Sprintf("context_type = '%s'", doltserver.EscapeSQL(f.ContextType)))
	}
	if f.StampType != "" {
		conditions = append(conditions, fmt.Sprintf("stamp_type = '%s'", doltserver.EscapeSQL(f.StampType)))
	}
	if f.PilotCohort != "" {
		conditions = append(conditions, fmt.Sprintf("pilot_cohort = '%s'", doltserver.EscapeSQL(f.PilotCohort)))
	}
	if f.Severity != "" {
		conditions = append(conditions, fmt.Sprintf("severity = '%s'", doltserver.EscapeSQL(f.Severity)))
	}
	if f.Skill != "" {
		// JSON_CONTAINS checks if the skill_tags array contains the given value
		conditions = append(conditions, fmt.Sprintf("JSON_CONTAINS(skill_tags, '\"%s\"')", doltserver.EscapeSQL(f.Skill)))
	}

	query := "SELECT id, author, subject, valence, confidence, severity, context_id, context_type, skill_tags, message, created_at FROM stamps"
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY created_at DESC"
	query += fmt.Sprintf(" LIMIT %d", f.Limit)

	return query
}

func runWLStamps(cmd *cobra.Command, args []string) error {
	options := readWlStampsOptions(cmd, args[0])

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Fast path: query through the Dolt server if the database is registered.
	dbName := wasteland.ResolveDBName(townRoot)
	if doltserver.DatabaseExists(townRoot, dbName) {
		return queryWlStampsServer(townRoot, dbName, options)
	}

	// Fallback: read from local filesystem clone.
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		return fmt.Errorf("dolt not found in PATH — install from https://docs.dolthub.com/introduction/installation")
	}

	cloneDir, cleanup, err := resolveWlStampsClone(doltPath, townRoot, options.jsonOutput)
	if err != nil {
		return err
	}
	defer cleanup()

	query := buildStampsQuery(options.filter)

	if options.jsonOutput {
		return queryWlStampsJSON(doltPath, cloneDir, query)
	}

	return renderStampsTable(doltPath, cloneDir, query, options.filter.Subject)
}

type wlStampsOptions struct {
	filter     StampsFilter
	jsonOutput bool
}

func readWlStampsOptions(cmd *cobra.Command, rig string) wlStampsOptions {
	return wlStampsOptions{
		filter: StampsFilter{
			Subject:     rig,
			Author:      commandStringFlag(cmd, "author"),
			Skill:       commandStringFlag(cmd, "skill"),
			ContextType: commandStringFlag(cmd, "type"),
			StampType:   commandStringFlag(cmd, "stamp-type"),
			PilotCohort: commandStringFlag(cmd, "cohort"),
			Severity:    commandStringFlag(cmd, "severity"),
			Limit:       commandIntFlag(cmd, "limit"),
		},
		jsonOutput: commandBoolFlag(cmd, "json"),
	}
}

func queryWlStampsServer(townRoot, dbName string, options wlStampsOptions) error {
	serverQuery := fmt.Sprintf("USE %s; %s", dbName, buildStampsQuery(options.filter))
	output, err := doltserver.QueryJSON(townRoot, serverQuery)
	if err != nil {
		return err
	}
	if options.jsonOutput {
		fmt.Print(output)
		return nil
	}
	return renderStampsRows([]byte(output), options.filter.Subject)
}

func resolveWlStampsClone(doltPath, townRoot string, jsonOutput bool) (string, func(), error) {
	if cloneDir := configuredWlStampsClone(townRoot); cloneDir != "" {
		return cloneDir, func() {}, nil
	}
	if cloneDir := findWLCommonsFork(townRoot); cloneDir != "" {
		return cloneDir, func() {}, nil
	}
	return cloneWlStampsDatabase(doltPath, townRoot, jsonOutput)
}

func configuredWlStampsClone(townRoot string) string {
	cfg, err := wasteland.LoadConfig(townRoot)
	if err != nil || cfg.LocalDir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(cfg.LocalDir, ".dolt")); err != nil {
		return ""
	}
	return cfg.LocalDir
}

func cloneWlStampsDatabase(doltPath, townRoot string, jsonOutput bool) (string, func(), error) {
	commonsOrg, commonsDB := wlStampsRemote(townRoot)
	tmpDir, err := os.MkdirTemp("", "wl-stamps-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating temp directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	cloneDir := filepath.Join(tmpDir, commonsDB)
	remote := fmt.Sprintf("%s/%s", commonsOrg, commonsDB)
	if !jsonOutput {
		fmt.Printf("Cloning %s...\n", style.Bold.Render(remote))
	}

	cloneCmd := exec.Command(doltPath, "clone", remote, cloneDir)
	if !jsonOutput {
		cloneCmd.Stderr = os.Stderr
	}
	if err := cloneCmd.Run(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("cloning %s: %w\nEnsure the database exists on DoltHub: https://www.dolthub.com/%s", remote, err, remote)
	}
	if !jsonOutput {
		fmt.Printf("%s Cloned successfully\n\n", style.Bold.Render("✓"))
	}
	return cloneDir, cleanup, nil
}

func wlStampsRemote(townRoot string) (string, string) {
	commonsOrg := "hop"
	commonsDB := "wl-commons"
	cfg, err := wasteland.LoadConfig(townRoot)
	if err == nil && cfg.Upstream != "" {
		if org, db, parseErr := wasteland.ParseUpstream(cfg.Upstream); parseErr == nil {
			commonsOrg = org
			commonsDB = db
		}
	}
	return commonsOrg, commonsDB
}

func queryWlStampsJSON(doltPath, cloneDir, query string) error {
	sqlCmd := exec.Command(doltPath, "sql", "-q", query, "-r", "json")
	sqlCmd.Dir = cloneDir
	sqlCmd.Stdout = os.Stdout
	sqlCmd.Stderr = os.Stderr
	return sqlCmd.Run()
}

func renderStampsTable(doltPath, cloneDir, query, rig string) error {
	// Use JSON output for richer parsing of nested fields (valence, skill_tags)
	sqlCmd := exec.Command(doltPath, "sql", "-q", query, "-r", "json")
	sqlCmd.Dir = cloneDir
	output, err := sqlCmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("query failed: %s", string(exitErr.Stderr))
		}
		return fmt.Errorf("running query: %w", err)
	}

	return renderStampsRows(output, rig)
}

func renderStampsRows(output []byte, rig string) error {
	var result struct {
		Rows []map[string]interface{} `json:"rows"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if len(result.Rows) == 0 {
		fmt.Printf("No stamps found for rig %q.\n", rig)
		return nil
	}

	tbl := style.NewTable(
		style.Column{Name: "ID", Width: 16},
		style.Column{Name: "AUTHOR", Width: 20},
		style.Column{Name: "VALENCE", Width: 28},
		style.Column{Name: "CONF", Width: 5, Align: style.AlignRight},
		style.Column{Name: "SEVERITY", Width: 8},
		style.Column{Name: "TYPE", Width: 14},
		style.Column{Name: "SKILLS", Width: 18},
		style.Column{Name: "DATE", Width: 10},
	)

	for _, row := range result.Rows {
		id := getString(row, "id")
		author := getString(row, "author")
		valence := formatValence(row["valence"])
		conf := getString(row, "confidence")
		severity := getString(row, "severity")
		ctxType := getString(row, "context_type")
		skills := formatSkillTags(row["skill_tags"])
		date := formatStampDate(getString(row, "created_at"))

		tbl.AddRow(id, author, valence, conf, severity, ctxType, skills, date)
	}

	fmt.Printf("Stamps for %s (%d):\n\n", style.Bold.Render(rig), len(result.Rows))
	fmt.Print(tbl.Render())

	return nil
}

// formatValence renders the valence JSON into a compact human-readable string.
// e.g. {"quality": 4, "reliability": 3, "creativity": 2} → "Q:4 R:3 C:2"
func formatValence(v interface{}) string {
	if v == nil {
		return ""
	}

	var valMap map[string]interface{}

	switch val := v.(type) {
	case string:
		if err := json.Unmarshal([]byte(val), &valMap); err != nil {
			return val
		}
	case map[string]interface{}:
		valMap = val
	default:
		return fmt.Sprintf("%v", v)
	}

	var parts []string
	for _, pair := range []struct {
		key   string
		label string
	}{
		{"quality", "Q"},
		{"reliability", "R"},
		{"creativity", "C"},
		{"volume", "V"},
	} {
		if score, ok := valMap[pair.key]; ok {
			parts = append(parts, fmt.Sprintf("%s:%.0f", pair.label, toFloat(score)))
		}
	}

	if len(parts) == 0 {
		// Fallback: show all keys
		for k, score := range valMap {
			parts = append(parts, fmt.Sprintf("%s:%.0f", k, toFloat(score)))
		}
	}

	return strings.Join(parts, " ")
}

// formatSkillTags renders the skill_tags JSON array into a comma-separated string.
func formatSkillTags(v interface{}) string {
	if v == nil {
		return ""
	}

	switch val := v.(type) {
	case string:
		var tags []string
		if err := json.Unmarshal([]byte(val), &tags); err != nil {
			return val
		}
		return strings.Join(tags, ", ")
	case []interface{}:
		var tags []string
		for _, t := range val {
			tags = append(tags, fmt.Sprintf("%v", t))
		}
		return strings.Join(tags, ", ")
	default:
		return fmt.Sprintf("%v", v)
	}
}

// formatStampDate extracts YYYY-MM-DD from a timestamp string.
func formatStampDate(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// toFloat converts a JSON number to float64.
func toFloat(v interface{}) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case int:
		return float64(val)
	default:
		return 0
	}
}
