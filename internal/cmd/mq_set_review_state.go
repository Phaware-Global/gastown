package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

// mq_set_review_state.go: write the review-fix loop state fields on an MR bead
// (review_loop_iter, review_fix_polecat). Called by the refinery patrol formula
// at Step PR.5 to record dispatch state so the loop is patrol-resumable — each
// cycle reads current state, takes one action, writes new state, returns to
// patrol.

var (
	mqSetReviewStatePolecat      string
	mqSetReviewStateIter         int
	mqSetReviewStateClearPolecat bool
	mqSetReviewStateClearIter    bool
)

var mqSetReviewStateCmd = &cobra.Command{
	Use:   "set-review-state <mr-id>",
	Short: "Update review-fix loop state fields on an MR bead (merge_strategy=pr)",
	Long: `Update the patrol-resumable review-fix loop state on an MR bead.

The refinery patrol formula calls this at Step PR.5 to record which review-fix
polecat is currently dispatched and how many iterations have run. State lives
on the MR bead's description so each patrol cycle can read it, take one action,
and return control to loop-check — no blocking wait inside the formula.

Fields:
  review_fix_polecat  — name of the currently-dispatched polecat, or empty
                        when no polecat is in flight.
  review_loop_iter    — number of review-fix dispatches already made for this PR.

Examples:
  # Record a fresh dispatch:
  gt mq set-review-state gt-mr-abc --polecat furiosa-42 --iter 2

  # Clear the in-flight polecat after it terminates:
  gt mq set-review-state gt-mr-abc --clear-polecat

  # Reset both (e.g., on MR recovery):
  gt mq set-review-state gt-mr-abc --clear-polecat --clear-iter

Exit codes:
  0 — fields updated successfully.
  Non-zero — MR bead not found, or bd update failed.`,
	Args: cobra.ExactArgs(1),
	RunE: runMqSetReviewState,
}

func init() {
	mqSetReviewStateCmd.Flags().StringVar(&mqSetReviewStatePolecat, "polecat", "",
		"Name of the currently-dispatched review-fix polecat (sets review_fix_polecat)")
	mqSetReviewStateCmd.Flags().IntVar(&mqSetReviewStateIter, "iter", -1,
		"Review-fix loop iteration count (sets review_loop_iter); must be ≥0")
	mqSetReviewStateCmd.Flags().BoolVar(&mqSetReviewStateClearPolecat, "clear-polecat", false,
		"Clear review_fix_polecat (mutually exclusive with --polecat)")
	mqSetReviewStateCmd.Flags().BoolVar(&mqSetReviewStateClearIter, "clear-iter", false,
		"Clear review_loop_iter to 0 (mutually exclusive with --iter)")

	mqCmd.AddCommand(mqSetReviewStateCmd)
}

func runMqSetReviewState(cmd *cobra.Command, args []string) error {
	mrID := args[0]

	// The int flag's default is -1 (sentinel for "unset"). Bare default-
	// comparison would silently accept `--iter -5` as "not provided", which
	// is surprising. Read Changed() to distinguish "flag not passed" from
	// "flag passed with a negative value" and reject the latter explicitly.
	iterProvided := cmd.Flags().Changed("iter")
	if iterProvided && mqSetReviewStateIter < 0 {
		return fmt.Errorf("--iter must be ≥0, got %d", mqSetReviewStateIter)
	}

	// Conflicting flag combinations fail fast so the caller finds out
	// immediately rather than after the read-modify-write round trip.
	if mqSetReviewStatePolecat != "" && mqSetReviewStateClearPolecat {
		return fmt.Errorf("--polecat and --clear-polecat are mutually exclusive")
	}
	if iterProvided && mqSetReviewStateClearIter {
		return fmt.Errorf("--iter and --clear-iter are mutually exclusive")
	}
	if mqSetReviewStatePolecat == "" && !mqSetReviewStateClearPolecat &&
		!iterProvided && !mqSetReviewStateClearIter {
		return fmt.Errorf("no-op: set at least one of --polecat/--clear-polecat/--iter/--clear-iter")
	}

	// Resolve the bead directory via routes.jsonl (same helper the sling
	// path uses for cross-rig writes). We don't need a rig name on the
	// command line because the MR prefix routes us to the correct .beads.
	bd := beads.New(resolveBeadDir(mrID))

	// Acquire a per-bead advisory lock so the read-modify-write below is
	// serialized against other writers of the same MR's description
	// (refinery patrol, mq reject, direct `bd update`, etc.). Without
	// this, a concurrent writer's change to another field (e.g.,
	// close_reason) could race with ours and be lost on last-writer-wins.
	unlock, err := bd.LockBead(mrID)
	if err != nil {
		return fmt.Errorf("acquiring bead lock for %s: %w", mrID, err)
	}
	defer unlock()

	issue, err := bd.Show(mrID)
	if err != nil {
		return fmt.Errorf("loading MR bead %s: %w", mrID, err)
	}
	if issue == nil {
		return fmt.Errorf("MR bead %s not found", mrID)
	}

	fields := beads.ParseMRFields(issue)
	if fields == nil {
		// Fresh MR bead without any parsed fields — start with an empty
		// MRFields so we don't lose other content in the description
		// (SetMRFields preserves non-MR lines as "other content").
		fields = &beads.MRFields{}
	}

	switch {
	case mqSetReviewStateClearPolecat:
		fields.ReviewFixPolecat = ""
	case mqSetReviewStatePolecat != "":
		fields.ReviewFixPolecat = mqSetReviewStatePolecat
	}
	switch {
	case mqSetReviewStateClearIter:
		fields.ReviewLoopIter = 0
	case iterProvided:
		// Negative values were rejected above; at this point mqSetReviewStateIter ≥ 0.
		fields.ReviewLoopIter = mqSetReviewStateIter
	}

	newDesc := beads.SetMRFields(issue, fields)
	if err := bd.Update(mrID, beads.UpdateOptions{Description: &newDesc}); err != nil {
		return fmt.Errorf("writing MR bead description: %w", err)
	}

	fmt.Fprintf(os.Stdout, "%s MR %s review-fix state: polecat=%q iter=%d\n",
		style.Bold.Render("✓"), mrID,
		fields.ReviewFixPolecat, fields.ReviewLoopIter)
	return nil
}
