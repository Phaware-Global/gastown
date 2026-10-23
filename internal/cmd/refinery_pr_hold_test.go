package cmd

import (
	"errors"
	"strings"
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
	t.Run("a source issue that reads not found waits with the remedy, not an operational error", func(t *testing.T) {
		out := captureStdout(t, func() {
			err := reviewFixHoldGate(&holdGateProvider{}, holdGateState(), "gt-mr-1",
				holdGateShow(map[string]*beads.Issue{"gt-mr-1": holdGateMR()}))
			wantExit(t, err, 1)
		})
		for _, want := range []string{"gt-mr-1", "gt-src-1", "source_issue"} {
			if !strings.Contains(out, want) {
				t.Errorf("output = %q, want it to name %q", out, want)
			}
		}
	})
	t.Run("a PR that is not held proceeds", func(t *testing.T) {
		err := reviewFixHoldGate(&holdGateProvider{}, holdGateState(), "gt-mr-1",
			holdGateShow(map[string]*beads.Issue{
				"gt-mr-1":  holdGateMR("gt:merge-request"),
				"gt-src-1": {ID: "gt-src-1", Status: "closed"},
			}))
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
}

// The formula's merge step passes no --mr, so the gate has to find the MR bead
// itself or a hold set during the approval wait is ignored.
func TestVerifyNotHeld_ResolvesMRWhenNoneGiven(t *testing.T) {
	found := func(mr *beads.Issue, err error) func(int) (*beads.Issue, error) {
		return func(pr int) (*beads.Issue, error) {
			if pr != 42 {
				t.Errorf("looked up PR %d, want 42", pr)
			}
			return mr, err
		}
	}
	issues := map[string]*beads.Issue{"gt-mr-1": holdGateMR("hold")}

	t.Run("held MR found by PR number blocks", func(t *testing.T) {
		err := verifyNotHeld(&holdGateProvider{}, 42, "", found(holdGateMR("hold"), nil), holdGateShow(issues))
		var held *refinery.PRHeldError
		if !errors.As(err, &held) {
			t.Errorf("err = %v, want a *PRHeldError", err)
		}
	})
	t.Run("deferred source of the found MR blocks", func(t *testing.T) {
		err := verifyNotHeld(&holdGateProvider{}, 42, "", found(holdGateMR(), nil), holdGateShow(map[string]*beads.Issue{
			"gt-mr-1":  holdGateMR(),
			"gt-src-1": {ID: "gt-src-1", Status: "deferred"},
		}))
		var held *refinery.PRHeldError
		if !errors.As(err, &held) {
			t.Errorf("err = %v, want a *PRHeldError", err)
		}
	})
	t.Run("a failed lookup fails closed", func(t *testing.T) {
		err := verifyNotHeld(&holdGateProvider{}, 42, "", found(nil, errors.New("dolt unreachable")), holdGateShow(issues))
		if err == nil {
			t.Error("err = nil, want the lookup error")
		}
	})
	t.Run("no MR tracks the PR: only the draft check applies", func(t *testing.T) {
		if err := verifyNotHeld(&holdGateProvider{}, 42, "", found(nil, nil), holdGateShow(issues)); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		if err := verifyNotHeld(&holdGateProvider{draft: true}, 42, "", found(nil, nil), holdGateShow(issues)); err == nil {
			t.Error("a draft PR with no MR was not blocked")
		}
	})
	t.Run("an explicit --mr is not looked up again", func(t *testing.T) {
		lookup := func(int) (*beads.Issue, error) {
			t.Error("MR looked up although --mr was given")
			return nil, nil
		}
		err := verifyNotHeld(&holdGateProvider{}, 42, "gt-mr-1", lookup, holdGateShow(issues))
		var held *refinery.PRHeldError
		if !errors.As(err, &held) {
			t.Errorf("err = %v, want a *PRHeldError", err)
		}
	})
}
