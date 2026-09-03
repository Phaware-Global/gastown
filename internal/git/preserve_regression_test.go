package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shimGitFailing puts a fake `git` first on PATH that fails any invocation
// whose full argument string matches the glob failOn (with the index.lock
// error a concurrent git process would produce), and execs the real git for
// everything else. This reproduces the exact partial-failure the preserve
// re-verification exists for: checkpoint_dog and gt done both operate in
// worktrees a live agent may also be using (PR #184 review).
func shimGitFailing(t *testing.T, failOn string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell git shim not portable to windows")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath git: %v", err)
	}
	shimDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  %s) echo "fatal: Unable to create '.git/index.lock': File exists." >&2; exit 128;;
esac
exec %q "$@"
`, failOn, realGit)
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// commitFiles writes and commits the given name->content files.
func commitFiles(t *testing.T, dir string, files map[string]string, msg string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		runGitTestCmd(t, dir, "add", name)
	}
	runGitTestCmd(t, dir, "commit", "-m", msg)
}

// TestAutoPreserveUncommittedWork_RefusesWhenExcludedPathStillStaged pins the
// round-2 refusal semantics that PR #184 round 4 inverted: when an exclusion
// reset fails and a must-never-publish path (here a tracked, modified
// CLAUDE.local.md carrying a secret) is still staged, the whole preserve must
// REFUSE — not wave the excluded path through into a commit that Push:true
// callers then force-push to origin.
func TestAutoPreserveUncommittedWork_RefusesWhenExcludedPathStillStaged(t *testing.T) {
	localDir, remoteDir, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)
	branch := "polecat/foo/gt-y8ts@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	commitFiles(t, localDir, map[string]string{"CLAUDE.local.md": "overlay\n"}, "track overlay")

	if err := os.WriteFile(filepath.Join(localDir, "CLAUDE.local.md"), []byte("SECRET=abc123\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test\nreal work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	headBefore, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev: %v", err)
	}

	// Only the unstage of the excluded overlay fails, exactly as a concurrent
	// index.lock would make it.
	shimGitFailing(t, `"reset HEAD -- CLAUDE.local.md"`)

	result, presErr := AutoPreserveUncommittedWork(g, branch, PreserveOptions{IssueID: "gt-y8ts", Push: true})
	if presErr == nil {
		t.Fatalf("expected refusal when an excluded path is still staged after a failed reset, got result=%+v", result)
	}
	if !strings.Contains(presErr.Error(), "refusing to auto-preserve") {
		t.Fatalf("error = %v, want a refusing-to-auto-preserve error", presErr)
	}
	if headAfter, _ := g.Rev("HEAD"); headAfter != headBefore {
		t.Fatalf("a commit was created despite the refusal: %s -> %s", headBefore, headAfter)
	}
	out, _ := exec.Command("git", "ls-remote", "--heads", remoteDir).Output()
	if strings.Contains(string(out), "preserve") {
		t.Fatalf("a preservation ref reached the remote despite the refusal:\n%s", out)
	}
}

// TestAutoPreserveUncommittedWork_RefusesWhenStagedDeletionSurvivesFailedReset
// pins the deletion half of the same contract: auto-preserve never records a
// deletion of a tracked file, so a staged deletion that survives a failed
// reset must abort the preserve — a deleted tracked file is by definition
// tracked at HEAD, so a tracked-at-HEAD check can never catch it (PR #184
// review).
func TestAutoPreserveUncommittedWork_RefusesWhenStagedDeletionSurvivesFailedReset(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	branch := "polecat/foo/gt-y8ts@abc123"
	runGitTestCmd(t, dir, "checkout", "-b", branch)
	commitFiles(t, dir, map[string]string{"doomed.txt": "keep me\n"}, "track doomed")

	if err := os.Remove(filepath.Join(dir, "doomed.txt")); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nreal work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	headBefore, _ := g.Rev("HEAD")

	shimGitFailing(t, `"reset HEAD -- doomed.txt"`)

	result, presErr := AutoPreserveUncommittedWork(g, branch, PreserveOptions{})
	if presErr == nil {
		t.Fatalf("expected refusal when a staged deletion survives a failed reset, got result=%+v", result)
	}
	if headAfter, _ := g.Rev("HEAD"); headAfter != headBefore {
		t.Fatalf("a commit recording the deletion was created despite the refusal: %s -> %s", headBefore, headAfter)
	}
}

// TestAutoPreserveUncommittedWork_FailsClosedWhenDeletionQueryFails: when the
// staged-deletions query itself fails (concurrent index.lock) and cannot be
// re-verified, the preserve must return an error rather than commit deletions
// it never inspected.
func TestAutoPreserveUncommittedWork_FailsClosedWhenDeletionQueryFails(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	branch := "polecat/foo/gt-y8ts@abc123"
	runGitTestCmd(t, dir, "checkout", "-b", branch)
	commitFiles(t, dir, map[string]string{"doomed.txt": "keep me\n"}, "track doomed")

	if err := os.Remove(filepath.Join(dir, "doomed.txt")); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nreal work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	headBefore, _ := g.Rev("HEAD")

	shimGitFailing(t, `*--diff-filter=D*`)

	result, presErr := AutoPreserveUncommittedWork(g, branch, PreserveOptions{})
	if presErr == nil {
		t.Fatalf("expected an error when staged deletions cannot be queried or re-verified, got result=%+v", result)
	}
	if headAfter, _ := g.Rev("HEAD"); headAfter != headBefore {
		t.Fatalf("a commit was created despite uninspected staged deletions: %s -> %s", headBefore, headAfter)
	}
}

// TestAutoPreserveUncommittedWork_UnstagesQuotedPathPreStagedFile: git
// C-quotes non-ASCII paths in --name-only output (core.quotePath), so
// filtering on that output misses them — a pre-staged `café.env` credential
// sailed through the add -u allowlist scrub while its ASCII sibling was
// caught (PR #184 review). The staged listings must use NUL-delimited
// plumbing.
func TestAutoPreserveUncommittedWork_UnstagesQuotedPathPreStagedFile(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	branch := "polecat/foo/gt-y8ts@abc123"
	runGitTestCmd(t, dir, "checkout", "-b", branch)

	if err := os.WriteFile(filepath.Join(dir, "café.env"), []byte("AWS_SECRET=hunter2\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The agent pre-staged the credential itself, before the safety net ran.
	runGitTestCmd(t, dir, "add", "café.env")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nreal work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Committed {
		t.Fatal("expected the tracked README.md modification to be committed")
	}

	names, err := g.run("show", "--name-only", "--format=", "HEAD")
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	if strings.Contains(names, "caf") {
		t.Fatalf("the pre-staged non-ASCII credential was committed:\n%s", names)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "café.env")); statErr != nil {
		t.Fatalf("café.env must remain in the working tree, just unstaged: %v", statErr)
	}
}

// TestAutoPreserveUncommittedWork_TreeAtHEADDoesNotLegitimizeNewFile: `git
// cat-file -e HEAD:<path>` succeeds when <path> is a TREE at HEAD, so a
// brand-new secret FILE at a former-directory path passed the
// tracked-at-HEAD checks (PR #184 review). In the default path git's own
// D/F-conflict handling happens to evict the new file when the child
// deletion is unstaged — so the bypass matters exactly when that deletion
// reset fails, which is the case exercised here: the re-verification must
// refuse rather than wave both the new `cfg` blob (tree at HEAD) and the
// staged `cfg/a.txt` deletion (blob at HEAD) into a commit.
func TestAutoPreserveUncommittedWork_TreeAtHEADDoesNotLegitimizeNewFile(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	branch := "polecat/foo/gt-y8ts@abc123"
	runGitTestCmd(t, dir, "checkout", "-b", branch)
	commitFiles(t, dir, map[string]string{"cfg/a.txt": "config\n"}, "track cfg dir")

	if err := os.RemoveAll(filepath.Join(dir, "cfg")); err != nil {
		t.Fatalf("rm -rf cfg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cfg"), []byte("AWS_SECRET_ACCESS_KEY=REALSECRET\n"), 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	runGitTestCmd(t, dir, "add", "cfg")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nreal work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	shimGitFailing(t, `"reset HEAD -- cfg/a.txt"`)

	result, presErr := AutoPreserveUncommittedWork(g, branch, PreserveOptions{})

	// Two safe outcomes exist: refuse outright, or scrub the hazards and
	// commit only the legitimate work. What must NEVER happen is the
	// pre-fix outcome — the new `cfg` blob (a tree at HEAD, so it passed
	// the tracked-at-HEAD check) and the staged `cfg/a.txt` deletion both
	// entering the commit.
	if presErr == nil {
		if !result.Committed {
			t.Fatalf("no error and nothing committed, got %+v", result)
		}
		names, err := g.run("show", "--name-only", "--format=", "HEAD")
		if err != nil {
			t.Fatalf("git show: %v", err)
		}
		if strings.TrimSpace(names) != "README.md" {
			t.Fatalf("commit must contain only the legitimate README.md change, got:\n%s", names)
		}
		if content, err := g.ShowFile("HEAD", "cfg/a.txt"); err != nil || content != "config" {
			t.Fatalf("the tracked cfg/a.txt must not be deleted at HEAD (content=%q, err=%v)", content, err)
		}
	}
}

func TestFileTrackedAtHEAD_TreeIsNotATrackedFile(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	commitFiles(t, dir, map[string]string{"cfg/a.txt": "config\n"}, "track cfg dir")

	if !g.FileTrackedAtHEAD("cfg/a.txt") {
		t.Fatal("cfg/a.txt is a blob at HEAD, want tracked=true")
	}
	if g.FileTrackedAtHEAD("cfg") {
		t.Fatal("cfg is a TREE at HEAD, want tracked=false — a tree is not a tracked file")
	}
	if g.FileTrackedAtHEAD("nope.txt") {
		t.Fatal("nope.txt does not exist at HEAD, want tracked=false")
	}
}

// TestAutoPreserveUncommittedWork_UnverifiedCommitGateSurvivesPushFalse: the
// durable Gastown-Unverified gate must run for Push:false callers too — gt
// done and polecat removal push the branch themselves right after this
// returns, and both gate that push only on result.HooksFailed (PR #184
// review).
func TestAutoPreserveUncommittedWork_UnverifiedCommitGateSurvivesPushFalse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook script not portable to windows")
	}
	dir := initTestRepo(t)
	g := NewGit(dir)
	branch := "polecat/foo/gt-y8ts@abc123"
	runGitTestCmd(t, dir, "checkout", "-b", branch)

	hooksDir := filepath.Join(dir, ".git", "hooks")
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("#!/bin/sh\necho 'SECRET DETECTED: AKIAREALSECRET' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nmodified\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Cycle 1: the hook fails, the work is committed unverified.
	res1, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{})
	if err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if !res1.Committed || !res1.HooksFailed {
		t.Fatalf("cycle 1: want Committed && HooksFailed, got %+v", res1)
	}

	// Cycle 2: clean tree, nothing to commit — but HEAD's ancestry still
	// carries the unverified commit, and the caller is about to push the
	// branch itself. HooksFailed must survive.
	res2, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{})
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if !res2.HooksFailed {
		t.Fatalf("cycle 2: want HooksFailed=true for a Push:false caller with an unverified commit in ancestry, got %+v", res2)
	}
	if res2.HookOutput == "" {
		t.Fatal("cycle 2: want HookOutput to explain the refusal")
	}
}

