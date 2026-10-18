package sweep

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// PortTimeout bounds each port's Sweep call.
var PortTimeout = 2 * time.Minute

// MaxNewPerKind bounds how many new findings of one kind a report prints in
// full (urgent findings are never capped); the rest are counted on an
// overflow line. Only listed findings enter the pending run, so an ack
// never consumes an item the Mayor did not see by key: overflow items stay
// new and are promoted into the full list as listed items clear. The cap
// is a report limit, not truncation, so it never affects eviction.
const MaxNewPerKind = 25

// Options controls one run.
type Options struct {
	// Write records a pending state file so the run can be acknowledged.
	// Manual, --json and --dry-run invocations leave it false.
	Write bool
	// DryRun makes ports trace their external queries instead of running them.
	DryRun bool
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
		Ports:    map[string]Port{"beads": NewBeadsPort(townRoot), "jira": NewJiraPort(townRoot, os.Getenv), "github": NewGitHubPort(townRoot, os.Getenv)},
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
		in := Input{TownRoot: r.TownRoot, Settings: r.Settings, Now: now, DryRun: opts.DryRun}
		var qmu sync.Mutex
		var queries []string
		if opts.DryRun {
			in.Trace = func(q string) { qmu.Lock(); queries = append(queries, q); qmu.Unlock() }
		}
		findings, perr := runPort(ctx, port, in)
		qmu.Lock()
		res.Queries = append([]string(nil), queries...)
		qmu.Unlock()
		res.DryRun = opts.DryRun && len(res.Queries) > 0
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
		schema, templates := port.Schema(), port.Suggestions()
		standing := map[string]*Standing{}
		pend.Seen[name] = []string{} // the port ran, even if it found nothing
		for i := range findings {
			f := &findings[i]
			f.Port = name
			enforceSchema(schema, templates, f)
		}
		findings = mergeDuplicates(findings)
		var acked []Finding // standing findings plus the new ones listed in full
		for i := range findings {
			f := &findings[i]
			id := f.ID()
			if e, ok := state.Entries[id]; ok && e.Fingerprint == f.Fingerprint {
				acked = append(acked, *f)
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
		sort.SliceStable(res.New, func(i, j int) bool {
			a, b := res.New[i], res.New[j]
			if severityRank(a.Severity) != severityRank(b.Severity) {
				return severityRank(a.Severity) < severityRank(b.Severity)
			}
			if a.Since.IsZero() != b.Since.IsZero() {
				return !a.Since.IsZero() // dated before undated within a severity
			}
			return a.Since.Before(b.Since) // oldest first within a severity
		})
		res.New, res.Overflow = capPerKind(res.New, MaxNewPerKind)
		// Upserted at ack: standing findings plus the new ones listed in full.
		pend.Findings = append(pend.Findings, append(acked, res.New...)...)
		// Seen (for eviction): every finding the port returned, counted ones
		// included, so the report cap never evicts an acknowledged entry.
		for i := range findings {
			pend.Seen[name] = append(pend.Seen[name], findings[i].ID())
		}
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

// capPerKind keeps at most max findings of each kind (in the given order)
// and returns the rest as per-kind counts with the oldest Since and the
// worst severity among them. Urgent findings are never capped.
func capPerKind(findings []Finding, max int) ([]Finding, []Standing) {
	shown := map[string]int{}
	over := map[string]*Standing{}
	var kept []Finding
	for _, f := range findings {
		if f.Severity == SeverityUrgent || shown[f.Kind] < max {
			shown[f.Kind]++
			kept = append(kept, f)
			continue
		}
		o := over[f.Kind]
		if o == nil {
			o = &Standing{Kind: f.Kind}
			over[f.Kind] = o
		}
		o.Count++
		if o.Severity == "" || severityRank(f.Severity) < severityRank(o.Severity) {
			o.Severity = f.Severity
		}
		if o.Oldest.IsZero() || (!f.Since.IsZero() && f.Since.Before(o.Oldest)) {
			o.Oldest = f.Since
		}
	}
	var overflow []Standing
	for _, o := range over {
		overflow = append(overflow, *o)
	}
	sort.Slice(overflow, func(i, j int) bool { return overflow[i].Kind < overflow[j].Kind })
	return kept, overflow
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
// Kind; a port that breaks that rule must not defeat the ack gate). The
// merged finding keeps the highest severity and the earliest Since; every
// fingerprint joins it (sorted once, "+"-separated) so it is standing only
// when every duplicate was acknowledged; the later duplicates' URL, Suggest,
// Fields and External are kept under "<name>.2", "<name>.3", ….
func mergeDuplicates(findings []Finding) []Finding {
	byID := map[string]int{}
	fps := map[string][]string{}
	out := findings[:0]
	for _, f := range findings {
		id := f.ID()
		fps[id] = append(fps[id], f.Fingerprint)
		idx, seen := byID[id]
		if !seen {
			byID[id] = len(out)
			out = append(out, f)
			continue
		}
		m := &out[idx]
		n := len(fps[id])
		if severityRank(f.Severity) < severityRank(m.Severity) {
			m.Severity = f.Severity
		}
		if !f.Since.IsZero() && (m.Since.IsZero() || f.Since.Before(m.Since)) {
			m.Since = f.Since
		}
		if f.URL != "" {
			m.addExternal(fmt.Sprintf("url.%d", n), f.URL)
		}
		if f.Suggest != "" {
			m.addExternal(fmt.Sprintf("suggest.%d", n), f.Suggest)
		}
		for k, v := range f.Fields {
			m.addExternal(fmt.Sprintf("%s.%d", k, n), v)
		}
		for k, v := range f.External {
			m.addExternal(fmt.Sprintf("%s.%d", k, n), v)
		}
	}
	for i := range out {
		if parts := fps[out[i].ID()]; len(parts) > 1 {
			sort.Strings(parts)
			out[i].Fingerprint = strings.Join(parts, "+")
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
