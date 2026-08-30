package reviewer

import (
	"fmt"
	"strings"
)

// The review summary follows a fixed template rather than being free prose.
//
// A free-text summary drifted in two directions at once: some rounds pasted
// each lens's full narrative into it — restating, above the fold, detail the
// inline threads already carry anchored to the lines they concern — and others
// collapsed six lenses into one sentence that accounted for none of them. A
// human opening the PR and an agent reading the body in the fix loop both got a
// different shape every round.
//
// So the summary is structured data, and Go owns the rendering. Two consequences
// follow, and both are the point:
//
//   - Consistency is free. Every review body has the same sections in the same
//     order, so "what blocks this?" is always in the same place.
//   - Presentation costs the author nothing. Headings, bullets, bold, code
//     spans, and the derived footer are emitted here, so none of them consume a
//     budget. The budgets below measure only what a pass actually wrote.
type ReviewSummary struct {
	// Verdicts is one line per perspective, in the order the perspectives ran.
	// Required and complete: the role contract forbids silence, so a lens that
	// found nothing still says so in its own line rather than being omitted.
	Verdicts []PerspectiveVerdict `json:"verdicts"`

	// Opportunities are improvements the diff does not have to make: a better
	// implementation, an architectural alternative, a refactor outside the
	// changed lines. They are advisory by construction — they reach the mayor
	// in the review body and may become follow-up beads, but they are not
	// findings, they open no threads, and they never move the review event.
	Opportunities []string `json:"opportunities,omitempty"`
}

// PerspectiveVerdict is one lens's line in the Verdicts section.
type PerspectiveVerdict struct {
	Perspective string `json:"perspective"`
	Verdict     string `json:"verdict"`
}

// Content budgets for the summary template, in runes.
//
// These count CONTENT only — the text a pass authored. Section headings, list
// markers, bold, code spans, the perspective labels, and the derived
// Blockers/footer lines are rendered by this package and are free, so a pass is
// never charged for formatting it did not choose and cannot avoid.
//
// There are three levels, and each catches a different failure:
//
//   - Per item (MaxVerdictLen, MaxOpportunityLen) — one lens cannot turn its
//     line into an essay. A verdict is one line; an opportunity is one clause.
//   - Per section (MaxVerdictsLen, MaxOpportunitiesLen) — no single section can
//     crowd out the others, whatever the perspective count.
//   - Overall (MaxSummaryLen) — the body a human actually has to read.
//
// The section budgets deliberately sum to more than MaxSummaryLen: each one
// bounds its own section's shape, and the overall budget binds first when
// several sections run long together.
//
// Like the per-finding budgets in normalizeFinding, these are enforced by
// REJECTION, never truncation: a cut would land mid-sentence, and the sentence
// it cut might be the one saying a pass ran out of time.
const (
	MaxVerdictLen       = 200
	MaxOpportunityLen   = 160
	MaxVerdictsLen      = 900
	MaxOpportunitiesLen = 500
	MaxSummaryLen       = 1200
)

// ContentLen is the summary's total authored length in runes — what
// MaxSummaryLen bounds. Rendering is excluded by construction: it does not
// exist yet at this point.
func (s *ReviewSummary) ContentLen() int {
	return s.verdictsLen() + s.opportunitiesLen()
}

func (s *ReviewSummary) verdictsLen() int {
	n := 0
	for _, v := range s.Verdicts {
		n += len([]rune(v.Verdict))
	}
	return n
}

func (s *ReviewSummary) opportunitiesLen() int {
	n := 0
	for _, o := range s.Opportunities {
		n += len([]rune(o))
	}
	return n
}

