package plugin

import "github.com/steveyegge/gastown/internal/config"

// DefaultCooldown is used for cooldown gates that specify no duration.
const DefaultCooldown = "1h"

// CooldownDuration returns the effective cooldown for a cooldown gate.
//
// Resolution order: a valid value of the town setting named by
// `duration_setting` (only settings listed here are recognized; an unknown
// name is ignored), then the gate's static `duration`, then DefaultCooldown.
// So the static duration really is the fallback when settings are missing,
// unparseable, below the setting's floor, or failed to load (nil settings).
func (g *Gate) CooldownDuration(settings *config.TownSettings) string {
	if g == nil {
		return DefaultCooldown
	}
	switch g.DurationSetting {
	case "assistant_sweep_interval":
		if v, ok := settings.AssistantSweepIntervalSetting(); ok {
			return v
		}
	case "assistant_brief_interval":
		if v, ok := settings.AssistantBriefIntervalSetting(); ok {
			return v
		}
	}
	if g.Duration == "" {
		return DefaultCooldown
	}
	return g.Duration
}
