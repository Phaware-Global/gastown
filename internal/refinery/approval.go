package refinery

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// NeedsApprovalError indicates a PR does not yet satisfy its configured
// approval gates. Callers can distinguish this from a lookup/provider
// failure via errors.As.
//
// Both doMergePR (refinery patrol path) and the `gt refinery pr merge`
// CLI subcommand check approval through VerifyPRApproval and branch on
// this type: NeedsApproval sets ProcessResult.NeedsApproval=true in the
// patrol path (so the MR stays in queue); the CLI surfaces the detail
// and exits non-zero.
type NeedsApprovalError struct {
	PRNumber int
	Detail   string
}

func (e *NeedsApprovalError) Error() string { return e.Detail }

// VerifyPRApproval checks the PR's approval state against cfg's gates
// (PRApprover and GetPRRequiredApprovals), plus one unconditional
// invariant that isn't a "gate" in the configured sense. Returns nil when
// everything is satisfied, a *NeedsApprovalError when something is unmet,
// or a plain error on provider-lookup failure.
//
// Checked first, unconditionally (not gated by cfg — it applies even to a
// rig with no PRApprover/PRRequiredApprovals configured):
//
//  0. No reviewer may have an active CHANGES_REQUESTED verdict. This
//     exists because a `disposition` escalation with no anchorable
//     findings (an architectural objection that doesn't map to a diff
//     line) produces zero inline review threads — so the threads-count
//     proxy used elsewhere in the patrol (AwaitReviewStep,
//     dispatch-review-fix) sees "nothing to resolve" and reports ready,
//     while the reviewer's own verdict is still blocking. This is the
//     actual merge decision point (VerifyPRApproval backs both the
//     patrol path and `gt refinery pr merge`), so it's where that verdict
//     has to be checked — not left to a thread count that may be zero by
//     construction. It clears the same way GitHub clears it: a
//     subsequent review from the same user that isn't itself
//     CHANGES_REQUESTED (ChangesRequestedReviewers excludes a superseded
//     verdict) — never by resolving threads that were never created.
//
// Gate semantics — evaluated independently, both must pass when both are
// configured:
//
//	a) If cfg.PRApprover is non-empty, that specific user must have an
//	   active APPROVED review.
//	b) If cfg.GetPRRequiredApprovals() > 0, the count of distinct
//	   approving reviewers must meet the threshold.
//
// Under MergeStrategy="pr", config validation accepts an empty
// PRApprover ONLY when the resolved count gate is zero — i.e.,
// GetPRRequiredApprovals() returns 0. The two shapes that resolve to
// zero are an explicit `pr_required_approvals: 0` and the deprecated
// `require_review: false`; either, paired with empty PRApprover,
// opts out of every per-user approval gate ("no per-user approval,
// gate on review-loop + threads alone"). In that combination the
// "both gates absent → return nil" branch below IS reachable on a
// production pr-mode rig, and is the load-bearing semantic that lets
// the refinery merge based on review-loop completion alone. The same
// branch also covers non-pr-mode and test-constructed configs that
// reach VerifyPRApproval without configuring approval
// (defense-in-depth no-op). Returning nil in either case is correct:
// there is no per-user gate to enforce.
//
// Context plumbing: VerifyPRApproval performs network I/O via the
// PRProvider methods but does not accept a context.Context yet —
// neither PRProvider.IsPRApprovedBy nor CountApprovals take one. When
// the provider interface is updated to be context-aware, this helper
// and doMergePR should thread context through. Out of scope for G21.
//
// out is optional — when non-nil, VerifyPRApproval emits
// [Engineer]-prefixed progress lines for each gate consulted, matching
// the original inline logging in doMergePR so patrol output is
// unchanged. Pass nil from contexts that don't want progress noise
// (e.g., the CLI subcommand which has its own output format).
func VerifyPRApproval(provider PRProvider, cfg *MergeQueueConfig, prNumber int, out io.Writer) error {
	if provider == nil {
		return fmt.Errorf("no PR provider configured")
	}
	if cfg == nil {
		return fmt.Errorf("no MergeQueueConfig provided")
	}

	if err := verifyNoBlockingReview(provider, cfg, prNumber, out); err != nil {
		return err
	}

	approver := cfg.PRApprover
	requiredApprovals := cfg.GetPRRequiredApprovals()

	if approver != "" {
		approved, err := provider.IsPRApprovedBy(prNumber, approver)
		if err != nil {
			return fmt.Errorf("failed to check PR #%d approval by %s: %w", prNumber, approver, err)
		}
		if !approved {
			if out != nil {
				_, _ = fmt.Fprintf(out, "[Engineer] PR #%d awaiting approval from %s — deferring merge\n", prNumber, approver)
			}
			return &NeedsApprovalError{
				PRNumber: prNumber,
				Detail:   fmt.Sprintf("PR #%d requires approving review from %s before merge", prNumber, approver),
			}
		}
		if out != nil {
			_, _ = fmt.Fprintf(out, "[Engineer] PR #%d has approving review from %s\n", prNumber, approver)
		}
	}

	if requiredApprovals > 0 {
		count, err := provider.CountApprovals(prNumber)
		if err != nil {
			return fmt.Errorf("failed to count approvals on PR #%d: %w", prNumber, err)
		}
		if count < requiredApprovals {
			if out != nil {
				_, _ = fmt.Fprintf(out, "[Engineer] PR #%d has %d/%d required approvals — deferring merge\n",
					prNumber, count, requiredApprovals)
			}
			return &NeedsApprovalError{
				PRNumber: prNumber,
				Detail:   fmt.Sprintf("PR #%d has %d of %d required approvals", prNumber, count, requiredApprovals),
			}
		}
		if out != nil {
			_, _ = fmt.Fprintf(out, "[Engineer] PR #%d has %d/%d required approvals\n", prNumber, count, requiredApprovals)
		}
	}

	return nil
}

