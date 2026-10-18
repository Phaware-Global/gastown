package sweep

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// DefaultBeadsStaleAfter is the beads port's staleness window when unset.
const DefaultBeadsStaleAfter = 72 * time.Hour

// beadsSource is the slice of *beads.Beads the port needs, so tests can
// substitute a fake.
type beadsSource interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	Ready() ([]*beads.Issue, error)
	Blocked() ([]*beads.Issue, error)
	ListEscalations() ([]*beads.Issue, error)
}

// BeadsPort reports stalled decisions, stalled or blocked beads, unslung
// ready work and stuck convoys from the town (hq) store and each rig store.
type BeadsPort struct {
	townRoot string
	// open returns the beads client for a directory; replaced in tests.
	open func(dir string) beadsSource
	// rigs returns the registered rig names; replaced in tests.
	rigs func() []string
}

// NewBeadsPort returns the production beads port.
func NewBeadsPort(townRoot string) *BeadsPort {
	return &BeadsPort{
		townRoot: townRoot,
		open:     func(dir string) beadsSource { return beads.New(dir) },
		rigs: func() []string {
			cfg, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
			if err != nil || cfg == nil {
				return nil
			}
			var names []string
			for name := range cfg.Rigs {
				names = append(names, name)
			}
			return names
		},
	}
}

func (p *BeadsPort) Name() string { return "beads" }

func (p *BeadsPort) Suggestions() []string {
	return []string{
		"decide or `gt escalate ack {key}`",
		"review `gt convoy status {key}`",
		"reassign, sling onward, or close with reason",
		"chase the blocker",
		"`gt sling {key} {rig}`",
	}
}

// Schema: every bead field the port emits has a machine shape.
func (p *BeadsPort) Schema() map[string]Shape {
	return map[string]Shape{
		"status":   ShapeEnum,
		"priority": ShapeInt,
		"updated":  ShapeTime,
		"rig":      ShapeEnum,
		"blockers": ShapeInt,
	}
}

func staleAfter(in Input) time.Duration {
	if raw := in.Settings.BeadsSweep().StaleAfter; raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return DefaultBeadsStaleAfter
}

// Sweep runs the five beads checks. Rig store failures are reported as a
// PartialError so the runner keeps that rig's acknowledged entries.
func (p *BeadsPort) Sweep(ctx context.Context, in Input) ([]Finding, error) {
	stale := staleAfter(in)
	var out []Finding
	var partial []string

	hq := p.open(beads.ResolveBeadsDir(p.townRoot))
	esc, err := hq.ListEscalations()
	if err != nil {
		partial = append(partial, "escalations: "+err.Error())
	}
	for _, i := range esc {
		if age := in.Now.Sub(updatedAt(i, in.Now)); age >= stale {
			out = append(out, p.finding("stalled-decision", "hq", i, in.Now, SeverityActionable,
				fmt.Sprintf("decide or `gt escalate ack %s`", i.ID)))
		}
	}
	convoys, err := hq.List(beads.ListOptions{Status: "open", Label: "gt:convoy", Priority: -1, Limit: 500})
	if err != nil {
		partial = append(partial, "convoys: "+err.Error())
	}
	for _, i := range convoys {
		if in.Now.Sub(updatedAt(i, in.Now)) >= stale {
			out = append(out, p.finding("convoy-stuck", "hq", i, in.Now, SeverityActionable, fmt.Sprintf("review `gt convoy status %s`", i.ID)))
		}
	}

	for _, rig := range p.rigs() {
		if ctx.Err() != nil {
			partial = append(partial, "timeout before rig "+rig)
			break
		}
		src := p.open(filepath.Join(p.townRoot, rig))
		open, err := src.List(beads.ListOptions{Status: "open", Priority: -1, Limit: 1000})
		if err != nil {
			partial = append(partial, rig+": list: "+err.Error())
		}
		for _, i := range open {
			if (i.Status == "in_progress" || i.Status == "hooked") && in.Now.Sub(updatedAt(i, in.Now)) >= stale {
				out = append(out, p.finding("stalled", rig, i, in.Now, SeverityActionable, "reassign, sling onward, or close with reason"))
			}
		}
		blocked, err := src.Blocked()
		if err != nil {
			partial = append(partial, rig+": blocked: "+err.Error())
		}
		for _, i := range blocked {
			if in.Now.Sub(updatedAt(i, in.Now)) >= stale {
				f := p.finding("blocked", rig, i, in.Now, SeverityActionable, "chase the blocker")
				f.Fields["blockers"] = fmt.Sprint(len(i.BlockedBy))
				out = append(out, f)
			}
		}
		ready, err := src.Ready()
		if err != nil {
			partial = append(partial, rig+": ready: "+err.Error())
		}
		for _, i := range ready {
			if i.HookBead == "" && i.Assignee == "" {
				out = append(out, p.finding("unslung", rig, i, in.Now, SeverityInfo, fmt.Sprintf("`gt sling %s %s`", i.ID, rig)))
			}
		}
	}
	if len(partial) > 0 {
		return out, &PartialError{Reason: strings.Join(partial, "; ")}
	}
	return out, nil
}

func (p *BeadsPort) finding(kind, rig string, i *beads.Issue, now time.Time, severity, suggest string) Finding {
	upd := updatedAt(i, now)
	return Finding{
		Kind: kind, Key: i.ID, Since: upd, Severity: severity, Suggest: suggest,
		Fields:      map[string]string{"status": i.Status, "priority": fmt.Sprint(i.Priority), "updated": upd.UTC().Format(time.RFC3339), "rig": rig},
		External:    map[string]string{"title": i.Title},
		Fingerprint: i.Status + "|" + i.UpdatedAt + "|" + i.Assignee,
	}
}

// updatedAt parses a bead timestamp in either format bd emits; an unparseable
// value is treated as "now" so the bead is never falsely reported as stale.
func updatedAt(i *beads.Issue, now time.Time) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, i.UpdatedAt); err == nil {
			return t
		}
	}
	return now
}
