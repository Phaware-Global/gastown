package cmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reviewer"
)

func TestReviewerPassSettingsKeepsOnlyGuards(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GT_HOME", "")
	got, err := reviewerPassSettings("myrig")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "PreToolUse") || strings.Contains(string(got), "SessionStart") {
		t.Errorf("settings = %s", got)
	}
}

func TestClaudePassArgsReplacesSuppliedConfig(t *testing.T) {
	got := claudePassArgs([]string{
		"--dangerously-skip-permissions",
		"--settings", "/role.json", "--settings=/role2.json",
		"--setting-sources", "project", "--setting-sources=local",
		"--mcp-config", "/a.json", "/b.json", "--mcp-config=/c.json",
		"--strict-mcp-config",
		"--model", "opus",
		"--", "--settings", "/late.json",
	}, "/guards.json")
	want := append([]string{"--dangerously-skip-permissions", "--model", "opus"},
		claudePassArgs(nil, "/guards.json")...)
	if !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestClaudeAnswer(t *testing.T) {
	if got, err := claudeAnswer([]byte(`{"subtype":"success","result":"ok"}`)); err != nil || got != "ok" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{`{"subtype":"error_max_turns"}`, `{"subtype":"success","is_error":true}`, `junk`} {
		if _, err := claudeAnswer([]byte(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestRunReviewerPassesSiblingCannotReplaceResults(t *testing.T) {
	answer := func(name string) string {
		return `{"perspective":"` + name + `","verdict":"v","findings":[]}`
	}
	dir := t.TempDir()
	forge := func(name string) {
		for _, f := range []string{"perspective-" + name + ".json", "receipt-" + name + ".json"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("forged"), 0o644); err != nil {
				t.Error(err)
			}
		}
	}
	securityDone, brokenDone := make(chan struct{}), make(chan struct{})
	invoke := func(_ context.Context, prompt string) (string, error) {
		switch prompt {
		case "security":
			defer close(securityDone)
			return answer("security"), nil
		case "broken":
			defer close(brokenDone)
			return "", errors.New("killed")
		}
		// The last pass still running tampers with its finished siblings.
		<-securityDone
		<-brokenDone
		time.Sleep(50 * time.Millisecond)
		forge("security")
		forge("broken")
		return answer("adversarial"), nil
	}
	var specs []reviewer.PassSpec
	for _, name := range []string{"security", "broken", "adversarial"} {
		specs = append(specs, reviewer.PassSpec{Perspective: name, PR: 7, Prompt: name})
	}
	var out strings.Builder
	if err := runReviewerPasses(context.Background(), &out, dir, specs, invoke); err == nil {
		t.Error("the failed pass was not reported")
	}
	for _, name := range []string{"security", "adversarial"} {
		result, err := os.ReadFile(filepath.Join(dir, "perspective-"+name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(result) != answer(name)+"\n" {
			t.Errorf("%s result = %q", name, result)
		}
		if want := fmt.Sprintf("result sha256 %x", sha256.Sum256(result)); !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q: %s", want, out.String())
		}
		receipt, err := os.ReadFile(filepath.Join(dir, "receipt-"+name+".json"))
		if err != nil || string(receipt) == "forged" {
			t.Errorf("%s receipt = %q, %v", name, receipt, err)
		}
	}
	for _, f := range []string{"perspective-broken.json", "receipt-broken.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s planted for a failed pass survived (err %v)", f, err)
		}
	}
}

func TestReviewerRunRefusesTwoStdinInputs(t *testing.T) {
	pr, prior, extra := reviewerPromptPR, reviewerPromptPriorThreads, reviewerPromptInstructions
	t.Cleanup(func() {
		reviewerPromptPR, reviewerPromptPriorThreads, reviewerPromptInstructions = pr, prior, extra
	})
	reviewerPromptPR, reviewerPromptPriorThreads, reviewerPromptInstructions = 7, "-", "-"
	err := runReviewerRun(reviewerRunCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot both read from stdin") {
		t.Errorf("err = %v", err)
	}
}
