package refinery

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// awaitFakeProvider is a scripted fake for the subset of PRProvider
// AwaitReview actually touches: PostComment, HasReviewFrom,
// UnresolvedThreads. The "script" model (per-call responses) lets tests
// simulate "reviewer shows up on the 3rd poll" without racing real
// goroutines. Other PRProvider methods panic — AwaitReview does not
// call them and should not start to without test coverage.
type awaitFakeProvider struct {
	postedComments []string
	postErr        error

	hasReviewScript []bool  // consumed one per call; past end → last value repeats
	hasReviewErrs   []error // parallel to hasReviewScript; nil on hit = no error
	hasReviewCalls  int

	threadsScript [][]ReviewThread
	threadsErrs   []error
	threadsCalls  int
}

func (p *awaitFakeProvider) PostComment(prNumber int, body string) error {
	if p.postErr != nil {
		return p.postErr
	}
	p.postedComments = append(p.postedComments, body)
	return nil
}

func (p *awaitFakeProvider) HasReviewFrom(prNumber int, user string) (bool, error) {
	i := p.hasReviewCalls
	p.hasReviewCalls++
	if i >= len(p.hasReviewScript) {
		i = len(p.hasReviewScript) - 1
	}
	var err error
	if i >= 0 && i < len(p.hasReviewErrs) {
		err = p.hasReviewErrs[i]
	}
	if i < 0 {
		return false, err
	}
	return p.hasReviewScript[i], err
}

