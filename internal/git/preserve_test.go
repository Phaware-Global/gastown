package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// addTestOriginRemote adds a bare "origin" remote to dir and pushes its
// current branch, returning that branch's name. Tests that don't exercise
// the unverified-commit guard's protected-branch behavior still need a
// resolvable origin ref to satisfy it now that it fails closed (gt-35un/
// gt-xitk) — call this before checking out any feature branch, so the
// pushed name is the repo's real base, not the branch under test.
func addTestOriginRemote(t *testing.T, dir string) string {
	t.Helper()
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if err := exec.Command("git", "init", "--bare", remoteDir).Run(); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}
	runGitTestCmd(t, dir, "remote", "add", "origin", remoteDir)
	branch, err := NewGit(dir).CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	runGitTestCmd(t, dir, "push", "-u", "origin", branch)
	return branch
}

func TestAutoPreserveUncommittedWork_CommitsDirtyWork(t *testing.T) {
	dir := initTestRepo(t)
	protected := addTestOriginRemote(t, dir)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "-b", "polecat/foo/gt-y8ts@abc123")

	// Modify a file already tracked by initTestRepo's initial commit: staging
	// is now `git add -u` (allowlist, gt-i4ej FIX 1), which only picks up
	// changes to files git already tracks, never new untracked files.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nmodified\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, "polecat/foo/gt-y8ts@abc123", PreserveOptions{IssueID: "gt-y8ts", ProtectedBranches: []string{protected}})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Committed {
		t.Fatal("expected Committed=true for a dirty source file")
	}
	if result.Pushed {
		t.Fatal("expected Pushed=false when Push option is not set")
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if status.HasUncommittedChanges {
		t.Fatalf("expected clean tree after auto-preserve, got: %s", status.String())
	}

	msg, err := g.GetBranchCommitMessage("HEAD")
	if err != nil {
		t.Fatalf("GetBranchCommitMessage: %v", err)
	}
	if !strings.Contains(msg, "gt-y8ts") || !strings.Contains(msg, "gt-pvx safety net") {
		t.Fatalf("commit message = %q, want it to mention gt-y8ts and gt-pvx safety net", msg)
	}
}

func TestAutoPreserveUncommittedWork_ExcludesRuntimeArtifacts(t *testing.T) {
	dir := initTestRepo(t)
	protected := addTestOriginRemote(t, dir)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "-b", "polecat/foo/gt-y8ts@abc123")

	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "state.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, "polecat/foo/gt-y8ts@abc123", PreserveOptions{ProtectedBranches: []string{protected}})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if result.Committed {
		t.Fatal("expected Committed=false when only runtime artifacts are dirty")
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if !status.HasUncommittedChanges {
		t.Fatal("runtime artifact must remain uncommitted in the working tree, not be silently dropped")
	}
}

func TestAutoPreserveUncommittedWork_ExtraExcludePaths(t *testing.T) {
	dir := initTestRepo(t)
	protected := addTestOriginRemote(t, dir)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "-b", "polecat/foo/gt-y8ts@abc123")

	// CLAUDE.md must already be tracked for `git add -u` (gt-i4ej FIX 1) to
	// even stage its modification — this test exercises ExtraExcludePaths
	// unstaging an already-tracked overlay file, not skipping a new one.
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("overlay\n"), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	runGitTestCmd(t, dir, "add", "CLAUDE.md")
	runGitTestCmd(t, dir, "commit", "-m", "add CLAUDE.md")

	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("overlay v2\n"), 0644); err != nil {
		t.Fatalf("modify CLAUDE.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nreal change\n"), 0644); err != nil {
		t.Fatalf("modify README.md: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, "polecat/foo/gt-y8ts@abc123", PreserveOptions{
		ExtraExcludePaths: []string{"CLAUDE.md"},
		ProtectedBranches: []string{protected},
	})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Committed {
		t.Fatal("expected Committed=true — README.md is not excluded")
	}

	// Check exact committed/worktree content directly rather than via
	// Status()/CheckUncommittedWork(): when CLAUDE.md ends up as the sole
	// remaining dirty file, its porcelain line (" M CLAUDE.md") is the
	// entire "git status --porcelain" output, and g.run's whole-output
	// TrimSpace strips that line's leading space along with it — corrupting
	// the 2-char XY code parse for exactly this single-remaining-file case
	// (pre-existing bug, unrelated to gt-i4ej; filed separately).
	committedReadme, err := g.ShowFile("HEAD", "README.md")
	if err != nil {
		t.Fatalf("ShowFile README.md: %v", err)
	}
	if committedReadme != "# Test\nreal change" {
		t.Fatalf("committed README.md = %q, want the modified content", committedReadme)
	}
	committedClaude, err := g.ShowFile("HEAD", "CLAUDE.md")
	if err != nil {
		t.Fatalf("ShowFile CLAUDE.md: %v", err)
	}
	if committedClaude != "overlay" {
		t.Fatalf("committed CLAUDE.md = %q, want it unchanged (excluded from the commit)", committedClaude)
	}
	worktreeClaude, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if string(worktreeClaude) != "overlay v2\n" {
		t.Fatalf("worktree CLAUDE.md = %q, want the modification left uncommitted", worktreeClaude)
	}
}

