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
