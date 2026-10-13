package refinery

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// probeFake is a PRApprovalProbe with canned answers.
type probeFake struct {
	pr         int
	prErr      error
	approved   bool
	approvedEr error
	count      int
	countErr   error
}

func (f *probeFake) FindPRNumber(string) (int, error) { return f.pr, f.prErr }
func (f *probeFake) IsPRApproved(int) (bool, error)   { return f.approved, f.approvedEr }
func (f *probeFake) CountApprovals(int) (int, error)  { return f.count, f.countErr }

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", name)
	mustGit(t, dir, "commit", "-m", msg)
}

type syncRepos struct {
	origin string   // bare remote
	seed   string   // clone used to move origin around
	work   string   // clone the refinery operates in
	g      *git.Git // git handle on work
}

const testBranch = "feature/x"

// newSyncRepos builds: origin with main (M0 then M1) and feature/x branched
// from M0 with one commit F1. The work clone has fetched everything, so the
// branch is exactly one commit behind main. The remote refuses non-fast-forward
// updates, so any force-push to it fails — a successful push proves it was not
// a force-push.
func newSyncRepos(t *testing.T, conflicting bool) *syncRepos {
	t.Helper()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	mustGit(t, tmp, "init", "--bare", "-b", "main", origin)
	mustGit(t, origin, "config", "receive.denyNonFastForwards", "true")

	seed := filepath.Join(tmp, "seed")
	mustGit(t, tmp, "clone", origin, seed)
	commitFile(t, seed, "base.txt", "base\n", "M0")
	mustGit(t, seed, "push", "origin", "HEAD:main")

	mustGit(t, seed, "checkout", "-b", testBranch)
	if conflicting {
		commitFile(t, seed, "base.txt", "feature side\n", "F1")
	} else {
		commitFile(t, seed, "feature.txt", "feature\n", "F1")
	}
	mustGit(t, seed, "push", "origin", testBranch)

	mustGit(t, seed, "checkout", "main")
	if conflicting {
		commitFile(t, seed, "base.txt", "main side\n", "M1")
	} else {
		commitFile(t, seed, "main.txt", "main\n", "M1")
	}
	mustGit(t, seed, "push", "origin", "main")

	work := filepath.Join(tmp, "work")
	mustGit(t, tmp, "clone", origin, work)
	mustGit(t, work, "config", "user.email", "t@example.com")
	mustGit(t, work, "config", "user.name", "T")
	return &syncRepos{origin: origin, seed: seed, work: work, g: git.NewGit(work)}
}

func (r *syncRepos) originSHA(t *testing.T, ref string) string {
	t.Helper()
	return mustGit(t, r.origin, "rev-parse", ref)
}

func (r *syncRepos) parents(t *testing.T, ref string) int {
	t.Helper()
	return len(strings.Fields(mustGit(t, r.work, "rev-list", "--parents", "-n", "1", ref))) - 1
}

