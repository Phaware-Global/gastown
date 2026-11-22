package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

const remoteWorkBranch = "polecat/nux/gt-1@abc"

func rwGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remoteWorkRepo is a host worktree on a polecat branch at its start point,
// cloned from a bare origin. It returns the host path and origin path.
func remoteWorkRepo(t *testing.T) (host, origin string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	rwGit(t, root, "init", "-q", "--bare", "-b", "main", origin)
	rwGit(t, root, "init", "-q", "-b", "main", seed)
	rwGit(t, seed, "config", "user.email", "test@example.com")
	rwGit(t, seed, "config", "user.name", "Test")
	rwGit(t, seed, "commit", "-q", "--allow-empty", "-m", "base")
	rwGit(t, seed, "push", "-q", origin, "main")
	host = filepath.Join(root, "host")
	rwGit(t, root, "clone", "-q", origin, host)
	rwGit(t, host, "config", "user.email", "test@example.com")
	rwGit(t, host, "config", "user.name", "Test")
	rwGit(t, host, "checkout", "-q", "-b", remoteWorkBranch)
	return host, origin
}

// pushRemoteWork stands in for the worker: it leaves n commits in the host's
// work ref for the branch while the host worktree stays at its start point.
func pushRemoteWork(t *testing.T, host string, n int) {
	t.Helper()
	worker := filepath.Join(t.TempDir(), "worker")
	rwGit(t, host, "clone", "-q", host, worker)
	rwGit(t, worker, "config", "user.email", "worker@example.com")
	rwGit(t, worker, "config", "user.name", "Worker")
	for i := 0; i < n; i++ {
		rwGit(t, worker, "commit", "-q", "--allow-empty", "-m", "remote work")
	}
	rwGit(t, worker, "push", "-q", host, "HEAD:"+constants.RemoteWorkRef(remoteWorkBranch))
}

func TestUnpublishedRemoteWork_NoRefMeansNoWork(t *testing.T) {
	host, _ := remoteWorkRepo(t)
	g := NewGit(host)

	work, err := g.UnpublishedRemoteWork(remoteWorkBranch, "origin")
	if err != nil {
		t.Fatalf("UnpublishedRemoteWork: %v", err)
	}
	if work.Unpublished != 0 || work.Tip != "" {
		t.Fatalf("work = %+v, want none", work)
	}
	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if status.UnpushedCommits != 0 {
		t.Fatalf("UnpushedCommits = %d, want 0 for a branch with no work ref", status.UnpushedCommits)
	}
}

// The host worktree of a remote polecat sits at its start point, so on its own
// it reads as a polecat with nothing unpushed. The shared readers must count
// what the worker pushed to the work ref.
func TestRemoteWorkIsCountedAsUnpushedByTheSharedReaders(t *testing.T) {
	host, _ := remoteWorkRepo(t)
	pushRemoteWork(t, host, 2)
	g := NewGit(host)

	work, err := g.UnpublishedRemoteWork(remoteWorkBranch, "origin")
	if err != nil {
		t.Fatalf("UnpublishedRemoteWork: %v", err)
	}
	if work.Unpublished != 2 || work.Tip == "" || work.Ref != constants.RemoteWorkRef(remoteWorkBranch) {
		t.Fatalf("work = %+v, want 2 unpublished commits at %s", work, constants.RemoteWorkRef(remoteWorkBranch))
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		t.Fatalf("CheckUncommittedWork: %v", err)
	}
	if status.UnpushedCommits != 2 {
		t.Errorf("CheckUncommittedWork.UnpushedCommits = %d, want 2", status.UnpushedCommits)
	}

	pushed, unpushed, err := g.BranchPushedToRemote(remoteWorkBranch, "origin")
	if err != nil {
		t.Fatalf("BranchPushedToRemote: %v", err)
	}
	if pushed || unpushed != 2 {
		t.Errorf("BranchPushedToRemote = (%v, %d), want (false, 2)", pushed, unpushed)
	}

	target, err := g.BranchTargetStatus(remoteWorkBranch, "origin", nil)
	if err != nil {
		t.Fatalf("BranchTargetStatus: %v", err)
	}
	if target.Preserved || target.UnpreservedPatchCount < 2 {
		t.Errorf("BranchTargetStatus = %+v, want unpreserved with the 2 remote commits counted", target)
	}

	branches, err := g.RemoteWorkBranches()
	if err != nil || len(branches) != 1 || branches[0] != remoteWorkBranch {
		t.Errorf("RemoteWorkBranches = %v, %v; want [%s]", branches, err, remoteWorkBranch)
	}
}

// Publishing sends the work ref to origin as the branch without checking it
// out and without running a hook, and afterwards nothing is unpublished.
func TestPublishRemoteWork_PublishesWithoutHooksOrCheckout(t *testing.T) {
	host, origin := remoteWorkRepo(t)
	pushRemoteWork(t, host, 1)
	g := NewGit(host)

	marker := filepath.Join(t.TempDir(), "hook-ran")
	hooks := filepath.Join(host, ".githooks")
	if err := os.MkdirAll(hooks, 0755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hooks, "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("chmod", "+x", hook).CombinedOutput(); err != nil {
		t.Fatalf("chmod: %v\n%s", err, out)
	}
	rwGit(t, host, "config", "core.hooksPath", ".githooks")
	headBefore := rwGit(t, host, "rev-parse", "HEAD")

	if err := g.PublishRemoteWork(remoteWorkBranch, "origin"); err != nil {
		t.Fatalf("PublishRemoteWork: %v", err)
	}

	work, err := g.UnpublishedRemoteWork(remoteWorkBranch, "origin")
	if err != nil {
		t.Fatalf("UnpublishedRemoteWork: %v", err)
	}
	if work.Unpublished != 0 {
		t.Errorf("Unpublished = %d after publishing, want 0", work.Unpublished)
	}
	if got := rwGit(t, origin, "rev-parse", "refs/heads/"+remoteWorkBranch); got != work.Tip {
		t.Errorf("origin branch = %s, want the work ref tip %s", got, work.Tip)
	}
	if got := rwGit(t, host, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("host HEAD moved from %s to %s", headBefore, got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a pre-push hook ran while publishing remote work")
	}
}
