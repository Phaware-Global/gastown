package plugin

import "github.com/steveyegge/gastown/internal/config"

// DefaultCooldown is used for cooldown gates that specify no duration.
const DefaultCooldown = "1h"

// CooldownDuration returns the effective cooldown for a cooldown gate.
//
// When the gate names a town setting via `duration_setting`, that setting's
// value wins over the static `duration` (which then only documents the
// fallback). Only settings listed here are recognised; an unknown name is
// ignored so a typo degrades to the static duration rather than disabling the
// gate. settings may be nil.
func (g *Gate) CooldownDuration(settings *config.TownSettings) string {
	if g == nil {
		return DefaultCooldown
	}
	switch g.DurationSetting {
	case "assistant_sweep_interval":
		// The Mayor's Assistant sweep cadence; defaults to 1h in config.
		return settings.AssistantSweepInterval()
	}
	if g.Duration == "" {
		return DefaultCooldown
	}
	return g.Duration
}
