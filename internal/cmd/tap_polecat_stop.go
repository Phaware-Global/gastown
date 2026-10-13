package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/workspace"
)

var tapPolecatStopCmd = &cobra.Command{
	Use:   "polecat-stop-check",
	Short: "Remind a polecat with pending work to run gt done (Stop hook; never acts)",
	Long: `Stop-hook check for the "idle polecat" problem: polecats that finish work
but forget to call gt done.

Claude Code fires the Stop hook at the end of EVERY assistant turn, not only
when the session ends. The session is therefore always alive when this runs, and
the polecat may simply be waiting on a build, a review or a tool. This command
never runs gt done, closes a bead or touches the worktree: doing so would close
the hooked bead, mail the dispatcher, possibly create an MR and check the
worktree out to origin/<base> in the middle of the polecat's work. It only
prints a reminder to stderr when the polecat has commits ahead of origin/main
and has not started gt done. The Witness owns recovery of polecats that really
are idle.

Exit code is always 0 so the hook never blocks the session.`,
	RunE:         runTapPolecatStop,
	SilenceUsage: true,
}

func init() {
	tapCmd.AddCommand(tapPolecatStopCmd)
}

func runTapPolecatStop(cmd *cobra.Command, args []string) error {
	// Only applies to polecats
	polecatName := os.Getenv("GT_POLECAT")
	if polecatName == "" {
		return nil // Not a polecat session — nothing to do
	}

	sessionName := os.Getenv("GT_SESSION")
	if sessionName == "" {
		return nil // No session tracking — can't check state
	}

	// Find town root for heartbeat check
	townRoot, _, _ := workspace.FindFromCwdWithFallback()
	if townRoot == "" {
		townRoot = os.Getenv("GT_TOWN_ROOT")
	}
	if townRoot == "" {
		return nil // Can't find workspace — exit quietly
	}

	hb := polecat.ReadSessionHeartbeat(townRoot, sessionName)

	// Check if the polecat is on a feature branch with commits
	rigName := os.Getenv("GT_RIG")
	if rigName == "" {
		return nil
	}

	// Reconstruct polecat worktree path
	polecatDir := filepath.Join(townRoot, rigName, "polecats", polecatName)
	// Try the nested clone layout first (polecats/<name>/<rig>/)
	cloneDir := filepath.Join(polecatDir, rigName)
	if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
		// Fall back to flat layout
		cloneDir = polecatDir
		if _, err := os.Stat(filepath.Join(cloneDir, ".git")); err != nil {
			return nil // No git repo found — exit quietly
		}
	}

	// Check current branch
	branchCmd := exec.Command("git", "-C", cloneDir, "rev-parse", "--abbrev-ref", "HEAD")
	branchOut, err := branchCmd.Output()
	if err != nil {
		return nil // Can't determine branch — exit quietly
	}
	branch := strings.TrimSpace(string(branchOut))

	aheadCmd := exec.Command("git", "-C", cloneDir, "rev-list", "--count", "origin/main..HEAD")
	aheadOut, err := aheadCmd.Output()
	if err != nil {
		return nil // Can't check — exit quietly (don't block session stop)
	}
	ahead := strings.TrimSpace(string(aheadOut))

	if decidePolecatStopAction(hb, branch, ahead) == stopActionRemind {
		fmt.Fprintf(os.Stderr, "Polecat %s has %s commit(s) ahead of origin/main on %s. Run gt done when the work is finished.\n", polecatName, ahead, branch)
	}
	return nil
}

type polecatStopAction int

const (
	stopActionNone polecatStopAction = iota
	stopActionRemind
)

// decidePolecatStopAction maps the polecat's heartbeat, current branch and
// commits-ahead count (as printed by git rev-list --count) to an action. No
// action ever mutates anything: see the command documentation.
func decidePolecatStopAction(hb *polecat.SessionHeartbeat, branch, ahead string) polecatStopAction {
	if hb != nil {
		state := hb.EffectiveState()
		if state == polecat.HeartbeatExiting || state == polecat.HeartbeatIdle {
			return stopActionNone // gt done already ran or polecat is idle
		}
	}
	if branch == "main" || branch == "master" || branch == "HEAD" {
		return stopActionNone // On default branch — nothing to submit
	}
	if ahead == "0" {
		return stopActionNone // No commits ahead — nothing to submit
	}
	return stopActionRemind
}
