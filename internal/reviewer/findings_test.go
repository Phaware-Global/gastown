package reviewer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery"
)

func TestParseFindings_Valid(t *testing.T) {
	data := []byte(`{
		"summary": {"verdicts": [{"perspective": "adversarial", "verdict": "1 finding"}]},
		"reviewed_sha": "abc123",
		"findings": [
			{"path": "internal/foo.go", "line": 42, "priority": "High",
			 "perspective": "adversarial", "title": "nil deref", "body": "boom",
			 "suggestion": "guard it"}
		]
	}`)
	fs, err := ParseFindings(data)
	if err != nil {
		t.Fatalf("ParseFindings: %v", err)
	}
	if len(fs.Findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(fs.Findings))
	}
	if fs.Findings[0].Priority != "high" {
		t.Errorf("priority not normalized: %q", fs.Findings[0].Priority)
	}
	if fs.ReviewedSHA != "abc123" {
		t.Errorf("ReviewedSHA = %q", fs.ReviewedSHA)
	}
}

func TestParseFindings_DefaultsPriorityToMedium(t *testing.T) {
	data := []byte(`{"summary":` + okSummary + `,"findings":[{"path":"a.go","line":1,"title":"t"}]}`)
	fs, err := ParseFindings(data)
	if err != nil {
		t.Fatalf("ParseFindings: %v", err)
	}
	if fs.Findings[0].Priority != "medium" {
		t.Errorf("default priority = %q, want medium", fs.Findings[0].Priority)
	}
}

