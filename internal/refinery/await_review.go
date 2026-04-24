package refinery

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// Default durations for AwaitReview. Seeded into DefaultMergeQueueConfig
// so LoadConfig'd rigs pick them up automatically.
const (
	DefaultPRReviewWait         = 5 * time.Minute
	DefaultPRReviewPollInterval = 30 * time.Second
	DefaultPRReviewTimeout      = 30 * time.Minute
	DefaultPRTriggerComment     = "augment review"
)

// AwaitReviewOptions carries the runtime inputs for AwaitReview.
// Callers resolve defaults at their own layer (CLI reads rig config;
// tests pass explicit short values) so AwaitReview does not silently
// fill in behavior the caller didn't intend.
type AwaitReviewOptions struct {
	// Reviewer is the GitHub user whose review must land before
	// AwaitReview returns success. Required.
	Reviewer string

	// TriggerComment is the body posted to wake the reviewer bot.
	// Empty skips the trigger-post step entirely — useful in tests and
	// for rigs that trigger the reviewer elsewhere (e.g. via
	// request-review).
	TriggerComment string

	// MinWait is the imperative physical-reality delay — AwaitReview
	// will not check for a review until this much wall time has elapsed
	// since the trigger was posted. Pass 0 in tests to skip.
	MinWait time.Duration

	// PollInterval is how often to re-check for a review + resolved
	// threads after MinWait elapses. Must be > 0.
	PollInterval time.Duration

	// Timeout caps total wall time (MinWait + polling phase). Must be
	// strictly greater than MinWait.
	Timeout time.Duration

	// Sleep is injected for testability. nil falls back to time.Sleep.
	Sleep func(time.Duration)

	// Now is injected for testability. nil falls back to time.Now.
	Now func() time.Time
}

// AwaitReviewTimeoutError is returned when AwaitReview exhausts Timeout
// waiting for the reviewer to engage. Distinct from
// NeedsReviewResolutionError — this one means "the reviewer never
// showed up, escalate," whereas NeedsReviewResolutionError means "the
// reviewer engaged and left threads, run the fix loop."
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

// AwaitReview posts opts.TriggerComment, sleeps opts.MinWait, then
// polls until the reviewer has engaged AND no unresolved threads
// remain, or Timeout elapses.
//
// Return-case ordering is load-bearing: when threads are blocking we
// surface *NeedsReviewResolutionError regardless of timeout so the
// caller gets actionable findings instead of a generic "timed out"
// that hides them. The timeout branch fires only when the reviewer
// never engaged AND threads stayed clean — the "reviewer never showed
// up" escalation case, distinct from the review-fix loop case.
func AwaitReview(provider PRProvider, prNumber int, opts AwaitReviewOptions, out io.Writer) error {
	if provider == nil {
		return fmt.Errorf("no PR provider configured")
	}
	if opts.Reviewer == "" {
		return fmt.Errorf("await-review: reviewer must be non-empty")
	}
	if opts.PollInterval <= 0 {
		return fmt.Errorf("await-review: poll-interval must be positive (got %v)", opts.PollInterval)
	}
	if opts.Timeout <= 0 {
		return fmt.Errorf("await-review: timeout must be positive (got %v)", opts.Timeout)
	}
	if opts.Timeout <= opts.MinWait {
		return fmt.Errorf("await-review: timeout (%s) must exceed min-wait (%s)",
			opts.Timeout, opts.MinWait)
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	start := now()

	if opts.TriggerComment != "" {
		if out != nil {
			_, _ = fmt.Fprintf(out,
				"[Engineer] PR #%d: posting review trigger (%q) to wake %s\n",
				prNumber, opts.TriggerComment, opts.Reviewer)
		}
		if err := provider.PostComment(prNumber, opts.TriggerComment); err != nil {
			return fmt.Errorf("posting review trigger comment: %w", err)
		}
	}

	if opts.MinWait > 0 {
		if out != nil {
			_, _ = fmt.Fprintf(out,
				"[Engineer] PR #%d: min-wait %s before first review check\n",
				prNumber, opts.MinWait.Round(time.Second))
		}
		sleep(opts.MinWait)
	}

	deadline := start.Add(opts.Timeout)
	// Only log "still waiting" on the first poll; subsequent polls are
	// silent until state changes, so a 30-minute wait doesn't scroll 60
	// identical lines past the operator.
	firstPoll := true
	for {
		// Threads first: if unresolved findings exist we can return
		// without even spending an API call on HasReviewFrom, and the
		// caller gets actionable output regardless of reviewer state.
		threadsErr := VerifyReviewThreadsResolved(provider, prNumber, out)
		if threadsErr != nil {
			var needs *NeedsReviewResolutionError
			if errors.As(threadsErr, &needs) {
				return threadsErr
			}
			return threadsErr
		}

		hasReview, err := provider.HasReviewFrom(prNumber, opts.Reviewer)
		if err != nil {
			return fmt.Errorf("checking for review from %s: %w", opts.Reviewer, err)
		}

		if hasReview {
			if out != nil {
				_, _ = fmt.Fprintf(out,
					"[Engineer] PR #%d: %s has reviewed, no unresolved threads — proceeding\n",
					prNumber, opts.Reviewer)
			}
			return nil
		}

		nowT := now()
		remaining := deadline.Sub(nowT)
		if remaining <= 0 {
			return &AwaitReviewTimeoutError{
				PRNumber: prNumber,
				Reviewer: opts.Reviewer,
				Waited:   nowT.Sub(start),
			}
		}
		if firstPoll && out != nil {
			_, _ = fmt.Fprintf(out,
				"[Engineer] PR #%d: no review from %s yet, polling every %s\n",
				prNumber, opts.Reviewer, opts.PollInterval.Round(time.Second))
			firstPoll = false
		}
		// Cap sleep at remaining budget so one last check fires right at
		// the deadline instead of after it.
		wait := opts.PollInterval
		if remaining < wait {
			wait = remaining
		}
		sleep(wait)
	}
}
