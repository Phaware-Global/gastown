package refinery

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakePRProvider records how each approval-gate method was called and
// returns the configured result. Only the approval-related methods are
// exercised by VerifyPRApproval; other PRProvider methods panic if
// called unexpectedly to surface misuse in tests.
type fakePRProvider struct {
	approvedBy       map[string]bool // user → whether IsPRApprovedBy returns true
	approvalCount    int
	approvedByErr    error
	approvalCountErr error

	// changesRequestedReviewers/Err back ChangesRequestedReviewers directly —
	// constructed straight from the fake, not routed through Consolidate/
	// ParseFindings/ReviewEvent, so a test can reach the
	// findings-free-REQUEST_CHANGES state VerifyPRApproval must guard even
	// though nothing in this package can normally produce it that way.
	changesRequestedReviewers []string
	changesRequestedErr       error

	isApprovedCalls       []string // users queried, in order
	countCalls            int
	changesRequestedCalls int
}

func (f *fakePRProvider) IsPRApprovedBy(prNumber int, user string) (bool, error) {
	f.isApprovedCalls = append(f.isApprovedCalls, user)
	if f.approvedByErr != nil {
		return false, f.approvedByErr
	}
	return f.approvedBy[user], nil
}

func (f *fakePRProvider) CountApprovals(prNumber int) (int, error) {
	f.countCalls++
	if f.approvalCountErr != nil {
		return 0, f.approvalCountErr
	}
	return f.approvalCount, nil
}

func (f *fakePRProvider) ChangesRequestedReviewers(int) ([]string, error) {
	f.changesRequestedCalls++
	if f.changesRequestedErr != nil {
		return nil, f.changesRequestedErr
	}
	return f.changesRequestedReviewers, nil
}

// Unused PRProvider methods — panic if exercised so mis-wired tests fail loudly.
func (f *fakePRProvider) FindPRNumber(string) (int, error)              { panic("unused") }
func (f *fakePRProvider) IsPRApproved(int) (bool, error)                { panic("unused") }
func (f *fakePRProvider) MergePR(int, string) (string, error)           { panic("unused") }
func (f *fakePRProvider) CreatePR(CreatePROptions) (int, string, error) { panic("unused") }
func (f *fakePRProvider) RequestReview(int, []string) error             { panic("unused") }
func (f *fakePRProvider) UnresolvedThreads(int) ([]ReviewThread, error) { panic("unused") }
func (f *fakePRProvider) AllThreads(int) ([]ReviewThread, error)        { panic("unused") }
func (f *fakePRProvider) ChecksRollup(int) (string, bool, error)        { panic("unused") }
func (f *fakePRProvider) PostComment(int, string) error                 { panic("unused") }
func (f *fakePRProvider) HasReviewFrom(int, string) (bool, error)       { panic("unused") }
func (f *fakePRProvider) ListReviewAuthors(int) ([]string, error)       { panic("unused") }
func (f *fakePRProvider) HasReviewFromOnSHA(int, string, string) (bool, error) {
	panic("unused")
}
func (f *fakePRProvider) CurrentHeadSHA(int) (string, error)        { panic("unused") }
func (f *fakePRProvider) SubmitReview(int, SubmitReviewInput) error { panic("unused") }

func intPtr(i int) *int { return &i }

func TestVerifyPRApproval_NoGatesConfigured_ReturnsNil(t *testing.T) {
	// A config with no approval gates — empty PRApprover AND
	// PRRequiredApprovals=0 — is the supported "review-loop only"
	// opt-out under MergeStrategy="pr". Engineer.LoadConfig now
	// accepts this combination explicitly, and VerifyPRApproval
	// must return nil (no per-user gate to enforce — the merge
	// decision falls to the review-loop and unresolved-threads
	// gates that live elsewhere in the patrol formula).
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRApprover:          "",
		PRRequiredApprovals: intPtr(0),
	}
	provider := &fakePRProvider{}
	if err := VerifyPRApproval(provider, cfg, 42, nil); err != nil {
		t.Fatalf("expected nil for no-gate config, got %v", err)
	}
	if len(provider.isApprovedCalls) > 0 || provider.countCalls > 0 {
		t.Errorf("expected no provider calls, got isApproved=%v count=%d",
			provider.isApprovedCalls, provider.countCalls)
	}
}

