package git

import (
	"fmt"
	"os"
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

	// ProtectedBranches lists the rig's protected integration branches
	// (e.g. its configured default branch, "develop") on Remote. A commit
	// reachable from any of them is exempt from the unverified-commit scan
	// — reaching a protected branch only happens through a reviewed PR
	// under branch protection, which is what makes it safe to treat as
	// "accepted."
	//
	// MUST be sourced from rig config (settings/config.json /
	// config.json's default_branch) by the caller — NEVER derived from a
	// bead, the branch name, or --issue. Those are agent-writable, and
	// gt-xitk found that letting any of them influence this list is
	// exactly how three earlier formulations of this guard were each
	// defeated (either a false-positive on a develop-based rig, or a
	// self-exemption once the checked branch was itself pushed). Empty
	// means the scan fails closed rather than guessing a baseline.
	ProtectedBranches []string

	// Ephemeral, when true, snapshots the uncommitted TRACKED changes with
	// `git stash create` (which builds a commit without touching HEAD, the
	// index or the working tree, and runs no hooks) and records it under a
	// LOCAL ref, LocalPreservationRefName(branch) = refs/gt/preserve/<branch>.
	//
	// Guarantees: nothing is ever pushed (Push, Remote and ProtectedBranches are
	// ignored); the ref lives in the shared common git dir so it survives
	// `git worktree remove`; HEAD, the branch ref, the index, the working tree
	// and sequencer state are untouched; a snapshot is taken even while a
	// merge, cherry-pick, revert or rebase is in progress (it reads no
	// sequencer state), but it refuses when any path is unmerged; an unchanged
	// dirty tree is not re-recorded each cycle; a detached HEAD keeps one ref
	// across cycles, and a clean detached HEAD whose commits no branch or
	// remote-tracking ref reaches is pinned to that ref so they outlive the
	// worktree.
	//
	// Does not: capture untracked files, or preserve the agent's own committed
	// work on a branch (that lives on its branch ref). ExtraExcludePaths is ignored — the
	// tracked-file allowlist existed to keep unattended PUSHES safe, and nothing
	// leaves the machine now.
	//
	// Root fix for gt-94p1/gt-2stz: a periodic safety-net commit made ON the
	// branch (checkpoint_dog, pre-removal preserve) rode along with any later
	// ordinary push of that branch onto a real, possibly already-reviewed PR
	// branch (graphql-api #162/#166, gastown #250/#251). Committed reports that
	// a snapshot was recorded, not that the branch advanced; Pushed is always
	// false; Ref is the local ref.
	//
	// Leave false for a caller where the commit IS the polecat's real work and
	// a normal push of the branch follows in the same flow (gt done).
	Ephemeral bool
}

