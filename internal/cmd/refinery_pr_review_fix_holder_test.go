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
			ID: "gt-scoped", Status: "open", Title: "Address feedback",
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
			got := reviewFixHolder([]*beads.Issue{tc.issue}, holderState(), "gt-mr-1")
			if got == nil || got.ID != tc.issue.ID {
				t.Fatalf("reviewFixHolder = %v, want %s", got, tc.issue.ID)
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewFixHolder([]*beads.Issue{tc.issue}, holderState(), "gt-mr-1"); got != nil {
				t.Fatalf("reviewFixHolder = %s, want nil", got.ID)
			}
		})
	}
}

func TestReviewFixHolderGate(t *testing.T) {
	t.Run("a held PR is skipped, naming the holder", func(t *testing.T) {
		out := captureStdout(t, func() {
			err := reviewFixHolderGate(holderLister(&beads.Issue{
				ID: "gt-scoped", Status: "hooked", Assignee: "gastown/polecats/capable",
				Title: "Fix review threads on PR #345",
			}), holderState(), "gt-mr-1")
			wantExit(t, err, 1)
		})
		for _, want := range []string{"PR #345", "gt-scoped", "gastown/polecats/capable"} {
			if !strings.Contains(out, want) {
				t.Errorf("output %q missing %q", out, want)
			}
		}
	})
	t.Run("an unheld PR goes ahead", func(t *testing.T) {
		if err := reviewFixHolderGate(holderLister(), holderState(), "gt-mr-1"); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	t.Run("an unanswerable holder query is operational, not a go-ahead", func(t *testing.T) {
		err := reviewFixHolderGate(func() ([]*beads.Issue, error) {
			return nil, errors.New("bd: timeout")
		}, holderState(), "gt-mr-1")
		wantExit(t, err, 4)
	})
}