// TestAutoPreserveUncommittedWork_HookOutputIsRedacted: the failing hook is
// typically a secret scanner, and its stderr can contain the very credential
// it matched. Callers print HookOutput into operator terminals, agent
// transcripts, and logs, so the raw hook stderr must never be carried in it —
// point at the worktree instead (PR #184 review).
func TestAutoPreserveUncommittedWork_HookOutputIsRedacted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook script not portable to windows")
	}
	dir := initTestRepo(t)
	g := NewGit(dir)
	branch := "polecat/foo/gt-y8ts@abc123"
	runGitTestCmd(t, dir, "checkout", "-b", branch)

	hooksDir := filepath.Join(dir, ".git", "hooks")
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("#!/bin/sh\necho 'gitleaks: SECRET DETECTED aws_key=AKIAREALSECRET' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nmodified\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.HooksFailed {
		t.Fatalf("want HooksFailed=true, got %+v", result)
	}
	if strings.Contains(result.HookOutput, "AKIAREALSECRET") {
		t.Fatalf("HookOutput carries the scanner-matched secret verbatim: %q", result.HookOutput)
	}
	if !strings.Contains(result.HookOutput, dir) {
		t.Fatalf("HookOutput = %q, want it to point at the worktree so an operator can reproduce the hook output there", result.HookOutput)
	}
}

