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

	isApprovedCalls []string // users queried, in order
	countCalls      int
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

// Unused PRProvider methods — panic if exercised so mis-wired tests fail loudly.
func (f *fakePRProvider) FindPRNumber(string) (int, error)              { panic("unused") }
func (f *fakePRProvider) IsPRApproved(int) (bool, error)                { panic("unused") }
func (f *fakePRProvider) MergePR(int, string) (string, error)           { panic("unused") }
func (f *fakePRProvider) CreatePR(CreatePROptions) (int, string, error) { panic("unused") }
func (f *fakePRProvider) RequestReview(int, []string) error             { panic("unused") }
func (f *fakePRProvider) UnresolvedThreads(int) ([]ReviewThread, error) { panic("unused") }
func (f *fakePRProvider) AllThreads(int) ([]ReviewThread, error)        { panic("unused") }
func (f *fakePRProvider) ChecksRollup(int) (string, bool, error)        { panic("unused") }

func intPtr(i int) *int { return &i }

func TestVerifyPRApproval_NoGatesConfigured_ReturnsNil(t *testing.T) {
	// Rig opts out of approval policy by leaving both PRApprover unset and
	// PRRequiredApprovals=0. Preserves existing rig behavior.
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
