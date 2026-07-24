package worker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Checkpointer implements the continuous work-checkpoint protocol
// (docs/design/remote-polecat-execution.md §9.2): every interval — and on
// shutdown — the worktree's tracked changes are committed as a DISPOSABLE
// orphan commit, the checkpoint ref is force-moved to it, and the ref is
// force-pushed to the remote (in production, the host `.repo.git` through the
// relay). The polecat branch is never touched: it advances only on the
// agent's own commits and `gt done`.
//
// Checkpoint commits are orphans (no parent) so a long session never
// accumulates an interval-granularity commit chain — each force-move leaves
// the previous checkpoint unreferenced for the host's periodic `git gc`.
// Staging is tracked-only (`git add -u` semantics, .gitignore irrelevant by
// construction): untracked trees like node_modules or build caches are never
// swept in. Untracked-but-wanted files are the design's explicit rare
// exception and are not handled here.
//
// All git operations run against a TEMPORARY index file, so the agent's real
// index, HEAD, and branch state are never disturbed by a checkpoint.
type Checkpointer struct {
	Worktree string // polecat worktree path
	Ref      string // checkpoint ref, e.g. refs/checkpoints/polecat/furiosa
	Remote   string // git remote to push to (default "origin")

	// Debounce is the quiescence window: a checkpoint is taken only when the
	// worktree status is unchanged across this pause, so a half-written file
	// is not captured mid-flush. The checkpoint interval is a ceiling, not a
	// metronome — a busy worktree skips the tick. 0 disables the guard
	// (used by the final shutdown flush, which must capture what's there).
	Debounce time.Duration

	lastTree string // tree hash of the last checkpoint taken (skip no-ops)
}

// git runs a git command in the worktree with optional extra env, returning
// trimmed stdout.
func (c *Checkpointer) git(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = c.Worktree
	cmd.Env = append(os.Environ(), extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// remote returns the effective remote name.
func (c *Checkpointer) remote() string {
	if c.Remote == "" {
		return "origin"
	}
	return c.Remote
}

// buildTree snapshots the tracked worktree state (current HEAD's tree plus
// all tracked modifications/deletions) into a throwaway index and returns the
// resulting tree hash. The agent's real index and HEAD are untouched.
//
// "Tracked" means tracked in HEAD: `add -u` only updates paths HEAD already
// knows, so a NEW file the agent `git add`ed but has not yet committed
// (staged-only) is NOT captured — that is the design's documented
// untracked-but-wanted exception (§9.2), accepted so the checkpoint can never
// sweep in untracked trees. TestCheckpoint_StagedNewFileNotCaptured pins the
// boundary.
func (c *Checkpointer) buildTree(ctx context.Context) (string, error) {
	// The index lives in a private 0700 temp dir: git recreates index files
	// with umask perms at whatever path GIT_INDEX_FILE names, so the
	// directory provides the isolation a shared /tmp path would lose.
	tmpDir, err := os.MkdirTemp("", "gt-checkpoint-*")
	if err != nil {
		return "", fmt.Errorf("temp index dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	idxEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(tmpDir, "index")}

	if _, err := c.git(ctx, idxEnv, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := c.git(ctx, idxEnv, "add", "-u"); err != nil {
		return "", err
	}
	return c.git(ctx, idxEnv, "write-tree")
}

// Checkpoint captures the current tracked worktree state into the checkpoint
// ref and force-pushes it. Returns pushed=false with a nil error when there
// was nothing new to capture or the worktree was not quiescent.
//
// A push failure still leaves the LOCAL checkpoint ref updated — durability
// is delayed, never lost locally — and the error is returned for the
// caller's backoff/dead-man accounting (§9.6).
func (c *Checkpointer) Checkpoint(ctx context.Context) (pushed bool, err error) {
	// Guard against a misconfigured ref being parsed as a git option; the
	// argv builders also use `--` end-of-options (defense-in-depth — Ref and
	// Remote come from our own flags, never from relayed input).
	if !strings.HasPrefix(c.Ref, "refs/") {
		return false, fmt.Errorf("checkpoint ref %q must start with refs/", c.Ref)
	}

	tree, err := c.buildTree(ctx)
	if err != nil {
		return false, err
	}

	// Quiescence guard: the tree must be CONTENT-stable across the debounce
	// window (porcelain status is content-insensitive — a file being
	// rewritten in place shows the same " M" line before and after). If the
	// snapshot moved, the worktree is mid-flush: skip, the next tick retries.
	if c.Debounce > 0 {
		select {
		case <-time.After(c.Debounce):
		case <-ctx.Done():
			return false, ctx.Err()
		}
		again, err := c.buildTree(ctx)
		if err != nil {
			return false, err
		}
		if again != tree {
			return false, nil // busy worktree; the next tick will retry
		}
	}

	if tree == c.lastTree {
		return false, nil // nothing changed since the last checkpoint
	}

	// Orphan commit: no parent (see type comment).
	commit, err := c.git(ctx, nil, "commit-tree", tree, "-m",
		fmt.Sprintf("checkpoint %s", time.Now().UTC().Format(time.RFC3339)))
	if err != nil {
		return false, err
	}
	if _, err := c.git(ctx, nil, "update-ref", "--", c.Ref, commit); err != nil {
		return false, err
	}
	c.lastTree = tree

	if _, err := c.git(ctx, nil, "push", "--force", "--", c.remote(),
		c.Ref+":"+c.Ref); err != nil {
		return false, fmt.Errorf("checkpoint push (local ref %s updated): %w", c.Ref, err)
	}
	return true, nil
}

// Probe cheaply verifies the remote (the control plane, in production) is
// reachable — used to feed the dead-man's switch on ticks with nothing to
// push.
func (c *Checkpointer) Probe(ctx context.Context) error {
	_, err := c.git(ctx, nil, "ls-remote", "--heads", "--", c.remote())
	return err
}

// CheckpointRefForPolecat returns the §9.2 checkpoint ref for a polecat name.
func CheckpointRefForPolecat(name string) string {
	return "refs/checkpoints/polecat/" + filepath.Base(name)
}
