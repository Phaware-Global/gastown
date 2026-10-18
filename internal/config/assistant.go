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

	// GitHub configures the GitHub port (repos come from the rig registry).
	GitHub *GitHubSweepConfig `json:"github,omitempty"`
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

	// Reviewers maps a GitHub login to the Jira account ID of the same
	// person, for the PR-linked kinds (review-handoff, done-not-in-uat).
	Reviewers map[string]string `json:"reviewers,omitempty"`
	// Status names used by the PR-linked kinds (defaults in parentheses).
	InReviewStatus   string `json:"in_review_status,omitempty"`   // "In Review"
	InProgressStatus string `json:"in_progress_status,omitempty"` // "In Progress"
	DoneStatus       string `json:"done_status,omitempty"`        // "Done"
	UATStatus        string `json:"uat_status,omitempty"`         // "In UAT"
	UATSweepAfter    string `json:"uat_sweep_after,omitempty"`    // default "24h"

	UnansweredAfter   string `json:"unanswered_after,omitempty"`    // default "4h"
	ChaseAfter        string `json:"chase_after,omitempty"`         // default "24h"
	UnansweredMaxAge  string `json:"unanswered_max_age,omitempty"`  // default "720h": an older wait is noise, not a finding
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

// GitHubSweepConfig configures the GitHub port. "Our" PRs are those authored
// by a login in telegraph.mayor.github_logins; repos are the rig registry's
// GitHub remotes plus telegraph.providers.github.extra_repos plus ExtraRepos.
type GitHubSweepConfig struct {
	TokenEnv   string   `json:"token_env,omitempty"`   // default "GITHUB_TOKEN"; falls back to `gh auth token`
	ExtraRepos []string `json:"extra_repos,omitempty"` // "owner/repo"
	Humans     []string `json:"humans,omitempty"`      // GitHub logins the persona waits on
	// ReleaseBranches maps "owner/repo" to path.Match patterns of release
	// branches; "*" is the default for repos not listed (default "release/*").
	ReleaseBranches map[string][]string `json:"release_branches,omitempty"`

	RemindAfter           string `json:"remind_after,omitempty"`            // default "24h"
	ApprovedUnmergedAfter string `json:"approved_unmerged_after,omitempty"` // default "4h"
	RespondAfter          string `json:"respond_after,omitempty"`           // default "12h"
	ReviewLoopAfter       string `json:"review_loop_after,omitempty"`       // default "2h"
	CIFailingAfter        string `json:"ci_failing_after,omitempty"`        // default "1h"
	ReleaseWindow         string `json:"release_window,omitempty"`          // default "336h" (14d)
	StaleAfter            string `json:"stale_after,omitempty"`             // default "168h" (7d)
}

// GitHubSweep returns the GitHub port configuration, never nil.
func (s *TownSettings) GitHubSweep() *GitHubSweepConfig {
	if s == nil || s.Assistant == nil || s.Assistant.Sweep == nil || s.Assistant.Sweep.GitHub == nil {
		return &GitHubSweepConfig{}
	}
	return s.Assistant.Sweep.GitHub
}
