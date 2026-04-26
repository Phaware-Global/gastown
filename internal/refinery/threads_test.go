package refinery

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// threadsFakeProvider is a focused fake for the PRProvider surface
// VerifyReviewThreadsResolved touches. Reuses the pattern from
// approval_test.go's fakePRProvider but scoped to thread state —
// IsPRApprovedBy / CountApprovals / etc. panic here because this test
// file only exercises the threads path.
type threadsFakeProvider struct {
	threads    []ReviewThread
	threadsErr error
}

func (p *threadsFakeProvider) UnresolvedThreads(prNumber int) ([]ReviewThread, error) {
	if p.threadsErr != nil {
		return nil, p.threadsErr
	}
	return p.threads, nil
}

// Unused PRProvider surface — panic if exercised.
func (p *threadsFakeProvider) FindPRNumber(string) (int, error)              { panic("unused") }
func (p *threadsFakeProvider) IsPRApproved(int) (bool, error)                { panic("unused") }
func (p *threadsFakeProvider) IsPRApprovedBy(int, string) (bool, error)      { panic("unused") }
func (p *threadsFakeProvider) MergePR(int, string) (string, error)           { panic("unused") }
func (p *threadsFakeProvider) CreatePR(CreatePROptions) (int, string, error) { panic("unused") }
func (p *threadsFakeProvider) RequestReview(int, []string) error             { panic("unused") }
func (p *threadsFakeProvider) AllThreads(int) ([]ReviewThread, error)        { panic("unused") }
func (p *threadsFakeProvider) CountApprovals(int) (int, error)               { panic("unused") }
func (p *threadsFakeProvider) ChecksRollup(int) (string, bool, error)        { panic("unused") }

func TestVerifyReviewThreadsResolved_Empty_ReturnsNil(t *testing.T) {
	provider := &threadsFakeProvider{threads: nil}
	if err := VerifyReviewThreadsResolved(provider, 42, nil); err != nil {
		t.Fatalf("expected nil for empty-threads PR, got %v", err)
	}
}

func TestVerifyReviewThreadsResolved_AllResolved_ReturnsNil(t *testing.T) {
	// Threads that are already Resolved don't block merge.
	provider := &threadsFakeProvider{threads: []ReviewThread{
		{ID: "1", IsResolved: true, Author: "gemini-code-assist", Body: "fix this"},
		{ID: "2", IsResolved: true, Author: "augmentcode", Body: "and this"},
	}}
	if err := VerifyReviewThreadsResolved(provider, 42, nil); err != nil {
		t.Fatalf("expected nil when all threads resolved, got %v", err)
	}
}

func TestVerifyReviewThreadsResolved_OutdatedSkipped(t *testing.T) {
	// Outdated threads annotate code that's been replaced — they do
	// NOT count as blocking.
	provider := &threadsFakeProvider{threads: []ReviewThread{
		{ID: "1", IsResolved: false, IsOutdated: true, Author: "gemini-code-assist",
			Body: "edge case in old impl"},
	}}
	if err := VerifyReviewThreadsResolved(provider, 42, nil); err != nil {
		t.Fatalf("expected nil when all unresolved threads are outdated, got %v", err)
	}
}

func TestVerifyReviewThreadsResolved_Unresolved_ReturnsNeedsResolution(t *testing.T) {
	provider := &threadsFakeProvider{threads: []ReviewThread{
		{
			ID:     "t1",
			URL:    "https://github.com/o/r/pull/42#discussion_r1",
			Author: "gemini-code-assist",
			Path:   "internal/foo/bar.go",
			Line:   123,
			Body:   "![high](https://www.gstatic.com/codereviewagent/high-priority.svg)\n\nThe loop bound is off-by-one.",
		},
	}}
	err := VerifyReviewThreadsResolved(provider, 42, nil)
	if err == nil {
		t.Fatal("expected NeedsReviewResolutionError, got nil")
	}
	var needs *NeedsReviewResolutionError
	if !errors.As(err, &needs) {
		t.Fatalf("expected *NeedsReviewResolutionError, got %T: %v", err, err)
	}
	if needs.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", needs.PRNumber)
	}
	if len(needs.Threads) != 1 {
		t.Fatalf("expected 1 blocking thread, got %d", len(needs.Threads))
	}
	th := needs.Threads[0]
	if th.Priority != "high" {
		t.Errorf("expected priority=high parsed from gemini shield, got %q", th.Priority)
	}
	if th.Path != "internal/foo/bar.go" || th.Line != 123 {
		t.Errorf("expected path+line preserved, got %s:%d", th.Path, th.Line)
	}
	if !strings.Contains(th.Preview, "off-by-one") {
		t.Errorf("expected preview to contain thread body, got %q", th.Preview)
	}
	// Error message surfaces thread count + author + location so the
	// refinery LLM can read it and know what to fix.
	msg := err.Error()
	if !strings.Contains(msg, "PR #42 has 1 unresolved") {
		t.Errorf("error should announce PR + count, got %q", msg)
	}
	if !strings.Contains(msg, "gemini-code-assist") {
		t.Errorf("error should name the author, got %q", msg)
	}
	if !strings.Contains(msg, "bar.go:123") {
		t.Errorf("error should show file:line, got %q", msg)
	}
}

