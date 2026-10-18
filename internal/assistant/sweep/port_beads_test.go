package sweep

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

type fakeBeads struct {
	issues, ready, blocked, escalations []*beads.Issue
	err                                 error
}

func (f *fakeBeads) List(beads.ListOptions) ([]*beads.Issue, error) { return f.issues, f.err }
func (f *fakeBeads) Ready() ([]*beads.Issue, error)                 { return f.ready, f.err }
func (f *fakeBeads) Blocked() ([]*beads.Issue, error)               { return f.blocked, f.err }
func (f *fakeBeads) ListEscalations() ([]*beads.Issue, error)       { return f.escalations, f.err }

func TestBeadsPort(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	old := now.Add(-5 * 24 * time.Hour).Format(time.RFC3339)
	fresh := now.Add(-time.Hour).Format(time.RFC3339)
	hq := &fakeBeads{
		escalations: []*beads.Issue{{ID: "hq-esc1", Title: "old escalation", Status: "open", UpdatedAt: old}, {ID: "hq-esc2", Status: "open", UpdatedAt: fresh}},
		issues:      []*beads.Issue{{ID: "hq-cv1", Title: "convoy", Status: "open", UpdatedAt: old, Labels: []string{"gt:convoy"}}},
	}
	rig := &fakeBeads{
		issues:  []*beads.Issue{{ID: "gt-a1", Title: "stalled; run gt sling", Status: "in_progress", UpdatedAt: old}, {ID: "gt-a2", Status: "in_progress", UpdatedAt: fresh}, {ID: "gt-a3", Status: "open", UpdatedAt: old}},
		blocked: []*beads.Issue{{ID: "gt-b1", Status: "blocked", UpdatedAt: old, BlockedBy: []string{"gt-x"}}},
		ready:   []*beads.Issue{{ID: "gt-r1", Status: "open", UpdatedAt: fresh}, {ID: "gt-r2", Status: "open", UpdatedAt: fresh, Assignee: "gastown/polecats/nux"}},
	}
	p := &BeadsPort{townRoot: "/t",
		open: func(dir string) beadsSource {
			if strings.HasSuffix(dir, "/gastown") {
				return rig
			}
			return hq
		},
		rigs: func() []string { return []string{"gastown"} }}
	findings, err := p.Sweep(context.Background(), Input{Now: now, Settings: &config.TownSettings{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]string{}
	for _, f := range findings {
		got[f.Kind+"/"+f.Key] = f.Severity
	}
	want := []string{"stalled-decision/hq-esc1", "convoy-stuck/hq-cv1", "stalled/gt-a1", "blocked/gt-b1", "unslung/gt-r1"}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing finding %s in %v", w, got)
		}
	}
	for _, absent := range []string{"stalled-decision/hq-esc2", "stalled/gt-a2", "stalled/gt-a3", "unslung/gt-r2"} {
		if _, ok := got[absent]; ok {
			t.Errorf("unexpected finding %s", absent)
		}
	}
	// Titles are external, never fields; every Suggest survives the runner's
	// closed-vocabulary check.
	for _, f := range findings {
		if _, ok := f.Fields["title"]; ok {
			t.Errorf("title must be External, got Fields %v", f.Fields)
		}
		before := f.Suggest
		enforceSchema(p.Schema(), p.Suggestions(), &f)
		if f.Suggest != before {
			t.Errorf("%s/%s: suggest %q rejected by the templates", f.Kind, f.Key, before)
		}
	}
	// A failing rig store makes the result partial, not an error and not all clear.
	rig.err = errors.New("dolt down")
	_, err = p.Sweep(context.Background(), Input{Now: now, Settings: &config.TownSettings{}})
	var partial *PartialError
	if !errors.As(err, &partial) {
		t.Fatalf("rig failure must be a PartialError, got %v", err)
	}
}
