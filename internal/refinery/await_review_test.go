package refinery

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// awaitFakeProvider is a focused fake for the PRProvider methods
// AwaitReviewStep touches: PostComment, HasReviewFrom, UnresolvedThreads.
// Each method returns a configured response; the unused PRProvider
// surface panics so misuse fails loudly.
type awaitFakeProvider struct {
	postedComments []string
	postErr        error

	hasReview    bool
	hasReviewErr error

	threads    []ReviewThread
	threadsErr error

	reviewAuthors    []string
	reviewAuthorsErr error
}

func (p *awaitFakeProvider) PostComment(prNumber int, body string) error {
	if p.postErr != nil {
		return p.postErr
	}
	p.postedComments = append(p.postedComments, body)
	return nil
}

func (p *awaitFakeProvider) HasReviewFrom(prNumber int, user string) (bool, error) {
	return p.hasReview, p.hasReviewErr
}

func (p *awaitFakeProvider) ListReviewAuthors(prNumber int) ([]string, error) {
	return p.reviewAuthors, p.reviewAuthorsErr
}

func (p *awaitFakeProvider) UnresolvedThreads(prNumber int) ([]ReviewThread, error) {
	return p.threads, p.threadsErr
}

func (p *awaitFakeProvider) FindPRNumber(string) (int, error)              { panic("unused") }
func (p *awaitFakeProvider) IsPRApproved(int) (bool, error)                { panic("unused") }
func (p *awaitFakeProvider) IsPRApprovedBy(int, string) (bool, error)      { panic("unused") }
func (p *awaitFakeProvider) MergePR(int, string) (string, error)           { panic("unused") }
func (p *awaitFakeProvider) CreatePR(CreatePROptions) (int, string, error) { panic("unused") }
func (p *awaitFakeProvider) RequestReview(int, []string) error             { panic("unused") }
func (p *awaitFakeProvider) AllThreads(int) ([]ReviewThread, error)        { panic("unused") }
func (p *awaitFakeProvider) CountApprovals(int) (int, error)               { panic("unused") }
func (p *awaitFakeProvider) ChecksRollup(int) (string, bool, error)        { panic("unused") }

// fixedClock returns a deterministic time for AwaitReviewStep's
// "Now()" injection. Tests compose past/future timestamps relative to
// this anchor.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func baseInput(now time.Time, overrides ...func(*AwaitReviewStepInput)) AwaitReviewStepInput {
	in := AwaitReviewStepInput{
		Reviewer:       "augment",
		TriggerComment: "augment review",
		MinWait:        5 * time.Minute,
		Timeout:        30 * time.Minute,
		Now:            fixedClock(now),
	}
	for _, o := range overrides {
		o(&in)
	}
	return in
}

func TestAwaitReviewStep_NilProvider_ReturnsError(t *testing.T) {
	_, err := AwaitReviewStep(nil, 42, baseInput(time.Unix(0, 0)))
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestAwaitReviewStep_EmptyReviewer_ReturnsError(t *testing.T) {
	_, err := AwaitReviewStep(&awaitFakeProvider{}, 42, baseInput(time.Unix(0, 0),
		func(in *AwaitReviewStepInput) { in.Reviewer = "" }))
	if err == nil || !strings.Contains(err.Error(), "reviewer must be non-empty") {
		t.Fatalf("expected reviewer-required error, got %v", err)
	}
}

func TestAwaitReviewStep_TimeoutLEMinWait_ReturnsError(t *testing.T) {
	_, err := AwaitReviewStep(&awaitFakeProvider{}, 42, AwaitReviewStepInput{
		Reviewer: "augment",
		MinWait:  5 * time.Minute,
		Timeout:  5 * time.Minute,
	})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout<=min-wait validation error, got %v", err)
	}
}

func TestAwaitReviewStep_FirstCall_PostsTriggerAndReturnsPosted(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	provider := &awaitFakeProvider{}
	res, err := AwaitReviewStep(provider, 42, baseInput(now))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusTriggerPosted {
		t.Errorf("status = %v, want AwaitStatusTriggerPosted", res.Status)
	}
	if !res.NewStartedAt.Equal(now) {
		t.Errorf("NewStartedAt = %v, want %v", res.NewStartedAt, now)
	}
	if len(provider.postedComments) != 1 || provider.postedComments[0] != "augment review" {
		t.Errorf("expected one 'augment review' comment posted, got %v", provider.postedComments)
	}
}

