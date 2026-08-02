package reviewer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHeartbeatPath_LivesOutsideTheWorktree(t *testing.T) {
	got := HeartbeatPath("/town/myrig")
	want := filepath.Join("/town", "myrig", "reviewer", "heartbeat.json")
	if got != want {
		t.Errorf("HeartbeatPath = %q, want %q", got, want)
	}
	// The worktree is <rig>/reviewer/rig — the heartbeat must not be inside it,
	// or a detached checkout could sweep it and `git status` would show it.
	if worktree := filepath.Join("/town", "myrig", "reviewer", "rig"); len(got) > len(worktree) && got[:len(worktree)+1] == worktree+string(filepath.Separator) {
		t.Errorf("heartbeat %q is inside the reviewer worktree %q", got, worktree)
	}
}

func TestReadHeartbeat_AbsentOrMalformedIsNil(t *testing.T) {
	rig := t.TempDir()

	if hb := ReadHeartbeat(rig); hb != nil {
		t.Errorf("absent heartbeat: got %+v, want nil", hb)
	}

	path := HeartbeatPath(rig)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hb := ReadHeartbeat(rig); hb != nil {
		t.Errorf("malformed heartbeat: got %+v, want nil (monitoring path must not hard-error)", hb)
	}
}

func TestNilHeartbeat_AgeIsHugeAndElapsedIsZero(t *testing.T) {
	var hb *Heartbeat
	// Absent must read as "not progressing" so the reaper needs no special case.
	if age := hb.Age(); age < 24*time.Hour {
		t.Errorf("nil Age() = %v, want a very large duration", age)
	}
	// But Elapsed must be zero, so "no data" can never trip a runtime cap.
	if el := hb.Elapsed(); el != 0 {
		t.Errorf("nil Elapsed() = %v, want 0", el)
	}
}

func TestElapsed_ZeroWhenStartedAtUnset(t *testing.T) {
	hb := &Heartbeat{Timestamp: time.Now()}
	if el := hb.Elapsed(); el != 0 {
		t.Errorf("Elapsed with unset StartedAt = %v, want 0 — an absolute cap must not fire on missing data", el)
	}
}

func TestTouchHeartbeat_RoundTrip(t *testing.T) {
	rig := t.TempDir()

	if err := TouchHeartbeat(rig, PhaseDispatched, 175, 1, "abc123"); err != nil {
		t.Fatalf("TouchHeartbeat: %v", err)
	}
	hb := ReadHeartbeat(rig)
	if hb == nil {
		t.Fatal("ReadHeartbeat returned nil after a write")
	}
	if hb.Phase != PhaseDispatched || hb.PR != 175 || hb.Round != 1 || hb.SHA != "abc123" {
		t.Errorf("round-trip mismatch: %+v", hb)
	}
	if hb.StartedAt.IsZero() {
		t.Error("StartedAt must be seeded on the first touch")
	}
	if hb.Age() > time.Minute {
		t.Errorf("fresh heartbeat Age() = %v, want ~0", hb.Age())
	}
}

func TestTouchHeartbeat_PreservesStartedAtAcrossPhases(t *testing.T) {
	rig := t.TempDir()

	// Seed a dispatch that began well in the past.
	origin := time.Now().UTC().Add(-40 * time.Minute)
	if err := WriteHeartbeat(rig, &Heartbeat{
		Timestamp: origin, StartedAt: origin, Phase: PhaseDispatched, PR: 42, Round: 2, SHA: "deadbeef",
	}); err != nil {
		t.Fatal(err)
	}

	// An in-session phase touch carries no PR/round/SHA — the shape the
	// reviewer subcommands actually use.
	if err := TouchHeartbeat(rig, PhaseConsolidate, 0, 0, ""); err != nil {
		t.Fatal(err)
	}

	hb := ReadHeartbeat(rig)
	if hb == nil {
		t.Fatal("nil heartbeat after touch")
	}
	if !hb.StartedAt.Equal(origin) {
		t.Errorf("StartedAt = %v, want preserved %v — total review time must survive phase changes", hb.StartedAt, origin)
	}
	if el := hb.Elapsed(); el < 39*time.Minute {
		t.Errorf("Elapsed() = %v, want ~40m from the preserved StartedAt", el)
	}
	if hb.Age() > time.Minute {
		t.Errorf("Age() = %v, want ~0 — the phase clock resets even though StartedAt doesn't", hb.Age())
	}
	// Identity fields must be inherited, not blanked.
	if hb.PR != 42 || hb.Round != 2 || hb.SHA != "deadbeef" {
		t.Errorf("identity not inherited on a bare phase touch: %+v", hb)
	}
	if hb.Phase != PhaseConsolidate {
		t.Errorf("Phase = %q, want %q", hb.Phase, PhaseConsolidate)
	}
}

func TestTouchHeartbeat_NewPRResetsStartedAt(t *testing.T) {
	rig := t.TempDir()

	stale := time.Now().UTC().Add(-90 * time.Minute)
	if err := WriteHeartbeat(rig, &Heartbeat{
		Timestamp: stale, StartedAt: stale, Phase: PhasePost, PR: 100, Round: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// A session draining a second request is a NEW review. Inheriting the old
	// clock would make an absolute-runtime cap kill it almost immediately.
	if err := TouchHeartbeat(rig, PhaseDispatched, 200, 1, "newsha"); err != nil {
		t.Fatal(err)
	}
	hb := ReadHeartbeat(rig)
	if hb == nil {
		t.Fatal("nil heartbeat")
	}
	if hb.PR != 200 {
		t.Errorf("PR = %d, want 200", hb.PR)
	}
	if el := hb.Elapsed(); el > time.Minute {
		t.Errorf("Elapsed() = %v after switching PRs, want ~0 — a new review starts a new clock", el)
	}
	if hb.SHA != "newsha" {
		t.Errorf("SHA = %q, want the new review's SHA", hb.SHA)
	}
}

func TestClearHeartbeat_IdempotentAndRemoves(t *testing.T) {
	rig := t.TempDir()

	if err := TouchHeartbeat(rig, PhasePost, 7, 1, "sha"); err != nil {
		t.Fatal(err)
	}
	if err := ClearHeartbeat(rig); err != nil {
		t.Fatalf("ClearHeartbeat: %v", err)
	}
	if hb := ReadHeartbeat(rig); hb != nil {
		t.Errorf("heartbeat survived clear: %+v", hb)
	}
	// A second clear (double `done`, or a clear racing the reaper) is success.
	if err := ClearHeartbeat(rig); err != nil {
		t.Errorf("second ClearHeartbeat: %v, want nil (idempotent)", err)
	}
}

func TestWriteHeartbeat_LeavesNoTempFiles(t *testing.T) {
	rig := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := TouchHeartbeat(rig, PhasePrompt, 1, 1, "s"); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(HeartbeatPath(rig)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != heartbeatFile {
			t.Errorf("stray file after atomic write: %q", e.Name())
		}
	}
}
