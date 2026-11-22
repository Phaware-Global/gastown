package cmd

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/refinery"
)

// Failures that happen before the sync/push runs must still exit 4 with a
// message on stderr: the patrol formula handles 0/2/3/4 and nothing else.
func TestRefineryPrSyncPush_PreflightFailuresExitRefused(t *testing.T) {
	t.Setenv("GT_ROOT", "")
	t.Chdir(t.TempDir())

	cases := []struct {
		name string
		run  func(*cobra.Command, []string) error
		set  func()
	}{
		{"sync-branch missing flags", runRefineryPrSyncBranch, func() { refPrSyncBranch, refPrSyncTarget = "", "" }},
		{"sync-branch outside a workspace", runRefineryPrSyncBranch, func() { refPrSyncBranch, refPrSyncTarget = "feature/x", "main" }},
		{"push-branch missing flag", runRefineryPrPushBranch, func() { refPrPushBranch = "" }},
		{"push-branch outside a workspace", runRefineryPrPushBranch, func() { refPrPushBranch = "feature/x" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.set()
			cmd := &cobra.Command{}
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)

			err := tc.run(cmd, nil)
			code, ok := IsSilentExit(err)
			if !ok || code != syncExitRefused {
				t.Fatalf("got err=%v, want silent exit %d", err, syncExitRefused)
			}
			if stderr.Len() == 0 {
				t.Fatal("want the failure reason on stderr")
			}
		})
	}
}

type branchHoldProvider struct {
	holdGateProvider
	prNumber int
	findErr  error
}

func (p *branchHoldProvider) FindPRNumber(string) (int, error) { return p.prNumber, p.findErr }

