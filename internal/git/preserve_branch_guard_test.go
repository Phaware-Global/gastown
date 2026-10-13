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
