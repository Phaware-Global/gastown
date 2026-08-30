package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery"
)

// postFakeProvider records the order of the two GitHub round-trips
// postReviewAndClearStaleBlock makes. Every other PRProvider method is
// unimplemented and panics if exercised.
type postFakeProvider struct {
	refinery.PRProvider
	submitErr  error
	dismissErr error
	calls      []string
}

func (f *postFakeProvider) SubmitReview(prNumber int, in refinery.SubmitReviewInput) error {
	f.calls = append(f.calls, "submit:"+in.Event)
	return f.submitErr
}

func (f *postFakeProvider) DismissChangesRequestedReviews(prNumber int, user, message string) error {
	f.calls = append(f.calls, "dismiss:"+user)
	return f.dismissErr
}

// A failed submit must dismiss nothing. Dismissing first would clear the prior
// CHANGES_REQUESTED and then fail to replace it: GhPrChangesRequestedReviewers
// folds to the newest terminal state, DISMISSED is terminal, so the reviewer
// would stop counting as blocking and the merge gate would go green on a round
// whose review never landed.
func TestPostReviewAndClearStaleBlock_SubmitFailureDismissesNothing(t *testing.T) {
	p := &postFakeProvider{submitErr: errors.New("502 Bad Gateway")}
	cfg := &refinery.MergeQueueConfig{PRReviewer: "reviewer-bot"}

	err := postReviewAndClearStaleBlock(p, cfg, 7, refinery.SubmitReviewInput{Event: "APPROVE"})

	if err == nil {
		t.Fatal("a failed submit must be reported, not swallowed")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q should carry the provider failure", err)
	}
	if want := []string{"submit:APPROVE"}; !equalCalls(p.calls, want) {
		t.Errorf("calls = %v, want %v — the stale block must survive a failed submit", p.calls, want)
	}
}

// The happy path: the new verdict lands first, then the verdict it supersedes
// is cleared.
func TestPostReviewAndClearStaleBlock_DismissesAfterSubmit(t *testing.T) {
	for _, event := range []string{"APPROVE", "COMMENT"} {
		t.Run(event, func(t *testing.T) {
			p := &postFakeProvider{}
			cfg := &refinery.MergeQueueConfig{PRReviewer: "reviewer-bot"}

			if err := postReviewAndClearStaleBlock(p, cfg, 7, refinery.SubmitReviewInput{Event: event}); err != nil {
				t.Fatalf("postReviewAndClearStaleBlock: %v", err)
			}
			want := []string{"submit:" + event, "dismiss:reviewer-bot"}
			if !equalCalls(p.calls, want) {
				t.Errorf("calls = %v, want %v (in that order)", p.calls, want)
			}
		})
	}
}

// The review just posted is itself a CHANGES_REQUESTED from the same login, so
// dismissing after it would clear the verdict this command exists to publish.
func TestPostReviewAndClearStaleBlock_NeverDismissesItsOwnBlock(t *testing.T) {
	p := &postFakeProvider{}
	cfg := &refinery.MergeQueueConfig{PRReviewer: "reviewer-bot"}

	if err := postReviewAndClearStaleBlock(p, cfg, 7, refinery.SubmitReviewInput{Event: "request_changes"}); err != nil {
		t.Fatalf("postReviewAndClearStaleBlock: %v", err)
	}
	if want := []string{"submit:request_changes"}; !equalCalls(p.calls, want) {
		t.Errorf("calls = %v, want %v — a fresh block must not dismiss itself", p.calls, want)
	}
}

// A dismissal failure is warned about, never fatal: the review has already
// landed, and failing here would report a successful post as an error.
func TestPostReviewAndClearStaleBlock_DismissFailureIsNotFatal(t *testing.T) {
	p := &postFakeProvider{dismissErr: errors.New("403 Forbidden")}
	cfg := &refinery.MergeQueueConfig{PRReviewer: "reviewer-bot"}

	if err := postReviewAndClearStaleBlock(p, cfg, 7, refinery.SubmitReviewInput{Event: "APPROVE"}); err != nil {
		t.Errorf("a failed dismissal must not fail the posted review: %v", err)
	}
}

// With no configured pr_reviewer there is no identity to scope the dismissal
// to, and "dismiss every changes-request" would clear a human's block.
func TestPostReviewAndClearStaleBlock_NoReviewerIdentityDismissesNothing(t *testing.T) {
	p := &postFakeProvider{}
	cfg := &refinery.MergeQueueConfig{PRReviewer: "  "}

	if err := postReviewAndClearStaleBlock(p, cfg, 7, refinery.SubmitReviewInput{Event: "APPROVE"}); err != nil {
		t.Fatalf("postReviewAndClearStaleBlock: %v", err)
	}
	if want := []string{"submit:APPROVE"}; !equalCalls(p.calls, want) {
		t.Errorf("calls = %v, want %v", p.calls, want)
	}
}

func equalCalls(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
