package witness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/util"
)

// listAssignedWork returns the beads assigned to assignee in the given status,
// from both the rig and the town databases. Hooked wisps are included.
// A var so tests can supply the hook contents.
var listAssignedWork = func(townRoot, rigName, assignee, status string) ([]*beads.Issue, error) {
	rigDir := beads.GetRigDirForName(townRoot, rigName)
	if rigDir == "" {
		rigDir = filepath.Join(townRoot, rigName)
	}
	var found []*beads.Issue
	for _, dir := range []string{rigDir, townRoot} {
		issues, err := beads.New(dir).List(beads.ListOptions{Status: status, Assignee: assignee, Priority: -1})
		if err != nil {
			return nil, fmt.Errorf("listing %s work for %s in %s: %w", status, assignee, dir, err)
		}
		found = append(found, issues...)
	}
	return found, nil
}

// stopPolecatSession kills a polecat's session, keeping its worktree and branch.
// A var so tests can observe whether a stop was attempted.
var stopPolecatSession = func(workDir, rigName, polecatName string) error {
	address := fmt.Sprintf("%s/%s", rigName, polecatName)
	if err := util.ExecRun(workDir, "gt", "session", "stop", address, "--force"); err != nil {
		return fmt.Errorf("session stop failed: %w", err)
	}
	return nil
}

// polecatNeedsSession reports whether a polecat has anything a new session would
// act on: work on its hook, or changes in its worktree that are not yet pushed.
// A session without either only occupies a slot. Anything that cannot be read
// counts as work, so a failed lookup never strands it.
func polecatNeedsSession(bd *BdCli, workDir, townRoot, rigName, polecatName, hookBead string) bool {
	if hookBead != "" {
		status, ok := getBeadStatus(bd, workDir, hookBead)
		if !ok || isOpenHookStatus(status) {
			return true
		}
	}
	assignee := rigName + "/polecats/" + polecatName
	for _, status := range []string{beads.StatusHooked, "in_progress"} {
		issues, err := listAssignedWork(townRoot, rigName, assignee, status)
		if err != nil || len(issues) > 0 {
			return true
		}
	}
	return hasUnsavedWork(townRoot, rigName, polecatName)
}

// hasUnsavedWork reports whether the polecat's worktree holds uncommitted files,
// stashes or unpushed commits.
func hasUnsavedWork(townRoot, rigName, polecatName string) bool {
	clone := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if _, err := os.Stat(clone); errors.Is(err, fs.ErrNotExist) {
		clone = filepath.Dir(clone)
	}
	// Without its own .git, git would answer for an enclosing repository.
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	status, err := git.NewGit(clone).CheckUncommittedWork()
	if err != nil {
		return true
	}
	return !status.CleanExcludingRuntime() || status.StashCount > 0 || status.UnpushedCommits > 0
}

// restartOrStop handles a zombie whose session is still up: restart it if the
// polecat has work for a new session, otherwise stop it.
func restartOrStop(bd *BdCli, workDir, townRoot, rigName, polecatName string, zombie *ZombieResult, opts DetectZombieOptions) {
	if opts.NoRestart {
		zombie.Action = restartSuppressedAction(zombie.Classification)
		return
	}
	if polecatNeedsSession(bd, workDir, townRoot, rigName, polecatName, zombie.HookBead) {
		if err := restartPolecatSession(workDir, rigName, polecatName); err != nil {
			zombie.Error = err
			zombie.Action = fmt.Sprintf("restart-failed (%s): %v", zombie.Classification, err)
		}
		return
	}
	// Not active work: there is nothing to report as lost or to re-dispatch.
	zombie.WasActive = false
	zombie.Action = "stopped-session (no hooked or unsaved work)"
	if err := stopPolecatSession(workDir, rigName, polecatName); err != nil {
		zombie.Error = err
		zombie.Action = fmt.Sprintf("stop-failed (%s): %v", zombie.Classification, err)
	}
}
