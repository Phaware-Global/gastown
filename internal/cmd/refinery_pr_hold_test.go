package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/refinery"
)

type holdGateProvider struct {
	refinery.PRProvider
	draft    bool
	draftErr error
}

func (p *holdGateProvider) IsDraft(int) (bool, error) { return p.draft, p.draftErr }

func holdGateShow(issues map[string]*beads.Issue) refinery.BeadShower {
	return func(id string) (*beads.Issue, error) {
		if issue, ok := issues[id]; ok {
			return issue, nil
		}
		return nil, beads.ErrNotFound
	}
}

func holdGateState() dispatchReviewFixState {
	return dispatchReviewFixState{PRNumber: 42, Branch: "feat/x", SourceIssue: "gt-src-1"}
}

func holdGateMR(labels ...string) *beads.Issue {
	return &beads.Issue{
		ID: "gt-mr-1", Labels: labels,
		Description: "branch: feat/x\ntarget: main\nsource_issue: gt-src-1\nreview_pr: 42",
	}
}

func wantExit(t *testing.T, err error, want int) {
	t.Helper()
	code, ok := IsSilentExit(err)
	if !ok || code != want {
		t.Errorf("err = %v, want silent exit %d", err, want)
	}
}

// A held PR gets no review-fix: the gate stops the cycle with "patrol again"
// before any thread poll, capacity probe or sling.
func TestReviewFixHoldGate(t *testing.T) {
	t.Run("draft PR is skipped", func(t *testing.T) {
		err := reviewFixHoldGate(&holdGateProvider{draft: true}, holdGateState(), "gt-mr-1",
			holdGateShow(map[string]*beads.Issue{"gt-mr-1": holdGateMR()}))
		wantExit(t, err, 1)
	})
	t.Run("held MR bead is skipped", func(t *testing.T) {
		err := reviewFixHoldGate(&holdGateProvider{}, holdGateState(), "gt-mr-1",
			holdGateShow(map[string]*beads.Issue{"gt-mr-1": holdGateMR("hold")}))
		wantExit(t, err, 1)
	})
	t.Run("deferred source issue is skipped", func(t *testing.T) {
		err := reviewFixHoldGate(&holdGateProvider{}, holdGateState(), "gt-mr-1",
			holdGateShow(map[string]*beads.Issue{
				"gt-mr-1":  holdGateMR(),
				"gt-src-1": {ID: "gt-src-1", Status: "deferred"},
			}))
		wantExit(t, err, 1)
	})
	t.Run("an unanswerable hold check is operational, not a go-ahead", func(t *testing.T) {
		err := reviewFixHoldGate(&holdGateProvider{draftErr: errors.New("gh: rate limited")}, holdGateState(), "gt-mr-1",
			holdGateShow(map[string]*beads.Issue{"gt-mr-1": holdGateMR()}))
		wantExit(t, err, 4)
	})
	t.Run("a PR that is not held proceeds", func(t *testing.T) {
		err := reviewFixHoldGate(&holdGateProvider{}, holdGateState(), "gt-mr-1",
			holdGateShow(map[string]*beads.Issue{"gt-mr-1": holdGateMR("gt:merge-request")}))
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
}
