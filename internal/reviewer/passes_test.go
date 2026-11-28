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

func TestConsolidatePassesEditsAndStamps(t *testing.T) {
	dir := t.TempDir()
	runTestPass(t, dir)
	passes, err := VerifyPasses(dir, []string{"security"}, 7, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := ConsolidatePasses(passes, &Edits{
		Drop:            []string{"security:1"},
		DropOpportunity: []string{"security:1"},
		Reanchor:        []string{"security:2=12"},
		Verdict:         []string{"security=trimmed"},
	}, testSHA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.Findings) != 1 || fs.Findings[0].Title != "second" || fs.Findings[0].Line != 12 {
		t.Fatalf("findings = %+v", fs.Findings)
	}
	body := fs.SummaryBody(testSHA)
	stamp := passes[0].Receipt.FinishedAt.Format(time.RFC3339)
	for _, want := range []string{"trimmed", "pass finished " + stamp, "dropped findings security:1", "rewrote the security verdict"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if len(passes[0].Result.Findings) != 2 {
		t.Error("edits mutated the verified result")
	}
	for _, bad := range []*Edits{{Drop: []string{"security:3"}}, {Drop: []string{"nobody:1"}}, {Verdict: []string{"security="}}} {
		if _, err := ConsolidatePasses(passes, bad, testSHA, nil); err == nil {
			t.Errorf("edit %+v: want error", bad)
		}
	}
}
