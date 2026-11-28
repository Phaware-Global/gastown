package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A checkout holds the PR's own files, so nothing in it may reach the argv or
// settings a pass is launched with.
func TestClaudePassLaunchIgnoresCheckoutConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the agent")
	}
	bin, record := t.TempDir(), t.TempDir()
	// Records its argv and the settings file it was given.
	script := `#!/bin/sh
: > "$PASS_RECORD/argv"
while [ $# -gt 0 ]; do
  if [ "$1" = "--settings" ]; then
    cat "$2" > "$PASS_RECORD/settings" 2>/dev/null || printf '%s' "$2" > "$PASS_RECORD/settings"
    printf '%s' "$2" > "$PASS_RECORD/settings-path"
    echo "--settings" >> "$PASS_RECORD/argv"
    shift 2
    continue
  fi
  printf '%s\n' "$1" >> "$PASS_RECORD/argv"
  shift
done
cat > /dev/null
echo '{"subtype":"success","result":"ok"}'
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GT_HOME", t.TempDir())
	t.Setenv("GT_COST_TIER", "")
	t.Setenv("PASS_RECORD", record)

	town := t.TempDir()
	rig := filepath.Join(town, "myrig")
	worktree := filepath.Join(rig, "reviewer", "rig")

	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(record, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	launch := func() (argv, settings string) {
		invoke, err := claudePassInvoker(town, rig, worktree)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := invoke(context.Background(), "prompt"); err != nil {
			t.Fatal(err)
		}
		if path := read("settings-path"); !filepath.IsAbs(path) || strings.HasPrefix(path, worktree) {
			t.Errorf("--settings is not a file outside the worktree: %q", path)
		}
		return read("argv"), read("settings")
	}

	if err := os.MkdirAll(filepath.Join(worktree, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(worktree)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude/settings.json", `{"hooks":{"PreToolUse":[]}}`)
	cleanArgv, cleanSettings := launch()

	const hostile = `{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"pr-owned-hook"}]}]},` +
		`"enabledPlugins":{"pr-owned-plugin":true}}`
	write(".claude/settings.json", hostile)
	write(".claude/settings.local.json", hostile)
	write(".mcp.json", `{"mcpServers":{"pr-owned-server":{"command":"pr-owned"}}}`)
	argv, settings := launch()

	if argv != cleanArgv || settings != cleanSettings {
		t.Errorf("checkout changed the launch:\nargv %s\nsettings %s", argv, settings)
	}
	if strings.Contains(argv+settings, "pr-owned") {
		t.Errorf("checkout content reached the launch:\nargv %s\nsettings %s", argv, settings)
	}
	// The guards are the reviewer role's.
	if !strings.Contains(settings, "gt reviewer post") {
		t.Errorf("reviewer guards missing: %s", settings)
	}
	for _, want := range []string{"--setting-sources\nuser\n", "--strict-mcp-config\n"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv is missing %q: %s", want, argv)
		}
	}
}
