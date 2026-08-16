package config

import (
	"strings"
	"testing"
)

// prConfig builds a minimally-valid PR-strategy merge queue config.
func prConfig(approver, reviewer string) *MergeQueueConfig {
	return &MergeQueueConfig{
		MergeStrategy: MergeStrategyPR,
		VCSProvider:   VCSProviderGitHub,
		PRApprover:    approver,
		PRReviewer:    reviewer,
	}
}

func TestValidateMergeQueue_RejectsApproverEqualToReviewer(t *testing.T) {
	// The Reviewer posts a real APPROVE on a clean pass. If it is also the named
	// pr_approver, VerifyPRApproval's gate (a) is satisfied by the bot approving
	// its own review, and the PR merges with zero human review. This was
	// documented as an operator rule and never enforced.
	err := validateMergeQueueConfig(prConfig("phaware-val", "phaware-val"))
	if err == nil {
		t.Fatal("pr_approver == pr_reviewer must be rejected — the bot would approve its own reviews")
	}
	if !strings.Contains(err.Error(), "pr_approver") || !strings.Contains(err.Error(), "pr_reviewer") {
		t.Errorf("error must name both fields so the fix is obvious, got: %v", err)
	}
}

func TestValidateMergeQueue_ApproverReviewerComparisonIsCaseInsensitive(t *testing.T) {
	// GitHub logins are case-insensitive, so a case variant is the same identity
	// and must not slip past the check.
	if err := validateMergeQueueConfig(prConfig("Phaware-Val", "phaware-val")); err == nil {
		t.Error("case-variant logins are the same GitHub identity and must be rejected")
	}
}

func TestValidateMergeQueue_ApproverReviewerComparisonIgnoresSurroundingSpace(t *testing.T) {
	if err := validateMergeQueueConfig(prConfig("phaware-val ", " phaware-val")); err == nil {
		t.Error("whitespace-padded logins are the same identity and must be rejected")
	}
}

func TestValidateMergeQueue_AcceptsDistinctApproverAndReviewer(t *testing.T) {
	if err := validateMergeQueueConfig(prConfig("alice", "phaware-val")); err != nil {
		t.Errorf("distinct identities must be accepted: %v", err)
	}
}

func TestValidateMergeQueue_EmptyReviewerDoesNotTripTheCheck(t *testing.T) {
	// Rigs not running the in-town Reviewer leave pr_reviewer empty. An empty
	// value must not be treated as "equal to" an empty approver, or every
	// approval-opt-out rig would fail to load.
	if err := validateMergeQueueConfig(prConfig("alice", "")); err != nil {
		t.Errorf("empty pr_reviewer must not trip the equality check: %v", err)
	}
	zero := 0
	cfg := prConfig("", "")
	cfg.PRRequiredApprovals = &zero
	if err := validateMergeQueueConfig(cfg); err != nil {
		t.Errorf("both empty (approval opt-out) must not trip the equality check: %v", err)
	}
}

// TestValidateMergeQueue_EmptyReviewerIsAKnownGapNotAGuarantee documents,
// deliberately, a known limitation rather than letting it pass as
// unremarked-on intended behavior: the pr_approver != pr_reviewer check is a
// proxy comparison between two config strings, not a check against the
// Reviewer's actual identity. On a rig that provisions the Reviewer through
// the standalone (no-MR) request path — where pr_reviewer is legitimately
// left unset because the merge-queue engagement gate it drives doesn't apply
// — nothing stops an operator from setting pr_approver to the Reviewer's own
// machine-user login. This config loads cleanly and, per
// docs/design/reviewer-role.md's "Approval semantics" table, the Reviewer's
// own clean-pass APPROVE would then satisfy VerifyPRApproval's named-approver
// gate: the exact self-approval this whole invariant exists to prevent,
// just reached through the one state (pr_reviewer empty) the string
// comparison can't examine.
//
// Closing this for real means recording the Reviewer's login independently
// of the engagement-gate field (pr_reviewer is a hint for AwaitReviewStep,
// not an identity registry) — out of scope for this fix. Until then, an
// operator running the standalone Reviewer must not also name it as
// pr_approver; this test exists so that limit is asserted and visible in
// the suite rather than silently relied upon.
func TestValidateMergeQueue_EmptyReviewerIsAKnownGapNotAGuarantee(t *testing.T) {
	reviewerLogin := "phaware-val"
	if err := validateMergeQueueConfig(prConfig(reviewerLogin, "")); err != nil {
		t.Fatalf("known gap: pr_approver naming the Reviewer's own login with "+
			"pr_reviewer unset currently loads without error (nothing can check "+
			"it — see this test's comment); validateMergeQueueConfig now returns "+
			"%v, so either the gap was closed (update this test to assert the new "+
			"rejection) or something else regressed", err)
	}
}
