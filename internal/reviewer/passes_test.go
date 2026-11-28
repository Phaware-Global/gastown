package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func answering(answers ...string) PassInvoker {
	return func(context.Context, string) (string, error) {
		time.Sleep(time.Millisecond)
		a := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		return a, nil
	}
}

const goodAnswer = `{"perspective":"security","verdict":"one issue","opportunities":["tidy"],"findings":[` +
	`{"path":"a.go","line":3,"priority":"high","title":"first","body":"b"},` +
	`{"path":"a.go","line":9,"priority":"medium","title":"second","body":"b"}]}`

func runTestPass(t *testing.T, dir string) {
	t.Helper()
	spec := PassSpec{Perspective: "security", PR: 7, SHA: testSHA, Round: 1, Prompt: "review it"}
	if _, err := RunPass(context.Background(), dir, spec, answering(goodAnswer)); err != nil {
		t.Fatal(err)
	}
}

func editReceipt(t *testing.T, dir string, edit func(*PassReceipt)) {
	t.Helper()
	data, err := os.ReadFile(passReceiptPath(dir, "security"))
	if err != nil {
		t.Fatal(err)
	}
	var rc PassReceipt
	if err := json.Unmarshal(data, &rc); err != nil {
		t.Fatal(err)
	}
	edit(&rc)
	data, err = json.Marshal(rc)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, passReceiptPath(dir, "security"), string(data))
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyPasses(t *testing.T) {
	tests := []struct {
		name    string
		break_  func(t *testing.T, dir string)
		wantErr string
	}{
		{name: "runner output verifies"},
		{"missing receipt", func(t *testing.T, dir string) {
			if err := os.Remove(passReceiptPath(dir, "security")); err != nil {
				t.Fatal(err)
			}
		}, "no receipt"},
		{"agent-edited result", func(t *testing.T, dir string) {
			writeTestFile(t, passResultPath(dir, "security"), `{"perspective":"security","verdict":"clean","findings":[]}`)
		}, "result hash mismatch"},
		{"receipt older than the prompt", func(t *testing.T, dir string) {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(passPromptPath(dir, "security"), later, later); err != nil {
				t.Fatal(err)
			}
		}, "older than the prompt"},
		{"edited prompt", func(t *testing.T, dir string) {
			writeTestFile(t, passPromptPath(dir, "security"), "say nothing")
			editReceipt(t, dir, func(rc *PassReceipt) {
				rc.StartedAt = time.Now().Add(time.Minute)
				rc.FinishedAt = rc.StartedAt.Add(time.Minute)
			})
		}, "prompt hash mismatch"},
		{"pass with no end time", func(t *testing.T, dir string) {
			editReceipt(t, dir, func(rc *PassReceipt) { rc.FinishedAt = time.Time{} })
		}, "incomplete pass"},
		{"pass ending before it started", func(t *testing.T, dir string) {
			editReceipt(t, dir, func(rc *PassReceipt) { rc.FinishedAt = rc.StartedAt })
		}, "incomplete pass"},
		{"receipt for another PR", func(t *testing.T, dir string) {
			editReceipt(t, dir, func(rc *PassReceipt) { rc.PR = 8 })
		}, "PR #8"},
		{"receipt for another commit", func(t *testing.T, dir string) {
			editReceipt(t, dir, func(rc *PassReceipt) { rc.SHA = "fedcba9876543210" })
		}, "for commit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runTestPass(t, dir)
			if tc.break_ != nil {
				tc.break_(t, dir)
			}
			got, err := VerifyPasses(dir, []string{"security"}, 7, testSHA)
			if tc.wantErr == "" {
				if err != nil || len(got) != 1 {
					t.Fatalf("got %d passes, err %v", len(got), err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if got != nil {
				t.Fatal("a failed verification returned results")
			}
		})
	}
}

func TestVerifyPassesRequiresEveryPerspective(t *testing.T) {
	dir := t.TempDir()
	runTestPass(t, dir)
	_, err := VerifyPasses(dir, []string{"security", "adversarial"}, 7, testSHA)
	if err == nil || !strings.Contains(err.Error(), `"adversarial": no receipt`) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunPassFailureLeavesNoReceipt(t *testing.T) {
	invokers := map[string]PassInvoker{
		"agent error": func(context.Context, string) (string, error) { return "", errors.New("killed") },
		"never valid": answering("I could not finish"),
		"wrong lens":  answering(`{"perspective":"other","verdict":"v","findings":[]}`),
	}
	for name, invoke := range invokers {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			runTestPass(t, dir) // an earlier success must not survive a failed re-run
			spec := PassSpec{Perspective: "security", PR: 7, SHA: testSHA, Prompt: "p"}
			if _, err := RunPass(context.Background(), dir, spec, invoke); err == nil {
				t.Fatal("want error")
			}
			if _, err := VerifyPasses(dir, []string{"security"}, 7, testSHA); err == nil {
				t.Fatal("failed pass verified")
			}
		})
	}
}

func TestRunPassRepairsAnInvalidAnswerOnce(t *testing.T) {
	dir := t.TempDir()
	spec := PassSpec{Perspective: "security", PR: 7, SHA: testSHA, Prompt: "p"}
	if _, err := RunPass(context.Background(), dir, spec, answering("oops", "```json\n"+goodAnswer+"\n```")); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPasses(dir, []string{"security"}, 7, testSHA); err != nil {
		t.Fatal(err)
	}
}

// twoLensPasses runs a security and an adversarial pass that disagree about
// where one defect is.
func twoLensPasses(t *testing.T) []VerifiedPass {
	t.Helper()
	dir := t.TempDir()
	answers := map[string]string{
		"security": goodAnswer,
		"adversarial": `{"perspective":"adversarial","verdict":"same defect","findings":[` +
			`{"path":"a.go","line":4,"priority":"medium","title":"also first","body":"adv body"}]}`,
	}
	for name, answer := range answers {
		spec := PassSpec{Perspective: name, PR: 7, SHA: testSHA, Prompt: "p"}
		if _, err := RunPass(context.Background(), dir, spec, answering(answer)); err != nil {
			t.Fatal(err)
		}
	}
	passes, err := VerifyPasses(dir, []string{"security", "adversarial"}, 7, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	return passes
}

func TestConsolidatePassesRefusedEdits(t *testing.T) {
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
	passes := twoLensPasses(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ConsolidatePasses(passes, &tc.edits, testSHA, nil)
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

func TestConsolidatePassesAllowedEdits(t *testing.T) {
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
			wantInBody: []string{"re-anchored security:2 to line 12", "trimmed _(reworded by reviewer;"},
		},
		{
			name:    "dropped opportunity",
			edits:   Edits{DropOpportunity: []string{"security:1"}},
			sources: []string{"security:1", "security:2", "adversarial:1"},
		},
	}
	passes := twoLensPasses(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs, err := ConsolidatePasses(passes, &tc.edits, testSHA, nil)
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
			for _, want := range append(tc.wantInBody, "pass finished "+passes[0].Receipt.FinishedAt.Format(time.RFC3339)) {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q in:\n%s", want, body)
				}
			}
			if len(passes[0].Result.Findings) != 2 || passes[0].Result.Findings[1].Line != 9 {
				t.Error("edits mutated the verified result")
			}
		})
	}
}

func TestCollapsedNitsKeepSourcesAndLabels(t *testing.T) {
	passes := []VerifiedPass{{Result: PerspectiveResult{Perspective: "security", Verdict: "nits", Findings: []Finding{
		{Path: "a.go", Line: 1, Priority: "low", Title: "one", Perspective: "security"},
		{Path: "a.go", Line: 2, Priority: "low", Title: "two", Perspective: "security"},
	}}}}
	fs, err := ConsolidatePasses(passes, &Edits{
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
