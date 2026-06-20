package reviewer

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery"
)

func TestParseFindings_Valid(t *testing.T) {
	data := []byte(`{
		"summary": "adversarial: 1 finding. security: clean.",
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
	data := []byte(`{"summary":"s","findings":[{"path":"a.go","line":1,"title":"t"}]}`)
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
		"blank summary":    `{"summary":"   ","findings":[]}`,
		"missing path":     `{"summary":"s","findings":[{"line":1,"title":"t"}]}`,
		"nonpositive line": `{"summary":"s","findings":[{"path":"a.go","line":0,"title":"t"}]}`,
		"missing title":    `{"summary":"s","findings":[{"path":"a.go","line":1}]}`,
		"bad priority":     `{"summary":"s","findings":[{"path":"a.go","line":1,"title":"t","priority":"urgent"}]}`,
		"unknown field":    `{"summary":"s","bogus":true}`,
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
		Summary: "s",
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
		Summary: "adversarial: 2. security: 1.",
		Findings: []Finding{
			{Path: "a.go", Line: 1, Priority: "high", Title: "x"},
			{Path: "b.go", Line: 2, Priority: "high", Title: "y"},
			{Path: "c.go", Line: 3, Priority: "low", Title: "z"},
		},
	}
	body := fs.SummaryBody("deadbeef")
	if !strings.Contains(body, "Findings: 3 (high: 2, low: 1)") {
		t.Errorf("count line wrong:\n%s", body)
	}
	if !strings.Contains(body, "Reviewed SHA: deadbeef") {
		t.Errorf("missing reviewed SHA:\n%s", body)
	}
	if !strings.HasPrefix(body, "adversarial: 2. security: 1.") {
		t.Errorf("summary should lead:\n%s", body)
	}
}

func TestSummaryBody_NoFindings(t *testing.T) {
	fs := &Findings{Summary: "all clear"}
	body := fs.SummaryBody("")
	if !strings.Contains(body, "Findings: 0") {
		t.Errorf("expected zero-count line:\n%s", body)
	}
	if strings.Contains(body, "Reviewed SHA:") {
		t.Errorf("no SHA expected when none provided:\n%s", body)
	}
}

func TestBuildReviewInput(t *testing.T) {
	fs := &Findings{
		Summary:  "s",
		Findings: []Finding{{Path: "a.go", Line: 1, Priority: "high", Title: "t"}},
	}
	in := fs.BuildReviewInput("sha1")
	if in.CommitID != "sha1" {
		t.Errorf("CommitID = %q", in.CommitID)
	}
	if len(in.Comments) != 1 {
		t.Errorf("got %d comments", len(in.Comments))
	}
	if !strings.Contains(in.Body, "Reviewed SHA: sha1") {
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
		{"has high", []string{"low", "high", "medium"}, "REQUEST_CHANGES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &Findings{Summary: "s"}
			for _, p := range tc.priorities {
				fs.Findings = append(fs.Findings, Finding{Path: "a.go", Line: 1, Priority: p, Title: "t"})
			}
			if got := fs.ReviewEvent(); got != tc.want {
				t.Errorf("ReviewEvent() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReviewEvent_ExplicitDispositionOverrides(t *testing.T) {
	// An explicit disposition wins over the severity-derived default: a clean
	// review can request changes, and a high finding can be downgraded to comment.
	fs := &Findings{Summary: "s", Disposition: "request_changes"}
	if got := fs.ReviewEvent(); got != "REQUEST_CHANGES" {
		t.Errorf("clean+request_changes: ReviewEvent() = %q, want REQUEST_CHANGES", got)
	}
	fs = &Findings{
		Summary:     "s",
		Disposition: "comment",
		Findings:    []Finding{{Path: "a.go", Line: 1, Priority: "high", Title: "t"}},
	}
	if got := fs.ReviewEvent(); got != "COMMENT" {
		t.Errorf("high+comment: ReviewEvent() = %q, want COMMENT", got)
	}
}

func TestParseFindings_InvalidDisposition(t *testing.T) {
	data := []byte(`{"summary": "s", "disposition": "block", "findings": []}`)
	if _, err := ParseFindings(data); err == nil {
		t.Error("expected error for invalid disposition")
	}
}
