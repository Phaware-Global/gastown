package sweep

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// PortTimeout bounds each port's Sweep call.
const PortTimeout = 2 * time.Minute

// Options controls one run.
type Options struct {
	// Write records a pending state file so the run can be acknowledged.
	// Manual, --json and --dry-run invocations leave it false.
	Write bool
}

// Runner executes the enabled ports against the acknowledged state.
type Runner struct {
	TownRoot string
	Settings *config.TownSettings
	Ports    map[string]Port
	Store    *Store
	Now      func() time.Time
}

// NewRunner wires the built-in ports for a town.
func NewRunner(townRoot string, settings *config.TownSettings) *Runner {
	return &Runner{
		TownRoot: townRoot,
		Settings: settings,
		Ports:    map[string]Port{},
		Store:    NewStore(townRoot),
		Now:      time.Now,
	}
}

// Run executes every enabled port, dedupes against the acknowledged state,
// enforces field schemas, and (when opts.Write) records a pending run.
func (r *Runner) Run(ctx context.Context, opts Options) (*Report, error) {
	now := r.Now()
	state, err := r.Store.Load()
	if err != nil {
		return nil, fmt.Errorf("loading sweep state: %w", err)
	}
	rep := &Report{RunID: newRunID(), GeneratedAt: now}
	pend := &pending{RunID: rep.RunID, GeneratedAt: now, Seen: map[string][]string{}}

	for _, name := range r.Settings.SweepPorts() {
		res := PortResult{Name: name}
		port, ok := r.Ports[name]
		if !ok {
			res.Error = fmt.Sprintf("unknown port %q (not checked)", name)
			rep.Ports = append(rep.Ports, res)
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, PortTimeout)
		findings, perr := port.Sweep(pctx, Input{TownRoot: r.TownRoot, Settings: r.Settings, Now: now})
		cancel()
		var partial *PartialError
		switch {
		case errors.As(perr, &partial):
			res.Partial = true
			res.Error = "" // findings below are usable; absence is not
			res.New = append(res.New, Finding{Port: name, Kind: "port-partial", Key: name, Severity: SeverityInfo,
				Fields: map[string]string{"reason": "partial"}, External: map[string]string{"detail": partial.Reason}, Fingerprint: "partial"})
		case perr != nil:
			res.Error = perr.Error() + " (not checked)"
			rep.Ports = append(rep.Ports, res)
			continue
		}
		schema := port.Schema()
		standing := map[string]*Standing{}
		for i := range findings {
			f := &findings[i]
			f.Port = name
			enforceSchema(schema, f)
			id := f.ID()
			pend.Seen[name] = append(pend.Seen[name], id)
			pend.Findings = append(pend.Findings, *f)
			if e, ok := state.Entries[id]; ok && e.Fingerprint == f.Fingerprint {
				s := standing[f.Kind]
				if s == nil {
					s = &Standing{Kind: f.Kind}
					standing[f.Kind] = s
				}
				s.Count++
				if s.Oldest.IsZero() || (!f.Since.IsZero() && f.Since.Before(s.Oldest)) {
					s.Oldest = f.Since
				}
				continue
			}
			res.New = append(res.New, *f)
		}
		for _, s := range standing {
			res.Standing = append(res.Standing, *s)
		}
		sort.Slice(res.Standing, func(i, j int) bool { return res.Standing[i].Kind < res.Standing[j].Kind })
		sort.SliceStable(res.New, func(i, j int) bool { return severityRank(res.New[i].Severity) < severityRank(res.New[j].Severity) })
		if res.Clean() {
			pend.CleanPorts = append(pend.CleanPorts, name)
		}
		rep.Ports = append(rep.Ports, res)
	}

	if opts.Write {
		if err := r.Store.WritePending(pend); err != nil {
			return nil, fmt.Errorf("recording pending sweep run: %w", err)
		}
		rep.Pending = true
	}
	return rep, nil
}

func severityRank(s string) int {
	switch s {
	case SeverityUrgent:
		return 0
	case SeverityActionable:
		return 1
	default:
		return 2
	}
}

func newRunID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return hex.EncodeToString(b[:])
}
