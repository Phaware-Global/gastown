package reviewer

import (
	"strings"
	"testing"
)

// The rendered body follows the template in a fixed order, so a reader always
// finds the same thing in the same place: what blocks, what each lens said,
// what is worth a follow-up, and the tally.
func TestSummaryBody_RendersTheTemplateInOrder(t *testing.T) {
	fs := &Findings{
		Summary: ReviewSummary{
			Verdicts: []PerspectiveVerdict{
				{Perspective: "adversarial", Verdict: "no blocking issues; error paths guarded"},
				{Perspective: "security", Verdict: "token TTL is hardcoded"},
			},
			Opportunities: []string{"retry loop has no jitter", "audit log lacks the actor id"},
		},
		Findings: []Finding{
			{Path: "internal/auth/token.go", Line: 42, Priority: "high",
				Perspective: "security", Title: "token TTL is hardcoded"},
			{Path: "internal/auth/token.go", Line: 88, Priority: "low",
				Perspective: "adversarial", Title: "stale comment"},
		},
	}

	body := fs.SummaryBody("abc1234")

	order := []string{"### Blockers", "### Verdicts", "### Opportunities", "**Findings:**"}
	at := -1
	for _, section := range order {
		i := strings.Index(body, section)
		if i < 0 {
			t.Fatalf("body is missing %q:\n%s", section, body)
		}
		if i < at {
			t.Errorf("%q is out of order:\n%s", section, body)
		}
		at = i
	}

	for _, want := range []string{
		"- `internal/auth/token.go:42` — token TTL is hardcoded [security]",
		"- **adversarial** — no blocking issues; error paths guarded",
		"- **security** — token TTL is hardcoded",
		"- retry loop has no jitter",
		"**Findings:** 2 (high: 1, low: 1) · **Reviewed SHA:** `abc1234`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	// Only the high finding blocks; the low one is a thread, not a headline.
	if strings.Contains(body, "token.go:88") {
		t.Errorf("a low finding was listed as a blocker:\n%s", body)
	}
}

// No blockers means no Blockers section: the absence is the signal, and an
// empty heading would make a clean review look like it was missing something.
func TestSummaryBody_OmitsEmptySections(t *testing.T) {
	fs := &Findings{Summary: ReviewSummary{
		Verdicts: []PerspectiveVerdict{{Perspective: "security", Verdict: "clean"}},
	}}

	body := fs.SummaryBody("")

	for _, unwanted := range []string{"### Blockers", "### Opportunities", "Reviewed SHA"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("body should omit %q when it has no content:\n%s", unwanted, body)
		}
	}
	if !strings.Contains(body, "**Findings:** 0 — no findings.") {
		t.Errorf("body missing the zero-findings tally:\n%s", body)
	}
}

// An out-of-scope finding never reports the PR as blocked: Consolidate demotes
// it, and the Blockers section applies the same rule directly so the headline
// cannot disagree with the verdict event.
func TestSummaryBody_BlockersExcludeOutOfScope(t *testing.T) {
	fs := &Findings{
		Summary: ReviewSummary{Verdicts: []PerspectiveVerdict{{Perspective: "ddd", Verdict: "v"}}},
		Findings: []Finding{
			{Path: "untouched.go", Line: 1, Priority: "high", Title: "elsewhere", Scope: ScopeOut},
		},
	}
	if body := fs.SummaryBody(""); strings.Contains(body, "### Blockers") {
		t.Errorf("an out-of-scope finding must not be reported as a blocker:\n%s", body)
	}
}

// The template is a contract, not a suggestion: a summary that omits a
// perspective, or whose verdict is multi-line, is rejected rather than rendered
// into a shape the reader cannot scan.
func TestReviewSummary_NormalizeRejectsBrokenTemplate(t *testing.T) {
	cases := map[string]ReviewSummary{
		"no verdicts":     {},
		"blank verdict":   {Verdicts: []PerspectiveVerdict{{Perspective: "p", Verdict: "  "}}},
		"no perspective":  {Verdicts: []PerspectiveVerdict{{Verdict: "v"}}},
		"multiline":       {Verdicts: []PerspectiveVerdict{{Perspective: "p", Verdict: "one\ntwo"}}},
		"blank bullet":    {Verdicts: []PerspectiveVerdict{{Perspective: "p", Verdict: "v"}}, Opportunities: []string{" "}},
		"bullet newlines": {Verdicts: []PerspectiveVerdict{{Perspective: "p", Verdict: "v"}}, Opportunities: []string{"a\nb"}},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if err := s.Normalize("summary"); err == nil {
				t.Errorf("%s was accepted", name)
			}
		})
	}
}

// Normalize trims in place, so the rendered bullets carry no stray whitespace.
func TestReviewSummary_NormalizeTrims(t *testing.T) {
	s := ReviewSummary{
		Verdicts:      []PerspectiveVerdict{{Perspective: "  security  ", Verdict: "  clean  "}},
		Opportunities: []string{"  add jitter  "},
	}
	if err := s.Normalize("summary"); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if s.Verdicts[0].Perspective != "security" || s.Verdicts[0].Verdict != "clean" {
		t.Errorf("verdict not trimmed: %+v", s.Verdicts[0])
	}
	if s.Opportunities[0] != "add jitter" {
		t.Errorf("opportunity not trimmed: %q", s.Opportunities[0])
	}
}
