package refinery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// holdFakeProvider is a PRProvider that finds PR 42 and records whether the
// merge was attempted. Everything else is deliberately left nil: a held PR must
// never get far enough to call it.
type holdFakeProvider struct {
	PRProvider
	merged bool
}

func (f *holdFakeProvider) FindPRNumber(string) (int, error) { return 42, nil }
func (f *holdFakeProvider) UnresolvedThreads(int) ([]ReviewThread, error) {
	return nil, nil
}
func (f *holdFakeProvider) IsPRApproved(int) (bool, error)                  { return true, nil }
func (f *holdFakeProvider) IsPRApprovedBy(int, string) (bool, error)        { return true, nil }
func (f *holdFakeProvider) CountApprovals(int) (int, error)                 { return 1, nil }
func (f *holdFakeProvider) ChangesRequestedReviewers(int) ([]string, error) { return nil, nil }
func (f *holdFakeProvider) AllThreads(int) ([]ReviewThread, error)          { return nil, nil }
func (f *holdFakeProvider) ChecksRollup(int) (string, bool, error)          { return "SUCCESS", true, nil }
func (f *holdFakeProvider) MergePR(int, string) (string, error) {
	f.merged = true
	return "", errors.New("merge attempted")
}

// draftFakeProvider also implements PRDraftChecker.
type draftFakeProvider struct {
	holdFakeProvider
	draft    bool
	draftErr error
}

func (f *draftFakeProvider) IsDraft(int) (bool, error) { return f.draft, f.draftErr }

func mrBead(labels ...string) *beads.Issue {
	return &beads.Issue{
		ID:          "gt-mr-1",
		Labels:      labels,
		Description: "branch: feat/x\ntarget: main\nsource_issue: gt-src-1\nreview_pr: 42",
	}
}

func showFrom(issues map[string]*beads.Issue) BeadShower {
	return func(id string) (*beads.Issue, error) {
		if issue, ok := issues[id]; ok {
			return issue, nil
		}
		return nil, beads.ErrNotFound
	}
}

func mergeWith(t *testing.T, prov PRProvider, show BeadShower, mrID string) ProcessResult {
	t.Helper()
	workDir, g, _ := testGitRepo(t)
	e := newTestEngineer(t, workDir, g)
	e.prProvider = prov
	e.showBead = show
	createFeatureBranch(t, workDir, "feat/x", "test.txt", "hello")
	return e.doMergePR(context.Background(), mrID, "feat/x", "main")
}

func TestDoMergePR_RefusesDraftPR(t *testing.T) {
	prov := &draftFakeProvider{draft: true}
	res := mergeWith(t, prov, nil, "")
	if prov.merged {
		t.Error("MergePR was called for a draft PR")
	}
	if res.Success || !strings.Contains(res.Error, "on hold") || !strings.Contains(res.Error, "draft") {
		t.Errorf("result = %+v, want a failure naming the draft hold", res)
	}
}

func TestDoMergePR_RefusesPRWhoseMRIsHeld(t *testing.T) {
	prov := &draftFakeProvider{}
	res := mergeWith(t, prov, showFrom(map[string]*beads.Issue{"gt-mr-1": mrBead("hold")}), "gt-mr-1")
	if prov.merged {
		t.Error("MergePR was called for a PR whose MR bead is held")
	}
	if res.Success || !strings.Contains(res.Error, "labeled hold") {
		t.Errorf("result = %+v, want a failure naming the MR hold", res)
	}
}

func TestDoMergePR_RefusesPRWhoseSourceIssueIsHeld(t *testing.T) {
	prov := &draftFakeProvider{}
	show := showFrom(map[string]*beads.Issue{
		"gt-mr-1":  mrBead(),
		"gt-src-1": {ID: "gt-src-1", Status: "deferred"},
	})
	res := mergeWith(t, prov, show, "gt-mr-1")
	if prov.merged {
		t.Error("MergePR was called for a PR whose source issue is deferred")
	}
	if res.Success || !strings.Contains(res.Error, "status deferred") {
		t.Errorf("result = %+v, want a failure naming the source hold", res)
	}
}

// If the hold state cannot be established, the merge does not go ahead on a guess.
func TestDoMergePR_RefusesWhenHoldCheckFails(t *testing.T) {
	t.Run("draft lookup fails", func(t *testing.T) {
		prov := &draftFakeProvider{draftErr: errors.New("gh: rate limited")}
		res := mergeWith(t, prov, nil, "")
		if prov.merged || res.Success {
			t.Errorf("merged=%v result=%+v, want a refusal", prov.merged, res)
		}
	})
	t.Run("bead read fails", func(t *testing.T) {
		prov := &draftFakeProvider{}
		show := func(string) (*beads.Issue, error) { return nil, errors.New("dolt unreachable") }
		res := mergeWith(t, prov, show, "gt-mr-1")
		if prov.merged || res.Success {
			t.Errorf("merged=%v result=%+v, want a refusal", prov.merged, res)
		}
	})
}

// A PR that is neither draft nor held is not stopped by the gate, and neither is
// one on a provider that cannot report draft status.
func TestDoMergePR_UnheldPRReachesTheMerge(t *testing.T) {
	t.Run("not draft, not held", func(t *testing.T) {
		prov := &draftFakeProvider{}
		mergeWith(t, prov, showFrom(map[string]*beads.Issue{"gt-mr-1": mrBead("gt:merge-request")}), "gt-mr-1")
		if !prov.merged {
			t.Error("a PR that is not held never reached MergePR")
		}
	})
	t.Run("provider cannot report draft status", func(t *testing.T) {
		prov := &holdFakeProvider{}
		mergeWith(t, prov, nil, "")
		if !prov.merged {
			t.Error("a provider without draft support never reached MergePR")
		}
	})
	t.Run("source issue already gone", func(t *testing.T) {
		prov := &draftFakeProvider{}
		mergeWith(t, prov, showFrom(map[string]*beads.Issue{"gt-mr-1": mrBead()}), "gt-mr-1")
		if !prov.merged {
			t.Error("a PR whose source issue is gone (normal after gt done) never reached MergePR")
		}
	})
}