// trustedBlockingReviewers filters CHANGES_REQUESTED logins down to the
// identities this rig has actually designated for review.
//
// The unfiltered version was a denial-of-service on the town's only merge path.
// GhPrChangesRequestedReviewers returns EVERY login whose newest terminal review
// is CHANGES_REQUESTED, with no permission or identity filter, and on a public
// repository any GitHub account can submit one. That account then blocks the
// merge queue indefinitely, because only an APPROVED or DISMISSED review from
// the same login supersedes it and no in-town actor can produce either on
// someone else's behalf.
//
// Scoping to pr_reviewer and pr_approver keeps what the gate was added for — the
// Reviewer's REQUEST_CHANGES, including the findings-free escalation that
// creates no resolvable thread, must actually block — while making a stranger's
// review advisory. It also restores the opt-out the unconditional form removed:
// a rig that configures neither identity has no per-user gate, and this must not
// invent one.
func trustedBlockingReviewers(cfg *MergeQueueConfig, blocking []string) []string {
	trusted := make(map[string]bool, 2)
	// pr_approver is always trusted: it names a human, and a human can always
	// clear their own verdict.
	//
	// pr_reviewer only when reviewer_local. The gate must be no wider than the
	// clearing path, and the clearing path is a re-dispatch of the IN-TOWN
	// Reviewer. On an external-bot rig there is nothing in town that can produce
	// the APPROVED or DISMISSED review GitHub requires — reRequestBlockingReviewers
	// skips pr_reviewer by design, a follow-up COMMENT does not supersede, and the
	// bot answers to nobody here — so trusting it would wedge the queue until a
	// human dismissed the review by hand. That is the exact unclearable state
	// this gate was added to prevent, and it is worse than the gate's absence:
	// before any of this, an external bot's verdict did not block the merge path
	// at all, and the thread-based gate still covers everything it can anchor.
	if l := strings.ToLower(strings.TrimSpace(cfg.PRApprover)); l != "" {
		trusted[l] = true
	}
	if l := strings.ToLower(strings.TrimSpace(cfg.PRReviewer)); l != "" && cfg.ReviewerLocal {
		trusted[l] = true
	}
	if len(trusted) == 0 {
		return nil
	}
	var out []string
	for _, login := range blocking {
		if trusted[strings.ToLower(strings.TrimSpace(login))] {
			out = append(out, login)
		}
	}
	return out
}

