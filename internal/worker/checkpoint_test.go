package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitOut runs git in dir and returns trimmed stdout, failing the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Isolate from the developer's global git config.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// newCheckpointRepo builds a bare "host" repo and a cloned worktree with one
// committed file, returning (worktree, bare, checkpointer).
func newCheckpointRepo(t *testing.T) (string, string, *Checkpointer) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "host.git")
	wt := filepath.Join(root, "wt")
	gitOut(t, root, "init", "--bare", bare)
	gitOut(t, root, "clone", bare, wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("v1\n"), 0644))
	gitOut(t, wt, "add", "main.go")
	gitOut(t, wt, "commit", "-m", "initial")
	gitOut(t, wt, "push", "origin", "HEAD")
	return wt, bare, &Checkpointer{
		Worktree: wt,
		Ref:      CheckpointRefForPolecat("furiosa"),
		Remote:   "origin",
	}
}

func TestCheckpoint_OrphanCommitPushedBranchUntouched(t *testing.T) {
	wt, bare, c := newCheckpointRepo(t)
	branchBefore := gitOut(t, wt, "rev-parse", "HEAD")

	// Tracked modification gets captured...
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("v2\n"), 0644))
	// ...but an untracked tree (node_modules-style) must NOT be swept in.
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "node_modules", "x"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "node_modules", "x", "big.js"), []byte("junk"), 0644))

	pushed, err := c.Checkpoint(context.Background())
	require.NoError(t, err)
	assert.True(t, pushed)

	ref := c.Ref
	sha := gitOut(t, bare, "rev-parse", ref)

	// Orphan: no parents.
	parents := gitOut(t, bare, "log", "-1", "--format=%P", sha)
	assert.Empty(t, parents, "checkpoint commit must be an orphan")

	// Contains the tracked change, not the untracked tree.
	files := gitOut(t, bare, "ls-tree", "-r", "--name-only", sha)
	assert.Contains(t, files, "main.go")
	assert.NotContains(t, files, "node_modules")
	content := gitOut(t, bare, "show", sha+":main.go")
	assert.Equal(t, "v2", content)

	// Branch, HEAD, and the agent's index are untouched.
	assert.Equal(t, branchBefore, gitOut(t, wt, "rev-parse", "HEAD"))
	staged := gitOut(t, wt, "diff", "--cached", "--name-only")
	assert.Empty(t, staged, "the agent's real index must not be disturbed")
}

func TestCheckpoint_ForceMovesRefAndSkipsNoops(t *testing.T) {
	wt, bare, c := newCheckpointRepo(t)

	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("v2\n"), 0644))
	pushed, err := c.Checkpoint(context.Background())
	require.NoError(t, err)
	require.True(t, pushed)
	first := gitOut(t, bare, "rev-parse", c.Ref)

	// No changes → no new checkpoint, no error.
	pushed, err = c.Checkpoint(context.Background())
	require.NoError(t, err)
	assert.False(t, pushed)
	assert.Equal(t, first, gitOut(t, bare, "rev-parse", c.Ref))

	// New change → ref force-moves to a fresh orphan; the old commit is left
	// unreferenced (reclaimable by gc), not chained.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("v3\n"), 0644))
	pushed, err = c.Checkpoint(context.Background())
	require.NoError(t, err)
	require.True(t, pushed)
	second := gitOut(t, bare, "rev-parse", c.Ref)
	assert.NotEqual(t, first, second)
	assert.Empty(t, gitOut(t, bare, "log", "-1", "--format=%P", second))
}

func TestCheckpoint_SkipsWhenNotQuiescent(t *testing.T) {
	wt, _, c := newCheckpointRepo(t)
	c.Debounce = 300 * time.Millisecond

	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("v2\n"), 0644))
	// Keep the worktree churning during the debounce window.
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				_ = os.WriteFile(filepath.Join(wt, "main.go"), []byte(strings.Repeat("x", i+1)), 0644)
			}
		}
	}()
	pushed, err := c.Checkpoint(context.Background())
	close(stop)
	require.NoError(t, err)
	assert.False(t, pushed, "a churning worktree must be skipped, not captured mid-flush")
}

func TestCheckpoint_PushFailureKeepsLocalRef(t *testing.T) {
	wt, _, c := newCheckpointRepo(t)
	// Break the remote.
	gitOut(t, wt, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))

	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("v2\n"), 0644))
	pushed, err := c.Checkpoint(context.Background())
	require.Error(t, err)
	assert.False(t, pushed)

	// The LOCAL checkpoint ref still holds the work (durability delayed, not
	// lost).
	sha := gitOut(t, wt, "rev-parse", c.Ref)
	assert.Equal(t, "v2", gitOut(t, wt, "show", sha+":main.go"))
}

