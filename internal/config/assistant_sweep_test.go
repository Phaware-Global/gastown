package config

import "testing"

func TestAssistantSweepInterval(t *testing.T) {
	t.Parallel()
	var nilSettings *TownSettings
	if got := nilSettings.AssistantSweepInterval(); got != "1h" {
		t.Errorf("nil settings: got %q, want 1h", got)
	}
	cases := map[string]string{"": "1h", "30m": "30m", "2h": "2h", "bogus": "1h", "-5m": "1h", "0s": "1h"}
	for raw, want := range cases {
		if got := (&TownSettings{AssistantSweepIntervalRaw: raw}).AssistantSweepInterval(); got != want {
			t.Errorf("raw %q: got %q, want %q", raw, got, want)
		}
	}
}
