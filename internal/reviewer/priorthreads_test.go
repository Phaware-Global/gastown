package reviewer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery"
)

// findingBody builds a thread body in the shape FormatBody emits: a shields.io
// priority badge line, then the perspective-tagged title, then prose. The badge
// line is the reason a naive first-line preview carried no information.
func findingBody(priority, perspective, title, prose string) string {
	return fmt.Sprintf("%s\n**[%s]** %s\n\n%s",
		refinery.PriorityBadge(priority), perspective, title, prose)
}

func TestFormatPriorThreads_Empty(t *testing.T) {
	if got := FormatPriorThreads(nil); got != "" {
		t.Errorf("nil threads = %q, want empty", got)
	}
	if got := FormatPriorThreads([]refinery.ReviewThread{}); got != "" {
		t.Errorf("empty threads = %q, want empty", got)
	}
}

// The bug this guards: every prior-thread line used to render as the same
// shields.io badge URL, because the finding body's first line IS the badge.
func TestFormatPriorThreads_PreviewIsTitleNotBadge(t *testing.T) {
	threads := []refinery.ReviewThread{{
		ID:     "T1",
		Path:   "libs/domain/survey-code.value.ts",
		Line:   37,
		Author: "phaware-val",
		Body: findingBody("high", "domain-driven-design",
			"Fail-closed flip silently gates a write", "Long explanation here."),
	}}

	got := FormatPriorThreads(threads)

	if strings.Contains(got, "shields.io") {
		t.Errorf("preview leaked the priority badge:\n%s", got)
	}
	if !strings.Contains(got, "Fail-closed flip silently gates a write") {
		t.Errorf("preview dropped the finding title:\n%s", got)
	}
	if !strings.Contains(got, "libs/domain/survey-code.value.ts:37") {
		t.Errorf("preview dropped the anchor:\n%s", got)
	}
}

// Resolved threads must appear, marked, or the contract's "do not relitigate
// already-resolved threads" rule has no data to act on.
func TestFormatPriorThreads_IncludesResolved(t *testing.T) {
	threads := []refinery.ReviewThread{
		{ID: "T1", Path: "a.ts", Line: 1, Author: "val", IsResolved: true,
			Body: findingBody("high", "security", "Fixed last round", "x")},
		{ID: "T2", Path: "b.ts", Line: 2, Author: "val", IsResolved: false,
			Body: findingBody("medium", "adversarial", "Still open", "y")},
	}

	got := FormatPriorThreads(threads)

	if !strings.Contains(got, "[RESOLVED] a.ts:1") {
		t.Errorf("resolved thread missing or unmarked:\n%s", got)
	}
	if !strings.Contains(got, "[OPEN] b.ts:2") {
		t.Errorf("open thread missing or unmarked:\n%s", got)
	}
	if !strings.Contains(got, "2 prior thread(s): 1 unresolved, 1 resolved.") {
		t.Errorf("counts line wrong:\n%s", got)
	}
	// Open leads: it is the round's actual worklist.
	if strings.Index(got, "[OPEN] b.ts:2") > strings.Index(got, "[RESOLVED] a.ts:1") {
		t.Errorf("unresolved threads must be listed first:\n%s", got)
	}
}

func TestFormatPriorThreads_MarksOutdated(t *testing.T) {
	threads := []refinery.ReviewThread{{
		ID: "T1", Path: "a.ts", Line: 9, Author: "val", IsOutdated: true,
		Body: findingBody("low", "typescript", "Anchor moved", "z"),
	}}
	if got := FormatPriorThreads(threads); !strings.Contains(got, "[OPEN/OUTDATED]") {
		t.Errorf("outdated thread not marked:\n%s", got)
	}
}

// A payload that reshuffles between dispatches reads as new information to the
// pass and defeats prompt caching, so ordering must be total and stable.
func TestFormatPriorThreads_DeterministicOrder(t *testing.T) {
	mk := func(id, path string, line int) refinery.ReviewThread {
		return refinery.ReviewThread{ID: id, Path: path, Line: line, Author: "val",
			Body: findingBody("medium", "security", "t-"+id, "body")}
	}
	a := []refinery.ReviewThread{mk("T3", "b.ts", 5), mk("T1", "a.ts", 9), mk("T2", "a.ts", 2)}
	b := []refinery.ReviewThread{mk("T2", "a.ts", 2), mk("T3", "b.ts", 5), mk("T1", "a.ts", 9)}

	if FormatPriorThreads(a) != FormatPriorThreads(b) {
		t.Errorf("input order changed the payload:\n--- A ---\n%s\n--- B ---\n%s",
			FormatPriorThreads(a), FormatPriorThreads(b))
	}
	got := FormatPriorThreads(a)
	if strings.Index(got, "a.ts:2") > strings.Index(got, "a.ts:9") {
		t.Errorf("lines not ascending within a path:\n%s", got)
	}
	if strings.Index(got, "a.ts:9") > strings.Index(got, "b.ts:5") {
		t.Errorf("paths not ascending:\n%s", got)
	}
}

// Overflow must be announced, not silent: a pass told nothing would read a
// truncated list as the complete history and re-raise what it cannot see.
func TestFormatPriorThreads_OverflowIsAnnounced(t *testing.T) {
	var threads []refinery.ReviewThread
	for i := 0; i < MaxPriorThreads+7; i++ {
		threads = append(threads, refinery.ReviewThread{
			ID:   fmt.Sprintf("T%03d", i),
			Path: fmt.Sprintf("f%03d.ts", i), Line: 1, Author: "val",
			Body: findingBody("low", "security", fmt.Sprintf("finding %d", i), "body"),
		})
	}

	got := FormatPriorThreads(threads)

	if n := strings.Count(got, "- ["); n != MaxPriorThreads {
		t.Errorf("listed %d threads, want the cap %d", n, MaxPriorThreads)
	}
	if !strings.Contains(got, "7 further thread(s) not listed") {
		t.Errorf("overflow not announced:\n%s", got)
	}
	if !strings.Contains(got, fmt.Sprintf("%d prior thread(s)", MaxPriorThreads+7)) {
		t.Errorf("total count should reflect all threads, not just the listed ones:\n%s", got)
	}
}

func TestFormatPriorThreads_MissingBodyStillAnchors(t *testing.T) {
	threads := []refinery.ReviewThread{{
		ID: "T1", Path: "a.ts", Line: 4, Author: "val",
		Body: refinery.PriorityBadge("high"), // badge only, no title
	}}
	got := FormatPriorThreads(threads)
	if !strings.Contains(got, "a.ts:4") {
		t.Errorf("anchor lost when body had no prose:\n%s", got)
	}
	if !strings.Contains(got, "(no preview available)") {
		t.Errorf("empty preview not handled:\n%s", got)
	}
}