func TestAwaitReviewStep_FirstCall_EmptyTrigger_SkipsPostButRecordsTime(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	provider := &awaitFakeProvider{}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.TriggerComment = "" }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusTriggerPosted {
		t.Errorf("status = %v, want AwaitStatusTriggerPosted", res.Status)
	}
	if len(provider.postedComments) != 0 {
		t.Errorf("expected no comments posted, got %v", provider.postedComments)
	}
	if !res.NewStartedAt.Equal(now) {
		t.Errorf("NewStartedAt = %v, want %v", res.NewStartedAt, now)
	}
	// When no trigger was posted the message must NOT claim "posted trigger";
	// operators diagnosing a non-engaging reviewer bot need to see that
	// the trigger was disabled rather than that a comment failed silently.
	if strings.Contains(res.Message, "posted trigger") {
		t.Errorf("empty-trigger path must not claim 'posted trigger', got: %s", res.Message)
	}
	if !strings.Contains(res.Message, "trigger comment disabled") {
		t.Errorf("expected message to say 'trigger comment disabled', got: %s", res.Message)
	}
}

// TestAwaitReviewStep_FutureStartedAt_ClampsElapsedToZero guards against
// clock skew or a manually-edited MR bead pushing StartedAt into the
// future. A negative elapsed would compare less than MinWait by definition
// and unexpectedly extend (or freeze) the wait window. The function must
// treat future timestamps as "min-wait window starts now" so the patrol
// can't get stuck.
func TestAwaitReviewStep_FutureStartedAt_ClampsElapsedToZero(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	// StartedAt is 1 hour in the future relative to "now". Without the
	// guard, elapsed = -1h, elapsed < MinWait (5m) is true, and the call
	// returns Waiting with a remaining of 1h5m — much longer than the
	// configured min-wait. With the guard, elapsed clamps to 0 and we
	// expect Waiting with remaining ≈ MinWait.
	future := now.Add(1 * time.Hour)
	provider := &awaitFakeProvider{}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = future }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusWaiting {
		t.Fatalf("status = %v, want AwaitStatusWaiting (clamped elapsed=0 < MinWait)", res.Status)
	}
	// MinWait is 5m by baseInput. Remaining must be 5m exactly, NOT >5m
	// (which would prove the guard didn't fire).
	if res.RemainingWait != 5*time.Minute {
		t.Errorf("RemainingWait = %v, want 5m (clamped). A larger value means the "+
			"future-StartedAt guard did not fire and the patrol could get stuck",
			res.RemainingWait)
	}
	// The persisted StartedAt must be corrected to `now` so subsequent
	// patrol cycles don't re-detect the same skew. Without this, every
	// future cycle would clamp elapsed=0 and keep waiting until wall
	// clock passed the future timestamp + MinWait.
	if res.NewStartedAt.IsZero() {
		t.Error("expected NewStartedAt to be set to current time so the bead's StartedAt " +
			"gets corrected — without persistence, every patrol cycle re-hits the same skew")
	}
	if !res.NewStartedAt.Equal(now) {
		t.Errorf("NewStartedAt = %v, want %v (current clock)", res.NewStartedAt, now)
	}
}

// TestAwaitReviewStep_NormalElapsed_NoStartedAtCorrection verifies that
// the future-StartedAt correction is scoped — when elapsed is non-negative,
// AwaitReviewStep must NOT propose a NewStartedAt (which would otherwise
// cause the caller to overwrite a perfectly valid bead timestamp on every
// cycle, losing the original "wait window started at X" record).
func TestAwaitReviewStep_NormalElapsed_NoStartedAtCorrection(t *testing.T) {
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(2 * time.Minute) // 2m in, still inside MinWait=5m
	provider := &awaitFakeProvider{}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.NewStartedAt.IsZero() {
		t.Errorf("NewStartedAt = %v, want zero (only set on first call or future-StartedAt correction)",
			res.NewStartedAt)
	}
}

func TestAwaitReviewStep_TriggerPostError_IsReturned(t *testing.T) {
	provider := &awaitFakeProvider{postErr: fmt.Errorf("gh pr comment exploded")}
	_, err := AwaitReviewStep(provider, 42, baseInput(time.Unix(0, 0)))
	if err == nil || !strings.Contains(err.Error(), "gh pr comment exploded") {
		t.Fatalf("expected wrapped post error, got %v", err)
	}
}

