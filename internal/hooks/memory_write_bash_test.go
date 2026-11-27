package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

// Shell redirects write memory files too, so the guard runs on Bash calls in
// the synced config of every role and in both settings templates.
func TestMemoryWriteGuardRunsOnBash(t *testing.T) {
	t.Setenv("GT_HOME", t.TempDir()) // built-in base and overrides only
	command := gtCommand("gt tap guard memory-write")

	for _, target := range []string{"mayor", "gastown/crew", "gastown/polecats", "gastown/witness", "gastown/refinery", "deacon", "boot", "gastown/reviewer", "assistant"} {
		cfg, err := ComputeExpected(target)
		if err != nil {
			t.Fatal(err)
		}
		if !hasHook(cfg.PreToolUse, "Bash", command) {
			t.Errorf("%s: memory-write does not run on Bash", target)
		}
	}

	for _, tmpl := range []string{"templates/claude/settings-autonomous.json", "templates/claude/settings-interactive.json"} {
		data, err := templateFS.ReadFile(tmpl)
		if err != nil {
			t.Fatal(err)
		}
		var settings struct {
			Hooks map[string][]HookEntry `json:"hooks"`
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(command, resolveGTBinary(), "{{GT_BIN}}", 1)
		if !hasHook(settings.Hooks["PreToolUse"], "Bash", want) {
			t.Errorf("%s: memory-write does not run on Bash", tmpl)
		}
	}
}
