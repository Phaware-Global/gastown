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

	blocking, err := provider.ChangesRequestedReviewers(prNumber)
	if err != nil && !errors.Is(err, ErrUnsupported) {
		return fmt.Errorf("failed to check blocking reviews on PR #%d: %w", prNumber, err)
	}
	if len(blocking) > 0 {
		if out != nil {
			_, _ = fmt.Fprintf(out, "[Engineer] PR #%d has an active CHANGES_REQUESTED review from %s — deferring merge\n",
				prNumber, strings.Join(blocking, ", "))
		}
		return &NeedsApprovalError{
			PRNumber: prNumber,
			Detail: fmt.Sprintf(
				"PR #%d has an active CHANGES_REQUESTED review from %s; a subsequent review with "+
					"a non-blocking disposition is required (there may be no unresolved threads to "+
					"resolve — an unanchored objection doesn't create one)",
				prNumber, strings.Join(blocking, ", ")),
		}
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
