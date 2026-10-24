package cmd

import (
	"fmt"
)

// rebaseGit is the subset of *git.Git that autoRebaseOnTarget needs. Defined as
// an interface so tests can drive the decision logic without standing up a full
// git repo for every gating case.
type rebaseGit interface {
	Rebase(onto string) error
	AbortRebase() error
	PushRemoteBranchExists(remote, branch string) (bool, error)
}

// autoRebaseOnTarget rebases the current branch onto base when the branch is
// behind the target. It is a no-op when there is nothing to rebase, when the
// polecat ran the formula's pre-verify step (rebasing again would invalidate
// the gate results that --pre-verified attests to), when a prior push
// checkpoint exists, or when the branch already exists on origin (it may back
// an open PR; the rewritten history would not fast-forward from origin, so the
// push would be rejected and force-pushing would discard reviewed SHAs). Base
// sync for such a branch is left to the refinery's merge step. That last guard
// does not apply when directPush is set: the push then targets the default
// branch, which only fast-forwards if the branch is rebased onto it.
//
// Returns:
//   - rebased: true if a rebase actually ran successfully.
//   - skipReason: non-empty when behind > 0 but the rebase was intentionally
//     skipped. Empty when behind == 0 (no rebase needed) or when rebased == true.
//   - err: rebase failure, after AbortRebase has been attempted to clean up.
//
// gh#3400.
func autoRebaseOnTarget(g rebaseGit, branch, base string, behind int, preVerified, alreadyPushed, directPush bool) (rebased bool, skipReason string, err error) {
	if behind <= 0 {
		return false, "", nil
	}
	switch {
	case preVerified:
		return false, "--pre-verified is set", nil
	case alreadyPushed:
		return false, "prior push checkpoint exists", nil
	}
	// A direct push (branch:default) needs the branch to contain the target
	// tip, and the branch on origin backs no PR, so the rebase is both needed
	// and safe there. Otherwise ask the target the push will use (pushurl or
	// fork), not the fetch url. If it can't be queried, skip the rebase: not
	// rebasing is always safe, rewriting a published branch is not.
	if !directPush {
		onOrigin, originErr := g.PushRemoteBranchExists("origin", branch)
		switch {
		case originErr != nil:
			return false, fmt.Sprintf("could not check origin: %v", originErr), nil
		case onOrigin:
			return false, "branch already exists on origin", nil
		}
	}

	fmt.Printf("→ Auto-rebasing onto %s (%d commits behind)\n", base, behind)
	if rebaseErr := g.Rebase(base); rebaseErr != nil {
		_ = g.AbortRebase()
		return false, "", fmt.Errorf("auto-rebase onto %s failed: %w\n"+
			"Resolve conflicts manually (git fetch origin && git rebase %s), commit the resolution, then rerun gt done.",
			base, rebaseErr, base)
	}
	return true, "", nil
}
