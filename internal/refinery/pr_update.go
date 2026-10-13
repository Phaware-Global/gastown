package refinery

import (
	"errors"
	"fmt"

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
func SyncBranch(g *git.Git, p PRApprovalProbe, strategy, branch, target, local string) (SyncResult, error) {
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
		return res, g.CheckoutResetBranch(local, branchRef)
	case strategy == config.PRUpdateStrategyNone:
		res.Outcome = SyncBehindBase
		return res, nil
	case strategy == config.PRUpdateStrategyRebase:
		if err := CheckForcePushAllowed(p, branch); err != nil {
			return res, err
		}
	}

	if err := g.CheckoutResetBranch(local, branchRef); err != nil {
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

// PushBranch publishes the local branch `local` to origin/<branch> as left by
// SyncBranch. The push is a plain fast-forward push whenever possible. Only the
// rebase strategy may force-push, and then only with a lease on the tip that
// was fetched at sync time and only when CheckForcePushAllowed passes. Under
// "none" nothing is ever pushed, and under "merge" a non-fast-forward is an
// error, never a force.
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

	remoteSHA, err := g.Rev("origin/" + branch)
	if err != nil {
		// No remote-tracking ref: the branch is new to the remote.
		if strategy == config.PRUpdateStrategyNone {
			return res, fmt.Errorf("pr_update_strategy=none: refusing to push %s", branch)
		}
		if err := g.Push("origin", local+":"+dst, false); err != nil {
			return res, err
		}
		res.Pushed = true
		return res, nil
	}
	if remoteSHA == localSHA {
		return res, nil
	}
	if strategy == config.PRUpdateStrategyNone {
		return res, fmt.Errorf("pr_update_strategy=none: refusing to update %s (local %s differs from origin %s)",
			branch, short(localSHA), short(remoteSHA))
	}

	fastForward, err := g.IsAncestor(remoteSHA, localSHA)
	if err != nil {
		return res, fmt.Errorf("comparing %s with origin: %w", local, err)
	}
	if fastForward {
		if err := g.Push("origin", local+":"+dst, false); err != nil {
			return res, err
		}
		res.Pushed = true
		return res, nil
	}

	if strategy != config.PRUpdateStrategyRebase {
		return res, fmt.Errorf("pr_update_strategy=%s: %s is not a fast-forward of origin/%s; refusing to force-push — re-run sync-branch",
			strategy, local, branch)
	}
	if err := CheckForcePushAllowed(p, branch); err != nil {
		return res, err
	}
	if err := g.PushForceWithLease("origin", local, branch, remoteSHA); err != nil {
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
