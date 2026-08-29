package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jonbaldie/gastown/internal/doltserver"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/wasteland"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var wlStampCmd = &cobra.Command{
	Use:   "stamp",
	Short: "Create a reputation stamp for a rig",
	Args:  cobra.NoArgs,
	RunE:  runWlStamp,
	Long: `Create a reputation stamp — the core HOP reputation primitive.

A stamp records a validator's assessment of a worker's contribution.
The validator (you) stamps the subject (worker) with dimensional scores.

Valence scores are 0-5 integers for quality, reliability, and creativity.
Confidence is auto-computed from your validator tier if not specified.

Phase 1: writes directly to the local wl-commons database.

EXAMPLES:
  gt wl stamp --subject alice --completion c-abc123 \
    --quality 4 --reliability 5 --creativity 3 \
    --skills go,federation --stamp-type work \
    --context-type completion --evidence 'https://github.com/org/repo/pull/42'

  gt wl stamp --subject bob --quality 3 --reliability 3 --creativity 2 \
    --stamp-type peer_review --context-type endorsement \
    --message "Great code review feedback"`,
}

func init() {
	wlStampCmd.Flags().String("subject", "", "Rig handle of worker being stamped (required)")
	wlStampCmd.Flags().String("completion", "", "Completion ID this stamp references")
	wlStampCmd.Flags().Float64("quality", -1, "Quality score 0-5 (required)")
	wlStampCmd.Flags().Float64("reliability", -1, "Reliability score 0-5")
	wlStampCmd.Flags().Float64("creativity", -1, "Creativity score 0-5")
	wlStampCmd.Flags().Float64("confidence", -1, "Confidence 0.0-1.0 (auto-computed from tier if omitted)")
	wlStampCmd.Flags().String("severity", "leaf", "Severity: leaf, branch, root")
	wlStampCmd.Flags().StringSlice("skills", nil, "Skill tags (comma-separated, e.g., go,federation)")
	wlStampCmd.Flags().String("stamp-type", "work", "Stamp type: work, mentoring, peer_review, boot_block")
	wlStampCmd.Flags().String("context-type", "completion", "Context type: completion, endorsement, boot_block, validation_received, sandboxed_completion")
	wlStampCmd.Flags().String("evidence", "", "Evidence URL (PR link, SkillBench summary)")
	wlStampCmd.Flags().String("message", "", "Optional human-readable note")
	wlStampCmd.Flags().String("pilot-cohort", "", "Pilot cohort tag (andela-pilot, commbank-pilot, indie)")

	_ = wlStampCmd.MarkFlagRequired("subject")
	_ = wlStampCmd.MarkFlagRequired("quality")

	wlCmd.AddCommand(wlStampCmd)
}

type stampOptions struct {
	subject      string
	completionID string
	quality      float64
	reliability  float64
	creativity   float64
	confidence   float64
	severity     string
	skills       []string
	stampType    string
	contextType  string
	evidenceURL  string
	message      string
	pilotCohort  string
}

func stampOptionsFromCommand(cmd *cobra.Command) stampOptions {
	return stampOptions{
		subject:      commandStringFlag(cmd, "subject"),
		completionID: commandStringFlag(cmd, "completion"),
		quality:      commandFloat64Flag(cmd, "quality"),
		reliability:  commandFloat64Flag(cmd, "reliability"),
		creativity:   commandFloat64Flag(cmd, "creativity"),
		confidence:   commandFloat64Flag(cmd, "confidence"),
		severity:     commandStringFlag(cmd, "severity"),
		skills:       commandStringArrayFlag(cmd, "skills"),
		stampType:    commandStringFlag(cmd, "stamp-type"),
		contextType:  commandStringFlag(cmd, "context-type"),
		evidenceURL:  commandStringFlag(cmd, "evidence"),
		message:      commandStringFlag(cmd, "message"),
		pilotCohort:  commandStringFlag(cmd, "pilot-cohort"),
	}
}

