package refinery

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Default durations for AwaitReview. Seeded into DefaultMergeQueueConfig
// so LoadConfig'd rigs pick them up automatically.
const (
	DefaultPRReviewWait     = 5 * time.Minute
	DefaultPRReviewTimeout  = 30 * time.Minute
	DefaultPRTriggerComment = "augment review"
)

// AwaitReviewStatus is the outcome of one AwaitReviewStep call.
// Single-step + status enum is what makes await-review patrol-resumable
// — the formula re-enters this function on every patrol cycle, takes
// at most one action, and returns immediately. No inline sleep.
type AwaitReviewStatus int

const (
	// AwaitStatusTriggerPosted means the trigger was just posted on
	// this call. The caller persists the new StartedAt timestamp and
	// waits for the next patrol cycle.
	AwaitStatusTriggerPosted AwaitReviewStatus = iota

	// AwaitStatusWaiting means the min-wait window has not yet
	// elapsed, OR it has elapsed but the reviewer hasn't engaged yet
	// and we're still inside the timeout. Caller skips this MR for
	// this cycle and re-enters on the next patrol.
	AwaitStatusWaiting

	// AwaitStatusReady means the reviewer has engaged AND no
	// unresolved non-outdated threads remain. The PR can advance to
	// the approval gate.
	AwaitStatusReady

	// AwaitStatusNeedsResolution means unresolved non-outdated threads
	// exist. The caller maps this to the review-fix loop (PR.5).
	AwaitStatusNeedsResolution

	// AwaitStatusTimedOut means total elapsed time has exceeded
	// Timeout AND the reviewer never engaged. Caller escalates.
	AwaitStatusTimedOut
)

// AwaitReviewStepInput carries one patrol cycle's inputs. The caller
// (CLI subcommand) reads the persisted timestamp from the MR bead,
// fills this in, calls AwaitReviewStep, then persists any returned
// timestamp change back to the bead.
type AwaitReviewStepInput struct {
	// Reviewer is the GitHub user whose review must land. Required.
	Reviewer string

	// TriggerComment is the body posted on the FIRST call (when
	// StartedAt is zero). Empty skips the trigger-post step entirely.
	TriggerComment string

	// MinWait is the minimum wall-time delay between trigger-posted
	// and the first reviewer/threads check. Pass 0 in tests.
	MinWait time.Duration

	// Timeout caps total wall time from StartedAt. After this elapses
	// without reviewer engagement, AwaitReviewStep returns
	// AwaitStatusTimedOut. Must exceed MinWait.
	Timeout time.Duration

	// StartedAt is the persisted timestamp from the prior call's
	// AwaitStatusTriggerPosted result. Zero (time.Time{}) means
	// "trigger has not been posted yet — post it now."
	StartedAt time.Time

	// Now overrides time.Now for tests. nil → time.Now.
	Now func() time.Time
}

// AwaitReviewStepResult carries the outcome of one cycle. NewStartedAt
// is non-zero when the caller must persist a timestamp change back to
// the MR bead. UnresolvedThreads is populated only when Status is
// AwaitStatusNeedsResolution.
type AwaitReviewStepResult struct {
	Status            AwaitReviewStatus
	NewStartedAt      time.Time
	UnresolvedThreads []UnresolvedThread

	// Message is a single-line operator-friendly summary suitable for
	// patrol logs. Always populated.
	Message string

	// RemainingWait is non-zero when Status is AwaitStatusWaiting and
	// we're still inside the min-wait window. Zero outside that case.
	RemainingWait time.Duration
}

// AwaitReviewTimeoutError remains as a thin sentinel so existing
// callers that do errors.As(err, *AwaitReviewTimeoutError{}) continue
// to compile. It is constructed from an AwaitStatusTimedOut result
// when the CLI subcommand exits with a wrapped error.
type AwaitReviewTimeoutError struct {
	PRNumber int
	Reviewer string
	Waited   time.Duration
}

func (e *AwaitReviewTimeoutError) Error() string {
	if e == nil {
		return "<nil AwaitReviewTimeoutError>"
	}
	return fmt.Sprintf(
		"timed out after %s waiting for review from %q on PR #%d — escalate to operator",
		e.Waited.Round(time.Second), e.Reviewer, e.PRNumber)
}