// A held MR must stop sync-branch and push-branch before they touch the
// branch: the gate returns exit 5 and the MR lookup never rewrites anything.
func TestHoldGateForBranchUpdate(t *testing.T) {
	show := func(labels ...string) refinery.BeadShower {
		return holdGateShow(map[string]*beads.Issue{
			"gt-mr-1":  holdGateMR(labels...),
			"gt-src-1": {ID: "gt-src-1"},
		})
	}
	noFind := func(int) (*beads.Issue, error) { return nil, nil }
	gate := func(p refinery.PRProvider, strategy, mrID string, show refinery.BeadShower) error {
		cmd := &cobra.Command{}
		cmd.SetErr(&bytes.Buffer{})
		return reportHoldGate(cmd, "sync-branch",
			holdGateForBranchUpdate(p, strategy, "feat/x", mrID, noFind, show))
	}

	t.Run("held MR bead is refused with the held exit code", func(t *testing.T) {
		err := gate(&branchHoldProvider{prNumber: 42}, "pr", "gt-mr-1", show("hold"))
		wantExit(t, err, syncExitHeld)
	})
	t.Run("held MR bead is refused when no PR exists yet", func(t *testing.T) {
		err := gate(&branchHoldProvider{}, "pr", "gt-mr-1", show("hold"))
		wantExit(t, err, syncExitHeld)
	})
	t.Run("held MR bead is refused when the rig does not publish PRs", func(t *testing.T) {
		err := gate(&branchHoldProvider{}, "direct", "gt-mr-1", show("hold"))
		wantExit(t, err, syncExitHeld)
	})
	t.Run("draft PR is refused", func(t *testing.T) {
		p := &branchHoldProvider{prNumber: 42}
		p.draft = true
		err := gate(p, "pr", "gt-mr-1", show())
		wantExit(t, err, syncExitHeld)
	})
	t.Run("unheld MR passes", func(t *testing.T) {
		if err := gate(&branchHoldProvider{prNumber: 42}, "pr", "gt-mr-1", show()); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
	t.Run("a check that cannot run fails closed with the refused code", func(t *testing.T) {
		err := gate(&branchHoldProvider{prNumber: 42}, "pr", "gt-mr-missing", show())
		wantExit(t, err, syncExitRefused)
	})
	t.Run("PR lookup failure fails closed", func(t *testing.T) {
		err := gate(&branchHoldProvider{findErr: errors.New("gh down")}, "pr", "gt-mr-1", show())
		wantExit(t, err, syncExitRefused)
	})
	t.Run("source issue that reads as not found is a failed check, not a hold", func(t *testing.T) {
		missingSrc := holdGateShow(map[string]*beads.Issue{"gt-mr-1": holdGateMR()})
		err := gate(&branchHoldProvider{prNumber: 42}, "pr", "gt-mr-1", missingSrc)
		wantExit(t, err, syncExitRefused)
	})
	t.Run("held source issue is still a hold", func(t *testing.T) {
		heldSrc := holdGateShow(map[string]*beads.Issue{
			"gt-mr-1":  holdGateMR(),
			"gt-src-1": {ID: "gt-src-1", Status: "deferred"},
		})
		err := gate(&branchHoldProvider{prNumber: 42}, "pr", "gt-mr-1", heldSrc)
		wantExit(t, err, syncExitHeld)
	})
}

// The refinery role prompt runs sync-branch/push-branch without --mr. The gate
// must still run: the MR is found by PR number and the draft check applies.
func TestHoldGateForBranchUpdate_NoMR(t *testing.T) {
	issues := map[string]*beads.Issue{
		"gt-mr-1":  holdGateMR("hold"),
		"gt-src-1": {ID: "gt-src-1"},
	}
	findMR := func(mr *beads.Issue, err error) func(int) (*beads.Issue, error) {
		return func(pr int) (*beads.Issue, error) {
			if pr != 42 {
				t.Errorf("looked up PR %d, want 42", pr)
			}
			return mr, err
		}
	}
	gate := func(p refinery.PRProvider, strategy string, find func(int) (*beads.Issue, error)) error {
		cmd := &cobra.Command{}
		cmd.SetErr(&bytes.Buffer{})
		return reportHoldGate(cmd, "sync-branch",
			holdGateForBranchUpdate(p, strategy, "feat/x", "", find, holdGateShow(issues)))
	}

	t.Run("held PR found by number is refused", func(t *testing.T) {
		err := gate(&branchHoldProvider{prNumber: 42}, "pr", findMR(holdGateMR("hold"), nil))
		wantExit(t, err, syncExitHeld)
	})
	t.Run("draft PR is refused", func(t *testing.T) {
		p := &branchHoldProvider{prNumber: 42}
		p.draft = true
		wantExit(t, gate(p, "pr", findMR(nil, nil)), syncExitHeld)
	})
	t.Run("unheld PR proceeds", func(t *testing.T) {
		unheld := map[string]*beads.Issue{"gt-mr-1": holdGateMR(), "gt-src-1": {ID: "gt-src-1"}}
		mr := holdGateMR()
		cmd := &cobra.Command{}
		cmd.SetErr(&bytes.Buffer{})
		err := reportHoldGate(cmd, "sync-branch",
			holdGateForBranchUpdate(&branchHoldProvider{prNumber: 42}, "pr", "feat/x", "", findMR(mr, nil), holdGateShow(unheld)))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
	t.Run("a failed MR lookup fails closed", func(t *testing.T) {
		err := gate(&branchHoldProvider{prNumber: 42}, "pr", findMR(nil, errors.New("dolt unreachable")))
		wantExit(t, err, syncExitRefused)
	})
	t.Run("no PR yet and no MR: nothing to check", func(t *testing.T) {
		never := func(int) (*beads.Issue, error) {
			t.Error("MR lookup ran with no PR number")
			return nil, nil
		}
		if err := gate(&branchHoldProvider{}, "pr", never); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	})
	t.Run("PR lookup failure fails closed", func(t *testing.T) {
		err := gate(&branchHoldProvider{findErr: errors.New("gh down")}, "pr", findMR(nil, nil))
		wantExit(t, err, syncExitRefused)
	})
}
