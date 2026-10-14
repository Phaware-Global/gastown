package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
)

// refinery_pr_sync.go: the refinery's branch-update primitives. The patrol
// formula must go through these instead of raw "git rebase" / "git push -f", so
// the rig's pr_update_strategy and the approved-PR force-push guard are
// enforced by the binary rather than by formula prose.

var (
	refPrSyncBranch string
	refPrSyncTarget string
	refPrSyncLocal  string

	refPrPushBranch string
	refPrPushLocal  string
)

// Exit codes for "gt refinery pr sync-branch" (and 4 for push-branch).
const (
	syncExitBehindBase = 2 // strategy=none and the branch is behind its target
	syncExitConflict   = 3 // merge/rebase conflicted and was aborted
	syncExitRefused    = 4 // refused (approved PR force-push) or operational error
)

var refineryPrSyncBranchCmd = &cobra.Command{
	Use:   "sync-branch",
	Short: "Bring a behind-base branch up to date per the rig's pr_update_strategy",
	Long: `Bring origin/<branch> up to date with origin/<target> according to the rig's
merge_queue.pr_update_strategy, leaving the result on the local working branch
(--local, default "temp"). Never pushes; use 'gt refinery pr push-branch'.

Strategies:
  rebase (default)  rebase the branch onto the target. Refused when the branch
                    has an open PR with an approving review.
  merge             merge the target into the branch with a merge commit.
  none              change nothing; report that the branch is behind its base.

Exit codes:
  0  up-to-date, or synced onto the local working branch
  2  behind base and strategy=none — report to the mayor, leave the branch alone
  3  conflict (merge/rebase aborted, nothing changed) — create a conflict task
  4  refused or operational error`,
	Args: cobra.NoArgs,
	RunE: runRefineryPrSyncBranch,
}

var refineryPrPushBranchCmd = &cobra.Command{
	Use:   "push-branch",
	Short: "Publish the synced working branch to origin without ever force-pushing an approved PR",
	Long: `Push the local working branch (--local, default "temp") to origin/<branch>.

A fast-forward is pushed plainly. Only pr_update_strategy=rebase may
force-push, with a lease on the tip fetched at sync time, and never when the
branch has an open PR with an approving review. Under strategy=none nothing is
pushed; under strategy=merge a non-fast-forward is an error.

Exit codes:
  0  pushed, or nothing to push
  4  refused or operational error`,
	Args: cobra.NoArgs,
	RunE: runRefineryPrPushBranch,
}

func init() {
	refineryPrCmd.AddCommand(refineryPrSyncBranchCmd)
	refineryPrCmd.AddCommand(refineryPrPushBranchCmd)

	refineryPrSyncBranchCmd.Flags().StringVar(&refPrSyncBranch, "branch", "", "work branch to update (required)")
	refineryPrSyncBranchCmd.Flags().StringVar(&refPrSyncTarget, "target", "", "branch it must contain (required)")
	refineryPrSyncBranchCmd.Flags().StringVar(&refPrSyncLocal, "local", "temp", "local working branch to leave the result on")

	refineryPrPushBranchCmd.Flags().StringVar(&refPrPushBranch, "branch", "", "remote branch to update (required)")
	refineryPrPushBranchCmd.Flags().StringVar(&refPrPushLocal, "local", "temp", "local working branch to push")
}

// noOpenPR is the approval probe for rigs that do not publish branches through
// PRs (merge_strategy != pr): the refinery never force-pushes the work branch
// there, so there is no PR whose approval a rewrite could invalidate.
type noOpenPR struct{}

func (noOpenPR) FindPRNumber(string) (int, error) { return 0, nil }
func (noOpenPR) IsPRApproved(int) (bool, error)   { return false, nil }
func (noOpenPR) CountApprovals(int) (int, error)  { return 0, nil }

// refusePrSync reports a failure that happens before the sync/push itself runs
// (bad flags, no workspace, invalid config) the same way a refused sync is
// reported: on stderr with exit code 4, so the patrol formula's exit-code
// handling covers it instead of seeing an unhandled exit 1.
func refusePrSync(cmd *cobra.Command, name string, err error) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", name, err)
	return NewSilentExit(syncExitRefused)
}

func runRefineryPrSyncBranch(cmd *cobra.Command, _ []string) error {
	if refPrSyncBranch == "" || refPrSyncTarget == "" {
		return refusePrSync(cmd, "sync-branch", errors.New("--branch and --target are required"))
	}
	provider, cfg, _, err := getRefineryPRContext()
	if err != nil {
		return refusePrSync(cmd, "sync-branch", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return refusePrSync(cmd, "sync-branch", err)
	}
	var probe refinery.PRApprovalProbe = provider
	if cfg.MergeStrategy != "pr" {
		probe = noOpenPR{}
	}
	strategy := cfg.GetPRUpdateStrategy()

	res, err := refinery.SyncBranch(git.NewGit(cwd), probe, strategy, refPrSyncBranch, refPrSyncTarget, refPrSyncLocal)
	if err != nil {
		return refusePrSync(cmd, "sync-branch", err)
	}
	out := cmd.OutOrStdout()
	switch res.Outcome {
	case refinery.SyncUpToDate:
		fmt.Fprintf(out, "up-to-date: %s already contains %s (strategy=%s)\n", refPrSyncBranch, refPrSyncTarget, res.Strategy)
	case refinery.SyncSynced:
		fmt.Fprintf(out, "synced: %s updated from %s by %s (%d commit(s)) on local %q — not pushed\n",
			refPrSyncBranch, refPrSyncTarget, res.Strategy, res.Behind, refPrSyncLocal)
	case refinery.SyncBehindBase:
		fmt.Fprintf(out, "behind-base: %s is %d commit(s) behind %s; pr_update_strategy=none, branch left untouched.\n"+
			"Report this to the mayor; do not rebase, merge, or push it.\n", refPrSyncBranch, res.Behind, refPrSyncTarget)
		return NewSilentExit(syncExitBehindBase)
	case refinery.SyncConflict:
		fmt.Fprintf(out, "conflict: %s conflicts with %s under strategy=%s; aborted, nothing changed.\n"+
			"Target %s, branch %s.\n", refPrSyncBranch, refPrSyncTarget, res.Strategy, res.TargetSHA, res.BranchSHA)
		return NewSilentExit(syncExitConflict)
	}
	return nil
}

func runRefineryPrPushBranch(cmd *cobra.Command, _ []string) error {
	if refPrPushBranch == "" {
		return refusePrSync(cmd, "push-branch", errors.New("--branch is required"))
	}
	provider, cfg, _, err := getRefineryPRContext()
	if err != nil {
		return refusePrSync(cmd, "push-branch", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return refusePrSync(cmd, "push-branch", err)
	}
	res, err := refinery.PushBranch(git.NewGit(cwd), provider, cfg.GetPRUpdateStrategy(), refPrPushBranch, refPrPushLocal)
	if err != nil {
		return refusePrSync(cmd, "push-branch", err)
	}
	out := cmd.OutOrStdout()
	switch {
	case !res.Pushed:
		fmt.Fprintf(out, "nothing to push: origin/%s already at %s\n", refPrPushBranch, res.SHA)
	case res.Forced:
		fmt.Fprintf(out, "pushed (force-with-lease): origin/%s -> %s\n", refPrPushBranch, res.SHA)
	default:
		fmt.Fprintf(out, "pushed (fast-forward): origin/%s -> %s\n", refPrPushBranch, res.SHA)
	}
	return nil
}
