package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// A review-fix dispatch (review_pr set) must be treated as no_merge even when
// the no_merge stamp is missing: entering the regular merge path duplicates
// the in-flight MR bead via same-branch/new-SHA supersede semantics (ha-z60).
func TestIsReviewFixDispatch(t *testing.T) {
	tests := []struct {
		name   string
		fields *beads.AttachmentFields
		want   bool
	}{
		{"nil fields", nil, false},
		{"plain work bead", &beads.AttachmentFields{}, false},
		{"no_merge only (not review-fix)", &beads.AttachmentFields{NoMerge: true}, false},
		{"review-fix dispatch", &beads.AttachmentFields{ReviewPR: 52}, true},
		{"review-fix with stamp", &beads.AttachmentFields{ReviewPR: 52, NoMerge: true}, true},
	}
	for _, tt := range tests {
		if got := isReviewFixDispatch(tt.fields); got != tt.want {
			t.Errorf("%s: isReviewFixDispatch() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// gt-y4gw: a review-fix dispatch (review_pr set) must complete against the
// PR it already targets, not open a fresh PR from the polecat's own
// worktree branch. Two real duplicates (PR #200/#201, PR #208/#219) came
// from the completion path picking createPR whenever merge_strategy=pr,
// without ever checking review_pr.
func TestResolveNoMergeCompletion(t *testing.T) {
	tests := []struct {
		name            string
		fields          *beads.AttachmentFields
		mergeStrategyPR bool
		wantCreatePR    bool
		wantReusePR     int
		wantErr         bool
	}{
		{
			name:            "plain no_merge, merge_strategy=pr creates a fresh PR",
			fields:          &beads.AttachmentFields{NoMerge: true},
			mergeStrategyPR: true,
			wantCreatePR:    true,
		},
		{
			name:            "plain no_merge, merge_strategy!=pr stays on branch",
			fields:          &beads.AttachmentFields{NoMerge: true},
			mergeStrategyPR: false,
		},
		{
			name:            "review-fix dispatch reuses the existing PR, never creates a new one",
			fields:          &beads.AttachmentFields{ReviewPR: 219},
			mergeStrategyPR: true,
			wantReusePR:     219,
		},
		{
			name:            "review-fix dispatch reuses the existing PR even when merge_strategy is not pr",
			fields:          &beads.AttachmentFields{ReviewPR: 219},
			mergeStrategyPR: false,
			wantReusePR:     219,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveNoMergeCompletion(tt.fields, tt.mergeStrategyPR)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveNoMergeCompletion() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got.createPR != tt.wantCreatePR {
				t.Errorf("createPR = %v, want %v", got.createPR, tt.wantCreatePR)
			}
			if got.reusePR != tt.wantReusePR {
				t.Errorf("reusePR = %d, want %d", got.reusePR, tt.wantReusePR)
			}
			if got.reusePR > 0 && got.createPR {
				t.Errorf("reusePR and createPR both set — must never open a new PR when reusing an existing one")
			}
		})
	}
}

// PR #221 round-2 adversarial findings: on the reused-PR completion path,
// gt done must (1) surface the live MR bead's ID so shouldNudgeRefinery
// fires and CompletionMetadata.MRID is accurate, and (2) recognize when
// issueID IS that MR's source_issue so the work bead is left open instead
// of force-closed out from under a still-live MR.
func TestResolveReviewFixMRLookup(t *testing.T) {
	const (
		reusePR = 221
		branch  = "polecat/slit/gt-y4gw@mtg1grxe"
		issueID = "gt-y4gw"
	)

	t.Run("lookup error aborts gt done — fail closed, never force-close on an unverified lookup", func(t *testing.T) {
		// PR #221 round 3: a Dolt lock timeout must NOT proceed with
		// refineryOwnsMerge=false, which would force-close the live MR's
		// source_issue. The helper returns an error so gt done fails
		// loudly and stays retryable; the bead is left untouched.
		got, err := resolveReviewFixMRLookup(nil, errors.New("dolt: connection refused"), reusePR, issueID, branch)
		if err == nil {
			t.Fatal("err = nil, want non-nil — a lookup error must abort gt done, not fall through to the force-close path")
		}
		if got.mrFailed || got.mrID != "" || got.refineryOwnsMerge {
			t.Errorf("result = %+v, want zero value alongside the error", got)
		}
	})

	t.Run("no MR bead found leaves mrID empty and flags mrFailed", func(t *testing.T) {
		got, err := resolveReviewFixMRLookup(nil, nil, reusePR, issueID, branch)
		if err != nil {
			t.Fatalf("err = %v, want nil — a definitive not-found is not an abort", err)
		}
		if got.mrID != "" {
			t.Errorf("mrID = %q, want empty when no MR bead tracks the PR", got.mrID)
		}
		if !got.mrFailed || got.errMsg == "" {
			t.Errorf("mrFailed = %v, errMsg = %q, want mrFailed=true with a non-empty message", got.mrFailed, got.errMsg)
		}
	})

	t.Run("live MR bead whose source_issue is this bead: mrID set, work bead left open", func(t *testing.T) {
		mrIssue := &beads.Issue{
			ID:          "gt-wisp-ne5u",
			Description: "branch: " + branch + "\nsource_issue: " + issueID + "\nreview_pr: 221",
		}
		got, err := resolveReviewFixMRLookup(mrIssue, nil, reusePR, issueID, branch)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got.mrID != "gt-wisp-ne5u" {
			t.Errorf("mrID = %q, want %q — shouldNudgeRefinery(exitType, mrID) never fires if this stays empty", got.mrID, "gt-wisp-ne5u")
		}
		if !got.refineryOwnsMerge {
			t.Errorf("refineryOwnsMerge = false, want true — force-closing %s here would strand the live MR without its source_issue", issueID)
		}
		if got.mrFailed {
			t.Errorf("mrFailed = true, want false — a live MR bead was found")
		}
	})

	t.Run("live MR bead tracking a DIFFERENT source_issue: mrID set, but work bead still closes", func(t *testing.T) {
		// resolveReviewFixDispatchBead creates a fresh dispatch bead (source
		// closed/tombstoned/absent) when the original source_issue is gone —
		// that fresh bead is not the MR's source_issue and must still be
		// force-closed so it doesn't sit open forever.
		mrIssue := &beads.Issue{
			ID:          "gt-wisp-ne5u",
			Description: "branch: " + branch + "\nsource_issue: gt-y4gw\nreview_pr: 221",
		}
		got, err := resolveReviewFixMRLookup(mrIssue, nil, reusePR, "gt-c9z7", branch)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got.mrID != "gt-wisp-ne5u" {
			t.Errorf("mrID = %q, want %q", got.mrID, "gt-wisp-ne5u")
		}
		if got.refineryOwnsMerge {
			t.Errorf("refineryOwnsMerge = true, want false — gt-c9z7 is not this MR's source_issue, so it must still be force-closed")
		}
	})
}
