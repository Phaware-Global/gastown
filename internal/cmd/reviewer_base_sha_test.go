package cmd

import (
	"errors"
	"testing"
)

// fakeReviewBaseGit stubs the three git operations computeReviewBaseSHA
// composes, recording what it was asked so tests can assert the decision
// logic (which branch the merge-base is computed against) rather than git
// plumbing, which TestMergeBase and
// TestCheckoutPRHeadDetachedFreshensBaseRefs cover with real repos.
type fakeReviewBaseGit struct {
	baseBranch    string
	baseBranchErr error
	defaultBranch string
	mergeBase     string
	mergeBaseErr  error

	mergeBaseAskedWith string
}

func (f *fakeReviewBaseGit) GhPrBaseBranch(prNumber int) (string, error) {
	return f.baseBranch, f.baseBranchErr
}

func (f *fakeReviewBaseGit) RemoteDefaultBranch() string {
	return f.defaultBranch
}

func (f *fakeReviewBaseGit) MergeBase(a, b string) (string, error) {
	f.mergeBaseAskedWith = a
	return f.mergeBase, f.mergeBaseErr
}

func TestComputeReviewBaseSHAUsesPRBaseBranch(t *testing.T) {
	// The PR targets develop — the pin must NOT assume the default branch.
	g := &fakeReviewBaseGit{
		baseBranch:    "develop",
		defaultBranch: "main",
		mergeBase:     "abc123",
	}
	if got := computeReviewBaseSHA(g, 42, "headsha"); got != "abc123" {
		t.Errorf("computeReviewBaseSHA = %q, want %q", got, "abc123")
	}
	if g.mergeBaseAskedWith != "origin/develop" {
		t.Errorf("merge-base computed against %q, want origin/develop (the PR's actual base)", g.mergeBaseAskedWith)
	}
}

func TestComputeReviewBaseSHAFallsBackToRemoteDefault(t *testing.T) {
	// gh unavailable (offline, auth failure) — fall back to the remote
	// default branch rather than disabling the pin outright.
	g := &fakeReviewBaseGit{
		baseBranchErr: errors.New("gh: not logged in"),
		defaultBranch: "main",
		mergeBase:     "def456",
	}
	if got := computeReviewBaseSHA(g, 42, "headsha"); got != "def456" {
		t.Errorf("computeReviewBaseSHA = %q, want %q", got, "def456")
	}
	if g.mergeBaseAskedWith != "origin/main" {
		t.Errorf("merge-base computed against %q, want origin/main fallback", g.mergeBaseAskedWith)
	}
}

func TestComputeReviewBaseSHAEmptyOnMergeBaseFailure(t *testing.T) {
	// No computable merge-base (unfetched SHA, unrelated histories): the pin
	// must come back empty so the prompt falls back to derive-it-yourself —
	// never a bogus base.
	g := &fakeReviewBaseGit{
		baseBranch:   "main",
		mergeBaseErr: errors.New("fatal: no merge base"),
	}
	if got := computeReviewBaseSHA(g, 42, "headsha"); got != "" {
		t.Errorf("computeReviewBaseSHA = %q, want empty on merge-base failure", got)
	}
}
