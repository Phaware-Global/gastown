package refinery

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// This file is where the refinery's branch-update policy is enforced in code.
// The patrol formula drives it through `gt refinery pr sync-branch` and
// `gt refinery pr push-branch`; those commands are the only sanctioned way for
// the refinery to bring a work branch up to date and publish it, so a rig's
// pr_update_strategy and the approved-PR force-push guard hold regardless of
// what the formula prose says.

// ErrForcePushRefused is returned when a force-push would rewrite a branch
// that has an open PR with an approving review, or when that cannot be ruled
// out.
var ErrForcePushRefused = errors.New("force-push refused")

// PRApprovalProbe is the slice of PRProvider the force-push guard needs.
type PRApprovalProbe interface {
	FindPRNumber(branch string) (int, error)
	IsPRApproved(prNumber int) (bool, error)
	CountApprovals(prNumber int) (int, error)
}

// SyncOutcome is the result of bringing a branch up to date with its target.
type SyncOutcome string

const (
	// SyncUpToDate: the branch already contains its target; nothing to do.
	SyncUpToDate SyncOutcome = "up-to-date"
	// SyncSynced: the target was merged or rebased in; the result is on the
	// local working branch, not yet pushed.
	SyncSynced SyncOutcome = "synced"
	// SyncBehindBase: the branch is behind and the strategy is "none"; nothing
	// was changed and the caller should report it.
	SyncBehindBase SyncOutcome = "behind-base"
	// SyncConflict: the merge/rebase conflicted and was aborted; nothing was
	// changed.
	SyncConflict SyncOutcome = "conflict"
)

// SyncResult describes what SyncBranch did.
type SyncResult struct {
	Outcome   SyncOutcome
	Strategy  string
	Behind    int // commits on the target that the branch lacked
	BranchSHA string
	TargetSHA string
}

// PushResult describes what PushBranch did.
type PushResult struct {
	Pushed bool
	Forced bool
	SHA    string
}

// CheckForcePushAllowed refuses a force-push of branch when it has an open PR
// with an approving review. Fail-closed: if the PR or its approvals cannot be
// determined, the force-push is refused too — a rewritten head invalidates the
// approval the human gave, and "I could not check" is not a reason to proceed.
func CheckForcePushAllowed(p PRApprovalProbe, branch string) error {
	pr, err := p.FindPRNumber(branch)
	if err != nil {
		return fmt.Errorf("%w: cannot determine whether %s has an open PR: %v", ErrForcePushRefused, branch, err)
	}
	if pr == 0 {
		return nil
	}
	n, err := p.CountApprovals(pr)
	if err != nil {
		return fmt.Errorf("%w: cannot read approvals on PR #%d: %v", ErrForcePushRefused, pr, err)
	}
	if n > 0 {
		return fmt.Errorf("%w: PR #%d for %s has %d approving review(s); rewriting its head would invalidate them — merge the base into the branch instead",
			ErrForcePushRefused, pr, branch, n)
	}
	approved, err := p.IsPRApproved(pr)
	if err != nil {
		return fmt.Errorf("%w: cannot read review decision on PR #%d: %v", ErrForcePushRefused, pr, err)
	}
	if approved {
		return fmt.Errorf("%w: PR #%d for %s is approved; rewriting its head would invalidate the approval — merge the base into the branch instead",
			ErrForcePushRefused, pr, branch)
	}
	return nil
}

