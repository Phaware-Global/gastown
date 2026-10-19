package sweep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// DefaultUnslungMaxAge is how old a ready, unassigned bead may be (by
// creation) and still count as unslung work: older beads are backlog the
// Mayor chose not to sling, and listing them every sweep buries the new ones.
const DefaultUnslungMaxAge = 14 * 24 * time.Hour

// Page caps: a result that fills its cap may be truncated, which the port
// reports as partial so standing entries beyond the cut are never evicted.
const (
	beadsListCap   = 1000 // list and ready page size (bd ready alone would cap at 100)
	beadsConvoyCap = 500
)

// beadsSource is the slice of *beads.Beads the port needs, so tests can
// substitute a fake.
type beadsSource interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	ReadyLimit(limit int) ([]*beads.Issue, error)
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
	rigs func() ([]string, error)
}

// NewBeadsPort returns the production beads port.
func NewBeadsPort(townRoot string) *BeadsPort {
	return &BeadsPort{
		townRoot: townRoot,
		open:     func(dir string) beadsSource { return beads.New(dir) },
		rigs: func() ([]string, error) {
			cfg, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
			if err != nil {
				return nil, err
			}
			if cfg == nil {
				return nil, fmt.Errorf("rigs registry %s is empty", constants.MayorRigsPath(townRoot))
			}
			var names []string
			for name := range cfg.Rigs {
				names = append(names, name)
			}
			return names, nil
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

// unslungMaxAge returns the unslung age cap: the configured duration, the
// default when unset or unparseable, and no cap (zero) when configured as "0".
func unslungMaxAge(in Input) time.Duration {
	raw := in.Settings.BeadsSweep().UnslungMaxAge
	if raw == "" {
		return DefaultUnslungMaxAge
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return DefaultUnslungMaxAge
	}
	return d
}

// createdAt parses a bead's creation time, falling back to its update time.
func createdAt(i *beads.Issue, now time.Time) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, i.CreatedAt); err == nil {
			return t
		}
	}
	return updatedAt(i, now)
}

// Sweep runs the five beads checks. Rig store failures are reported as a
// PartialError so the runner keeps that rig's acknowledged entries.
func (p *BeadsPort) Sweep(ctx context.Context, in Input) ([]Finding, error) {
	stale := staleAfter(in)
	maxAge := unslungMaxAge(in)
	var out []Finding
	var partial []string

	hq := p.open(beads.ResolveBeadsDir(p.townRoot))
	esc, err := hq.ListEscalations()
	if err != nil {
		partial = append(partial, "escalations: "+err.Error())
	}
	for _, i := range esc {
		if beads.HasLabel(i, "acked") {
			continue // the Mayor already acknowledged it; acking again only resets the clock
		}
		if age := in.Now.Sub(updatedAt(i, in.Now)); age >= stale {
			out = append(out, p.finding("stalled-decision", "hq", i, in.Now, SeverityActionable,
				fmt.Sprintf("decide or `gt escalate ack %s`", i.ID)))
		}
	}
	convoys, err := hq.List(beads.ListOptions{Status: "open", Label: "gt:convoy", Priority: -1, Limit: beadsConvoyCap})
	if err != nil {
		partial = append(partial, "convoys: "+err.Error())
	}
	if len(convoys) >= beadsConvoyCap {
		partial = append(partial, fmt.Sprintf("convoys truncated at %d", beadsConvoyCap))
	}
	for _, i := range convoys {
		if in.Now.Sub(updatedAt(i, in.Now)) >= stale {
			out = append(out, p.finding("convoy-stuck", "hq", i, in.Now, SeverityActionable, fmt.Sprintf("review `gt convoy status %s`", i.ID)))
		}
	}

	rigs, err := p.rigs()
	if err != nil {
		partial = append(partial, "rigs: "+err.Error())
	}
	for _, rig := range rigs {
		if ctx.Err() != nil {
			partial = append(partial, "timeout before rig "+rig)
			break
		}
		src := p.open(filepath.Join(p.townRoot, rig))
		// in_progress and hooked are stored statuses: "open" would never match
		// them, and asking for "hooked" also unions the hooked wisps.
		active, err := src.List(beads.ListOptions{Status: "in_progress,hooked", Priority: -1, Limit: beadsListCap})
		if err != nil {
			partial = append(partial, rig+": list: "+err.Error())
		}
		if len(active) >= beadsListCap {
			partial = append(partial, fmt.Sprintf("%s: active list truncated at %d", rig, beadsListCap))
		}
		for _, i := range active {
			if in.Now.Sub(updatedAt(i, in.Now)) >= stale {
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
		ready, err := src.ReadyLimit(beadsListCap)
		if err != nil {
			partial = append(partial, rig+": ready: "+err.Error())
		}
		if len(ready) >= beadsListCap {
			partial = append(partial, fmt.Sprintf("%s: ready list truncated at %d", rig, beadsListCap))
		}
		for _, i := range ready {
			if beads.HasLabel(i, "gt:escalation") || beads.IsProtectedBead(i) {
				continue // escalations and agent/rig/role identity beads are not slingable work
			}
			if i.HookBead != "" || i.Assignee != "" {
				continue
			}
			if maxAge > 0 && in.Now.Sub(createdAt(i, in.Now)) > maxAge {
				continue // backlog, not a loose end: older than unslung_max_age
			}
			out = append(out, p.finding("unslung", rig, i, in.Now, SeverityInfo, fmt.Sprintf("`gt sling %s %s`", i.ID, rig)))
		}
	}
	if len(partial) > 0 {
		return out, &PartialError{Reason: strings.Join(partial, "; ")}
	}
	return out, nil
}

// finding builds a Finding for a bead. A bead ID is operator-chosen text
// (`bd create --id` accepts anything with a hyphen), so an ID outside
// ShapeBeadID never reaches Key or Suggest: the Key becomes a hash
// placeholder, the raw ID is fenced under External["id"], and the
// suggestion (which would quote the ID) is dropped.
func (p *BeadsPort) finding(kind, rig string, i *beads.Issue, now time.Time, severity, suggest string) Finding {
	upd := updatedAt(i, now)
	f := Finding{
		Kind: kind, Key: i.ID, Since: upd, Severity: severity, Suggest: suggest,
		Fields:      map[string]string{"status": i.Status, "priority": fmt.Sprint(i.Priority), "updated": upd.UTC().Format(time.RFC3339), "rig": rig},
		External:    map[string]string{"title": i.Title},
		Fingerprint: i.Status + "|" + i.UpdatedAt + "|" + i.Assignee,
	}
	if !ValidShape(ShapeBeadID, i.ID) {
		sum := sha256.Sum256([]byte(i.ID))
		f.Key = "invalid-id-" + hex.EncodeToString(sum[:6])
		f.External["id"] = i.ID
		f.Suggest = ""
	}
	return f
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
