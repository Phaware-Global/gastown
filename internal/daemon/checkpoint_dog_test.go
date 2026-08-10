package daemon

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/checkpoint"
)

func TestCheckpointDogInterval_Default(t *testing.T) {
	interval := checkpointDogInterval(nil)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultCheckpointDogInterval, interval)
	}
}

func TestCheckpointDogInterval_NilPatrols(t *testing.T) {
	config := &DaemonPatrolConfig{}
	interval := checkpointDogInterval(config)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultCheckpointDogInterval, interval)
	}
}

func TestCheckpointDogInterval_NilCheckpointDog(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{},
	}
	interval := checkpointDogInterval(config)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval %v, got %v", defaultCheckpointDogInterval, interval)
	}
}

func TestCheckpointDogInterval_Configured(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			CheckpointDog: &CheckpointDogConfig{
				Enabled:     true,
				IntervalStr: "5m",
			},
		},
	}
	interval := checkpointDogInterval(config)
	if interval != 5*time.Minute {
		t.Errorf("expected 5m, got %v", interval)
	}
}

func TestCheckpointDogInterval_InvalidFallsBack(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			CheckpointDog: &CheckpointDogConfig{
				Enabled:     true,
				IntervalStr: "not-a-duration",
			},
		},
	}
	interval := checkpointDogInterval(config)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval for invalid config, got %v", interval)
	}
}

func TestCheckpointDogInterval_ZeroFallsBack(t *testing.T) {
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			CheckpointDog: &CheckpointDogConfig{
				Enabled:     true,
				IntervalStr: "0s",
			},
		},
	}
	interval := checkpointDogInterval(config)
	if interval != defaultCheckpointDogInterval {
		t.Errorf("expected default interval for zero config, got %v", interval)
	}
}

func TestCheckpointDogEnabled(t *testing.T) {
	// Nil config → disabled (opt-in patrol)
	if IsPatrolEnabled(nil, "checkpoint_dog") {
		t.Error("expected checkpoint_dog disabled for nil config")
	}

	// Explicitly enabled
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			CheckpointDog: &CheckpointDogConfig{
				Enabled: true,
			},
		},
	}
	if !IsPatrolEnabled(config, "checkpoint_dog") {
		t.Error("expected checkpoint_dog enabled")
	}

	// Explicitly disabled
	config.Patrols.CheckpointDog.Enabled = false
	if IsPatrolEnabled(config, "checkpoint_dog") {
		t.Error("expected checkpoint_dog disabled when Enabled=false")
	}
}

func TestResolveCheckpointWorkDir_NestedLayout(t *testing.T) {
	// New polecat layout: polecats/<name>/<rigName>/.git is the worktree.
	tmp := t.TempDir()
	rig := "myrig"
	polecat := "alice"
	polecatsDir := filepath.Join(tmp, "polecats")
	worktree := filepath.Join(polecatsDir, polecat, rig)
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := resolveCheckpointWorkDir(polecatsDir, polecat, rig)
	if got != worktree {
		t.Errorf("got %q, want %q", got, worktree)
	}
}

func TestResolveCheckpointWorkDir_LegacyFlatLayout(t *testing.T) {
	// Legacy layout: polecats/<name>/.git directly. polecat.Manager still
	// recognizes this; checkpoint_dog must too rather than silently skip.
	tmp := t.TempDir()
	rig := "myrig"
	polecat := "bob"
	polecatsDir := filepath.Join(tmp, "polecats")
	worktree := filepath.Join(polecatsDir, polecat)
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := resolveCheckpointWorkDir(polecatsDir, polecat, rig)
	if got != worktree {
		t.Errorf("got %q, want %q (legacy flat layout)", got, worktree)
	}
}

func TestResolveCheckpointWorkDir_NoGitNeitherLevel(t *testing.T) {
	// Critical regression case: polecat container exists but has no .git
	// at either level. Function MUST return "" so the caller skips, NOT
	// fall back to a parent dir (which would have the workspace's .git
	// and cause the wrong-branch checkpoint bug this code prevents).
	tmp := t.TempDir()
	rig := "myrig"
	polecat := "carol"
	polecatsDir := filepath.Join(tmp, "polecats")
	if err := os.MkdirAll(filepath.Join(polecatsDir, polecat, rig), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Simulate top-level workspace .git that git would walk up to find.
	// resolveCheckpointWorkDir must NOT return a path that lets git walk
	// to this — it should return "" so the caller skips entirely.
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatalf("setup parent .git: %v", err)
	}
	got := resolveCheckpointWorkDir(polecatsDir, polecat, rig)
	if got != "" {
		t.Errorf("got %q, want empty (skip — no polecat-level .git)", got)
	}
}

