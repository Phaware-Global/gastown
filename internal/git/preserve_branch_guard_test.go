package git

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the three refusals a commit-on-branch auto-save must make
// (gt-2stz): it committed a revert of an approved fix onto a reviewed PR
// branch in graphql-api #162/#166, gastown #251 and heartworks-android #100.

// assertRefusedWithoutCommit fails unless the preserve errored with
// wantInErr and left both HEAD and the branch ref where they were.
func assertRefusedWithoutCommit(t *testing.T, g *Git, branch, headBefore string, res *PreserveResult, err error, wantInErr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal containing %q, got result=%+v", wantInErr, res)
	}
	if !strings.Contains(err.Error(), wantInErr) {
		t.Fatalf("error = %v, want it to contain %q", err, wantInErr)
	}
	if headAfter, _ := g.Rev("HEAD"); headAfter != headBefore {
		t.Fatalf("a commit was created despite the refusal: %s -> %s", headBefore, headAfter)
	}
	if tip, _ := g.Rev("refs/heads/" + branch); tip != headBefore {
		t.Fatalf("branch %s moved despite the refusal: %s -> %s", branch, headBefore, tip)
	}
}

// (a) The remote branch is ahead of HEAD: a commit made here would diverge
// from, or when force-pushed overwrite, what was already reviewed.
func TestAutoPreserveUncommittedWork_RefusesWhenHEADBehindRemoteBranchTip(t *testing.T) {
	localDir, remoteDir, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	commitFiles(t, localDir, map[string]string{"fix.txt": "the approved fix\n"}, "the fix")
	runGitTestCmd(t, localDir, "push", "origin", branch)

	// Another worktree advances the branch on the remote.
	other := filepath.Join(t.TempDir(), "other")
	if out, err := exec.Command("git", "clone", "-b", branch, remoteDir, other).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	runGitTestCmd(t, other, "config", "user.email", "o@test.com")
	runGitTestCmd(t, other, "config", "user.name", "Other")
	commitFiles(t, other, map[string]string{"fix2.txt": "more approved work\n"}, "more work")
	runGitTestCmd(t, other, "push", "origin", branch)

	writeTestFile(t, localDir, "README.md", "# Test\nstale edit\n")
	headBefore, _ := g.Rev("HEAD")

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	assertRefusedWithoutCommit(t, g, branch, headBefore, res, err, "not the tip")
}

// (a) HEAD is on a different branch than the one the caller asked to commit
// onto — the worktree has moved off the branch since the caller looked.
func TestAutoPreserveUncommittedWork_RefusesWhenHEADNotOnTargetBranch(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	commitFiles(t, localDir, map[string]string{"fix.txt": "the approved fix\n"}, "the fix")
	runGitTestCmd(t, localDir, "checkout", mainBranch)

	writeTestFile(t, localDir, "README.md", "# Test\nedit on main\n")
	headBefore, _ := g.Rev("HEAD")

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	if err == nil {
		t.Fatalf("expected a refusal, got result=%+v", res)
	}
	if headAfter, _ := g.Rev("HEAD"); headAfter != headBefore {
		t.Fatalf("a commit landed on the wrong branch: %s -> %s", headBefore, headAfter)
	}
	if tip, _ := g.Rev("refs/heads/" + branch); tip == "" || strings.Contains(tip, headBefore) {
		t.Fatalf("branch tip unexpected: %q", tip)
	}
}

// (a) HEAD detached while the caller names a real branch.
func TestAutoPreserveUncommittedWork_RefusesWhenHEADDetachedFromTargetBranch(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	commitFiles(t, localDir, map[string]string{"fix.txt": "the approved fix\n"}, "the fix")
	branchTip, _ := g.Rev("HEAD")
	runGitTestCmd(t, localDir, "checkout", "--detach", mainBranch)

	writeTestFile(t, localDir, "README.md", "# Test\nedit on detached develop\n")
	headBefore, _ := g.Rev("HEAD")

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	if err == nil {
		t.Fatalf("expected a refusal, got result=%+v", res)
	}
	if headAfter, _ := g.Rev("HEAD"); headAfter != headBefore {
		t.Fatalf("a commit was made on the detached HEAD: %s -> %s", headBefore, headAfter)
	}
	if tip, _ := g.Rev("refs/heads/" + branch); tip != branchTip {
		t.Fatalf("branch moved: %s -> %s", branchTip, tip)
	}
}

