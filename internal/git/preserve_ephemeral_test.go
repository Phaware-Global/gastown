package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testWIPMessage = "WIP: checkpoint (auto)"

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// repoState is everything an Ephemeral preserve must leave byte-identical.
type repoState struct {
	head, branchRef, mergeHead string
	index                      []byte
}

func captureRepoState(t *testing.T, dir, branch string) repoState {
	t.Helper()
	gitDir := gitOut(t, dir, "rev-parse", "--absolute-git-dir")
	idx, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	st := repoState{head: gitOut(t, dir, "rev-parse", "HEAD"), index: idx}
	if branch != "" {
		st.branchRef = gitOut(t, dir, "rev-parse", "refs/heads/"+branch)
	}
	if mh, err := os.ReadFile(filepath.Join(gitDir, "MERGE_HEAD")); err == nil {
		st.mergeHead = string(mh)
	}
	return st
}

func requireSameRepoState(t *testing.T, before, after repoState) {
	t.Helper()
	if before.head != after.head {
		t.Errorf("HEAD moved %s -> %s", before.head, after.head)
	}
	if before.branchRef != after.branchRef {
		t.Errorf("branch ref moved %s -> %s", before.branchRef, after.branchRef)
	}
	if before.mergeHead != after.mergeHead {
		t.Errorf("MERGE_HEAD changed %q -> %q", before.mergeHead, after.mergeHead)
	}
	if string(before.index) != string(after.index) {
		t.Errorf("the index changed (%d -> %d bytes)", len(before.index), len(after.index))
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// ephemeralFixture is a polecat-style branch off a pushed base, with origin
// as a local bare repo.
func ephemeralFixture(t *testing.T, branch string) (dir string, g *Git, opts PreserveOptions) {
	t.Helper()
	dir = initTestRepo(t)
	protected := addTestOriginRemote(t, dir)
	runGitTestCmd(t, dir, "checkout", "-b", branch)
	return dir, NewGit(dir), PreserveOptions{
		IssueID:           "gt-94p1",
		Push:              true,
		CommitMessage:     testWIPMessage,
		ProtectedBranches: []string{protected},
		Ephemeral:         true,
	}
}

func TestEphemeralPreserve_LeavesHeadBranchIndexAndMergeHeadByteIdentical(t *testing.T) {
	branch := "polecat/foo/gt-94p1@abc123"
	dir, g, opts := ephemeralFixture(t, branch)

	// One staged edit plus a further unstaged edit to the same file: the
	// index holds content different from both HEAD and the worktree.
	writeTestFile(t, dir, "README.md", "# Test\nstaged\n")
	runGitTestCmd(t, dir, "add", "README.md")
	writeTestFile(t, dir, "README.md", "# Test\nstaged\nunstaged\n")

	before := captureRepoState(t, dir, branch)
	statusBefore := gitOut(t, dir, "status", "--porcelain")
	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Committed || result.Pushed {
		t.Fatalf("expected a local-only snapshot, got %+v", result)
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, branch))
	if got := gitOut(t, dir, "status", "--porcelain"); got != statusBefore {
		t.Errorf("working tree status changed: %q, was %q", got, statusBefore)
	}

	// The snapshot is the WORKTREE content, a child of the untouched HEAD,
	// and it is what the preservation ref holds.
	if got := gitOut(t, dir, "show", result.Commit+":README.md"); got != "# Test\nstaged\nunstaged" {
		t.Errorf("snapshot README.md = %q, want the worktree content", got)
	}
	if parent := gitOut(t, dir, "rev-parse", result.Commit+"^"); parent != before.head {
		t.Errorf("snapshot parent = %s, want HEAD %s", parent, before.head)
	}
	if subj := gitOut(t, dir, "log", "-1", "--format=%s", result.Commit); !strings.HasSuffix(subj, testWIPMessage) {
		t.Errorf("snapshot subject = %q, want it to end with %q", subj, testWIPMessage)
	}
	if got := gitOut(t, dir, "rev-parse", LocalPreservationRefName(branch)); got != result.Commit || result.Ref != LocalPreservationRefName(branch) {
		t.Errorf("local preserve ref = %s (result.Ref %q), want %s", got, result.Ref, result.Commit)
	}
	// The work is still uncommitted where the agent left it.
	if gitOut(t, dir, "status", "--porcelain") == "" {
		t.Error("the agent's uncommitted work must remain uncommitted")
	}
}

func TestEphemeralPreserve_RefusesWhileMergeHeadExists(t *testing.T) {
	branch := "polecat/foo/gt-94p1@merge"
	dir, g, opts := ephemeralFixture(t, branch)

	// A resolved, uncommitted merge: MERGE_HEAD is set and other.txt is
	// staged, exactly the state the agent's own next `git commit` consumes.
	runGitTestCmd(t, dir, "checkout", "-b", "other", "HEAD")
	writeTestFile(t, dir, "other.txt", "from other\n")
	runGitTestCmd(t, dir, "add", "other.txt")
	runGitTestCmd(t, dir, "commit", "-m", "other work")
	runGitTestCmd(t, dir, "checkout", branch)
	runGitTestCmd(t, dir, "merge", "--no-commit", "--no-ff", "other")
	writeTestFile(t, dir, "README.md", "# Test\nedited mid-merge\n")

	before := captureRepoState(t, dir, branch)
	if before.mergeHead == "" {
		t.Fatal("fixture: expected MERGE_HEAD to be set")
	}
	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err == nil || !strings.Contains(err.Error(), "MERGE_HEAD") {
		t.Fatalf("want a refusal naming MERGE_HEAD, got err=%v result=%+v", err, result)
	}
	if result.Committed || result.Pushed {
		t.Fatalf("a mid-merge preserve must make and push no snapshot, got %+v", result)
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, branch))
	if tip, _ := g.RemoteBranchTip("origin", PreservationRefName(branch)); tip != "" {
		t.Fatalf("nothing may reach origin mid-merge, preserve ref tip = %s", tip)
	}

	runGitTestCmd(t, dir, "commit", "-am", "complete the merge")
	if parents := strings.Fields(gitOut(t, dir, "rev-list", "--parents", "-n1", "HEAD")); len(parents) != 3 {
		t.Fatalf("the agent's merge commit has %d parents, want 2 — the merge was flattened", len(parents)-1)
	}
}

