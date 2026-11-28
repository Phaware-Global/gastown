package cmd

import (
	"slices"
	"strings"
	"testing"
)

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
