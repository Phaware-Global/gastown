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
	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Committed || !result.Pushed {
		t.Fatalf("expected a pushed snapshot, got %+v", result)
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, branch))

	// The snapshot is the WORKTREE content, a child of the untouched HEAD,
	// and it is what the preservation ref holds.
	if got := gitOut(t, dir, "show", result.Commit+":README.md"); got != "# Test\nstaged\nunstaged" {
		t.Errorf("snapshot README.md = %q, want the worktree content", got)
	}
	if parent := gitOut(t, dir, "rev-parse", result.Commit+"^"); parent != before.head {
		t.Errorf("snapshot parent = %s, want HEAD %s", parent, before.head)
	}
	if subj := gitOut(t, dir, "log", "-1", "--format=%s", result.Commit); subj != testWIPMessage {
		t.Errorf("snapshot subject = %q, want %q", subj, testWIPMessage)
	}
	if tip, err := g.RemoteBranchTip("origin", result.Ref); err != nil || tip != result.Commit {
		t.Errorf("preservation ref tip = %q (err %v), want %s", tip, err, result.Commit)
	}
	// The work is still uncommitted where the agent left it.
	if gitOut(t, dir, "status", "--porcelain") == "" {
		t.Error("the agent's uncommitted work must remain uncommitted")
	}
}

func TestEphemeralPreserve_MidMergeKeepsMergeHeadAndParents(t *testing.T) {
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
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Pushed {
		t.Fatalf("expected a pushed snapshot, got %+v", result)
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, branch))

	runGitTestCmd(t, dir, "commit", "-am", "complete the merge")
	if parents := strings.Fields(gitOut(t, dir, "rev-list", "--parents", "-n1", "HEAD")); len(parents) != 3 {
		t.Fatalf("the agent's merge commit has %d parents, want 2 — the merge was flattened", len(parents)-1)
	}
}

func TestEphemeralPreserve_FailedPushLeavesBranchUntouched(t *testing.T) {
	branch := "polecat/foo/gt-94p1@pushfail"
	dir, g, opts := ephemeralFixture(t, branch)
	writeTestFile(t, dir, "README.md", "# Test\nwork\n")

	goodURL := gitOut(t, dir, "remote", "get-url", "origin")
	runGitTestCmd(t, dir, "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "no-such-remote.git"))

	before := captureRepoState(t, dir, branch)
	if _, err := AutoPreserveUncommittedWork(g, branch, opts); err == nil {
		t.Fatal("expected the push to fail")
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, branch))

	// A later cycle with the remote back must not find a stranded snapshot.
	runGitTestCmd(t, dir, "remote", "set-url", "--push", "origin", goodURL)
	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil || !result.Pushed {
		t.Fatalf("retry: result %+v, err %v", result, err)
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, branch))
	if gitOut(t, dir, "log", "--format=%s", "HEAD") == testWIPMessage {
		t.Fatal("the branch carries a WIP commit")
	}
}

func TestEphemeralPreserve_ConcurrentAgentCommitIsNeverDropped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook script not portable to windows")
	}
	branch := "polecat/foo/gt-94p1@race"
	dir, g, opts := ephemeralFixture(t, branch)
	writeTestFile(t, dir, "README.md", "# Test\nwork\n")

	// Stands in for the live agent: it commits during the preserve's push
	// window, the interval in which a rollback-based design loses it.
	hook := "#!/bin/sh\nunset GIT_DIR GIT_INDEX_FILE GIT_WORK_TREE\ngit -C '" + dir + "' commit --allow-empty -q -m 'agent real commit'\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-push"), []byte(hook), 0755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	before := captureRepoState(t, dir, branch)
	if _, err := AutoPreserveUncommittedWork(g, branch, opts); err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if got := gitOut(t, dir, "log", "-1", "--format=%s", "HEAD"); got != "agent real commit" {
		t.Fatalf("HEAD subject = %q — the agent's concurrent commit was dropped", got)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD^"); got != before.head {
		t.Fatalf("agent commit's parent = %s, want %s", got, before.head)
	}
}

func TestEphemeralPreserve_DetachedHeadReusesOneRefAcrossCycles(t *testing.T) {
	dir := initTestRepo(t)
	protected := addTestOriginRemote(t, dir)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "--detach")
	opts := PreserveOptions{IssueID: "furiosa", Push: true, CommitMessage: testWIPMessage, ProtectedBranches: []string{protected}, Ephemeral: true}

	writeTestFile(t, dir, "README.md", "# Test\ncycle one\n")
	first, err := AutoPreserveUncommittedWork(g, "HEAD", opts)
	if err != nil || !first.Pushed {
		t.Fatalf("cycle 1: %+v, err %v", first, err)
	}
	writeTestFile(t, dir, "README.md", "# Test\ncycle two\n")
	second, err := AutoPreserveUncommittedWork(g, "HEAD", opts)
	if err != nil || !second.Pushed {
		t.Fatalf("cycle 2: %+v, err %v", second, err)
	}
	if first.Ref != second.Ref {
		t.Fatalf("cycle refs differ: %s vs %s — a new permanent remote ref per cycle", first.Ref, second.Ref)
	}
	if second.Commit == first.Commit {
		t.Fatal("cycle 2 should have pushed a fresh snapshot of the changed work")
	}
	remote := gitOut(t, dir, "remote", "get-url", "origin")
	if refs := gitOut(t, dir, "ls-remote", "--heads", remote, "polecat/preserve-detached-*"); len(strings.Split(refs, "\n")) != 1 {
		t.Fatalf("want exactly one detached preservation ref on the remote, got:\n%s", refs)
	}
}

