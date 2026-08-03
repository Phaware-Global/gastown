package cmd

import (
	"os"
	"strings"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reviewer"
)

func TestCollectReviewerState_SurfacesHeartbeatFieldsSanitized(t *testing.T) {
	rigPath := t.TempDir()
	if err := reviewer.WriteHeartbeat(rigPath, &reviewer.Heartbeat{
		Timestamp: time.Now().Add(-5 * time.Minute),
		StartedAt: time.Now().Add(-30 * time.Minute),
		// A forged phase carrying a terminal escape: unescaped, this would let a
		// rig-writable file rewrite the operator's screen.
		Phase: "prompt\x1b[2J\rFAKE",
		PR:    175,
		Round: 2,
		SHA:   "not-a-sha",
	}); err != nil {
		t.Fatal(err)
	}

	st := collectReviewerState("testrig", rigPath)
	if st.Phase != "unknown" {
		t.Errorf("Phase = %q, want \"unknown\" — an unrecognized phase must not reach the terminal", st.Phase)
	}
	if st.SHA != "unknown" {
		t.Errorf("SHA = %q, want \"unknown\" for a non-hex value", st.SHA)
	}
	if st.PR != 175 || st.Round != 2 {
		t.Errorf("identity fields not surfaced: %+v", st)
	}
	if st.Diagnosis == "" {
		t.Error("Diagnosis must always be populated")
	}
}

func TestCollectReviewerState_UsesTheSharedClassifier(t *testing.T) {
	// The CLI must never report a state the reaper would not act on. Both go
	// through reviewer.Classify, so a heartbeat with no session inside the spawn
	// window reads as "spawning" here exactly as it does in the daemon.
	rigPath := t.TempDir()
	if err := reviewer.WriteHeartbeat(rigPath, &reviewer.Heartbeat{
		Timestamp: time.Now(), StartedAt: time.Now(),
		Phase: reviewer.PhaseDispatched, PR: 9,
	}); err != nil {
		t.Fatal(err)
	}
	st := collectReviewerState("testrig", rigPath)
	want := reviewer.StateSpawning.Describe()
	if !strings.HasPrefix(st.Diagnosis, want) {
		t.Errorf("Diagnosis = %q, want %q", st.Diagnosis, want)
	}
}

func TestCollectReviewerState_NoHeartbeatLeavesProgressEmpty(t *testing.T) {
	st := collectReviewerState("testrig", t.TempDir())
	if st.Phase != "" || st.PR != 0 || st.Elapsed != "" {
		t.Errorf("absent heartbeat must leave progress fields empty, got %+v", st)
	}
	if st.Diagnosis == "" {
		t.Error("Diagnosis must always be populated")
	}
}

func TestCollectReviewerState_UnreadableIsNotIdle(t *testing.T) {
	// A corrupt heartbeat previously read as "idle", which is the state that
	// invites the harshest cleanup — and stop could not clear it.
	rigPath := t.TempDir()
	path := reviewer.HeartbeatPath(rigPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := collectReviewerState("testrig", rigPath)
	if st.Diagnosis == reviewer.StateIdle.Describe() {
		t.Error("a corrupt heartbeat must not report as idle")
	}
}

func TestCollectReviewerState_ReportsUnknownRatherThanHealthy(t *testing.T) {
	// An unresolvable session target must surface, not silently look fine.
	st := collectReviewerState("rig-with-no-registry-entry", t.TempDir())
	if st.Err == "" {
		t.Error("an uninspectable rig must not report as working")
	}
}

func TestDashIfEmpty(t *testing.T) {
	if got := dashIfEmpty(""); got != "-" {
		t.Errorf("dashIfEmpty(\"\") = %q, want \"-\"", got)
	}
	if got := dashIfEmpty("x"); got != "x" {
		t.Errorf("dashIfEmpty(\"x\") = %q, want \"x\"", got)
	}
}
