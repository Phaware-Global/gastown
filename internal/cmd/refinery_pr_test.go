package cmd

import (
	"strings"
	"testing"
)

// TestWriteReviewPRToMR_RejectsNonPositive pins the augmentcode-flagged guard
// on PR #80: writeReviewPRToMR must refuse a non-positive PR number BEFORE it
// touches the bead. Without the guard, FormatMRFields omits review_pr for a
// zero/negative value, so a stray 0 would CLEAR a previously-persisted
// review_pr and re-break dispatch-review-fix — the exact gt-5le failure this
// command exists to fix. The guard short-circuits ahead of resolveBeadDir /
// LockBead, so these cases need no bead store.
func TestWriteReviewPRToMR_RejectsNonPositive(t *testing.T) {
	for _, pr := range []int{0, -1} {
		err := writeReviewPRToMR("gt-mr-xyz", pr)
		if err == nil {
			t.Errorf("writeReviewPRToMR(_, %d) = nil; want error (must not clear review_pr)", pr)
			continue
		}
		if !strings.Contains(err.Error(), "non-positive") {
			t.Errorf("writeReviewPRToMR(_, %d) error = %q; want it to mention the non-positive guard", pr, err)
		}
	}
}
