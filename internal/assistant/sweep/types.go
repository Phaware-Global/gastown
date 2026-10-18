// Package sweep runs the Mayor's Assistant loose-ends sweep: a set of
// deterministic ports (beads, and later Jira and GitHub) that report findings
// the assistant turns into a SWEEP: mail for the Mayor.
//
// Design: docs/design/assistant-role.md, "Sweep ports". The runner owns the
// trust boundary (every human-authored value is fenced before an agent reads
// it) and the dedup state advances only when the assistant acknowledges a run
// after mailing the Mayor.
package sweep

import (
	"context"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// Severity classifies a finding for the assistant's briefing.
const (
	SeverityUrgent     = "urgent"
	SeverityActionable = "actionable"
	SeverityInfo       = "info"
)

// Input is what a port receives for one run.
type Input struct {
	TownRoot string
	Settings *config.TownSettings
	Now      time.Time
}

// Port is one deterministic source of sweep findings.
type Port interface {
	// Name is the port's identifier ("beads", "jira", "github").
	Name() string
	// Schema declares the Fields keys this port may emit and their shapes.
	// A value that fails its shape, or a key not declared here, is moved to
	// External by the runner.
	Schema() map[string]Shape
	// Suggestions declares the port's Suggest templates, e.g.
	// "gt sling {key} {rig}". A finding's Suggest is printed only when it
	// equals one of them with {key} replaced by the finding's Key and every
	// other placeholder by that validated Fields value; anything else is
	// fenced. Provider text therefore never reaches the Suggest line.
	Suggestions() []string
	// Sweep returns the port's findings. Key must be unique per Kind within
	// one run (the runner merges duplicates so they cannot defeat the ack
	// gate, but their fingerprints then move together). A partial result
	// (truncated pagination, timeout) must be reported through *PartialError
	// so the runner can keep acknowledged entries for this port. Sweep must
	// honor ctx: the runner stops waiting at PortTimeout.
	Sweep(ctx context.Context, in Input) ([]Finding, error)
}

// Finding is one condition a port observed.
type Finding struct {
	Port     string            `json:"port"`
	Kind     string            `json:"kind"`
	Key      string            `json:"key"`
	URL      string            `json:"url,omitempty"`
	Since    time.Time         `json:"since,omitempty"`
	Severity string            `json:"severity"`
	Suggest  string            `json:"suggest,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
	External map[string]string `json:"external,omitempty"`
	// Fingerprint identifies the condition's state (comment ID, head SHA,
	// status, updated-at). A changed fingerprint makes the finding new again.
	Fingerprint string `json:"fingerprint"`
}

// ID is the dedup key (port, kind, key).
func (f Finding) ID() string { return f.Port + "|" + f.Kind + "|" + f.Key }

// PartialError marks a port result as incomplete: findings may be missing,
// so the runner must not evict this port's acknowledged entries.
type PartialError struct{ Reason string }

func (e *PartialError) Error() string { return "partial result: " + e.Reason }

// Standing summarizes acknowledged, unchanged findings of one kind.
type Standing struct {
	Kind     string    `json:"kind"`
	Severity string    `json:"severity,omitempty"` // overflow only: worst severity among the counted findings
	Count    int       `json:"count"`
	Oldest   time.Time `json:"oldest,omitempty"`
}

// PortResult is one port's contribution to a report.
type PortResult struct {
	Name     string     `json:"name"`
	Error    string     `json:"error,omitempty"`    // port did not run or failed: NOT "all clear"
	Partial  bool       `json:"partial,omitempty"`  // ran but may be missing findings
	New      []Finding  `json:"new,omitempty"`      // fingerprint differs from the acknowledged one
	Standing []Standing `json:"standing,omitempty"` // acknowledged and unchanged, counts only
	Overflow []Standing `json:"overflow,omitempty"` // new findings beyond MaxNewPerKind, counts only
}

// Clean reports whether the port completed without error or truncation, which
// is the condition for evicting its absent entries at --ack.
func (r PortResult) Clean() bool { return r.Error == "" && !r.Partial }

// Report is the output of one run.
type Report struct {
	RunID       string       `json:"run_id"`
	GeneratedAt time.Time    `json:"generated_at"`
	Ports       []PortResult `json:"ports"`
	// Pending is true when the run wrote a pending state file that
	// `gt assistant sweep --ack <run_id>` commits.
	Pending bool `json:"pending"`
}
