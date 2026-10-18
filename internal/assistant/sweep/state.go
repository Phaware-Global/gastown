package sweep

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry is the acknowledged state of one finding.
type Entry struct {
	Fingerprint string    `json:"fingerprint"`
	FirstSeen   time.Time `json:"first_seen"`
	LastAcked   time.Time `json:"last_acked"`
}

// State is the acknowledged dedup state: fingerprints the Mayor has actually
// been told about. It advances only through Ack.
type State struct {
	Entries map[string]Entry `json:"entries"` // keyed by Finding.ID()
}

// pending is what a run wrote and Ack commits.
type pending struct {
	RunID       string              `json:"run_id"`
	GeneratedAt time.Time           `json:"generated_at"`
	Findings    []Finding           `json:"findings"`     // every finding reported (new and standing)
	CleanPorts  []string            `json:"clean_ports"`  // ports that completed without error or truncation
	Seen        map[string][]string `json:"seen_by_port"` // port → IDs present in this run
}

// Store persists state under <town>/assistant/.runtime.
type Store struct{ dir string }

// NewStore returns the store for a town.
func NewStore(townRoot string) *Store {
	return &Store{dir: filepath.Join(townRoot, "assistant", ".runtime")}
}

func (s *Store) statePath() string { return filepath.Join(s.dir, "sweep-state.json") }
func (s *Store) pendingPath(run string) string {
	return filepath.Join(s.dir, "sweep-pending-"+run+".json")
}

// Load returns the acknowledged state (empty when none exists).
func (s *Store) Load() (*State, error) {
	st := &State{Entries: map[string]Entry{}}
	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.statePath(), err)
	}
	if st.Entries == nil {
		st.Entries = map[string]Entry{}
	}
	return st, nil
}

func (s *Store) writeJSON(path string, v any) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WritePending records a run for a later Ack.
func (s *Store) WritePending(p *pending) error { return s.writeJSON(s.pendingPath(p.RunID), p) }

// Ack commits a pending run: every reported fingerprint is upserted (the
// Mayor was told about it, whatever happened to the rest of its port), and
// entries absent from a port that completed cleanly are evicted so a
// recurrence is new again. Ports that failed or were partial keep their
// previously acknowledged entries untouched.
func (s *Store) Ack(runID string, now time.Time) error {
	if strings.ContainsAny(runID, "/\\.") || runID == "" {
		return fmt.Errorf("invalid run id %q", runID)
	}
	data, err := os.ReadFile(s.pendingPath(runID))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no pending sweep run %q (already acknowledged, or the run was --dry-run/--json)", runID)
	}
	if err != nil {
		return err
	}
	var p pending
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("parsing pending run: %w", err)
	}
	st, err := s.Load()
	if err != nil {
		return err
	}
	clean := map[string]bool{}
	for _, c := range p.CleanPorts {
		clean[c] = true
	}
	seen := map[string]bool{}
	for _, f := range p.Findings {
		id := f.ID()
		seen[id] = true
		e, ok := st.Entries[id]
		if !ok {
			e = Entry{FirstSeen: f.Since}
			if e.FirstSeen.IsZero() {
				e.FirstSeen = now
			}
		}
		e.Fingerprint = f.Fingerprint
		e.LastAcked = now
		st.Entries[id] = e
	}
	for id := range st.Entries {
		port := strings.SplitN(id, "|", 2)[0]
		if clean[port] && !seen[id] {
			delete(st.Entries, id) // condition cleared on a clean run
		}
	}
	if err := s.writeJSON(s.statePath(), st); err != nil {
		return err
	}
	return os.Remove(s.pendingPath(runID))
}