func TestVerifyPRApproval_ApproverGateOnly_Satisfied(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRApprover:          "gatekeeper",
		PRRequiredApprovals: intPtr(0),
	}
	provider := &fakePRProvider{
		approvedBy: map[string]bool{"gatekeeper": true},
	}
	if err := VerifyPRApproval(provider, cfg, 42, nil); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if provider.countCalls != 0 {
		t.Errorf("count gate disabled but CountApprovals called %d times", provider.countCalls)
	}
}

func TestVerifyPRApproval_ApproverGateFails_ReturnsNeedsApprovalError(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy: "pr",
		PRApprover:    "gatekeeper",
	}
	provider := &fakePRProvider{
		approvedBy: map[string]bool{"gatekeeper": false},
	}
	err := VerifyPRApproval(provider, cfg, 42, nil)
	if err == nil {
		t.Fatal("expected NeedsApprovalError, got nil")
	}
	var needsErr *NeedsApprovalError
	if !errors.As(err, &needsErr) {
		t.Fatalf("expected *NeedsApprovalError, got %T: %v", err, err)
	}
	if needsErr.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", needsErr.PRNumber)
	}
	if !strings.Contains(err.Error(), "gatekeeper") {
		t.Errorf("error should name the missing approver, got %q", err.Error())
	}
}

func TestVerifyPRApproval_CountGateOnly_Satisfied(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRRequiredApprovals: intPtr(2),
	}
	provider := &fakePRProvider{approvalCount: 2}
	if err := VerifyPRApproval(provider, cfg, 42, nil); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(provider.isApprovedCalls) != 0 {
		t.Errorf("approver gate disabled but IsPRApprovedBy called: %v", provider.isApprovedCalls)
	}
}

func TestVerifyPRApproval_CountGateFails_ReturnsNeedsApprovalError(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRRequiredApprovals: intPtr(2),
	}
	provider := &fakePRProvider{approvalCount: 1}
	err := VerifyPRApproval(provider, cfg, 42, nil)
	var needsErr *NeedsApprovalError
	if !errors.As(err, &needsErr) {
		t.Fatalf("expected *NeedsApprovalError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("error should report count, got %q", err.Error())
	}
}

func TestVerifyPRApproval_BothGates_ApproverFailsFirst(t *testing.T) {
	// When both gates are configured and both would fail, the approver
	// gate is evaluated first and short-circuits — CountApprovals is
	// never called.
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRApprover:          "gatekeeper",
		PRRequiredApprovals: intPtr(5),
	}
	provider := &fakePRProvider{
		approvedBy:    map[string]bool{"gatekeeper": false},
		approvalCount: 0,
	}
	err := VerifyPRApproval(provider, cfg, 42, nil)
	var needsErr *NeedsApprovalError
	if !errors.As(err, &needsErr) {
		t.Fatalf("expected *NeedsApprovalError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "gatekeeper") {
		t.Errorf("expected approver-gate error first, got %q", err.Error())
	}
	if provider.countCalls != 0 {
		t.Errorf("approver gate should short-circuit, but CountApprovals called %d times", provider.countCalls)
	}
}

func TestVerifyPRApproval_BothGatesSatisfied_ReturnsNil(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRApprover:          "gatekeeper",
		PRRequiredApprovals: intPtr(2),
	}
	provider := &fakePRProvider{
		approvedBy:    map[string]bool{"gatekeeper": true},
		approvalCount: 2,
	}
	if err := VerifyPRApproval(provider, cfg, 42, nil); err != nil {
		t.Fatalf("expected nil with both gates satisfied, got %v", err)
	}
}

