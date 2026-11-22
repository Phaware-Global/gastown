package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
)

// doneWork answers the two questions gt done asks about the work it is
// completing: which commit is it, and which local ref is published to origin.
//
// For a local polecat the work is the worktree's HEAD and the ref is the
// polecat branch. For a remote polecat neither is true: its commits are pushed
// by the worker to constants.RemoteWorkRef(branch) in the rig repo, and the
// host worktree stays on the branch at its start point. gt done then completes
// from that ref and never checks it out — nothing a worker pushed is ever
// materialized as files on the host, so nothing it pushed (a hook, a config
// file) can run there.
type doneWork struct {
	remote bool
	// rev is the commit being completed, as a git revision: "HEAD" for a local
	// polecat, the work ref for a remote one.
	rev string
	// srcRef is the fully-qualified local ref published to origin.
	srcRef string
}

// resolveDoneWork decides how gt done reads the work for branch.
//
// The remote decision is made on the host from host state only: the rig's own
// execution settings and whether this session is a polecat. Nothing the caller
// passes on the command line or in a request body can select it, and a crew
// member or other local role in a remote rig still completes from its own
// worktree.
func resolveDoneWork(rigPath, branch string, isPolecat bool) doneWork {
	local := doneWork{rev: "HEAD", srcRef: "refs/heads/" + branch}
	if !isPolecat {
		return local
	}
	settings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	if err != nil || settings == nil || !settings.Execution.IsRemote() {
		return local
	}
	workRef := constants.RemoteWorkRef(branch)
	return doneWork{remote: true, rev: workRef, srcRef: workRef}
}

// localRev is the local revision that stands for the branch's work in
// comparisons (commits ahead, diff stat): the branch itself for a local
// polecat, the work ref for a remote one.
func (w doneWork) localRev(branch string) string {
	if w.remote {
		return w.srcRef
	}
	return branch
}

// refspec publishes the work to target on origin. A local polecat keeps the
// short branch:target form gt done has always used. A remote polecat's source
// is not under refs/heads, so git cannot infer the destination namespace and
// it is spelled out.
func (w doneWork) refspec(branch, target string) string {
	if w.remote {
		return w.srcRef + ":refs/heads/" + target
	}
	return branch + ":" + target
}

// remoteDonePushEnv disables every client-side hook for the publish of a
// remote polecat's work. The push runs in a host repository whose hooks path
// may resolve inside a worktree; the work being published was written on
// another machine, and no hook may run on its account.
var remoteDonePushEnv = []string{
	"GIT_CONFIG_COUNT=1",
	"GIT_CONFIG_KEY_0=core.hooksPath",
	"GIT_CONFIG_VALUE_0=/dev/null",
}

// push publishes refspec to origin from g.
func (w doneWork) push(g *git.Git, refspec string) error {
	if w.remote {
		return g.PushWithEnv("origin", refspec, false, remoteDonePushEnv)
	}
	return g.Push("origin", refspec, false)
}

// pushDirect publishes the work straight to a protected branch (the direct
// merge strategy). Hooks stay on, for a remote polecat too: the rig's pre-push
// hook is what refuses a push that would land an integration branch on the
// default branch, and it must see this push. Running it is safe — the hooks
// path resolves in the host worktree, which holds the branch's start point
// and never the worker's files, so the hook that runs is the host's own.
func (w doneWork) pushDirect(g *git.Git, refspec string) error {
	return g.Push("origin", refspec, false)
}

// contamination measures how far the work has fallen behind base and how many
// commits it adds. gt done's staleness thresholds read it, so for a remote
// polecat it must describe the work ref: the host worktree's HEAD stays at the
// start point and only ever falls further behind, however often the worker
// rebases.
func (w doneWork) contamination(g *git.Git, base string) (git.BranchContamination, error) {
	if !w.remote {
		return g.CheckBranchContamination(base)
	}
	var result git.BranchContamination
	behind, err := g.CountCommitsBetween(w.rev, base)
	if err != nil {
		return result, fmt.Errorf("counting commits behind %s: %w", base, err)
	}
	result.Behind = behind
	ahead, err := g.CommitsAhead(base, w.rev)
	if err != nil {
		return result, fmt.Errorf("counting commits ahead of %s: %w", base, err)
	}
	result.Ahead = ahead
	return result, nil
}

// mrGuardEnv is the environment the merge-request submit guards run in. The
// guards ask which patches the MR would carry by naming HEAD; for a remote
// polecat that question is about the work ref, the commit the MR records.
func (w doneWork) mrGuardEnv(g *git.Git) mrGuardEnv {
	env := realMRGuardEnv(g)
	if !w.remote {
		return env
	}
	patchIDs := env.RangePatchIDs
	env.RangePatchIDs = func(base, tip string) ([]string, error) {
		if tip == "HEAD" {
			tip = w.rev
		}
		return patchIDs(base, tip)
	}
	return env
}

// pushCheckpointHolds reports whether a push checkpoint from an earlier run
// still stands, so the push can be skipped. For a remote polecat it stands
// only while origin's branch is at the work ref's current commit: the worker
// may have pushed more since, and skipping would record an MR for a commit
// origin does not have.
func (w doneWork) pushCheckpointHolds(g *git.Git, branch string) bool {
	if !w.remote {
		return true
	}
	tip, err := g.Rev(w.rev)
	if err != nil {
		return false
	}
	return g.VerifyPushedCommit("origin", branch, tip) == nil
}

// requireRemoteWork refuses a COMPLETED gt done for a remote polecat whose
// work never reached the host. Without the work ref there is nothing to
// publish, and completing would close the bead with no work behind it.
func (w doneWork) requireRemoteWork(g *git.Git, branch string) error {
	if !w.remote {
		return nil
	}
	exists, err := g.RefExists(w.srcRef)
	if err != nil {
		return fmt.Errorf("cannot complete: checking %s: %w", w.srcRef, err)
	}
	if !exists {
		return fmt.Errorf("cannot complete: no work has been pushed for branch %s\n"+
			"This rig's polecats run on a remote worker, and their commits reach the host as %s.\n"+
			"Commit your changes and run gt done again from the worker's worktree, which pushes them first.\n"+
			"If there is nothing to submit: gt done --status DEFERRED",
			branch, w.srcRef)
	}
	return nil
}

// releaseRemoteWork deletes the work ref once gt done has finished with it.
//
// It is called only where gt done also deletes the polecat's local branch:
// after a completed run in which the push and the merge request both
// succeeded. From then on the published branch on origin is the record of the
// work. Leaving the ref behind would be worse than removing it: a later
// session for the same branch resumes from the work ref when one exists, and
// would silently restart from this old tip instead of the branch's current
// one. On any failure the ref is kept so the work can be recovered.
//
// The ref is deleted only if origin holds every commit it points at, and only
// while it still points there. A worker can push again while gt done is busy
// with the merge request; those commits exist nowhere else on the host, so a
// ref that has moved on, or that origin does not fully hold, is left in place
// and reported.
func (w doneWork) releaseRemoteWork(g *git.Git) error {
	if !w.remote {
		return nil
	}
	exists, err := g.RefExists(w.srcRef)
	if err != nil || !exists {
		return err
	}
	tip, err := g.Rev(w.srcRef)
	if err != nil {
		return err
	}
	unpublished, err := g.CommitsNotOnRemote(tip, "origin")
	if err != nil {
		return err
	}
	if unpublished > 0 {
		return fmt.Errorf("it holds %d commit(s) origin does not have; left in place", unpublished)
	}
	return g.DeleteRef(w.srcRef, tip)
}
