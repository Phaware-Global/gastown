package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
)

const (
	remoteRigSettings = `{"type":"rig-settings","version":1,"execution":{"backend":"socket","socket":{"address":"worker.example:9899"}}}`
	remoteDoneBranch  = "polecat/foo/gt-94p1@abc" // the branch wipGuardRepo checks out
)

func writeRigSettings(t *testing.T, rigPath, body string) {
	t.Helper()
	path := config.RigSettingsPath(rigPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// writeMarkerHook installs a pre-push hook under dir/.githooks that creates
// marker when it runs.
func writeMarkerHook(t *testing.T, dir, marker string) {
	t.Helper()
	hooks := filepath.Join(dir, ".githooks")
	if err := os.MkdirAll(hooks, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hooks, "pre-push")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("chmod", "+x", path).CombinedOutput(); err != nil {
		t.Fatalf("chmod: %v\n%s", err, out)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// remoteDoneRepo is wipGuardRepo — a host worktree on the polecat branch at
// its start point, with a bare origin — configured the way a rig repo is, with
// hooks resolved from the worktree, and holding a host pre-push hook that
// leaves hostMarker behind when it runs.
func remoteDoneRepo(t *testing.T) (host, hostMarker string) {
	t.Helper()
	host = wipGuardRepo(t)
	hostMarker = filepath.Join(t.TempDir(), "host-hook-ran")
	writeMarkerHook(t, host, hostMarker)
	gitOut(t, host, "config", "core.hooksPath", ".githooks")
	return host, hostMarker
}

// pushWorkerCommits plays the worker: it clones the host repo, commits a file
// and a pre-push hook of its own on the branch, and pushes the result to the
// host's work ref. It returns the pushed commit.
func pushWorkerCommits(t *testing.T, host, workerMarker string) string {
	t.Helper()
	worker := filepath.Join(t.TempDir(), "worker")
	gitOut(t, filepath.Dir(worker), "clone", "-q", host, worker)
	gitOut(t, worker, "config", "user.email", "worker@test.com")
	gitOut(t, worker, "config", "user.name", "Worker")
	gitOut(t, worker, "checkout", "-q", "-B", remoteDoneBranch, "origin/"+remoteDoneBranch)
	commitFile(t, worker, "work.txt", "feat: the work")
	writeMarkerHook(t, worker, workerMarker)
	gitOut(t, worker, "add", ".githooks/pre-push")
	gitOut(t, worker, "commit", "-q", "-m", "chore: a hook from the worker")
	gitOut(t, worker, "push", "-q", "origin", "HEAD:"+constants.RemoteWorkRef(remoteDoneBranch))
	return gitOut(t, worker, "rev-parse", "HEAD")
}

// pushMoreWork plays the worker pushing again: one more commit on top of
// whatever the work ref holds. It returns the new commit.
func pushMoreWork(t *testing.T, host, file string) string {
	t.Helper()
	worker := filepath.Join(t.TempDir(), "worker-again")
	gitOut(t, filepath.Dir(worker), "clone", "-q", host, worker)
	gitOut(t, worker, "config", "user.email", "worker@test.com")
	gitOut(t, worker, "config", "user.name", "Worker")
	ref := constants.RemoteWorkRef(remoteDoneBranch)
	gitOut(t, worker, "fetch", "-q", "origin", ref)
	gitOut(t, worker, "checkout", "-q", "-B", "more", "FETCH_HEAD")
	commitFile(t, worker, file, "feat: more work")
	gitOut(t, worker, "push", "-q", "origin", "HEAD:"+ref)
	return gitOut(t, worker, "rev-parse", "HEAD")
}

// advanceOriginMain adds n commits to origin's main from a separate clone and
// fetches them into host, as gt done's own fetch would.
func advanceOriginMain(t *testing.T, host string, n int) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	gitOut(t, filepath.Dir(other), "clone", "-q", gitOut(t, host, "remote", "get-url", "origin"), other)
	gitOut(t, other, "config", "user.email", "other@test.com")
	gitOut(t, other, "config", "user.name", "Other")
	gitOut(t, other, "checkout", "-q", "main")
	for i := 0; i < n; i++ {
		commitFile(t, other, "upstream-"+string(rune('a'+i))+".txt", "feat: upstream change")
	}
	gitOut(t, other, "push", "-q", "origin", "main")
	gitOut(t, host, "fetch", "-q", "origin")
}

func remoteWork(t *testing.T, isPolecat bool) doneWork {
	t.Helper()
	rig := t.TempDir()
	writeRigSettings(t, rig, remoteRigSettings)
	return resolveDoneWork(rig, remoteDoneBranch, isPolecat)
}

// Remote mode is decided from the rig's settings and the session's role, and
// a local polecat keeps exactly the values gt done has always used.
func TestResolveDoneWork(t *testing.T) {
	const branch = remoteDoneBranch
	local := doneWork{rev: "HEAD", srcRef: "refs/heads/" + branch}

	t.Run("a local rig completes from HEAD and the branch", func(t *testing.T) {
		rig := t.TempDir()
		writeRigSettings(t, rig, `{"type":"rig-settings","version":1}`)
		w := resolveDoneWork(rig, branch, true)
		if w != local {
			t.Fatalf("got %+v, want %+v", w, local)
		}
		if got := w.refspec(branch, branch); got != branch+":"+branch {
			t.Errorf("refspec = %q, want the short branch:branch form", got)
		}
		if got := w.refspec(branch, "main"); got != branch+":main" {
			t.Errorf("direct refspec = %q", got)
		}
		if got := w.localRev(branch); got != branch {
			t.Errorf("localRev = %q, want %q", got, branch)
		}
	})
	t.Run("a rig with no settings file is local", func(t *testing.T) {
		if w := resolveDoneWork(t.TempDir(), branch, true); w != local {
			t.Fatalf("got %+v, want %+v", w, local)
		}
	})
	t.Run("a polecat in a remote rig completes from its work ref", func(t *testing.T) {
		w := remoteWork(t, true)
		ref := constants.RemoteWorkRef(branch)
		if !w.remote || w.rev != ref || w.srcRef != ref {
			t.Fatalf("got %+v, want remote mode on %s", w, ref)
		}
		if got := w.refspec(branch, branch); got != ref+":refs/heads/"+branch {
			t.Errorf("refspec = %q", got)
		}
		if got := w.refspec(branch, "main"); got != ref+":refs/heads/main" {
			t.Errorf("direct refspec = %q", got)
		}
		if got := w.localRev(branch); got != ref {
			t.Errorf("localRev = %q, want %q", got, ref)
		}
	})
	t.Run("a non-polecat role in a remote rig stays local", func(t *testing.T) {
		if w := remoteWork(t, false); w != local {
			t.Fatalf("got %+v, want %+v", w, local)
		}
	})
}

// The whole point of remote mode: the worker's commits are found through the
// work ref and published to origin as the polecat branch, while the host
// worktree never receives them and no hook runs — neither the one the worker
// committed nor the host's own.
func TestRemoteDonePublishesTheWorkRefWithoutCheckingItOut(t *testing.T) {
	host, hostMarker := remoteDoneRepo(t)
	workerMarker := filepath.Join(t.TempDir(), "worker-hook-ran")
	startPoint := gitOut(t, host, "rev-parse", "HEAD")
	workCommit := pushWorkerCommits(t, host, workerMarker)

	g := git.NewGit(host)
	w := remoteWork(t, true)

	if err := w.requireRemoteWork(g, remoteDoneBranch); err != nil {
		t.Fatalf("requireRemoteWork: %v", err)
	}
	if got, err := g.Rev(w.rev); err != nil || got != workCommit {
		t.Fatalf("work commit = %q, %v; want %s", got, err, workCommit)
	}
	if ahead, err := g.CommitsAhead("origin/main", w.rev); err != nil || ahead != 2 {
		t.Fatalf("commits ahead via the work ref = %d, %v; want 2", ahead, err)
	}
	// The worktree's own view is still the start point: reading HEAD here is
	// what would have completed the bead with no work.
	if ahead, err := g.CommitsAhead("origin/main", "HEAD"); err != nil || ahead != 0 {
		t.Fatalf("commits ahead via HEAD = %d, %v; want 0", ahead, err)
	}
	if reason := refuseUnverifiedPushRef(g, w.srcRef, []string{"main"}); reason != "" {
		t.Fatalf("unverified-commit guard on the work ref: %s", reason)
	}
	if reason := refuseWIPCommitPushRef(g, w.srcRef, []string{"main"}); reason != "" {
		t.Fatalf("WIP-commit guard on the work ref: %s", reason)
	}

	if err := w.push(g, w.refspec(remoteDoneBranch, remoteDoneBranch)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	origin := gitOut(t, host, "remote", "get-url", "origin")
	if got := gitOut(t, origin, "rev-parse", "refs/heads/"+remoteDoneBranch); got != workCommit {
		t.Errorf("origin branch = %s, want the work commit %s", got, workCommit)
	}
	if err := g.VerifyPushedCommit("origin", remoteDoneBranch, workCommit); err != nil {
		t.Errorf("VerifyPushedCommit: %v", err)
	}
	if got := gitOut(t, host, "rev-parse", "HEAD"); got != startPoint {
		t.Errorf("host HEAD moved to %s, want the start point %s", got, startPoint)
	}
	if got := gitOut(t, host, "rev-parse", "refs/heads/"+remoteDoneBranch); got != startPoint {
		t.Errorf("host branch moved to %s, want the start point %s", got, startPoint)
	}
	if exists(filepath.Join(host, "work.txt")) {
		t.Error("the worker's file was checked out into the host worktree")
	}
	if exists(workerMarker) {
		t.Error("the hook the worker committed ran on the host")
	}
	if exists(hostMarker) {
		t.Error("a host hook ran while publishing a remote polecat's work")
	}
}

// The WIP-commit guard reads the ref it is given, so a checkpoint auto-save
// commit in a remote polecat's work is refused like one on a local branch.
func TestRemoteDoneGuardsReadTheWorkRef(t *testing.T) {
	host, _ := remoteDoneRepo(t)
	worker := filepath.Join(t.TempDir(), "worker")
	gitOut(t, filepath.Dir(worker), "clone", "-q", host, worker)
	gitOut(t, worker, "config", "user.email", "worker@test.com")
	gitOut(t, worker, "config", "user.name", "Worker")
	gitOut(t, worker, "checkout", "-q", "-B", remoteDoneBranch, "origin/"+remoteDoneBranch)
	commitFile(t, worker, "wip.txt", checkpoint.WIPCommitPrefix)
	gitOut(t, worker, "push", "-q", "origin", "HEAD:"+constants.RemoteWorkRef(remoteDoneBranch))

	g := git.NewGit(host)
	w := remoteWork(t, true)
	if reason := refuseWIPCommitPushRef(g, w.srcRef, []string{"main"}); reason == "" {
		t.Fatal("a checkpoint auto-save commit in the work ref was not refused")
	}
	// The local branch carries no such commit, so the branch-based guard a
	// local polecat uses would have missed it.
	if reason := refuseWIPCommitPush(g, remoteDoneBranch, []string{"main"}); reason != "" {
		t.Fatalf("branch-based guard unexpectedly refused: %s", reason)
	}
}

// A local polecat's publish is untouched: the short refspec, and hooks run.
func TestLocalDonePushStillRunsHooks(t *testing.T) {
	host, hostMarker := remoteDoneRepo(t)
	commitFile(t, host, "local.txt", "feat: local work")
	g := git.NewGit(host)
	w := resolveDoneWork(t.TempDir(), remoteDoneBranch, true)
	if w.remote {
		t.Fatal("a rig with no settings must be local")
	}
	if err := w.requireRemoteWork(g, remoteDoneBranch); err != nil {
		t.Fatalf("requireRemoteWork must not apply to a local polecat: %v", err)
	}
	if err := w.push(g, w.refspec(remoteDoneBranch, remoteDoneBranch)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if !exists(hostMarker) {
		t.Error("the pre-push hook did not run for a local polecat's push")
	}
	if err := w.releaseRemoteWork(g); err != nil {
		t.Fatalf("releaseRemoteWork must be a no-op for a local polecat: %v", err)
	}
}

func TestRequireRemoteWork(t *testing.T) {
	host, _ := remoteDoneRepo(t)
	g := git.NewGit(host)
	w := remoteWork(t, true)

	err := w.requireRemoteWork(g, remoteDoneBranch)
	if err == nil {
		t.Fatal("expected a refusal when nothing has been pushed to the work ref")
	}
	for _, want := range []string{remoteDoneBranch, constants.RemoteWorkRef(remoteDoneBranch), "DEFERRED"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}

	pushWorkerCommits(t, host, filepath.Join(t.TempDir(), "unused"))
	if err := w.requireRemoteWork(g, remoteDoneBranch); err != nil {
		t.Fatalf("after the worker pushed: %v", err)
	}
}

// The work ref is released only by an explicit call, which gt done makes on
// its success path; until then it stays, and releasing twice is harmless.
func TestReleaseRemoteWork(t *testing.T) {
	host, _ := remoteDoneRepo(t)
	pushWorkerCommits(t, host, filepath.Join(t.TempDir(), "unused"))
	g := git.NewGit(host)
	w := remoteWork(t, true)
	ref := constants.RemoteWorkRef(remoteDoneBranch)

	if err := w.push(g, w.refspec(remoteDoneBranch, remoteDoneBranch)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if ok, err := g.RefExists(ref); err != nil || !ok {
		t.Fatalf("the work ref must survive the publish: exists=%v err=%v", ok, err)
	}
	if err := w.releaseRemoteWork(g); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, err := g.RefExists(ref); err != nil || ok {
		t.Fatalf("the work ref must be gone after release: exists=%v err=%v", ok, err)
	}
	if err := w.releaseRemoteWork(g); err != nil {
		t.Fatalf("a second release must be a no-op: %v", err)
	}
}

// The work ref is the only host copy of commits origin does not have, so it
// is released only when origin holds everything it points at. A worker that
// pushes again while gt done is finishing must not lose those commits.
func TestReleaseRemoteWorkKeepsUnpublishedCommits(t *testing.T) {
	ref := constants.RemoteWorkRef(remoteDoneBranch)

	t.Run("never published", func(t *testing.T) {
		host, _ := remoteDoneRepo(t)
		pushWorkerCommits(t, host, filepath.Join(t.TempDir(), "unused"))
		g := git.NewGit(host)
		if err := remoteWork(t, true).releaseRemoteWork(g); err == nil {
			t.Fatal("releasing a work ref origin does not hold must be refused")
		}
		if ok, err := g.RefExists(ref); err != nil || !ok {
			t.Fatalf("the work ref must be kept: exists=%v err=%v", ok, err)
		}
	})

	t.Run("worker pushed again after the publish", func(t *testing.T) {
		host, _ := remoteDoneRepo(t)
		pushWorkerCommits(t, host, filepath.Join(t.TempDir(), "unused"))
		g := git.NewGit(host)
		w := remoteWork(t, true)
		if err := w.push(g, w.refspec(remoteDoneBranch, remoteDoneBranch)); err != nil {
			t.Fatalf("publish: %v", err)
		}
		later := pushMoreWork(t, host, "later.txt")
		if err := w.releaseRemoteWork(g); err == nil {
			t.Fatal("a work ref that moved past the published commit must not be released")
		}
		if got := gitOut(t, host, "rev-parse", ref); got != later {
			t.Fatalf("work ref = %s, want the later commit %s kept", got, later)
		}
	})
}

// A push checkpoint from an earlier run lets a retry skip the push. For a
// remote polecat it holds only while origin's branch is at the work ref's
// commit; after another worker push the retry must publish again.
func TestRemotePushCheckpointHoldsOnlyWhileOriginHasTheWork(t *testing.T) {
	host, _ := remoteDoneRepo(t)
	pushWorkerCommits(t, host, filepath.Join(t.TempDir(), "unused"))
	g := git.NewGit(host)
	w := remoteWork(t, true)

	if w.pushCheckpointHolds(g, remoteDoneBranch) {
		t.Fatal("nothing is on origin yet: the checkpoint must not hold")
	}
	if err := w.push(g, w.refspec(remoteDoneBranch, remoteDoneBranch)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !w.pushCheckpointHolds(g, remoteDoneBranch) {
		t.Fatal("origin is at the work commit: the checkpoint must hold")
	}
	pushMoreWork(t, host, "later.txt")
	if w.pushCheckpointHolds(g, remoteDoneBranch) {
		t.Fatal("the worker pushed again: the checkpoint must not hold")
	}
	if local := resolveDoneWork(t.TempDir(), remoteDoneBranch, true); !local.pushCheckpointHolds(g, remoteDoneBranch) {
		t.Fatal("a local polecat's checkpoint is unchanged: it always holds")
	}
}

// gt done's staleness thresholds must measure the work ref for a remote
// polecat. The host worktree stays at the start point, so its HEAD only falls
// further behind and a worker-side rebase would never clear the block.
func TestRemoteContaminationMeasuresTheWorkRef(t *testing.T) {
	host, _ := remoteDoneRepo(t)
	pushWorkerCommits(t, host, filepath.Join(t.TempDir(), "unused"))
	advanceOriginMain(t, host, 3)
	g := git.NewGit(host)
	w := remoteWork(t, true)

	got, err := w.contamination(g, "origin/main")
	if err != nil {
		t.Fatalf("contamination: %v", err)
	}
	if got.Behind != 3 || got.Ahead != 2 {
		t.Fatalf("before the worker rebases: behind=%d ahead=%d, want 3 and 2", got.Behind, got.Ahead)
	}

	// The worker rebases onto the new base and pushes again.
	worker := filepath.Join(t.TempDir(), "rebase")
	gitOut(t, filepath.Dir(worker), "clone", "-q", host, worker)
	gitOut(t, worker, "config", "user.email", "worker@test.com")
	gitOut(t, worker, "config", "user.name", "Worker")
	ref := constants.RemoteWorkRef(remoteDoneBranch)
	gitOut(t, worker, "fetch", "-q", "origin", ref)
	gitOut(t, worker, "checkout", "-q", "-B", "rebased", "FETCH_HEAD")
	gitOut(t, worker, "fetch", "-q", gitOut(t, host, "remote", "get-url", "origin"), "main")
	gitOut(t, worker, "rebase", "-q", "FETCH_HEAD")
	gitOut(t, worker, "push", "-q", "--force", "origin", "HEAD:"+ref)

	got, err = w.contamination(g, "origin/main")
	if err != nil {
		t.Fatalf("contamination after rebase: %v", err)
	}
	if got.Behind != 0 || got.Ahead != 2 {
		t.Fatalf("after the worker rebases: behind=%d ahead=%d, want 0 and 2", got.Behind, got.Ahead)
	}
	// The host worktree's own HEAD is what the check used to read.
	if stale, err := g.CheckBranchContamination("origin/main"); err != nil || stale.Behind != 3 {
		t.Fatalf("host HEAD: behind=%d err=%v, want 3 (the start point never moves)", stale.Behind, err)
	}
}

// The shared-commit guard asks which patches the MR would carry. For a remote
// polecat that is the work ref, not the host worktree's HEAD, which carries
// none of them.
func TestRemoteMRGuardReadsTheWorkRef(t *testing.T) {
	host, _ := remoteDoneRepo(t)
	pushWorkerCommits(t, host, filepath.Join(t.TempDir(), "unused"))
	g := git.NewGit(host)

	remote, err := remoteWork(t, true).mrGuardEnv(g).RangePatchIDs("origin/main", "HEAD")
	if err != nil {
		t.Fatalf("remote RangePatchIDs: %v", err)
	}
	if len(remote) != 2 {
		t.Fatalf("remote polecat: %d patches, want the worker's 2", len(remote))
	}
	local, err := resolveDoneWork(t.TempDir(), remoteDoneBranch, true).mrGuardEnv(g).RangePatchIDs("origin/main", "HEAD")
	if err != nil {
		t.Fatalf("local RangePatchIDs: %v", err)
	}
	if len(local) != 0 {
		t.Fatalf("local polecat: %d patches, want HEAD's 0", len(local))
	}
}

// A direct push lands on a protected branch, and the rig's pre-push hook is
// what refuses one that would land an integration branch there. It must run
// for a remote polecat too. The hook that runs is the host's: the worker's
// committed hook is never in the host worktree.
func TestRemoteDirectPushRunsTheHostPrePushHook(t *testing.T) {
	host, hostMarker := remoteDoneRepo(t)
	workerMarker := filepath.Join(t.TempDir(), "worker-hook-ran")
	workCommit := pushWorkerCommits(t, host, workerMarker)
	g := git.NewGit(host)
	w := remoteWork(t, true)

	if err := w.pushDirect(g, w.refspec(remoteDoneBranch, "main")); err != nil {
		t.Fatalf("direct push: %v", err)
	}
	origin := gitOut(t, host, "remote", "get-url", "origin")
	if got := gitOut(t, origin, "rev-parse", "refs/heads/main"); got != workCommit {
		t.Fatalf("origin main = %s, want the work commit %s", got, workCommit)
	}
	if !exists(hostMarker) {
		t.Error("the host's pre-push hook did not run for a remote polecat's direct push")
	}
	if exists(workerMarker) {
		t.Error("the hook the worker committed ran on the host")
	}
}
