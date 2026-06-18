package rig

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/util"
)

// EnsureCodegraphIndex launches `codegraph init <worktreePath>` as a detached
// background process when codegraph indexing is enabled for this town/rig.
// Non-fatal: returns nil if indexing is disabled or the binary is absent.
// Returns an error only on a real config-load failure or if the process fails
// to start, so callers can surface it with their own context.
func EnsureCodegraphIndex(worktreePath, townRoot, rigPath string) error {
	townSettings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading town settings: %w", err)
	}
	rigSettings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	if err != nil && !errors.Is(err, config.ErrNotFound) {
		return fmt.Errorf("loading rig settings: %w", err)
	}

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
	cmd.Stdout = nil // discard — detached process must not bleed onto the caller's terminal
	cmd.Stderr = nil // discard
	util.SetDetachedProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting codegraph indexing: %w", err)
	}

	// Reap the detached process to prevent zombie/defunct entries in a long-running daemon.
	go func() {
		_ = cmd.Wait()
	}()

	return nil
}
