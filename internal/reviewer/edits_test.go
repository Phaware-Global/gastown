package reviewer

import (
	"strings"
	"testing"
)

func twoLensResults(t *testing.T) []PerspectiveResult {
	t.Helper()
	var out []PerspectiveResult
	for _, answer := range []string{goodAnswer, adversarialAnswer} {
		r, err := ParsePerspectiveResult([]byte(answer))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, *r)
	}
	return out
}

const adversarialAnswer = `{"perspective":"adversarial","verdict":"same defect","findings":[` +
	`{"path":"a.go","line":4,"priority":"medium","title":"also first","body":"adv body"}]}`

func TestConsolidateWithEditsRefusedEdits(t *testing.T) {
	long := strings.Repeat("x", 801)
	tests := []struct {
		name    string
		edits   Edits
		wantErr string
	}{
		{"drop without a reason", Edits{Drop: []DropEdit{{ID: "security:1"}}}, "reason"},
		{"drop with a multi-line reason", Edits{Drop: []DropEdit{{ID: "security:1", Reason: "a\nb"}}}, "reason"},
		{"drop of an unknown id", Edits{Drop: []DropEdit{{ID: "security:3", Reason: "r"}}}, "no raw finding"},
		{"note on a finding no pass raised", Edits{Note: []NoteEdit{{ID: "perf:1", Text: "t"}}}, "no raw finding"},
		{"reword of an unknown id", Edits{Reword: []RewordEdit{{ID: "security:9", Title: "t"}}}, "no raw finding"},
		{"empty reword", Edits{Reword: []RewordEdit{{ID: "security:1"}}}, "changes nothing"},
		{"multi-line title", Edits{Reword: []RewordEdit{{ID: "security:1", Title: "a\nb"}}}, "one line"},
		{"empty note", Edits{Note: []NoteEdit{{ID: "security:1"}}}, "empty"},
		{"oversized note", Edits{Note: []NoteEdit{{ID: "security:1", Text: long}}}, "limit"},
		{"merge of one", Edits{Merge: [][]string{{"security:1"}}}, "at least two"},
		{"merge with an unknown id", Edits{Merge: [][]string{{"security:1", "adversarial:2"}}}, "no raw finding"},
		{"id merged twice", Edits{Merge: [][]string{{"security:1", "adversarial:1"}, {"security:2", "adversarial:1"}}}, "twice"},
		{"edit of a dropped finding", Edits{
			Drop: []DropEdit{{ID: "security:1", Reason: "r"}},
			Note: []NoteEdit{{ID: "security:1", Text: "t"}},
		}, "dropped"},
		{"merge into a dropped finding", Edits{
			Drop:  []DropEdit{{ID: "security:1", Reason: "r"}},
			Merge: [][]string{{"security:1", "adversarial:1"}},
		}, "dropped"},
		{"reanchor to line zero", Edits{Reanchor: []ReanchorEdit{{ID: "security:1"}}}, "line"},
		{"unknown opportunity", Edits{DropOpportunity: []string{"adversarial:1"}}, "matches nothing"},
		{"verdict for no pass", Edits{Verdict: map[string]string{"perf": "v"}}, "no pass"},
	}
	results := twoLensResults(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ConsolidateWithEdits(results, &tc.edits, testSHA, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestPassResultMayNotSetReviewerFields(t *testing.T) {
	for _, field := range []string{`"sources":["security:1"]`, `"reworded":["x"]`, `"reviewer_note":"n"`} {
		data := `{"perspective":"security","verdict":"v","findings":[{"path":"a.go","line":1,"priority":"low","title":"t",` + field + `}]}`
		if _, err := ParsePerspectiveResult([]byte(data)); err == nil {
			t.Errorf("%s: accepted", field)
		}
	}
}

func TestConsolidateWithEditsAllowedEdits(t *testing.T) {
	tests := []struct {
		name       string
		edits      Edits
		sources    []string // per posted finding
		wantInBody []string // summary plus every thread
	}{
		{
			name:    "no edits traces every finding",
			sources: []string{"security:1", "security:2", "adversarial:1"},
		},
		{
			name:    "drop with a reason is listed",
			edits:   Edits{Drop: []DropEdit{{ID: "security:1", Reason: "guarded by the caller"}}},
			sources: []string{"security:2", "adversarial:1"},
			wantInBody: []string{"### Dropped by reviewer (1)", "`a.go:3` high", "first", "[security:1]",
				"guarded by the caller"},
		},
		{
			name:       "merge records both ids",
			edits:      Edits{Merge: [][]string{{"security:1", "adversarial:1"}}},
			sources:    []string{"security:1+adversarial:1", "security:2"},
			wantInBody: []string{"**[security, adversarial]**", "adv body", "merged security:1+adversarial:1"},
		},
		{
			name:       "reworded text is marked",
			edits:      Edits{Reword: []RewordEdit{{ID: "security:1", Title: "shorter", Body: "tight"}}},
			sources:    []string{"security:1", "security:2", "adversarial:1"},
			wantInBody: []string{"shorter", "tight", "wording by the reviewer", "security:1 title, body", "(reworded by reviewer)"},
		},
		{
			name:       "added text is marked as the reviewer's",
			edits:      Edits{Note: []NoteEdit{{ID: "security:1", Text: "reproduced locally"}}},
			sources:    []string{"security:1", "security:2", "adversarial:1"},
			wantInBody: []string{"**Reviewer note** (not from a perspective pass):\nreproduced locally"},
		},
		{
			name:       "reanchor and verdict are disclosed",
			edits:      Edits{Reanchor: []ReanchorEdit{{ID: "security:2", Line: 12}}, Verdict: map[string]string{"security": "trimmed"}},
			sources:    []string{"security:1", "security:2", "adversarial:1"},
			wantInBody: []string{"re-anchored security:2 to line 12", "trimmed _(reworded by reviewer)_"},
		},
		{
			name:    "dropped opportunity",
			edits:   Edits{DropOpportunity: []string{"security:1"}},
			sources: []string{"security:1", "security:2", "adversarial:1"},
		},
	}
	results := twoLensResults(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs, err := ConsolidateWithEdits(results, &tc.edits, testSHA, nil)
			if err != nil {
				t.Fatal(err)
			}
			var sources []string
			body := fs.SummaryBody(testSHA)
			for _, f := range fs.Findings {
				sources = append(sources, strings.Join(f.Sources, "+"))
				body += "\n" + f.FormatBody()
			}
			if got, want := strings.Join(sources, " "), strings.Join(tc.sources, " "); got != want {
				t.Errorf("sources = %q, want %q", got, want)
			}
			for _, want := range tc.wantInBody {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q in:\n%s", want, body)
				}
			}
			if len(results[0].Findings) != 2 || results[0].Findings[1].Line != 9 {
				t.Error("edits mutated the verified result")
			}
		})
	}
}

func TestCollapsedNitsKeepSourcesAndLabels(t *testing.T) {
	results := []PerspectiveResult{{Perspective: "security", Verdict: "nits", Findings: []Finding{
		{Path: "a.go", Line: 1, Priority: "low", Title: "one", Perspective: "security"},
		{Path: "a.go", Line: 2, Priority: "low", Title: "two", Perspective: "security"},
	}}}
	fs, err := ConsolidateWithEdits(results, &Edits{
		Note:   []NoteEdit{{ID: "security:2", Text: "seen"}},
		Reword: []RewordEdit{{ID: "security:1", Title: "uno"}},
	}, testSHA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.Findings) != 1 || strings.Join(fs.Findings[0].Sources, " ") != "security:1 security:2" {
		t.Fatalf("findings = %+v", fs.Findings)
	}
	body := fs.Findings[0].FormatBody()
	for _, want := range []string{"Reviewer note (not from a perspective pass): seen", "wording by the reviewer: security:1 title"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}
