package polecat

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
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
