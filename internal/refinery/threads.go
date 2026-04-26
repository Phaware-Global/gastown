package refinery

import (
	"fmt"
	"io"
	"strings"
)

// NeedsReviewResolutionError indicates a PR has unresolved, non-outdated
// review threads that must be addressed before merge. Distinct from
// NeedsApprovalError (which is about formal review state) — an
// unresolved thread can exist with or without a corresponding review
// decision.
//
// Callers use errors.As to branch: the refinery patrol path maps this
// to PR.5 (review-fix loop) rather than escalating; the CLI surfaces
// the thread details and exits non-zero.
type NeedsReviewResolutionError struct {
	PRNumber int
	Threads  []UnresolvedThread
}

// UnresolvedThread is the renderer-friendly view of a single unresolved
// thread surfaced by NeedsReviewResolutionError. It captures the fields
// the refinery LLM needs to decide what to fix: who posted, where in
// the code, priority signal if present, and a preview of the body.
type UnresolvedThread struct {
	URL      string
	Author   string
	Path     string // file path the thread is attached to (empty for PR-level)
	Line     int    // line number (0 for PR-level)
	Priority string // "high", "medium", "low" parsed from body; empty if absent
	Preview  string // first line of thread body, truncated to 120 chars
}

func (e *NeedsReviewResolutionError) Error() string {
	if e == nil {
		return "<nil NeedsReviewResolutionError>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PR #%d has %d unresolved review thread(s):\n", e.PRNumber, len(e.Threads))
	for i, t := range e.Threads {
		loc := t.URL
		if t.Path != "" {
			loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
		}
		prio := ""
		if t.Priority != "" {
			prio = fmt.Sprintf("[%s] ", strings.ToUpper(t.Priority))
		}
		fmt.Fprintf(&b, "  %d. %s%s@%s — %s\n", i+1, prio, t.Author, loc, t.Preview)
		if t.URL != "" && t.Path != "" {
			fmt.Fprintf(&b, "     %s\n", t.URL)
		}
	}
	return b.String()
}

// VerifyReviewThreadsResolved calls provider.UnresolvedThreads and
// returns a *NeedsReviewResolutionError listing any threads that are
// still unresolved AND not outdated. Returns nil when the list is
// empty (nothing blocking the merge) or a plain error on lookup
// failure.
//
// Outdated threads (IsOutdated=true) are considered auto-dismissed:
// the code they annotated has been replaced by later pushes and the
// thread no longer applies to current HEAD. GitHub keeps outdated
// threads in the listing for history, but they do not count as
// "addressable" for a gate-checking purpose.
//
// out is optional — when non-nil, emits a one-line [Engineer] summary
// of how many threads are blocking. Matches the logging shape used by
// VerifyPRApproval so patrol output is visually consistent.
func VerifyReviewThreadsResolved(provider PRProvider, prNumber int, out io.Writer) error {
	if provider == nil {
		return fmt.Errorf("no PR provider configured")
	}
	threads, err := provider.UnresolvedThreads(prNumber)
	if err != nil {
		return fmt.Errorf("failed to list unresolved threads on PR #%d: %w", prNumber, err)
	}
	var blocking []UnresolvedThread
	for _, t := range threads {
		if t.IsResolved || t.IsOutdated {
			continue
		}
		blocking = append(blocking, UnresolvedThread{
			URL:      t.URL,
			Author:   t.Author,
			Path:     t.Path,
			Line:     t.Line,
			Priority: parseThreadPriority(t.Body),
			Preview:  firstLinePreview(t.Body, 120),
		})
	}
	if len(blocking) == 0 {
		if out != nil {
			_, _ = fmt.Fprintf(out, "[Engineer] PR #%d has 0 unresolved review threads\n", prNumber)
		}
		return nil
	}
	if out != nil {
		_, _ = fmt.Fprintf(out, "[Engineer] PR #%d has %d unresolved review thread(s) — deferring merge\n",
			prNumber, len(blocking))
	}
	return &NeedsReviewResolutionError{
		PRNumber: prNumber,
		Threads:  blocking,
	}
}

// parseThreadPriority extracts the priority tag embedded in reviewer-bot
// thread bodies. Both gemini-code-assist and augmentcode inline a
// priority shield at the top of the comment — e.g.:
//
//	![high](https://www.gstatic.com/codereviewagent/high-priority.svg)
//	**Severity: medium**
//
// Returns "high", "medium", "low", or "" (unknown / absent). Case
// is normalized to lowercase.
func parseThreadPriority(body string) string {
	lower := strings.ToLower(body)
	// gemini-code-assist shield form
	for _, p := range []string{"high", "medium", "low"} {
		if strings.Contains(lower, "priority.svg") && strings.Contains(lower, "!["+p+"]") {
			return p
		}
	}
	// augmentcode severity form
	for _, p := range []string{"high", "medium", "low"} {
		if strings.Contains(lower, "severity: "+p) || strings.Contains(lower, "**severity: "+p+"**") {
			return p
		}
	}
	return ""
}

// firstLinePreview returns the first non-empty line of body, trimmed
// of whitespace, truncated to max chars with an ellipsis when longer.
// Used to give the refinery LLM enough context to recognize a thread
// without flooding the error message with body text.
func firstLinePreview(body string, max int) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip image-shield lines and empty lines — the interesting
		// content is usually the first prose sentence after them.
		if trimmed == "" || strings.HasPrefix(trimmed, "![") {
			continue
		}
		// Slice on rune boundaries so multi-byte UTF-8 characters (e.g.,
		// emoji or non-ASCII text in reviewer comments) don't get cut
		// mid-codepoint and produce invalid output.
		runes := []rune(trimmed)
		if len(runes) > max {
			return string(runes[:max]) + "…"
		}
		return trimmed
	}
	return ""
}