// SyncBranch brings origin/<branch> up to date with origin/<target> according
// to strategy, leaving the result on the local branch `local`. It never pushes.
//
//   - none:   reports behind-base and changes nothing.
//   - merge:  merges the target into the branch with a merge commit.
//   - rebase: rebases the branch onto the target. Refused up front when the
//     branch has an approved open PR, because the result could only be
//     published by force-pushing.
//
// A conflict aborts the merge/rebase and is reported as SyncConflict.
//
// The synced-tip record for `local` is cleared before anything else, so a sync
// that fails part-way leaves no record for PushBranch to mistake for proof of
// an earlier sync of a different branch.
func SyncBranch(g *git.Git, p PRApprovalProbe, strategy, branch, target, local string) (SyncResult, error) {
	if err := g.ConfigUnset(syncedFromKey(local)); err != nil {
		return SyncResult{}, fmt.Errorf("clearing the synced tip of %s: %w", local, err)
	}
	strategy, err := config.NormalizePRUpdateStrategy(strategy)
	if err != nil {
		return SyncResult{}, err
	}
	if err := g.Fetch("origin"); err != nil {
		return SyncResult{}, fmt.Errorf("fetching origin: %w", err)
	}
	branchRef, targetRef := "origin/"+branch, "origin/"+target
	res := SyncResult{Strategy: strategy}
	if res.BranchSHA, err = g.Rev(branchRef); err != nil {
		return res, fmt.Errorf("resolving %s: %w", branchRef, err)
	}
	if res.TargetSHA, err = g.Rev(targetRef); err != nil {
		return res, fmt.Errorf("resolving %s: %w", targetRef, err)
	}
	if res.Behind, err = g.CountCommitsBetween(branchRef, targetRef); err != nil {
		return res, fmt.Errorf("counting commits behind %s: %w", targetRef, err)
	}

	switch {
	case res.Behind == 0:
		res.Outcome = SyncUpToDate
		return res, resetToSyncedTip(g, local, branch, res.BranchSHA)
	case strategy == config.PRUpdateStrategyNone:
		res.Outcome = SyncBehindBase
		return res, nil
	case strategy == config.PRUpdateStrategyRebase:
		if err := CheckForcePushAllowed(p, branch); err != nil {
			return res, err
		}
	}

	if err := resetToSyncedTip(g, local, branch, res.BranchSHA); err != nil {
		return res, fmt.Errorf("checking out %s: %w", branchRef, err)
	}
	var opErr error
	var abort func() error
	if strategy == config.PRUpdateStrategyMerge {
		opErr = g.MergeNoFF(targetRef, fmt.Sprintf("Merge %s into %s", target, branch))
		abort = g.AbortMerge
	} else {
		opErr = g.Rebase(targetRef)
		abort = g.AbortRebase
	}
	if opErr == nil {
		res.Outcome = SyncSynced
		return res, nil
	}
	conflicts, cErr := g.GetConflictingFiles()
	if cErr != nil || len(conflicts) == 0 {
		_ = abort()
		return res, fmt.Errorf("%s of %s onto %s failed: %w", strategy, branch, targetRef, opErr)
	}
	if err := abort(); err != nil {
		return res, fmt.Errorf("aborting conflicted %s: %w", strategy, err)
	}
	res.Outcome = SyncConflict
	return res, nil
}

// syncedFromKey is the git config key under which SyncBranch records the tip of
// origin/<branch> that `local` was built from, as "<branch> <sha>". The branch
// is part of the record because `local` is reused across MRs: without it, the
// record left by one branch's sync would authorize publishing to another.
func syncedFromKey(local string) string {
	return "branch." + local + ".gtSyncedFrom"
}

// resetToSyncedTip points `local` at sha, the tip SyncBranch resolved, and
// records it for PushBranch. It checks out the SHA rather than origin/<branch>
// so a fetch landing in between (the refinery shares refs with every polecat
// through .repo.git) cannot make the recorded tip differ from what was built on.
func resetToSyncedTip(g *git.Git, local, branch, sha string) error {
	if err := g.CheckoutResetBranch(local, sha); err != nil {
		return err
	}
	return g.ConfigSet(syncedFromKey(local), branch+" "+sha)
}

// readSyncedTip returns the branch and tip recorded by the last successful
// SyncBranch into `local`. Both are empty when there is no record.
func readSyncedTip(g *git.Git, local string) (branch, sha string, err error) {
	v, err := g.ConfigGet(syncedFromKey(local))
	if err != nil || v == "" {
		return "", "", err
	}
	branch, sha, ok := strings.Cut(v, " ")
	if !ok {
		return "", "", fmt.Errorf("malformed synced-tip record %q", v)
	}
	return branch, sha, nil
}

