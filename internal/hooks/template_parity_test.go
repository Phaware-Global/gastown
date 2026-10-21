package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

// gt hooks sync replaces a Claude agent's hooks with ComputeExpected, so a
// guard that exists only in the settings template disappears at the first
// sync. Every template hook must survive in the synced config for each role
// that template is installed for, apart from the intended differences below.
func TestComputeExpectedKeepsTemplateHooks(t *testing.T) {
	t.Setenv("GT_HOME", t.TempDir()) // built-in base and overrides only

	// Intended differences, by "<target> <event> <command>". Crew cycles
	// the session on compaction (gt handoff --cycle) instead of re-priming.
	intended := map[string]bool{
		"gastown/crew PreCompact gt prime --hook": true,
	}

	for tmpl, targets := range map[string][]string{
		"templates/claude/settings-autonomous.json":  {"gastown/polecats", "gastown/witness", "gastown/refinery", "deacon", "boot", "gastown/reviewer", "assistant"},
		"templates/claude/settings-interactive.json": {"mayor", "gastown/crew"},
	} {
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
		for _, target := range targets {
			expected, err := ComputeExpected(target)
			if err != nil {
				t.Fatal(err)
			}
			for event, entries := range settings.Hooks {
				for _, entry := range entries {
					for _, h := range entry.Hooks {
						command := strings.ReplaceAll(h.Command, "{{GT_BIN}}", resolveGTBinary())
						// The template filters mail injection by GT_ROLE in
						// shell; the synced config runs it plainly and the
						// role overrides switch it off instead.
						if event == "UserPromptSubmit" && strings.HasPrefix(command, "case ") {
							continue
						}
						short := strings.Replace(command, resolveGTBinary(), "gt", 1)
						if intended[target+" "+event+" "+short] {
							continue
						}
						if !hasHook(expected.GetEntries(event), entry.Matcher, command) {
							t.Errorf("%s: %s %q -> %q from %s is missing after sync", target, event, entry.Matcher, short, tmpl)
						}
					}
				}
			}
		}
	}
}

func hasHook(entries []HookEntry, matcher, command string) bool {
	for _, e := range entries {
		if e.Matcher != matcher {
			continue
		}
		for _, h := range e.Hooks {
			if h.Command == command {
				return true
			}
		}
	}
	return false
}
