package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/git"
)

// wipGuardRepo builds a local repo with a bare origin whose "main" is pushed,
// then checks out a polecat-style feature branch. Returns the worktree dir.
func wipGuardRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	local := filepath.Join(tmp, "local")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run(tmp, "init", "--bare", remote)
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	run(local, "init")
	run(local, "config", "user.email", "test@test.com")
	run(local, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(local, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run(local, "add", ".")
	run(local, "commit", "-m", "initial")
	run(local, "branch", "-M", "main")
	run(local, "remote", "add", "origin", remote)
	run(local, "push", "-u", "origin", "main")
	run(local, "checkout", "-b", "polecat/foo/gt-94p1@abc")
	return local
}

func commitFile(t *testing.T, dir, name, subject string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	for _, args := range [][]string{{"add", name}, {"commit", "-m", subject}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestRefuseWIPCommitPush pins gt-94p1 requirement 3: a push of a branch
// carrying a checkpoint auto-save commit to a real branch is refused.
func TestRefuseWIPCommitPush(t *testing.T) {
	dir := wipGuardRepo(t)
	g := git.NewGit(dir)
	branch := "polecat/foo/gt-94p1@abc"

	commitFile(t, dir, "real.txt", "real work")
	if reason := refuseWIPCommitPush(g, branch, []string{"main"}); reason != "" {
		t.Fatalf("clean branch must not be refused, got: %s", reason)
	}

	commitFile(t, dir, "wip.txt", checkpoint.WIPCommitPrefix)
	reason := refuseWIPCommitPush(g, branch, []string{"main"})
	if reason == "" {
		t.Fatal("a branch carrying a WIP checkpoint commit must be refused")
	}
	if !strings.Contains(reason, "checkpoint auto-save") {
		t.Fatalf("reason should name the checkpoint auto-save commit, got: %s", reason)
	}
	// Agents cannot drive an interactive editor, so the recovery advice must
	// be something they can actually run.
	if strings.Contains(reason, "rebase -i") {
		t.Fatalf("reason must not advise interactive rebase, got: %s", reason)
	}
	if !strings.Contains(reason, "git rebase --onto") {
		t.Fatalf("reason should give a non-interactive recovery, got: %s", reason)
	}
}

// The guard scans the ref being pushed, not whatever HEAD happens to be.
func TestRefuseWIPCommitPush_ScansThePushedBranchNotHEAD(t *testing.T) {
	dir := wipGuardRepo(t)
	g := git.NewGit(dir)
	branch := "polecat/foo/gt-94p1@abc"

	commitFile(t, dir, "wip.txt", checkpoint.WIPCommitPrefix)
	cmd := exec.Command("git", "checkout", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout main: %v\n%s", err, out)
	}

	if reason := refuseWIPCommitPush(g, branch, []string{"main"}); reason == "" {
		t.Fatal("HEAD is clean but the branch being pushed carries a WIP commit — must be refused")
	}
}

func TestRefuseWIPCommitPush_FailsClosedWithoutProtectedBranches(t *testing.T) {
	dir := wipGuardRepo(t)
	g := git.NewGit(dir)
	branch := "polecat/foo/gt-94p1@abc"

	if reason := refuseWIPCommitPush(g, branch, nil); reason == "" {
		t.Fatal("with no protected branches the guard must refuse rather than scan without a baseline")
	}
}
