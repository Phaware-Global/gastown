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

	// Jira configures the Jira port (disabled until site and projects are set).
	Jira *JiraSweepConfig `json:"jira,omitempty"`
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

// JiraSweepConfig configures the Jira port. Identity comes from
// telegraph.toml (telegraph.mayor.jira_account_ids); the API token from the
// environment variable named by TokenEnv. Durations are Go duration strings.
type JiraSweepConfig struct {
	Site     string   `json:"site,omitempty"`      // "example.atlassian.net"
	Email    string   `json:"email,omitempty"`     // persona's Atlassian login for basic auth
	TokenEnv string   `json:"token_env,omitempty"` // default "JIRA_API_TOKEN"
	Projects []string `json:"projects,omitempty"`  // project keys to sweep
	BoardID  int      `json:"board_id,omitempty"`  // Kanban board the issues should sit on (its render data decides membership); 0 disables off-board
	Humans   []string `json:"humans,omitempty"`    // account IDs the persona waits on
	Bots     []string `json:"bots,omitempty"`      // account IDs that are never humans

	ActiveStatuses []string `json:"active_statuses,omitempty"` // default To Do, Research, In Progress, In Review
	WIPStatuses    []string `json:"wip_statuses,omitempty"`    // counted against WIPLimit; default In Progress, Research
	WIPLimit       int      `json:"wip_limit,omitempty"`       // default 4

	UnansweredAfter   string `json:"unanswered_after,omitempty"`    // default "4h"
	ChaseAfter        string `json:"chase_after,omitempty"`         // default "24h"
	StatusUpdateAfter string `json:"status_update_after,omitempty"` // default "72h"
	OffBoardWindow    string `json:"off_board_window,omitempty"`    // default "168h"
	StaleAfter        string `json:"stale_after,omitempty"`         // default "72h"
}

// JiraSweep returns the Jira port configuration, never nil.
func (s *TownSettings) JiraSweep() *JiraSweepConfig {
	if s == nil || s.Assistant == nil || s.Assistant.Sweep == nil || s.Assistant.Sweep.Jira == nil {
		return &JiraSweepConfig{}
	}
	return s.Assistant.Sweep.Jira
}
