package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// polecatWorkStatusFn is a seam for tests. It reports the uncommitted-work
// state of the polecat worktree at clonePath.
var polecatWorkStatusFn = func(clonePath string) (*git.UncommittedWorkStatus, error) {
	return git.NewGit(clonePath).CheckUncommittedWork()
}

// checkReassignPreservesWork refuses a forced re-sling when the current holder
// is a polecat whose worktree carries work that exists nowhere else: uncommitted
// edits, a stash, or unpushed commits. Re-slinging sends the holder a
// LIFECYCLE:Shutdown and unhooks its bead, so without this check that work is
// lost with nothing preserved (gt-a02n). The same safety bar as `gt polecat nuke`.
//
// assignee is the bead's current holder (e.g. "gastown/polecats/toast");
// non-polecat holders and holders whose worktree is already gone pass.
func checkReassignPreservesWork(townRoot, assignee string) error {
	parts := strings.Split(assignee, "/")
	if len(parts) < 3 || parts[1] != "polecats" {
		return nil
	}
	rigName, polecatName := parts[0], parts[2]

	clonePath := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if info, err := os.Stat(clonePath); err != nil || !info.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cannot inspect worktree of %s (%s): %w", assignee, clonePath, err)
		}
		return nil
	}

	status, err := polecatWorkStatusFn(clonePath)
	if err != nil {
		return fmt.Errorf("cannot verify %s has no unpreserved work (%s): %w", assignee, clonePath, err)
	}
	if status.CleanExcludingRuntime() && status.StashCount == 0 && status.UnpushedCommits == 0 {
		return nil
	}

	return fmt.Errorf("%s has unpreserved work in %s (%s); refusing to re-sling and shut it down.\n"+
		"  Commit and push (or stash elsewhere) its work first, or nuke it deliberately with `gt polecat nuke`",
		assignee, clonePath, status.String())
}
