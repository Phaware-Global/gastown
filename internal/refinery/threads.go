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
// thread bodies. Reviewer bots (and the in-town Reviewer role) inline a
// priority shield at the top of the comment — e.g.:
//
//	![high](https://www.gstatic.com/codereviewagent/high-priority.svg)   (legacy gemini gstatic)
//	![high](https://img.shields.io/badge/priority-high-red.svg)          (shields.io, gt reviewer)
//	**Severity: medium**                                                 (legacy augmentcode)
//
// The shield form is matched by `![<priority>]` plus a badge URL that
// contains both "priority" and ".svg" — NOT the contiguous substring
// "priority.svg", which only the legacy gstatic form satisfies. This
// widening (P23-2376) lets the neutral shields.io badge emitted by
// `gt reviewer post` parse while keeping the legacy gstatic and augmentcode
// forms accepted so interim external bots coexist during migration.
//
// Returns "high", "medium", "low", or "" (unknown / absent). Case
// is normalized to lowercase.
func parseThreadPriority(body string) string {
	lower := strings.ToLower(body)
	// Shield form: a markdown image `![<priority>](<url>)` whose URL contains
	// both "priority" and ".svg". Anchoring the check to the URL inside the
	// image syntax (rather than scanning the whole body for the three
	// substrings independently) avoids false positives where a body merely
	// mentions a priority word, has a "![high]" alt-text snippet, and contains
	// an unrelated ".svg" somewhere. Covers the gstatic, shields.io, and
	// gt-reviewer badge URLs.
	for _, p := range []string{"high", "medium", "low"} {
		marker := "![" + p + "]("
		rest := lower
		for {
			idx := strings.Index(rest, marker)
			if idx == -1 {
				break
			}
			rest = rest[idx+len(marker):]
			closeIdx := strings.IndexByte(rest, ')')
			if closeIdx == -1 {
				break
			}
			url := rest[:closeIdx]
			if strings.Contains(url, "priority") && strings.Contains(url, ".svg") {
				return p
			}
			rest = rest[closeIdx+1:]
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

// priorityBadgeColor maps a priority to its shields.io badge color.
var priorityBadgeColor = map[string]string{
	"high":   "red",
	"medium": "orange",
	"low":    "yellow",
}

// PriorityBadge renders the neutral shields.io priority badge that the in-town
// Reviewer prepends to each finding (P23-2376). It is the emitter half of the
// emitter/parser pair: every badge it produces is recognized by
// parseThreadPriority, and the two share test fixtures so the contract can't
// drift. priority must be "high", "medium", or "low"; any other value yields
// an empty string (no badge).
func PriorityBadge(priority string) string {
	p := strings.ToLower(strings.TrimSpace(priority))
	color, ok := priorityBadgeColor[p]
	if !ok {
		return ""
	}
	return fmt.Sprintf("![%s](https://img.shields.io/badge/priority-%s-%s.svg)", p, p, color)
}

// isSeverityHeaderLine reports whether trimmedLine is solely an
// augmentcode-style severity marker (e.g., `**Severity: medium**` or
// `Severity: low`). Used by firstLinePreview to skip past these
// non-prose preamble lines so the preview surfaces actionable text
// rather than the priority label, which is already extracted by
// parseThreadPriority.
func isSeverityHeaderLine(trimmedLine string) bool {
	lower := strings.ToLower(trimmedLine)
	for _, p := range []string{"high", "medium", "low"} {
		if lower == "severity: "+p || lower == "**severity: "+p+"**" {
			return true
		}
	}
	return false
}

// firstLinePreview returns the first non-empty line of body, trimmed
// of whitespace, truncated to max chars with an ellipsis when longer.
// Used to give the refinery LLM enough context to recognize a thread
// without flooding the error message with body text.
func firstLinePreview(body string, max int) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip preamble lines that don't carry the actionable prose:
		//   - empty lines
		//   - image-shield priority markers (gemini-code-assist):
		//     `![high](https://www.gstatic.com/codereviewagent/high-priority.svg)`
		//   - augmentcode severity headers in either bold or plain form:
		//     `**Severity: medium**`, `Severity: low`
		// The actionable sentence usually follows on the next non-skip line.
		if trimmed == "" || strings.HasPrefix(trimmed, "![") {
			continue
		}
		if isSeverityHeaderLine(trimmed) {
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
