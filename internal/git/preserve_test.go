package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAutoPreserveUncommittedWork_CommitsDirtyWork(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "-b", "polecat/foo/gt-y8ts@abc123")

	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, "polecat/foo/gt-y8ts@abc123", PreserveOptions{IssueID: "gt-y8ts"})
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
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "-b", "polecat/foo/gt-y8ts@abc123")

	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "state.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, "polecat/foo/gt-y8ts@abc123", PreserveOptions{})
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
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "-b", "polecat/foo/gt-y8ts@abc123")

	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("overlay\n"), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatalf("write real.go: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, "polecat/foo/gt-y8ts@abc123", PreserveOptions{
		ExtraExcludePaths: []string{"CLAUDE.md"},
	})
	if err != nil {
		t.Fatalf("AutoPreserveUncommittedWork: %v", err)
	}
	if !result.Committed {
		t.Fatal("expected Committed=true — real.go is not excluded")
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if len(status.ModifiedFiles)+len(status.UntrackedFiles) != 1 || !status.HasUncommittedChanges {
		t.Fatalf("expected only CLAUDE.md to remain uncommitted, got: %s", status.String())
	}
	if got := status.NonRuntimePaths(); len(got) != 1 || got[0] != "CLAUDE.md" {
		t.Fatalf("NonRuntimePaths = %v, want [CLAUDE.md]", got)
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
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	branch := "polecat/foo/gt-y8ts@abc123"
	if err := g.CreateBranch(branch); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := g.Checkout(branch); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "handler.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{IssueID: "gt-y8ts", Push: true})
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
	result2, err := AutoPreserveUncommittedWork(g, branch, PreserveOptions{IssueID: "gt-y8ts", Push: true})
	if err != nil {
		t.Fatalf("second AutoPreserveUncommittedWork: %v", err)
	}
	if result2.Committed || result2.Pushed {
		t.Fatalf("expected no-op on second call with nothing new, got %+v", result2)
	}
}

func TestAutoPreserveUncommittedWork_CommitMessageOverride(t *testing.T) {
	dir := initTestRepo(t)
	g := NewGit(dir)
	runGitTestCmd(t, dir, "checkout", "-b", "polecat/foo/gt-y8ts@abc123")

	if err := os.WriteFile(filepath.Join(dir, "handler.go"), []byte("package x\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := AutoPreserveUncommittedWork(g, "polecat/foo/gt-y8ts@abc123", PreserveOptions{
		IssueID:       "gt-y8ts", // must be ignored when CommitMessage is set
		CommitMessage: "WIP: checkpoint (auto)",
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
