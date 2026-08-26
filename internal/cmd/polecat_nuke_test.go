package cmd

import (
	"testing"

	"github.com/jonbaldie/gastown/internal/beads"
)

func TestShouldPushPolecatBranchBeforeNuke(t *testing.T) {
	tests := []struct {
		name            string
		issue           *beads.Issue
		pushPolicyKnown bool
		want            bool
	}{
		{
			name:            "local-only task never publishes during cleanup",
			issue:           &beads.Issue{Description: "Do not push upstream."},
			pushPolicyKnown: true,
			want:            false,
		},
		{
			name:            "remote review task preserves unpushed work",
			issue:           &beads.Issue{Description: "Prepare this work for human review."},
			pushPolicyKnown: true,
			want:            true,
		},
		{
			name:            "stored local strategy never publishes during cleanup",
			issue:           &beads.Issue{Description: "merge_strategy: local\n"},
			pushPolicyKnown: true,
			want:            false,
		},
		{
			name:            "missing task metadata preserves unpushed work",
			pushPolicyKnown: true,
			want:            true,
		},
		{
			name:            "unreadable task metadata never publishes during cleanup",
			pushPolicyKnown: false,
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPushPolecatBranchBeforeNuke(tt.issue, tt.pushPolicyKnown); got != tt.want {
				t.Fatalf("shouldPushPolecatBranchBeforeNuke(%+v, %v) = %v, want %v", tt.issue, tt.pushPolicyKnown, got, tt.want)
			}
		})
	}
}