func TestVerifyPRApproval_ApproverLookupError_IsNotNeedsApproval(t *testing.T) {
	// Lookup failures must NOT return NeedsApprovalError — the distinction
	// is load-bearing: NeedsApproval means "wait for a reviewer", while a
	// lookup failure means "tooling broken, bubble up."
	cfg := &MergeQueueConfig{
		MergeStrategy: "pr",
		PRApprover:    "gatekeeper",
	}
	provider := &fakePRProvider{
		approvedByErr: fmt.Errorf("github unavailable"),
	}
	err := VerifyPRApproval(provider, cfg, 42, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var needsErr *NeedsApprovalError
	if errors.As(err, &needsErr) {
		t.Errorf("lookup error wrongly reported as NeedsApprovalError: %v", err)
	}
}

func TestVerifyPRApproval_CountLookupError_IsNotNeedsApproval(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRRequiredApprovals: intPtr(1),
	}
	provider := &fakePRProvider{
		approvalCountErr: fmt.Errorf("graphql timeout"),
	}
	err := VerifyPRApproval(provider, cfg, 42, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var needsErr *NeedsApprovalError
	if errors.As(err, &needsErr) {
		t.Errorf("lookup error wrongly reported as NeedsApprovalError: %v", err)
	}
}

func TestVerifyPRApproval_NilProvider_ReturnsError(t *testing.T) {
	cfg := &MergeQueueConfig{MergeStrategy: "pr", PRApprover: "gatekeeper"}
	if err := VerifyPRApproval(nil, cfg, 42, nil); err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestVerifyPRApproval_NilConfig_ReturnsError(t *testing.T) {
	provider := &fakePRProvider{}
	if err := VerifyPRApproval(provider, nil, 42, nil); err == nil {
		t.Fatal("expected error for nil config")
	}
}

func TestVerifyPRApproval_OutputWriter_EmitsProgressLines(t *testing.T) {
	// When out is non-nil, each gate consulted should emit an [Engineer]
	// progress line — matches the original inline logging in doMergePR.
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRApprover:          "gatekeeper",
		PRRequiredApprovals: intPtr(1),
	}
	provider := &fakePRProvider{
		approvedBy:    map[string]bool{"gatekeeper": true},
		approvalCount: 1,
	}
	var out bytes.Buffer
	if err := VerifyPRApproval(provider, cfg, 42, &out); err != nil {
		t.Fatalf("unexpected err %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "[Engineer]") {
		t.Errorf("expected [Engineer]-prefixed output, got %q", got)
	}
	if !strings.Contains(got, "gatekeeper") || !strings.Contains(got, "approvals") {
		t.Errorf("expected both gate progress lines, got %q", got)
	}
}

func TestVerifyPRApproval_OutputWriter_Nil_NoPanic(t *testing.T) {
	// Nil writer must be safe — refinery_pr.go's CLI path passes nil to
	// skip [Engineer] formatting (CLI has its own output).
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRApprover:          "gatekeeper",
		PRRequiredApprovals: intPtr(1),
	}
	provider := &fakePRProvider{
		approvedBy:    map[string]bool{"gatekeeper": true},
		approvalCount: 1,
	}
	if err := VerifyPRApproval(provider, cfg, 42, nil); err != nil {
		t.Fatalf("unexpected err with nil writer: %v", err)
	}
}

// TestVerifyPRApproval_ChangesRequestedFromTheDesignatedReviewerBlocks pins the
// HIGH this gate exists for: a `disposition: request_changes` escalation with no
// anchorable findings produces zero inline review threads, so the threads-count
// proxy used elsewhere in the patrol (AwaitReviewStep, dispatch-review-fix) sees
// nothing to resolve and would report ready — and the refinery would merge the
// PR the Reviewer had just blocked.
//
// The fake's ChangesRequestedReviewers is set directly — bypassing
// Consolidate/ParseFindings/ReviewEvent entirely — so this reaches the state a
// findings-routed test structurally cannot: zero findings, zero threads, one
// blocking reviewer.
//
// Note the config: the gate is scoped to pr_reviewer/pr_approver, NOT
// unconditional. See the sibling test below for why.
func TestVerifyPRApproval_ChangesRequestedFromTheDesignatedReviewerBlocks(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRReviewer:          "reviewer-bot",
		PRApprover:          "",
		PRRequiredApprovals: intPtr(0),
	}
	provider := &fakePRProvider{
		changesRequestedReviewers: []string{"reviewer-bot"},
	}
	err := VerifyPRApproval(provider, cfg, 42, nil)
	var needsErr *NeedsApprovalError
	if !errors.As(err, &needsErr) {
		t.Fatalf("expected *NeedsApprovalError, got %T: %v", err, err)
	}
	if needsErr.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", needsErr.PRNumber)
	}
	if !strings.Contains(err.Error(), "reviewer-bot") {
		t.Errorf("error should name the blocking reviewer, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "CHANGES_REQUESTED") {
		t.Errorf("error should say CHANGES_REQUESTED, got %q", err.Error())
	}
	// The remediation has to be one that works. GitHub supersedes a
	// CHANGES_REQUESTED review only with an APPROVED or DISMISSED one from the
	// same login, so telling an operator that a "non-blocking disposition" clears
	// it sent them after something impossible.
	if !strings.Contains(err.Error(), "APPROVED or DISMISSED") {
		t.Errorf("error must state what actually supersedes the block, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "non-blocking disposition is required") {
		t.Error("error still advertises a remediation that cannot clear the block")
	}
	// The approver/count gates never even get a chance to run — a blocking
	// reviewer short-circuits before them, same shape as the approver gate
	// short-circuiting the count gate.
	if provider.changesRequestedCalls != 1 {
		t.Errorf("expected ChangesRequestedReviewers called once, got %d", provider.changesRequestedCalls)
	}
}

// TestVerifyPRApproval_ChangesRequestedFromAStrangerDoesNotBlock is the reason
// the gate above is scoped rather than unconditional.
//
// GhPrChangesRequestedReviewers returns every login whose newest terminal review
// is CHANGES_REQUESTED, with no permission or identity filter. On a PUBLIC
// repository any GitHub account can submit one — and since only an APPROVED or
// DISMISSED review from that same login supersedes it, and no in-town actor can
// produce either on someone else's behalf, an unconditional gate handed any
// stranger an indefinite block on the town's only merge path.
func TestVerifyPRApproval_ChangesRequestedFromAStrangerDoesNotBlock(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRReviewer:          "reviewer-bot",
		PRApprover:          "human-approver",
		PRRequiredApprovals: intPtr(0),
	}
	// The approver gate itself is satisfied, so the only thing that can defer the
	// merge here is the CHANGES_REQUESTED check under test.
	provider := &fakePRProvider{
		approvedBy:                map[string]bool{"human-approver": true},
		changesRequestedReviewers: []string{"drive-by-stranger"},
	}
	if err := VerifyPRApproval(provider, cfg, 42, nil); err != nil {
		t.Fatalf("a stranger's CHANGES_REQUESTED must not gate the merge queue, got %v", err)
	}
	// The designated approver still blocks — the scoping is about WHO, not about
	// weakening the gate.
	provider.changesRequestedReviewers = []string{"human-approver"}
	if err := VerifyPRApproval(provider, cfg, 42, nil); err == nil {
		t.Error("the designated approver's CHANGES_REQUESTED must still block")
	}
}

