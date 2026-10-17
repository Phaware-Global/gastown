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

// polecatClonePath locates a polecat's worktree, mirroring
// polecat.Manager.clonePath: the current layout is polecats/<name>/<rig>/, and
// the legacy layout is polecats/<name>/ when it holds a .git. It returns "" when
// no worktree exists.
func polecatClonePath(townRoot, rigName, polecatName string) (string, error) {
	polecatDir := filepath.Join(townRoot, rigName, "polecats", polecatName)

	current := filepath.Join(polecatDir, rigName)
	info, err := os.Stat(current)
	if err == nil && info.IsDir() {
		return current, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("%s: %w", current, err)
	}

	if _, err := os.Stat(filepath.Join(polecatDir, ".git")); err == nil {
		return polecatDir, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("%s: %w", polecatDir, err)
	}
	return "", nil
}

// slingTargetIsHolder reports whether the sling target is the bead's current
// holder, so a forced re-sling keeps that polecat's worktree. Only an exact
// match counts: a rig-only target spawns a fresh polecat even when the holder
// lives in that rig, so matchesSlingTarget's rig-only match must not exempt it.
// Anything unresolvable is not the holder, which leaves the guard in force.
func slingTargetIsHolder(target, assignee string) bool {
	holder := normalizeAgentID(assignee)
	if holder == "" {
		return false
	}
	target = strings.TrimSpace(target)
	if target == "" || target == "." {
		self, _, _, err := resolveSelfTarget()
		return err == nil && normalizeAgentID(self) == holder
	}
	if normalizeAgentID(target) == holder {
		return true
	}
	if strings.Contains(target, "/") {
		id, err := resolveTargetAgentID(target)
		return err == nil && normalizeAgentID(id) == holder
	}
	return false
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

	clonePath, err := polecatClonePath(townRoot, rigName, polecatName)
	if err != nil {
		return fmt.Errorf("cannot inspect worktree of %s: %w", assignee, err)
	}
	if clonePath == "" {
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