func writePreCommitHook(t *testing.T, dir, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell hook script not portable to windows")
	}
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
}

// The snapshot is made with commit-tree, which runs no hooks; the configured
// pre-commit hook (typically the secret scanner) must still gate the push.
func TestEphemeralPreserve_DetachedHeadReusesOneRefAcrossCycles(t *testing.T) {
	dir := initTestRepo(t)
	addTestOriginRemote(t, dir)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "--detach")
	opts := PreserveOptions{IssueID: "furiosa", CommitMessage: testWIPMessage, Ephemeral: true}

	writeTestFile(t, dir, "README.md", "# Test\ncycle one\n")
	first, err := AutoPreserveUncommittedWork(g, "HEAD", opts)
	if err != nil || !first.Committed {
		t.Fatalf("cycle 1: %+v, err %v", first, err)
	}
	writeTestFile(t, dir, "README.md", "# Test\ncycle two\n")
	second, err := AutoPreserveUncommittedWork(g, "HEAD", opts)
	if err != nil || !second.Committed {
		t.Fatalf("cycle 2: %+v, err %v", second, err)
	}
	if first.Ref != second.Ref {
		t.Fatalf("cycle refs differ: %s vs %s — a new ref per cycle", first.Ref, second.Ref)
	}
	if second.Commit == first.Commit {
		t.Fatal("cycle 2 should have recorded a fresh snapshot of the changed work")
	}
	if refs := gitOut(t, dir, "for-each-ref", "refs/gt/preserve/"); len(strings.Split(refs, "\n")) != 1 {
		t.Fatalf("want exactly one detached preservation ref, got:\n%s", refs)
	}
}

func TestEphemeralPreserve_UnchangedWorkIsNotRerecordedEachCycle(t *testing.T) {
	branch := "polecat/foo/gt-94p1@dedupe"
	dir, g, opts := ephemeralFixture(t, branch)
	writeTestFile(t, dir, "README.md", "# Test\nwork\n")

	first, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil || !first.Committed {
		t.Fatalf("first: %+v, err %v", first, err)
	}
	second, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Committed {
		t.Fatalf("an unchanged dirty tree must not be re-snapshotted, got %+v", second)
	}
	if got := gitOut(t, dir, "rev-parse", first.Ref); got != first.Commit {
		t.Fatalf("preservation ref moved to %s, want it left at %s", got, first.Commit)
	}
}

