package refinery

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
		mergeWith(t, prov, showFrom(map[string]*beads.Issue{
			"gt-mr-1":  mrBead("gt:merge-request"),
			"gt-src-1": {ID: "gt-src-1", Status: "closed"},
		}), "gt-mr-1")
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
	t.Run("source issue closed by gt done", func(t *testing.T) {
		prov := &draftFakeProvider{}
		show := showFrom(map[string]*beads.Issue{
			"gt-mr-1":  mrBead(),
			"gt-src-1": {ID: "gt-src-1", Status: "closed"},
		})
		mergeWith(t, prov, show, "gt-mr-1")
		if !prov.merged {
			t.Error("a PR whose source issue is closed (normal after gt done) never reached MergePR")
		}
	})
}

// gt done closes the source issue, it does not delete it, and a misrouted
// lookup reads "not found" too, so an unreadable source is not a go-ahead.
func TestDoMergePR_RefusesWhenSourceIssueNotFound(t *testing.T) {
	prov := &draftFakeProvider{}
	res := mergeWith(t, prov, showFrom(map[string]*beads.Issue{"gt-mr-1": mrBead()}), "gt-mr-1")
	if prov.merged || res.Success {
		t.Errorf("merged=%v result=%+v, want a refusal", prov.merged, res)
	}
	if !strings.Contains(res.Error, "gt-src-1") {
		t.Errorf("error = %q, want it to name the source issue", res.Error)
	}
}

// A missing MR bead is a routing fault (a misrouted or embedded clone reads
// "database not found" as ErrNotFound), not the absence of a hold.
func TestVerifyPRNotHeld_MissingMRBeadFailsClosed(t *testing.T) {
	err := VerifyPRNotHeld(&draftFakeProvider{}, 42, "gt-mr-1", showFrom(map[string]*beads.Issue{}))
	if err == nil {
		t.Fatal("VerifyPRNotHeld = nil for an MR bead that cannot be read, want an error")
	}
	var held *PRHeldError
	if errors.As(err, &held) {
		t.Errorf("err = %v, want an operational error, not a hold", err)
	}
	if !strings.Contains(err.Error(), "gt-mr-1") {
		t.Errorf("err = %v, want it to name the MR bead", err)
	}
}

// stubGtOnPath puts a recording `gt` first on PATH so a nudge the code under
// test sends lands in the returned log instead of the live town.
func stubGtOnPath(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "gt.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gt"), []byte(script), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatalf("write gt stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func guessedSourceMR() *beads.Issue {
	return &beads.Issue{
		ID:          "gt-mr-1",
		Description: "branch: feat/x\ntarget: main\nsource_issue: refinery-hold\nreview_pr: 42",
	}
}

// gt mq submit can record a source_issue guessed from the branch name that
// never existed. Refusing it stays fail-closed, but the error has to tell an
// operator what to fix.
func TestDoMergePR_SourceNotFoundNamesMRSourceAndRemedy(t *testing.T) {
	prov := &draftFakeProvider{}
	res := mergeWith(t, prov, showFrom(map[string]*beads.Issue{"gt-mr-1": guessedSourceMR()}), "gt-mr-1")
	if prov.merged || res.Success {
		t.Fatalf("merged=%v result=%+v, want a refusal", prov.merged, res)
	}
	for _, want := range []string{"gt-mr-1", "refinery-hold", "source_issue"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("error = %q, want it to name %q", res.Error, want)
		}
	}
	if !res.Held {
		t.Errorf("result = %+v, want Held: a refused hold check is not a build failure", res)
	}
}

// A hold refusal repeats on every poll until an operator acts. It is reported
// as its own non-build result, once per MR state change, and never as a
// MERGE_FAILED nudge to the polecat and the mayor.
func TestHandleMRInfoFailure_HoldRefusalReportsOncePerStateChange(t *testing.T) {
	gtLog := stubGtOnPath(t)
	workDir, g, _ := testGitRepo(t)
	e := newTestEngineer(t, workDir, g)
	out := &bytes.Buffer{}
	e.output = out
	escalations := recordEscalations(e)
	prov := &draftFakeProvider{}
	e.prProvider = prov
	issues := map[string]*beads.Issue{"gt-mr-1": guessedSourceMR()}
	e.showBead = showFrom(issues)
	createFeatureBranch(t, workDir, "feat/x", "test.txt", "hello")
	mr := &MRInfo{ID: "gt-mr-1", Branch: "feat/x", Target: "main", SourceIssue: "refinery-hold", Worker: "polecats/nux"}

	poll := func(n int) {
		for i := 0; i < n; i++ {
			if res := e.doMergePR(context.Background(), mr.ID, mr.Branch, mr.Target); !res.Success {
				e.HandleMRInfoFailure(mr, res)
			}
		}
	}

	poll(4)
	if prov.merged {
		t.Fatal("the PR was merged")
	}
	if len(*escalations) != 1 {
		t.Fatalf("got %d reports across 4 polls, want 1: %+v", len(*escalations), *escalations)
	}
	first := (*escalations)[0]
	if !strings.Contains(first.fingerprint, "gt-mr-1") || !strings.Contains(first.message, "refinery-hold") {
		t.Errorf("report = %+v, want a fingerprint and message naming the MR and its source_issue", first)
	}

	// The operator puts the MR on hold: a different state, reported once more.
	issues["gt-mr-1"] = &beads.Issue{ID: "gt-mr-1", Labels: []string{"hold"}, Description: guessedSourceMR().Description}
	poll(3)
	if len(*escalations) != 2 {
		t.Fatalf("got %d reports after the MR state changed, want 2: %+v", len(*escalations), *escalations)
	}
	if (*escalations)[1].fingerprint == first.fingerprint {
		t.Errorf("a changed MR state reused fingerprint %q", first.fingerprint)
	}

	if strings.Contains(out.String(), "MERGE_FAILED") {
		t.Errorf("output reports a build failure:\n%s", out.String())
	}
	if data, _ := os.ReadFile(gtLog); len(data) != 0 { //nolint:gosec // test-owned path
		t.Errorf("gt was invoked (a nudge to the polecat or mayor):\n%s", data)
	}
}