// TestAutoPreserveUncommittedWork_NewUntrackedFileIsNotCaptured pins the
// deliberate tradeoff of gt-i4ej FIX 1: staging moved from `git add -A`
// (denylist — stages every untracked file, then unstages known runtime
// paths) to `git add -u` (allowlist — stages only changes to files git
// already tracks). A denylist can never enumerate what it has never seen
// (a .env, a dumped token, a credentials file), and this safety net runs
// unattended and can force-push its result to origin, making the old
// behavior an automated secret-publication path. The accepted cost: a
// brand-new untracked source file is not preserved by this safety net.
func TestAutoPreserveUncommittedWork_NewUntrackedFileIsNotCaptured(t *testing.T) {
	dir := initTestRepo(t)
	protected := addTestOriginRemote(t, dir)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "-b", "polecat/foo/gt-y8ts@abc123")

	if err := os.WriteFile(filepath.Join(dir, "new_secret_looking_file.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, "polecat/foo/gt-y8ts@abc123", PreserveOptions{ProtectedBranches: []string{protected}})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if result.Committed {
		t.Fatal("expected Committed=false — a new untracked file must never be staged by the allowlist")
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if len(status.UntrackedFiles) != 1 || status.UntrackedFiles[0] != "new_secret_looking_file.go" {
		t.Fatalf("expected the untracked file to remain untouched in the worktree, got: %s", status.String())
	}
}

func TestAutoPreserveUncommittedWork_RefusesProtectedBranch(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)

	current, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if !IsProtectedBranch(current) {
		t.Skipf("test repo default branch %q is not in protectedBranchSet", current)
	}

	if err := os.WriteFile(filepath.Join(dir, "dirty.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, current, PreserveOptions{})
	if err == nil {
		t.Fatal("expected error auto-preserving on protected branch")
	}
	if !strings.Contains(err.Error(), "G41 guard") {
		t.Fatalf("error = %v, want it to mention the G41 guard", err)
	}
	if result.Committed {
		t.Fatal("must not commit on a protected branch")
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if !status.HasUncommittedChanges {
		t.Fatal("work must remain uncommitted in the worktree after refusal")
	}
}

func TestAutoPreserveUncommittedWork_RefusesUnmergedConflicts(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)

	runGitTestCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("feature\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitTestCmd(t, dir, "add", "conflict.txt")
	runGitTestCmd(t, dir, "commit", "-m", "feature change")

	runGitTestCmd(t, dir, "checkout", "-")
	if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("main\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitTestCmd(t, dir, "add", "conflict.txt")
	runGitTestCmd(t, dir, "commit", "-m", "main change")

	runGitTestCmd(t, dir, "checkout", "feature")
	// git merge <previous-branch> is expected to conflict here.
	runGitTestCmdWantFailure(t, dir, "merge", "-")

	result, err := AutoPreserveUncommittedWork(g, "feature", PreserveOptions{})
	if err == nil {
		t.Fatal("expected error auto-preserving with unmerged conflicts")
	}
	if !strings.Contains(err.Error(), "conflict.txt") {
		t.Fatalf("error = %v, want it to name conflict.txt", err)
	}
	if result.Committed {
		t.Fatal("must not commit unresolved conflicts")
	}
}

func TestAutoPreserveUncommittedWork_PushesAndVerifies(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	branch := "polecat/foo/gt-y8ts@abc123"
	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	// Modify a tracked file — `git add -u` (gt-i4ej FIX 1) never stages new
	// untracked files.
	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test\nmodified\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{IssueID: "gt-y8ts", Push: true, ProtectedBranches: []string{mainBranch}})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Committed || !result.Pushed {
		t.Fatalf("expected Committed and Pushed both true, got %+v", result)
	}
	wantRef := "polecat/preserve-" + strings.ReplaceAll(branch, "/", "-")
	if result.Ref != wantRef {
		t.Fatalf("Ref = %q, want %q", result.Ref, wantRef)
	}

	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev: %v", err)
	}
	if err := g.VerifyPushedCommit("origin", wantRef, head); err != nil {
		t.Fatalf("VerifyPushedCommit: %v", err)
	}

	// The branch itself must NOT have been pushed — only the dedicated
	// preservation ref. A mid-work checkpoint must never disturb a branch
	// that might already have an open PR / CI / review state on it.
	if tip, tipErr := g.PushRemoteBranchTip("origin", branch); tipErr == nil && tip != "" {
		t.Fatalf("branch %s was pushed to origin, want only the preserve ref touched", branch)
	}

	// A second call with nothing new to preserve should be a no-op.
	result2, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{IssueID: "gt-y8ts", Push: true, ProtectedBranches: []string{mainBranch}})
	if err != nil {
		t.Fatalf("second AutoPreserveUncommittedWork: %v", err)
	}
	if result2.Committed || result2.Pushed {
		t.Fatalf("expected no-op on second call with nothing new, got %+v", result2)
	}
}

func TestAutoPreserveUncommittedWork_CommitMessageOverride(t *testing.T) {
	dir := initTestRepo(t)
	protected := addTestOriginRemote(t, dir)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "-b", "polecat/foo/gt-y8ts@abc123")

	// Modify a tracked file — `git add -u` (gt-i4ej FIX 1) never stages new
	// untracked files.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nmodified\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := AutoPreserveUncommittedWork(g, "polecat/foo/gt-y8ts@abc123", PreserveOptions{
		IssueID:           "gt-y8ts", // must be ignored when CommitMessage is set
		CommitMessage:     "WIP: checkpoint (auto)",
		ProtectedBranches: []string{protected},
	})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}

	msg, err := g.GetBranchCommitMessage("HEAD")
	if err != nil {
		t.Fatalf("GetBranchCommitMessage: %v", err)
	}
	if msg != "WIP: checkpoint (auto)" {
		t.Fatalf("commit message = %q, want exact override %q", msg, "WIP: checkpoint (auto)")
	}
}

