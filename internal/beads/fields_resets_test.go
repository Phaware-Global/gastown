package beads

import (
	"strings"
	"testing"
)

// Regression for the mrKeys omission: review_loop_resets was parsed by
// ParseMRFields and emitted by FormatMRFields but missing from SetMRFields's
// mrKeys allow-list, so its old value line survived as "other content" while a
// fresh one was written at the top. ParseMRFields is last-match-wins, so after
// one interleaved write the counter read the stale trailing copy and stopped
// advancing.
func TestSetMRFields_ReviewLoopResetsRoundTrips(t *testing.T) {
	issue := &Issue{Description: "some prose\n\nreview_pr: 42\nreview_loop_resets: 1\n"}

	// An unrelated write, the shape writeAwaitReviewStateUpdates performs every
	// reviewer round.
	fields := ParseMRFields(issue)
	if fields == nil {
		t.Fatal("ParseMRFields returned nil")
	}
	if fields.ReviewLoopResets != 1 {
		t.Fatalf("ReviewLoopResets = %d, want 1", fields.ReviewLoopResets)
	}
	fields.CommitSHA = "deadbeef"
	issue.Description = SetMRFields(issue, fields)

	if n := strings.Count(issue.Description, "review_loop_resets:"); n != 1 {
		t.Fatalf("review_loop_resets appears %d times after a rewrite, want 1:\n%s",
			n, issue.Description)
	}

	// A second clear-iter must see 1 and advance to 2.
	fields = ParseMRFields(issue)
	if fields.ReviewLoopResets != 1 {
		t.Fatalf("after rewrite, ReviewLoopResets = %d, want 1 — the counter read a "+
			"stale duplicate", fields.ReviewLoopResets)
	}
	fields.ReviewLoopResets++
	issue.Description = SetMRFields(issue, fields)

	if got := ParseMRFields(issue).ReviewLoopResets; got != 2 {
		t.Errorf("ReviewLoopResets = %d, want 2 — the counter must keep advancing "+
			"across interleaved writes", got)
	}
	if n := strings.Count(issue.Description, "review_loop_resets:"); n != 1 {
		t.Errorf("review_loop_resets duplicated (%d copies):\n%s", n, issue.Description)
	}
}