// Untracked files are never captured; tracked modifications are.
func TestEphemeralPreserve_CapturesTrackedChangesNotUntrackedFiles(t *testing.T) {
	branch := "polecat/foo/gt-94p1@tracked"
	dir, g, opts := ephemeralFixture(t, branch)
	writeTestFile(t, dir, "README.md", "# Test\nedited\n")
	writeTestFile(t, dir, ".env", "SECRET=1\n") // untracked
	head := gitOut(t, dir, "rev-parse", "HEAD")

	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil || !result.Committed {
		t.Fatalf("result %+v, err %v", result, err)
	}
	if got := gitOut(t, dir, "diff-tree", "-r", "--no-renames", "--name-status", head, result.Commit); got != "M\tREADME.md" {
		t.Fatalf("snapshot differs from HEAD by:\n%s\nwant only the tracked modification README.md", got)
	}
}

func TestEphemeralPreserve_NothingModifiedMakesNoSnapshot(t *testing.T) {
	branch := "polecat/foo/gt-94p1@clean"
	dir, g, opts := ephemeralFixture(t, branch)
	writeTestFile(t, dir, "untracked.txt", "never captured\n")

	before := captureRepoState(t, dir, branch)
	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if result.Committed || result.Pushed {
		t.Fatalf("expected a no-op, got %+v", result)
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, branch))
}

func TestEphemeralPreserve_RefusesUnmergedConflictsWithoutTouchingState(t *testing.T) {
	dir := initTestRepo(t)
	protected := addTestOriginRemote(t, dir)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "-b", "feature")
	writeTestFile(t, dir, "conflict.txt", "feature\n")
	runGitTestCmd(t, dir, "add", "conflict.txt")
	runGitTestCmd(t, dir, "commit", "-m", "feature change")
	runGitTestCmd(t, dir, "checkout", "-")
	writeTestFile(t, dir, "conflict.txt", "main\n")
	runGitTestCmd(t, dir, "add", "conflict.txt")
	runGitTestCmd(t, dir, "commit", "-m", "main change")
	runGitTestCmd(t, dir, "checkout", "feature")
	runGitTestCmdWantFailure(t, dir, "merge", "-")

	before := captureRepoState(t, dir, "feature")
	result, err := AutoPreserveUncommittedWork(g, "feature", PreserveOptions{Push: true, CommitMessage: testWIPMessage, ProtectedBranches: []string{protected}, Ephemeral: true})
	if err == nil || !strings.Contains(err.Error(), "conflict.txt") {
		t.Fatalf("want an error naming conflict.txt, got %v", err)
	}
	if result.Committed {
		t.Fatal("must not snapshot unresolved conflicts")
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, "feature"))
}

