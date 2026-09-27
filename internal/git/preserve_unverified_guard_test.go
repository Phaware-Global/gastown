package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the MAYOR DESIGN RULING on gt-xitk (2026-09-27), which
// gt-35un implements: the unverified-commit guard's exempt set must be
// commits reachable from the rig's PROTECTED branches (taken from rig
// config), never from a merge-base against the repo's git-level default
// branch and never from anything bead/branch/formula-var derived.
//
// Four cases, each a hole a prior formulation of this guard fell into:
//  1. A commit already merged to the rig's real base (e.g. "develop") must
//     not be flagged, even when the repo's git-level default branch is a
//     different name (e.g. "main") — this is gt-35un's original bug.
//  2. A commit that exists ONLY on the polecat's own pushed branch must
//     still be flagged — protected branches must not accidentally include
//     the branch being checked (round 2's "too wide" hole).
//  3. A configured protected branch that doesn't resolve on the remote
//     means refuse (fail closed), not silently widen the search.
//  4. No protected branches at all (rig config unavailable) also fails
//     closed rather than guessing a baseline.

// commitUnverified makes an unverified (hooks-bypassed) commit carrying the
// durable trailer, the same way AutoPreserveUncommittedWork's fallback path
// does, without needing a real failing hook script.
func commitUnverified(t *testing.T, dir, path, contents, subject string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, path), []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	runGitTestCmd(t, dir, "add", path)
	runGitTestCmd(t, dir, "commit", "--no-verify", "-m", subject+"\n\n"+unverifiedCommitTrailer)
	g := NewGit(dir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("Rev HEAD: %v", err)
	}
	return head
}

func TestHasUnverifiedCommit_AlreadyMergedToProtectedBranchNotFlagged(t *testing.T) {
	localDir, remoteDir, mainBranch := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	// Simulate a develop-based rig: develop diverges from the repo's
	// git-level default branch (mainBranch here stands in for "main").
	runGitTestCmd(t, localDir, "checkout", "-b", "develop")
	badSHA := commitUnverified(t, localDir, "already-merged.txt", "on develop", "merged via review")
	runGitTestCmd(t, localDir, "push", "origin", "develop")

	// A brand new feature branch forked from develop — its own history is
	// clean, but it carries badSHA in its ancestry because it forked after
	// that commit landed on develop.
	runGitTestCmd(t, localDir, "checkout", "-b", "polecat/foo/gt-35un@abc123", "develop")
	runGitTestCmd(t, localDir, "fetch", "origin")

	got, err := HasUnverifiedCommit(g, "origin", []string{"develop"})
	if err != nil {
		t.Fatalf("HasUnverifiedCommit: %v", err)
	}
	if got != "" {
		t.Fatalf("HasUnverifiedCommit = %q, want \"\" — %s is already on the protected branch develop and should be exempt", got, badSHA)
	}

	_ = mainBranch
	_ = remoteDir
}

func TestHasUnverifiedCommit_OwnUnpushedCommitStillFlagged(t *testing.T) {
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	runGitTestCmd(t, localDir, "checkout", "-b", "develop")
	runGitTestCmd(t, localDir, "push", "-u", "origin", "develop")

	runGitTestCmd(t, localDir, "checkout", "-b", "polecat/foo/gt-35un@abc123")
	badSHA := commitUnverified(t, localDir, "own-work.txt", "own work", "bypassed hooks")

	got, err := HasUnverifiedCommit(g, "origin", []string{"develop"})
	if err != nil {
		t.Fatalf("HasUnverifiedCommit: %v", err)
	}
	if got != badSHA {
		t.Fatalf("HasUnverifiedCommit = %q, want %q — a commit unique to the polecat's own branch must still be refused", got, badSHA)
	}
}

