package tmux

import (
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// The prompt prefix resolves through the provider preset for town-defined
// agents; a preset without a ready prompt keeps the default for idle
// detection but yields "" for submit verification, so an opencode pane is
// never judged "submitted" by Claude's prefix while its input box still holds
// the pasted nudge.
func TestPromptPrefixForAgent(t *testing.T) {
	townRoot := t.TempDir()
	path := config.TownSettingsPath(townRoot)
	settings, err := config.LoadOrCreateTownSettings(path)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	settings.Agents["opencode-nemotron"] = &config.RuntimeConfig{Command: "opencode"}
	settings.Agents["claude-sonnet"] = &config.RuntimeConfig{Command: "claude"}
	if err := config.SaveTownSettings(path, settings); err != nil {
		t.Fatalf("save: %v", err)
	}
	codex := config.GetAgentPresetByName("codex")
	if codex == nil || codex.ReadyPromptPrefix == "" {
		t.Fatalf("codex preset must have a ready prompt for this test, got %+v", codex)
	}
	for _, c := range []struct {
		agent        string
		idle, submit string
	}{
		{"opencode", DefaultReadyPromptPrefix, ""},
		{"opencode-nemotron", DefaultReadyPromptPrefix, ""}, // town agent, provider inferred from its command
		{"codex", codex.ReadyPromptPrefix, codex.ReadyPromptPrefix},
		{"claude-sonnet", DefaultReadyPromptPrefix, DefaultReadyPromptPrefix},
		{"no-such-agent", DefaultReadyPromptPrefix, DefaultReadyPromptPrefix},
		{"", DefaultReadyPromptPrefix, DefaultReadyPromptPrefix},
	} {
		if got := promptPrefixForAgent(c.agent, townRoot, false); got != c.idle {
			t.Errorf("%q idle: got %q, want %q", c.agent, got, c.idle)
		}
		if got := promptPrefixForAgent(c.agent, townRoot, true); got != c.submit {
			t.Errorf("%q submit: got %q, want %q", c.agent, got, c.submit)
		}
	}
}
