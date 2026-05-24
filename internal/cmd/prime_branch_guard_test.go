package cmd

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
)

// TestIsDefaultBranch pins which branch names the off-main guard treats as
// "don't work here" for a polecat. main/master must trip the guard; polecat
// branches, detached HEAD, and the empty string must not.
func TestIsDefaultBranch(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"main", true},
		{"master", true},
		{"  main  ", true}, // trimmed
		{"polecat/nux-gt-tk5", false},
		{"polecat/nux/gt-tk5@mpkb5d9l", false},
		{"HEAD", false}, // detached HEAD — a different problem, not ours
		{"", false},
		{"maintenance", false}, // substring trap
		{"feature/main", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := isDefaultBranch(c.in); got != c.want {
				t.Errorf("isDefaultBranch(%q) = %v; want %v", c.in, got, c.want)
			}
		})
	}
}

// gitRun runs a git command in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %s: %v", args, out, err)
	}
}

// newOriginAndClone builds a bare "origin" with a main branch and a working
// clone of it, returning the clone's path. The clone is what a polecat worktree
// looks like: it has origin/main to branch from.
func newOriginAndClone(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "seed")

	gitRun(t, root, "init", "--bare", "-b", "main", origin)
	gitRun(t, root, "init", "-b", "main", work)
	gitRun(t, work, "config", "user.email", "test@example.com")
	gitRun(t, work, "config", "user.name", "Test")
	gitRun(t, work, "commit", "--allow-empty", "-m", "init")
	gitRun(t, work, "remote", "add", "origin", origin)
	gitRun(t, work, "push", "origin", "main")

	clone := filepath.Join(root, "clone")
	gitRun(t, root, "clone", origin, clone)
	gitRun(t, clone, "config", "user.email", "test@example.com")
	gitRun(t, clone, "config", "user.name", "Test")
	return clone
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	b, err := git.NewGit(dir).CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	return b
}

// TestEnsurePolecatWorkBranch_CreatesFromMain is the core gt-tk5 case: a
// worktree sitting on main is moved onto a fresh polecat/<name>-<bead> branch.
func TestEnsurePolecatWorkBranch_CreatesFromMain(t *testing.T) {
	clone := newOriginAndClone(t)
	if got := currentBranch(t, clone); got != "main" {
		t.Fatalf("precondition: expected to start on main, got %q", got)
	}

	res, err := ensurePolecatWorkBranch(clone, "nux", "gt-tk5")
	if err != nil {
		t.Fatalf("ensurePolecatWorkBranch: %v", err)
	}
	if res.Action != polecatBranchCreated {
		t.Errorf("Action = %q; want %q", res.Action, polecatBranchCreated)
	}
	if res.Target != "polecat/nux-gt-tk5" {
		t.Errorf("Target = %q; want polecat/nux-gt-tk5", res.Target)
	}
	if got := currentBranch(t, clone); got != "polecat/nux-gt-tk5" {
		t.Errorf("worktree left on %q; want polecat/nux-gt-tk5", got)
	}
}

// TestEnsurePolecatWorkBranch_Noop: already on the target branch is a no-op.
func TestEnsurePolecatWorkBranch_Noop(t *testing.T) {
	clone := newOriginAndClone(t)
	gitRun(t, clone, "checkout", "-b", "polecat/nux-gt-tk5")

	res, err := ensurePolecatWorkBranch(clone, "nux", "gt-tk5")
	if err != nil {
		t.Fatalf("ensurePolecatWorkBranch: %v", err)
	}
	if res.Action != polecatBranchNoop {
		t.Errorf("Action = %q; want %q", res.Action, polecatBranchNoop)
	}
}

// TestEnsurePolecatWorkBranch_ResumesExisting: the target branch exists locally
// but the worktree drifted back to main (post-merge cleanup, gt-pvx re-run).
// We re-checkout it without -b rather than failing.
func TestEnsurePolecatWorkBranch_ResumesExisting(t *testing.T) {
	clone := newOriginAndClone(t)
	gitRun(t, clone, "branch", "polecat/nux-gt-tk5", "main") // exists, but not checked out

	res, err := ensurePolecatWorkBranch(clone, "nux", "gt-tk5")
	if err != nil {
		t.Fatalf("ensurePolecatWorkBranch: %v", err)
	}
	if res.Action != polecatBranchResumed {
		t.Errorf("Action = %q; want %q", res.Action, polecatBranchResumed)
	}
	if got := currentBranch(t, clone); got != "polecat/nux-gt-tk5" {
		t.Errorf("worktree left on %q; want polecat/nux-gt-tk5", got)
	}
}

