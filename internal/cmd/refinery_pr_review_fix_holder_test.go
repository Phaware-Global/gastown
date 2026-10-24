package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func holderState() dispatchReviewFixState {
	return dispatchReviewFixState{
		PRNumber:    345,
		Branch:      "polecat/valkyrie/gt-84xe4@mvm5un22",
		SourceIssue: "gt-84xe4",
	}
}

func holderLister(issues ...*beads.Issue) func() ([]*beads.Issue, error) {
	return func() ([]*beads.Issue, error) { return issues, nil }
}

// A bead another polecat already holds for the PR must stop a second review-fix
// dispatch, however that bead names the PR.
func TestReviewFixHolder_FindsABeadThatClaimsThePR(t *testing.T) {
	cases := []struct {
		name  string
		issue *beads.Issue
	}{
		{"scoped bead names the PR in its title", &beads.Issue{
			ID: "gt-scoped", Status: "hooked", Assignee: "gastown/polecats/capable",
			Title: "Fix review threads on PR #345",
		}},
		{"title spells the PR without a hash", &beads.Issue{
			ID: "gt-scoped", Status: "in_progress", Assignee: "gastown/polecats/capable",
			Title: "Reverify PR345 after rebase",
		}},
		{"description carries review_pr", &beads.Issue{
			ID: "gt-scoped", Status: "in_progress", Assignee: "gastown/polecats/capable",
			Title:       "Address feedback",
			Description: "review_pr: 345\nbranch: other",
		}},
		{"description names the PR branch", &beads.Issue{
			ID: "gt-scoped", Status: "hooked", Assignee: "gastown/polecats/capable",
			Title:       "Address feedback",
			Description: "resume_branch=polecat/valkyrie/gt-84xe4@mvm5un22\nmore",
		}},
		{"an assigned generic review-fix bead", &beads.Issue{
			ID: "gt-generic", Status: "hooked", Assignee: "gastown/polecats/furiosa",
			Title: "Review-fix: PR #345",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reviewFixHolders([]*beads.Issue{tc.issue}, holderState(), "gt-mr-1")
			if len(got) != 1 || got[0].ID != tc.issue.ID {
				t.Fatalf("reviewFixHolders = %v, want [%s]", got, tc.issue.ID)
			}
		})
	}
}

func TestReviewFixHolder_IgnoresBeadsThatDoNotClaimThePR(t *testing.T) {
	cases := []struct {
		name  string
		issue *beads.Issue
	}{
		{"PR number is only a prefix of another", &beads.Issue{
			ID: "a", Status: "hooked", Title: "Fix threads on PR #3450",
		}},
		{"PR number is a suffix of another", &beads.Issue{
			ID: "a", Status: "hooked", Title: "Fix threads on PR #1345",
		}},
		{"prose in a description is not a claim", &beads.Issue{
			ID: "a", Status: "hooked", Assignee: "gastown/polecats/furiosa",
			Title:       "Dedupe review-fix dispatch",
			Description: "gt-roxmg (PR #345, held by valkyrie) was duplicated",
		}},
		{"a longer branch name is a different branch", &beads.Issue{
			ID: "a", Status: "hooked", Title: "x",
			Description: "branch: polecat/valkyrie/gt-84xe4@mvm5un22-v2",
		}},
		{"a closed bead holds nothing", &beads.Issue{
			ID: "a", Status: "closed", Title: "Fix threads on PR #345",
		}},
		{"the MR bead is not a holder", &beads.Issue{
			ID: "gt-mr-1", Status: "open", Title: "Merge: gt-84xe4",
			Labels: []string{"gt:merge-request"}, Description: "review_pr: 345",
		}},
		{"the source issue is the normal re-sling path", &beads.Issue{
			ID: "gt-84xe4", Status: "in_progress", Title: "PR #345 work",
		}},
		{"an unassigned generic review-fix is an orphan to reuse", &beads.Issue{
			ID: "a", Status: "open", Title: "Review-fix: PR #345",
		}},
		{"an open unassigned scoped bead holds nothing", &beads.Issue{
			ID: "a", Status: "open", Title: "Fix review threads on PR #345",
		}},
		{"an open bead holds nothing even when assigned", &beads.Issue{
			ID: "a", Status: "open", Assignee: "gastown/polecats/capable",
			Title: "Fix review threads on PR #345",
		}},
		{"a blocked bead holds nothing", &beads.Issue{
			ID: "a", Status: "blocked", Assignee: "gastown/polecats/capable",
			Title: "Fix review threads on PR #345",
		}},
		{"an open bead carrying review_pr holds nothing", &beads.Issue{
			ID: "a", Status: "open", Title: "Address feedback",
			Description: "review_pr: 345\nbranch: other",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewFixHolders([]*beads.Issue{tc.issue}, holderState(), "gt-mr-1"); len(got) != 0 {
				t.Fatalf("reviewFixHolders = %v, want none", got)
			}
		})
	}
}

const holderRig = "gastown"

// holderAlive builds a liveness probe from a name -> alive/err table; an
// unlisted polecat is an unknown one, which gt reports as not alive.
func holderAlive(alive map[string]bool, errs map[string]error) func(string) (bool, error) {
	return func(name string) (bool, error) {
		if err := errs[name]; err != nil {
			return false, err
		}
		return alive[name], nil
	}
}

func scopedHolder(status, assignee string) *beads.Issue {
	return &beads.Issue{
		ID: "gt-scoped", Status: status, Assignee: assignee,
		Title: "Fix review threads on PR #345",
	}
}

