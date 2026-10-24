package cmd

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
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
// unlisted polecat is reported as not alive. That is a stub's convenience, not
// what the real probe does for a polecat that was nuked: see
// stubRealLivenessProbe for a probe that returns the real result.
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

// stubRealLivenessProbe stands in for the two lookups behind
// isReviewFixPolecatAlive. exists is what the polecat manager answers for the
// polecat (polecat.ErrPolecatNotFound once it is nuked); status is what
// `gt polecat status --json` prints and returns, which for a nuked polecat is an
// exit 1 with empty stdout.
func stubRealLivenessProbe(t *testing.T, exists error, statusOut string, statusErr error) {
	t.Helper()
	origLookup, origStatus := reviewFixPolecatLookup, reviewFixPolecatStatusJSON
	t.Cleanup(func() { reviewFixPolecatLookup, reviewFixPolecatStatusJSON = origLookup, origStatus })
	reviewFixPolecatLookup = func(rig, name string) error { return exists }
	reviewFixPolecatStatusJSON = func(rig, name string) ([]byte, error) {
		return []byte(statusOut), statusErr
	}
}

// nukedStatusExit is the failure `gt polecat status` exits with for a polecat
// whose worktree is gone: runPolecatStatus returns an error, so stdout is empty.
func nukedStatusExit(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run()
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	return err
}

func TestIsReviewFixPolecatAlive_NukedPolecatIsNotAlive(t *testing.T) {
	t.Run("not found is dead, not an error", func(t *testing.T) {
		stubRealLivenessProbe(t, polecat.ErrPolecatNotFound, "", nukedStatusExit(t))
		alive, err := isReviewFixPolecatAlive(holderRig, "nuked")
		if err != nil || alive {
			t.Errorf("isReviewFixPolecatAlive = %v, %v; want false, nil", alive, err)
		}
	})
	t.Run("a wrapped not found is dead too", func(t *testing.T) {
		stubRealLivenessProbe(t, fmt.Errorf("loading: %w", polecat.ErrPolecatNotFound), "", nukedStatusExit(t))
		alive, err := isReviewFixPolecatAlive(holderRig, "nuked")
		if err != nil || alive {
			t.Errorf("isReviewFixPolecatAlive = %v, %v; want false, nil", alive, err)
		}
	})
	t.Run("a lookup that fails for another reason stays an error", func(t *testing.T) {
		stubRealLivenessProbe(t, errors.New("rig 'gastown' not found"), "", nukedStatusExit(t))
		if _, err := isReviewFixPolecatAlive(holderRig, "nuked"); err == nil {
			t.Error("err = nil, want the lookup failure")
		}
	})
	t.Run("stderr text that merely says not found is not a not-found", func(t *testing.T) {
		stubRealLivenessProbe(t, errors.New("polecat 'x' not found in rig 'gastown'"), "", nukedStatusExit(t))
		if _, err := isReviewFixPolecatAlive(holderRig, "x"); err == nil {
			t.Error("err = nil, want an error: only polecat.ErrPolecatNotFound is definitive")
		}
	})
	t.Run("an existing polecat whose status fails stays an error", func(t *testing.T) {
		stubRealLivenessProbe(t, nil, "", nukedStatusExit(t))
		if _, err := isReviewFixPolecatAlive(holderRig, "capable"); err == nil {
			t.Error("err = nil, want the status failure")
		}
	})
	t.Run("an existing working polecat is alive", func(t *testing.T) {
		stubRealLivenessProbe(t, nil, `{"state":"working"}`, nil)
		alive, err := isReviewFixPolecatAlive(holderRig, "capable")
		if err != nil || !alive {
			t.Errorf("isReviewFixPolecatAlive = %v, %v; want true, nil", alive, err)
		}
	})
}

// The gate must not stall a PR on a bead whose assignee was nuked.
func TestReviewFixHolderGate_NukedAssigneeDoesNotHold(t *testing.T) {
	realProbe := func(name string) (bool, error) { return isReviewFixPolecatAlive(holderRig, name) }

	t.Run("a nuked assignee does not hold", func(t *testing.T) {
		stubRealLivenessProbe(t, polecat.ErrPolecatNotFound, "", nukedStatusExit(t))
		err := reviewFixHolderGate(holderLister(scopedHolder("in_progress", "gastown/polecats/nuked")),
			realProbe, holderRig, holderState(), "gt-mr-1")
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	t.Run("any other status failure still holds, with the loud message", func(t *testing.T) {
		stubRealLivenessProbe(t, nil, "", nukedStatusExit(t))
		out := captureStdout(t, func() {
			err := reviewFixHolderGate(holderLister(scopedHolder("in_progress", "gastown/polecats/capable")),
				realProbe, holderRig, holderState(), "gt-mr-1")
			wantExit(t, err, 1)
		})
		for _, want := range []string{"gt-scoped", "capable", "liveness lookup failed"} {
			if !strings.Contains(out, want) {
				t.Errorf("output %q missing %q", out, want)
			}
		}
	})
}

// Stage 1 shares the probe: a recorded review-fix polecat that was nuked is past
// its dispatch, so the marker is cleared and the next patrol re-enters PR.4
// instead of retrying the status check forever.
func TestReviewFixInFlight_NukedRecordedPolecatProceeds(t *testing.T) {
	state := holderState()
	state.ReviewFixName = "nuked"

	t.Run("a nuked recorded polecat clears the marker", func(t *testing.T) {
		stubRealLivenessProbe(t, polecat.ErrPolecatNotFound, "", nukedStatusExit(t))
		cleared := false
		out := captureStdout(t, func() {
			err := reviewFixInFlight(holderRig, state, isReviewFixPolecatAlive,
				func() error { cleared = true; return nil })
			wantExit(t, err, 1)
		})
		if !cleared {
			t.Errorf("marker not cleared; output %q", out)
		}
	})
	t.Run("any other status failure keeps the marker", func(t *testing.T) {
		stubRealLivenessProbe(t, nil, "", nukedStatusExit(t))
		cleared := false
		out := captureStdout(t, func() {
			err := reviewFixInFlight(holderRig, state, isReviewFixPolecatAlive,
				func() error { cleared = true; return nil })
			wantExit(t, err, 1)
		})
		if cleared {
			t.Error("marker cleared on a transient status failure")
		}
		if !strings.Contains(out, "status check failed") {
			t.Errorf("output %q missing the retry message", out)
		}
	})
	t.Run("a working recorded polecat keeps the marker", func(t *testing.T) {
		stubRealLivenessProbe(t, nil, `{"state":"working"}`, nil)
		cleared := false
		captureStdout(t, func() {
			err := reviewFixInFlight(holderRig, state, isReviewFixPolecatAlive,
				func() error { cleared = true; return nil })
			wantExit(t, err, 1)
		})
		if cleared {
			t.Error("marker cleared while the polecat is working")
		}
	})
}
