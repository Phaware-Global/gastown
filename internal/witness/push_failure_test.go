package witness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

const (
	pfRig     = "alpha"
	pfPolecat = "nux"
)

// pushFixture is a town with one polecat worktree cloned from a bare origin,
// plus a second clone ("other") that stands in for every route by which work
// reaches origin without going through the polecat's own branch.
type pushFixture struct {
	t     *testing.T
	town  string
	clone string // the polecat worktree
	other string
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	f := &pushFixture{t: t, town: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(f.town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.town, "mayor", "town.json"), []byte("{}"), 0o644); err != nil { //nolint:gosec // G306: test fixture
		t.Fatal(err)
	}
	origin := filepath.Join(f.town, "origin.git")
	seed := filepath.Join(t.TempDir(), "seed")
	gitIn(t, f.town, "init", "--bare", "-b", "main", origin)
	gitIn(t, f.town, "init", "-b", "main", seed)
	gitIn(t, seed, "config", "user.email", "test@example.com")
	gitIn(t, seed, "config", "user.name", "Test")
	f.write(seed, "README.md", "base\n")
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-m", "base")
	gitIn(t, seed, "remote", "add", "origin", origin)
	gitIn(t, seed, "push", "origin", "main")

	f.clone = filepath.Join(f.town, pfRig, "polecats", pfPolecat, pfRig)
	f.other = filepath.Join(t.TempDir(), "other")
	for _, dir := range []string{f.clone, f.other} {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		gitIn(t, f.town, "clone", "-q", origin, dir)
		gitIn(t, dir, "config", "user.email", "test@example.com")
		gitIn(t, dir, "config", "user.name", "Test")
	}
	return f
}

func (f *pushFixture) write(dir, name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil { //nolint:gosec // G306: test fixture
		f.t.Fatal(err)
	}
}

// commit adds a file in dir on its current branch and returns the new SHA.
func (f *pushFixture) commit(dir, name string) string {
	f.t.Helper()
	f.write(dir, name, name+"\n")
	gitIn(f.t, dir, "add", name)
	gitIn(f.t, dir, "commit", "-m", "add "+name)
	return gitIn(f.t, dir, "rev-parse", "HEAD")
}

// polecatBranch puts the polecat worktree on a local polecat branch and commits
// the named files on it.
func (f *pushFixture) polecatBranch(branch string, files ...string) {
	f.t.Helper()
	gitIn(f.t, f.clone, "checkout", "-q", "-b", branch)
	for _, name := range files {
		f.commit(f.clone, name)
	}
}

// pushedElsewhere makes origin hold branch at the polecat's HEAD, pushed from
// the other clone so the polecat worktree has no remote-tracking ref for it.
func (f *pushFixture) pushedElsewhere(branch string) {
	f.t.Helper()
	gitIn(f.t, f.clone, "push", "-q", f.other, "HEAD:refs/heads/staging-"+branch)
	gitIn(f.t, f.other, "push", "-q", "origin", "staging-"+branch+":refs/heads/"+branch)
	gitIn(f.t, f.other, "branch", "-q", "-D", "staging-"+branch)
}

func (f *pushFixture) assess(recordedBranch string) pushFailureAssessment {
	f.t.Helper()
	return assessPushFailure(f.town, pfRig, pfPolecat, recordedBranch)
}

func expectSafe(t *testing.T, a pushFailureAssessment) {
	t.Helper()
	if a.AtRisk {
		t.Errorf("work reported at risk, want safe (reason: %s)", a.Reason)
	}
}

func expectAtRisk(t *testing.T, a pushFailureAssessment) {
	t.Helper()
	if !a.AtRisk {
		t.Errorf("work reported safe, want at risk (reason: %s)", a.Reason)
	}
	if a.Reason == "" {
		t.Error("an at-risk verdict must say why")
	}
}

// Genuinely unpushed work is the case the alarm exists for.
func TestAssessPushFailure_UnpushedCommitsAreAtRisk(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "feature.txt")
	expectAtRisk(t, f.assess("polecat/nux-al-1"))
}