// A bare repo's HEAD is its default branch. Scanning HEAD there passes a push
// whose actual source ref carries a WIP commit.
func TestHasWIPCommit_ScansTheTipNotHEAD(t *testing.T) {
	dir := initTestRepo(t)
	runGitTestCmd(t, dir, "branch", "-M", "main")
	bare := filepath.Join(t.TempDir(), "repo.git")
	runGitTestCmd(t, dir, "clone", "--bare", dir, bare)
	runGitTestCmd(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitTestCmd(t, bare, "update-ref", "refs/remotes/origin/main", "refs/heads/main")

	runGitTestCmd(t, dir, "checkout", "-b", "polecat/x")
	writeTestFile(t, dir, "wip.txt", "wip\n")
	runGitTestCmd(t, dir, "add", "wip.txt")
	runGitTestCmd(t, dir, "commit", "-m", testWIPMessage+" (x)")
	wip := gitOut(t, dir, "rev-parse", "HEAD")
	runGitTestCmd(t, dir, "push", bare, "polecat/x")

	g := NewGitWithDir(bare, "")
	if got, err := HasWIPCommit(g, "origin", "HEAD", testWIPMessage, []string{"main"}); err != nil || got != "" {
		t.Fatalf("premise: scanning HEAD (main) sees nothing, got %q, err %v", got, err)
	}
	if got, err := HasWIPCommit(g, "origin", "refs/heads/polecat/x", testWIPMessage, []string{"main"}); err != nil || got != wip {
		t.Fatalf("scanning the pushed ref = %q, err %v; want the WIP commit %s", got, err, wip)
	}
}

// gt-94p1 round 3 (mayor ruling): snapshots of uncommitted work never leave
// the machine. They live in a ref of the repository's shared common git dir.

func TestEphemeralPreserve_SnapshotSurvivesWorktreeRemoval(t *testing.T) {
	repo := initTestRepo(t)
	addTestOriginRemote(t, repo)
	branch := "polecat/foo/gt-94p1@wtremove"
	wt := filepath.Join(t.TempDir(), "wt")
	runGitTestCmd(t, repo, "worktree", "add", "-b", branch, wt, "HEAD")
	writeTestFile(t, wt, "README.md", "# Test\ndirty file in a linked worktree\n")

	result, err := AutoPreserveUncommittedWork(NewGit(wt), branch, PreserveOptions{Push: true, CommitMessage: testWIPMessage, ProtectedBranches: []string{"master"}, Ephemeral: true})
	if err != nil || !result.Committed {
		t.Fatalf("result %+v, err %v", result, err)
	}
	ref := LocalPreservationRefName(branch)
	if result.Ref != ref {
		t.Fatalf("result.Ref = %q, want the local ref %q", result.Ref, ref)
	}

	runGitTestCmd(t, repo, "worktree", "remove", "--force", wt)

	if got := gitOut(t, repo, "rev-parse", ref); got != result.Commit {
		t.Fatalf("after worktree removal %s = %s, want %s", ref, got, result.Commit)
	}
	if got := gitOut(t, repo, "show", ref+":README.md"); got != "# Test\ndirty file in a linked worktree" {
		t.Fatalf("snapshot README.md = %q, want the dirty file's content", got)
	}
}

func TestEphemeralPreserve_NeverPushesToAnyRemote(t *testing.T) {
	branch := "polecat/foo/gt-94p1@nopush2"
	dir, g, opts := ephemeralFixture(t, branch)
	writeTestFile(t, dir, "README.md", "# Test\nwork\n")

	// Any push attempt errors (unreachable push URL) or trips the marker hook.
	remote := gitOut(t, dir, "remote", "get-url", "origin")
	runGitTestCmd(t, dir, "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "no-such-remote.git"))
	marker := filepath.Join(t.TempDir(), "pushed")
	hook := "#!/bin/sh\ntouch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-push"), []byte(hook), 0755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil {
		t.Fatalf("an Ephemeral preserve must not need a working remote: %v", err)
	}
	if result.Pushed || !result.Committed {
		t.Fatalf("want a local-only snapshot, got %+v", result)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("git push was invoked")
	}
	if refs := gitOut(t, dir, "ls-remote", "--heads", remote); strings.Contains(refs, "preserve") {
		t.Fatalf("a preserve ref reached the remote:\n%s", refs)
	}
}

func TestEphemeralPreserve_DoesNotRunHooks(t *testing.T) {
	branch := "polecat/foo/gt-94p1@nohooks"
	dir, g, opts := ephemeralFixture(t, branch)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	writePreCommitHook(t, dir, "touch '"+marker+"'")
	writeTestFile(t, dir, "README.md", "# Test\nwork\n")

	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil || !result.Committed {
		t.Fatalf("result %+v, err %v", result, err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("a pre-commit hook ran against live worktree state; nothing leaves the machine, so no scan is needed")
	}
}

func TestEphemeralPreserve_RefusesAnySequencerState(t *testing.T) {
	for _, state := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		t.Run(state, func(t *testing.T) {
			branch := "polecat/foo/gt-94p1@seq"
			dir, g, opts := ephemeralFixture(t, branch)
			gitDir := gitOut(t, dir, "rev-parse", "--absolute-git-dir")
			if strings.HasPrefix(state, "rebase-") {
				if err := os.MkdirAll(filepath.Join(gitDir, state), 0755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(gitDir, state), []byte(gitOut(t, dir, "rev-parse", "HEAD")+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, dir, "README.md", "# Test\nwork\n")

			result, err := AutoPreserveUncommittedWork(g, branch, opts)
			if err == nil || !strings.Contains(err.Error(), state) {
				t.Fatalf("want a refusal naming %s, got err=%v result=%+v", state, err, result)
			}
			if result.Committed {
				t.Fatalf("no snapshot may be made during %s", state)
			}
			if out, _ := exec.Command("git", "-C", dir, "rev-parse", "--verify", "-q", LocalPreservationRefName(branch)).Output(); len(out) != 0 {
				t.Fatalf("no preserve ref may be recorded during %s", state)
			}
		})
	}
}