func (p *awaitFakeProvider) UnresolvedThreads(prNumber int) ([]ReviewThread, error) {
	i := p.threadsCalls
	p.threadsCalls++
	if i >= len(p.threadsScript) {
		i = len(p.threadsScript) - 1
	}
	var err error
	if i >= 0 && i < len(p.threadsErrs) {
		err = p.threadsErrs[i]
	}
	if i < 0 {
		return nil, err
	}
	return p.threadsScript[i], err
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

// fakeClock is a deterministic clock that advances only through sleep
// calls. Lets AwaitReview's "now()" and the timeout deadline walk in
// lockstep with each injected sleep.
type fakeClock struct {
	now    time.Time
	slept  []time.Duration
	sleeps int
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(d time.Duration) {
	c.slept = append(c.slept, d)
	c.sleeps++
	c.now = c.now.Add(d)
}

func testOpts(clock *fakeClock, overrides ...func(*AwaitReviewOptions)) AwaitReviewOptions {
	// A permissive default: short wait, short poll, enough timeout to
	// let the poll loop run a few times without tripping. Individual
	// tests override.
	opts := AwaitReviewOptions{
		Reviewer:       "augment",
		TriggerComment: "augment review",
		MinWait:        1 * time.Second,
		PollInterval:   1 * time.Second,
		Timeout:        1 * time.Minute,
		Sleep:          clock.Sleep,
		Now:            clock.Now,
	}
	for _, o := range overrides {
		o(&opts)
	}
	return opts
}

func TestAwaitReview_NilProvider_ReturnsError(t *testing.T) {
	err := AwaitReview(nil, 42, AwaitReviewOptions{Reviewer: "augment"}, nil)
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestAwaitReview_EmptyReviewer_ReturnsError(t *testing.T) {
	err := AwaitReview(&awaitFakeProvider{}, 42, AwaitReviewOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "reviewer must be non-empty") {
		t.Fatalf("expected reviewer-required error, got %v", err)
	}
}

func TestAwaitReview_TimeoutLEMinWait_ReturnsError(t *testing.T) {
	// Pathological config; catching it here prevents an infinite-deadline loop.
	err := AwaitReview(&awaitFakeProvider{}, 42, AwaitReviewOptions{
		Reviewer:     "augment",
		MinWait:      5 * time.Minute,
		PollInterval: 30 * time.Second,
		Timeout:      5 * time.Minute,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout<=min-wait validation error, got %v", err)
	}
}

func TestAwaitReview_PostsTriggerComment(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		hasReviewScript: []bool{true},
		threadsScript:   [][]ReviewThread{nil},
	}
	err := AwaitReview(provider, 42, testOpts(clock), nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(provider.postedComments) != 1 || provider.postedComments[0] != "augment review" {
		t.Errorf("expected one 'augment review' comment, got %v", provider.postedComments)
	}
}

func TestAwaitReview_SkipsTriggerWhenEmpty(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		hasReviewScript: []bool{true},
		threadsScript:   [][]ReviewThread{nil},
	}
	err := AwaitReview(provider, 42, testOpts(clock, func(o *AwaitReviewOptions) {
		o.TriggerComment = ""
	}), nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(provider.postedComments) != 0 {
		t.Errorf("expected no comments posted, got %v", provider.postedComments)
	}
}

func TestAwaitReview_SleepsMinWaitBeforeFirstCheck(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		hasReviewScript: []bool{true},
		threadsScript:   [][]ReviewThread{nil},
	}
	err := AwaitReview(provider, 42, testOpts(clock, func(o *AwaitReviewOptions) {
		o.MinWait = 5 * time.Minute
		o.Timeout = 30 * time.Minute
	}), nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(clock.slept) < 1 || clock.slept[0] != 5*time.Minute {
		t.Errorf("expected first sleep to be MinWait=5m, got %v", clock.slept)
	}
	// With reviewer==true on first check and no threads, we should NOT poll-sleep again.
	if len(clock.slept) != 1 {
		t.Errorf("expected exactly one sleep (min-wait only), got %v", clock.slept)
	}
}

func TestAwaitReview_ReturnsNil_WhenReviewAndThreadsClear(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		hasReviewScript: []bool{true},
		threadsScript:   [][]ReviewThread{nil},
	}
	if err := AwaitReview(provider, 42, testOpts(clock), nil); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestAwaitReview_PollsUntilReviewLands(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		// Simulate augment showing up on the 3rd poll.
		hasReviewScript: []bool{false, false, true},
		threadsScript:   [][]ReviewThread{nil, nil, nil},
	}
	err := AwaitReview(provider, 42, testOpts(clock, func(o *AwaitReviewOptions) {
		o.MinWait = 0
		o.PollInterval = 10 * time.Second
		o.Timeout = 10 * time.Minute
	}), nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if provider.hasReviewCalls != 3 {
		t.Errorf("expected 3 HasReviewFrom calls, got %d", provider.hasReviewCalls)
	}
	// Two poll sleeps between the three checks.
	if len(clock.slept) != 2 {
		t.Errorf("expected 2 poll-interval sleeps, got %v", clock.slept)
	}
}

func TestAwaitReview_ReturnsNeedsResolution_WhenThreadsUnresolved(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		// Reviewer shows up, but with blocking threads — the caller must
		// run the review-fix loop, NOT continue to merge.
		hasReviewScript: []bool{true},
		threadsScript: [][]ReviewThread{{
			{ID: "1", IsResolved: false, Author: "augmentcode",
				Path: "a.go", Line: 1,
				Body: "**Severity: high**\n\nbroken"},
		}},
	}
	err := AwaitReview(provider, 42, testOpts(clock), nil)
	if err == nil {
		t.Fatal("expected NeedsReviewResolutionError, got nil")
	}
	var needs *NeedsReviewResolutionError
	if !errors.As(err, &needs) {
		t.Fatalf("expected *NeedsReviewResolutionError, got %T: %v", err, err)
	}
	if needs.PRNumber != 42 || len(needs.Threads) != 1 {
		t.Errorf("expected 1 thread on PR 42, got PR %d with %d threads",
			needs.PRNumber, len(needs.Threads))
	}
}

func TestAwaitReview_ThreadsUnresolvedBeforeReview_StillBlocks(t *testing.T) {
	// Scenario: gemini-code-assist auto-reviews immediately and leaves
	// blocking threads; augment hasn't engaged yet. The threads-first
	// ordering should surface the actionable findings rather than
	// waiting for augment to also show up.
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		hasReviewScript: []bool{false},
		threadsScript: [][]ReviewThread{{
			{ID: "1", IsResolved: false, Author: "gemini-code-assist",
				Path: "b.go", Line: 2, Body: "issue"},
		}},
	}
	err := AwaitReview(provider, 42, testOpts(clock), nil)
	var needs *NeedsReviewResolutionError
	if !errors.As(err, &needs) {
		t.Fatalf("expected NeedsReviewResolutionError, got %v", err)
	}
	// Must NOT have been reported as a timeout — threads take priority.
	var to *AwaitReviewTimeoutError
	if errors.As(err, &to) {
		t.Errorf("did not expect timeout error when threads are actionable: %v", err)
	}
}

func TestAwaitReview_TimesOut_WhenNoReviewAndNoThreads(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		// Reviewer never shows up; threads stay empty.
		hasReviewScript: []bool{false},
		threadsScript:   [][]ReviewThread{nil},
	}
	err := AwaitReview(provider, 42, testOpts(clock, func(o *AwaitReviewOptions) {
		o.MinWait = 0
		o.PollInterval = 30 * time.Second
		o.Timeout = 2 * time.Minute
	}), nil)
	if err == nil {
		t.Fatal("expected AwaitReviewTimeoutError, got nil")
	}
	var to *AwaitReviewTimeoutError
	if !errors.As(err, &to) {
		t.Fatalf("expected *AwaitReviewTimeoutError, got %T: %v", err, err)
	}
	if to.PRNumber != 42 || to.Reviewer != "augment" {
		t.Errorf("timeout error missing PRNumber/Reviewer: %+v", to)
	}
}

func TestAwaitReview_TriggerPostError_IsReturned(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		postErr: fmt.Errorf("gh pr comment exploded"),
	}
	err := AwaitReview(provider, 42, testOpts(clock), nil)
	if err == nil || !strings.Contains(err.Error(), "gh pr comment exploded") {
		t.Fatalf("expected wrapped post error, got %v", err)
	}
}

