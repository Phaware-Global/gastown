package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenderClaudeSettingsReplacesFileWithGuards(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "reviewer", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"env":{"PLANTED":"1"},"hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RenderClaudeSettings(path, "myrig/reviewer", "reviewer"); err != nil {
		t.Fatal(err)
	}

	got, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Extra["env"]; ok {
		t.Error("content already in the file survived the render")
	}
	want, err := ComputeExpected("myrig/reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if !HooksEqual(want, &got.Hooks) {
		t.Error("rendered hooks differ from the expected reviewer hooks")
	}
	for _, entry := range got.Hooks.PreToolUse {
		if entry.Matcher == "mcp__github__merge_pull_request" {
			return
		}
	}
	t.Error("reviewer PreToolUse guards are missing")
}