func TestAssessPushFailure_DirtyWorktreeIsAtRisk(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "feature.txt")
	f.pushedElsewhere("polecat/nux-al-1")
	f.write(f.clone, "feature.txt", "edited after the push\n")
	expectAtRisk(t, f.assess("polecat/nux-al-1"))
}

func TestAssessPushFailure_MissingWorktreeIsAtRisk(t *testing.T) {
	town := t.TempDir()
	a := assessPushFailure(town, pfRig, pfPolecat, "polecat/nux-al-1")
	expectAtRisk(t, a)
}

// The recorded branch is on origin at HEAD even though the polecat worktree
// never fetched it: the flag is stale, ls-remote is the authority.
func TestAssessPushFailure_RecordedBranchOnOriginIsSafe(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "feature.txt")
	f.pushedElsewhere("polecat/nux/al-1@mvx")
	expectSafe(t, f.assess("polecat/nux/al-1@mvx"))
}

// The recorded branch is the literal "HEAD" (a detached or unresolved branch at
// push time). HEAD is held by another origin branch, here a PR branch the
// polecat pushed to, so the work is delivered under a name the flag never held.
func TestAssessPushFailure_LiteralHEADHeldByOtherOriginBranchIsSafe(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "feature.txt")
	f.pushedElsewhere("polecat/jade/al-1@mvx")

	a := f.assess("HEAD")
	expectSafe(t, a)
	if a.Branch == "HEAD" {
		t.Errorf("Branch = %q, want the worktree's branch name resolved", a.Branch)
	}
}

// The same, with the worktree itself detached.
func TestAssessPushFailure_DetachedHEADHeldByOriginBranchIsSafe(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "feature.txt")
	f.pushedElsewhere("polecat/jade/al-1@mvx")
	gitIn(t, f.clone, "checkout", "-q", "--detach")

	expectSafe(t, f.assess("HEAD"))
}

// A literal HEAD on a worktree whose commits are nowhere on origin is still lost
// work, and the verdict names the worktree's branch rather than "HEAD".
func TestAssessPushFailure_LiteralHEADWithUnpushedCommitsIsAtRisk(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "feature.txt")

	a := f.assess("HEAD")
	expectAtRisk(t, a)
	if a.Branch != "polecat/nux-al-1" {
		t.Errorf("Branch = %q, want the resolved worktree branch", a.Branch)
	}
}

// An empty recorded branch is resolved from the worktree too, so the alarm
// never names nothing.
func TestAssessPushFailure_EmptyBranchIsResolvedFromWorktree(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "feature.txt")

	a := f.assess("")
	expectAtRisk(t, a)
	if a.Branch != "polecat/nux-al-1" {
		t.Errorf("Branch = %q, want the resolved worktree branch", a.Branch)
	}
}

// cherryPickTo replays the polecat's commits (all but the base) onto branch in
// the other clone and pushes it. The branch starts from main plus an unrelated
// commit, so the replayed commits get new SHAs but keep their patches, as after
// a rebase or a re-push from another worktree.
func (f *pushFixture) cherryPickTo(branch, scratchFile string, commits ...string) {
	f.t.Helper()
	gitIn(f.t, f.other, "fetch", "-q", f.clone, "+refs/heads/*:refs/remotes/clone/*")
	gitIn(f.t, f.other, "checkout", "-q", "-b", branch, "origin/main")
	f.commit(f.other, scratchFile)
	for _, c := range commits {
		gitIn(f.t, f.other, "cherry-pick", c)
	}
	gitIn(f.t, f.other, "push", "-q", "origin", branch)
	gitIn(f.t, f.other, "checkout", "-q", "--detach")
}

// Commits whose patches are all on an origin branch are delivered even though
// no origin branch contains their SHAs.
func TestAssessPushFailure_PatchEquivalentToOriginBranchIsSafe(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "a.txt", "b.txt")
	b := gitIn(t, f.clone, "rev-parse", "HEAD")
	a := gitIn(t, f.clone, "rev-parse", "HEAD~1")
	f.cherryPickTo("pr/al-1", "scratch.txt", a, b)

	expectSafe(t, f.assess("polecat/nux-al-1"))
}

