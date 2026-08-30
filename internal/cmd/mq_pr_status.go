package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/spf13/cobra"
)

var mqPRStatusCmd = &cobra.Command{
	Use:   "pr-status <rig> <mr-id-or-branch>",
	Short: "Resolve the GitHub PR state for a merge-request bead",
	Long: `Resolve the GitHub PR state for a merge-request bead.

The lookup prefers recorded PR URL/number metadata, then falls back to an
unambiguous target-repo head lookup. Ambiguous branch matches fail closed so
patrols do not misclassify fork-head or deleted-head PRs.`,
	Args: cobra.ExactArgs(2),
	RunE: runMQPRStatus,
}

func init() {
	mqPRStatusCmd.Flags().Bool("json", false, "Output JSON")
	mqPRStatusCmd.Flags().String("pr-url", "", "Recorded GitHub PR URL")
	mqPRStatusCmd.Flags().Int("pr-number", 0, "Recorded GitHub PR number")
	mqPRStatusCmd.Flags().String("head-owner", "", "GitHub owner for qualified head fallback")
	mqPRStatusCmd.Flags().String("repo", "", "Target GitHub repo owner/name (defaults to upstream or origin)")
	mqCmd.AddCommand(mqPRStatusCmd)
}

type mqPRStatusResult struct {
	Found bool                 `json:"found"`
	PR    *git.PullRequestInfo `json:"pr,omitempty"`
}

func runMQPRStatus(cmd *cobra.Command, args []string) error {
	rigName := args[0]
	mrID := args[1]
	prURLFlag := commandStringFlag(cmd, "pr-url")
	prNumberFlag := commandIntFlag(cmd, "pr-number")
	headOwner := commandStringFlag(cmd, "head-owner")
	targetRepo := commandStringFlag(cmd, "repo")
	jsonOutput := commandBoolFlag(cmd, "json")

	mgr, r, _, err := getRefineryManager(rigName)
	if err != nil {
		return err
	}
	mr, err := mgr.FindMR(mrID)
	if err != nil {
		return err
	}
	rigGit, err := getRigGit(r.Path)
	if err != nil {
		return fmt.Errorf("resolve PR status: %w", err)
	}

	prURL, prNumber := resolveMQPRIdentity(mr.IssueID, mr.PRURL, mr.PRNumber, r.BeadsPath(), prURLFlag, prNumberFlag)

	pr, err := git.LookupPullRequest(rigGit, git.PullRequestRef{
		URL:        prURL,
		Number:     prNumber,
		Branch:     mr.Branch,
		HeadOwner:  headOwner,
		HeadSHA:    mr.CommitSHA,
		TargetRepo: targetRepo,
	})
	if err != nil {
		if errors.Is(err, git.ErrPullRequestNotFound) {
			return printMQPRStatus(mqPRStatusResult{Found: false}, jsonOutput)
		}
		return err
	}
	return printMQPRStatus(mqPRStatusResult{Found: true, PR: pr}, jsonOutput)
}

func resolveMQPRIdentity(issueID, recordedURL string, recordedNumber int, beadsPath, prURLFlag string, prNumberFlag int) (string, int) {
	prURL := firstNonEmpty(prURLFlag, recordedURL)
	prNumber := prNumberFlag
	if prNumber == 0 {
		prNumber = recordedNumber
	}
	if issueID == "" || (prURL != "" && prNumber != 0) {
		return prURL, prNumber
	}

	sourceURL, sourceNumber, ok := loadMQPRSourceIdentity(issueID, beadsPath)
	if !ok {
		return prURL, prNumber
	}
	if prURL == "" {
		prURL = sourceURL
	}
	if prNumber == 0 {
		prNumber = sourceNumber
	}
	return prURL, prNumber
}

func loadMQPRSourceIdentity(issueID, beadsPath string) (string, int, bool) {
	source, err := beads.New(beadsPath).Show(issueID)
	if err != nil || source == nil {
		return "", 0, false
	}
	sourceURL := ""
	if looksLikeGitHubPRURL(source.ExternalRef) {
		sourceURL = source.ExternalRef
	}
	return sourceURL, prNumberFromLabels(source.Labels), true
}

func printMQPRStatus(result mqPRStatusResult, jsonOutput bool) error {
	if jsonOutput {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	if !result.Found || result.PR == nil {
		fmt.Println("NOT_FOUND")
		return nil
	}
	fmt.Printf("#%d %s %s\n", result.PR.Number, result.PR.State, result.PR.URL)
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func looksLikeGitHubPRURL(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "https://github.com/") && strings.Contains(value, "/pull/")
}

func prNumberFromLabels(labels []string) int {
	for _, label := range labels {
		n, ok := strings.CutPrefix(strings.TrimSpace(label), "pr:")
		if !ok {
			continue
		}
		prNumber, err := strconv.Atoi(n)
		if err == nil && prNumber > 0 {
			return prNumber
		}
	}
	return 0
}
