package reviewer

import (
	"fmt"
	"strings"
	"time"
)

// EjectOutcome names what should happen to a PR whose review loop is not
// converging.
type EjectOutcome string

const (
	// EjectNone means the loop is still making progress; keep reviewing.
	EjectNone EjectOutcome = ""
	// EjectApproveWithNotes means the PR is fundamentally sound and
	// over-reviewed: blocking findings are gone or trivial, and what remains
	// belongs in follow-up work rather than in another round.
	EjectApproveWithNotes EjectOutcome = "approve_with_notes"
	// EjectDecompose means the PR is carrying more than one change and rounds
	// are not reducing the blocking set. It should be split.
	EjectDecompose EjectOutcome = "decompose"
)

// RoundRecord is one completed review round's outcome.
type RoundRecord struct {
	// Round is the round number this record describes (1-based).
	Round int
	// BlockingThreads is how many unresolved threads the round left open.
	// This, not the finding count, is what the fix loop must drive to zero.
	BlockingThreads int
	// At is when the round was recorded, used only for the age criterion.
	At time.Time
}

// ConvergenceInput is everything Assess needs to judge a PR's review loop.
type ConvergenceInput struct {
	PRNumber int
	// History is the completed rounds, oldest first.
	History []RoundRecord
	// Resets counts how many times an operator cleared review_loop_iter for
	// this PR. Without it the iteration cap is advisory: the cap escalates, an
	// operator clears the counter, and the loop restarts from zero. graphql-api
	// PR #112 reached 23 posted reviews and PR #132 reached 31, both under a
	// configured cap of 3.
	Resets int
	// MaxRounds is the rig's configured review iteration cap.
	MaxRounds int
	// Age is how long the PR has been open with review outstanding.
	Age time.Duration
}

// MaxPRAge is the age past which a still-blocked PR is treated as
// non-converging regardless of its round history.
//
// A PR can sit blocked without accumulating rounds at all — waiting on a
// wedged reviewer, a capacity-starved fix loop, or an operator who never
// cleared an escalation. graphql-api #112, #113, #118 and #120 all passed two
// weeks in that state. Round-count criteria alone never fire on those, so age
// is a separate rail.
const MaxPRAge = 7 * 24 * time.Hour

// EjectDecision is the structured result of a convergence assessment.
type EjectDecision struct {
	Outcome EjectOutcome
	// Reason states which criterion fired, in operator-facing language.
	Reason string
	// BlockingThreads is the latest round's blocking count, carried so the
	// caller can describe the state without re-deriving it.
	BlockingThreads int
}

// Triggered reports whether the loop should stop taking incremental rounds.
func (d EjectDecision) Triggered() bool { return d.Outcome != EjectNone }

// Assess decides whether a PR's review loop has stopped converging, and if so
// which of the two eject outcomes fits.
//
// The loop already had a cap, and the cap had exactly one outcome: escalate to
// a human. That is not a convergence protocol — it is a notification. It says
// the loop ran too long without saying what should happen to the PR, so the PR
// waits for someone to decide, which on the observed PRs meant days. Worse, the
// cap is evadable: clearing review_loop_iter restarts it, so "3 rounds" became
// 23 and 31 in practice.
//
// Non-convergence fires on any of four rails, deliberately independent so a PR
// stuck in an unusual way still trips one:
//
//   - three consecutive rounds with no net reduction in blocking threads — the
//     loop is running but not draining;
//   - rounds past the configured cap;
//   - the cap has been reset at least once, since a reset means the cap already
//     fired and the loop was restarted rather than resolved;
//   - the PR is older than MaxPRAge with blocking threads still open.
//
// The outcome is a RECOMMENDATION carried to a human, not an automatic merge:
// approving a PR is not a decision this should take unilaterally, and the
// distinction between "sound but over-reviewed" and "needs splitting" needs a
// holistic read of the diff that a counter cannot perform. What this does is
// make the choice explicit and put the evidence next to it.
func Assess(in ConvergenceInput) EjectDecision {
	if len(in.History) == 0 {
		return EjectDecision{}
	}
	latest := in.History[len(in.History)-1]

	// Nothing is blocking: the loop converged, whatever the round count.
	if latest.BlockingThreads == 0 {
		return EjectDecision{BlockingThreads: 0}
	}

	decide := func(reason string) EjectDecision {
		return EjectDecision{
			Outcome:         outcomeFor(in.History),
			Reason:          reason,
			BlockingThreads: latest.BlockingThreads,
		}
	}

	if in.Resets > 0 {
		return decide(fmt.Sprintf(
			"the review-loop cap has been cleared %d time(s), so the cap has already "+
				"fired and the loop was restarted rather than resolved", in.Resets))
	}
	if in.MaxRounds > 0 && latest.Round > in.MaxRounds {
		return decide(fmt.Sprintf("round %d is past the configured cap of %d",
			latest.Round, in.MaxRounds))
	}
	if in.Age > MaxPRAge && latest.BlockingThreads > 0 {
		return decide(fmt.Sprintf("open %s with %d blocking thread(s) still unresolved",
			in.Age.Round(24*time.Hour), latest.BlockingThreads))
	}
	if stalled, n := stalledRounds(in.History); stalled {
		return decide(fmt.Sprintf(
			"%d consecutive rounds with no net reduction in blocking threads "+
				"(still %d)", n, latest.BlockingThreads))
	}
	return EjectDecision{BlockingThreads: latest.BlockingThreads}
}