// (b) The dirty tree is the base branch's version of a file the branch
// changed: committing it would revert the branch's own work (the android #100
// shape — develop's copy of the files read as "uncommitted work").
func TestAutoPreserveUncommittedWork_RefusesCommitThatRevertsBranchChange(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	commitFiles(t, localDir, map[string]string{"README.md": "# Test\nthe approved fix\n"}, "the fix")
	headBefore, _ := g.Rev("HEAD")

	// Worktree content falls back to the base branch's version.
	writeTestFile(t, localDir, "README.md", "# Test\n")

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	assertRefusedWithoutCommit(t, g, branch, headBefore, res, err, "revert")
}

// (b) A pure removal of lines the branch added is a revert too, even when the
// file does not return byte-for-byte to the base.
func TestAutoPreserveUncommittedWork_RefusesCommitThatDropsBranchAddedLines(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	commitFiles(t, localDir, map[string]string{"README.md": "# Test\nauthorization check one\nauthorization check two\n"}, "the fix")
	headBefore, _ := g.Rev("HEAD")

	writeTestFile(t, localDir, "README.md", "# Test\n")

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	assertRefusedWithoutCommit(t, g, branch, headBefore, res, err, "revert")
}

// Control for (b): building on the branch's own change is the normal case the
// safety net exists for and must still commit.
func TestAutoPreserveUncommittedWork_CommitsEditBuildingOnBranchChange(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	commitFiles(t, localDir, map[string]string{"README.md": "# Test\nfirst draft\n"}, "draft")
	headBefore, _ := g.Rev("HEAD")

	writeTestFile(t, localDir, "README.md", "# Test\nsecond draft, refined\nplus a new line\n")

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	if err != nil {
		t.Fatalf("a refinement of the branch's own work must still be auto-saved: %v", err)
	}
	if !res.Committed {
		t.Fatalf("want Committed, got %+v", res)
	}
	if headAfter, _ := g.Rev("HEAD"); headAfter == headBefore {
		t.Fatal("HEAD did not advance")
	}
}

// (c) .beads/ state is machine-local runtime data: gt-15qi saw a test-polluted
// .beads/config.yaml committed in seven worktrees. It must never ride into an
// auto-save commit, however it got staged.
func TestAutoPreserveUncommittedWork_NeverCommitsBeadsPaths(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	commitFiles(t, localDir, map[string]string{".beads/config.yaml": "dolt:\n  port: 3307\n"}, "track beads config")

	writeTestFile(t, localDir, ".beads/config.yaml", "dolt:\n  port: 3308\n")
	runGitTestCmd(t, localDir, "add", ".beads/config.yaml") // pre-staged by something else
	writeTestFile(t, localDir, "README.md", "# Test\nreal work\n")

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	if err != nil {
		t.Fatalf("real work beside a .beads change should still be saved: %v", err)
	}
	if !res.Committed {
		t.Fatalf("want Committed, got %+v", res)
	}
	out, err := exec.Command("git", "-C", localDir, "show", "--name-only", "--format=", "HEAD").Output()
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	if strings.Contains(string(out), ".beads/") {
		t.Fatalf("auto-save commit touched .beads/:\n%s", out)
	}
	if !strings.Contains(string(out), "README.md") {
		t.Fatalf("auto-save commit lost the real work:\n%s", out)
	}
}

// pushedBranchWithFile creates branch off main, commits each content to
// README.md in order, and publishes it, so HEAD is the remote tip and the
// HEAD-not-branch-tip refusal stays out of the way.
func pushedBranchWithFile(t *testing.T, localDir, branch string, contents ...string) {
	t.Helper()
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	for i, c := range contents {
		commitFiles(t, localDir, map[string]string{"README.md": c}, "commit "+string(rune('1'+i)))
	}
	runGitTestCmd(t, localDir, "push", "origin", branch)
}

