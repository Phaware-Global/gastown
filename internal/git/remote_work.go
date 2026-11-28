package git

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/constants"
)

// RemoteWork describes what a remote polecat's worker has pushed to the host
// for a branch and origin does not have yet.
//
// A remote polecat's host worktree stays on its branch at the start point; its
// commits arrive at constants.RemoteWorkRef(branch) and are published to origin
// only by gt done. Until then they exist nowhere but that ref, so every reader
// that asks "does this polecat have unpushed work" must count them — the
// worktree alone always looks like a polecat that has done nothing.
type RemoteWork struct {
	// Ref is the work ref for the branch, whether or not it exists.
	Ref string
	// Tip is the commit the work ref points at; empty when there is no ref.
	Tip string
	// Unpublished is how many of its commits are on no remote-tracking ref.
	Unpublished int
}

// UnpublishedRemoteWork reports the remote work pushed for branch that remote
// does not hold. A branch with no work ref — every local polecat, and a remote
// one that has not pushed — has none, and that is not an error.
func (g *Git) UnpublishedRemoteWork(branch, remote string) (RemoteWork, error) {
	work := RemoteWork{}
	if branch == "" || branch == "HEAD" {
		return work, nil
	}
	if remote == "" {
		remote = "origin"
	}
	work.Ref = constants.RemoteWorkRef(branch)
	exists, err := g.RefExists(work.Ref)
	if err != nil {
		return work, fmt.Errorf("checking %s: %w", work.Ref, err)
	}
	if !exists {
		return work, nil
	}
	if work.Tip, err = g.Rev(work.Ref); err != nil {
		return work, fmt.Errorf("resolving %s: %w", work.Ref, err)
	}
	if work.Unpublished, err = g.CommitsNotOnRemote(work.Tip, remote); err != nil {
		return work, fmt.Errorf("counting commits in %s: %w", work.Ref, err)
	}
	return work, nil
}

// RemoteWorkBranches lists the branches that have a remote work ref.
func (g *Git) RemoteWorkBranches() ([]string, error) {
	out, err := g.run("for-each-ref", "--format=%(refname)", constants.RemoteWorkRefPrefix)
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, ref := range strings.Split(out, "\n") {
		if ref = strings.TrimSpace(ref); ref != "" {
			branches = append(branches, strings.TrimPrefix(ref, constants.RemoteWorkRefPrefix))
		}
	}
	return branches, nil
}

// HooksDisabledEnv returns the environment that disables client-side hooks, as
// KEY=value entries. The host publishes commits written on another machine
// under it: the repository's hooks path may resolve inside a worktree, and no
// hook may run on their account.
func HooksDisabledEnv() []string {
	var env []string
	for k, v := range constants.GitHooksDisabledEnv() {
		env = append(env, k+"="+v)
	}
	return env
}

// PublishRemoteWork pushes tip, a commit of the work ref for branch, to remote
// as the branch itself, with client-side hooks disabled and without checking
// anything out.
//
// The caller names the commit, not the ref: the worker can push to the ref at
// any moment, and what reaches the remote must be exactly the commit the
// caller examined.
func (g *Git) PublishRemoteWork(branch, tip, remote string) error {
	if remote == "" {
		remote = "origin"
	}
	if tip == "" {
		return fmt.Errorf("publishing remote work for %s: no commit given", branch)
	}
	return g.PushWithEnv(remote, tip+":refs/heads/"+branch, false, HooksDisabledEnv())
}

// withRemoteWork folds unpublished remote work for branch into a preservation
// verdict: work that is only in the work ref is not preserved, whatever the
// host worktree's own HEAD says. That holds even when the worktree had nothing
// to compare against (errNoComparisonRefs); any other error stands.
func (g *Git) withRemoteWork(status BranchPreservationStatus, statusErr error, branch, remote string) (BranchPreservationStatus, error) {
	if statusErr != nil && !errors.Is(statusErr, errNoComparisonRefs) {
		return status, statusErr
	}
	work, err := g.UnpublishedRemoteWork(branch, remote)
	if err != nil {
		return status, err
	}
	if work.Unpublished == 0 {
		return status, statusErr
	}
	if statusErr != nil {
		status = BranchPreservationStatus{}
	}
	status.Preserved = false
	status.UnpreservedPatchCount += work.Unpublished
	status.Evidence = "remote_work_ref"
	return status, nil
}