// TestVerifyPRApproval_NoDesignatedIdentitiesMeansNoBlockingGate pins the
// opt-out the unconditional form removed. A rig that configures neither
// pr_reviewer nor pr_approver has deliberately opted out of per-user approval
// gating ("gate on review-loop + threads alone"); this check must not invent one
// on its behalf, least of all from an arbitrary account's review.
func TestVerifyPRApproval_NoDesignatedIdentitiesMeansNoBlockingGate(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRRequiredApprovals: intPtr(0),
	}
	provider := &fakePRProvider{
		changesRequestedReviewers: []string{"anybody"},
	}
	if err := VerifyPRApproval(provider, cfg, 42, nil); err != nil {
		t.Fatalf("a rig with no designated review identities must have no blocking gate, got %v", err)
	}
}

// TestVerifyPRApproval_ChangesRequestedClearsWhenTheReviewerNoLongerBlocks
// covers the clearing path.
//
// The previous version of this test configured no pr_reviewer and handed the
// fake an empty blocking list, so it asserted "no blocking reviewer means no
// block" — true before this gate existed and true after, and therefore unable to
// detect any change to it. It also described the clearing mechanism wrongly: a
// subsequent COMMENT review does NOT supersede a CHANGES_REQUESTED one. Only an
// APPROVED or DISMISSED review from the same login does, which in practice means
// the Reviewer passing a later round cleanly.
//
// This version drives the transition against a configured identity, so it fails
// if the gate stops blocking or stops clearing.
func TestVerifyPRApproval_ChangesRequestedClearsWhenTheReviewerNoLongerBlocks(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRReviewer:          "reviewer-bot",
		PRRequiredApprovals: intPtr(0),
	}
	provider := &fakePRProvider{changesRequestedReviewers: []string{"reviewer-bot"}}
	if err := VerifyPRApproval(provider, cfg, 42, nil); err == nil {
		t.Fatal("precondition: a designated reviewer's CHANGES_REQUESTED must block")
	}
	// A later APPROVED review from the same login supersedes it, so the provider
	// no longer reports the login as blocking.
	provider.changesRequestedReviewers = nil
	if err := VerifyPRApproval(provider, cfg, 42, nil); err != nil {
		t.Fatalf("expected nil once the reviewer no longer blocks, got %v", err)
	}
}

