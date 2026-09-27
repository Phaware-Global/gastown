package daemon

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/git"
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
	// checkpointWorktree resolves its protected branch from rig config
	// (gt-35un/gt-xitk MAYOR DESIGN RULING) and falls back to "main" when
	// none is found — as here, since newTestDaemon has no config. The guard
	// fails closed when that branch doesn't resolve on origin, so it must
	// actually exist there, matching a real polecat worktree's origin/main.
	run(localDir, "branch", "-M", "main")
	run(localDir, "remote", "add", "origin", remoteDir)
	run(localDir, "push", "-u", "origin", "main")
	run(localDir, "checkout", "-b", "polecat/foo/bead@1")

	return localDir
}

func newTestDaemon() *Daemon {
	return &Daemon{logger: log.New(io.Discard, "", 0)}
}

func TestCheckpointWorktree_CommitsAndPushesPreserveRef(t *testing.T) {
	workDir := initCheckpointTestRepo(t)
	// initCheckpointTestRepo lays out <tmp>/remote.git next to <tmp>/local
	// (the returned workDir) — see its definition below.
	remoteDir := filepath.Join(filepath.Dir(workDir), "remote.git")
	// Modify a file already tracked by initCheckpointTestRepo's initial
	// commit: staging is `git add -u` (allowlist, gt-i4ej FIX 1), which only
	// picks up changes to files git already tracks, never new untracked
	// files (see the matching comment in preserve_test.go).
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# Test\nmodified\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	headBeforeOut, err := exec.Command("git", "-C", workDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD before: %v", err)
	}
	headBefore := strings.TrimSpace(string(headBeforeOut))

	d := newTestDaemon()
	if !d.checkpointWorktree(workDir, "myrig", "foo") {
		t.Fatal("expected checkpointWorktree to report a preservation")
	}

	// gt-94p1 root fix: a checkpoint must NEVER move the branch. If the WIP
	// commit stayed on HEAD, any later ordinary push of this branch (the
	// polecat's own push, gt done, a pre-nuke push) would carry it to a real
	// PR branch — the mechanism behind gt-2stz's four incidents.
	headAfterOut, err := exec.Command("git", "-C", workDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD after: %v", err)
	}
	if headAfter := strings.TrimSpace(string(headAfterOut)); headAfter != headBefore {
		t.Fatalf("branch HEAD moved from %s to %s — a checkpoint must leave the branch exactly where it was", headBefore, headAfter)
	}

	// A test named "...AndPushesPreserveRef" must actually verify the push:
	// mutation testing showed the previous version here stayed green with
	// Push:true deleted entirely, because it only ever inspected `git log`
	// on the local worktree (PR #184 review). Derive the expected ref name
	// from the function under test rather than hand-writing it, and check
	// the bare remote directly.
	wantRef := git.PreservationRefName("polecat/foo/bead@1")
	lsOut, err := exec.Command("git", "ls-remote", "--heads", remoteDir, wantRef).Output()
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	fields := strings.Fields(string(lsOut))
	if len(fields) < 2 {
		t.Fatalf("preservation ref %q was not pushed to the bare remote (ls-remote returned %q)", wantRef, lsOut)
	}
	preservedSHA := fields[0]
	if preservedSHA == headBefore {
		t.Fatalf("preservation ref %s points at the unchanged branch head %s — the checkpoint commit was not what got pushed", wantRef, headBefore)
	}

	// The WIP subject now lives ONLY on the preservation ref's commit. The
	// commit object exists locally too (it was made here before being
	// rolled off the branch), so it can be inspected directly.
	subjOut, err := exec.Command("git", "-C", workDir, "log", "-1", "--format=%s", preservedSHA).Output()
	if err != nil {
		t.Fatalf("git log %s: %v", preservedSHA, err)
	}
	if got := string(subjOut); got != checkpoint.WIPCommitPrefix+"\n" {
		t.Fatalf("preserved commit subject = %q, want %q", got, checkpoint.WIPCommitPrefix)
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