// localPreserveRefs lists the local snapshot refs an Ephemeral preserve wrote.
func localPreserveRefs(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "for-each-ref", "--format=%(refname)", "refs/gt/preserve/").Output()
	if err != nil {
		t.Fatalf("for-each-ref: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// (b) The worktree holds an earlier version of the branch's file — the one a
// review fix replaced. The lines it brings back were never in the merge-base,
// so only a comparison against the branch's own history catches it.
func TestAutoPreserveUncommittedWork_RefusesRestoreOfEarlierBranchVersion(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	pushedBranchWithFile(t, localDir, branch,
		"# Test\nallow(user)\n",
		"# Test\nif authorized(user) { allow(user) }\n")
	headBefore, _ := g.Rev("HEAD")

	writeTestFile(t, localDir, "README.md", "# Test\nallow(user)\n")

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	assertRefusedWithoutCommit(t, g, branch, headBefore, res, err, "revert")
}

// (b) The base branch changed the file after the branch was cut and the
// worktree holds the base's current copy (the android #100 shape).
func TestAutoPreserveUncommittedWork_RefusesBaseBranchCopyOfAdvancedFile(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	pushedBranchWithFile(t, localDir, branch, "# Test\nif !authorized(user) { return }\n")
	headBefore, _ := g.Rev("HEAD")

	mainCopy := "# Test\nimport \"log\"\nlog.Println(user)\n"
	runGitTestCmd(t, localDir, "checkout", mainBranch)
	commitFiles(t, localDir, map[string]string{"README.md": mainCopy}, "main moves on")
	runGitTestCmd(t, localDir, "push", "origin", mainBranch)
	runGitTestCmd(t, localDir, "checkout", branch)

	writeTestFile(t, localDir, "README.md", mainCopy)

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	assertRefusedWithoutCommit(t, g, branch, headBefore, res, err, "revert")
}

// (b) Same shape, but the worktree holds only part of what the base added:
// lines the base branch added are base content, not new work, so dropping the
// branch's line while restoring them is still a revert.
func TestAutoPreserveUncommittedWork_RefusesPartialBaseBranchContent(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	pushedBranchWithFile(t, localDir, branch, "# Test\nif !authorized(user) { return }\n")
	headBefore, _ := g.Rev("HEAD")

	runGitTestCmd(t, localDir, "checkout", mainBranch)
	commitFiles(t, localDir, map[string]string{"README.md": "# Test\nimport \"log\"\nlog.Println(user)\n"}, "main moves on")
	runGitTestCmd(t, localDir, "push", "origin", mainBranch)
	runGitTestCmd(t, localDir, "checkout", branch)

	writeTestFile(t, localDir, "README.md", "# Test\nimport \"log\"\n")

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	assertRefusedWithoutCommit(t, g, branch, headBefore, res, err, "revert")
}

// (b) A removal the heuristic cannot tell from a revert — here the branch's
// only change to the file — is refused without claiming the worktree is stale,
// the tracked changes are snapshotted, and the caller is told to commit by hand.
func TestAutoPreserveUncommittedWork_IntentionalRemovalRefusalSnapshotsAndAdvises(t *testing.T) {
	cases := []struct {
		name    string
		branch  string // README.md content committed on the branch
		removed string // worktree content after the removal
	}{
		{"only change is one debug line", "# Test\ndebug print here\n", "# Test\n"},
		{"two debug lines of three added", "# Test\nthe real fix\ndebug line one\ndebug line two\n", "# Test\nthe real fix\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			localDir, _, mainBranch := initTestRepoWithRemote(t)
			g := NewGit(localDir)
			branch := "polecat/foo/gt-2stz@abc123"
			pushedBranchWithFile(t, localDir, branch, tc.branch)
			headBefore, _ := g.Rev("HEAD")

			writeTestFile(t, localDir, "README.md", tc.removed)

			res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
			assertRefusedWithoutCommit(t, g, branch, headBefore, res, err, "committed by hand")
			if strings.Contains(err.Error(), "not new work") {
				t.Fatalf("refusal asserts staleness it cannot know: %v", err)
			}
			ref := localPreserveRefs(t, localDir)
			if ref == "" {
				t.Fatal("no snapshot of the refused removal was recorded")
			}
			if !strings.Contains(err.Error(), ref) {
				t.Fatalf("error %q does not name the snapshot ref %q", err, ref)
			}
			if got, _ := g.run("show", ref+":README.md"); got != strings.TrimRight(tc.removed, "\n") {
				t.Fatalf("snapshot holds %q, want the removal %q", got, tc.removed)
			}
		})
	}
}

// diffLineSets must keep content lines that merely look like diff headers.
func TestDiffLineSets_KeepsContentLinesThatLookLikeHeaders(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/q.sql b/q.sql",
		"index 1111111..2222222 100644",
		"--- a/q.sql",
		"+++ b/q.sql",
		"@@ -2 +2 @@",
		"--- note about backfill",
		"+++counter;",
		"diff --git a/other.sql b/other.sql",
		"--- a/other.sql",
		"+++ b/other.sql",
		"@@ -1 +1 @@",
		"-old statement",
		"+new statement",
	}, "\n")
	added, removed := diffLineSets(diff)
	for _, want := range []string{"++counter;", "new statement"} {
		if !added[want] {
			t.Errorf("added is missing %q: %v", want, added)
		}
	}
	for _, want := range []string{"-- note about backfill", "old statement"} {
		if !removed[want] {
			t.Errorf("removed is missing %q: %v", want, removed)
		}
	}
	for _, header := range []string{"a/q.sql", "b/q.sql", "a/other.sql", "b/other.sql"} {
		if added[header] || removed[header] {
			t.Errorf("file header %q leaked into the line sets", header)
		}
	}
}

