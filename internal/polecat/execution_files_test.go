package polecat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteAgentFiles_ShipsSettingsWithPortableGtCommand(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	content := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/Users/agent/.local/bin/gt tap guard pr-workflow"}]}],` +
		`"UserPromptSubmit":[{"hooks":[{"type":"command","command":"case \"${GT_ROLE:-}\" in *witness*) exit 0 ;; *) /Users/agent/.local/bin/gt mail check --inject ;; esac"}]}],` +
		`"Stop":[{"hooks":[{"type":"command","command":"gt costs record &"}]}]}}`
	if err := os.WriteFile(settings, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	files, err := remoteAgentFiles([]string{"claude", "--settings", settings, "--model", "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"gt tap guard pr-workflow"}]}],` +
		`"UserPromptSubmit":[{"hooks":[{"type":"command","command":"case \"${GT_ROLE:-}\" in *witness*) exit 0 ;; *) gt mail check --inject ;; esac"}]}],` +
		`"Stop":[{"hooks":[{"type":"command","command":"gt costs record &"}]}]}}`
	if got := files[settings]; got != want {
		t.Fatalf("settings content:\n got %s\nwant %s", got, want)
	}
	if len(files) != 1 {
		t.Fatalf("want exactly the settings file, got %d files", len(files))
	}
}

func TestRemoteAgentFiles_NoSettingsFlag(t *testing.T) {
	files, err := remoteAgentFiles([]string{"codex", "-m", "gpt"})
	if err != nil || len(files) != 0 {
		t.Fatalf("got %v, %v; want no files", files, err)
	}
}

// A settings path the host cannot read must fail the start: the agent would
// otherwise come up on the worker with no hooks at all.
func TestRemoteAgentFiles_UnreadableSettingsIsAnError(t *testing.T) {
	if _, err := remoteAgentFiles([]string{"claude", "--settings", filepath.Join(t.TempDir(), "missing.json")}); err == nil {
		t.Fatal("expected an error")
	}
}
