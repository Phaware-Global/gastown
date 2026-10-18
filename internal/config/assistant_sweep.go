package config

import "time"

// DefaultAssistantSweepInterval is the cooldown between Mayor's Assistant
// loose-ends sweeps when settings/config.json has no assistant_sweep_interval.
const DefaultAssistantSweepInterval = "1h"

// MinAssistantSweepInterval is the floor for assistant_sweep_interval. Every
// sweep produces trigger and SWEEP: mail (permanent beads and Dolt commits),
// so a value shorter than the daemon heartbeat would flood the town; a typo
// such as "1s" falls back to the default instead.
const MinAssistantSweepInterval = 5 * time.Minute

// AssistantSweepIntervalSetting returns the configured sweep cadence and true
// when settings/config.json carries a valid value (parseable, at least
// MinAssistantSweepInterval). It returns ("", false) when the setting is
// absent, unparseable, or too short, so callers can apply their own fallback.
// Safe to call on a nil receiver.
func (s *TownSettings) AssistantSweepIntervalSetting() (string, bool) {
	if s == nil || s.AssistantSweepIntervalRaw == "" {
		return "", false
	}
	d, err := time.ParseDuration(s.AssistantSweepIntervalRaw)
	if err != nil || d < MinAssistantSweepInterval {
		return "", false
	}
	return s.AssistantSweepIntervalRaw, true
}

// AssistantSweepInterval returns the configured sweep cadence for the Mayor's
// Assistant as a duration string, defaulting to DefaultAssistantSweepInterval
// when the setting is absent or invalid (see AssistantSweepIntervalSetting).
// It is safe to call on a nil receiver and is what the assistant's prompt
// renders; the plugin gate uses AssistantSweepIntervalSetting so that it can
// fall back to the plugin's own static duration first.
func (s *TownSettings) AssistantSweepInterval() string {
	if v, ok := s.AssistantSweepIntervalSetting(); ok {
		return v
	}
	return DefaultAssistantSweepInterval
}