// TestAutoPreserveUncommittedWork_HookFailureBlocksPushButPreservesLocally
// covers gt-i4ej FIX 2: hooks gate the push, not the commit. A failing
// pre-commit hook (e.g. a secret scanner) must never turn preservation into
// a no-op — the work is still committed locally — but the commit it produced
// was never verified, so it must not reach origin unattended.
func TestAutoPreserveUncommittedWork_HookFailureBlocksPushButPreservesLocally(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook script not portable to windows")
	}
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	branch := "polecat/foo/gt-y8ts@abc123"
	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	hooksDir := filepath.Join(localDir, ".git", "hooks")
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test\nmodified\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{IssueID: "gt-y8ts", Push: true})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Committed {
		t.Fatal("expected work to still be committed locally even though the hook failed")
	}
	if !result.HooksFailed {
		t.Fatal("expected HooksFailed=true")
	}
	if result.HookOutput == "" {
		t.Fatal("expected HookOutput to carry the failing hook's output for the caller to escalate")
	}
	if result.Pushed {
		t.Fatal("expected Pushed=false — an unverified commit must never be pushed")
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if status.HasUncommittedChanges {
		t.Fatalf("expected the tree to be clean (committed locally), got: %s", status.String())
	}

	if tip, tipErr := g.PushRemoteBranchTip("origin", PreservationRefName(branch)); tipErr == nil && tip != "" {
		t.Fatalf("preservation ref was pushed to origin despite a failing hook: %s", tip)
	}
}

// TestAutoPreserveUncommittedWork_DetachedHEADGetsUniqueRef covers gt-i4ej
// FIX 3. A detached HEAD reports as literal "HEAD", which is the normal
// idle state for a polecat worktree — collapsing every detached worktree
// onto the shared "polecat/preserve-HEAD" ref would make two different
// polecats' preserved work collide on the same ref.
func TestAutoPreserveUncommittedWork_DetachedHEADGetsUniqueRef(t *testing.T) {
	localDir, _, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev: %v", err)
	}
	if err := g.CheckoutDetach(head); err != nil {
		t.Fatalf("CheckoutDetach: %v", err)
	}
	branch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "HEAD" {
		t.Fatalf("branch = %q, want detached HEAD to report as %q", branch, "HEAD")
	}

	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test\nmodified\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{IssueID: "rictus", Push: true, ProtectedBranches: []string{mainBranch}})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Committed || !result.Pushed {
		t.Fatalf("expected Committed and Pushed both true, got %+v", result)
	}
	if result.Ref == PreservationRefName("HEAD") {
		t.Fatalf("ref = %q must not be the shared literal-HEAD ref every detached polecat would collide on", result.Ref)
	}
	if !strings.Contains(result.Ref, "rictus") {
		t.Fatalf("ref = %q, want it to include the caller's identity (rictus)", result.Ref)
	}
}

