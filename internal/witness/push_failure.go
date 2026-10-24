package witness

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// pushFailureAssessment is the verdict on an agent bead's push_failed flag.
type pushFailureAssessment struct {
	AtRisk bool
	Branch string // the branch the check resolved; never "HEAD" when a name is known
	Reason string // why the work is, or is not, considered at risk
}

// assessPushFailure decides whether a polecat's push_failed flag still means
// commits exist only in its worktree. The flag is set when a push fails and is
// never cleared when the work later reaches origin by another route (a PR
// branch, a retry, a merge), so the worktree and origin are the authority, and
// anything that cannot be verified is reported.
func assessPushFailure(workDir, rigName, polecatName, recordedBranch string) pushFailureAssessment {
	a := pushFailureAssessment{AtRisk: true, Branch: recordedBranch}

	townRoot := workDirToTownRoot(workDir)
	if townRoot == "" || rigName == "" || polecatName == "" {
		a.Reason = "polecat worktree location unknown"
		return a
	}
	g := git.NewGit(filepath.Join(townRoot, rigName, "polecats", polecatName, rigName))
	if !g.IsRepo() {
		a.Reason = "polecat worktree is missing or not a git repository"
		return a
	}

	status, err := g.CheckUncommittedWork()
	if err != nil {
		a.Reason = fmt.Sprintf("worktree state unreadable: %v", err)
		return a
	}
	if !status.CleanExcludingRuntime() {
		a.Reason = "worktree has uncommitted changes"
		return a
	}

	// A recorded branch of "" or the literal "HEAD" names nothing on origin;
	// use the worktree's own branch so the check and the alarm have a name.
	if recordedBranch == "" || recordedBranch == "HEAD" {
		if current, err := g.CurrentBranch(); err == nil && current != "" {
			a.Branch = current
		}
	}

	// Best effort: a failed or timed-out fetch leaves older remote-tracking refs
	// to judge by. Bounded, because this runs inside the patrol sweep of every
	// polecat and one stuck remote must not stall the rest.
	_ = g.FetchBounded("origin")

	if a.Branch != "" && a.Branch != "HEAD" {
		if tip, err := g.PushRemoteBranchTip("origin", a.Branch); err == nil && tip != "" {
			if held, err := g.IsAncestor("HEAD", tip); err == nil && held {
				a.AtRisk = false
				a.Reason = "HEAD is on origin/" + a.Branch
				return a
			}
		}
	}

	// HEAD may be held by a branch with a different name: a PR branch the
	// polecat pushed to instead of its own.
	if held, err := g.HeadHeldByDurableRef(); err == nil && held {
		a.AtRisk = false
		a.Reason = "HEAD is held by another origin branch"
		return a
	}

	// git cherry skips merge commits, so the patch comparison below cannot see
	// a hand-resolved merge. One that no origin branch holds is work that exists
	// only here, whatever the patches say.
	if merges, err := g.UnheldMergeCommits("origin"); err != nil {
		a.Reason = fmt.Sprintf("merge commits unreadable: %v", err)
		return a
	} else if merges > 0 {
		a.Reason = fmt.Sprintf("%d merge commit(s) not on any origin branch", merges)
		return a
	}

	// Rebases, re-pushes from another worktree and squash-merging a single
	// commit give new SHAs with the same patches. Compare by patch: HEAD's
	// commits are delivered when every one has a counterpart on some origin
	// branch, not necessarily the same one.
	if unmatched, err := commitsWithoutOriginPatch(g); err == nil && unmatched == 0 {
		a.AtRisk = false
		a.Reason = "every commit has a patch-equivalent on origin"
		return a
	} else if err == nil {
		a.Reason = fmt.Sprintf("%d commit(s) not on any origin branch", unmatched)
		return a
	}

	a.Reason = "HEAD is not on any origin branch"
	return a
}

// patchComparisonRefLimit caps how many origin branches are compared by patch;
// the most recently updated are the ones work is delivered through.
const patchComparisonRefLimit = 64

// verbatimOriginCommitLimit caps how many origin-only commits are read to
// confirm a patch match byte for byte. A match beyond it goes unconfirmed and
// the commit is reported, which errs toward the alarm.
const verbatimOriginCommitLimit = 500

// commitsWithoutOriginPatch counts HEAD's commits that no origin branch holds,
// by SHA or by patch. git cherry decides "same patch" without regard to
// whitespace, so each such match is then confirmed byte for byte.
func commitsWithoutOriginPatch(g *git.Git) (int, error) {
	refs, err := g.RecentRemoteTrackingRefs("origin", patchComparisonRefLimit)
	if err != nil {
		return 0, err
	}
	var unmatched map[string]bool
	for _, ref := range refs {
		out, err := g.Cherry(ref, "HEAD")
		if err != nil {
			continue
		}
		plus := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "+" {
				plus[fields[1]] = true
			}
		}
		if unmatched == nil {
			unmatched = plus
		} else {
			for sha := range unmatched {
				if !plus[sha] {
					delete(unmatched, sha)
				}
			}
		}
		if len(unmatched) == 0 {
			break
		}
	}
	if unmatched == nil {
		return 0, fmt.Errorf("no origin branches to compare against")
	}

	// Commits cherry matched loosely: not held by SHA anywhere, not unmatched.
	candidates, err := g.UnheldNonMergeCommits("origin")
	if err != nil {
		return 0, err
	}
	var loose []string
	for _, sha := range candidates {
		if !unmatched[sha] {
			loose = append(loose, sha)
		}
	}
	if len(loose) == 0 {
		return len(unmatched), nil
	}
	headIDs, err := g.VerbatimPatchIDs(append([]string{"--no-walk=unsorted"}, loose...)...)
	if err != nil {
		return 0, err
	}
	originArgs := append([]string{"-n", strconv.Itoa(verbatimOriginCommitLimit)}, refs...)
	originIDs, err := g.VerbatimPatchIDs(append(originArgs, "--not", "HEAD")...)
	if err != nil {
		return 0, err
	}
	onOrigin := map[string]bool{}
	for _, id := range originIDs {
		onOrigin[id] = true
	}
	for _, sha := range loose {
		if id, ok := headIDs[sha]; !ok || !onOrigin[id] {
			unmatched[sha] = true
		}
	}
	return len(unmatched), nil
}

// deliveredBranch is the branch to carry on once work is judged delivered: the
// resolved one, or "" when none could be, since "HEAD" names no branch and would
// match any MR recorded under the same bad value.
func (a pushFailureAssessment) deliveredBranch() string {
	if a.Branch == "HEAD" {
		return ""
	}
	return a.Branch
}

// sendMayorNudge delivers a nudge to the live mayor session. Tests replace it so
// a fixture can never reach the town it runs beside.
var sendMayorNudge = func(msg string) {
	mayorSession := session.MayorSessionName()
	t := tmux.NewTmux()
	if running, err := t.HasSession(mayorSession); err == nil && running {
		_ = t.NudgeSession(mayorSession, msg)
	}
}

// notifyMayorPushFailed nudges the mayor about work that may be lost.
func notifyMayorPushFailed(workDir, polecatName, branch, issue, reason string) {
	townRoot, _ := workspace.Find(workDir)
	if townRoot == "" {
		return
	}
	sendMayorNudge(fmt.Sprintf("PUSH_FAILED: polecat=%s branch=%s issue=%s — possible work loss: %s",
		polecatName, branch, issue, reason))
}