func TestReviewFixHolderGate(t *testing.T) {
	const capable = "gastown/polecats/capable"

	t.Run("a live in-progress holder skips the dispatch, naming the bead and why", func(t *testing.T) {
		out := captureStdout(t, func() {
			err := reviewFixHolderGate(holderLister(scopedHolder("in_progress", capable)),
				holderAlive(map[string]bool{"capable": true}, nil), holderRig, holderState(), "gt-mr-1")
			wantExit(t, err, 1)
		})
		for _, want := range []string{"PR #345", "gt-scoped", "capable", "alive"} {
			if !strings.Contains(out, want) {
				t.Errorf("output %q missing %q", out, want)
			}
		}
	})
	t.Run("a live hooked holder skips the dispatch", func(t *testing.T) {
		captureStdout(t, func() {
			err := reviewFixHolderGate(holderLister(scopedHolder("hooked", capable)),
				holderAlive(map[string]bool{"capable": true}, nil), holderRig, holderState(), "gt-mr-1")
			wantExit(t, err, 1)
		})
	})
	t.Run("a liveness lookup error holds, naming the bead and the error", func(t *testing.T) {
		out := captureStdout(t, func() {
			err := reviewFixHolderGate(holderLister(scopedHolder("in_progress", capable)),
				holderAlive(nil, map[string]error{"capable": errors.New("status timeout")}),
				holderRig, holderState(), "gt-mr-1")
			wantExit(t, err, 1)
		})
		for _, want := range []string{"PR #345", "gt-scoped", "capable", "status timeout"} {
			if !strings.Contains(out, want) {
				t.Errorf("output %q missing %q", out, want)
			}
		}
		if strings.Contains(out, "alive") {
			t.Errorf("output %q reports a lookup error as a live holder", out)
		}
	})
	t.Run("a holder whose polecat is dead does not hold", func(t *testing.T) {
		err := reviewFixHolderGate(holderLister(scopedHolder("in_progress", capable)),
			holderAlive(map[string]bool{"capable": false}, nil), holderRig, holderState(), "gt-mr-1")
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	t.Run("an open unassigned scoped bead does not hold", func(t *testing.T) {
		err := reviewFixHolderGate(holderLister(scopedHolder("open", "")),
			holderAlive(nil, nil), holderRig, holderState(), "gt-mr-1")
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	t.Run("an in-progress bead with no polecat assignee has no one to be alive", func(t *testing.T) {
		for _, assignee := range []string{"", "gastown/crew/nux", "other/polecats/capable"} {
			err := reviewFixHolderGate(holderLister(scopedHolder("in_progress", assignee)),
				holderAlive(nil, nil), holderRig, holderState(), "gt-mr-1")
			if err != nil {
				t.Errorf("assignee %q: err = %v, want nil", assignee, err)
			}
		}
	})
	t.Run("a dead holder does not mask a live one behind it", func(t *testing.T) {
		dead := scopedHolder("in_progress", "gastown/polecats/gone")
		dead.ID = "gt-dead"
		live := scopedHolder("hooked", capable)
		live.ID = "gt-live"
		out := captureStdout(t, func() {
			err := reviewFixHolderGate(holderLister(dead, live),
				holderAlive(map[string]bool{"capable": true}, nil), holderRig, holderState(), "gt-mr-1")
			wantExit(t, err, 1)
		})
		if !strings.Contains(out, "gt-live") {
			t.Errorf("output %q does not name the live holder", out)
		}
	})
	t.Run("an unheld PR goes ahead", func(t *testing.T) {
		if err := reviewFixHolderGate(holderLister(), holderAlive(nil, nil), holderRig, holderState(), "gt-mr-1"); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	t.Run("an unanswerable holder query is operational, not a go-ahead", func(t *testing.T) {
		err := reviewFixHolderGate(func() ([]*beads.Issue, error) {
			return nil, errors.New("bd: timeout")
		}, holderAlive(nil, nil), holderRig, holderState(), "gt-mr-1")
		wantExit(t, err, 4)
	})
}

// The gate guards a dispatch, not the merge path: a PR with nothing to fix must
// reach wait-approval however many beads match it, and the iteration-cap
// escalation must not wait on a holder either.
func TestNextReviewFixStep(t *testing.T) {
	gateCalled := false
	held := func() error {
		gateCalled = true
		return NewSilentExit(1)
	}
	open := func() error {
		gateCalled = true
		return nil
	}

	t.Run("a held PR with zero threads still advances", func(t *testing.T) {
		gateCalled = false
		step, err := nextReviewFixStep(0, 0, 3, held)
		if err != nil || step != reviewFixAdvance {
			t.Errorf("step, err = %v, %v; want advance, nil", step, err)
		}
		if gateCalled {
			t.Error("holder gate ran for a PR with no threads to dispatch")
		}
	})
	t.Run("a held PR at the iteration cap still escalates", func(t *testing.T) {
		gateCalled = false
		step, err := nextReviewFixStep(2, 3, 3, held)
		if err != nil || step != reviewFixEscalateCap {
			t.Errorf("step, err = %v, %v; want escalate-cap, nil", step, err)
		}
		if gateCalled {
			t.Error("holder gate ran ahead of the iteration-cap escalation")
		}
	})
	t.Run("a held PR with threads is not dispatched", func(t *testing.T) {
		gateCalled = false
		_, err := nextReviewFixStep(2, 0, 3, held)
		wantExit(t, err, 1)
		if !gateCalled {
			t.Error("holder gate did not run before a dispatch")
		}
	})
	t.Run("an unheld PR with threads dispatches", func(t *testing.T) {
		step, err := nextReviewFixStep(2, 0, 3, open)
		if err != nil || step != reviewFixDispatch {
			t.Errorf("step, err = %v, %v; want dispatch, nil", step, err)
		}
	})
}