// stalledRoundThreshold is how many consecutive non-reducing rounds count as a
// stall. Two rounds can legitimately fail to reduce the count — a fix round
// that uncovers a genuine second defect looks identical to one that spins — so
// the third is where "not converging" becomes the better explanation.
const stalledRoundThreshold = 3

// stalledRounds reports whether the most recent rounds failed to reduce the
// blocking-thread count, and how many such rounds there were.
//
// Measured on the count LEAVING each round, so a round that closes three
// threads and opens three others reads as no progress — which is exactly what
// it is for the fix loop, and exactly the pattern the round-1 mislabeling
// produced (hga-y1b: passes re-raised findings that already existed as open
// threads).
func stalledRounds(history []RoundRecord) (bool, int) {
	if len(history) < stalledRoundThreshold+1 {
		return false, 0
	}
	tail := history[len(history)-(stalledRoundThreshold+1):]
	// NET reduction across the window, not a pairwise check. A sawtooth — close
	// three threads, open three others, repeat — reduces pairwise on alternate
	// rounds while going nowhere, and that is precisely the pattern to catch.
	if tail[len(tail)-1].BlockingThreads < tail[0].BlockingThreads {
		return false, 0
	}
	return true, stalledRoundThreshold
}

// outcomeFor picks between the two eject outcomes from the shape of the
// history alone.
//
// A blocking set that has shrunk substantially since round 1 describes a PR
// that is fundamentally sound and has been ground down by review — the
// remaining items are the tail, and the right move is to approve with notes and
// file them. A blocking set that has held steady or grown describes a PR whose
// surface keeps producing new defects, which is what a change carrying several
// logical slices looks like from inside a review loop; that one wants splitting.
//
// This is a heuristic on counts and is deliberately only a recommendation: the
// caller presents it to a human alongside the evidence.
func outcomeFor(history []RoundRecord) EjectOutcome {
	first := history[0].BlockingThreads
	latest := history[len(history)-1].BlockingThreads
	if first > 0 && latest*2 <= first {
		return EjectApproveWithNotes
	}
	return EjectDecompose
}

// Describe renders an eject decision as the escalation body a human reads.
// It names the criterion, the recommended outcome, and what each outcome would
// mean, so the decision in front of the operator is a choice between two stated
// options rather than an open-ended "this ran too long".
func (d EjectDecision) Describe(prNumber int) string {
	if !d.Triggered() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PR #%d review loop is not converging: %s.\n\n", prNumber, d.Reason)
	fmt.Fprintf(&b, "%d blocking thread(s) remain. Further incremental rounds are "+
		"unlikely to help; the loop needs a decision, not another pass.\n\n", d.BlockingThreads)
	b.WriteString("Recommended: ")
	switch d.Outcome {
	case EjectApproveWithNotes:
		b.WriteString("APPROVE WITH NOTES.\n" +
			"The blocking set has shrunk substantially since the first round, which is the " +
			"shape of a sound change that has been over-reviewed. File the remaining items " +
			"as follow-up beads, resolve their threads citing those beads, and let the PR merge.\n")
	case EjectDecompose:
		b.WriteString("DECOMPOSE.\n" +
			"The blocking set has not shrunk across rounds, which is the shape of a PR " +
			"carrying more than one logical change. Identify the slice that can merge on its " +
			"own, split the rest into a follow-up PR, and re-review the smaller diff.\n")
	}
	b.WriteString("\nThe alternative outcome remains available; this is a recommendation " +
		"from the round history, not a merge decision.\n")
	return b.String()
}
