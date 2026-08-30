package reviewer

import (
	"encoding/json"
	"strings"
	"testing"
)

// The regression this pins is graphql-api PR #114: four lenses raised the same
// organizationMemberships[0] defect on line 88 in four different sentences, the
// title-keyed dedup treated them as four findings, and the fixer got four
// blocking threads for one edit.
func TestConsolidate_MergesSameLineAcrossPerspectives(t *testing.T) {
	results := []PerspectiveResult{
		{Perspective: "adversarial", Verdict: "v", Findings: []Finding{{
			Path: "a.ts", Line: 88, Priority: "high", Perspective: "adversarial",
			Title: "organizationMemberships[0] selects an arbitrary membership",
			Body:  "Index-0 selection is order dependent.",
		}}},
		{Perspective: "security", Verdict: "v", Findings: []Finding{{
			Path: "a.ts", Line: 88, Priority: "medium", Perspective: "security",
			Title: "First-membership selection permits cross-org read",
			Body:  "A user in two orgs can read the wrong tenant.",
		}}},
		{Perspective: "typescript", Verdict: "v", Findings: []Finding{{
			Path: "a.ts", Line: 88, Priority: "medium", Perspective: "typescript",
			Title: "Unguarded index access",
			Body:  "noUncheckedIndexedAccess would reject this.",
		}}},
	}

	fs := Consolidate(results, "sha", nil)

	if len(fs.Findings) != 1 {
		t.Fatalf("got %d findings, want 1 merged thread", len(fs.Findings))
	}
	f := fs.Findings[0]
	if f.Priority != "high" {
		t.Errorf("priority = %q, want high (most severe wins)", f.Priority)
	}
	// The most severe framing leads.
	if !strings.Contains(f.Title, "arbitrary membership") {
		t.Errorf("title = %q, want the high finding's title", f.Title)
	}
	// No lens's framing may be lost.
	for _, want := range []string{"cross-org read", "Unguarded index access"} {
		if !strings.Contains(f.Body, want) {
			t.Errorf("merged body dropped %q:\n%s", want, f.Body)
		}
	}
	for _, want := range []string{"adversarial", "security", "typescript"} {
		if !strings.Contains(f.Perspective, want) {
			t.Errorf("perspective tags dropped %q: %q", want, f.Perspective)
		}
	}
	if fs.ReviewEvent() != "REQUEST_CHANGES" {
		t.Errorf("event = %q, want REQUEST_CHANGES", fs.ReviewEvent())
	}
}

// Distinct defects on distinct lines must stay distinct — the merge is keyed on
// the line, not the file.
func TestConsolidate_DifferentLinesStaySeparate(t *testing.T) {
	results := []PerspectiveResult{{Perspective: "security", Verdict: "v", Findings: []Finding{
		{Path: "a.ts", Line: 10, Priority: "high", Perspective: "security", Title: "One"},
		{Path: "a.ts", Line: 20, Priority: "high", Perspective: "security", Title: "Two"},
	}}}
	if fs := Consolidate(results, "sha", nil); len(fs.Findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(fs.Findings))
	}
}

func TestConsolidate_CollapsesLowsPerFile(t *testing.T) {
	results := []PerspectiveResult{{Perspective: "security", Verdict: "v", Findings: []Finding{
		{Path: "a.ts", Line: 30, Priority: "low", Perspective: "security", Title: "Nit three"},
		{Path: "a.ts", Line: 10, Priority: "low", Perspective: "security", Title: "Nit one"},
		{Path: "a.ts", Line: 20, Priority: "low", Perspective: "security", Title: "Nit two"},
		{Path: "b.ts", Line: 5, Priority: "low", Perspective: "security", Title: "Lonely nit"},
		{Path: "a.ts", Line: 40, Priority: "medium", Perspective: "security", Title: "Real issue"},
	}}}

	fs := Consolidate(results, "sha", nil)

	var nits, lonely, medium *Finding
	for i := range fs.Findings {
		switch {
		case strings.HasPrefix(fs.Findings[i].Title, "Nits:"):
			nits = &fs.Findings[i]
		case fs.Findings[i].Title == "Lonely nit":
			lonely = &fs.Findings[i]
		case fs.Findings[i].Title == "Real issue":
			medium = &fs.Findings[i]
		}
	}

	if nits == nil {
		t.Fatalf("a.ts lows were not collapsed: %+v", fs.Findings)
	}
	if !strings.Contains(nits.Title, "3 non-blocking") {
		t.Errorf("collapsed title = %q, want a count of 3", nits.Title)
	}
	// The collapsed thread anchors at the earliest line so it sorts naturally.
	if nits.Line != 10 {
		t.Errorf("collapsed anchor line = %d, want 10", nits.Line)
	}
	for _, want := range []string{"Nit one", "Nit two", "Nit three", "line 30"} {
		if !strings.Contains(nits.Body, want) {
			t.Errorf("collapsed body dropped %q:\n%s", want, nits.Body)
		}
	}
	// A single low on a file is left alone — wrapping it would only obscure it.
	if lonely == nil {
		t.Errorf("a lone low on b.ts should not be collapsed: %+v", fs.Findings)
	}
	// Medium and above stay individually anchored.
	if medium == nil {
		t.Errorf("medium finding was collapsed: %+v", fs.Findings)
	}
	// Lows are non-blocking, and collapsing must not change that. Neither the
	// medium nor the collapsed nits withhold approval — the threads carry the
	// work, the verdict carries only "does this block the merge".
	if fs.ReviewEvent() != "APPROVE" {
		t.Errorf("event = %q, want APPROVE (one medium, rest low)", fs.ReviewEvent())
	}
}

func TestNormalizeFinding_LengthBudgets(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Finding)
		wantErr string
	}{
		{"title over budget", func(f *Finding) { f.Title = strings.Repeat("x", MaxTitleLen+1) }, "title is 121 characters"},
		{"body over budget", func(f *Finding) { f.Body = strings.Repeat("x", MaxBodyLen+1) }, "body is 1201 characters"},
		{"suggestion over budget", func(f *Finding) { f.Suggestion = strings.Repeat("x", MaxSuggestionLen+1) }, "suggestion is 801 characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Finding{Path: "a.ts", Line: 1, Priority: "high", Title: "ok"}
			tt.mutate(&f)
			err := normalizeFinding(&f, "findings[0]")
			if err == nil {
				t.Fatalf("over-budget %s accepted", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to name the size (%s)", err, tt.wantErr)
			}
		})
	}
}

// The budget counts runes, not bytes: measuring bytes would silently halve the
// budget for a finding written with non-ASCII characters.
func TestNormalizeFinding_BudgetCountsRunes(t *testing.T) {
	f := Finding{
		Path: "a.ts", Line: 1, Priority: "high", Title: "ok",
		// 3 bytes per rune, exactly at the limit.
		Body: strings.Repeat("é", MaxBodyLen),
	}
	if err := normalizeFinding(&f, "findings[0]"); err != nil {
		t.Errorf("a body of exactly MaxBodyLen runes was rejected: %v", err)
	}
}

func TestParseFindings_RejectsOverBudgetPayload(t *testing.T) {
	payload, err := json.Marshal(Findings{
		Summary: summaryOf("s"),
		Findings: []Finding{{
			Path: "a.ts", Line: 1, Priority: "high", Title: "t",
			Body: strings.Repeat("x", MaxBodyLen+50),
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ParseFindings(payload); err == nil {
		t.Fatal("ParseFindings accepted an over-budget body")
	}
}