// TestEnsurePolecatWorkBranch_RefusesOtherPolecatBranch: never silently swap one
// polecat branch for another — uncommitted work would be lost or contaminated.
func TestEnsurePolecatWorkBranch_RefusesOtherPolecatBranch(t *testing.T) {
	clone := newOriginAndClone(t)
	gitRun(t, clone, "checkout", "-b", "polecat/rictus-gt-mwy.5")

	_, err := ensurePolecatWorkBranch(clone, "nux", "gt-tk5")
	if err == nil {
		t.Fatal("expected refusal switching between polecat branches, got nil")
	}
}

// TestEnsurePolecatWorkBranch_RejectsBadBeadID: the bead-id validation guards
// against branch-name pollution before any git command runs.
func TestEnsurePolecatWorkBranch_RejectsBadBeadID(t *testing.T) {
	clone := newOriginAndClone(t)
	if _, err := ensurePolecatWorkBranch(clone, "nux", "gt-mwy;rm -rf /"); err == nil {
		t.Fatal("expected bead-id validation error, got nil")
	}
}

// TestEnsurePolecatOffMain_NonPolecatNoop: the guard only applies to polecats.
// A witness/refinery on main must pass through untouched.
func TestEnsurePolecatOffMain_NonPolecatNoop(t *testing.T) {
	clone := newOriginAndClone(t)
	ctx := RoleContext{Role: RoleWitness, Polecat: "nux", WorkDir: clone}
	if err := ensurePolecatOffMain(ctx, &beads.Issue{ID: "gt-tk5"}); err != nil {
		t.Fatalf("non-polecat on main should be a no-op, got: %v", err)
	}
	if got := currentBranch(t, clone); got != "main" {
		t.Errorf("guard moved a non-polecat off %q", got)
	}
}

// TestEnsurePolecatOffMain_AlreadyOffMain: a polecat already on its work branch
// is left alone (the common path on every prime).
func TestEnsurePolecatOffMain_AlreadyOffMain(t *testing.T) {
	clone := newOriginAndClone(t)
	gitRun(t, clone, "checkout", "-b", "polecat/nux-gt-tk5")
	ctx := RoleContext{Role: RolePolecat, Polecat: "nux", WorkDir: clone}
	if err := ensurePolecatOffMain(ctx, &beads.Issue{ID: "gt-tk5"}); err != nil {
		t.Fatalf("polecat already off main should be a no-op, got: %v", err)
	}
	if got := currentBranch(t, clone); got != "polecat/nux-gt-tk5" {
		t.Errorf("guard changed branch to %q", got)
	}
}

// TestEnsurePolecatOffMain_RestoresFromMain: the headline gt-tk5 behavior — a
// polecat that primes on main is auto-restored to its work branch.
func TestEnsurePolecatOffMain_RestoresFromMain(t *testing.T) {
	clone := newOriginAndClone(t)
	ctx := RoleContext{Role: RolePolecat, Polecat: "nux", WorkDir: clone}
	if err := ensurePolecatOffMain(ctx, &beads.Issue{ID: "gt-tk5"}); err != nil {
		t.Fatalf("expected auto-restore, got error: %v", err)
	}
	if got := currentBranch(t, clone); got != "polecat/nux-gt-tk5" {
		t.Errorf("worktree left on %q; want polecat/nux-gt-tk5", got)
	}
}

// TestEnsurePolecatOffMain_RefusesWithoutBead: on main with no hooked bead we
// can't name the branch, so the guard must refuse (non-nil error) and leave the
// worktree on main for the operator.
func TestEnsurePolecatOffMain_RefusesWithoutBead(t *testing.T) {
	clone := newOriginAndClone(t)
	ctx := RoleContext{Role: RolePolecat, Polecat: "nux", WorkDir: clone}
	if err := ensurePolecatOffMain(ctx, nil); err == nil {
		t.Fatal("expected refusal on main with no bead, got nil")
	}
	if got := currentBranch(t, clone); got != "main" {
		t.Errorf("guard moved worktree to %q despite refusing", got)
	}
}

// TestEnsurePolecatOffMain_DryRunNoMutation: --dry-run reports but must not
// touch git.
func TestEnsurePolecatOffMain_DryRunNoMutation(t *testing.T) {
	clone := newOriginAndClone(t)
	prev := primeDryRun
	primeDryRun = true
	defer func() { primeDryRun = prev }()

	ctx := RoleContext{Role: RolePolecat, Polecat: "nux", WorkDir: clone}
	if err := ensurePolecatOffMain(ctx, &beads.Issue{ID: "gt-tk5"}); err != nil {
		t.Fatalf("dry-run should not error, got: %v", err)
	}
	if got := currentBranch(t, clone); got != "main" {
		t.Errorf("dry-run mutated branch to %q; want main", got)
	}
}