// TestVerifyPRApproval_ChangesRequestedLookupErrorPropagates ensures a
// real provider failure (as opposed to ErrUnsupported) is not silently
// swallowed — this is a merge-blocking safety check, not a best-effort
// nudge like reRequestBlockingReviewers.
func TestVerifyPRApproval_ChangesRequestedLookupErrorPropagates(t *testing.T) {
	cfg := &MergeQueueConfig{MergeStrategy: "pr", PRApprover: "", PRRequiredApprovals: intPtr(0)}
	provider := &fakePRProvider{changesRequestedErr: fmt.Errorf("boom")}
	err := VerifyPRApproval(provider, cfg, 42, nil)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	var needsErr *NeedsApprovalError
	if errors.As(err, &needsErr) {
		t.Errorf("a lookup failure is not a NeedsApprovalError, got %v", err)
	}
}

// TestVerifyPRApproval_ChangesRequestedUnsupportedIsTolerated ensures
// providers that can't enumerate review states (Bitbucket) don't get
// hard-blocked by this check — mirrors reRequestBlockingReviewers'
// tolerance of ErrUnsupported.
func TestVerifyPRApproval_ChangesRequestedUnsupportedIsTolerated(t *testing.T) {
	cfg := &MergeQueueConfig{
		MergeStrategy:       "pr",
		PRApprover:          "gatekeeper",
		PRRequiredApprovals: intPtr(0),
	}
	provider := &fakePRProvider{
		changesRequestedErr: ErrUnsupported,
		approvedBy:          map[string]bool{"gatekeeper": true},
	}
	if err := VerifyPRApproval(provider, cfg, 42, nil); err != nil {
		t.Fatalf("expected ErrUnsupported to be tolerated, got %v", err)
	}
}
