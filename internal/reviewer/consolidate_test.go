package reviewer

import (
	"strings"
	"testing"
)

func TestParsePerspectiveResult_Valid(t *testing.T) {
	in := []byte(`{
	  "perspective": "security",
	  "verdict": "no findings: inputs validated",
	  "findings": [
	    {"path": "a.go", "line": 3, "priority": "HIGH", "title": "tainted path"}
	  ]
	}`)
	r, err := ParsePerspectiveResult(in)
	if err != nil {
		t.Fatalf("ParsePerspectiveResult: %v", err)
	}
	if r.Perspective != "security" {
		t.Errorf("perspective = %q", r.Perspective)
	}
	// Priority normalized, perspective inherited onto the finding.
	if r.Findings[0].Priority != "high" {
		t.Errorf("priority not normalized: %q", r.Findings[0].Priority)
	}
	if r.Findings[0].Perspective != "security" {
		t.Errorf("finding perspective not inherited: %q", r.Findings[0].Perspective)
	}
}

func TestParsePerspectiveResult_Invalid(t *testing.T) {
	cases := map[string]string{
		"missing verdict":      `{"perspective": "x", "findings": []}`,
		"missing perspective":  `{"verdict": "ok", "findings": []}`,
		"unknown field":        `{"perspective": "x", "verdict": "ok", "extra": 1}`,
		"bad finding line":     `{"perspective": "x", "verdict": "ok", "findings": [{"path": "a.go", "line": 0, "title": "t"}]}`,
		"bad priority":         `{"perspective": "x", "verdict": "ok", "findings": [{"path": "a.go", "line": 1, "title": "t", "priority": "urgent"}]}`,
		"trailing content":     `{"perspective": "x", "verdict": "ok", "findings": []} and more`,
		"perspective mismatch": `{"perspective": "x", "verdict": "ok", "findings": [{"path": "a.go", "line": 1, "title": "t", "perspective": "y"}]}`,
		"multiline verdict":    `{"perspective": "x", "verdict": "line one\nline two", "findings": []}`,
	}
	for name, in := range cases {
		if _, err := ParsePerspectiveResult([]byte(in)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestConsolidate_SummaryAccountsForEveryPerspective(t *testing.T) {
	results := []PerspectiveResult{
		{Perspective: "adversarial", Verdict: "found a nil deref", Findings: []Finding{
			{Path: "a.go", Line: 5, Priority: "medium", Perspective: "adversarial", Title: "nil deref"},
		}},
		{Perspective: "security", Verdict: "no findings: nothing tainted"},
	}
	fs := Consolidate(results, "sha123", nil)
	if fs.ReviewedSHA != "sha123" {
		t.Errorf("reviewed SHA = %q", fs.ReviewedSHA)
	}
	// Both verdicts — including the zero-finding one — appear in the summary.
	if len(fs.Summary.Verdicts) != 2 {
		t.Fatalf("summary carries %d verdicts, want one per perspective", len(fs.Summary.Verdicts))
	}
	if got := fs.Summary.Verdicts[0]; got.Perspective != "adversarial" || got.Verdict != "found a nil deref" {
		t.Errorf("verdict[0] = %+v, want the adversarial verdict", got)
	}
	// The zero-finding lens still gets its line — a perspective is never silent.
	if got := fs.Summary.Verdicts[1]; got.Perspective != "security" || got.Verdict != "no findings: nothing tainted" {
		t.Errorf("verdict[1] = %+v, want the security verdict", got)
	}
	if len(fs.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(fs.Findings))
	}
}

func TestConsolidate_DedupsAndEscalatesPriority(t *testing.T) {
	results := []PerspectiveResult{
		{Perspective: "adversarial", Verdict: "v1", Findings: []Finding{
			{Path: "a.go", Line: 10, Priority: "low", Perspective: "adversarial", Title: "Unchecked Error"},
		}},
		{Perspective: "security", Verdict: "v2", Findings: []Finding{
			// Same file:line, title differs only by case → deduped.
			{Path: "a.go", Line: 10, Priority: "high", Perspective: "security", Title: "unchecked error"},
			// Distinct finding → kept.
			{Path: "b.go", Line: 2, Priority: "medium", Perspective: "security", Title: "ssrf"},
		}},
	}
	fs := Consolidate(results, "", nil)
	if len(fs.Findings) != 2 {
		t.Fatalf("findings = %d, want 2 (one deduped)", len(fs.Findings))
	}
	merged := fs.Findings[0]
	if merged.Priority != "high" {
		t.Errorf("deduped priority = %q, want high (escalated)", merged.Priority)
	}
	// Both lenses credited on the merged finding.
	if !strings.Contains(merged.Perspective, "adversarial") || !strings.Contains(merged.Perspective, "security") {
		t.Errorf("merged perspective = %q, want both lenses", merged.Perspective)
	}
}

func TestConsolidate_MergesDifferingBodiesAndSuggestions(t *testing.T) {
	results := []PerspectiveResult{
		{Perspective: "adversarial", Verdict: "v1", Findings: []Finding{
			{Path: "a.go", Line: 10, Priority: "medium", Perspective: "adversarial",
				Title: "unchecked error", Body: "caller panics on nil", Suggestion: "guard nil"},
		}},
		{Perspective: "security", Verdict: "v2", Findings: []Finding{
			{Path: "a.go", Line: 10, Priority: "medium", Perspective: "security",
				Title: "unchecked error", Body: "tainted path reaches sink", Suggestion: "validate input"},
		}},
	}
	fs := Consolidate(results, "", nil)
	if len(fs.Findings) != 1 {
		t.Fatalf("findings = %d, want 1 (deduped)", len(fs.Findings))
	}
	m := fs.Findings[0]
	// Perspective-specific detail from BOTH lenses is preserved, not discarded.
	if !strings.Contains(m.Body, "caller panics on nil") || !strings.Contains(m.Body, "tainted path reaches sink") {
		t.Errorf("merged body dropped detail: %q", m.Body)
	}
	if !strings.Contains(m.Suggestion, "guard nil") || !strings.Contains(m.Suggestion, "validate input") {
		t.Errorf("merged suggestion dropped detail: %q", m.Suggestion)
	}
}

func TestMergeText_KeepsDistinctSubstringBlock(t *testing.T) {
	// "err" is a substring of the existing block but a distinct explanation;
	// block-wise comparison must keep it rather than swallow it.
	got := mergeText("the caller dereferences a nil err value", "err")
	if !strings.Contains(got, "\n\nerr") {
		t.Errorf("distinct substring block was swallowed: %q", got)
	}
	// An exact block repeat is not duplicated.
	same := mergeText("alpha\n\nbeta", "beta")
	if same != "alpha\n\nbeta" {
		t.Errorf("exact block should not be re-appended: %q", same)
	}
}

func TestMergePerspectives_CaseInsensitiveDedup(t *testing.T) {
	got := mergePerspectives("adversarial", "Adversarial")
	if got != "adversarial" {
		t.Errorf("case-variant tag not deduped: %q", got)
	}
}

func TestParsePerspectiveResult_CanonicalizesCaseVariantPerspective(t *testing.T) {
	in := []byte(`{"perspective":"security","verdict":"ok","findings":[{"path":"a.go","line":1,"title":"t","perspective":"Security"}]}`)
	r, err := ParsePerspectiveResult(in)
	if err != nil {
		t.Fatalf("case-variant perspective should be accepted: %v", err)
	}
	if r.Findings[0].Perspective != "security" {
		t.Errorf("finding perspective not canonicalized: %q", r.Findings[0].Perspective)
	}
}

func TestConsolidate_RoundTripsThroughParseFindings(t *testing.T) {
	results := []PerspectiveResult{
		{Perspective: "adversarial", Verdict: "v", Findings: []Finding{
			{Path: "a.go", Line: 1, Priority: "high", Perspective: "adversarial", Title: "t"},
		}},
	}
	fs := Consolidate(results, "sha", nil)
	// The consolidated payload must be valid input for `gt reviewer post`.
	body := fs.BuildReviewInput("sha")
	if len(body.Comments) != 1 {
		t.Fatalf("expected 1 inline comment, got %d", len(body.Comments))
	}
	if !strings.Contains(body.Body, "**Reviewed SHA:** `sha`") {
		t.Error("summary body missing reviewed SHA")
	}
}

// Opportunities are the channel §4a points out-of-scope improvements at: they
// reach the review body as advisory bullets, deduplicated across lenses, and
// they never move the verdict event.
func TestConsolidate_CollectsAndDedupsOpportunities(t *testing.T) {
	results := []PerspectiveResult{
		{Perspective: "security", Verdict: "no blocking issues",
			Opportunities: []string{"token TTL is hardcoded", "audit log lacks the actor id"}},
		{Perspective: "adversarial", Verdict: "one medium finding",
			// Same observation as the security lens, differently cased: one
			// follow-up, so one bullet.
			Opportunities: []string{"Token TTL is hardcoded", "retry loop has no jitter"},
			Findings: []Finding{
				{Path: "a.go", Line: 5, Priority: "medium", Perspective: "adversarial", Title: "unbounded retry"},
			}},
	}

	fs := Consolidate(results, "sha1", nil)

	want := []string{"token TTL is hardcoded", "audit log lacks the actor id", "retry loop has no jitter"}
	if len(fs.Summary.Opportunities) != len(want) {
		t.Fatalf("opportunities = %q, want %q (deduped case-insensitively)", fs.Summary.Opportunities, want)
	}
	for i, w := range want {
		if fs.Summary.Opportunities[i] != w {
			t.Errorf("opportunity[%d] = %q, want %q", i, fs.Summary.Opportunities[i], w)
		}
	}
	// Advisory: an opportunity is not a finding and does not withhold approval.
	if len(fs.Findings) != 1 {
		t.Errorf("opportunities must not become findings: %+v", fs.Findings)
	}
	if ev := fs.ReviewEvent(); ev != "APPROVE" {
		t.Errorf("event = %q — neither the medium finding nor the opportunities withhold approval", ev)
	}
	// And they survive to the posted body.
	body := fs.SummaryBody("sha1")
	for _, w := range want {
		if !strings.Contains(body, "- "+w) {
			t.Errorf("review body dropped opportunity %q:\n%s", w, body)
		}
	}
}

// A pass learns it overran from its own result, where the offender is obvious,
// rather than from a payload folded out of every lens's output.
func TestParsePerspectiveResult_EnforcesSummaryBudgets(t *testing.T) {
	long := strings.Repeat("x", MaxVerdictLen+1)
	if _, err := ParsePerspectiveResult([]byte(`{"perspective":"security","verdict":"` + long + `","findings":[]}`)); err == nil {
		t.Error("an over-budget verdict was accepted")
	}
	longOpp := strings.Repeat("x", MaxOpportunityLen+1)
	payload := `{"perspective":"security","verdict":"ok","opportunities":["` + longOpp + `"],"findings":[]}`
	if _, err := ParsePerspectiveResult([]byte(payload)); err == nil {
		t.Error("an over-budget opportunity was accepted")
	}
	ok := `{"perspective":"security","verdict":"ok","opportunities":["  add jitter  "],"findings":[]}`
	r, err := ParsePerspectiveResult([]byte(ok))
	if err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	if len(r.Opportunities) != 1 || r.Opportunities[0] != "add jitter" {
		t.Errorf("opportunities = %q, want them trimmed", r.Opportunities)
	}
}