func TestResolveCheckpointWorkDir_PrefersNestedOverFlat(t *testing.T) {
	// If both levels have .git (transitional state during a migration),
	// prefer the nested (newer) layout.
	tmp := t.TempDir()
	rig := "myrig"
	polecat := "dave"
	polecatsDir := filepath.Join(tmp, "polecats")
	flat := filepath.Join(polecatsDir, polecat)
	nested := filepath.Join(flat, rig)
	for _, d := range []string{flat, nested} {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatalf("setup %s: %v", d, err)
		}
	}
	got := resolveCheckpointWorkDir(polecatsDir, polecat, rig)
	if got != nested {
		t.Errorf("got %q, want nested %q", got, nested)
	}
}

func TestIsGitWorktree(t *testing.T) {
	tmp := t.TempDir()
	if isGitWorktree(tmp) {
		t.Error("empty dir should not be a worktree")
	}
	// .git as directory (full clone)
	dirGit := filepath.Join(tmp, "fullclone")
	if err := os.MkdirAll(filepath.Join(dirGit, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isGitWorktree(dirGit) {
		t.Error(".git directory should count as worktree")
	}
	// .git as file (linked worktree — git uses a file pointing to commondir)
	fileGit := filepath.Join(tmp, "linked")
	if err := os.MkdirAll(fileGit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fileGit, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isGitWorktree(fileGit) {
		t.Error(".git file (linked worktree) should count as worktree")
	}
}

// initCheckpointTestRepo creates a local git worktree with a bare "origin"
// remote and returns the local worktree dir, on branch "polecat/foo/bead@1".
func initCheckpointTestRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	remoteDir := filepath.Join(tmp, "remote.git")
	localDir := filepath.Join(tmp, "local")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run(tmp, "init", "--bare", remoteDir)
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	run(localDir, "init")
	run(localDir, "config", "user.email", "test@test.com")
	run(localDir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(localDir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run(localDir, "add", ".")
	run(localDir, "commit", "-m", "initial")
	run(localDir, "remote", "add", "origin", remoteDir)
	run(localDir, "checkout", "-b", "polecat/foo/bead@1")

	return localDir
}

func newTestDaemon() *Daemon {
	return &Daemon{logger: log.New(io.Discard, "", 0)}
}

func TestCheckpointWorktree_CommitsAndPushesPreserveRef(t *testing.T) {
	workDir := initCheckpointTestRepo(t)
	if err := os.WriteFile(filepath.Join(workDir, "handler.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	d := newTestDaemon()
	if !d.checkpointWorktree(workDir, "myrig", "foo") {
		t.Fatal("expected checkpointWorktree to report a preservation")
	}

	cmd := exec.Command("git", "log", "-1", "--format=%s")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if got := string(out); got != checkpoint.WIPCommitPrefix+"\n" {
		t.Fatalf("commit subject = %q, want %q", got, checkpoint.WIPCommitPrefix)
	}
}

func TestCheckpointWorktree_CleanWorktreeReportsNothing(t *testing.T) {
	workDir := initCheckpointTestRepo(t)

	d := newTestDaemon()
	if d.checkpointWorktree(workDir, "myrig", "foo") {
		t.Fatal("expected checkpointWorktree to report nothing for a clean worktree")
	}
}

func TestCheckpointWorktree_RefusesProtectedBranch(t *testing.T) {
	workDir := initCheckpointTestRepo(t)

	// initCheckpointTestRepo already switched to a feature branch; switch
	// back to whatever the repo's default init branch was (main/master per
	// local git config) rather than assuming its exact name.
	cmd := exec.Command("git", "checkout", "-")
	cmd.Dir = workDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout -: %v\n%s", err, out)
	}
	branchCmd := exec.Command("git", "branch", "--show-current")
	branchCmd.Dir = workDir
	branchOut, err := branchCmd.Output()
	if err != nil {
		t.Fatalf("branch --show-current: %v", err)
	}
	defaultBranch := string(branchOut)
	if len(defaultBranch) > 0 && defaultBranch[len(defaultBranch)-1] == '\n' {
		defaultBranch = defaultBranch[:len(defaultBranch)-1]
	}
	if defaultBranch != "main" && defaultBranch != "master" && defaultBranch != "develop" {
		t.Skipf("repo default branch %q is not in protectedBranchSet", defaultBranch)
	}

	if err := os.WriteFile(filepath.Join(workDir, "handler.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	d := newTestDaemon()
	if d.checkpointWorktree(workDir, "myrig", "foo") {
		t.Fatal("must not checkpoint a protected branch (G41 guard)")
	}

	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = workDir
	out, err := statusCmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("work must remain uncommitted in the worktree after refusal")
	}
}
