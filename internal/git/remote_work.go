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
	out, err := g.run("rev-list", "--count", work.Ref, "--not", "--remotes="+remote)
	if err != nil {
		return work, fmt.Errorf("counting commits in %s: %w", work.Ref, err)
	}
	if _, err := fmt.Sscanf(out, "%d", &work.Unpublished); err != nil {
		return work, fmt.Errorf("parsing commit count for %s: %w", work.Ref, err)
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

// hooksDisabledEnv makes git run no client-side hook. It is used wherever the
// host publishes commits written on another machine: the repository's hooks
// path may resolve inside a worktree, and no hook may run on their account.
var hooksDisabledEnv = []string{
	"GIT_CONFIG_COUNT=1",
	"GIT_CONFIG_KEY_0=core.hooksPath",
	"GIT_CONFIG_VALUE_0=/dev/null",
}

// HooksDisabledEnv returns the environment that disables client-side hooks.
func HooksDisabledEnv() []string {
	return append([]string(nil), hooksDisabledEnv...)
}

// PublishRemoteWork pushes the work ref for branch to remote as the branch
// itself, with client-side hooks disabled and without checking anything out.
func (g *Git) PublishRemoteWork(branch, remote string) error {
	if remote == "" {
		remote = "origin"
	}
	return g.PushWithEnv(remote, constants.RemoteWorkRef(branch)+":refs/heads/"+branch, false, hooksDisabledEnv)
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
