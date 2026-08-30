package polecat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

func TestTouchAndReadSessionHeartbeat(t *testing.T) {
	townRoot := t.TempDir()

	// No heartbeat initially
	hb := ReadSessionHeartbeat(townRoot, "gt-test-session")
	if hb != nil {
		t.Fatal("expected nil heartbeat before touch")
	}

	// Touch heartbeat
	TouchSessionHeartbeat(townRoot, "gt-test-session")

	// Read it back
	hb = ReadSessionHeartbeat(townRoot, "gt-test-session")
	if hb == nil {
		t.Fatal("expected non-nil heartbeat after touch")
	}

	if time.Since(hb.Timestamp) > 5*time.Second {
		t.Errorf("heartbeat timestamp too old: %v", hb.Timestamp)
	}

	// v2: TouchSessionHeartbeat writes state="working" by default (gt-3vr5)
	if hb.State != HeartbeatWorking {
		t.Errorf("heartbeat state = %q, want %q", hb.State, HeartbeatWorking)
	}
}

func TestTouchSessionHeartbeatWithState(t *testing.T) {
	townRoot := t.TempDir()

	TouchSessionHeartbeatWithState(townRoot, "gt-test-state", HeartbeatExiting, "gt done", "gt-abc123")

	hb := ReadSessionHeartbeat(townRoot, "gt-test-state")
	if hb == nil {
		t.Fatal("expected non-nil heartbeat after touch with state")
	}

	if hb.State != HeartbeatExiting {
		t.Errorf("state = %q, want %q", hb.State, HeartbeatExiting)
	}
	if hb.Context != "gt done" {
		t.Errorf("context = %q, want %q", hb.Context, "gt done")
	}
	if hb.Bead != "gt-abc123" {
		t.Errorf("bead = %q, want %q", hb.Bead, "gt-abc123")
	}
}

