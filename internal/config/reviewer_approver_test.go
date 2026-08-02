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
