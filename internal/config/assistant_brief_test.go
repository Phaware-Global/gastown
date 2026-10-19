package config

import "testing"

func TestAssistantBriefIntervalSetting(t *testing.T) {
	t.Parallel()
	var nilSettings *TownSettings
	if _, ok := nilSettings.AssistantBriefIntervalSetting(); ok {
		t.Error("nil settings must not count as configured")
	}
	for raw, want := range map[string]bool{"": false, "15m": true, "5m": true, "4m": false, "bogus": false, "-1m": false} {
		v, ok := (&TownSettings{AssistantBriefIntervalRaw: raw}).AssistantBriefIntervalSetting()
		if ok != want || (ok && v != raw) {
			t.Errorf("raw %q: got (%q, %v), want ok=%v", raw, v, ok, want)
		}
	}
}