func TestSessionHeartbeat_EffectiveState(t *testing.T) {
	tests := []struct {
		name  string
		state HeartbeatState
		want  HeartbeatState
	}{
		{"empty (v1 compat)", "", HeartbeatWorking},
		{"working", HeartbeatWorking, HeartbeatWorking},
		{"idle", HeartbeatIdle, HeartbeatIdle},
		{"exiting", HeartbeatExiting, HeartbeatExiting},
		{"stuck", HeartbeatStuck, HeartbeatStuck},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hb := &SessionHeartbeat{State: tt.state}
			if got := hb.EffectiveState(); got != tt.want {
				t.Errorf("EffectiveState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionHeartbeat_IsV2(t *testing.T) {
	// v1 heartbeat (no state)
	v1 := &SessionHeartbeat{Timestamp: time.Now()}
	if v1.IsV2() {
		t.Error("expected IsV2()=false for v1 heartbeat")
	}

	// v2 heartbeat (has state)
	v2 := &SessionHeartbeat{Timestamp: time.Now(), State: HeartbeatWorking}
	if !v2.IsV2() {
		t.Error("expected IsV2()=true for v2 heartbeat")
	}
}

func TestIsSessionHeartbeatStale_NoFile(t *testing.T) {
	townRoot := t.TempDir()

	stale, exists := IsSessionHeartbeatStale(townRoot, "nonexistent")
	if exists {
		t.Error("expected exists=false for missing heartbeat")
	}
	if stale {
		t.Error("expected stale=false for missing heartbeat")
	}
}

func TestIsSessionHeartbeatStale_Fresh(t *testing.T) {
	townRoot := t.TempDir()

	TouchSessionHeartbeat(townRoot, "gt-test-fresh")

	stale, exists := IsSessionHeartbeatStale(townRoot, "gt-test-fresh")
	if !exists {
		t.Error("expected exists=true for fresh heartbeat")
	}
	if stale {
		t.Error("expected stale=false for fresh heartbeat")
	}
}

func TestIsSessionHeartbeatStale_Old(t *testing.T) {
	townRoot := t.TempDir()

	// Write a heartbeat with an old timestamp
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	oldTime := time.Now().Add(-10 * time.Minute).UTC()
	data := []byte(`{"timestamp":"` + oldTime.Format(time.RFC3339Nano) + `"}`)
	if err := os.WriteFile(filepath.Join(dir, "gt-test-stale.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	stale, exists := IsSessionHeartbeatStale(townRoot, "gt-test-stale")
	if !exists {
		t.Error("expected exists=true for old heartbeat")
	}
	if !stale {
		t.Error("expected stale=true for 10-minute-old heartbeat")
	}
}

func TestRemoveSessionHeartbeat(t *testing.T) {
	townRoot := t.TempDir()

	TouchSessionHeartbeat(townRoot, "gt-test-remove")

	// Verify it exists
	hb := ReadSessionHeartbeat(townRoot, "gt-test-remove")
	if hb == nil {
		t.Fatal("expected heartbeat to exist before removal")
	}

	// Remove it
	RemoveSessionHeartbeat(townRoot, "gt-test-remove")

	// Verify it's gone
	hb = ReadSessionHeartbeat(townRoot, "gt-test-remove")
	if hb != nil {
		t.Error("expected nil heartbeat after removal")
	}
}

func TestRemoveSessionHeartbeat_NoopOnMissing(t *testing.T) {
	townRoot := t.TempDir()
	// Should not panic or error on missing file
	RemoveSessionHeartbeat(townRoot, "nonexistent")
}

func TestIsSessionProcessDead_HeartbeatFresh(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-test-hb-alive"

	// Touch a fresh heartbeat — isSessionProcessDead should return false
	TouchSessionHeartbeat(townRoot, sessionName)

	dead := isSessionProcessDead(nil, sessionName, townRoot)
	if dead {
		t.Error("expected alive (dead=false) for session with fresh heartbeat")
	}
}

// fakeAgentProbe drives every branch of the corroboration logic without a live
// tmux server.
type fakeAgentProbe struct {
	hasSession    bool
	hasSessionErr error
	agentAlive    bool
	agentAliveErr error
	heartbeatOnly bool
	aliveCalls    int
}

func (f *fakeAgentProbe) HasSession(string) (bool, error)          { return f.hasSession, f.hasSessionErr }
func (f *fakeAgentProbe) AgentLivenessIsHeartbeatOnly(string) bool { return f.heartbeatOnly }
func (f *fakeAgentProbe) AgentAliveE(string) (bool, error) {
	f.aliveCalls++
	return f.agentAlive, f.agentAliveErr
}

// withProbe routes sessionAgentAlive at the fake while keeping the REAL
// corroboration logic in play, so these tests cover the production branches
// rather than a stub that replaces them.
func withProbe(t *testing.T, f *fakeAgentProbe) {
	t.Helper()
	real := sessionAgentAlive
	prev := sessionAgentAlive
	sessionAgentAlive = func(_ agentProbe, s string) (bool, bool) { return real(f, s) }
	t.Cleanup(func() { sessionAgentAlive = prev })
}

func writeStaleHeartbeat(t *testing.T, townRoot, sessionName string) {
	t.Helper()
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-20 * time.Minute).UTC()
	data := []byte(`{"timestamp":"` + oldTime.Format(time.RFC3339Nano) + `","state":"working"}`)
	if err := os.WriteFile(filepath.Join(dir, sessionName+".json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestSessionAgentAliveCorroboration covers the probe's own branches. Both
// directions are destructive if wrong: too lenient and a dead agent behind a
// surviving wrapper pane keeps its worktree and name-pool slot forever
// (hq-k1ot / np-tt5s, gt-jn40ft); too eager and a transient pgrep/ps failure
// reaps a healthy agent mid-task (gt-kncti).
func TestSessionAgentAliveCorroboration(t *testing.T) {
	tests := []struct {
		name      string
		probe     fakeAgentProbe
		wantAlive bool
		wantOK    bool
	}{
		{
			name:   "agent alive is conclusive",
			probe:  fakeAgentProbe{hasSession: true, agentAlive: true},
			wantOK: true, wantAlive: true,
		},
		{
			name:   "agent absent with a working probe is confirmed dead",
			probe:  fakeAgentProbe{hasSession: true, agentAlive: false},
			wantOK: true, wantAlive: false,
		},
		{
			// The probe machinery itself failed — pgrep unable to fork under
			// load, or show-environment erroring so a codex/cursor session
			// would be matched against Claude's process names. AgentAliveE
			// surfaces these as an error instead of a bare false.
			name:   "unusable probe yields no verdict",
			probe:  fakeAgentProbe{hasSession: true, agentAliveErr: errors.New("agent liveness could not be determined: pgrep: fork: resource temporarily unavailable")},
			wantOK: false, wantAlive: false,
		},
		{
			name:   "tmux control path erroring yields no verdict",
			probe:  fakeAgentProbe{hasSessionErr: errors.New("server exited")},
			wantOK: false, wantAlive: false,
		},
		{
			// HasSession collapses several failures into (false, nil) — every
			// error on Windows/psmux, and ErrNoServer everywhere — so a missing
			// session is indistinguishable from a downed control path.
			name:   "missing session is not proof of death",
			probe:  fakeAgentProbe{hasSession: false},
			wantOK: false, wantAlive: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.probe
			alive, ok := sessionAgentAlive(&f, "gt-x")
			if alive != tt.wantAlive || ok != tt.wantOK {
				t.Errorf("got (alive=%v, ok=%v), want (alive=%v, ok=%v)", alive, ok, tt.wantAlive, tt.wantOK)
			}
			if f.aliveCalls > 1 {
				t.Errorf("IsAgentAlive called %d times; re-sampling repeats a deterministic probe and doubles pgrep fork pressure", f.aliveCalls)
			}
		})
	}
}

// TestIsSessionProcessDead_HeartbeatGate covers which heartbeat states may lead
// to a kill at all.
func TestIsSessionProcessDead_HeartbeatGate(t *testing.T) {
	t.Run("stale heartbeat plus dead agent is reaped", func(t *testing.T) {
		townRoot := t.TempDir()
		writeStaleHeartbeat(t, townRoot, "gt-s1")
		withProbe(t, &fakeAgentProbe{hasSession: true, agentAlive: false})
		if !isSessionProcessDead(&tmux.Tmux{}, "gt-s1", townRoot) {
			t.Error("a confirmed-dead agent behind a live pane must be reaped")
		}
	})

	t.Run("stale heartbeat plus live agent is not reaped", func(t *testing.T) {
		townRoot := t.TempDir()
		writeStaleHeartbeat(t, townRoot, "gt-s2")
		withProbe(t, &fakeAgentProbe{hasSession: true, agentAlive: true})
		if isSessionProcessDead(&tmux.Tmux{}, "gt-s2", townRoot) {
			t.Error("gt-azm0: an agent mid-turn lapses its heartbeat while healthy")
		}
	})

	t.Run("missing heartbeat is never reaped", func(t *testing.T) {
		// SessionManager.Start writes the first heartbeat only after the runtime
		// is ready (up to ClaudeStartTimeout). Until then IsAgentAlive sees only
		// the wrapper shell, so probing reports death and a concurrent gt sling
		// would kill a polecat that is still starting up.
		townRoot := t.TempDir()
		withProbe(t, &fakeAgentProbe{hasSession: true, agentAlive: false})
		if isSessionProcessDead(&tmux.Tmux{}, "gt-starting-up", townRoot) {
			t.Error("a session that has never checked in cannot have proved death")
		}
	})

	t.Run("empty town root yields no verdict", func(t *testing.T) {
		withProbe(t, &fakeAgentProbe{hasSession: true, agentAlive: false})
		if isSessionProcessDead(&tmux.Tmux{}, "gt-s3", "") {
			t.Error("without a town root the heartbeat cannot be consulted")
		}
	})
}

func TestIsSessionProcessDead_EmptyTownRoot(t *testing.T) {
	// With empty townRoot, heartbeat check is skipped entirely.
	// This tests backward compatibility when townRoot isn't available.
	// We can't test the full PID fallback without a real tmux session,
	// but we verify no panic with empty townRoot.
	sessionName := "gt-test-no-townroot"

	// Empty townRoot skips heartbeat, falls through to PID check.
	// Can't test PID path without tmux, but verify heartbeat path is skipped.
	stale, exists := IsSessionHeartbeatStale("", sessionName)
	if exists {
		t.Error("expected exists=false with empty townRoot")
	}
	if stale {
		t.Error("expected stale=false with empty townRoot")
	}
}

func TestReadSessionHeartbeat_V1BackwardsCompat(t *testing.T) {
	townRoot := t.TempDir()

	// Write a v1 heartbeat (timestamp only, no state field)
	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	ts := time.Now().UTC()
	data := []byte(`{"timestamp":"` + ts.Format(time.RFC3339Nano) + `"}`)
	if err := os.WriteFile(filepath.Join(dir, "gt-test-v1.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	hb := ReadSessionHeartbeat(townRoot, "gt-test-v1")
	if hb == nil {
		t.Fatal("expected non-nil heartbeat for v1 format")
	}

	// State should be empty (v1)
	if hb.State != "" {
		t.Errorf("v1 heartbeat state = %q, want empty", hb.State)
	}

	// IsV2 should return false
	if hb.IsV2() {
		t.Error("expected IsV2()=false for v1 heartbeat")
	}

	// EffectiveState should default to working
	if hb.EffectiveState() != HeartbeatWorking {
		t.Errorf("v1 EffectiveState() = %q, want %q", hb.EffectiveState(), HeartbeatWorking)
	}
}

func TestReadSessionHeartbeat_V2AllStates(t *testing.T) {
	townRoot := t.TempDir()

	dir := filepath.Join(townRoot, ".runtime", "heartbeats")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	states := []HeartbeatState{HeartbeatWorking, HeartbeatIdle, HeartbeatExiting, HeartbeatStuck}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			session := "gt-test-v2-" + string(state)
			hb := SessionHeartbeat{
				Timestamp: time.Now().UTC(),
				State:     state,
				Context:   "test context",
				Bead:      "gt-test-bead",
			}
			data, err := json.Marshal(hb)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, session+".json"), data, 0644); err != nil {
				t.Fatal(err)
			}

			read := ReadSessionHeartbeat(townRoot, session)
			if read == nil {
				t.Fatal("expected non-nil heartbeat")
			}
			if read.State != state {
				t.Errorf("state = %q, want %q", read.State, state)
			}
			if !read.IsV2() {
				t.Error("expected IsV2()=true")
			}
			if read.EffectiveState() != state {
				t.Errorf("EffectiveState() = %q, want %q", read.EffectiveState(), state)
			}
			if read.Context != "test context" {
				t.Errorf("context = %q, want %q", read.Context, "test context")
			}
			if read.Bead != "gt-test-bead" {
				t.Errorf("bead = %q, want %q", read.Bead, "gt-test-bead")
			}
		})
	}
}