func TestEphemeralPreserve_UnchangedWorkIsNotRepushedEachCycle(t *testing.T) {
	branch := "polecat/foo/gt-94p1@dedupe"
	dir, g, opts := ephemeralFixture(t, branch)
	writeTestFile(t, dir, "README.md", "# Test\nwork\n")

	first, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil || !first.Pushed {
		t.Fatalf("first: %+v, err %v", first, err)
	}
	second, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Pushed || second.Committed {
		t.Fatalf("an unchanged dirty tree must not be re-snapshotted, got %+v", second)
	}
	if tip, _ := g.RemoteBranchTip("origin", first.Ref); tip != first.Commit {
		t.Fatalf("preservation ref moved to %s, want it left at %s", tip, first.Commit)
	}
}

func TestEphemeralPreserve_SnapshotIsAnAllowlistOfTrackedModifications(t *testing.T) {
	branch := "polecat/foo/gt-94p1@allow"
	dir, g, opts := ephemeralFixture(t, branch)
	writeTestFile(t, dir, "notes.txt", "notes\n")
	writeTestFile(t, dir, "CLAUDE.local.md", "overlay\n")
	writeTestFile(t, dir, "extra.txt", "extra\n")
	runGitTestCmd(t, dir, "add", "notes.txt", "CLAUDE.local.md", "extra.txt")
	runGitTestCmd(t, dir, "commit", "-m", "track files")
	head := gitOut(t, dir, "rev-parse", "HEAD")

	writeTestFile(t, dir, "README.md", "# Test\nedited\n") // kept
	writeTestFile(t, dir, "CLAUDE.local.md", "changed\n")  // runtime artifact
	writeTestFile(t, dir, "extra.txt", "changed\n")        // ExtraExcludePaths
	if err := os.Remove(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, ".env", "SECRET=1\n")      // untracked
	writeTestFile(t, dir, "staged-new.txt", "new\n") // added to the index only
	runGitTestCmd(t, dir, "add", "staged-new.txt")
	opts.ExtraExcludePaths = []string{"extra.txt"}

	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil || !result.Pushed {
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

func TestEphemeralPreserve_PreservesAgentsOwnUnpushedCommits(t *testing.T) {
	branch := "polecat/foo/gt-94p1@real"
	dir, g, opts := ephemeralFixture(t, branch)
	writeTestFile(t, dir, "real.txt", "real work\n")
	runGitTestCmd(t, dir, "add", "real.txt")
	runGitTestCmd(t, dir, "commit", "-m", "real work, not a checkpoint")

	before := captureRepoState(t, dir, branch)
	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if result.Committed || !result.Pushed || result.Commit != before.head {
		t.Fatalf("want the agent's own commit pushed to the preservation ref without a snapshot, got %+v", result)
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, branch))
}

func TestEphemeralPreserve_WithoutPushPinsSnapshotUnderLocalRef(t *testing.T) {
	branch := "polecat/foo/gt-94p1@nopush"
	dir, g, opts := ephemeralFixture(t, branch)
	opts.Push = false
	writeTestFile(t, dir, "README.md", "# Test\nwork\n")

	before := captureRepoState(t, dir, branch)
	result, err := AutoPreserveUncommittedWork(g, branch, opts)
	if err != nil || !result.Committed || result.Pushed {
		t.Fatalf("result %+v, err %v", result, err)
	}
	requireSameRepoState(t, before, captureRepoState(t, dir, branch))
	if got := gitOut(t, dir, "rev-parse", "refs/preserve/"+PreservationRefName(branch)); got != result.Commit {
		t.Fatalf("local preserve ref = %s, want %s", got, result.Commit)
	}
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

func TestEphemeralPreserve_RequiresWIPCommitMessage(t *testing.T) {
	branch := "polecat/foo/gt-94p1@nomsg"
	dir, g, opts := ephemeralFixture(t, branch)
	opts.CommitMessage = ""
	writeTestFile(t, dir, "README.md", "# Test\nwork\n")
	if _, err := AutoPreserveUncommittedWork(g, branch, opts); err == nil {
		t.Fatal("an Ephemeral preserve without a CommitMessage would create a snapshot the WIP guard cannot recognize")
	}
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