func runWlStamp(cmd *cobra.Command, _ []string) error {
	opts := stampOptionsFromCommand(cmd)
	// Validate inputs
	if err := validateStampInputs(opts); err != nil {
		return err
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	wlCfg, err := wasteland.LoadConfig(townRoot)
	if err != nil {
		return fmt.Errorf("loading wasteland config: %w", err)
	}
	author := wlCfg.RigHandle

	if author == opts.subject {
		return fmt.Errorf("cannot stamp yourself (author=%q, subject=%q)", author, opts.subject)
	}

	// Build valence JSON
	valence := buildValenceJSON(opts.quality, opts.reliability, opts.creativity)

	// Build skill tags JSON
	skillTagsJSON := ""
	if len(opts.skills) > 0 {
		skillTagsJSON = buildSkillTagsJSON(opts.skills)
	}

	// Compute confidence (default to 0.7 if not specified — "trusted" tier)
	confidence := opts.confidence
	if confidence < 0 {
		confidence = 0.7
	}

	// Generate stamp ID from content hash
	stampID := generateStampID(author, opts.subject, valence, opts.completionID)

	stamp := &doltserver.StampRecord{
		ID:          stampID,
		Author:      author,
		Subject:     opts.subject,
		Valence:     valence,
		Confidence:  confidence,
		Severity:    opts.severity,
		ContextID:   opts.completionID,
		ContextType: opts.contextType,
		StampType:   opts.stampType,
		PilotCohort: opts.pilotCohort,
		SkillTags:   skillTagsJSON,
		Message:     opts.message,
		StampIndex:  -1, // will be computed below
	}

	dbName := wasteland.ResolveDBName(townRoot)
	if !doltserver.DatabaseExists(townRoot, dbName) {
		if wlCfg.LocalDir == "" {
			return fmt.Errorf("database %q not found\nJoin a wasteland first with: gt wl join <org/db>", dbName)
		}
		return insertStampInLocalClone(wlCfg.LocalDir, stamp)
	}

	store := doltserver.NewWLCommons(townRoot)
	if err := insertStamp(store, stamp); err != nil {
		return err
	}

	fmt.Printf("%s Stamp created\n", style.Bold.Render("✓"))
	fmt.Printf("  Stamp ID: %s\n", stampID)
	fmt.Printf("  Author: %s\n", author)
	fmt.Printf("  Subject: %s\n", opts.subject)
	fmt.Printf("  Valence: %s\n", valence)
	fmt.Printf("  Confidence: %.2f\n", confidence)
	fmt.Printf("  Severity: %s\n", opts.severity)
	fmt.Printf("  Type: %s\n", opts.stampType)
	if opts.pilotCohort != "" {
		fmt.Printf("  Cohort: %s\n", opts.pilotCohort)
	}
	if opts.completionID != "" {
		fmt.Printf("  Completion: %s\n", opts.completionID)
	}
	if stamp.StampIndex >= 0 {
		fmt.Printf("  Stamp index: %d\n", stamp.StampIndex)
	}

	return nil
}

func validateStampInputs(opts stampOptions) error {
	if opts.quality < 0 || opts.quality > 5 {
		return fmt.Errorf("quality must be 0-5 (got %.1f)", opts.quality)
	}
	if opts.reliability >= 0 && opts.reliability > 5 {
		return fmt.Errorf("reliability must be 0-5 (got %.1f)", opts.reliability)
	}
	if opts.creativity >= 0 && opts.creativity > 5 {
		return fmt.Errorf("creativity must be 0-5 (got %.1f)", opts.creativity)
	}
	if opts.confidence >= 0 && (opts.confidence < 0 || opts.confidence > 1) {
		return fmt.Errorf("confidence must be 0.0-1.0 (got %.2f)", opts.confidence)
	}

	validSeverities := map[string]bool{"leaf": true, "branch": true, "root": true}
	if !validSeverities[opts.severity] {
		return fmt.Errorf("severity must be leaf, branch, or root (got %q)", opts.severity)
	}

	validStampTypes := map[string]bool{"work": true, "mentoring": true, "peer_review": true, "endorsement": true, "boot_block": true}
	if !validStampTypes[opts.stampType] {
		return fmt.Errorf("stamp-type must be work, mentoring, peer_review, endorsement, or boot_block (got %q)", opts.stampType)
	}

	if opts.pilotCohort != "" {
		validCohorts := map[string]bool{"andela-pilot": true, "commbank-pilot": true, "indie": true}
		if !validCohorts[opts.pilotCohort] {
			return fmt.Errorf("pilot-cohort must be andela-pilot, commbank-pilot, or indie (got %q)", opts.pilotCohort)
		}
	}

	validContextTypes := map[string]bool{
		"completion": true, "endorsement": true, "boot_block": true,
		"validation_received": true, "sandboxed_completion": true,
	}
	if !validContextTypes[opts.contextType] {
		return fmt.Errorf("context-type must be completion, endorsement, boot_block, validation_received, or sandboxed_completion (got %q)", opts.contextType)
	}

	return nil
}

func buildValenceJSON(quality, reliability, creativity float64) string {
	parts := []string{fmt.Sprintf(`"quality":%.0f`, quality)}
	if reliability >= 0 {
		parts = append(parts, fmt.Sprintf(`"reliability":%.0f`, reliability))
	}
	if creativity >= 0 {
		parts = append(parts, fmt.Sprintf(`"creativity":%.0f`, creativity))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func buildSkillTagsJSON(skills []string) string {
	quoted := make([]string, len(skills))
	for i, s := range skills {
		quoted[i] = fmt.Sprintf(`"%s"`, strings.TrimSpace(s))
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// stampCounter provides a monotonically-incrementing component for generateStampID.
// On Windows, time.Now() has ~100ns–15ms resolution, making back-to-back calls
// return identical timestamps and therefore identical IDs (GH#3104).
var stampCounter atomic.Uint64

func generateStampID(author, subject, valence, contextID string) string {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	seq := stampCounter.Add(1)
	input := fmt.Sprintf("%s|%s|%s|%s|%s|%d", author, subject, valence, contextID, now, seq)
	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("s-%s", hex.EncodeToString(hash[:])[:12])
}

// insertStamp computes passbook chain linkage and inserts the stamp via the store.
func insertStamp(store doltserver.WLCommonsStore, stamp *doltserver.StampRecord) error {
	// Query the last stamp for this subject to compute chain linkage
	last, err := store.QueryLastStampForSubject(stamp.Subject)
	if err != nil {
		// Non-fatal: proceed without chain linkage
		stamp.StampIndex = 0
	} else if last == nil {
		// Genesis stamp for this subject
		stamp.StampIndex = 0
	} else {
		stamp.PrevStampHash = computeStampHash(last.ID)
		if last.StampIndex >= 0 {
			stamp.StampIndex = last.StampIndex + 1
		} else {
			stamp.StampIndex = 0
		}
	}

	return store.InsertStamp(stamp)
}

// computeStampHash generates a hash of a stamp ID for passbook chain linkage.
func computeStampHash(stampID string) string {
	hash := sha256.Sum256([]byte(stampID))
	return hex.EncodeToString(hash[:])
}

func insertStampInLocalClone(localDir string, stamp *doltserver.StampRecord) error {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	contextID := "NULL"
	if stamp.ContextID != "" {
		contextID = fmt.Sprintf("'%s'", doltserver.EscapeSQL(stamp.ContextID))
	}
	contextType := "NULL"
	if stamp.ContextType != "" {
		contextType = fmt.Sprintf("'%s'", doltserver.EscapeSQL(stamp.ContextType))
	}
	stampType := "NULL"
	if stamp.StampType != "" {
		stampType = fmt.Sprintf("'%s'", doltserver.EscapeSQL(stamp.StampType))
	}
	pilotCohort := "NULL"
	if stamp.PilotCohort != "" {
		pilotCohort = fmt.Sprintf("'%s'", doltserver.EscapeSQL(stamp.PilotCohort))
	}
	skillTags := "NULL"
	if stamp.SkillTags != "" {
		skillTags = fmt.Sprintf("'%s'", doltserver.EscapeSQL(stamp.SkillTags))
	}
	message := "NULL"
	if stamp.Message != "" {
		message = fmt.Sprintf("'%s'", doltserver.EscapeSQL(stamp.Message))
	}

	script := fmt.Sprintf(`INSERT INTO stamps (id, author, subject, valence, confidence, severity, context_id, context_type, stamp_type, pilot_cohort, skill_tags, message, created_at)
VALUES ('%s', '%s', '%s', '%s', %f, '%s', %s, %s, %s, %s, %s, %s, '%s');
CALL DOLT_ADD('-A');
CALL DOLT_COMMIT('-m', 'wl stamp: %s stamps %s');`,
		doltserver.EscapeSQL(stamp.ID), doltserver.EscapeSQL(stamp.Author), doltserver.EscapeSQL(stamp.Subject),
		doltserver.EscapeSQL(stamp.Valence), stamp.Confidence, doltserver.EscapeSQL(stamp.Severity),
		contextID, contextType, stampType, pilotCohort, skillTags, message, now,
		doltserver.EscapeSQL(stamp.Author), doltserver.EscapeSQL(stamp.Subject))

	cmd := exec.Command("dolt", "sql", "-q", script)
	cmd.Dir = localDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("inserting stamp: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