// Different commits can be delivered by different routes: one replayed on a PR
// branch, one landed on main.
func TestAssessPushFailure_PatchesSpreadAcrossOriginBranchesAreSafe(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "a.txt", "b.txt")
	b := gitIn(t, f.clone, "rev-parse", "HEAD")
	a := gitIn(t, f.clone, "rev-parse", "HEAD~1")
	f.cherryPickTo("pr/al-1", "scratch.txt", a)
	f.cherryPickTo("pr/al-2", "scratch2.txt", b)

	expectSafe(t, f.assess("polecat/nux-al-1"))
}

// One commit with no counterpart on origin is real unpushed work.
func TestAssessPushFailure_OneUnmatchedPatchIsAtRisk(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "a.txt", "b.txt")
	a := gitIn(t, f.clone, "rev-parse", "HEAD~1")
	f.cherryPickTo("pr/al-1", "scratch.txt", a)

	expectAtRisk(t, f.assess("polecat/nux-al-1"))
}

func pushFailedFields(branch string) *beads.AgentFields {
	return &beads.AgentFields{
		ExitType:        string(ExitTypeCompleted),
		Branch:          branch,
		LastSourceIssue: "al-1",
		PushFailed:      true,
	}
}

func noBeads() *BdCli {
	return &BdCli{
		Exec: func(string, ...string) (string, error) { return "[]", nil },
		Run:  func(string, ...string) error { return nil },
	}
}

// captureMayorNudges replaces the mayor nudge sender for the test, so nothing
// here can reach a live town's tmux session, and returns what was sent.
func captureMayorNudges(t *testing.T) *[]string {
	t.Helper()
	var sent []string
	orig := sendMayorNudge
	sendMayorNudge = func(msg string) { sent = append(sent, msg) }
	t.Cleanup(func() { sendMayorNudge = orig })
	return &sent
}

// Both PUSH_FAILED sites act on the verdict, not on the flag alone.
func TestPushFailedHandlersFollowTheAssessment(t *testing.T) {
	t.Run("delivered work is not reported", func(t *testing.T) {
		sent := captureMayorNudges(t)
		f := newPushFixture(t)
		f.polecatBranch("polecat/nux-al-1", "feature.txt")
		f.pushedElsewhere("polecat/jade/al-1@mvx")

		res := HandlePolecatDoneFromBead(noBeads(), f.town, pfRig, pfPolecat, pushFailedFields("HEAD"), nil)
		if strings.Contains(res.Action, "push-failed-recovery-needed") {
			t.Errorf("FromBead reported delivered work: %s", res.Action)
		}

		d := &CompletionDiscovery{}
		processDiscoveredCompletion(noBeads(), f.town, pfRig, &PolecatDonePayload{
			PolecatName: pfPolecat, Exit: string(ExitTypeCompleted), Branch: "HEAD", IssueID: "al-1", PushFailed: true,
		}, d)
		if strings.Contains(d.Action, "push-failed-recovery-needed") {
			t.Errorf("discovery reported delivered work: %s", d.Action)
		}
		if len(*sent) != 0 {
			t.Errorf("mayor nudged for delivered work: %q", *sent)
		}
	})

	t.Run("unpushed work is reported with branch and reason", func(t *testing.T) {
		sent := captureMayorNudges(t)
		f := newPushFixture(t)
		f.polecatBranch("polecat/nux-al-1", "feature.txt")

		res := HandlePolecatDoneFromBead(noBeads(), f.town, pfRig, pfPolecat, pushFailedFields("HEAD"), nil)
		for _, want := range []string{"push-failed-recovery-needed", "branch=polecat/nux-al-1", "issue=al-1", "not on any origin branch"} {
			if !strings.Contains(res.Action, want) {
				t.Errorf("FromBead action %q missing %q", res.Action, want)
			}
		}

		d := &CompletionDiscovery{}
		processDiscoveredCompletion(noBeads(), f.town, pfRig, &PolecatDonePayload{
			PolecatName: pfPolecat, Exit: string(ExitTypeCompleted), IssueID: "al-1", PushFailed: true,
		}, d)
		if !strings.Contains(d.Action, "branch=polecat/nux-al-1") {
			t.Errorf("discovery action %q should name the resolved branch", d.Action)
		}

		// One nudge per site, each naming what is at risk.
		if len(*sent) != 2 {
			t.Fatalf("mayor nudges = %q, want 2", *sent)
		}
		for _, msg := range *sent {
			for _, want := range []string{"PUSH_FAILED: polecat=nux", "branch=polecat/nux-al-1", "issue=al-1", "not on any origin branch"} {
				if !strings.Contains(msg, want) {
					t.Errorf("nudge %q missing %q", msg, want)
				}
			}
		}
	})
}

