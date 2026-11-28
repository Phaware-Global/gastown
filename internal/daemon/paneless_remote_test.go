package daemon

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/execution"
)

// panelessBackend is a Backend whose Discover returns a fixed session list and
// whose Teardown records the sessions it was asked to release.
type panelessBackend struct {
	execution.LocalBackend
	sessions    []string
	tornDown    []string
	teardownErr error
}

func (b *panelessBackend) Discover(_ context.Context, filter execution.IdentityTags) ([]execution.Endpoint, error) {
	var eps []execution.Endpoint
	for _, s := range b.sessions {
		eps = append(eps, execution.Endpoint{ID: s, Identity: execution.IdentityTags{Rig: filter.Rig, Session: s}})
	}
	return eps, nil
}

func (b *panelessBackend) Teardown(_ context.Context, ep execution.Endpoint) error {
	if b.teardownErr != nil {
		return b.teardownErr
	}
	b.tornDown = append(b.tornDown, ep.Identity.Session)
	return nil
}

func panelessTestDaemon() (*Daemon, *strings.Builder) {
	var logBuf strings.Builder
	return &Daemon{ctx: context.Background(), logger: log.New(&logBuf, "", 0)}, &logBuf
}

func sessionsAlive(alive ...string) func(string) (bool, error) {
	return func(name string) (bool, error) {
		for _, a := range alive {
			if a == name {
				return true, nil
			}
		}
		return false, nil
	}
}

// A worker session is released only after its host session has stayed gone
// for the whole timeout, measured from the first pass that saw it gone.
func TestReapPanelessRemoteSessions_ReleasesAfterTimeout(t *testing.T) {
	d, _ := panelessTestDaemon()
	b := &panelessBackend{sessions: []string{"r-live", "r-gone"}}
	alive := sessionsAlive("r-live")
	t0 := time.Now()

	d.reapPanelessRemoteSessions("myr", b, alive, t0, 15*time.Minute)
	d.reapPanelessRemoteSessions("myr", b, alive, t0.Add(14*time.Minute), 15*time.Minute)
	if len(b.tornDown) != 0 {
		t.Fatalf("released before the timeout: %v", b.tornDown)
	}

	d.reapPanelessRemoteSessions("myr", b, alive, t0.Add(15*time.Minute), 15*time.Minute)
	if len(b.tornDown) != 1 || b.tornDown[0] != "r-gone" {
		t.Fatalf("tornDown = %v, want [r-gone]", b.tornDown)
	}
	if len(d.panelessSince) != 0 {
		t.Errorf("record kept after release: %v", d.panelessSince)
	}
}

// A host session that comes back (a restart onto the existing worker session)
// restarts the wait.
func TestReapPanelessRemoteSessions_ReturningSessionResetsWait(t *testing.T) {
	d, _ := panelessTestDaemon()
	b := &panelessBackend{sessions: []string{"r-cat"}}
	t0 := time.Now()

	d.reapPanelessRemoteSessions("myr", b, sessionsAlive(), t0, 15*time.Minute)
	d.reapPanelessRemoteSessions("myr", b, sessionsAlive("r-cat"), t0.Add(10*time.Minute), 15*time.Minute)
	d.reapPanelessRemoteSessions("myr", b, sessionsAlive(), t0.Add(20*time.Minute), 15*time.Minute)
	d.reapPanelessRemoteSessions("myr", b, sessionsAlive(), t0.Add(30*time.Minute), 15*time.Minute)
	if len(b.tornDown) != 0 {
		t.Fatalf("released %v only 10m after the session was last alive", b.tornDown)
	}
}

// An unknown tmux state neither starts nor resets the wait, a failed teardown
// is retried, and a session the worker no longer reports is forgotten.
func TestReapPanelessRemoteSessions_ErrorsAndVanishedSessions(t *testing.T) {
	d, logBuf := panelessTestDaemon()
	b := &panelessBackend{sessions: []string{"r-cat"}, teardownErr: errors.New("worker unreachable")}
	tmuxDown := func(string) (bool, error) { return false, errors.New("no server") }
	t0 := time.Now()

	d.reapPanelessRemoteSessions("myr", b, tmuxDown, t0, time.Minute)
	if len(d.panelessSince) != 0 {
		t.Fatalf("tmux error started the wait: %v", d.panelessSince)
	}

	d.reapPanelessRemoteSessions("myr", b, sessionsAlive(), t0, time.Minute)
	d.reapPanelessRemoteSessions("myr", b, sessionsAlive(), t0.Add(time.Minute), time.Minute)
	if !strings.Contains(logBuf.String(), "worker unreachable") || len(d.panelessSince) != 1 {
		t.Fatalf("failed teardown must be logged and kept for retry: log=%q records=%v", logBuf.String(), d.panelessSince)
	}

	b.teardownErr = nil
	d.reapPanelessRemoteSessions("myr", b, sessionsAlive(), t0.Add(2*time.Minute), time.Minute)
	if len(b.tornDown) != 1 {
		t.Fatalf("teardown not retried: %v", b.tornDown)
	}

	d.reapPanelessRemoteSessions("myr", b, sessionsAlive(), t0.Add(3*time.Minute), time.Minute)
	d.panelessSince["other/r-cat"] = t0
	b.sessions = nil
	d.reapPanelessRemoteSessions("myr", b, sessionsAlive(), t0.Add(4*time.Minute), time.Minute)
	if _, kept := d.panelessSince["myr/r-cat"]; kept {
		t.Errorf("record kept for a session the worker no longer reports")
	}
	if _, kept := d.panelessSince["other/r-cat"]; !kept {
		t.Errorf("another rig's record was dropped")
	}
}
