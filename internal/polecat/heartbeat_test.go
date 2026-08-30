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

// TestIsSessionProcessDead_StaleHeartbeatIsNotDeath is the gt-azm0 regression.
//
// A polecat mid-turn — reasoning or editing rather than shelling out to gt —
// lets its heartbeat lapse past SessionHeartbeatStaleThreshold while its process
// is very much alive. Before the fix the stale heartbeat short-circuited and
// reported the session dead, so `gt polecat list` showed the polecat as
// stalled/NEEDS_RECOVERY and the witness reaped it mid-task and reassigned its
// bead.
//
// A nil tmux handle stands in for "nothing to probe": the contract is that an
// unprobeable session is never reported dead, so staleness alone must not
// produce a kill.
func TestIsSessionProcessDead_StaleHeartbeatIsNotDeath(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-test-hb-stale-but-live"
	writeStaleHeartbeat(t, townRoot, sessionName)

	if stale, exists := IsSessionHeartbeatStale(townRoot, sessionName); !exists || !stale {
		t.Fatalf("test setup: want an existing stale heartbeat, got exists=%v stale=%v", exists, stale)
	}

	if isSessionProcessDead(nil, sessionName, townRoot) {
		t.Error("a stale heartbeat alone must not report dead: it falls through to process probing")
	}
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

// withProbe swaps both probe steps for the duration of a test so either
// direction of isSessionProcessDead's contract can be driven without a tmux
// server or a real process.
func withProbe(t *testing.T, pid string, pidErr error, alive bool) {
	t.Helper()
	prevPID, prevAlive := panePID, pidAlive
	panePID = func(*tmux.Tmux, string) (string, error) { return pid, pidErr }
	pidAlive = func(int) bool { return alive }
	t.Cleanup(func() { panePID, pidAlive = prevPID, prevAlive })
}

// TestIsSessionProcessDead_ReapsAConfirmedDeadPane pins the other half of the
// contract: an existing-and-stale heartbeat over a pane whose process is gone
// must STILL report dead.
//
// This is the direction that authorizes KillSessionWithProcesses, so leaving it
// unpinned would let the function be hardwired to `return false` and ship green
// — especially in a change that deliberately widens the under-report window.
func TestIsSessionProcessDead_ReapsAConfirmedDeadPane(t *testing.T) {
	tests := []struct {
		name     string
		pid      string
		pidErr   error
		alive    bool
		wantDead bool
	}{
		{
			name: "stale heartbeat over a dead pane process reaps",
			pid:  "4242", alive: false, wantDead: true,
		},
		{
			name: "no pane pid at all means no process",
			pid:  "", alive: false, wantDead: true,
		},
		{
			// gt-azm0: the agent is mid-turn, so its heartbeat lapsed while its
			// process is very much alive.
			name: "stale heartbeat over a live pane process does not reap",
			pid:  "4242", alive: true, wantDead: false,
		},
		{
			// gt-kncti: a tmux query that failed is not evidence of death.
			name: "unreadable pane yields no verdict",
			pid:  "", pidErr: errors.New("server busy"), alive: false, wantDead: false,
		},
		{
			name: "non-numeric pid does not authorize a kill",
			pid:  "not-a-pid", alive: false, wantDead: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := t.TempDir()
			sessionName := "gt-test-reap"
			writeStaleHeartbeat(t, townRoot, sessionName)
			withProbe(t, tt.pid, tt.pidErr, tt.alive)

			if got := isSessionProcessDead(&tmux.Tmux{}, sessionName, townRoot); got != tt.wantDead {
				t.Errorf("isSessionProcessDead() = %v, want %v", got, tt.wantDead)
			}
		})
	}
}

// TestIsSessionProcessDead_FreshHeartbeatSkipsTheProbe pins the fast path: a
// fresh heartbeat proves life on its own and must not spend a probe.
func TestIsSessionProcessDead_FreshHeartbeatSkipsTheProbe(t *testing.T) {
	townRoot := t.TempDir()
	sessionName := "gt-test-fresh"
	TouchSessionHeartbeat(townRoot, sessionName)

	probed := false
	prev := panePID
	panePID = func(*tmux.Tmux, string) (string, error) { probed = true; return "", nil }
	t.Cleanup(func() { panePID = prev })

	if isSessionProcessDead(&tmux.Tmux{}, sessionName, townRoot) {
		t.Error("a fresh heartbeat must report alive")
	}
	if probed {
		t.Error("a fresh heartbeat must short-circuit before probing the pane")
	}
}