// TestDetachedPreservationIdentity exercises the identity-resolution helper
// directly: IssueID takes precedence when set, the worktree directory name
// is a usable fallback, and an unresolvable case (gt-i4ej FIX 3) is
// rejected rather than guessed.
func TestDetachedPreservationIdentity(t *testing.T) {
	g := NewGit("/some/worktree/rictus")

	id, err := detachedPreservationIdentity(g, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "rictus" {
		t.Fatalf("identity = %q, want %q (fallback to workdir basename)", id, "rictus")
	}

	id2, err := detachedPreservationIdentity(g, "gt-y8ts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id2 != "gt-y8ts" {
		t.Fatalf("identity = %q, want IssueID to take precedence over the workdir fallback", id2)
	}

	gEmpty := NewGit("")
	if _, err := detachedPreservationIdentity(gEmpty, ""); err == nil {
		t.Fatal("expected an error when neither IssueID nor a resolvable workdir is available")
	}
}

// TestAutoPreserveUncommittedWork_EphemeralNeverAdvancesBranch pins gt-94p1
// requirement 1: a checkpoint auto-save must never move a branch's HEAD.
// Without Ephemeral, this commit would sit on the branch itself — and ANY
// later ordinary push (the polecat's own push, gt done, a pre-nuke push)
// would carry it to a real, possibly already-reviewed PR branch (the
// mechanism behind graphql-api #162/#166 and gastown #250/#251, gt-2stz).
func TestAutoPreserveUncommittedWork_EphemeralNeverAdvancesBranch(t *testing.T) {
	dir := initTestRepo(t)
	protected := addTestOriginRemote(t, dir)
	g := NewGit(dir)
	branch := "polecat/foo/gt-94p1@abc123"
	runGitTestCmd(t, dir, "checkout", "-b", branch)

	origHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev HEAD: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\nwip content\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{
		IssueID:           "gt-94p1",
		Push:              true,
		CommitMessage:     "WIP: checkpoint (auto)",
		ProtectedBranches: []string{protected},
		Ephemeral:         true,
	})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Committed {
		t.Fatal("expected Committed=true for a dirty source file")
	}
	if !result.Pushed {
		t.Fatal("expected Pushed=true")
	}

	newHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev HEAD after preserve: %v", err)
	}
	if newHead != origHead {
		t.Fatalf("branch HEAD = %s, want unchanged at %s — Ephemeral must never advance the branch", newHead, origHead)
	}
	currentBranch, err := g.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if currentBranch != branch {
		t.Fatalf("checked-out branch changed to %q, want still %q", currentBranch, branch)
	}

	// The snapshot must still be durably reachable — just not from the branch.
	tip, err := g.RemoteBranchTip("origin", result.Ref)
	if err != nil {
		t.Fatalf("RemoteBranchTip(%s): %v", result.Ref, err)
	}
	if tip != result.Commit {
		t.Fatalf("preservation ref %s tip = %s, want %s", result.Ref, tip, result.Commit)
	}

	// The work itself is not lost: still present (now uncommitted again),
	// ready to be retried or committed for real.
	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if !status.HasUncommittedChanges {
		t.Fatal("expected the preserved change to remain staged/uncommitted on the worktree after an Ephemeral preserve")
	}
}

// TestAutoPreserveUncommittedWork_EphemeralLeavesRealCommitsAlone ensures
// Ephemeral only rolls back a commit THIS CALL made — pre-existing commits
// the agent made itself are real work and must reach the preservation ref
// (and stay reachable from the branch) untouched.
func TestAutoPreserveUncommittedWork_EphemeralLeavesRealCommitsAlone(t *testing.T) {
	dir := initTestRepo(t)
	protected := addTestOriginRemote(t, dir)
	g := NewGit(dir)
	branch := "polecat/foo/gt-94p1@def456"
	runGitTestCmd(t, dir, "checkout", "-b", branch)

	// The agent's own real, already-committed work.
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("real work\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitTestCmd(t, dir, "add", "real.txt")
	runGitTestCmd(t, dir, "commit", "-m", "real work, not a checkpoint")
	realHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev HEAD: %v", err)
	}

	// Nothing uncommitted this cycle.
	result, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{
		Push:              true,
		ProtectedBranches: []string{protected},
		Ephemeral:         true,
	})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if result.Committed {
		t.Fatal("expected Committed=false — nothing was uncommitted")
	}
	if !result.Pushed {
		t.Fatal("expected the agent's real unpushed commit to still be pushed to the preservation ref")
	}

	newHead, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev HEAD after preserve: %v", err)
	}
	if newHead != realHead {
		t.Fatalf("branch HEAD = %s, want %s — Ephemeral must never roll back the agent's own real commit", newHead, realHead)
	}
}

