package refinery

import (
	"fmt"
	"io"
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
// (PRApprover and GetPRRequiredApprovals). Returns nil when all
// configured gates are satisfied, a *NeedsApprovalError when a gate is
// unmet, or a plain error on provider-lookup failure.
//
// Gate semantics — evaluated independently, both must pass when both are
// configured:
//
//	a) If cfg.PRApprover is non-empty, that specific user must have an
//	   active APPROVED review.
//	b) If cfg.GetPRRequiredApprovals() > 0, the count of distinct
//	   approving reviewers must meet the threshold.
//
// When neither gate is configured, returns nil immediately (preserves
// opt-in behavior for rigs that haven't defined approval policy).
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