func TestSupervisor_MaxRuntimeSelfReleaseWithFinalFlush(t *testing.T) {
	wt, bare, c := newCheckpointRepo(t)
	sup := NewSupervisor(SupervisorConfig{
		Checkpointer: c,
		Interval:     50 * time.Millisecond,
		MaxRuntime:   120 * time.Millisecond,
	})

	// Change made just before the cap fires: the final flush must capture it
	// even though no regular tick got to it.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("last-words\n"), 0644))

	done := make(chan StopReason, 1)
	go func() { done <- sup.Run(context.Background()) }()
	select {
	case reason := <-done:
		assert.Equal(t, StopMaxRuntime, reason)
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not self-release at max_runtime")
	}

	sha := gitOut(t, bare, "rev-parse", c.Ref)
	assert.Equal(t, "last-words", gitOut(t, bare, "show", sha+":main.go"))
}

func TestSupervisor_DeadmanSelfRelease(t *testing.T) {
	wt, _, c := newCheckpointRepo(t)
	// Control plane gone: every push and probe fails.
	gitOut(t, wt, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))

	sup := NewSupervisor(SupervisorConfig{
		Checkpointer: c,
		Interval:     50 * time.Millisecond,
		DeadmanAfter: 200 * time.Millisecond,
	})
	done := make(chan StopReason, 1)
	go func() { done <- sup.Run(context.Background()) }()
	select {
	case reason := <-done:
		assert.Equal(t, StopDeadman, reason)
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not self-release on dead-man window")
	}
}

func TestSupervisor_InterruptRunsShutdownSequence(t *testing.T) {
	wt, bare, c := newCheckpointRepo(t)

	stopped := false
	sup := NewSupervisor(SupervisorConfig{
		Checkpointer: c,
		Interval:     time.Hour, // no regular tick will fire
		StopWork: func(context.Context) error {
			stopped = true
			return nil
		},
	})

	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("interrupted\n"), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan StopReason, 1)
	go func() { done <- sup.Run(ctx) }()
	cancel() // provider interruption / operator shutdown

	select {
	case reason := <-done:
		assert.Equal(t, StopInterrupted, reason)
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not stop on interruption")
	}

	assert.True(t, stopped, "StopWork must run before the final flush")
	sha := gitOut(t, bare, "rev-parse", c.Ref)
	assert.Equal(t, "interrupted", gitOut(t, bare, "show", sha+":main.go"))
}

func TestCheckpoint_StagedNewFileNotCaptured(t *testing.T) {
	wt, bare, c := newCheckpointRepo(t)

	// A NEW file the agent staged but has not committed: not in HEAD, so
	// tracked-only (`add -u`) staging deliberately excludes it — the design's
	// untracked-but-wanted exception (§9.2). This test locks the boundary in
	// so a change to it is a conscious decision, not an accident.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "brand-new.go"), []byte("staged\n"), 0644))
	gitOut(t, wt, "add", "brand-new.go")
	// Plus a tracked change so a checkpoint actually happens.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("v2\n"), 0644))

	pushed, err := c.Checkpoint(context.Background())
	require.NoError(t, err)
	require.True(t, pushed)

	files := gitOut(t, bare, "ls-tree", "-r", "--name-only", c.Ref)
	assert.Contains(t, files, "main.go")
	assert.NotContains(t, files, "brand-new.go",
		"staged-but-uncommitted new files are excluded by design (tracked-in-HEAD only)")

	// The agent's own staging area still holds the file untouched.
	staged := gitOut(t, wt, "diff", "--cached", "--name-only")
	assert.Contains(t, staged, "brand-new.go")
}

func TestCheckpoint_RejectsNonRefsRef(t *testing.T) {
	_, _, c := newCheckpointRepo(t)
	c.Ref = "--upload-pack=evil"
	_, err := c.Checkpoint(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must start with refs/")
}

// TestSupervisor_HungGitCannotStarveWatchdog pins the OpTimeout guarantee: a
// checkpoint/probe that HANGS (silent partition — no RST, no fast failure)
// is killed at the per-op deadline, so the max-runtime/dead-man checks keep
// running and self-release still fires.
func TestSupervisor_HungGitCannotStarveWatchdog(t *testing.T) {
	wt, _, c := newCheckpointRepo(t)

	// A remote whose transport hangs: an ext:: command that sleeps far past
	// the test horizon without ever speaking the git protocol.
	gitOut(t, wt, "config", "protocol.ext.allow", "always")
	gitOut(t, wt, "remote", "set-url", "origin", "ext::sh -c \"sleep 600\"")
	// Ensure there is something to push so Checkpoint reaches the transport.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "main.go"), []byte("v2\n"), 0644))

	sup := NewSupervisor(SupervisorConfig{
		Checkpointer: c,
		Interval:     50 * time.Millisecond,
		OpTimeout:    150 * time.Millisecond,
		DeadmanAfter: 400 * time.Millisecond,
	})
	done := make(chan StopReason, 1)
	go func() { done <- sup.Run(context.Background()) }()
	select {
	case reason := <-done:
		assert.Equal(t, StopDeadman, reason)
	case <-time.After(15 * time.Second):
		t.Fatal("hung git starved the watchdog — dead-man self-release never fired")
	}
}
