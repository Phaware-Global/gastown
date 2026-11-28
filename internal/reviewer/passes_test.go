package reviewer

import (
	"context"
	"errors"
	"os"
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

func assertNoReceipt(t *testing.T, dir string) {
	t.Helper()
	for _, p := range []string{passReceiptPath(dir, "security"), passResultPath(dir, "security")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived a failed pass (err %v)", p, err)
		}
	}
}

func TestRunPassWritesResultAndReceipt(t *testing.T) {
	dir := t.TempDir()
	runTestPass(t, dir)
	result, err := os.ReadFile(passResultPath(dir, "security"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(passReceiptPath(dir, "security"))
	if err != nil {
		t.Fatal(err)
	}
	var rc PassReceipt
	if err := decodeStrictJSON(raw, &rc); err != nil {
		t.Fatal(err)
	}
	if rc.PR != 7 || rc.SHA != testSHA || rc.Perspective != "security" || rc.PassID == "" {
		t.Errorf("receipt = %+v", rc)
	}
	if rc.ResultSHA256 != sha256Hex(result) || rc.PromptSHA256 != sha256Hex([]byte("review it")) {
		t.Errorf("receipt hashes do not match: %+v", rc)
	}
	if !rc.FinishedAt.After(rc.StartedAt) {
		t.Errorf("times: %s .. %s", rc.StartedAt, rc.FinishedAt)
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
			assertNoReceipt(t, dir)
		})
	}
}

func TestRunPassRepairsAnInvalidAnswerOnce(t *testing.T) {
	dir := t.TempDir()
	spec := PassSpec{Perspective: "security", PR: 7, SHA: testSHA, Prompt: "p"}
	if _, err := RunPass(context.Background(), dir, spec, answering("oops", "```json\n"+goodAnswer+"\n```")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(passResultPath(dir, "security"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsePassResult(data, "security"); err != nil {
		t.Fatal(err)
	}
}
