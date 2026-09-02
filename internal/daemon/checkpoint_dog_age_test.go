package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// initAgedTestRepo creates a git repo whose only commit is backdated to
// oldDate — the shape of a freshly-created polecat worktree checked out at a
// base branch that last moved long ago.
func initAgedTestRepo(t *testing.T, oldDate string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(nil, "init")
	run(nil, "config", "user.email", "test@test.com")
	run(nil, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run(nil, "add", ".")
	run([]string{"GIT_AUTHOR_DATE=" + oldDate, "GIT_COMMITTER_DATE=" + oldDate}, "commit", "-m", "ancient base")
	return dir
}

// TestCheckpointDogWorktreeAge_FreshWorktreeOnOldBaseIsNotAbandoned pins the
// PR #184 finding that HEAD's commit date is the BASE BRANCH's date for a
// polecat that died before its first commit: a worktree created minutes ago
// on a base that last moved a year ago must not be classified abandoned —
// that is exactly the dead-session population this dog exists to preserve.
// The worktree's own activity signals (the git index was just written) must
// win over the inherited commit date.
func TestCheckpointDogWorktreeAge_FreshWorktreeOnOldBaseIsNotAbandoned(t *testing.T) {
	workDir := initAgedTestRepo(t, "2024-01-01T00:00:00Z")
	townRoot := t.TempDir() // no session heartbeat — the reaped case

	age, err := checkpointDogWorktreeAge(workDir, townRoot, "gt-rig-polecat")
	if err != nil {
		t.Fatalf("checkpointDogWorktreeAge: %v", err)
	}
	if age > checkpointDogAbandonedThreshold {
		t.Fatalf("fresh worktree classified abandoned: age=%v (> %v) — HEAD's commit date is the base branch's date, not this worktree's activity", age, checkpointDogAbandonedThreshold)
	}
}

// TestCheckpointDogWorktreeAge_GenuinelyStaleWorktreeIsAbandoned pins the
// other direction: when every available signal (heartbeat missing, index
// mtime, HEAD commit date) is old, the worktree is classified abandoned.
func TestCheckpointDogWorktreeAge_GenuinelyStaleWorktreeIsAbandoned(t *testing.T) {
	workDir := initAgedTestRepo(t, "2024-01-01T00:00:00Z")
	townRoot := t.TempDir()

	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(workDir, ".git", "index"), old, old); err != nil {
		t.Fatalf("backdating index: %v", err)
	}

	age, err := checkpointDogWorktreeAge(workDir, townRoot, "gt-rig-polecat")
	if err != nil {
		t.Fatalf("checkpointDogWorktreeAge: %v", err)
	}
	if age < checkpointDogAbandonedThreshold {
		t.Fatalf("stale worktree not classified abandoned: age=%v (< %v)", age, checkpointDogAbandonedThreshold)
	}
}
