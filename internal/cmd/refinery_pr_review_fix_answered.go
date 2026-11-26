package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/refinery"
)

// reviewFixDispatchGate is every check that runs on the way to a dispatch: the
// answered check first, then holder.
func reviewFixDispatchGate(provider refinery.PRProvider, cfg *refinery.MergeQueueConfig, state dispatchReviewFixState, threads []refinery.ReviewThread, holder func() error) func() error {
	return func() error {
		if err := reviewFixAnsweredGate(provider, cfg, state, threads); err != nil {
			return err
		}
		return holder()
	}
}

// reviewFixAnsweredGate stops a dispatch that has no code to change: a
// configured reviewer or approver approved the current head, nobody requests
// changes, and the PR author has the last word on every unresolved thread. The
// threads still block the merge, so the exit is 1 ("patrol again"), never the
// advance code.
func reviewFixAnsweredGate(provider refinery.PRProvider, cfg *refinery.MergeQueueConfig, state dispatchReviewFixState, threads []refinery.ReviewThread) error {
	checker, ok := provider.(refinery.HeadApprovalChecker)
	if !ok || cfg == nil {
		return nil
	}
	for _, t := range threads {
		if t.LastReplyAuthor == "" {
			return nil
		}
	}

	fail := func(what string, err error) error {
		return wrapOperationalErr(fmt.Errorf("checking whether PR #%d needs a review-fix: %s: %w", state.PRNumber, what, err))
	}
	author, err := checker.PRAuthor(state.PRNumber)
	if err != nil {
		return fail("reading the PR author", err)
	}
	for _, t := range threads {
		if !strings.EqualFold(t.LastReplyAuthor, author) {
			return nil
		}
	}
	head, err := provider.CurrentHeadSHA(state.PRNumber)
	if err != nil {
		return fail("reading the head commit", err)
	}
	blockers, err := provider.ChangesRequestedReviewers(state.PRNumber)
	if err != nil {
		return fail("reading change requests", err)
	}
	if len(blockers) > 0 {
		return nil
	}
	for _, user := range []string{cfg.PRReviewer, cfg.PRApprover} {
		if user == "" {
			continue
		}
		approved, err := checker.IsPRApprovedAtSHA(state.PRNumber, user, head)
		if err != nil {
			return fail("reading approvals", err)
		}
		if approved {
			fmt.Fprintf(os.Stdout,
				"PR #%d: review-fix not needed — %s approved head %s and the author replied last on all %d unresolved thread(s); skipping dispatch. The threads still block the merge until resolved\n",
				state.PRNumber, user, head, len(threads))
			return NewSilentExit(1)
		}
	}
	return nil
}
