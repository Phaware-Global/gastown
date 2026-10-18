package sweep

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// PortTimeout bounds each port's Sweep call.
var PortTimeout = 2 * time.Minute

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
		findings, perr := runPort(ctx, port, Input{TownRoot: r.TownRoot, Settings: r.Settings, Now: now})
		var partial *PartialError
		switch {
		case errors.As(perr, &partial):
			res.Partial = true
			res.Error = "" // findings below are usable; absence is not
			res.New = append(res.New, Finding{Port: name, Kind: "port-partial", Key: name, Severity: SeverityInfo,
				External: map[string]string{"detail": partial.Reason}, Fingerprint: "partial"})
		case perr != nil:
			res.Error = perr.Error() // external text: rendered fenced, never as a bare line
			rep.Ports = append(rep.Ports, res)
			continue
		}
		schema := port.Schema()
		standing := map[string]*Standing{}
		for i := range findings {
			f := &findings[i]
			f.Port = name
			enforceSchema(schema, f)
		}
		findings = mergeDuplicates(findings)
		for i := range findings {
			f := &findings[i]
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

// runPort calls port.Sweep with PortTimeout and returns when the port does or
// when the timeout fires, whichever is first, so a port that ignores ctx
// cannot block the run.
func runPort(ctx context.Context, port Port, in Input) ([]Finding, error) {
	pctx, cancel := context.WithTimeout(ctx, PortTimeout)
	defer cancel()
	type out struct {
		f   []Finding
		err error
	}
	ch := make(chan out, 1)
	go func() {
		f, err := port.Sweep(pctx, in)
		ch <- out{f, err}
	}()
	select {
	case o := <-ch:
		return o.f, o.err
	case <-pctx.Done():
		return nil, fmt.Errorf("port did not return within %s", PortTimeout)
	}
}

// mergeDuplicates folds findings that share an ID (Key must be unique per
// Kind; a port that breaks that rule must not defeat the ack gate). The first
// finding is kept; later fingerprints join it (sorted, "+"-separated) so the
// merged entry is standing only when every duplicate was acknowledged, and
// later External values are kept under "<key>.2", "<key>.3", ….
func mergeDuplicates(findings []Finding) []Finding {
	byID := map[string]int{}
	out := findings[:0]
	for _, f := range findings {
		idx, seen := byID[f.ID()]
		if !seen {
			byID[f.ID()] = len(out)
			out = append(out, f)
			continue
		}
		m := &out[idx]
		fps := append(strings.Split(m.Fingerprint, "+"), f.Fingerprint)
		sort.Strings(fps)
		m.Fingerprint = strings.Join(fps, "+")
		n := strings.Count(m.Fingerprint, "+") + 1
		for k, v := range f.External {
			m.addExternal(fmt.Sprintf("%s.%d", k, n), v)
		}
	}
	return out
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
