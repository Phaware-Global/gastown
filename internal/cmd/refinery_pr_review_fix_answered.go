package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/refinery"
)

// reviewFixDispatchGate is every check that runs on the way to a dispatch: the
// answered check first, then holder.
func reviewFixDispatchGate(provider refinery.PRProvider, cfg *refinery.MergeQueueConfig, state dispatchReviewFixState, mrID string,
	threads []refinery.ReviewThread, holder func() error, escalate escalateFunc) func() error {
	return func() error {
		if err := reviewFixAnsweredGate(provider, cfg, state, mrID, threads, escalate); err != nil {
			return err
		}
		return holder()
	}
}

// escalateFunc files an escalation; fingerprint dedupes repeats.
type escalateFunc func(description, reason, fingerprint string) error

// reviewFixAnsweredGate stops a dispatch that has no code to change: a
// configured reviewer or approver approved the current head, nobody requests
// changes, the PR author has the last word on every unresolved thread, and that
// approval is newer than every such reply. An older approval says nothing about
// the replies: it is often the same review that opened the threads.
//
// The threads still block the merge, so the exit is 1 ("patrol again"), never
// the advance code. With no polecat dispatched there is no iteration cap to
// reach anyone, so the skip escalates, deduped per MR and head.
func reviewFixAnsweredGate(provider refinery.PRProvider, cfg *refinery.MergeQueueConfig, state dispatchReviewFixState, mrID string,
	threads []refinery.ReviewThread, escalate escalateFunc) error {
	checker, ok := provider.(refinery.HeadApprovalChecker)
	if !ok || cfg == nil {
		return nil
	}
	var lastReply time.Time
	for _, t := range threads {
		if t.LastReplyAuthor == "" || t.LastReplyAt.IsZero() {
			return nil
		}
		if t.LastReplyAt.After(lastReply) {
			lastReply = t.LastReplyAt
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
		approvedAt, err := checker.HeadApprovalTime(state.PRNumber, user, head)
		if err != nil {
			return fail("reading approvals", err)
		}
		if !approvedAt.After(lastReply) {
			continue
		}
		var reason strings.Builder
		fmt.Fprintf(&reason, "%s approved head %s of PR #%d after the author's last reply on every unresolved thread, "+
			"so no review-fix is dispatched. The threads still block the merge: check each answer, then resolve "+
			"the thread or dispatch a fix.\n", user, head, state.PRNumber)
		for _, t := range threads {
			fmt.Fprintf(&reason, "- %s %s\n", t.ID, t.URL)
		}
		if err := escalate(
			fmt.Sprintf("PR #%d: %d answered thread(s) still unresolved and blocking the merge", state.PRNumber, len(threads)),
			reason.String(),
			fmt.Sprintf("dispatch-review-fix:answered:%s:%s", mrID, head)); err != nil {
			return wrapOperationalErr(fmt.Errorf("escalating answered threads on PR #%d: %w", state.PRNumber, err))
		}
		fmt.Fprintf(os.Stdout,
			"PR #%d: review-fix not needed — %s approved head %s after the author replied last on all %d unresolved thread(s); skipping dispatch. The threads still block the merge until resolved\n",
			state.PRNumber, user, head, len(threads))
		return NewSilentExit(1)
	}
	return nil
}
