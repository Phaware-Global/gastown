package git

import (
	"fmt"
	"path/filepath"
	"strings"
)

// unverifiedCommitTrailer marks an auto-save commit that could only be made
// by bypassing commit hooks (CommitNoVerify) — most often a secret scanner
// rejecting it. Recorded IN the commit message rather than relying on
// PreserveResult.HooksFailed alone: that field is fresh on every call
// (result is a new &PreserveResult{} each time), so a later call — with a
// clean worktree, nothing to commit, HooksFailed defaulting back to false —
// has no memory that HEAD's ancestry still contains an unverified commit,
// and would push it (gt-i4ej FIX 2 round-2 finding). The push gate below
// re-derives the fact from the commit range itself instead.
const unverifiedCommitTrailer = "Gastown-Unverified: pre-commit hook failed"

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
	// The preservation ref is named "polecat/preserve-<branch>" rather than
	// the branch's own remote counterpart, so a mid-work auto-save never
	// force pushes over a branch that may already have an open PR, review
	// state, or merge-queue attention on it. It must live under the
	// "polecat/" namespace: Gas Town repos commonly run a pre-push hook that
	// rejects any branch outside {default_branch, beads-sync, polecat/*,
	// integration/*} to block ad-hoc PR branches (see .githooks/pre-push in
	// this repo) — a bare "preserve/*" ref is exactly the kind of push that
	// hook exists to block, and gt-y8ts's own preservation attempt was
	// rejected by it on this repo before this fix (see bead notes/mail).
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

	// HooksFailed is true when the auto-save commit could only be made by
	// bypassing commit hooks (e.g. a secret scanner) because they failed.
	// The work is still Committed — never lost — but callers MUST NOT push
	// it: an unverified commit reaching origin is exactly the risk the
	// hooks exist to catch (gt-i4ej FIX 2). Never pushed, regardless of
	// opts.Push. Callers should escalate loudly, surfacing HookOutput.
	HooksFailed bool
	// HookOutput carries the failing hook's error output when HooksFailed,
	// for callers to include in their escalation.
	HookOutput string
}

// PreservationRefName returns the dedicated ref a branch's work-in-progress
// is preserved to, distinct from the branch's own remote counterpart.
//
// Namespaced under "polecat/" (not a bare "preserve/*") so the push passes
// Gas Town's pre-push hook allowlist — see the Push field doc above.
func PreservationRefName(branch string) string {
	sanitized := strings.NewReplacer("/", "-", " ", "-").Replace(branch)
	return "polecat/preserve-" + sanitized
}