func TestHasUnverifiedCommit_OwnPushedCommitStillFlagged(t *testing.T) {
	// Round 2's hole: exempting anything reachable from ANY remote-tracking
	// ref (including the branch's own, once pushed) silently disarmed the
	// guard. Protected branches must be an explicit rig-config list, never
	// "whatever is on origin for this branch."
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	runGitTestCmd(t, localDir, "checkout", "-b", "develop")
	runGitTestCmd(t, localDir, "push", "-u", "origin", "develop")

	branch := "polecat/foo/gt-35un@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	badSHA := commitUnverified(t, localDir, "own-work.txt", "own work", "bypassed hooks")
	runGitTestCmd(t, localDir, "push", "-u", "origin", branch)

	got, err := HasUnverifiedCommit(g, "origin", []string{"develop"})
	if err != nil {
		t.Fatalf("HasUnverifiedCommit: %v", err)
	}
	if got != badSHA {
		t.Fatalf("HasUnverifiedCommit = %q, want %q — pushing the branch itself must not exempt its own unverified commit", got, badSHA)
	}
}

func TestHasUnverifiedCommit_UnresolvableProtectedRefFailsClosed(t *testing.T) {
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	runGitTestCmd(t, localDir, "checkout", "-b", "polecat/foo/gt-35un@abc123")
	if err := os.WriteFile(filepath.Join(localDir, "f.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitTestCmd(t, localDir, "add", "f.txt")
	runGitTestCmd(t, localDir, "commit", "-m", "clean commit, no trailer")

	_, err := HasUnverifiedCommit(g, "origin", []string{"develop-does-not-exist"})
	if err == nil {
		t.Fatal("expected an error when the configured protected branch does not resolve on origin — a missing ref must fail closed, not fall back to scanning everything or nothing")
	}
	if !strings.Contains(err.Error(), "develop-does-not-exist") {
		t.Fatalf("error = %q, want it to name the missing ref", err)
	}
}

func TestHasUnverifiedCommit_ShadowingLocalBranchNotExempt(t *testing.T) {
	// Security: git resolves refs/heads/<name> before refs/remotes/<name>,
	// so an unqualified "origin/develop" ref can be shadowed by a local
	// branch of that same name in the checked worktree, exempting every
	// unverified commit (PR #253 review). The scan baseline must not be
	// selectable by the thing being checked.
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	runGitTestCmd(t, localDir, "checkout", "-b", "develop")
	runGitTestCmd(t, localDir, "push", "-u", "origin", "develop")

	branch := "polecat/foo/gt-35un@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	badSHA := commitUnverified(t, localDir, "own-work.txt", "own work", "bypassed hooks")

	// Shadow refs/remotes/origin/develop with a local branch of the same
	// unqualified name, at the tip of the unverified branch.
	runGitTestCmd(t, localDir, "branch", "origin/develop", "HEAD")

	got, err := HasUnverifiedCommit(g, "origin", []string{"develop"})
	if err != nil {
		t.Fatalf("HasUnverifiedCommit: %v", err)
	}
	if got != badSHA {
		t.Fatalf("HasUnverifiedCommit = %q, want %q — a local branch named origin/develop must not shadow the tracking ref and exempt the unverified commit", got, badSHA)
	}
}

func TestHasUnverifiedCommit_ShadowingLocalTagNotExempt(t *testing.T) {
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	runGitTestCmd(t, localDir, "checkout", "-b", "develop")
	runGitTestCmd(t, localDir, "push", "-u", "origin", "develop")

	branch := "polecat/foo/gt-35un@abc123"
	runGitTestCmd(t, localDir, "checkout", "-b", branch)
	badSHA := commitUnverified(t, localDir, "own-work.txt", "own work", "bypassed hooks")

	// Same shadow, but via a tag instead of a branch — the adversarial
	// pass reproduced this variant independently.
	runGitTestCmd(t, localDir, "tag", "origin/develop", "HEAD")

	got, err := HasUnverifiedCommit(g, "origin", []string{"develop"})
	if err != nil {
		t.Fatalf("HasUnverifiedCommit: %v", err)
	}
	if got != badSHA {
		t.Fatalf("HasUnverifiedCommit = %q, want %q — a local tag named origin/develop must not shadow the tracking ref and exempt the unverified commit", got, badSHA)
	}
}

func TestHasUnverifiedCommit_NoProtectedBranchesFailsClosed(t *testing.T) {
	localDir, _, _ := initTestRepoWithRemote(t)
	g := NewGit(localDir)

	got, err := HasUnverifiedCommit(g, "origin", nil)
	if err == nil {
		t.Fatalf("expected an error when no protected branches are configured, got badSHA=%q with no error — this must refuse rather than guess a baseline (e.g. by falling back to RemoteDefaultBranch)", got)
	}
}
