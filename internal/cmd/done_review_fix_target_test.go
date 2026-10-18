package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/steveyegge/gastown/internal/git"
)

func TestVerifyReviewFixTarget(t *testing.T) {
	const own = "polecat/dag/gt-hck3@mu7e7svg"
	const review = "polecat/capable/gt-tjz4@mu7bhq24"

	findOnly := func(want map[string]int) func(string) (int, error) {
		return func(b string) (int, error) { return want[b], nil }
	}
	pushed := func(ok bool, err error) func(string) (bool, error) {
		return func(string) (bool, error) { return ok, err }
	}
	failGH := func(string) (int, error) { return 0, errors.New("gh: not logged in") }

	tests := []struct {
		name       string
		localBr    string
		reviewBr   string
		find       func(string) (int, error)
		headPushed func(string) (bool, error)
		wantErr    string
		wantWarn   bool
	}{
		{
			name: "pushed to review branch, PR found there", localBr: own, reviewBr: review,
			find: findOnly(map[string]int{review: 234}), headPushed: pushed(true, nil),
		},
		{
			name: "own branch has no PR but review branch does (gt-bmx3)", localBr: own, reviewBr: review,
			find: findOnly(map[string]int{review: 234, own: 0}), headPushed: pushed(true, nil),
		},
		{
			name: "HEAD never pushed to review branch", localBr: own, reviewBr: review,
			find: findOnly(map[string]int{review: 234}), headPushed: pushed(false, nil),
			wantErr: "HEAD is not on origin/" + review,
		},
		{
			name: "ancestry check errors", localBr: own, reviewBr: review,
			find: findOnly(map[string]int{review: 234}), headPushed: pushed(false, errors.New("bad ref")),
			wantErr: "could not verify HEAD reached origin/" + review,
		},
		{
			name: "no PR names the searched branch, never PR #0", localBr: own, reviewBr: review,
			find: findOnly(nil), headPushed: pushed(true, nil),
			wantErr: "no open PR found for branch " + review,
		},
		{
			name: "review branch is on a different PR", localBr: own, reviewBr: review,
			find: findOnly(map[string]int{review: 99}), headPushed: pushed(true, nil),
			wantErr: "targets PR #234 but branch " + review + " is on PR #99",
		},
		{
			name: "no review_branch, own branch is the PR head", localBr: own,
			find: findOnly(map[string]int{own: 234}), headPushed: pushed(false, nil),
		},
		{
			name: "no review_branch, own branch has no PR", localBr: own,
			find: findOnly(nil), headPushed: pushed(false, nil),
			wantErr: "no open PR found for branch " + own,
		},
		{
			name: "gh down, no review_branch: refuse", localBr: own,
			find: failGH, headPushed: pushed(true, nil),
			wantErr: "no review_branch",
		},
		{
			name: "gh down, work on review branch: accept with warning", localBr: own, reviewBr: review,
			find: failGH, headPushed: pushed(true, nil), wantWarn: true,
		},
		{
			name: "gh down, work not on review branch: refuse", localBr: own, reviewBr: review,
			find: failGH, headPushed: pushed(false, nil),
			wantErr: "HEAD is not on origin/" + review,
		},
		{
			name: "gh down, review_branch is own branch: accept with warning", localBr: review, reviewBr: review,
			find: failGH, headPushed: pushed(false, nil), wantWarn: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, err := verifyReviewFixTarget(234, tt.localBr, tt.reviewBr, tt.find, tt.headPushed)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "PR #0") {
					t.Fatalf("error must not report a nonexistent PR #0: %v", err)
				}
			}
			if (warn != "") != tt.wantWarn {
				t.Fatalf("warning = %q, wantWarn %v", warn, tt.wantWarn)
			}
		})
	}
}

// TestHeadOnReviewBranch_IgnoresShadowingRefs proves the push check reads the
// remote-tracking ref, not whatever git's short-name rules find first: a stray
// local branch or tag named origin/<review_branch> must not stand in for a push.
func TestHeadOnReviewBranch_IgnoresShadowingRefs(t *testing.T) {
	const review = "polecat/capable/gt-tjz4@mu7bhq24"

	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	testRunGit(t, tmp, "init", "--initial-branch", "main", repo)
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")

	writeRepoFile(t, repo, "a.txt", "a\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "base")
	// The review branch as last fetched from origin.
	testRunGit(t, repo, "update-ref", "refs/remotes/origin/"+review, "HEAD")

	// A local fix that was never pushed.
	writeRepoFile(t, repo, "b.txt", "b\n")
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "unpushed fix")

	g := gitpkg.NewGit(repo)
	check := func(want bool, why string) {
		t.Helper()
		got, err := headOnReviewBranch(g, review)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", why, err)
		}
		if got != want {
			t.Fatalf("%s: headOnReviewBranch = %v, want %v", why, got, want)
		}
	}

	check(false, "unpushed HEAD")

	testRunGit(t, repo, "branch", "origin/"+review, "HEAD")
	check(false, "with shadowing local branch origin/<review>")
	testRunGit(t, repo, "branch", "-D", "origin/"+review)

	testRunGit(t, repo, "tag", "origin/"+review, "HEAD")
	check(false, "with shadowing tag origin/<review>")
	testRunGit(t, repo, "tag", "-d", "origin/"+review)

	testRunGit(t, repo, "update-ref", "refs/remotes/origin/"+review, "HEAD")
	check(true, "after the push lands on the remote-tracking ref")
}
