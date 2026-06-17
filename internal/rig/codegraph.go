package rig

import (
	"os/exec"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/util"
)

// EnsureCodegraphIndex launches `codegraph init <worktreePath>` as a detached
// background process when codegraph indexing is enabled for this town/rig.
// Non-fatal: returns nil if disabled, the binary is absent, or launch fails.
func EnsureCodegraphIndex(worktreePath, townRoot, rigPath string) error {
	townSettings, _ := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	rigSettings, _ := config.LoadRigSettings(config.RigSettingsPath(rigPath))

	if !config.IsCodeGraphIndexingEnabled(townSettings, rigSettings) {
		return nil
	}

	cgPath, err := exec.LookPath("codegraph")
	if err != nil {
		// codegraph not installed — silently skip
		return nil
	}

	cmd := exec.Command(cgPath, "init", worktreePath)
	cmd.Dir = worktreePath
	util.SetDetachedProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		style.PrintWarning("could not start codegraph indexing: %v", err)
	}

	return nil
}