// (b) End to end for the same: replacing a branch-added line with one that
// renders as "+++counter;" in a diff is new work layered on the removal, so it
// is saved, not refused as a pure removal.
func TestAutoPreserveUncommittedWork_CommitsRemovalPlusPlusPlusLine(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	pushedBranchWithFile(t, localDir, branch, "# Test\ndelete from staging;\n")
	headBefore, _ := g.Rev("HEAD")

	writeTestFile(t, localDir, "README.md", "# Test\n++counter;\n")

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	if err != nil {
		t.Fatalf("a removal plus a new line is new work: %v", err)
	}
	if !res.Committed {
		t.Fatalf("want Committed, got %+v", res)
	}
	if headAfter, _ := g.Rev("HEAD"); headAfter == headBefore {
		t.Fatal("HEAD did not advance")
	}
}

// (b) The worktree holds the base branch's exact copy of a file the base has
// since advanced. Even when the line-level share is too small to read as a
// revert (one of four branch lines differs), the blob match is conclusive.
func TestAutoPreserveUncommittedWork_RefusesExactBaseBranchCopyBelowLineThreshold(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-2stz@abc123"
	pushedBranchWithFile(t, localDir, branch, "# Test\nline alpha\nline bravo\nline charlie\nline delta\n")
	headBefore, _ := g.Rev("HEAD")

	mainCopy := "# Test\nline alpha\nline bravo\nline charlie\nmain-only line\n"
	runGitTestCmd(t, localDir, "checkout", mainBranch)
	commitFiles(t, localDir, map[string]string{"README.md": mainCopy}, "main moves on")
	runGitTestCmd(t, localDir, "push", "origin", mainBranch)
	runGitTestCmd(t, localDir, "checkout", branch)

	writeTestFile(t, localDir, "README.md", mainCopy)

	res, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{ProtectedBranches: []string{mainBranch}})
	assertRefusedWithoutCommit(t, g, branch, headBefore, res, err, "revert")
}
