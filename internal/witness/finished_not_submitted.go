package witness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/tmux"
)

// finishedNotSubmittedInput is the evidence that a live polecat has finished
// and pushed its work but never ran gt done.
type finishedNotSubmittedInput struct {
	HeartbeatAge    time.Duration // time since the last heartbeat; 0 when there is none
	HasHeartbeat    bool
	HookOpen        bool // the hooked bead exists and is still open/hooked/in_progress
	Submitted       bool // submission evidence (MR) already exists
	NoMergeOrFix    bool // bead is no_merge / review-fix: it lives on an existing PR branch
	OnPolecatBranch bool // HEAD is on a polecat/* branch (not detached, not the base)
	Clean           bool // no uncommitted changes, stash or unpushed commits
	AheadOfBase     int  // commits ahead of the base branch
}

// isFinishedNotSubmitted reports whether the witness should nudge the polecat
// to run gt done. Every condition must hold; any doubt means no nudge.
func isFinishedNotSubmitted(in finishedNotSubmittedInput, threshold time.Duration) bool {
	return in.HasHeartbeat && in.HeartbeatAge >= threshold &&
		in.HookOpen && !in.Submitted && !in.NoMergeOrFix &&
		in.OnPolecatBranch && in.Clean && in.AheadOfBase > 0
}

// detectFinishedNotSubmitted nudges a live polecat whose work is pushed and
// whose bead is still open after a long quiet period. It is the safe stand-in
// for the removed Stop-hook auto-gt-done: the witness never runs gt done, closes
// the bead or touches the worktree; the polecat reads the nudge and decides.
func detectFinishedNotSubmitted(bd *BdCli, workDir, townRoot, rigName, polecatName, sessionName string, t *tmux.Tmux, hb *polecat.SessionHeartbeat, snap *agentBeadSnapshot, threshold time.Duration) (ZombieResult, bool) {
	if snap == nil || snap.HookBead == "" || hb == nil {
		return ZombieResult{}, false
	}
	hookSt, ok := getBeadStatus(bd, workDir, snap.HookBead)
	if !ok || !isOpenHookStatus(hookSt) {
		return ZombieResult{}, false
	}
	age := time.Since(hb.Timestamp)
	if age < threshold {
		return ZombieResult{}, false
	}

	in := finishedNotSubmittedInput{
		HeartbeatAge: age,
		HasHeartbeat: true,
		HookOpen:     true,
		Submitted:    hasSuccessfulSubmissionEvidence(snap),
	}
	if out, err := bd.Exec(workDir, "show", snap.HookBead, "--json"); err == nil {
		var issues []beads.Issue
		if json.Unmarshal([]byte(out), &issues) == nil && len(issues) > 0 {
			if af := beads.ParseAttachmentFields(&issues[0]); af != nil {
				in.NoMergeOrFix = af.NoMerge || af.ReviewOnly || af.ReviewPR > 0
			}
		} else {
			return ZombieResult{}, false // cannot read the bead: fail closed
		}
	} else {
		return ZombieResult{}, false
	}

	clone := filepath.Join(townRoot, rigName, "polecats", polecatName, rigName)
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		clone = filepath.Join(townRoot, rigName, "polecats", polecatName)
		if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
			return ZombieResult{}, false
		}
	}
	g := git.NewGit(clone)
	branch, err := g.CurrentBranch()
	if err != nil {
		return ZombieResult{}, false
	}
	in.OnPolecatBranch = strings.HasPrefix(branch, "polecat/")
	ws, err := g.CheckUncommittedWork()
	if err != nil {
		return ZombieResult{}, false
	}
	in.Clean = ws.CleanExcludingRuntime() && ws.UnpushedCommits == 0 && ws.StashCount == 0
	if _, err := g.Rev("origin/HEAD"); err != nil {
		return ZombieResult{}, false
	}
	ahead, err := g.CommitsAhead("origin/HEAD", "HEAD")
	if err != nil {
		return ZombieResult{}, false
	}
	in.AheadOfBase = ahead

	if !isFinishedNotSubmitted(in, threshold) {
		return ZombieResult{}, false
	}

	zombie := ZombieResult{
		PolecatName:    polecatName,
		AgentState:     snap.AgentState,
		Classification: ZombieFinishedNotSubmitted,
		HookBead:       snap.HookBead,
		CleanupStatus:  snap.cleanupStatus(),
		WasActive:      false,
		Action:         fmt.Sprintf("nudged-finished-not-submitted (quiet=%v, hook_status=%s)", age.Round(time.Second), hookSt),
	}
	msg := fmt.Sprintf("RECOVERY_NEEDED: your branch %s is pushed and clean with %d commit(s) ahead, bead %s is still open, and there has been no activity for %v. If the work is finished, run gt done now. If you are waiting on something, ignore this.", branch, ahead, snap.HookBead, age.Round(time.Minute))
	if err := t.NudgeSession(sessionName, msg); err != nil {
		zombie.Error = err
		zombie.Action = fmt.Sprintf("nudge-finished-not-submitted-failed: %v", err)
	}
	return zombie, true
}
