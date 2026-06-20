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

// initReviewerTestRepo creates a minimal rig directory structure for Manager
// tests: a source git repo at <tmp>/mayor/rig with one commit on main.
// Returns the rig root (tmp) and the source repo path.
func initReviewerTestRepo(t *testing.T) (rigRoot, sourceDir string) {
	t.Helper()
	tmp := t.TempDir()
	sourceDir = filepath.Join(tmp, "mayor", "rig")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}
	runTestGit(t, sourceDir, "init", "--initial-branch=main")
	runTestGit(t, sourceDir, "config", "user.email", "test@test.com")
	runTestGit(t, sourceDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runTestGit(t, sourceDir, "add", ".")
	runTestGit(t, sourceDir, "commit", "-m", "initial")
	return tmp, sourceDir
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
	rigRoot, sourceDir := initReviewerTestRepo(t)

	// Simulate refinery/rig holding main by adding a worktree on main.
	refineryDir := filepath.Join(rigRoot, "refinery", "rig")
	if err := os.MkdirAll(filepath.Dir(refineryDir), 0o755); err != nil {
		t.Fatalf("mkdir refinery: %v", err)
	}
	runTestGit(t, sourceDir, "worktree", "add", refineryDir, "main")

	// The reviewer worktree must not already exist (ensureWorktree short-circuits).
	reviewerDir := filepath.Join(rigRoot, "reviewer", "rig")
	if _, err := os.Stat(reviewerDir); err == nil {
		t.Fatal("reviewer/rig already exists before test — unexpected")
	}

	r := &rig.Rig{Name: "test", Path: rigRoot}
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
		t.Errorf("reviewer worktree has a branch checked out (%q); expected detached HEAD", strings.TrimSpace(string(out)))
	}
}