// Normalize trims every field in place and enforces the template: a verdict per
// perspective, one line each, within the per-item, per-section, and overall
// budgets. ctx prefixes error messages ("findings.summary" from the payload
// boundary, "consolidated summary" from the build step) so a rejection names
// where the offending text came from.
//
// Errors name the field, its actual size, and the limit — the same contract as
// normalizeFinding — because the message is the only error-recovery guidance
// the producer has.
func (s *ReviewSummary) Normalize(ctx string) error {
	if len(s.Verdicts) == 0 {
		return fmt.Errorf("%s.verdicts is required: one line per perspective "+
			"(a review is never silent, and a lens that found nothing says so)", ctx)
	}
	for i := range s.Verdicts {
		v := &s.Verdicts[i]
		v.Perspective = strings.TrimSpace(v.Perspective)
		v.Verdict = strings.TrimSpace(v.Verdict)
		if v.Perspective == "" {
			return fmt.Errorf("%s.verdicts[%d]: perspective is required", ctx, i)
		}
		if v.Verdict == "" {
			return fmt.Errorf("%s.verdicts[%d] (%s): verdict is required "+
				"(say \"no findings\" explicitly)", ctx, i, v.Perspective)
		}
		// One line per lens is what makes the section scannable; a multi-line
		// verdict would also break the one-bullet-per-perspective rendering.
		if strings.ContainsAny(v.Verdict, "\n\r") {
			return fmt.Errorf("%s.verdicts[%d] (%s): verdict must be a single line "+
				"(put an out-of-scope improvement in opportunities instead)", ctx, i, v.Perspective)
		}
		if n := len([]rune(v.Verdict)); n > MaxVerdictLen {
			return fmt.Errorf("%s.verdicts[%d] (%s): verdict is %d characters, over the %d limit — "+
				"state the outcome for this lens in one line; the detail belongs in the findings",
				ctx, i, v.Perspective, n, MaxVerdictLen)
		}
	}
	if n := s.verdictsLen(); n > MaxVerdictsLen {
		return fmt.Errorf("%s.verdicts total %d characters, over the %d limit across %d "+
			"perspectives — shorten the longest verdicts", ctx, n, MaxVerdictsLen, len(s.Verdicts))
	}
	for i := range s.Opportunities {
		o := strings.TrimSpace(s.Opportunities[i])
		s.Opportunities[i] = o
		if o == "" {
			return fmt.Errorf("%s.opportunities[%d] is empty: drop the entry rather than "+
				"emitting a blank bullet", ctx, i)
		}
		if strings.ContainsAny(o, "\n\r") {
			return fmt.Errorf("%s.opportunities[%d]: an opportunity is one line "+
				"(split it into separate entries)", ctx, i)
		}
		if n := len([]rune(o)); n > MaxOpportunityLen {
			return fmt.Errorf("%s.opportunities[%d]: %d characters, over the %d limit — "+
				"name the improvement in one clause; it is a pointer for follow-up work, "+
				"not the work itself", ctx, i, n, MaxOpportunityLen)
		}
	}
	if n := s.opportunitiesLen(); n > MaxOpportunitiesLen {
		return fmt.Errorf("%s.opportunities total %d characters, over the %d limit across %d "+
			"entries — keep the ones worth a follow-up bead and drop the rest",
			ctx, n, MaxOpportunitiesLen, len(s.Opportunities))
	}
	if n := s.ContentLen(); n > MaxSummaryLen {
		return fmt.Errorf("%s is %d characters, over the %d limit — the summary carries one "+
			"line per perspective plus the opportunities; the detail belongs in the inline "+
			"findings, which already carry it", ctx, n, MaxSummaryLen)
	}
	return nil
}

// render writes the authored sections of the review body as GitHub-flavored
// markdown. Blockers and the footer are derived from the findings and are
// written by SummaryBody, which owns the section order.
func (s *ReviewSummary) render(b *strings.Builder) {
	b.WriteString("### Verdicts\n\n")
	for _, v := range s.Verdicts {
		fmt.Fprintf(b, "- **%s** — %s\n", v.Perspective, v.Verdict)
	}
	if len(s.Opportunities) == 0 {
		return
	}
	b.WriteString("\n### Opportunities\n\n")
	b.WriteString("_Out of scope for this PR — follow-up candidates, not blockers._\n\n")
	for _, o := range s.Opportunities {
		fmt.Fprintf(b, "- %s\n", o)
	}
}
