package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// remotePolecatDoneError refuses gt done for a rig whose polecats run on a
// remote execution backend.
//
// A remote polecat's commits are not in its host worktree: they are pushed to
// constants.RemoteWorkRefPrefix in the rig repo, and the host worktree stays at
// the branch's start point. Everything gt done does today reads that worktree
// (HEAD, the index, uncommitted changes), so running it would see a polecat
// with no work — and could complete the bead without it. Completing from the
// work ref is a separate change; until then this is refused, loudly, before
// anything is modified.
//
// Only a COMPLETED exit is refused: it is the one that verifies and submits
// work. ESCALATED and DEFERRED report a stuck or parked polecat and read no
// work from the worktree, and a remote polecat has to be able to send them.
//
// The decision comes from the rig's own settings on the host, never from the
// caller's arguments or environment.
func remotePolecatDoneError(rigPath, rigName, exitType string) error {
	if exitType != ExitCompleted {
		return nil
	}
	settings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	if err != nil || settings == nil || !settings.Execution.IsRemote() {
		return nil
	}
	return fmt.Errorf("gt done is not supported yet for rig %s, whose polecats run on the %q execution backend: "+
		"the work is in %s<branch> in the rig repo, not in the host worktree, and completing from there is not implemented. Nothing was changed",
		rigName, settings.Execution.BackendName(), constants.RemoteWorkRefPrefix)
}
