package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// A review-fix dispatch (review_pr set) must be treated as no_merge even when
// the no_merge stamp is missing: entering the regular merge path duplicates
// the in-flight MR bead via same-branch/new-SHA supersede semantics (ha-z60).
func TestIsReviewFixDispatch(t *testing.T) {
	tests := []struct {
		name   string
		fields *beads.AttachmentFields
		want   bool
	}{
		{"nil fields", nil, false},
		{"plain work bead", &beads.AttachmentFields{}, false},
		{"no_merge only (not review-fix)", &beads.AttachmentFields{NoMerge: true}, false},
		{"review-fix dispatch", &beads.AttachmentFields{ReviewPR: 52}, true},
		{"review-fix with stamp", &beads.AttachmentFields{ReviewPR: 52, NoMerge: true}, true},
	}
	for _, tt := range tests {
		if got := isReviewFixDispatch(tt.fields); got != tt.want {
			t.Errorf("%s: isReviewFixDispatch() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// gt-y4gw: a review-fix dispatch (review_pr set) must complete against the
// PR it already targets, not open a fresh PR from the polecat's own
// worktree branch. Two real duplicates (PR #200/#201, PR #208/#219) came
// from the completion path picking createPR whenever merge_strategy=pr,
// without ever checking review_pr.
func TestResolveNoMergeCompletion(t *testing.T) {
	tests := []struct {
		name            string
		fields          *beads.AttachmentFields
		mergeStrategyPR bool
		wantCreatePR    bool
		wantReusePR     int
		wantErr         bool
	}{
		{
			name:            "plain no_merge, merge_strategy=pr creates a fresh PR",
			fields:          &beads.AttachmentFields{NoMerge: true},
			mergeStrategyPR: true,
			wantCreatePR:    true,
		},
		{
			name:            "plain no_merge, merge_strategy!=pr stays on branch",
			fields:          &beads.AttachmentFields{NoMerge: true},
			mergeStrategyPR: false,
		},
		{
			name:            "review-fix dispatch reuses the existing PR, never creates a new one",
			fields:          &beads.AttachmentFields{ReviewPR: 219},
			mergeStrategyPR: true,
			wantReusePR:     219,
		},
		{
			name:            "review-fix dispatch reuses the existing PR even when merge_strategy is not pr",
			fields:          &beads.AttachmentFields{ReviewPR: 219},
			mergeStrategyPR: false,
			wantReusePR:     219,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveNoMergeCompletion(tt.fields, tt.mergeStrategyPR)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveNoMergeCompletion() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got.createPR != tt.wantCreatePR {
				t.Errorf("createPR = %v, want %v", got.createPR, tt.wantCreatePR)
			}
			if got.reusePR != tt.wantReusePR {
				t.Errorf("reusePR = %d, want %d", got.reusePR, tt.wantReusePR)
			}
			if got.reusePR > 0 && got.createPR {
				t.Errorf("reusePR and createPR both set — must never open a new PR when reusing an existing one")
			}
		})
	}
}