// verifyNoBlockingReview defers the merge while a designated reviewer's newest
// terminal review is CHANGES_REQUESTED.
//
// This gate exists because the Reviewer can now submit REQUEST_CHANGES, and
// nothing else in the merge path looks at review STATE. Thread-based gating
// misses it entirely: an unanchorable objection is delivered via `disposition`
// with zero findings, so there are no unresolved threads, and both
// VerifyReviewThreadsResolved and the review-fix loop read that as "ready to
// advance" — the refinery would merge the PR the Reviewer had just blocked.
//
// The gate fires at PR.7 (`gt refinery pr merge` / doMergePR), NOT at PR.6.
// PR.6 (`wait-approval`) polls only IsPRApprovedBy and CountApprovals and never
// consults review state, so it reports success and waves an unanchored block
// through — worth knowing when debugging a wedge, because the formula points a
// stuck MR back at PR.4 and PR.6, both of which will report ready.
//
// Clearing it is a MANUAL step today, and the message says so rather than
// implying otherwise. An automatic clearing round — re-dispatching the Reviewer
// when it holds an unanchored block — was implemented here and then withdrawn:
// it needs a persisted round counter, an in-flight marker the next patrol cycle
// can see, an escalation at the cap, and a scope that matches this gate's, and
// getting any of them wrong turns one objection into an unbounded dispatch loop
// against the town's only merge path. That belongs in its own change with its
// own review, not bolted onto this one.
//
// The detail message states what actually clears the block, which is narrower
// than it first appears: GitHub supersedes a CHANGES_REQUESTED review only with
// an APPROVED or a DISMISSED one from the SAME login. A follow-up COMMENT does
// not, so the Reviewer clears its own block by passing a later round cleanly
// (ReviewEvent yields APPROVE with no findings and no disposition) and not by
// posting anything else. Saying "a subsequent review with a non-blocking
// disposition is required" was wrong on exactly that point and sent operators
// after a remedy that cannot work.
func verifyNoBlockingReview(provider PRProvider, cfg *MergeQueueConfig, prNumber int, out io.Writer) error {
	blocking, err := provider.ChangesRequestedReviewers(prNumber)
	if err != nil {
		// ErrUnsupported means the provider cannot answer, not that nothing is
		// blocking. Tolerating it keeps Bitbucket working, and the help text says
		// so rather than presenting the block as universal.
		if errors.Is(err, ErrUnsupported) {
			return nil
		}
		return fmt.Errorf("failed to check blocking reviews on PR #%d: %w", prNumber, err)
	}
	blocking = trustedBlockingReviewers(cfg, blocking)
	if len(blocking) == 0 {
		return nil
	}
	who := strings.Join(blocking, ", ")
	if out != nil {
		_, _ = fmt.Fprintf(out, "[Engineer] PR #%d has an active CHANGES_REQUESTED review from %s — deferring merge\n",
			prNumber, who)
	}
	// Lead with the remedy that applies to the login actually holding the
	// verdict. Naming the inapplicable one first steers the reader wrong, and
	// this string is now the only clearing instruction there is.
	remedy := fmt.Sprintf(
		"Address the objection, then re-dispatch with `gt reviewer request %d` — the Reviewer "+
			"clears its own block by passing a round cleanly. Nothing re-triggers that "+
			"automatically: the review-fix loop is thread-driven and an unanchored objection "+
			"creates no thread. Dismissing the review on GitHub also clears it.", prNumber)
	if !holdsReviewerVerdict(cfg, blocking) {
		remedy = "That login is the rig's pr_approver, a human: they must approve the PR or " +
			"dismiss their own review. No in-town command can supersede it."
	}
	return &NeedsApprovalError{
		PRNumber: prNumber,
		Detail: fmt.Sprintf(
			"PR #%d has an active CHANGES_REQUESTED review from %s. GitHub supersedes it only with "+
				"an APPROVED or DISMISSED review from that same login — a follow-up COMMENT does not. %s",
			prNumber, who, remedy),
	}
}

// holdsReviewerVerdict reports whether the blocking set includes the rig's
// pr_reviewer, as opposed to only its pr_approver. The two have different
// remedies — one is an in-town re-dispatch, the other is a human — and the
// operator message leads with whichever applies.
func holdsReviewerVerdict(cfg *MergeQueueConfig, blocking []string) bool {
	want := strings.ToLower(strings.TrimSpace(cfg.PRReviewer))
	if want == "" || !cfg.ReviewerLocal {
		return false
	}
	for _, login := range blocking {
		if strings.ToLower(strings.TrimSpace(login)) == want {
			return true
		}
	}
	return false
}