func TestAwaitReview_HasReviewLookupError_Bubbles(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		hasReviewScript: []bool{false},
		hasReviewErrs:   []error{fmt.Errorf("graphql down")},
		threadsScript:   [][]ReviewThread{nil},
	}
	err := AwaitReview(provider, 42, testOpts(clock), nil)
	if err == nil || !strings.Contains(err.Error(), "graphql down") {
		t.Fatalf("expected wrapped lookup error, got %v", err)
	}
	// Lookup error must NOT present as NeedsReviewResolutionError or TimeoutError.
	var needs *NeedsReviewResolutionError
	var to *AwaitReviewTimeoutError
	if errors.As(err, &needs) || errors.As(err, &to) {
		t.Errorf("lookup error wrongly classified: %v", err)
	}
}

func TestAwaitReview_OutputWriter_EmitsProgress(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	provider := &awaitFakeProvider{
		hasReviewScript: []bool{true},
		threadsScript:   [][]ReviewThread{nil},
	}
	var out bytes.Buffer
	if err := AwaitReview(provider, 42, testOpts(clock), &out); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "posting review trigger") {
		t.Errorf("expected trigger-post progress line, got %q", got)
	}
	if !strings.Contains(got, "min-wait") {
		t.Errorf("expected min-wait progress line, got %q", got)
	}
	if !strings.Contains(got, "has reviewed") {
		t.Errorf("expected success line, got %q", got)
	}
}

func TestDefaultMergeQueueConfig_SeedsAwaitReviewDefaults(t *testing.T) {
	cfg := DefaultMergeQueueConfig()
	if cfg.PRReviewWait != DefaultPRReviewWait {
		t.Errorf("PRReviewWait = %v, want %v", cfg.PRReviewWait, DefaultPRReviewWait)
	}
	if cfg.PRReviewPollInterval != DefaultPRReviewPollInterval {
		t.Errorf("PRReviewPollInterval = %v, want %v",
			cfg.PRReviewPollInterval, DefaultPRReviewPollInterval)
	}
	if cfg.PRReviewTimeout != DefaultPRReviewTimeout {
		t.Errorf("PRReviewTimeout = %v, want %v", cfg.PRReviewTimeout, DefaultPRReviewTimeout)
	}
	if cfg.PRTriggerComment != DefaultPRTriggerComment {
		t.Errorf("PRTriggerComment = %q, want %q", cfg.PRTriggerComment, DefaultPRTriggerComment)
	}
}
