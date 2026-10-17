package config

import "time"

// DefaultAssistantSweepInterval is the cooldown between Mayor's Assistant
// loose-ends sweeps when settings/config.json has no assistant_sweep_interval.
const DefaultAssistantSweepInterval = "1h"

// AssistantSweepInterval returns the configured sweep cadence for the Mayor's
// Assistant as a duration string, defaulting to DefaultAssistantSweepInterval
// when unset or unparseable. It is safe to call on a nil receiver.
//
// The value feeds the assistant-sweep plugin's cooldown gate (via
// `duration_setting = "assistant_sweep_interval"`) and the assistant's prompt.
func (s *TownSettings) AssistantSweepInterval() string {
	if s == nil || s.AssistantSweepIntervalRaw == "" {
		return DefaultAssistantSweepInterval
	}
	if d, err := time.ParseDuration(s.AssistantSweepIntervalRaw); err != nil || d <= 0 {
		return DefaultAssistantSweepInterval
	}
	return s.AssistantSweepIntervalRaw
}