func TestParseFindings_Errors(t *testing.T) {
	cases := map[string]string{
		"missing summary":  `{"findings":[]}`,
		"blank verdict":    `{"summary":{"verdicts":[{"perspective":"p","verdict":"  "}]},"findings":[]}`,
		"no verdicts":      `{"summary":{"verdicts":[]},"findings":[]}`,
		"missing path":     `{"summary":` + okSummary + `,"findings":[{"line":1,"title":"t"}]}`,
		"nonpositive line": `{"summary":` + okSummary + `,"findings":[{"path":"a.go","line":0,"title":"t"}]}`,
		"missing title":    `{"summary":` + okSummary + `,"findings":[{"path":"a.go","line":1}]}`,
		"bad priority":     `{"summary":` + okSummary + `,"findings":[{"path":"a.go","line":1,"title":"t","priority":"urgent"}]}`,
		"unknown field":    `{"summary":` + okSummary + `,"bogus":true}`,
		"malformed json":   `{not json`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFindings([]byte(in)); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestFinding_FormatBody_RoundTripsPriority(t *testing.T) {
	f := Finding{
		Path: "a.go", Line: 1, Priority: "high", Perspective: "adversarial",
		Title: "nil deref", Body: "explanation", Suggestion: "guard it",
	}
	body := f.FormatBody()
	// The badge must parse back to the same priority via the shared contract.
	// parseThreadPriority is unexported in refinery, so assert via the public
	// PriorityBadge prefix instead.
	if !strings.HasPrefix(body, refinery.PriorityBadge("high")) {
		t.Errorf("body does not start with the high badge:\n%s", body)
	}
	if !strings.Contains(body, "**[adversarial]** nil deref") {
		t.Errorf("missing perspective tag + title:\n%s", body)
	}
	if !strings.Contains(body, "Suggested fix:") {
		t.Errorf("missing suggestion section:\n%s", body)
	}
	if !strings.Contains(body, "explanation") {
		t.Errorf("missing body text:\n%s", body)
	}
}

func TestFinding_FormatBody_NoPerspectiveNoSuggestion(t *testing.T) {
	f := Finding{Path: "a.go", Line: 2, Priority: "low", Title: "minor"}
	body := f.FormatBody()
	if strings.Contains(body, "Suggested fix:") {
		t.Errorf("unexpected suggestion section:\n%s", body)
	}
	if !strings.Contains(body, "**minor**") {
		t.Errorf("title without perspective should be bold-only:\n%s", body)
	}
}

func TestBuildComments(t *testing.T) {
	fs := &Findings{
		Summary: summaryOf("s"),
		Findings: []Finding{
			{Path: "a.go", Line: 10, Priority: "high", Title: "x"},
			{Path: "b.go", Line: 20, Priority: "low", Title: "y"},
		},
	}
	comments := fs.BuildComments()
	if len(comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(comments))
	}
	if comments[0].Path != "a.go" || comments[0].Line != 10 {
		t.Errorf("comment[0] anchor wrong: %+v", comments[0])
	}
	if comments[1].Path != "b.go" || comments[1].Line != 20 {
		t.Errorf("comment[1] anchor wrong: %+v", comments[1])
	}
}

func TestSummaryBody_CountsAndSHA(t *testing.T) {
	fs := &Findings{
		Summary: summaryOf("adversarial: 2. security: 1."),
		Findings: []Finding{
			{Path: "a.go", Line: 1, Priority: "high", Title: "x"},
			{Path: "b.go", Line: 2, Priority: "high", Title: "y"},
			{Path: "c.go", Line: 3, Priority: "low", Title: "z"},
		},
	}
	body := fs.SummaryBody("deadbeef")
	if !strings.Contains(body, "**Findings:** 3 (high: 2, low: 1)") {
		t.Errorf("count line wrong:\n%s", body)
	}
	if !strings.Contains(body, "**Reviewed SHA:** `deadbeef`") {
		t.Errorf("missing reviewed SHA:\n%s", body)
	}
	// Blockers lead: the two high findings are what a reader opens the review
	// for, and they are derived rather than authored.
	if !strings.HasPrefix(body, "### Blockers\n") {
		t.Errorf("blockers should lead:\n%s", body)
	}
	for _, want := range []string{"`a.go:1` — x", "`b.go:2` — y"} {
		if !strings.Contains(body, want) {
			t.Errorf("blockers section missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "`c.go:3`") {
		t.Errorf("a low finding must not be listed as a blocker:\n%s", body)
	}
}

func TestSummaryBody_NoFindings(t *testing.T) {
	fs := &Findings{Summary: summaryOf("all clear")}
	body := fs.SummaryBody("")
	if !strings.Contains(body, "**Findings:** 0") {
		t.Errorf("expected zero-count line:\n%s", body)
	}
	if strings.Contains(body, "Reviewed SHA:") {
		t.Errorf("no SHA expected when none provided:\n%s", body)
	}
}

func TestBuildReviewInput(t *testing.T) {
	fs := &Findings{
		Summary:  summaryOf("s"),
		Findings: []Finding{{Path: "a.go", Line: 1, Priority: "high", Title: "t"}},
	}
	in := fs.BuildReviewInput("sha1")
	if in.CommitID != "sha1" {
		t.Errorf("CommitID = %q", in.CommitID)
	}
	if len(in.Comments) != 1 {
		t.Errorf("got %d comments", len(in.Comments))
	}
	if !strings.Contains(in.Body, "**Reviewed SHA:** `sha1`") {
		t.Errorf("body missing SHA: %s", in.Body)
	}
	// A high-priority finding must post REQUEST_CHANGES, not a silent COMMENT.
	if in.Event != "REQUEST_CHANGES" {
		t.Errorf("Event = %q, want REQUEST_CHANGES", in.Event)
	}
	// SubmitReviewInput is the refinery contract type.
	var _ refinery.SubmitReviewInput = in
}

func TestReviewEvent_SeverityDerived(t *testing.T) {
	cases := []struct {
		name       string
		priorities []string
		want       string
	}{
		{"clean", nil, "APPROVE"},
		{"low only", []string{"low", "low"}, "APPROVE"},
		{"medium caps at comment", []string{"low", "medium"}, "COMMENT"},
		{"empty priority treated as medium", []string{""}, "COMMENT"},
		{"unknown priority treated as medium", []string{"low", "bogus"}, "COMMENT"},
		{"has high", []string{"low", "high", "medium"}, "REQUEST_CHANGES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &Findings{Summary: summaryOf("s")}
			for _, p := range tc.priorities {
				fs.Findings = append(fs.Findings, Finding{Path: "a.go", Line: 1, Priority: p, Title: "t"})
			}
			if got := fs.ReviewEvent(); got != tc.want {
				t.Errorf("ReviewEvent() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReviewEvent_ExplicitDispositionEscalatesOnly(t *testing.T) {
	// An explicit disposition raises the severity-derived default — a clean
	// review can request changes …
	fs := &Findings{Summary: summaryOf("s"), Disposition: "request_changes"}
	if got := fs.ReviewEvent(); got != "REQUEST_CHANGES" {
		t.Errorf("clean+request_changes: ReviewEvent() = %q, want REQUEST_CHANGES", got)
	}
	// … but it can never lower it. This assertion previously read "want COMMENT",
	// which locked in a downgrade: a "comment" disposition from one lens
	// suppressed another lens's high finding, turning a blocking review into an
	// advisory one with no injection and no bad actor involved.
	fs = &Findings{
		Summary:     summaryOf("s"),
		Disposition: "comment",
		Findings:    []Finding{{Path: "a.go", Line: 1, Priority: "high", Title: "t"}},
	}
	if got := fs.ReviewEvent(); got != "REQUEST_CHANGES" {
		t.Errorf("high+comment: ReviewEvent() = %q, want REQUEST_CHANGES (disposition is a floor, not a ceiling)", got)
	}
}

func TestParseFindings_InvalidDisposition(t *testing.T) {
	data := []byte(`{"summary":` + okSummary + `,"disposition":"block","findings":[]}`)
	if _, err := ParseFindings(data); err == nil {
		t.Error("expected error for invalid disposition")
	}
}

// The summary follows a template with three levels of budget: per item, per
// section, and overall. Each is enforced by rejection — never truncation — and
// each error names the field, its size, and the limit so the producer can trim
// and re-emit.
func TestParseFindings_SummaryTemplateBudgets(t *testing.T) {
	payload := func(s ReviewSummary) []byte {
		data, err := json.Marshal(Findings{Summary: s, Findings: []Finding{}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return data
	}
	verdicts := func(n, size int) []PerspectiveVerdict {
		out := make([]PerspectiveVerdict, n)
		for i := range out {
			out[i] = PerspectiveVerdict{
				Perspective: fmt.Sprintf("lens%d", i),
				Verdict:     strings.Repeat("x", size),
			}
		}
		return out
	}

	t.Run("verdict at the per-item limit is accepted", func(t *testing.T) {
		fs, err := ParseFindings(payload(ReviewSummary{Verdicts: verdicts(1, MaxVerdictLen)}))
		if err != nil {
			t.Fatalf("a verdict of exactly MaxVerdictLen runes was rejected: %v", err)
		}
		if got := len([]rune(fs.Summary.Verdicts[0].Verdict)); got != MaxVerdictLen {
			t.Errorf("verdict was modified: %d runes, want %d", got, MaxVerdictLen)
		}
	})

	t.Run("verdict over the per-item limit is rejected", func(t *testing.T) {
		_, err := ParseFindings(payload(ReviewSummary{Verdicts: verdicts(1, MaxVerdictLen+1)}))
		if err == nil {
			t.Fatal("an over-budget verdict was accepted; it must be rejected, not truncated")
		}
		for _, want := range []string{"verdict", "201 characters", "200"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("verdicts section budget is enforced across perspectives", func(t *testing.T) {
		// Six lenses, each individually legal, together over the section budget.
		_, err := ParseFindings(payload(ReviewSummary{Verdicts: verdicts(6, MaxVerdictLen)}))
		if err == nil {
			t.Fatal("an over-budget verdicts section was accepted")
		}
		for _, want := range []string{"verdicts total", "900", "6"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("opportunity over the per-item limit is rejected", func(t *testing.T) {
		_, err := ParseFindings(payload(ReviewSummary{
			Verdicts:      verdicts(1, 10),
			Opportunities: []string{strings.Repeat("x", MaxOpportunityLen+1)},
		}))
		if err == nil {
			t.Fatal("an over-budget opportunity was accepted")
		}
		for _, want := range []string{"opportunities[0]", "161 characters", "160"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("opportunities section budget is enforced", func(t *testing.T) {
		opps := make([]string, 5)
		for i := range opps {
			opps[i] = strings.Repeat("x", MaxOpportunityLen)
		}
		_, err := ParseFindings(payload(ReviewSummary{Verdicts: verdicts(1, 10), Opportunities: opps}))
		if err == nil {
			t.Fatal("an over-budget opportunities section was accepted")
		}
		if !strings.Contains(err.Error(), "opportunities total") {
			t.Errorf("error %q does not name the opportunities section", err)
		}
	})

	t.Run("overall budget binds before both sections max out", func(t *testing.T) {
		// Verdicts within their section budget, opportunities within theirs, but
		// together over the overall cap: the whole body is what a human reads.
		opps := make([]string, 3)
		for i := range opps {
			opps[i] = strings.Repeat("x", MaxOpportunityLen)
		}
		s := ReviewSummary{Verdicts: verdicts(4, MaxVerdictLen), Opportunities: opps}
		if s.verdictsLen() > MaxVerdictsLen || s.opportunitiesLen() > MaxOpportunitiesLen {
			t.Fatalf("test setup no longer exercises the overall cap alone: %d/%d",
				s.verdictsLen(), s.opportunitiesLen())
		}
		_, err := ParseFindings(payload(s))
		if err == nil {
			t.Fatal("an over-budget summary was accepted")
		}
		for _, want := range []string{"1280 characters", "1200"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	// Runes, not bytes: measuring bytes would silently halve the budget for a
	// summary written with non-ASCII characters.
	t.Run("counts runes not bytes", func(t *testing.T) {
		s := ReviewSummary{Verdicts: []PerspectiveVerdict{
			{Perspective: "lens", Verdict: strings.Repeat("é", MaxVerdictLen)},
		}}
		if _, err := ParseFindings(payload(s)); err != nil {
			t.Errorf("a verdict of MaxVerdictLen non-ASCII runes was rejected: %v", err)
		}
	})

	// Presentation is rendered by this package, so it can never consume budget:
	// a summary at exactly the overall limit still produces a much longer body.
	t.Run("markdown and derived sections cost nothing", func(t *testing.T) {
		s := ReviewSummary{Verdicts: verdicts(4, 150), Opportunities: []string{strings.Repeat("x", 100)}}
		fs, err := ParseFindings(payload(s))
		if err != nil {
			t.Fatalf("in-budget summary rejected: %v", err)
		}
		fs.Findings = []Finding{{Path: "a.go", Line: 1, Priority: "high", Title: "boom"}}
		body := fs.SummaryBody("abc1234")
		if len([]rune(body)) <= fs.Summary.ContentLen() {
			t.Errorf("rendered body (%d) should exceed the authored content (%d) — "+
				"formatting is free:\n%s", len([]rune(body)), fs.Summary.ContentLen(), body)
		}
	})
}

// okSummary is a minimal valid `summary` object, for the many tests that
// exercise something other than the summary template itself.
const okSummary = `{"verdicts":[{"perspective":"adversarial","verdict":"ok"}]}`

// summaryOf builds a one-verdict summary, keeping test literals short where the
// summary's content is beside the point.
func summaryOf(verdict string) ReviewSummary {
	return ReviewSummary{Verdicts: []PerspectiveVerdict{{Perspective: "adversarial", Verdict: verdict}}}
}
