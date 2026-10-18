package config

// AssistantConfig holds settings for the Mayor's Assistant role
// (settings/config.json → "assistant").
type AssistantConfig struct {
	// Sweep configures the periodic loose-ends sweep ports.
	Sweep *SweepConfig `json:"sweep,omitempty"`
}

// SweepConfig selects and tunes the sweep ports run by `gt assistant sweep`.
// See docs/design/assistant-role.md, "Sweep ports".
type SweepConfig struct {
	// Ports lists the enabled ports in run order. Default: ["beads"].
	Ports []string `json:"ports,omitempty"`

	// Beads tunes the beads port.
	Beads *BeadsSweepConfig `json:"beads,omitempty"`
}

// BeadsSweepConfig tunes the beads port.
type BeadsSweepConfig struct {
	// StaleAfter is how long an in-progress bead, blocker, escalation or
	// convoy may go without an update before it is reported (default "3d").
	StaleAfter string `json:"stale_after,omitempty"`
}

// DefaultSweepPorts is the port list when assistant.sweep.ports is unset.
var DefaultSweepPorts = []string{"beads"}

// SweepPorts returns the enabled sweep ports, defaulting to DefaultSweepPorts.
// Safe to call on a nil receiver.
func (s *TownSettings) SweepPorts() []string {
	if s == nil || s.Assistant == nil || s.Assistant.Sweep == nil || len(s.Assistant.Sweep.Ports) == 0 {
		return append([]string(nil), DefaultSweepPorts...)
	}
	return append([]string(nil), s.Assistant.Sweep.Ports...)
}

// BeadsSweep returns the beads port configuration, never nil.
func (s *TownSettings) BeadsSweep() *BeadsSweepConfig {
	if s == nil || s.Assistant == nil || s.Assistant.Sweep == nil || s.Assistant.Sweep.Beads == nil {
		return &BeadsSweepConfig{}
	}
	return s.Assistant.Sweep.Beads
}