// TestAutoPreserveUncommittedWork_DetachedRefStablePerAssignment: the
// detached-HEAD preservation ref must be stable across repeated checkpoints
// of one assignment (force-pushing over its own prior state, no unbounded
// ref growth) but MUST differ across assignments of a recycled polecat name —
// otherwise the next bead's checkpoint force-pushes over the previous bead's
// only preserved copy (PR #184 review).
func TestAutoPreserveUncommittedWork_DetachedRefStablePerAssignment(t *testing.T) {
	localDir, remoteDir, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev: %v", err)
	}
	if err := g.CheckoutDetach(base); err != nil {
		t.Fatalf("CheckoutDetach: %v", err)
	}

	// Assignment 1, checkpoint 1.
	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test\nbead A work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res1, err := AutoPreserveUncommittedWork(g, "HEAD", PreserveOptions{IssueID: "furiosa", Push: true})
	if err != nil {
		t.Fatalf("assignment 1 checkpoint 1: %v", err)
	}
	if !res1.Pushed {
		t.Fatalf("assignment 1 checkpoint 1: want Pushed, got %+v", res1)
	}

	// Assignment 1, checkpoint 2 — same assignment, more work: the ref must
	// be the same so the checkpoint overwrites its own prior state.
	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test\nbead A work, more\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res2, err := AutoPreserveUncommittedWork(g, "HEAD", PreserveOptions{IssueID: "furiosa", Push: true})
	if err != nil {
		t.Fatalf("assignment 1 checkpoint 2: %v", err)
	}
	if res2.Ref != res1.Ref {
		t.Fatalf("refs within one assignment must be stable: %q then %q", res1.Ref, res2.Ref)
	}
	assignment1Commit := res2.Commit

	// Reassignment: the pool hands "furiosa" to a different bead; the
	// worktree is reset to base and new work begins.
	runGitTestCmd(t, localDir, "reset", "--hard", base)
	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test\nbead B work\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res3, err := AutoPreserveUncommittedWork(g, "HEAD", PreserveOptions{IssueID: "furiosa", Push: true})
	if err != nil {
		t.Fatalf("assignment 2 checkpoint: %v", err)
	}
	if res3.Ref == res1.Ref {
		t.Fatalf("a reassigned polecat reused ref %q — the previous bead's preserved work would be force-pushed over", res1.Ref)
	}

	// The previous assignment's preserved copy must still be on the remote.
	out, err := exec.Command("git", "ls-remote", "--heads", remoteDir, "refs/heads/"+res1.Ref).Output()
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 || fields[0] != assignment1Commit {
		t.Fatalf("assignment 1's preserved commit %s is gone from %s (ls-remote: %q)", assignment1Commit, res1.Ref, out)
	}
}