func TestAwaitReviewStep_StillInsideMinWait_ReturnsWaiting(t *testing.T) {
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(2 * time.Minute) // 2m in, MinWait=5m
	provider := &awaitFakeProvider{}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusWaiting {
		t.Errorf("status = %v, want AwaitStatusWaiting", res.Status)
	}
	if res.RemainingWait != 3*time.Minute {
		t.Errorf("RemainingWait = %v, want 3m", res.RemainingWait)
	}
	if len(provider.postedComments) != 0 {
		t.Errorf("min-wait branch should not re-post trigger, got %v", provider.postedComments)
	}
}

func TestAwaitReviewStep_WaitElapsed_ReviewerEngaged_ThreadsClean_Ready(t *testing.T) {
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(6 * time.Minute) // past MinWait
	provider := &awaitFakeProvider{hasReview: true, threads: nil}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusReady {
		t.Errorf("status = %v, want AwaitStatusReady", res.Status)
	}
}

func TestAwaitReviewStep_WaitElapsed_ThreadsUnresolved_NeedsResolution(t *testing.T) {
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(10 * time.Minute)
	provider := &awaitFakeProvider{
		hasReview: true, // even with reviewer engaged, threads block
		threads: []ReviewThread{
			{ID: "1", IsResolved: false, Author: "augmentcode",
				Path: "a.go", Line: 1,
				Body: "**Severity: high**\n\nbroken"},
		},
	}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusNeedsResolution {
		t.Errorf("status = %v, want AwaitStatusNeedsResolution", res.Status)
	}
	if len(res.UnresolvedThreads) != 1 {
		t.Errorf("expected 1 thread surfaced, got %d", len(res.UnresolvedThreads))
	}
	if res.UnresolvedThreads[0].Priority != "high" {
		t.Errorf("expected priority parsed, got %q", res.UnresolvedThreads[0].Priority)
	}
}

func TestAwaitReviewStep_ThreadsBeforeReviewer_StillBlocks(t *testing.T) {
	// Gemini posted threads immediately, augment hasn't engaged yet.
	// Threads-first ordering returns the actionable findings rather
	// than waiting for augment to also show up.
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(10 * time.Minute)
	provider := &awaitFakeProvider{
		hasReview: false,
		threads: []ReviewThread{
			{ID: "1", IsResolved: false, Author: "gemini-code-assist",
				Path: "b.go", Line: 2, Body: "issue"},
		},
	}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusNeedsResolution {
		t.Errorf("status = %v, want AwaitStatusNeedsResolution (threads outweigh reviewer-absent)", res.Status)
	}
}

func TestAwaitReviewStep_WaitElapsed_NoReviewer_NoThreads_Waiting(t *testing.T) {
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(10 * time.Minute) // past min-wait, well within timeout
	provider := &awaitFakeProvider{hasReview: false, threads: nil}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusWaiting {
		t.Errorf("status = %v, want AwaitStatusWaiting", res.Status)
	}
	if res.RemainingWait != 0 {
		t.Errorf("post-min-wait Waiting should have RemainingWait=0, got %v", res.RemainingWait)
	}
}

func TestAwaitReviewStep_TimeoutElapsed_NoReviewer_TimedOut(t *testing.T) {
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(31 * time.Minute) // past Timeout (30m)
	provider := &awaitFakeProvider{hasReview: false, threads: nil}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusTimedOut {
		t.Errorf("status = %v, want AwaitStatusTimedOut", res.Status)
	}
	// No reviews on the PR: message should signal that explicitly so the
	// operator can distinguish "reviewer-bot never ran" from "reviewer-bot
	// ran but my pr_reviewer config is wrong".
	if !strings.Contains(res.Message, "no reviews submitted") {
		t.Errorf("expected 'no reviews submitted' diagnostic in message, got %q", res.Message)
	}
}

