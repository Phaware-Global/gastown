package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/refinery"
)

const humanGateHead = "1111111111111111111111111111111111111111"

// humanGateFake implements only what mergeVerifiedPR and waitForApproval touch;
// every other PRProvider method panics on the nil embedded interface. It
// records the head pin each MergePR call was given.
type humanGateFake struct {
	refinery.PRProvider
	head      string
	headErr   error
	approvals map[string][]string // commit -> logins with a terminal APPROVED there
	mergeErr  error
	pins      []string
	polls     int
}

func (f *humanGateFake) UnresolvedThreads(int) ([]refinery.ReviewThread, error) { return nil, nil }
func (f *humanGateFake) ChangesRequestedReviewers(int) ([]string, error)        { return nil, nil }
func (f *humanGateFake) CurrentHeadSHA(int) (string, error)                     { return f.head, f.headErr }
func (f *humanGateFake) ApprovedReviewersAtSHA(_ int, sha string) ([]string, error) {
	f.polls++
	return f.approvals[sha], nil
}
func (f *humanGateFake) MergePR(_ int, _, matchHeadSHA string) (string, error) {
	f.pins = append(f.pins, matchHeadSHA)
	return "merged-sha", f.mergeErr
}

func humanGateCfg() *refinery.MergeQueueConfig {
	zero := 0
	return &refinery.MergeQueueConfig{
		MergeStrategy:          "pr",
		PRRequiredApprovals:    &zero,
		RequiredHumanReviewers: []string{"alice"},
	}
}

// `gt refinery pr merge` must hand MergePR the head the gate verified. Dropping
// the argument lets a push that lands after the check merge on its strength.
func TestMergeVerifiedPR_PinsVerifiedHead(t *testing.T) {
	f := &humanGateFake{head: humanGateHead, approvals: map[string][]string{humanGateHead: {"alice"}}}

	sha, err := mergeVerifiedPR(f, humanGateCfg(), 7, "squash")
	if err != nil {
		t.Fatalf("mergeVerifiedPR: %v", err)
	}
	if sha != "merged-sha" {
		t.Errorf("sha = %q", sha)
	}
	if len(f.pins) != 1 || f.pins[0] != humanGateHead {
		t.Fatalf("MergePR pins = %v, want exactly the verified head", f.pins)
	}
}

func TestMergeVerifiedPR_NoHumanGateMergesUnpinned(t *testing.T) {
	f := &humanGateFake{}
	cfg := humanGateCfg()
	cfg.RequiredHumanReviewers = nil

	if _, err := mergeVerifiedPR(f, cfg, 7, "squash"); err != nil {
		t.Fatalf("mergeVerifiedPR: %v", err)
	}
	if len(f.pins) != 1 || f.pins[0] != "" {
		t.Fatalf("MergePR pins = %v, want one unpinned call", f.pins)
	}
}

func TestMergeVerifiedPR_RefusesWithoutHumanApprovalAtHead(t *testing.T) {
	f := &humanGateFake{head: humanGateHead, approvals: map[string][]string{"older": {"alice"}}}

	if _, err := mergeVerifiedPR(f, humanGateCfg(), 7, "squash"); err == nil {
		t.Fatal("must refuse when the only human approval is on an older commit")
	}
	if len(f.pins) != 0 {
		t.Errorf("MergePR was called despite the refusal: %v", f.pins)
	}
}

// A head that moved after the check is reported as such, with the remedy,
// not as a bare merge error.
func TestMergeVerifiedPR_HeadMovedReportsReapproval(t *testing.T) {
	f := &humanGateFake{
		head:      humanGateHead,
		approvals: map[string][]string{humanGateHead: {"alice"}},
		mergeErr:  &refinery.HeadMovedError{PRNumber: 7, Err: errors.New("gh: head branch was modified")},
	}

	_, err := mergeVerifiedPR(f, humanGateCfg(), 7, "squash")
	var moved *refinery.HeadMovedError
	if !errors.As(err, &moved) {
		t.Fatalf("want *HeadMovedError in the chain, got %v", err)
	}
	if !strings.Contains(err.Error(), "head moved since approval; re-approval needed") {
		t.Errorf("message should state the head moved, got: %v", err)
	}
	if !strings.Contains(err.Error(), "wait-approval") {
		t.Errorf("message should name the remedy, got: %v", err)
	}
}

// PR.6 must not report success for a PR that PR.7 will refuse. With a human
// approval only on an older commit, wait-approval has to keep waiting and time
// out rather than pass.
func TestWaitForApproval_StaleHumanApprovalTimesOut(t *testing.T) {
	f := &humanGateFake{head: humanGateHead, approvals: map[string][]string{"older": {"alice"}}}

	err := waitForApproval(f, humanGateCfg(), 7, "", 0, 20*time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout waiting for approval") {
		t.Fatalf("want a timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "required human reviewer") {
		t.Errorf("timeout should name the unmet human gate, got: %v", err)
	}
	if f.polls < 2 {
		t.Errorf("should have polled the gate repeatedly, polled %d time(s)", f.polls)
	}
}

func TestWaitForApproval_HumanApprovalAtHeadPasses(t *testing.T) {
	f := &humanGateFake{head: humanGateHead, approvals: map[string][]string{humanGateHead: {"alice"}}}

	// No --approver and no --min-approvals: the human gate alone is a valid gate.
	if err := waitForApproval(f, humanGateCfg(), 7, "", 0, time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitForApproval: %v", err)
	}
}

// A lookup failure will not clear by waiting; it must end the wait at once
// instead of spending the whole timeout.
func TestWaitForApproval_HumanGateLookupFailureEndsWait(t *testing.T) {
	f := &humanGateFake{headErr: errors.New("gh: 401")}

	start := time.Now()
	err := waitForApproval(f, humanGateCfg(), 7, "", 0, time.Hour, time.Millisecond)
	if err == nil || strings.Contains(err.Error(), "timeout waiting") {
		t.Fatalf("want an immediate lookup error, got %v", err)
	}
	if time.Since(start) > time.Minute {
		t.Error("lookup failure waited out the timeout")
	}
}

func TestWaitForApproval_NoGatesStillRejected(t *testing.T) {
	cfg := humanGateCfg()
	cfg.RequiredHumanReviewers = nil
	if err := waitForApproval(&humanGateFake{}, cfg, 7, "", 0, time.Second, time.Millisecond); err == nil {
		t.Fatal("no approver, no count and no human gate is a scripting error and must be rejected")
	}
	if err := waitForApproval(&humanGateFake{}, nil, 7, "", 0, time.Second, time.Millisecond); err == nil {
		t.Fatal("nil cfg with no flags must be rejected too")
	}
}
