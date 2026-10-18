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
	// PortRunAt is GeneratedAt of the newest acknowledged run per port. An
	// older run acknowledged later neither upserts nor evicts for that port,
	// so it can neither roll a fingerprint back nor resurrect an evicted entry.
	PortRunAt map[string]time.Time `json:"port_run_at,omitempty"`
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
	st := &State{Entries: map[string]Entry{}, PortRunAt: map[string]time.Time{}}
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
	if st.PortRunAt == nil {
		st.PortRunAt = map[string]time.Time{}
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
	tmp, err := os.CreateTemp(s.dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// lock serializes Ack across processes with an exclusive-create lock file;
// a lock older than lockStale is treated as abandoned.
const lockStale = time.Minute

func (s *Store) lock() (release func(), err error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dir, "sweep-state.lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, statErr := os.Stat(path); statErr == nil && time.Since(fi.ModTime()) > lockStale {
			// Claim the stale lock by renaming it: only one waiter can win
			// the rename, the others see ErrNotExist and retry the create.
			stale := fmt.Sprintf("%s.stale.%d", path, time.Now().UnixNano())
			if os.Rename(path, stale) == nil {
				os.Remove(stale)
			}
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("sweep state is locked by another ack (%s)", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
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
	release, err := s.lock()
	if err != nil {
		return err
	}
	defer release()
	st, err := s.Load()
	if err != nil {
		return err
	}
	clean := map[string]bool{}
	for _, c := range p.CleanPorts {
		clean[c] = true
	}
	// A port whose newer run was already acknowledged is frozen for this
	// (older) run: no upserts, no evictions.
	frozen := map[string]bool{}
	for port, at := range st.PortRunAt {
		if at.After(p.GeneratedAt) {
			frozen[port] = true
		}
	}
	seen := map[string]bool{}
	for _, f := range p.Findings {
		id := f.ID()
		seen[id] = true
		if frozen[f.Port] {
			continue
		}
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
		if clean[port] && !seen[id] && !frozen[port] {
			delete(st.Entries, id) // condition cleared on a clean run
		}
	}
	for port := range p.Seen {
		if !frozen[port] {
			st.PortRunAt[port] = p.GeneratedAt
		}
	}
	if err := s.writeJSON(s.statePath(), st); err != nil {
		return err
	}
	return os.Remove(s.pendingPath(runID))
}
