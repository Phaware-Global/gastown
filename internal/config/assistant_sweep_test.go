package config

import "testing"

func TestAssistantSweepInterval(t *testing.T) {
	t.Parallel()
	var nilSettings *TownSettings
	if got := nilSettings.AssistantSweepInterval(); got != "1h" {
		t.Errorf("nil settings: got %q, want 1h", got)
	}
	cases := map[string]string{"": "1h", "30m": "30m", "2h": "2h", "bogus": "1h", "-5m": "1h", "0s": "1h", "10s": "1h", "4m59s": "1h", "5m": "5m"}
	for raw, want := range cases {
		if got := (&TownSettings{AssistantSweepIntervalRaw: raw}).AssistantSweepInterval(); got != want {
			t.Errorf("raw %q: got %q, want %q", raw, got, want)
		}
	}
	if _, ok := (&TownSettings{AssistantSweepIntervalRaw: "10s"}).AssistantSweepIntervalSetting(); ok {
		t.Error("10s is below the floor and must not count as a configured setting")
	}
	if v, ok := (&TownSettings{AssistantSweepIntervalRaw: "45m"}).AssistantSweepIntervalSetting(); !ok || v != "45m" {
		t.Errorf("45m: got (%q, %v), want (45m, true)", v, ok)
	}
}