// AwaitReviewStep performs one patrol-cycle's worth of work on the
// await-review gate for prNumber and returns the outcome.
//
// State machine (in.StartedAt = "is trigger posted yet?"):
//
//   - StartedAt zero       → post trigger, return AwaitStatusTriggerPosted
//                            with NewStartedAt = Now(). Caller persists.
//   - elapsed < MinWait    → AwaitStatusWaiting (RemainingWait set).
//   - threads unresolved   → AwaitStatusNeedsResolution.
//   - reviewer engaged AND threads clean → AwaitStatusReady.
//   - elapsed >= Timeout AND reviewer absent → AwaitStatusTimedOut.
//   - reviewer absent (within timeout) → AwaitStatusWaiting.
//
// Threads-first ordering: actionable findings beat both the timeout
// and the reviewer-absent branch — a polecat dispatch can fix the
// findings while the reviewer is still engaging on subsequent
// commits.
func AwaitReviewStep(provider PRProvider, prNumber int, in AwaitReviewStepInput) (AwaitReviewStepResult, error) {
	if provider == nil {
		return AwaitReviewStepResult{}, fmt.Errorf("no PR provider configured")
	}
	if in.Reviewer == "" {
		return AwaitReviewStepResult{}, fmt.Errorf("await-review: reviewer must be non-empty")
	}
	if in.Timeout <= 0 {
		return AwaitReviewStepResult{}, fmt.Errorf("await-review: timeout must be positive (got %v)", in.Timeout)
	}
	if in.Timeout <= in.MinWait {
		return AwaitReviewStepResult{}, fmt.Errorf("await-review: timeout (%s) must exceed min-wait (%s)",
			in.Timeout, in.MinWait)
	}
	now := in.Now
	if now == nil {
		now = time.Now
	}

	// First entry: trigger has never been posted. Post it (if the
	// caller wants one), record the timestamp, and bail. The caller
	// returns to the patrol loop; the next cycle re-enters here with
	// StartedAt set.
	if in.StartedAt.IsZero() {
		if in.TriggerComment != "" {
			if err := provider.PostComment(prNumber, in.TriggerComment); err != nil {
				return AwaitReviewStepResult{}, fmt.Errorf("posting review trigger comment: %w", err)
			}
		}
		t := now()
		// Differentiate the message when no trigger was posted (pr_trigger_comment
		// empty / --no-trigger). "Posted trigger %q" is misleading when nothing
		// went out, and operators diagnosing a non-engaging reviewer bot need
		// to know whether the trigger actually fired.
		var msg string
		if in.TriggerComment != "" {
			msg = fmt.Sprintf(
				"PR #%d: posted trigger %q to wake %s; checking again after min-wait %s",
				prNumber, in.TriggerComment, in.Reviewer, in.MinWait.Round(time.Second))
		} else {
			msg = fmt.Sprintf(
				"PR #%d: trigger comment disabled — starting min-wait window of %s for %s to engage",
				prNumber, in.MinWait.Round(time.Second), in.Reviewer)
		}
		return AwaitReviewStepResult{
			Status:       AwaitStatusTriggerPosted,
			NewStartedAt: t,
			Message:      msg,
		}, nil
	}

	// Guard against a StartedAt in the future (clock skew across hosts,
	// or a manually-edited MR bead). A negative `elapsed` would compare
	// less than MinWait by definition and unexpectedly extend the wait
	// window — potentially indefinitely if the skew is large.
	//
	// Local clamping alone is not sufficient: the bead's persisted
	// StartedAt would stay in the future, every subsequent patrol cycle
	// would re-detect the skew, and the patrol would wait until wall
	// clock passes the future timestamp + MinWait. Instead, when we
	// detect a future StartedAt, propose `NewStartedAt = now()` so the
	// caller persists the corrected timestamp, and treat this cycle as
	// if the min-wait window starts now.
	currentTime := now()
	elapsed := currentTime.Sub(in.StartedAt)
	var correctedStartedAt time.Time
	if elapsed < 0 {
		elapsed = 0
		correctedStartedAt = currentTime
	}

	// Still inside the min-wait window — refuse to even check threads
	// or reviewer state. This is the imperative gate.
	if elapsed < in.MinWait {
		remaining := in.MinWait - elapsed
		msg := fmt.Sprintf("PR #%d: %s left in min-wait window before first check",
			prNumber, remaining.Round(time.Second))
		if !correctedStartedAt.IsZero() {
			msg = fmt.Sprintf("PR #%d: StartedAt was in the future (clock skew or manual edit); "+
				"corrected to %s, %s left in min-wait window",
				prNumber, currentTime.Format(time.RFC3339), remaining.Round(time.Second))
		}
		return AwaitReviewStepResult{
			Status:        AwaitStatusWaiting,
			RemainingWait: remaining,
			NewStartedAt:  correctedStartedAt,
			Message:       msg,
		}, nil
	}

	// Threads first: actionable findings outweigh both the timeout
	// branch and the reviewer-absent branch.
	threadsErr := VerifyReviewThreadsResolved(provider, prNumber, nil)
	if threadsErr != nil {
		var needs *NeedsReviewResolutionError
		if errors.As(threadsErr, &needs) {
			return AwaitReviewStepResult{
				Status:            AwaitStatusNeedsResolution,
				UnresolvedThreads: needs.Threads,
				Message: fmt.Sprintf("PR #%d: %d unresolved thread(s) — review-fix loop required",
					prNumber, len(needs.Threads)),
			}, nil
		}
		return AwaitReviewStepResult{}, threadsErr
	}

	hasReview, err := provider.HasReviewFrom(prNumber, in.Reviewer)
	if err != nil {
		return AwaitReviewStepResult{}, fmt.Errorf("checking for review from %s: %w", in.Reviewer, err)
	}

	if hasReview {
		return AwaitReviewStepResult{
			Status: AwaitStatusReady,
			Message: fmt.Sprintf("PR #%d: %s has reviewed, no unresolved threads — ready to advance",
				prNumber, in.Reviewer),
		}, nil
	}

	// Wait elapsed, no threads, no reviewer. Either still inside the
	// polling window (Waiting) or past the timeout (TimedOut).
	if elapsed >= in.Timeout {
		// Enrich the timeout message with the unique review-authors actually
		// observed on the PR. The classic failure mode is a misconfigured
		// pr_reviewer (e.g. set to the trigger keyword "augment" instead of
		// the bot's actual login "augmentcode") — surfacing the observed
		// authors makes the cause self-evident on first timeout rather than
		// after a 30-minute cycle. ErrUnsupported (Bitbucket) and any other
		// provider error fall back to the bare message; the diagnostic is a
		// best-effort enrichment, not a load-bearing piece of the gate.
		tail := ""
		if authors, err := provider.ListReviewAuthors(prNumber); err == nil {
			if len(authors) == 0 {
				tail = " (no reviews submitted on this PR yet)"
			} else {
				tail = fmt.Sprintf(" (PR has reviews from: %s; "+
					"pr_reviewer must match a reviewer's GitHub login)",
					strings.Join(authors, ", "))
			}
		}
		return AwaitReviewStepResult{
			Status: AwaitStatusTimedOut,
			Message: fmt.Sprintf("PR #%d: %s never engaged after %s — escalate%s",
				prNumber, in.Reviewer, elapsed.Round(time.Second), tail),
		}, nil
	}
	return AwaitReviewStepResult{
		Status: AwaitStatusWaiting,
		Message: fmt.Sprintf("PR #%d: no review from %s yet (elapsed=%s, timeout=%s)",
			prNumber, in.Reviewer, elapsed.Round(time.Second), in.Timeout),
	}, nil
}

// EmitAwaitReviewProgress writes a one-line [Engineer] summary of a
// step result to out, matching the shape used by VerifyPRApproval and
// VerifyReviewThreadsResolved. Safe to call with out == nil.
func EmitAwaitReviewProgress(out io.Writer, r AwaitReviewStepResult) {
	if out == nil {
		return
	}
	_, _ = fmt.Fprintf(out, "[Engineer] %s\n", r.Message)
}
