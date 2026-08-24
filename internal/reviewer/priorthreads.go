package reviewer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/refinery"
)

// MaxPriorThreads caps how many threads the prior-round payload lists.
//
// The payload travels in a mail body and then into every perspective prompt, so
// it competes for context with the diff the pass is there to review. A deep fix
// round can carry hundreds of threads (graphql-api PR #112 reached 199), and at
// the Reviewer's measured ~3.1kB per thread body an uncapped payload would be
// larger than most diffs. Threads beyond the cap are summarized as a count
// rather than dropped silently, so the pass knows its context is partial.
const MaxPriorThreads = 60

// priorThreadPreviewLen bounds each thread's one-line preview. Matches the
// width refinery uses for its own thread previews.
const priorThreadPreviewLen = 120

// FormatPriorThreads renders a PR's existing review threads into the
// deterministic block that `gt reviewer prompt --prior-threads` injects into
// every perspective pass.
//
// Two properties matter and neither was true of the ad-hoc assembly this
// replaces:
//
// Resolved threads are included, marked RESOLVED. The execution contract's
// round-2 rules tell a pass "do not relitigate already-resolved threads" — an
// instruction it could not follow, because the payload listed only UNRESOLVED
// threads. A finding fixed in round 3 was therefore invisible in round 4 and
// free to be raised again, which is the shape of the observed round-over-round
// churn. Open threads still come first: they are what the round must act on.
//
// The preview is the finding's title, not its first line. Every Reviewer
// finding body opens with a shields.io priority badge (see
// refinery.PriorityBadge), so a naive first-line preview rendered every entry
// as the same badge URL and carried no information at all.
//
// Ordering is (state, path, line, ID) so the same thread set always produces a
// byte-identical block: a payload that reshuffles between dispatches reads as
// new information to the pass and defeats prompt caching.
func FormatPriorThreads(threads []refinery.ReviewThread) string {
	if len(threads) == 0 {
		return ""
	}

	sorted := make([]refinery.ReviewThread, len(threads))
	copy(sorted, threads)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		// Unresolved first: the round's actual worklist leads.
		if a.IsResolved != b.IsResolved {
			return !a.IsResolved
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.ID < b.ID
	})

	var open, resolved int
	for _, t := range sorted {
		if t.IsResolved {
			resolved++
		} else {
			open++
		}
	}

	shown := sorted
	overflow := 0
	if len(shown) > MaxPriorThreads {
		overflow = len(shown) - MaxPriorThreads
		shown = shown[:MaxPriorThreads]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d prior thread(s): %d unresolved, %d resolved.\n", len(sorted), open, resolved)
	b.WriteString("Unresolved threads are this round's worklist. " +
		"Do NOT re-raise a RESOLVED finding unless the fix is demonstrably wrong.\n\n")

	for _, t := range shown {
		state := "OPEN"
		if t.IsResolved {
			state = "RESOLVED"
		}
		// An outdated thread anchors to a line the diff has since rewritten;
		// naming it tells the pass why the anchor may not match what it reads.
		if t.IsOutdated {
			state += "/OUTDATED"
		}
		preview := refinery.FirstLinePreview(t.Body, priorThreadPreviewLen)
		if preview == "" {
			preview = "(no preview available)"
		}
		fmt.Fprintf(&b, "- [%s] %s:%d (%s) %s\n", state, t.Path, t.Line, t.Author, preview)
	}

	if overflow > 0 {
		fmt.Fprintf(&b, "\n(%d further thread(s) not listed — this context is partial; "+
			"prefer confirming a finding against the diff over assuming it is new.)\n", overflow)
	}

	return strings.TrimRight(b.String(), "\n")
}
