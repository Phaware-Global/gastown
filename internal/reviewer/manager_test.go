package reviewer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
)

// runTestGit runs a git command in dir, failing the test on error.
func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// TestProvisionWorktree_DetachedWhenRepoHoldsBaseBranch verifies that the
// reviewer worktree is provisioned successfully even when another worktree
// (e.g. refinery/rig) already has the base branch (main) checked out.
//
// This is the scenario from gt-gfh: `git worktree add <path> main` fails with
// "fatal: 'main' is already checked out" when the refinery holds it.
// The fix is to use `git worktree add --detach <path> main`, which detaches
// HEAD at main's commit without taking the branch ref.
func TestProvisionWorktree_DetachedWhenRepoHoldsBaseBranch(t *testing.T) {
	tmp := t.TempDir()

	// Build a bare repo at <tmp>/.repo.git so ensureWorktree picks it as the
	// source. A linked worktree can hold "main" without conflicting with the
	// bare repo itself (bare repos have no working tree), which gives us
	// clean control over which worktrees hold which branches.
	bareDir := filepath.Join(tmp, ".repo.git")
	if err := os.MkdirAll(bareDir, 0o755); err != nil {
		t.Fatalf("mkdir .repo.git: %v", err)
	}
	// Use a staging clone to create the bare repo with an initial commit on main.
	stageDir := t.TempDir()
	runTestGit(t, stageDir, "init", "--initial-branch=main")
	runTestGit(t, stageDir, "config", "user.email", "test@test.com")
	runTestGit(t, stageDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(stageDir, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runTestGit(t, stageDir, "add", ".")
	runTestGit(t, stageDir, "commit", "-m", "initial")
	runTestGit(t, stageDir, "clone", "--bare", stageDir, bareDir)

	// Add refinery/rig as a linked worktree on main — simulating the refinery
	// holding the base branch while the reviewer is being provisioned.
	refineryDir := filepath.Join(tmp, "refinery", "rig")
	if err := os.MkdirAll(filepath.Dir(refineryDir), 0o755); err != nil {
		t.Fatalf("mkdir refinery: %v", err)
	}
	runTestGit(t, bareDir, "worktree", "add", refineryDir, "main")

	// The reviewer worktree must not already exist (ensureWorktree short-circuits).
	reviewerDir := filepath.Join(tmp, "reviewer", "rig")
	if _, err := os.Stat(reviewerDir); err == nil {
		t.Fatal("reviewer/rig already exists before test — unexpected")
	}

	r := &rig.Rig{Name: "test", Path: tmp}
	m := NewManager(r)

	// Before the fix this would fail: "fatal: 'main' is already checked out at …"
	got, err := m.ensureWorktree()
	if err != nil {
		t.Fatalf("ensureWorktree failed when refinery holds main: %v", err)
	}
	if got != reviewerDir {
		t.Errorf("ensureWorktree returned %q, want %q", got, reviewerDir)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("reviewer worktree directory not created: %v", err)
	}

	// The reviewer worktree must be in detached HEAD state (no branch checked out).
	cmd := exec.Command("git", "symbolic-ref", "--quiet", "HEAD")
	cmd.Dir = got
	out, _ := cmd.Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("reviewer worktree has a branch checked out (%q); expected detached HEAD",
			strings.TrimSpace(string(out)))
	}
}
