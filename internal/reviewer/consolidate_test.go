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
		"missing verdict":     `{"perspective": "x", "findings": []}`,
		"missing perspective": `{"verdict": "ok", "findings": []}`,
		"unknown field":       `{"perspective": "x", "verdict": "ok", "extra": 1}`,
		"bad finding line":    `{"perspective": "x", "verdict": "ok", "findings": [{"path": "a.go", "line": 0, "title": "t"}]}`,
		"bad priority":        `{"perspective": "x", "verdict": "ok", "findings": [{"path": "a.go", "line": 1, "title": "t", "priority": "urgent"}]}`,
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
	fs := Consolidate(results, "sha123")
	if fs.ReviewedSHA != "sha123" {
		t.Errorf("reviewed SHA = %q", fs.ReviewedSHA)
	}
	// Both verdicts — including the zero-finding one — appear in the summary.
	if !strings.Contains(fs.Summary, "[adversarial] found a nil deref") {
		t.Error("summary missing adversarial verdict")
	}
	if !strings.Contains(fs.Summary, "[security] no findings: nothing tainted") {
		t.Error("summary missing the zero-finding security verdict")
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
	fs := Consolidate(results, "")
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

func TestConsolidate_RoundTripsThroughParseFindings(t *testing.T) {
	results := []PerspectiveResult{
		{Perspective: "adversarial", Verdict: "v", Findings: []Finding{
			{Path: "a.go", Line: 1, Priority: "high", Perspective: "adversarial", Title: "t"},
		}},
	}
	fs := Consolidate(results, "sha")
	// The consolidated payload must be valid input for `gt reviewer post`.
	body := fs.BuildReviewInput("sha")
	if len(body.Comments) != 1 {
		t.Fatalf("expected 1 inline comment, got %d", len(body.Comments))
	}
	if !strings.Contains(body.Body, "Reviewed SHA: sha") {
		t.Error("summary body missing reviewed SHA")
	}
}
