package git

import (
	"fmt"
	"strings"
)

// PreserveOptions configures AutoPreserveUncommittedWork.
type PreserveOptions struct {
	// IssueID, if set, is embedded in the auto-save commit message so the
	// resulting commit can be traced back to the bead it was working on.
	IssueID string

	// Push, when true, pushes the (possibly just-created) HEAD commit to a
	// dedicated preservation ref on Remote and verifies the remote tip
	// matches. Commit-only is not preservation if the worktree can be
	// destroyed, reassigned, or reset before anything else pushes the
	// branch (gt-y8ts) — callers that sit in front of a destructive
	// operation (nuke, reassignment, periodic checkpointing) should set
	// this. Callers that know a normal push follows shortly after in the
	// same flow (gt done) can leave it false.
	//
	// The preservation ref is named "preserve/<branch>" rather than the
	// branch's own remote counterpart, so a mid-work auto-save never force
	// pushes over a branch that may already have an open PR, review
	// state, or merge-queue attention on it.
	Push bool

	// Remote defaults to "origin".
	Remote string

	// ExtraExcludePaths are unstaged after the built-in runtime-artifact
	// exclusions, for repo- or caller-specific files that must never land
	// in a preservation commit (e.g. an overlay CLAUDE.md carrying a
	// lifecycle marker). Paths that aren't staged are a no-op.
	ExtraExcludePaths []string

	// CommitMessage overrides the default "fix: auto-save uncommitted
	// implementation work (gt-pvx safety net)" message. Callers with their
	// own recognizable marker (e.g. checkpoint_dog's "WIP: checkpoint
	// (auto)" prefix, matched elsewhere for squashing) should set this
	// rather than let their commits go unrecognized by that tooling.
	CommitMessage string
}

// PreserveResult reports what AutoPreserveUncommittedWork actually did.
type PreserveResult struct {
	// Committed is true if a new auto-save commit was created.
	Committed bool
	// Pushed is true if HEAD was pushed to the preservation ref and the
	// push was verified against the remote tip.
	Pushed bool
	// Commit is the HEAD SHA after the call, set whenever Committed or
	// Pushed is true.
	Commit string
	// Ref is the preservation ref HEAD was pushed to, set when Pushed.
	Ref string
}

// PreservationRefName returns the dedicated ref a branch's work-in-progress
// is preserved to, distinct from the branch's own remote counterpart.
func PreservationRefName(branch string) string {
	sanitized := strings.NewReplacer("/", "-").Replace(branch)
	return "preserve/" + sanitized
}

// AutoPreserveUncommittedWork commits any uncommitted (staged or unstaged)
// work on branch — excluding Gas Town runtime scaffolding and deletions of
// tracked files — and, if requested, pushes+verifies it to a preservation
// ref. It is the single implementation behind every safety net that must
// never let a polecat's real work disappear: gt done's gt-pvx auto-save,
// polecat removal/nuke, and the checkpoint_dog patrol all call this instead
// of reimplementing the logic (gt-y8ts).
//
// Refuses to touch protected branches (G41 guard): auto-committing onto
// main/master/develop would land in-progress work directly on the
// integration branch. Refuses on unmerged conflicts rather than staging
// conflict markers into a commit.
func AutoPreserveUncommittedWork(g *Git, branch string, opts PreserveOptions) (*PreserveResult, error) {
	result := &PreserveResult{}
	if branch == "" {
		return result, nil
	}
	if IsProtectedBranch(branch) {
		return result, fmt.Errorf("refusing to auto-preserve uncommitted work on protected branch %q (G41 guard)", branch)
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		return nil, fmt.Errorf("checking uncommitted work: %w", err)
	}

	if status.HasUncommittedChanges && !status.CleanExcludingRuntime() {
		if len(status.UnmergedFiles) > 0 {
			return result, fmt.Errorf("cannot auto-preserve unmerged conflicts: %s", strings.Join(status.UnmergedFiles, ", "))
		}

		if err := g.Add("-A"); err != nil {
			return nil, fmt.Errorf("staging changes: %w", err)
		}
		// Unstage Gas Town overlay/runtime files that `git add -A` picked up —
		// these are runtime artifacts and must never land in a preservation
		// commit. Mirrors gt done's gt-pvx exclusion policy.
		_ = g.ResetFiles("CLAUDE.local.md")
		for _, path := range status.RuntimeArtifactPaths() {
			_ = g.ResetFiles(path)
		}
		for _, path := range opts.ExtraExcludePaths {
			_ = g.ResetFiles(path)
		}
		// Unstage deletions of tracked files: a safety net must preserve
		// work (additions + modifications), never destroy it (deletions).
		if deletions, delErr := g.StagedDeletions(); delErr == nil && len(deletions) > 0 {
			_ = g.ResetFiles(deletions...)
		}

		msg := opts.CommitMessage
		if msg == "" {
			msg = "fix: auto-save uncommitted implementation work (gt-pvx safety net)"
			if opts.IssueID != "" {
				msg = fmt.Sprintf("fix: auto-save uncommitted implementation work (%s, gt-pvx safety net)", opts.IssueID)
			}
		}
		if err := g.CommitNoVerify(msg); err != nil {
			return nil, fmt.Errorf("auto-committing: %w", err)
		}
		result.Committed = true
	}

	if !opts.Push {
		return result, nil
	}

	remote := opts.Remote
	if remote == "" {
		remote = "origin"
	}

	head, revErr := g.Rev("HEAD")
	if revErr != nil {
		return result, fmt.Errorf("resolving HEAD: %w", revErr)
	}

	// Skip the push only when we're certain there's nothing new to preserve.
	// Two independent proofs, either is sufficient: the preservation ref
	// already points at this exact HEAD (this exact state was already
	// pushed by a prior call — the common repeated-checkpoint case), or the
	// worktree's status shows zero commits not already reachable from a
	// known base (nothing polecat-specific has ever happened here — the
	// freshly-spawned/idle case). Absent either proof, push: a caller found
	// with local-only commits and no matching preserve-ref tip is exactly
	// the "committed but never pushed, worktree about to vanish" case this
	// function exists to catch.
	if !result.Committed {
		if tip, err := g.PushRemoteBranchTip(remote, PreservationRefName(branch)); err == nil && tip == head {
			return result, nil
		}
		if status.UnpushedCommits == 0 {
			return result, nil
		}
	}

	refName := PreservationRefName(branch)
	if err := g.Push(remote, "HEAD:refs/heads/"+refName, true); err != nil {
		return result, fmt.Errorf("pushing preservation ref %s: %w", refName, err)
	}
	if err := g.VerifyPushedCommit(remote, refName, head); err != nil {
		return result, fmt.Errorf("verifying preservation push: %w", err)
	}
	result.Pushed = true
	result.Ref = refName
	result.Commit = head
	return result, nil
}