// G38: when augment posts reviews under login "augmentcode" but the rig has
// pr_reviewer="augment", HasReviewFrom returns false and the gate times out.
// The timeout message must surface the actual review-authors so the
// misconfiguration is obvious from the patrol log alone, instead of
// manifesting as a silent 30-minute escalation cycle.
func TestAwaitReviewStep_TimeoutElapsed_DiagnosticListsActualReviewAuthors(t *testing.T) {
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(31 * time.Minute)
	provider := &awaitFakeProvider{
		hasReview:     false,
		reviewAuthors: []string{"augmentcode", "gemini-code-assist", "phaware-artie"},
	}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusTimedOut {
		t.Fatalf("status = %v, want AwaitStatusTimedOut", res.Status)
	}
	for _, want := range []string{"augmentcode", "gemini-code-assist", "phaware-artie", "pr_reviewer must match"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("expected message to contain %q, got %q", want, res.Message)
		}
	}
}

// ListReviewAuthors errors (e.g., ErrUnsupported on Bitbucket) must not
// suppress the timeout — the diagnostic is best-effort enrichment, not a
// load-bearing gate.
func TestAwaitReviewStep_TimeoutElapsed_AuthorListErrorFallsBackToBareMessage(t *testing.T) {
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(31 * time.Minute)
	provider := &awaitFakeProvider{
		hasReview:        false,
		reviewAuthorsErr: fmt.Errorf("bitbucket: not supported"),
	}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusTimedOut {
		t.Errorf("status = %v, want AwaitStatusTimedOut", res.Status)
	}
	if strings.Contains(res.Message, "PR has reviews from") || strings.Contains(res.Message, "no reviews submitted") {
		t.Errorf("expected bare timeout message when ListReviewAuthors errors, got %q", res.Message)
	}
}

func TestAwaitReviewStep_TimeoutElapsed_ReviewerEngaged_StillReady(t *testing.T) {
	// Past timeout but reviewer engaged late: advance, do not escalate.
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(45 * time.Minute)
	provider := &awaitFakeProvider{hasReview: true, threads: nil}
	res, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != AwaitStatusReady {
		t.Errorf("status = %v, want AwaitStatusReady (reviewer engaged late)", res.Status)
	}
}

func TestAwaitReviewStep_HasReviewLookupError_Bubbles(t *testing.T) {
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(10 * time.Minute)
	provider := &awaitFakeProvider{hasReviewErr: fmt.Errorf("graphql down")}
	_, err := AwaitReviewStep(provider, 42, baseInput(now,
		func(in *AwaitReviewStepInput) { in.StartedAt = started }))
	if err == nil || !strings.Contains(err.Error(), "graphql down") {
		t.Fatalf("expected wrapped lookup error, got %v", err)
	}
	var to *AwaitReviewTimeoutError
	if errors.As(err, &to) {
		t.Errorf("lookup error wrongly classified as AwaitReviewTimeoutError: %v", err)
	}
}

func TestAwaitReviewStep_PureGivenInputs(t *testing.T) {
	// Two calls with the same StartedAt produce the same result. The
	// CLI/formula owns persistence; this function is pure given
	// (provider, prNumber, in).
	started := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	now := started.Add(10 * time.Minute)
	provider := &awaitFakeProvider{hasReview: true, threads: nil}
	in := baseInput(now, func(i *AwaitReviewStepInput) { i.StartedAt = started })

	r1, err1 := AwaitReviewStep(provider, 42, in)
	r2, err2 := AwaitReviewStep(provider, 42, in)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected errs: %v, %v", err1, err2)
	}
	if r1.Status != r2.Status {
		t.Errorf("results differ across pure calls: %v vs %v", r1.Status, r2.Status)
	}
	if len(provider.postedComments) != 0 {
		t.Errorf("post-StartedAt path must not re-post trigger, got %v", provider.postedComments)
	}
}

func TestDefaultMergeQueueConfig_SeedsAwaitReviewDefaults(t *testing.T) {
	cfg := DefaultMergeQueueConfig()
	if cfg.PRReviewWait != DefaultPRReviewWait {
		t.Errorf("PRReviewWait = %v, want %v", cfg.PRReviewWait, DefaultPRReviewWait)
	}
	if cfg.PRReviewTimeout != DefaultPRReviewTimeout {
		t.Errorf("PRReviewTimeout = %v, want %v", cfg.PRReviewTimeout, DefaultPRReviewTimeout)
	}
	if cfg.PRTriggerComment != DefaultPRTriggerComment {
		t.Errorf("PRTriggerComment = %q, want %q", cfg.PRTriggerComment, DefaultPRTriggerComment)
	}
}