func TestVerifyReviewThreadsResolved_MixedResolvedAndUnresolved(t *testing.T) {
	// Only the unresolved non-outdated thread blocks.
	provider := &threadsFakeProvider{threads: []ReviewThread{
		{ID: "1", IsResolved: true, Author: "gemini-code-assist", Body: "already fixed"},
		{ID: "2", IsResolved: false, IsOutdated: true, Author: "augmentcode",
			Body: "annotated dead code"},
		{ID: "3", IsResolved: false, IsOutdated: false, Author: "augmentcode",
			Path: "pkg/x.go", Line: 7,
			Body: "**Severity: medium**\n\nPotential nil deref"},
	}}
	err := VerifyReviewThreadsResolved(provider, 99, nil)
	var needs *NeedsReviewResolutionError
	if !errors.As(err, &needs) {
		t.Fatalf("expected NeedsReviewResolutionError, got %v", err)
	}
	if len(needs.Threads) != 1 {
		t.Fatalf("expected 1 blocking thread (filtered), got %d", len(needs.Threads))
	}
	if needs.Threads[0].Priority != "medium" {
		t.Errorf("expected augment severity parsed, got %q", needs.Threads[0].Priority)
	}
}

func TestVerifyReviewThreadsResolved_LookupError_IsNotNeedsResolution(t *testing.T) {
	// Provider-lookup failures must NOT present as NeedsResolution —
	// the distinction is load-bearing: NeedsResolution means "review
	// loop must run", a plain error means "tooling broken, escalate".
	provider := &threadsFakeProvider{threadsErr: fmt.Errorf("graphql timeout")}
	err := VerifyReviewThreadsResolved(provider, 42, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var needs *NeedsReviewResolutionError
	if errors.As(err, &needs) {
		t.Errorf("lookup error wrongly reported as NeedsReviewResolutionError: %v", err)
	}
}

func TestVerifyReviewThreadsResolved_NilProvider_ReturnsError(t *testing.T) {
	if err := VerifyReviewThreadsResolved(nil, 42, nil); err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestVerifyReviewThreadsResolved_OutputWriter_EmitsSummary(t *testing.T) {
	provider := &threadsFakeProvider{threads: []ReviewThread{
		{ID: "1", IsResolved: true, Body: "x"},
	}}
	var out bytes.Buffer
	if err := VerifyReviewThreadsResolved(provider, 42, &out); err != nil {
		t.Fatalf("unexpected err %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "[Engineer]") || !strings.Contains(got, "0 unresolved") {
		t.Errorf("expected 0-unresolved summary, got %q", got)
	}
}

func TestParseThreadPriority(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"gemini high shield",
			"![high](https://www.gstatic.com/codereviewagent/high-priority.svg)\n\nDo the thing.",
			"high",
		},
		{
			"gemini medium shield",
			"![medium](https://www.gstatic.com/codereviewagent/medium-priority.svg)\n\nNit.",
			"medium",
		},
		{
			"gemini low shield",
			"![low](https://www.gstatic.com/codereviewagent/low-priority.svg)\nObservation.",
			"low",
		},
		{
			"augment severity medium",
			"The thing is wrong.\n\n**Severity: medium**\n\n[Fix This in Augment]",
			"medium",
		},
		{
			"augment severity high plain",
			"Severity: high\nwhatever",
			"high",
		},
		{
			"no priority marker",
			"Just a plain comment with no shield.",
			"",
		},
		{
			"priority mentioned in prose (not a shield)",
			"This is a high priority fix",
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseThreadPriority(tc.body)
			if got != tc.want {
				t.Errorf("parseThreadPriority(%q) = %q; want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestFirstLinePreview(t *testing.T) {
	tests := []struct {
		name string
		body string
		max  int
		want string
	}{
		{
			"plain first line",
			"Short comment.",
			120,
			"Short comment.",
		},
		{
			"skip image-shield first line",
			"![high](https://g.gstatic.com/x.svg)\n\nActual content here.",
			120,
			"Actual content here.",
		},
		{
			"truncate long line",
			strings.Repeat("a", 200),
			50,
			strings.Repeat("a", 50) + "…",
		},
		{
			"empty body",
			"",
			120,
			"",
		},
		{
			"leading whitespace trimmed",
			"   indented comment",
			120,
			"indented comment",
		},
		{
			// Multi-byte UTF-8 (each emoji = 4 bytes, each accented char = 2 bytes)
			// must be sliced on rune boundaries, not byte boundaries.
			"truncate at rune boundary, not byte",
			"héllo wörld 🎯 with extra trailing text",
			15,
			"héllo wörld 🎯 w" + "…",
		},
		{
			// Augmentcode bodies start with a bold severity header that
			// carries no actionable content. The preview must skip past
			// it to the prose, otherwise the refinery LLM and CLI just
			// see the priority label (which is already extracted via
			// parseThreadPriority).
			"skip augmentcode severity header (bold)",
			"**Severity: medium**\n\nThe new control-flow branch lacks a regression test.",
			120,
			"The new control-flow branch lacks a regression test.",
		},
		{
			// Plain (non-bold) severity form is also possible.
			"skip augmentcode severity header (plain)",
			"Severity: low\n\nMinor nit on the error wrap message.",
			120,
			"Minor nit on the error wrap message.",
		},
		{
			// Combined preamble: image-shield priority + augmentcode
			// severity (occasionally happens when a thread is double-
			// reviewed). Both must be skipped.
			"skip both image-shield and severity preamble",
			"![high](https://www.gstatic.com/codereviewagent/high-priority.svg)\n\n**Severity: high**\n\nUTF-8 byte slicing breaks multi-byte chars.",
			120,
			"UTF-8 byte slicing breaks multi-byte chars.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := firstLinePreview(tc.body, tc.max)
			if got != tc.want {
				t.Errorf("firstLinePreview(%q, %d) = %q; want %q", tc.body, tc.max, got, tc.want)
			}
		})
	}
}
