package config

import "time"

// DefaultAssistantBriefInterval is the cooldown of the assistant-brief plugin
// when settings/config.json has no assistant_brief_interval.
const DefaultAssistantBriefInterval = "15m"

// AssistantBriefIntervalSetting returns the configured brief cadence and true
// when settings/config.json carries a valid value (parseable, at least
// MinAssistantSweepInterval, which guards against the same mail flood). It
// returns ("", false) otherwise so the plugin gate falls back to its static
// duration. Safe to call on a nil receiver.
func (s *TownSettings) AssistantBriefIntervalSetting() (string, bool) {
	if s == nil || s.AssistantBriefIntervalRaw == "" {
		return "", false
	}
	d, err := time.ParseDuration(s.AssistantBriefIntervalRaw)
	if err != nil || d < MinAssistantSweepInterval {
		return "", false
	}
	return s.AssistantBriefIntervalRaw, true
}
