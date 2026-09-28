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

// git stash create reads no sequencer state, and the snapshot is the only
// recovery path under a forced removal, so a snapshot is taken mid-operation.
func TestEphemeralPreserve_SnapshotsDuringSequencerState(t *testing.T) {
	for _, state := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		t.Run(state, func(t *testing.T) {
			branch := "polecat/foo/gt-94p1@seq"
			dir, g, opts := ephemeralFixture(t, branch)
			gitDir := gitOut(t, dir, "rev-parse", "--absolute-git-dir")
			switch state {
			case "MERGE_HEAD":
				runGitTestCmd(t, dir, "checkout", "-b", "other", "HEAD")
				writeTestFile(t, dir, "other.txt", "from other\n")
				runGitTestCmd(t, dir, "add", "other.txt")
				runGitTestCmd(t, dir, "commit", "-m", "other work")
				runGitTestCmd(t, dir, "checkout", branch)
				runGitTestCmd(t, dir, "merge", "--no-commit", "--no-ff", "other")
			case "rebase-merge", "rebase-apply":
				if err := os.MkdirAll(filepath.Join(gitDir, state), 0755); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(filepath.Join(gitDir, state), []byte(gitOut(t, dir, "rev-parse", "HEAD")+"\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			writeTestFile(t, dir, "README.md", "# Test\nwork during "+state+"\n")

			before := captureRepoState(t, dir, branch)
			statusBefore := gitOut(t, dir, "status", "--porcelain")
			result, err := AutoPreserveUncommittedWork(g, branch, opts)
			if err != nil || !result.Committed {
				t.Fatalf("a snapshot must be taken during %s, got err=%v result=%+v", state, err, result)
			}
			if got := gitOut(t, dir, "show", result.Commit+":README.md"); got != "# Test\nwork during "+state {
				t.Fatalf("snapshot README.md = %q, want the dirty content", got)
			}
			requireSameRepoState(t, before, captureRepoState(t, dir, branch))
			if got := gitOut(t, dir, "status", "--porcelain"); got != statusBefore {
				t.Errorf("working tree status changed: %q, was %q", got, statusBefore)
			}
			if _, err := os.Stat(filepath.Join(gitDir, state)); err != nil {
				t.Fatalf("%s must be left in place: %v", state, err)
			}
		})
	}
}

// A detached worktree's commits that no branch or remote-tracking ref reaches
// live only in the worktree's own HEAD; removing it would orphan them.
func TestEphemeralPreserve_CleanDetachedHeadWithUnreachableCommitIsPinned(t *testing.T) {
	dir := initTestRepo(t)
	addTestOriginRemote(t, dir)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "--detach")
	writeTestFile(t, dir, "local.txt", "local work\n")
	runGitTestCmd(t, dir, "add", "local.txt")
	runGitTestCmd(t, dir, "commit", "-m", "local commit on a detached HEAD")
	head := gitOut(t, dir, "rev-parse", "HEAD")
	opts := PreserveOptions{IssueID: "furiosa", CommitMessage: testWIPMessage, Ephemeral: true}

	before := captureRepoState(t, dir, "")
	result, err := AutoPreserveUncommittedWork(g, "HEAD", opts)
	if err != nil || !result.Committed || result.Ref == "" {
		t.Fatalf("a clean detached HEAD with an unreachable commit must be pinned, got err=%v result=%+v", err, result)
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, ""))
	if got := gitOut(t, dir, "rev-parse", result.Ref); got != head {
		t.Fatalf("%s = %s, want HEAD %s", result.Ref, got, head)
	}

	// Idempotent: nothing new to record on the next cycle.
	again, err := AutoPreserveUncommittedWork(g, "HEAD", opts)
	if err != nil || again.Committed {
		t.Fatalf("second cycle must be a no-op, got err=%v result=%+v", err, again)
	}
}

func TestEphemeralPreserve_CleanDetachedHeadOnABranchIsNotPinned(t *testing.T) {
	dir := initTestRepo(t)
	addTestOriginRemote(t, dir)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "--detach")

	result, err := AutoPreserveUncommittedWork(g, "HEAD", PreserveOptions{IssueID: "furiosa", CommitMessage: testWIPMessage, Ephemeral: true})
	if err != nil || result.Committed {
		t.Fatalf("a detached HEAD already reachable from a branch needs no pin, got err=%v result=%+v", err, result)
	}
	if refs := gitOut(t, dir, "for-each-ref", "refs/gt/preserve/"); refs != "" {
		t.Fatalf("no ref expected, got:\n%s", refs)
	}
}