func TestCheckForcePushAllowed(t *testing.T) {
	boom := errors.New("gh down")
	tests := []struct {
		name    string
		probe   *probeFake
		refused bool
	}{
		{"no open PR", &probeFake{}, false},
		{"open PR, no approval", &probeFake{pr: 7}, false},
		{"open PR, approving review count", &probeFake{pr: 7, count: 1}, true},
		{"open PR, review decision APPROVED", &probeFake{pr: 7, approved: true}, true},
		{"PR lookup fails", &probeFake{prErr: boom}, true},
		{"approval count lookup fails", &probeFake{pr: 7, countErr: boom}, true},
		{"review decision lookup fails", &probeFake{pr: 7, approvedEr: boom}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckForcePushAllowed(tt.probe, testBranch)
			if tt.refused {
				if !errors.Is(err, ErrForcePushRefused) {
					t.Fatalf("want ErrForcePushRefused, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("want allowed, got %v", err)
			}
		})
	}
}

func TestSyncBranch_UpToDate(t *testing.T) {
	r := newSyncRepos(t, false)
	// Move the branch to main's tip: nothing to sync.
	mustGit(t, r.seed, "checkout", testBranch)
	mustGit(t, r.seed, "merge", "--no-edit", "main")
	mustGit(t, r.seed, "push", "origin", testBranch)
	mustGit(t, r.work, "fetch", "origin")

	for _, strategy := range []string{"rebase", "merge", "none"} {
		res, err := SyncBranch(r.g, &probeFake{}, strategy, testBranch, "main", "temp")
		if err != nil {
			t.Fatalf("%s: %v", strategy, err)
		}
		if res.Outcome != SyncUpToDate {
			t.Fatalf("%s: outcome = %q, want up-to-date", strategy, res.Outcome)
		}
	}
}

func TestSyncBranch_None_ReportsAndLeavesBranchAlone(t *testing.T) {
	r := newSyncRepos(t, false)
	before := r.originSHA(t, testBranch)

	res, err := SyncBranch(r.g, &probeFake{}, "none", testBranch, "main", "temp")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != SyncBehindBase || res.Behind != 1 {
		t.Fatalf("got %+v, want behind-base by 1", res)
	}
	if got := r.originSHA(t, testBranch); got != before {
		t.Fatalf("origin branch moved: %s -> %s", before, got)
	}
	if out := mustGit(t, r.work, "branch", "--list", "temp"); out != "" {
		t.Fatalf("none must not create a local working branch, got %q", out)
	}
}

func TestSyncBranch_Merge_MakesMergeCommit(t *testing.T) {
	r := newSyncRepos(t, false)
	branchTip := r.originSHA(t, testBranch)

	res, err := SyncBranch(r.g, &probeFake{}, "merge", testBranch, "main", "temp")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != SyncSynced {
		t.Fatalf("outcome = %q, want synced", res.Outcome)
	}
	if n := r.parents(t, "temp"); n != 2 {
		t.Fatalf("temp has %d parents, want a 2-parent merge commit", n)
	}
	// The original branch tip must be preserved as an ancestor — history is
	// extended, not rewritten.
	mustGit(t, r.work, "merge-base", "--is-ancestor", branchTip, "temp")
	mustGit(t, r.work, "merge-base", "--is-ancestor", "origin/main", "temp")
}

func TestSyncBranch_Merge_IgnoresApprovedPR(t *testing.T) {
	// A merge extends history; approval is irrelevant to it.
	r := newSyncRepos(t, false)
	res, err := SyncBranch(r.g, &probeFake{pr: 3, count: 2}, "merge", testBranch, "main", "temp")
	if err != nil || res.Outcome != SyncSynced {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestSyncBranch_Rebase_Linearizes(t *testing.T) {
	r := newSyncRepos(t, false)
	branchTip := r.originSHA(t, testBranch)

	res, err := SyncBranch(r.g, &probeFake{}, "rebase", testBranch, "main", "temp")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != SyncSynced {
		t.Fatalf("outcome = %q, want synced", res.Outcome)
	}
	if n := r.parents(t, "temp"); n != 1 {
		t.Fatalf("temp has %d parents, want linear history", n)
	}
	if err := exec.Command("git", "-C", r.work, "merge-base", "--is-ancestor", branchTip, "temp").Run(); err == nil {
		t.Fatal("rebase should rewrite the original branch tip")
	}
}

func TestSyncBranch_Rebase_RefusesApprovedPRBeforeRewriting(t *testing.T) {
	r := newSyncRepos(t, false)
	_, err := SyncBranch(r.g, &probeFake{pr: 3, count: 1}, "rebase", testBranch, "main", "temp")
	if !errors.Is(err, ErrForcePushRefused) {
		t.Fatalf("want ErrForcePushRefused, got %v", err)
	}
	if out := mustGit(t, r.work, "branch", "--list", "temp"); out != "" {
		t.Fatalf("refused sync must not rewrite anything locally, got branch %q", out)
	}
}

func TestSyncBranch_Conflict_AbortsAndCleans(t *testing.T) {
	for _, strategy := range []string{"rebase", "merge"} {
		t.Run(strategy, func(t *testing.T) {
			r := newSyncRepos(t, true)
			res, err := SyncBranch(r.g, &probeFake{}, strategy, testBranch, "main", "temp")
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != SyncConflict {
				t.Fatalf("outcome = %q, want conflict", res.Outcome)
			}
			if st := mustGit(t, r.work, "status", "--porcelain"); st != "" {
				t.Fatalf("worktree not clean after conflict: %q", st)
			}
			for _, state := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply"} {
				if _, err := os.Stat(filepath.Join(r.work, ".git", state)); err == nil {
					t.Fatalf("%s left behind", state)
				}
			}
		})
	}
}

func TestSyncBranch_RejectsUnknownStrategy(t *testing.T) {
	r := newSyncRepos(t, false)
	if _, err := SyncBranch(r.g, &probeFake{}, "squash-it", testBranch, "main", "temp"); err == nil {
		t.Fatal("want error for unknown strategy")
	}
}

func TestPushBranch_Merge_PlainPushNoForce(t *testing.T) {
	r := newSyncRepos(t, false)
	if _, err := SyncBranch(r.g, &probeFake{}, "merge", testBranch, "main", "temp"); err != nil {
		t.Fatal(err)
	}
	// origin has receive.denyNonFastForwards=true: this push can only succeed
	// if it was not a force-push.
	res, err := PushBranch(r.g, &probeFake{pr: 3, count: 1}, "merge", testBranch, "temp")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pushed || res.Forced {
		t.Fatalf("got %+v, want pushed non-forced", res)
	}
	if got, want := r.originSHA(t, testBranch), mustGit(t, r.work, "rev-parse", "temp"); got != want {
		t.Fatalf("origin = %s, want %s", got, want)
	}
}

func TestPushBranch_Merge_DivergedRefusesInsteadOfForcing(t *testing.T) {
	r := newSyncRepos(t, false)
	if _, err := SyncBranch(r.g, &probeFake{}, "merge", testBranch, "main", "temp"); err != nil {
		t.Fatal(err)
	}
	// Someone pushes to the branch after our sync: temp no longer descends
	// from origin's tip.
	mustGit(t, r.seed, "checkout", testBranch)
	commitFile(t, r.seed, "late.txt", "late\n", "late commit")
	mustGit(t, r.seed, "push", "origin", testBranch)
	mustGit(t, r.work, "fetch", "origin")
	before := r.originSHA(t, testBranch)

	_, err := PushBranch(r.g, &probeFake{}, "merge", testBranch, "temp")
	if err == nil {
		t.Fatal("want error when the push would not be a fast-forward")
	}
	if got := r.originSHA(t, testBranch); got != before {
		t.Fatalf("origin branch moved: %s -> %s", before, got)
	}
}

func TestPushBranch_Rebase_ForcesWhenNoApproval(t *testing.T) {
	r := newSyncRepos(t, false)
	if _, err := SyncBranch(r.g, &probeFake{}, "rebase", testBranch, "main", "temp"); err != nil {
		t.Fatal(err)
	}
	// Rebase rewrites history, so the push must be a force; the remote above
	// denies non-fast-forwards, so allow it for this test.
	mustGit(t, r.origin, "config", "receive.denyNonFastForwards", "false")
	res, err := PushBranch(r.g, &probeFake{pr: 3}, "rebase", testBranch, "temp")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pushed || !res.Forced {
		t.Fatalf("got %+v, want forced push", res)
	}
	if got, want := r.originSHA(t, testBranch), mustGit(t, r.work, "rev-parse", "temp"); got != want {
		t.Fatalf("origin = %s, want %s", got, want)
	}
}

func TestPushBranch_Rebase_ApprovedPRRefusesForcePush(t *testing.T) {
	r := newSyncRepos(t, false)
	if _, err := SyncBranch(r.g, &probeFake{}, "rebase", testBranch, "main", "temp"); err != nil {
		t.Fatal(err)
	}
	mustGit(t, r.origin, "config", "receive.denyNonFastForwards", "false")
	before := r.originSHA(t, testBranch)

	_, err := PushBranch(r.g, &probeFake{pr: 3, count: 1}, "rebase", testBranch, "temp")
	if !errors.Is(err, ErrForcePushRefused) {
		t.Fatalf("want ErrForcePushRefused, got %v", err)
	}
	if got := r.originSHA(t, testBranch); got != before {
		t.Fatalf("approved branch was rewritten: %s -> %s", before, got)
	}
}

func TestPushBranch_Rebase_ForceWithLeaseRejectsStaleView(t *testing.T) {
	r := newSyncRepos(t, false)
	if _, err := SyncBranch(r.g, &probeFake{}, "rebase", testBranch, "main", "temp"); err != nil {
		t.Fatal(err)
	}
	mustGit(t, r.origin, "config", "receive.denyNonFastForwards", "false")
	// A commit lands on the branch after our last fetch; the lease must catch it.
	mustGit(t, r.seed, "checkout", testBranch)
	commitFile(t, r.seed, "late.txt", "late\n", "late commit")
	mustGit(t, r.seed, "push", "origin", testBranch)
	before := r.originSHA(t, testBranch)

	if _, err := PushBranch(r.g, &probeFake{}, "rebase", testBranch, "temp"); err == nil {
		t.Fatal("want lease rejection")
	}
	if got := r.originSHA(t, testBranch); got != before {
		t.Fatalf("origin branch moved despite stale lease: %s -> %s", before, got)
	}
}

func TestPushBranch_None_RefusesToPushChanges(t *testing.T) {
	r := newSyncRepos(t, false)
	if _, err := SyncBranch(r.g, &probeFake{}, "merge", testBranch, "main", "temp"); err != nil {
		t.Fatal(err)
	}
	before := r.originSHA(t, testBranch)
	if _, err := PushBranch(r.g, &probeFake{}, "none", testBranch, "temp"); err == nil {
		t.Fatal("none must refuse to push a changed branch")
	}
	if got := r.originSHA(t, testBranch); got != before {
		t.Fatalf("origin branch moved under strategy none")
	}
}

func TestPushBranch_NothingToPush(t *testing.T) {
	r := newSyncRepos(t, false)
	mustGit(t, r.work, "checkout", "-B", "temp", "origin/"+testBranch)
	res, err := PushBranch(r.g, &probeFake{}, "none", testBranch, "temp")
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushed {
		t.Fatalf("got %+v, want no-op", res)
	}
}

func TestPRUpdateStrategyConfig(t *testing.T) {
	for in, want := range map[string]string{"": "rebase", "rebase": "rebase", "merge": "merge", "none": "none"} {
		got, err := config.NormalizePRUpdateStrategy(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := config.NormalizePRUpdateStrategy("force"); err == nil {
		t.Error("want error for invalid strategy")
	}
}

func TestConflictResolutionInstructions(t *testing.T) {
	for _, strategy := range []string{"merge", "none"} {
		got := ConflictResolutionInstructions(strategy, testBranch, "main")
		if !strings.Contains(got, "git merge origin/main") {
			t.Errorf("%s: want a base-merge instruction, got:\n%s", strategy, got)
		}
		for _, banned := range []string{"git rebase", "push -f", "--force"} {
			// "no rebase, no force-push" is prose; the commands must be absent.
			if strings.Contains(got, banned) {
				t.Errorf("%s: instructions contain %q:\n%s", strategy, banned, got)
			}
		}
	}
	for _, strategy := range []string{"", "rebase"} {
		got := ConflictResolutionInstructions(strategy, testBranch, "main")
		if !strings.Contains(got, "git rebase origin/main") || !strings.Contains(got, "--force-with-lease") {
			t.Errorf("%q: default must stay rebase + lease, got:\n%s", strategy, got)
		}
		if strings.Contains(got, "push -f") {
			t.Errorf("%q: bare -f must not be suggested:\n%s", strategy, got)
		}
		if !strings.Contains(got, "approving review") {
			t.Errorf("%q: must warn off rewriting an approved PR:\n%s", strategy, got)
		}
	}
}