// detachedPreservationIdentity returns a value unique to the calling
// polecat's worktree, used to disambiguate a detached-HEAD preservation ref
// (gt-i4ej FIX 3). Prefers issueID (the polecat/bead identity callers
// already pass via PreserveOptions.IssueID); falls back to the worktree's
// directory name, which is unique per polecat by construction. Errors
// rather than guessing when neither is available — silently collapsing two
// polecats onto the same ref is worse than refusing to preserve.
func detachedPreservationIdentity(g *Git, issueID string) (string, error) {
	identity := issueID
	if identity == "" {
		identity = filepath.Base(g.WorkDir())
	}
	if identity == "" || identity == "." || identity == string(filepath.Separator) {
		return "", fmt.Errorf("no polecat/bead identity available to disambiguate a detached-HEAD preservation ref")
	}
	return identity, nil
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

		// ALLOWLIST, not denylist: stage only modifications/deletions to
		// files git already tracks (`git add -u`). `git add -A` stages
		// every untracked file too, and a denylist of known Gas Town
		// runtime paths can never enumerate what it has never seen — a
		// .env, a dumped token, a debug log with a session key. This runs
		// unattended and can force-push the result to origin, which makes
		// it an automated secret-publication path (gt-i4ej FIX 1). The
		// accepted tradeoff: a genuinely new untracked source file is not
		// captured by this safety net. A safety net that occasionally
		// misses a new file is fine; one that occasionally publishes a
		// credential is not.
		if err := g.Add("-u"); err != nil {
			return nil, fmt.Errorf("staging changes: %w", err)
		}
		// Unstage Gas Town overlay/runtime files that were already tracked
		// (e.g. a checked-in CLAUDE.local.md) — these are runtime artifacts
		// and must never land in a preservation commit. Mirrors gt done's
		// gt-pvx exclusion policy.
		var excludePaths []string
		excludePaths = append(excludePaths, "CLAUDE.local.md")
		excludePaths = append(excludePaths, status.RuntimeArtifactPaths()...)
		excludePaths = append(excludePaths, opts.ExtraExcludePaths...)

		var resetErrs []error
		for _, path := range excludePaths {
			if err := g.ResetFiles(path); err != nil {
				resetErrs = append(resetErrs, fmt.Errorf("%s: %w", path, err))
			}
		}
		// Unstage deletions of tracked files: a safety net must preserve
		// work (additions + modifications), never destroy it (deletions). A
		// failed query is treated exactly like a failed reset below rather
		// than silently skipped — the old `delErr == nil &&` short-circuit
		// let a query error (e.g. a concurrent index.lock) through as if
		// there were simply no deletions, so any staged deletion went
		// uninspected into the commit and, on Push:true paths, force-pushed
		// (PR #184 review).
		if deletions, delErr := g.StagedDeletions(); delErr != nil {
			resetErrs = append(resetErrs, fmt.Errorf("querying staged deletions: %w", delErr))
		} else if len(deletions) > 0 {
			if err := g.ResetFiles(deletions...); err != nil {
				resetErrs = append(resetErrs, fmt.Errorf("staged deletions: %w", err))
			}
			excludePaths = append(excludePaths, deletions...)
		}

		// Re-verify the FULL staged set, not just the paths this call chose
		// to exclude: `git add -u` only re-stages modifications to files
		// already tracked at HEAD — it never touches a pre-existing "A"
		// (added) index entry for a path that was untracked before this
		// call, e.g. one the caller separately ran `git add <newfile>` on.
		// Left alone, that survives add -u's allowlist untouched and enters
		// an unattended, potentially force-pushed commit — exactly what the
		// allowlist exists to prevent (gt-i4ej FIX 1 round-2 finding).
		// Unconditional (not just when resetErrs is non-empty), since this
		// failure mode has nothing to do with a reset failing.
		if staged, sErr := g.StagedFiles(); sErr != nil {
			resetErrs = append(resetErrs, fmt.Errorf("listing staged files: %w", sErr))
		} else {
			for _, f := range staged {
				if pathExcluded(f, excludePaths) || g.FileTrackedAtHEAD(f) {
					continue
				}
				if err := g.ResetFiles(f); err != nil {
					resetErrs = append(resetErrs, fmt.Errorf("unstaging untracked %s: %w", f, err))
					continue
				}
				excludePaths = append(excludePaths, f)
			}
		}

		if len(resetErrs) > 0 {
			// A reset (or a query it depended on) failed, so an exclusion
			// may not have taken effect. Don't trust it silently —
			// re-verify against the actual index and refuse to commit (let
			// alone push) if anything that must be excluded, or that isn't
			// tracked at HEAD, is still staged (PR #184 review).
			staged, sErr := g.StagedFiles()
			if sErr != nil {
				return nil, fmt.Errorf("exclusion reset failed (%v) and could not re-verify the index: %w", resetErrs, sErr)
			}
			for _, f := range staged {
				if pathExcluded(f, excludePaths) || g.FileTrackedAtHEAD(f) {
					continue
				}
				return nil, fmt.Errorf("refusing to auto-preserve: exclusion reset failed and %q is still staged: %v", f, resetErrs)
			}
		}

		// `git add -u` legitimately stages nothing when the only
		// uncommitted work is a new untracked file (the accepted
		// tradeoff above) or when everything staged was then unstaged as
		// a runtime artifact. `git commit` errors on an empty index, so
		// check first rather than treating that as a failure.
		hasStaged, hsErr := g.HasStagedChanges()
		if hsErr != nil {
			return nil, fmt.Errorf("checking staged changes: %w", hsErr)
		}
		if hasStaged {
			msg := opts.CommitMessage
			if msg == "" {
				msg = "fix: auto-save uncommitted implementation work (gt-pvx safety net)"
				if opts.IssueID != "" {
					msg = fmt.Sprintf("fix: auto-save uncommitted implementation work (%s, gt-pvx safety net)", opts.IssueID)
				}
			}

			// Hooks gate the PUSH, not the commit (gt-i4ej FIX 2). Try a
			// verified commit first — pre-commit hooks are frequently the
			// secret scanner that FIX 1 leans on as a second line of
			// defense. If hooks fail, the work must still not be lost, so
			// fall back to an unverified local commit — but mark it so the
			// push step below refuses to publish it, and the caller can
			// escalate loudly instead.
			if err := g.Commit(msg); err != nil {
				// Trailer makes the unverified state a durable property of
				// the commit itself, not just this call's in-memory result
				// (see unverifiedCommitTrailer doc) — the push gate below
				// checks for it on every call, including ones that made no
				// commit at all.
				if nvErr := g.CommitNoVerify(msg + "\n\n" + unverifiedCommitTrailer); nvErr != nil {
					return nil, fmt.Errorf("auto-committing: %w", nvErr)
				}
				result.Committed = true
				result.HooksFailed = true
				result.HookOutput = err.Error()
			} else {
				result.Committed = true
			}
		}
	}

	if !opts.Push {
		return result, nil
	}

	if result.HooksFailed {
		// The commit was made without hook verification, so it must not
		// reach origin unattended: nothing unverified may be published.
		// The work is safe (committed locally); the caller is responsible
		// for escalating HookOutput loudly so a human resolves the hook
		// failure (gt-i4ej FIX 2).
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

	// Re-derive HooksFailed as a property of the commit range about to be
	// pushed, not of this call's in-memory result (see unverifiedCommitTrailer
	// doc): a prior cycle may have made an unverified commit and returned
	// before pushing it, and THIS cycle's worktree can be perfectly clean
	// with nothing to commit, so the `if result.HooksFailed` check above
	// would miss it entirely and push HEAD anyway (gt-i4ej FIX 2 round-2
	// finding).
	if badSHA, chkErr := hasUnverifiedCommit(g, remote, head); chkErr != nil {
		return result, fmt.Errorf("checking for a prior unverified commit before push: %w", chkErr)
	} else if badSHA != "" {
		result.HooksFailed = true
		result.HookOutput = fmt.Sprintf("commit %s bypassed pre-commit hooks (%s) and was never verified — refusing to push until resolved", shortSHA(badSHA), unverifiedCommitTrailer)
		return result, nil
	}

	// A detached HEAD reports as literal "HEAD" from `git rev-parse
	// --abbrev-ref HEAD`, not "". It is the normal idle state for a
	// polecat worktree, not an exotic one, so collapsing every detached
	// worktree onto the same "polecat/preserve-HEAD" ref is a real
	// collision risk, not a theoretical one. Derive a ref unique to this
	// polecat instead (gt-i4ej FIX 3).
	refBranch := branch
	if branch == "HEAD" {
		identity, idErr := detachedPreservationIdentity(g, opts.IssueID)
		if idErr != nil {
			return result, fmt.Errorf("cannot derive a unique preservation ref for detached HEAD: %w", idErr)
		}
		// Identity alone (no commit SHA) so repeated checkpoints of the
		// same polecat force-push over their own prior state instead of
		// minting a brand-new permanent remote branch every cycle — the
		// ref is already force-pushed, so a stable name is correct and
		// nothing here relies on the old name being distinct per commit
		// (PR #184 review).
		refBranch = "detached-" + identity
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
		if tip, err := g.PushRemoteBranchTip(remote, PreservationRefName(refBranch)); err == nil && tip == head {
			return result, nil
		}
		if status.UnpushedCommits == 0 {
			return result, nil
		}
	}

	refName := PreservationRefName(refBranch)
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

// hasUnverifiedCommit reports the SHA of the nearest commit, reachable from
// head back to its merge-base with remote's default branch, that carries
// unverifiedCommitTrailer — i.e. was committed with hooks bypassed and has
// never been confirmed safe to publish. Scoped to the merge-base range (this
// branch's own commits since it diverged) rather than all of history, so an
// unrelated marked commit merged in from elsewhere can't false-positive
// every future push. Falls back to head's full ancestry if the merge-base
// can't be resolved (e.g. the remote branch isn't fetched locally) — a
// wider search is the safe direction here, not a skipped one.
func hasUnverifiedCommit(g *Git, remote, head string) (string, error) {
	revRange := head
	if base, err := g.MergeBase(head, remote+"/"+g.RemoteDefaultBranch()); err == nil && base != "" {
		revRange = base + ".." + head
	}
	out, err := g.run("log", revRange, "--fixed-strings", "--grep="+unverifiedCommitTrailer, "--format=%H")
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	return strings.Fields(out)[0], nil
}

// pathExcluded reports whether staged path f falls under one of the
// exclude paths. exclude entries ending in "/" (directory roots from
// RuntimeArtifactPaths) match by prefix; everything else matches exactly.
func pathExcluded(f string, exclude []string) bool {
	for _, ex := range exclude {
		if strings.HasSuffix(ex, "/") {
			if strings.HasPrefix(f, ex) {
				return true
			}
			continue
		}
		if f == ex {
			return true
		}
	}
	return false
}