// PushBranch publishes the local branch `local` to origin/<branch> as left by
// SyncBranch. The push is a plain fast-forward push whenever possible. Only the
// rebase strategy may force-push, and then only with a lease on the tip that
// SyncBranch recorded (not the live origin/<branch> ref, which a fetch can move)
// and only when CheckForcePushAllowed passes. Under "none" nothing is ever
// pushed, and under "merge" a non-fast-forward is an error, never a force. It
// refuses when no sync was recorded for `local`, or when the recorded sync was
// of a different branch.
func PushBranch(g *git.Git, p PRApprovalProbe, strategy, branch, local string) (PushResult, error) {
	strategy, err := config.NormalizePRUpdateStrategy(strategy)
	if err != nil {
		return PushResult{}, err
	}
	localSHA, err := g.Rev(local)
	if err != nil {
		return PushResult{}, fmt.Errorf("resolving %s: %w", local, err)
	}
	res := PushResult{SHA: localSHA}
	dst := "refs/heads/" + branch

	syncedBranch, syncedFrom, err := readSyncedTip(g, local)
	if err != nil {
		return res, fmt.Errorf("reading the synced tip of %s: %w", local, err)
	}
	if syncedFrom == "" {
		return res, fmt.Errorf("no sync recorded for %s: run sync-branch first", local)
	}
	if syncedBranch != branch {
		return res, fmt.Errorf("%s was synced from %s, not %s: run sync-branch for %s first",
			local, syncedBranch, branch, branch)
	}
	if syncedFrom == localSHA {
		return res, nil
	}
	if strategy == config.PRUpdateStrategyNone {
		return res, fmt.Errorf("pr_update_strategy=none: refusing to update %s (local %s differs from synced %s)",
			branch, short(localSHA), short(syncedFrom))
	}

	fastForward, err := g.IsAncestor(syncedFrom, localSHA)
	if err != nil {
		return res, fmt.Errorf("comparing %s with synced tip: %w", local, err)
	}
	if fastForward {
		if err := g.Push("origin", local+":"+dst, false); err != nil {
			return res, err
		}
		res.Pushed = true
		return res, nil
	}

	if strategy != config.PRUpdateStrategyRebase {
		return res, fmt.Errorf("pr_update_strategy=%s: %s is not a fast-forward of the tip it was synced from (origin/%s); refusing to force-push — re-run sync-branch",
			strategy, local, branch)
	}
	if err := CheckForcePushAllowed(p, branch); err != nil {
		return res, err
	}
	if err := g.PushForceWithLease("origin", local, branch, syncedFrom); err != nil {
		return res, err
	}
	res.Pushed, res.Forced = true, true
	return res, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// ConflictResolutionInstructions returns the numbered steps for a
// conflict-resolution task, matching the rig's update strategy. Only the
// rebase strategy ever tells anyone to force-push, and even then it is gated
// on the branch not having an approved PR.
func ConflictResolutionInstructions(strategy, branch, target string) string {
	strategy, err := config.NormalizePRUpdateStrategy(strategy)
	if err != nil || strategy != config.PRUpdateStrategyRebase {
		return fmt.Sprintf(`1. Check out the branch: git checkout %[1]s
2. Merge the target in: git merge origin/%[2]s
3. Resolve conflicts in your editor, then commit the merge
4. Push normally: git push origin %[1]s  (no rebase, no force-push)
5. Close this task: bd close <this-task-id>

The Refinery will automatically retry the merge after you push.`, branch, target)
	}
	return fmt.Sprintf(`1. Check out the branch: git checkout %[1]s
2. If the branch has an open PR with an approving review, do NOT rebase: merge
   the target in instead (git merge origin/%[2]s, resolve, commit,
   git push origin %[1]s) and skip to step 6
3. Rebase onto target: git rebase origin/%[2]s
4. Resolve conflicts in your editor, then complete the rebase: git add . && git rebase --continue
5. Push with a lease: git push --force-with-lease origin %[1]s
6. Close this task: bd close <this-task-id>

The Refinery will automatically retry the merge after you push.`, branch, target)
}
