package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// destructiveTokens are command fragments that must never appear as an inline,
// executable copy in a plugin.md body. A plugin that ships a run.sh is
// dispatched as "run this script" (see FormatMailBody), but that is a directive
// to an LLM, not an enforced control: if run.sh is renamed or missing, the
// dispatcher falls back to handing the plugin.md body to the dog as
// instructions. Any destructive command left in that body then runs without the
// gates (dry-run, --destroy, visibility checks) that run.sh applies.
var destructiveTokens = []string{
	"-X DELETE",
	"branch -D",
	"gc --prune",
	"dolt push",
	"stash clear",
	"push --delete",
	"kill-session",
	"reset --hard",
	"rm -rf",
	"--force",
}

type shippedPlugin struct {
	name         string
	dir          string
	hasRunScript bool
	body         string
}

// loadShippedPlugins reads every plugins/<name>/plugin.md in the repo.
func loadShippedPlugins(t *testing.T) []shippedPlugin {
	t.Helper()

	root := filepath.Join("..", "..", "plugins")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}

	var plugins []shippedPlugin
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		content, err := os.ReadFile(filepath.Join(dir, "plugin.md")) //nolint:gosec // G304: repo-local test fixture path
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("reading %s/plugin.md: %v", dir, err)
		}
		parsed, err := parsePluginMD(content, dir, LocationTown, "")
		if err != nil {
			t.Fatalf("parsing %s/plugin.md: %v", dir, err)
		}
		info, statErr := os.Stat(filepath.Join(dir, "run.sh"))
		plugins = append(plugins, shippedPlugin{
			name:         e.Name(),
			dir:          dir,
			hasRunScript: statErr == nil && !info.IsDir(),
			body:         parsed.Instructions,
		})
	}
	return plugins
}

// TestShippedPlugins_DestructiveBodyRequiresRunScript pins the minimum
// invariant: a plugin.md that mentions a destructive command must be backed by
// a run.sh, so the gated script is the thing the dog executes.
func TestShippedPlugins_DestructiveBodyRequiresRunScript(t *testing.T) {
	plugins := loadShippedPlugins(t)
	if len(plugins) == 0 {
		t.Fatal("no shipped plugins found under plugins/")
	}
	for _, p := range plugins {
		if p.hasRunScript {
			continue
		}
		for _, tok := range destructiveTokens {
			if strings.Contains(p.body, tok) {
				t.Errorf("plugins/%s/plugin.md contains %q but the plugin has no run.sh", p.name, tok)
			}
		}
	}
}

// TestShippedPlugins_RunScriptPluginsHaveNoInlineCopy requires that when a
// plugin ships run.sh, its plugin.md body is only a description plus a pointer
// to run.sh: no destructive command anywhere in the body, and no fenced code
// block whose lines do anything other than invoke run.sh.
func TestShippedPlugins_RunScriptPluginsHaveNoInlineCopy(t *testing.T) {
	plugins := loadShippedPlugins(t)

	withScript := 0
	for _, p := range plugins {
		if !p.hasRunScript {
			continue
		}
		withScript++

		for _, tok := range destructiveTokens {
			if strings.Contains(p.body, tok) {
				t.Errorf("plugins/%s/plugin.md has run.sh but still contains %q; keep it out of the plugin.md body", p.name, tok)
			}
		}

		inFence := false
		for i, line := range strings.Split(p.body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") {
				inFence = !inFence
				continue
			}
			if !inFence || trimmed == "" {
				continue
			}
			if !strings.Contains(trimmed, "run.sh") {
				t.Errorf("plugins/%s/plugin.md body line %d is an executable inline line that does not invoke run.sh: %q", p.name, i+1, trimmed)
			}
		}
	}

	// Guard against the walk silently matching nothing.
	if withScript < 2 {
		t.Fatalf("expected at least 2 shipped plugins with run.sh, found %d", withScript)
	}
}