func TestHasWIPCommit_FlagsOwnUnmergedCommit(t *testing.T) {
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	runGitTestCmd(t, localDir, "checkout", "-b", "develop")
	runGitTestCmd(t, localDir, "push", "-u", "origin", "develop")

	runGitTestCmd(t, localDir, "checkout", "-b", "polecat/foo/gt-94p1@abc123")
	if err := os.WriteFile(filepath.Join(localDir, "wip.txt"), []byte("wip\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitTestCmd(t, localDir, "add", "wip.txt")
	runGitTestCmd(t, localDir, "commit", "-m", "WIP: checkpoint (auto) (gt-94p1)")
	wipSHA, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev HEAD: %v", err)
	}

	got, err := HasWIPCommit(g, "origin", "WIP: checkpoint (auto)", []string{"develop"})
	if err != nil {
		t.Fatalf("HasWIPCommit: %v", err)
	}
	if got != wipSHA {
		t.Fatalf("HasWIPCommit = %q, want %q", got, wipSHA)
	}
}

func TestHasWIPCommit_ExemptsCommitAlreadyOnProtectedBranch(t *testing.T) {
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	runGitTestCmd(t, localDir, "checkout", "-b", "develop")
	if err := os.WriteFile(filepath.Join(localDir, "wip.txt"), []byte("wip\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitTestCmd(t, localDir, "add", "wip.txt")
	runGitTestCmd(t, localDir, "commit", "-m", "WIP: checkpoint (auto) (gt-94p1)")
	runGitTestCmd(t, localDir, "push", "-u", "origin", "develop")

	runGitTestCmd(t, localDir, "checkout", "-b", "polecat/foo/gt-94p1@abc123", "develop")
	runGitTestCmd(t, localDir, "fetch", "origin")

	got, err := HasWIPCommit(g, "origin", "WIP: checkpoint (auto)", []string{"develop"})
	if err != nil {
		t.Fatalf("HasWIPCommit: %v", err)
	}
	if got != "" {
		t.Fatalf("HasWIPCommit = %q, want \"\" — a WIP commit already merged to a protected branch is accepted history, not a leak in progress", got)
	}
}

func TestHasWIPCommit_CleanBranchReturnsEmpty(t *testing.T) {
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	runGitTestCmd(t, localDir, "checkout", "-b", "develop")
	runGitTestCmd(t, localDir, "push", "-u", "origin", "develop")
	runGitTestCmd(t, localDir, "checkout", "-b", "polecat/foo/gt-94p1@abc123")
	if err := os.WriteFile(filepath.Join(localDir, "real.txt"), []byte("real\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitTestCmd(t, localDir, "add", "real.txt")
	runGitTestCmd(t, localDir, "commit", "-m", "real work")

	got, err := HasWIPCommit(g, "origin", "WIP: checkpoint (auto)", []string{"develop"})
	if err != nil {
		t.Fatalf("HasWIPCommit: %v", err)
	}
	if got != "" {
		t.Fatalf("HasWIPCommit = %q, want \"\" for a branch with no checkpoint commits", got)
	}
}

func TestCommitNoVerify_BypassesHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook script not portable to windows")
	}
	dir := initTestRepo(t)
	g := NewGit(dir)

	hooksDir := filepath.Join(dir, ".git", "hooks")
	if err := os.WriteFile(filepath.Join(hooksDir, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "blocked.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.Add("blocked.go"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := g.Commit("should be blocked by hook"); err == nil {
		t.Fatal("expected Commit to fail against a rejecting pre-commit hook")
	}

	if err := g.CommitNoVerify("bypasses hook"); err != nil {
		t.Fatalf("CommitNoVerify should bypass the hook: %v", err)
	}

	msg, err := g.GetBranchCommitMessage("HEAD")
	if err != nil {
		t.Fatalf("GetBranchCommitMessage: %v", err)
	}
	if msg != "bypasses hook" {
		t.Fatalf("commit message = %q, want %q", msg, "bypasses hook")
	}
}