// resolvedMergeOnTop pushes a polecat branch, then merges a conflicting main
// into it by hand and leaves that merge unpushed, as when the push after the
// resolution is rejected. It returns the merge commit's SHA.
func (f *pushFixture) resolvedMergeOnTop(branch string) string {
	f.t.Helper()
	gitIn(f.t, f.clone, "checkout", "-q", "-b", branch)
	f.commit(f.clone, "a.txt")
	f.write(f.clone, "README.md", "polecat\n")
	gitIn(f.t, f.clone, "commit", "-q", "-am", "polecat edits README")
	gitIn(f.t, f.clone, "push", "-q", "origin", branch)

	gitIn(f.t, f.other, "checkout", "-q", "main")
	f.write(f.other, "README.md", "main\n")
	gitIn(f.t, f.other, "commit", "-q", "-am", "main edits README")
	gitIn(f.t, f.other, "push", "-q", "origin", "main")

	gitIn(f.t, f.clone, "fetch", "-q", "origin")
	cmd := exec.Command("git", "merge", "origin/main")
	cmd.Dir = f.clone
	if out, err := cmd.CombinedOutput(); err == nil {
		f.t.Fatalf("merge did not conflict:\n%s", out)
	}
	f.write(f.clone, "README.md", "polecat and main, resolved by hand\n")
	gitIn(f.t, f.clone, "add", "README.md")
	gitIn(f.t, f.clone, "commit", "-q", "--no-edit")
	return gitIn(f.t, f.clone, "rev-parse", "HEAD")
}

// git cherry skips merge commits, so a conflict-resolving merge that is on no
// origin branch must still be reported, not read as delivered.
func TestAssessPushFailure_UnpushedConflictResolutionMergeIsAtRisk(t *testing.T) {
	f := newPushFixture(t)
	f.resolvedMergeOnTop("polecat/nux-al-1")

	a := f.assess("polecat/nux-al-1")
	expectAtRisk(t, a)
	if !strings.Contains(a.Reason, "merge") {
		t.Errorf("reason %q should name the unpushed merge", a.Reason)
	}
}

// Once the merge reaches origin it is held, and the work is safe.
func TestAssessPushFailure_PushedConflictResolutionMergeIsSafe(t *testing.T) {
	f := newPushFixture(t)
	f.resolvedMergeOnTop("polecat/nux-al-1")
	gitIn(t, f.clone, "push", "-q", "origin", "polecat/nux-al-1")

	expectSafe(t, f.assess("polecat/nux-al-1"))
}

// An amend that only changes indentation has the same whitespace-insensitive
// patch-id as the pushed version, but in a Makefile or YAML file the change is
// the whole point. When its push was rejected it exists only in the worktree.
func TestAssessPushFailure_WhitespaceOnlyAmendIsAtRisk(t *testing.T) {
	f := newPushFixture(t)
	gitIn(t, f.clone, "checkout", "-q", "-b", "polecat/nux-al-1")
	f.write(f.clone, "Makefile", "all:\n    echo hi\n")
	gitIn(t, f.clone, "add", "Makefile")
	gitIn(t, f.clone, "commit", "-q", "-m", "add Makefile")
	gitIn(t, f.clone, "push", "-q", "origin", "polecat/nux-al-1")

	f.write(f.clone, "Makefile", "all:\n\techo hi\n")
	gitIn(t, f.clone, "commit", "-q", "-a", "--amend", "--no-edit")

	expectAtRisk(t, f.assess("polecat/nux-al-1"))
}

