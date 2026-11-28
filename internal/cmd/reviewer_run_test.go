package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reviewer"
)

// stubReviewerPasses points the pass gate at a temp dir with a fixed set of
// enabled perspectives.
func stubReviewerPasses(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	prevNames, prevDir := reviewerEnabledPerspectives, reviewerPassDir
	reviewerEnabledPerspectives = func() ([]string, error) { return names, nil }
	reviewerPassDir = dir
	t.Cleanup(func() { reviewerEnabledPerspectives, reviewerPassDir = prevNames, prevDir })
	return dir
}

func runStubPass(t *testing.T, dir, name, sha, answer string) {
	t.Helper()
	spec := reviewer.PassSpec{Perspective: name, PR: 7, SHA: sha, Round: 1, Prompt: "p"}
	_, err := reviewer.RunPass(context.Background(), dir, spec, func(context.Context, string) (string, error) {
		time.Sleep(time.Millisecond)
		return answer, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGateReviewPost(t *testing.T) {
	const sha = "0123456789abcdef"
	const answer = `{"perspective":"security","verdict":"one issue","findings":[{"path":"a.go","line":3,"priority":"high","title":"t","body":"b"}]}`

	tests := []struct {
		name    string
		tamper  func(t *testing.T, dir string, fs *reviewer.Findings)
		shaFlag string
		wantErr string
	}{
		{name: "consolidated payload posts"},
		{name: "invented finding", tamper: func(_ *testing.T, _ string, fs *reviewer.Findings) {
			fs.Findings = append(fs.Findings, reviewer.Finding{Path: "b.go", Line: 1, Priority: "low", Title: "x"})
		}, wantErr: "is not what"},
		{name: "downgraded priority", tamper: func(_ *testing.T, _ string, fs *reviewer.Findings) {
			fs.Findings[0].Priority = "low"
		}, wantErr: "is not what"},
		{name: "missing receipt", tamper: func(t *testing.T, dir string, _ *reviewer.Findings) {
			if err := os.Remove(filepath.Join(dir, "receipt-security.json")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "no receipt"},
		{name: "result edited after consolidate", tamper: func(t *testing.T, dir string, _ *reviewer.Findings) {
			if err := os.WriteFile(filepath.Join(dir, "perspective-security.json"), []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "result hash mismatch"},
		{name: "no reviewed_sha", tamper: func(_ *testing.T, _ string, fs *reviewer.Findings) {
			fs.ReviewedSHA = ""
		}, wantErr: "no reviewed_sha"},
		{name: "other --sha", shaFlag: "fedcba9876543210", wantErr: "differs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := stubReviewerPasses(t, "security")
			runStubPass(t, dir, "security", sha, answer)
			supplied, err := consolidateVerifiedPasses(7, sha, &reviewer.Edits{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.tamper != nil {
				tc.tamper(t, dir, supplied)
			}
			got, err := gateReviewPost(7, tc.shaFlag, supplied)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(got.SummaryBody(sha), "pass finished ") {
					t.Errorf("body lacks the pass end time:\n%s", got.SummaryBody(sha))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestReviewerPassSettingsKeepsOnlyGuards(t *testing.T) {
	got, err := reviewerPassSettings([]byte(`{"hooks":{"PreToolUse":[{"matcher":"x"}],"SessionStart":[{"matcher":"y"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "PreToolUse") || strings.Contains(got, "SessionStart") {
		t.Errorf("settings = %s", got)
	}
	if _, err := reviewerPassSettings([]byte(`{"hooks":{}}`)); err == nil {
		t.Error("settings without guards were accepted")
	}
}

func TestClaudePassArgs(t *testing.T) {
	args := claudePassArgs([]string{"--dangerously-skip-permissions", "--settings", "/role.json"}, "{}")
	if slices.Contains(args, "/role.json") {
		t.Errorf("role settings not replaced: %v", args)
	}
	i := slices.Index(args, "--setting-sources")
	if i < 0 || args[i+1] != "user" {
		t.Errorf("project settings must not load: %v", args)
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
