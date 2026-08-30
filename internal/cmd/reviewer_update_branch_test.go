package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// setupBehindPR builds a "GitHub": a remote whose refs/pull/1/head is a PR
// branch, with the base branch one commit ahead of it — the behind state this
// feature reacts to. Returns the remote and a clone standing in for the
// reviewer worktree.
func setupBehindPR(t *testing.T) (remote, clone, base string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX-only")
	}
	remote = t.TempDir()
	gitIn(t, remote, "init", "-q", ".")
	gitIn(t, remote, "config", "user.email", "test@test.com")
	gitIn(t, remote, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(remote, "README.md"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, remote, "add", ".")
	gitIn(t, remote, "commit", "-qm", "initial")
	base = gitIn(t, remote, "rev-parse", "--abbrev-ref", "HEAD")

	gitIn(t, remote, "checkout", "-qb", "pr-branch")
	if err := os.WriteFile(filepath.Join(remote, "pr.txt"), []byte("pr\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, remote, "add", ".")
	gitIn(t, remote, "commit", "-qm", "pr work")
	gitIn(t, remote, "update-ref", "refs/pull/1/head", gitIn(t, remote, "rev-parse", "HEAD"))

	// The base moves on: the PR head no longer contains it.
	gitIn(t, remote, "checkout", "-q", base)
	if err := os.WriteFile(filepath.Join(remote, "merged.txt"), []byte("merged\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, remote, "add", ".")
	gitIn(t, remote, "commit", "-qm", "another PR merged to base")

	clone = t.TempDir()
	gitIn(t, clone, "clone", "-q", remote, ".")
	return remote, clone, base
}

// ghUpdateBranchStub installs a fake `gh` that answers the base-branch query
// and, when landUpdate is true, performs on the remote exactly what GitHub's
// "Update branch" does: merge the base into the head branch and republish the
// pull ref. With landUpdate false it exits 0 without changing anything — the
// shape of an update that is accepted but never lands.
func ghUpdateBranchStub(t *testing.T, remote, base string, landUpdate bool) {
	t.Helper()
	dir := t.TempDir()
	land := ""
	if landUpdate {
		land = "git -C " + remote + " checkout -q pr-branch\n" +
			"  git -C " + remote + " merge --no-edit -q " + base + "\n" +
			"  git -C " + remote + " update-ref refs/pull/1/head \"$(git -C " + remote + " rev-parse HEAD)\"\n" +
			"  git -C " + remote + " checkout -q " + base + "\n"
	}
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *update-branch*)\n  " + land + "  exit 0 ;;\n" +
		"  *baseRefName*) echo " + base + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { //nolint:gosec // test stub must be executable
		t.Fatalf("write gh stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shortenUpdateWait(t *testing.T) {
	t.Helper()
	origTimeout, origPoll := prUpdateTimeout, prUpdatePollWait
	prUpdateTimeout, prUpdatePollWait = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { prUpdateTimeout, prUpdatePollWait = origTimeout, origPoll })
}

// The happy path: a behind PR is updated, the update is confirmed against the
// ref the checkout will read, and the caller is told to drop its pinned SHA.
func TestUpdateReviewerPRIfBehind_ReportsAVerifiedUpdate(t *testing.T) {
	remote, clone, base := setupBehindPR(t)
	ghUpdateBranchStub(t, remote, base, true)
	shortenUpdateWait(t)

	if !updateReviewerPRIfBehind(git.NewGit(clone), 1, "") {
		t.Error("a behind PR whose update landed must report true")
	}
}

// An update that is accepted but never lands must NOT report success: the
// return value discards the operator-pinned --sha, so a false positive
// silently retargets the round at an unpinned, possibly still-behind head.
func TestUpdateReviewerPRIfBehind_KeepsThePinWhenTheUpdateNeverLands(t *testing.T) {
	remote, clone, base := setupBehindPR(t)
	ghUpdateBranchStub(t, remote, base, false)
	shortenUpdateWait(t)

	if updateReviewerPRIfBehind(git.NewGit(clone), 1, "") {
		t.Error("an unconfirmed update must not report success — the pinned SHA would be dropped")
	}
}

// A PR already containing its base is left alone: no update, and the pinned
// SHA survives.
func TestUpdateReviewerPRIfBehind_NoOpWhenAlreadyUpToDate(t *testing.T) {
	remote, clone, base := setupBehindPR(t)
	// Bring the PR head up to date before asking.
	gitIn(t, remote, "checkout", "-q", "pr-branch")
	gitIn(t, remote, "merge", "--no-edit", "-q", base)
	gitIn(t, remote, "update-ref", "refs/pull/1/head", gitIn(t, remote, "rev-parse", "HEAD"))
	gitIn(t, remote, "checkout", "-q", base)
	ghUpdateBranchStub(t, remote, base, true)
	shortenUpdateWait(t)

	if updateReviewerPRIfBehind(git.NewGit(clone), 1, "") {
		t.Error("an up-to-date PR must not be updated")
	}
}

// The decision is made about the commit the round would review, not the head:
// a pinned commit that is behind still triggers the update even when the head
// has since moved past the base.
func TestUpdateReviewerPRIfBehind_MeasuresThePinnedCommit(t *testing.T) {
	remote, clone, base := setupBehindPR(t)
	pinned := gitIn(t, remote, "rev-parse", "refs/pull/1/head")
	// The author pushes a head that DOES contain the base…
	gitIn(t, remote, "checkout", "-q", "pr-branch")
	gitIn(t, remote, "merge", "--no-edit", "-q", base)
	gitIn(t, remote, "update-ref", "refs/pull/1/head", gitIn(t, remote, "rev-parse", "HEAD"))
	gitIn(t, remote, "checkout", "-q", base)
	ghUpdateBranchStub(t, remote, base, true)
	shortenUpdateWait(t)

	g := git.NewGit(clone)
	// …so measuring the head would skip the update…
	if updateReviewerPRIfBehind(g, 1, "") {
		t.Error("the current head contains the base; no update should be needed")
	}
	// …but the pinned commit this round reviews is still behind it.
	if !updateReviewerPRIfBehind(g, 1, pinned) {
		t.Error("a pinned commit behind its base must still trigger the update")
	}
}