// A rebase keeps the patch byte for byte, so the whitespace check must not turn
// it into a false alarm.
func TestAssessPushFailure_VerbatimPatchOnOtherBranchStaysSafe(t *testing.T) {
	f := newPushFixture(t)
	f.polecatBranch("polecat/nux-al-1", "Makefile")
	f.write(f.clone, "Makefile", "all:\n\techo hi\n")
	gitIn(t, f.clone, "commit", "-q", "-a", "--amend", "--no-edit")
	head := gitIn(t, f.clone, "rev-parse", "HEAD")
	f.cherryPickTo("pr/al-1", "scratch.txt", head)

	expectSafe(t, f.assess("polecat/nux-al-1"))
}

// mrBeads answers the bd calls the completion handlers make with two open MRs:
// one another polecat recorded under the literal branch "HEAD", and this
// polecat's own. It records the description of each cleanup wisp created.
func mrBeads() (*BdCli, *[]string) {
	var wisps []string
	return &BdCli{
		Exec: func(_ string, args ...string) (string, error) {
			switch args[0] {
			case "query":
				return `[{"id":"mr-head","description":"branch: HEAD\nsource_issue: zz-9"},` +
					`{"id":"mr-real","description":"branch: polecat/nux-al-1\nsource_issue: al-1"}]`, nil
			case "show":
				return `[{"id":"` + args[1] + `","status":"open"}]`, nil
			case "create":
				for i, a := range args {
					if a == "--description" && i+1 < len(args) {
						wisps = append(wisps, args[i+1])
					}
				}
				return `{"id":"wisp-1"}`, nil
			}
			return "[]", nil
		},
		Run: func(string, ...string) error { return nil },
	}, &wisps
}

// Delivered work under an unresolved branch name must look up the MR by the
// branch the assessment resolved, not by "HEAD" (which matches another
// polecat's MR) or by "" (which skips the lookup and strands a real MR).
func TestPushFailedDeliveredWorkLooksUpMRByResolvedBranch(t *testing.T) {
	t.Setenv("GT_TEST_NUDGE_LOG", filepath.Join(t.TempDir(), "nudge.log"))
	for _, recorded := range []string{"HEAD", ""} {
		t.Run("recorded="+recorded, func(t *testing.T) {
			captureMayorNudges(t)
			f := newPushFixture(t)
			f.polecatBranch("polecat/nux-al-1", "feature.txt")
			f.pushedElsewhere("polecat/jade/al-1@mvx")

			bd, wisps := mrBeads()
			res := HandlePolecatDoneFromBead(bd, f.town, pfRig, pfPolecat, pushFailedFields(recorded), nil)
			if !strings.Contains(res.Action, "MR=mr-real") {
				t.Errorf("FromBead action %q should pend on this polecat's MR", res.Action)
			}

			bd, wisps = mrBeads()
			d := &CompletionDiscovery{}
			processDiscoveredCompletion(bd, f.town, pfRig, &PolecatDonePayload{
				PolecatName: pfPolecat, Exit: string(ExitTypeCompleted), Branch: recorded, IssueID: "al-1", PushFailed: true,
			}, d)
			if !strings.Contains(d.Action, "MR=mr-real") {
				t.Errorf("discovery action %q should pend on this polecat's MR", d.Action)
			}
			for _, w := range *wisps {
				if strings.Contains(w, "Branch: HEAD") || !strings.Contains(w, "Branch: polecat/nux-al-1") {
					t.Errorf("cleanup wisp should carry the resolved branch:\n%s", w)
				}
			}
		})
	}
}

func TestPushFailureAssessment_DeliveredBranchNeverHEAD(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"HEAD", ""}, {"", ""}, {"polecat/nux-al-1", "polecat/nux-al-1"},
	} {
		if got := (pushFailureAssessment{Branch: tc.in}).deliveredBranch(); got != tc.want {
			t.Errorf("deliveredBranch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