// PreserveResult reports what AutoPreserveUncommittedWork actually did.
type PreserveResult struct {
	// Committed is true if a new auto-save commit was created. Under
	// Ephemeral that commit is a snapshot recorded on the preservation ref
	// (or a local refs/preserve ref), never on the branch.
	Committed bool
	// Pushed is true if the commit was pushed to the preservation ref and the
	// push was verified against the remote tip.
	Pushed bool
	// Commit is the SHA that was preserved (HEAD, or the Ephemeral snapshot),
	// set whenever Committed or Pushed is true.
	Commit string
	// Ref is the preservation ref: the remote ref HEAD was pushed to when
	// Pushed, or the LOCAL ref (refs/gt/preserve/...) an Ephemeral snapshot was
	// recorded under.
	Ref string

	// HooksFailed is true when the auto-save commit could only be made by
	// bypassing commit hooks (e.g. a secret scanner) because they failed —
	// whether by THIS call or by a prior one whose unverified commit is
	// still in HEAD's ancestry (the durable trailer check runs for every
	// caller, including Push:false ones, since those callers push the
	// branch themselves — PR #184 review). The work is still Committed —
	// never lost — but callers MUST NOT push it: an unverified commit
	// reaching origin is exactly the risk the hooks exist to catch
	// (gt-i4ej FIX 2). Never pushed, regardless of opts.Push. Callers
	// should escalate loudly, surfacing HookOutput.
	HooksFailed bool
	// HookOutput carries a redacted, caller-facing description of the hook
	// failure when HooksFailed. Deliberately NOT the hook's raw stderr:
	// the hook is typically a secret scanner whose output can contain the
	// matched credential, and callers print this into terminals, agent
	// transcripts, and logs (PR #184 review). It points at the worktree
	// where the output can be reproduced instead.
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

// LocalPreservationRefName returns the LOCAL ref an Ephemeral snapshot of
// branch is recorded under: refs/gt/preserve/<sanitized branch>. Refs outside
// refs/worktree/ live in the repository's common git dir, shared by every
// worktree, so the snapshot survives `git worktree remove` of the worktree it
// was taken in. Never pushed anywhere (gt-94p1).
func LocalPreservationRefName(branch string) string {
	sanitized := strings.NewReplacer("/", "-", " ", "-").Replace(branch)
	return "refs/gt/preserve/" + sanitized
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

	if opts.Ephemeral {
		return autoPreserveEphemeral(g, branch, opts)
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
		// (PR #184 review). The re-verification below re-queries deletions
		// EXPLICITLY: a deleted file is by definition tracked at HEAD, so
		// no generic tracked-at-HEAD test can ever flag it (round-4
		// finding).
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
			// may not have taken effect. Don't trust it silently — re-verify
			// against the actual index, and REFUSE to commit (let alone
			// push) unless every staged path is provably legitimate. The
			// per-class semantics (PR #184 review — a round-4 rewrite
			// inverted these into "excluded ⇒ allowed" and published the
			// exclude list; this is the round-2 refusal contract, restored
			// and made explicit):
			//   - excluded path still staged        ⇒ REFUSE (must never publish)
			//   - staged deletion of a tracked file ⇒ REFUSE (auto-preserve
			//     never records deletions; checked explicitly, because a
			//     deleted file is by definition tracked at HEAD and no
			//     tracked-at-HEAD test can ever catch it)
			//   - staged path not a blob at HEAD    ⇒ REFUSE (a new file the
			//     scrub above failed to unstage)
			//   - tracked modification, not excluded ⇒ allowed (that IS the
			//     work being preserved)
			// Any re-verification query that itself fails aborts: committing
			// an index we could not inspect is exactly the fail-open this
			// gate exists to prevent.
			staged, sErr := g.StagedFiles()
			if sErr != nil {
				return nil, fmt.Errorf("exclusion reset failed (%v) and could not re-verify the index: %w", resetErrs, sErr)
			}
			stillDeleted, delErr := g.StagedDeletions()
			if delErr != nil {
				return nil, fmt.Errorf("exclusion reset failed (%v) and could not re-verify staged deletions: %w", resetErrs, delErr)
			}
			if len(stillDeleted) > 0 {
				return nil, fmt.Errorf("refusing to auto-preserve: exclusion reset failed and the deletion of %q is still staged: %v", stillDeleted[0], resetErrs)
			}
			for _, f := range staged {
				if pathExcluded(f, excludePaths) {
					return nil, fmt.Errorf("refusing to auto-preserve: exclusion reset failed and %q is still staged: %v", f, resetErrs)
				}
				if !g.FileTrackedAtHEAD(f) {
					return nil, fmt.Errorf("refusing to auto-preserve: %q is staged but not tracked at HEAD after a failed exclusion reset: %v", f, resetErrs)
				}
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
				// Deliberately NOT err.Error(): git relays the pre-commit
				// hook's stderr into its own, and the hook is typically a
				// secret scanner whose output can contain the very
				// credential it matched. Callers print HookOutput into
				// operator terminals, agent transcripts, and logs, so point
				// at the worktree instead of relocating the secret
				// (PR #184 review).
				result.HookOutput = fmt.Sprintf("pre-commit hook failed — output withheld, it can contain the secret a scanner matched; run `git commit` in %s to see it", g.WorkDir())
			} else {
				result.Committed = true
			}
		}
	}

	remote := opts.Remote
	if remote == "" {
		remote = "origin"
	}

	head, revErr := g.Rev("HEAD")
	if revErr != nil {
		return result, fmt.Errorf("resolving HEAD: %w", revErr)
	}

	// Re-derive HooksFailed as a property of the commit range, not of this
	// call's in-memory result (see unverifiedCommitTrailer doc): a prior
	// cycle may have made an unverified commit and returned before pushing
	// it, and THIS cycle's worktree can be perfectly clean with nothing to
	// commit (gt-i4ej FIX 2 round-2 finding). Runs for EVERY caller —
	// including Push:false ones — because gt done and polecat removal push
	// the branch to origin themselves right after this returns, gating that
	// push only on result.HooksFailed; a gate that only ran on the
	// Push:true path let a prior cycle's unverified commit straight through
	// to the shared remote (PR #184 review). Skipped when this very call
	// already set HooksFailed: its own commit carries the trailer.
	if err := markPriorUnverifiedCommit(g, remote, head, opts.ProtectedBranches, result); err != nil {
		return result, err
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

	refName, refErr := preservationRefFor(g, branch, remote, head, opts.IssueID)
	if refErr != nil {
		return result, refErr
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
		if tip, err := g.PushRemoteBranchTip(remote, refName); err == nil && tip == head {
			return result, nil
		}
		if status.UnpushedCommits == 0 {
			return result, nil
		}
	}

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

// markPriorUnverifiedCommit sets result.HooksFailed/HookOutput when head's
// ancestry holds a commit that bypassed pre-commit hooks — see
// unverifiedCommitTrailer. A no-op when result.HooksFailed is already set.
func markPriorUnverifiedCommit(g *Git, remote, head string, protectedBranches []string, result *PreserveResult) error {
	if result.HooksFailed {
		return nil
	}
	badSHA, err := hasUnverifiedCommit(g, remote, head, protectedBranches)
	if err != nil {
		return fmt.Errorf("checking for a prior unverified commit: %w", err)
	}
	if badSHA != "" {
		result.HooksFailed = true
		result.HookOutput = fmt.Sprintf("commit %s bypassed pre-commit hooks (%s) and was never verified — refusing to publish until resolved. To clear it: verify the work, then rewrite the marked commit so hooks re-run and the trailer is dropped (e.g. `git commit --amend`, or `git rebase -i` for older commits)", shortSHA(badSHA), unverifiedCommitTrailer)
	}
	return nil
}

// preservationRefFor returns the preservation ref name for branch.
// anchorHead is the commit the detached-HEAD ref is anchored on: the current
// HEAD for the commit-on-branch path, and the PRE-snapshot HEAD for an
// Ephemeral call (which never advances HEAD, so the anchor — and therefore
// the ref — stays stable across cycles).
func preservationRefBranch(g *Git, branch, remote, anchorHead, issueID string) (string, error) {
	// A detached HEAD reports as literal "HEAD" from `git rev-parse
	// --abbrev-ref HEAD`, not "". It is the normal idle state for a
	// polecat worktree, not an exotic one, so collapsing every detached
	// worktree onto the same "polecat/preserve-HEAD" ref is a real
	// collision risk, not a theoretical one. Derive a ref unique to this
	// polecat instead (gt-i4ej FIX 3).
	refBranch := branch
	if branch == "HEAD" {
		identity, idErr := detachedPreservationIdentity(g, issueID)
		if idErr != nil {
			return "", fmt.Errorf("cannot derive a unique preservation ref for detached HEAD: %w", idErr)
		}
		// Identity + the first commit this worktree added past the remote
		// default branch. Stable across repeated checkpoints of one
		// assignment (later checkpoints stack on top of the same first
		// commit), so the ref force-pushes over its own prior state instead
		// of minting a brand-new permanent remote branch every cycle
		// (PR #184 review, round 3). But polecat names are a recycled pool:
		// identity alone made the ref a function of the NAME, so the next
		// bead assigned to it force-pushed over the previous bead's only
		// preserved copy — the divergence anchor changes across
		// assignments, so each assignment gets its own ref (PR #184
		// review, round 4).
		refBranch = "detached-" + identity + "-" + shortSHA(divergenceAnchor(g, remote, anchorHead))
	}
	return refBranch, nil
}

// preservationRefFor returns the remote preservation ref name for branch.
func preservationRefFor(g *Git, branch, remote, anchorHead, issueID string) (string, error) {
	refBranch, err := preservationRefBranch(g, branch, remote, anchorHead, issueID)
	if err != nil {
		return "", err
	}
	return PreservationRefName(refBranch), nil
}

// snapshotGitEnv is the environment for git reads of the REAL repo state made
// by an Ephemeral preserve. GIT_OPTIONAL_LOCKS=0 stops git from opportunistically
// refreshing (rewriting) the real index as a side effect of a read, so the
// snapshot leaves .git/index byte-identical.
var snapshotGitEnv = []string{"GIT_OPTIONAL_LOCKS=0"}

// autoPreserveEphemeral is the Ephemeral implementation of
// AutoPreserveUncommittedWork (see PreserveOptions.Ephemeral): `git stash
// create` builds a commit of the tracked changes without touching HEAD, the
// index or the working tree and runs no hooks, and update-ref records it under
// a LOCAL ref. Nothing is ever pushed.
func autoPreserveEphemeral(g *Git, branch string, opts PreserveOptions) (*PreserveResult, error) {
	result := &PreserveResult{}
	remote := opts.Remote
	if remote == "" {
		remote = "origin"
	}

	head, err := g.Rev("HEAD")
	if err != nil {
		return result, fmt.Errorf("ephemeral preserve requires a resolvable HEAD: %w", err)
	}

	// An unresolved conflict would put conflict markers into the snapshot.
	unmerged, err := g.runWithEnv([]string{"ls-files", "-u", "-z"}, snapshotGitEnv)
	if err != nil {
		return result, fmt.Errorf("checking for unmerged paths: %w", err)
	}
	if unmerged != "" {
		var paths []string
		for _, rec := range strings.Split(unmerged, "\x00") {
			if _, p, ok := strings.Cut(rec, "\t"); ok {
				paths = append(paths, p)
			}
		}
		return result, fmt.Errorf("cannot auto-preserve unmerged conflicts: %s", strings.Join(paths, ", "))
	}

	msg := opts.CommitMessage
	if msg == "" {
		msg = "gt preserve snapshot of uncommitted work"
	}
	// stash create refreshes and rewrites the index it is given, so give it a
	// COPY: the agent's real index (and its index.lock) is never touched, and a
	// concurrent `git add` by the live agent cannot collide with the daemon.
	env, cleanup, err := scratchIndexEnv(g)
	if err != nil {
		return result, err
	}
	defer cleanup()
	sha, err := g.runWithEnv([]string{"stash", "create", msg}, env)
	if err != nil {
		return result, fmt.Errorf("creating snapshot: %w", err)
	}
	// Anchored on head, not the snapshot: head never moves under Ephemeral, so
	// a detached worktree keeps one ref across cycles until the agent makes its
	// own first commit.
	refBranch, err := preservationRefBranch(g, branch, remote, head, opts.IssueID)
	if err != nil {
		return result, err
	}
	ref := LocalPreservationRefName(refBranch)

	if sha == "" {
		// Clean tree. A detached HEAD's own commits may be reachable from no
		// branch at all; only the worktree's HEAD holds them, and removing the
		// worktree would orphan them. Pin HEAD so they outlive it.
		if branch == "HEAD" {
			return pinUnreachableHead(g, head, ref, result)
		}
		return result, nil // no tracked changes
	}

	// The dirty tree hasn't changed since the last cycle: the snapshot is
	// stamped with the wall clock, so recording it again would just churn.
	if cur, curErr := g.runWithEnv([]string{"rev-parse", "--verify", "-q", ref}, snapshotGitEnv); curErr == nil && cur != "" && snapshotMatches(g, cur, sha) {
		return result, nil
	}

	if _, err := g.run("update-ref", ref, sha); err != nil {
		return result, fmt.Errorf("recording snapshot %s: %w", shortSHA(sha), err)
	}
	result.Committed = true
	result.Commit = sha
	result.Ref = ref
	return result, nil
}

// pinUnreachableHead records head under ref when no branch or remote-tracking
// ref reaches it and ref does not already hold it.
func pinUnreachableHead(g *Git, head, ref string, result *PreserveResult) (*PreserveResult, error) {
	out, err := g.runWithEnv([]string{"for-each-ref", "--contains", head, "--count=1", "--format=%(refname)", "refs/heads/", "refs/remotes/"}, snapshotGitEnv)
	if err != nil {
		return result, fmt.Errorf("checking whether HEAD is reachable from a branch: %w", err)
	}
	if out != "" {
		return result, nil
	}
	if cur, curErr := g.runWithEnv([]string{"rev-parse", "--verify", "-q", ref}, snapshotGitEnv); curErr == nil && cur == head {
		return result, nil
	}
	if _, err := g.run("update-ref", ref, head); err != nil {
		return result, fmt.Errorf("recording detached HEAD %s: %w", shortSHA(head), err)
	}
	result.Committed = true
	result.Commit = head
	result.Ref = ref
	return result, nil
}

// scratchIndexEnv returns the environment for a git command that must not
// write the real index: GIT_INDEX_FILE pointing at a private copy of it.
func scratchIndexEnv(g *Git) ([]string, func(), error) {
	noop := func() {}
	p, err := g.runWithEnv([]string{"rev-parse", "--git-path", "index"}, snapshotGitEnv)
	if err != nil {
		return nil, noop, fmt.Errorf("locating index: %w", err)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(g.WorkDir(), p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, noop, fmt.Errorf("reading index: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "gt-preserve-index-")
	if err != nil {
		return nil, noop, fmt.Errorf("creating scratch index dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	idx := filepath.Join(tmpDir, "index")
	if err := os.WriteFile(idx, data, 0600); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("writing scratch index: %w", err)
	}
	return append([]string{"GIT_INDEX_FILE=" + idx}, snapshotGitEnv...), cleanup, nil
}

// snapshotMatches reports whether stash commits a and b hold the same tree on
// the same parent. Any failure reads as "no match", which just means the
// caller records the new snapshot.
func snapshotMatches(g *Git, a, b string) bool {
	for _, spec := range []string{"^{tree}", "^1"} {
		x, errX := g.runWithEnv([]string{"rev-parse", a + spec}, snapshotGitEnv)
		y, errY := g.runWithEnv([]string{"rev-parse", b + spec}, snapshotGitEnv)
		if errX != nil || errY != nil || x != y {
			return false
		}
	}
	return true
}

// divergenceAnchor returns the first commit reachable from head that is not
// on remote's default branch — the commit that started this worktree
// assignment's local history. Stable for the life of one assignment,
// different for the next one (a reassigned worktree starts diverging from a
// fresh base), which is exactly the property the detached preservation ref
// needs (PR #184 review). Falls back to head itself when the divergence
// can't be resolved (e.g. the remote default branch isn't fetched) — a
// per-commit ref in a degenerate setup beats a name collision that
// force-pushes over another bead's preserved work.
func divergenceAnchor(g *Git, remote, head string) string {
	base, err := g.MergeBase(head, remote+"/"+g.RemoteDefaultBranch())
	if err != nil || base == "" {
		return head
	}
	out, err := g.run("rev-list", "--reverse", base+".."+head)
	if err != nil {
		return head
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return head
	}
	return fields[0]
}

// HasUnverifiedCommit reports the SHA of a commit in HEAD's ancestry, not
// reachable from any of protectedBranches on remote, that was committed with
// hooks bypassed and never verified — see hasUnverifiedCommit. Exported for
// callers that push a branch to origin themselves rather than through
// AutoPreserveUncommittedWork's own push path (gt done, polecat removal's
// best-effort branch push), so they can refuse to publish it even when no
// preserve call happened in the same invocation (PR #184 review).
//
// protectedBranches must come from rig config — see PreserveOptions.ProtectedBranches.
func HasUnverifiedCommit(g *Git, remote string, protectedBranches []string) (string, error) {
	head, err := g.Rev("HEAD")
	if err != nil {
		return "", err
	}
	return hasUnverifiedCommit(g, remote, head, protectedBranches)
}

// hasUnverifiedCommit reports the SHA of the nearest commit, reachable from
// head but not from any of protectedBranches on remote, that carries
// unverifiedCommitTrailer — i.e. was committed with hooks bypassed and has
// never been confirmed safe to publish.
//
// MAYOR DESIGN RULING (gt-xitk, 2026-09-27): the exempt set is commits
// reachable from the rig's protected integration branches ONLY — never a
// merge-base against the repo's git-level default branch (that broke on
// develop-based rigs, gt-35un) and never a base inferred from a bead,
// --issue, or the branch name (agent-writable — the base a push is scoped
// against must not be selectable by the thing being checked).
//
// Fails closed rather than guessing a baseline: an empty protectedBranches,
// or any listed branch that doesn't resolve as remote/<branch>, is an error.
// A wider or narrower search here was tried three times (see gt-xitk) and
// each direction reopened a previous hole — refusing outright is the only
// formulation that can't be quietly disarmed.
func hasUnverifiedCommit(g *Git, remote, head string, protectedBranches []string) (string, error) {
	if len(protectedBranches) == 0 {
		return "", fmt.Errorf("no protected branches configured for the unverified-commit guard — refusing rather than scanning without a known-safe baseline")
	}
	args := []string{"log", head}
	for _, branch := range protectedBranches {
		// Fully qualified: a bare "remote/branch" resolves refs/heads/
		// before refs/remotes/, so a local branch or tag named e.g.
		// "origin/main" in the same worktree would shadow the tracking
		// ref and exempt every unverified commit (PR #253 review).
		ref := "refs/remotes/" + remote + "/" + branch
		if _, err := g.run("rev-parse", "--verify", ref); err != nil {
			return "", fmt.Errorf("protected branch ref %s does not resolve — refusing to check for unverified commits until it can be verified: %w", ref, err)
		}
		args = append(args, "--not", ref)
	}
	args = append(args, "--fixed-strings", "--grep="+unverifiedCommitTrailer, "--format=%H")
	out, err := g.run(args...)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	return strings.Fields(out)[0], nil
}

// HasWIPCommit reports the SHA of the nearest commit reachable from tip, not
// reachable from any of protectedBranches on remote, whose subject starts with
// prefix (the checkpoint package's WIPCommitPrefix).
//
// tip is the ref the caller is about to publish, e.g. "refs/heads/<branch>" —
// NOT necessarily HEAD: a bare repo's HEAD is its default branch, and a
// worktree's HEAD can differ from the branch being pushed, so scanning HEAD
// can pass a push whose actual source carries a WIP commit. Exported so
// every push site that lands commits on a real branch — gt done, the
// pre-nuke best-effort push, the polecat-removal helper — can refuse to
// publish a checkpoint auto-save commit there (gt-94p1 requirement 3).
//
// This is defense in depth alongside AutoPreserveUncommittedWork's Ephemeral
// mode, which is the primary fix: Ephemeral stops such a commit from ever
// reaching branch in the first place. This guard catches one that got there
// anyway — a commit made before Ephemeral existed, one made with a plain
// `git commit` reusing the prefix, or any other path this repo grows later.
//
// protectedBranches must come from rig config — see
// PreserveOptions.ProtectedBranches; fails closed exactly like
// HasUnverifiedCommit.
func HasWIPCommit(g *Git, remote, tip, prefix string, protectedBranches []string) (string, error) {
	head, err := g.Rev(tip)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", tip, err)
	}
	if len(protectedBranches) == 0 {
		return "", fmt.Errorf("no protected branches configured for the WIP-commit guard — refusing rather than scanning without a known-safe baseline")
	}

	const recordSep = "\x01"
	args := []string{"log", head}
	for _, branch := range protectedBranches {
		// Fully qualified for the same reason as hasUnverifiedCommit: a
		// bare "remote/branch" resolves refs/heads/ before refs/remotes/.
		ref := "refs/remotes/" + remote + "/" + branch
		if _, err := g.run("rev-parse", "--verify", ref); err != nil {
			return "", fmt.Errorf("protected branch ref %s does not resolve — refusing to check for WIP commits until it can be verified: %w", ref, err)
		}
		args = append(args, "--not", ref)
	}
	args = append(args, "--format=%H"+recordSep+"%s")
	out, err := g.run(args...)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	for _, line := range strings.Split(out, "\n") {
		sha, subject, ok := strings.Cut(line, recordSep)
		if !ok {
			continue
		}
		if strings.HasPrefix(subject, prefix) {
			return sha, nil
		}
	}
	return "", nil
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
