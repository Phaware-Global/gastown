package reviewer

import (
	"errors"
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

func TestTouchDispatch_SeedsAndRoundTrips(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 175, 1, "abc123"); err != nil {
		t.Fatalf("TouchDispatch: %v", err)
	}
	hb := ReadHeartbeat(rig)
	if hb == nil {
		t.Fatal("nil heartbeat after dispatch")
	}
	if hb.Phase != PhaseDispatched || hb.PR != 175 || hb.Round != 1 || hb.SHA != "abc123" {
		t.Errorf("round-trip mismatch: %+v", hb)
	}
	if hb.StartedAt.IsZero() {
		t.Error("dispatch must seed StartedAt")
	}
}

func TestTouchDispatch_NewRoundOfSamePRResetsTheClock(t *testing.T) {
	rig := t.TempDir()
	// Round 1 wedged 40 minutes ago. This is the DOMINANT re-dispatch shape:
	// await-review gives up at pr_review_timeout (30m) and the refinery
	// re-dispatches the same PR at round 2 before the reaper's stuck_threshold
	// (45m) has cleared round 1's record.
	old := time.Now().UTC().Add(-40 * time.Minute)
	if err := WriteHeartbeat(rig, &Heartbeat{
		Timestamp: old, StartedAt: old, Phase: PhasePrompt, PR: 176, Round: 1, SHA: "aaaa",
	}); err != nil {
		t.Fatal(err)
	}

	if err := TouchDispatch(rig, 176, 2, "bbbb"); err != nil {
		t.Fatalf("round-2 dispatch: %v", err)
	}
	hb := ReadHeartbeat(rig)
	if hb == nil {
		t.Fatal("nil heartbeat")
	}
	if hb.Round != 2 || hb.SHA != "bbbb" {
		t.Errorf("identity not updated: %+v", hb)
	}
	if el := hb.Elapsed(); el > time.Minute {
		t.Errorf("Elapsed = %v, want ~0 — a fresh round must not be born 40 minutes into "+
			"its budget, or the reaper kills it on its first cycle", el)
	}
}

func TestTouchDispatch_NewSHAOfSameRoundResetsTheClock(t *testing.T) {
	rig := t.TempDir()
	old := time.Now().UTC().Add(-50 * time.Minute)
	if err := WriteHeartbeat(rig, &Heartbeat{
		Timestamp: old, StartedAt: old, Phase: PhasePrompt, PR: 1, Round: 1, SHA: "aaaa",
	}); err != nil {
		t.Fatal(err)
	}
	if err := TouchDispatch(rig, 1, 1, "cccc"); err != nil {
		t.Fatal(err)
	}
	if el := ReadHeartbeat(rig).Elapsed(); el > time.Minute {
		t.Errorf("Elapsed = %v, want ~0 — a force-push is a new review target", el)
	}
}

func TestTouchDispatch_IdenticalRedispatchKeepsTheClock(t *testing.T) {
	rig := t.TempDir()
	old := time.Now().UTC().Add(-10 * time.Minute)
	if err := WriteHeartbeat(rig, &Heartbeat{
		Timestamp: old, StartedAt: old, Phase: PhaseCheckout, PR: 1, Round: 1, SHA: "aaaa",
	}); err != nil {
		t.Fatal(err)
	}
	// An idempotent retry of the SAME review must not hand the reviewer a fresh
	// budget, or a retry loop would make the cap unreachable.
	if err := TouchDispatch(rig, 1, 1, "aaaa"); err != nil {
		t.Fatal(err)
	}
	if el := ReadHeartbeat(rig).Elapsed(); el < 9*time.Minute {
		t.Errorf("Elapsed = %v, want the original clock preserved on an identical re-dispatch", el)
	}
}

func TestTouchDispatch_DoesNotClobberAnUnfinishedDifferentReview(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 100, 1, "sha100"); err != nil {
		t.Fatal(err)
	}
	if err := TouchHeartbeat(rig, PhasePrompt, 100, 1, "sha100"); err != nil {
		t.Fatal(err)
	}

	// A second request arrives while review 100 is in flight. One file cannot
	// represent the queue, and the IN-FLIGHT review's telemetry is what
	// supervisors need — overwriting it would reset a possibly-wedged reviewer's
	// clock from a third party.
	err := TouchDispatch(rig, 200, 1, "sha200")
	if !errors.Is(err, ErrReviewInFlight) {
		t.Fatalf("TouchDispatch during an in-flight review = %v, want ErrReviewInFlight", err)
	}
	hb := ReadHeartbeat(rig)
	if hb.PR != 100 || hb.Phase != PhasePrompt {
		t.Errorf("in-flight record was clobbered: %+v", hb)
	}
}

func TestTouchHeartbeat_CannotChangeIdentityOrReseedTheClock(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 100, 1, "sha100"); err != nil {
		t.Fatal(err)
	}
	origin := ReadHeartbeat(rig).StartedAt

	// The exploit this closes: the PR reaching in-session touches comes from the
	// reviewer's own flags, and the reviewer is what the absolute cap exists to
	// constrain. A mismatched --pr must not reseed the clock or rewrite identity.
	if err := TouchHeartbeat(rig, PhasePrompt, 999, 7, "evil"); err != nil {
		t.Fatal(err)
	}
	hb := ReadHeartbeat(rig)
	if hb.PR != 100 || hb.Round != 1 || hb.SHA != "sha100" {
		t.Errorf("in-session touch changed the review identity: %+v", hb)
	}
	if !hb.StartedAt.Equal(origin) {
		t.Errorf("in-session touch reseeded the clock: %v != %v", hb.StartedAt, origin)
	}
	if hb.Phase != PhasePrompt {
		t.Errorf("Phase = %q, want the phase to still advance", hb.Phase)
	}
}

func TestTouchHeartbeat_WithoutADispatchLeavesElapsedUnknown(t *testing.T) {
	rig := t.TempDir()
	// No dispatch on record. A phase touch must not start a clock, or an
	// in-session touch could establish one.
	if err := TouchHeartbeat(rig, PhaseConsolidate, 5, 1, "s"); err != nil {
		t.Fatal(err)
	}
	hb := ReadHeartbeat(rig)
	if hb == nil {
		t.Fatal("nil heartbeat")
	}
	if !hb.StartedAt.IsZero() {
		t.Error("an in-session touch with no dispatch must leave StartedAt zero (unknown)")
	}
	if hb.Elapsed() != 0 {
		t.Errorf("Elapsed = %v, want 0 (unknown never trips the cap)", hb.Elapsed())
	}
}

func TestClearHeartbeatFor_LeavesAQueuedReviewsRecord(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 200, 1, "sha200"); err != nil {
		t.Fatal(err)
	}
	// Finishing PR 100 must not erase PR 200's dispatch record — that would make
	// the queued review invisible, the exact blind spot the seed exists to close.
	cleared, err := ClearHeartbeatFor(rig, 100)
	if err != nil {
		t.Fatal(err)
	}
	if cleared {
		t.Error("clearing for PR 100 must not remove PR 200's record")
	}
	if ReadHeartbeat(rig) == nil {
		t.Fatal("queued review's heartbeat was destroyed")
	}

	cleared, err = ClearHeartbeatFor(rig, 200)
	if err != nil || !cleared {
		t.Fatalf("clearing for the matching PR must succeed: cleared=%v err=%v", cleared, err)
	}
	if ReadHeartbeat(rig) != nil {
		t.Error("matching clear did not remove the heartbeat")
	}
}

func TestClearHeartbeatFor_UnknownPRClearsUnconditionally(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 7, 1, "s"); err != nil {
		t.Fatal(err)
	}
	// pr <= 0 means the caller could not determine which review it finished.
	cleared, err := ClearHeartbeatFor(rig, 0)
	if err != nil || !cleared {
		t.Fatalf("cleared=%v err=%v, want an unconditional clear", cleared, err)
	}
	if ReadHeartbeat(rig) != nil {
		t.Error("heartbeat survived an unconditional clear")
	}
}

func TestClearHeartbeat_IdempotentAndRemoves(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 7, 1, "sha"); err != nil {
		t.Fatal(err)
	}
	if err := ClearHeartbeat(rig); err != nil {
		t.Fatalf("ClearHeartbeat: %v", err)
	}
	if ReadHeartbeat(rig) != nil {
		t.Error("heartbeat survived clear")
	}
	if err := ClearHeartbeat(rig); err != nil {
		t.Errorf("second ClearHeartbeat: %v, want nil (idempotent)", err)
	}
}

func TestWriteHeartbeat_UsesAFixedTempNameAndIsNotWorldReadable(t *testing.T) {
	rig := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := TouchDispatch(rig, 1, 1, "s"); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Dir(HeartbeatPath(rig))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A crashed write leaves at most ONE temp file, which the next write
	// overwrites — random names would accumulate, since nothing sweeps them.
	for _, e := range entries {
		if e.Name() != heartbeatFile && e.Name() != heartbeatFile+".tmp" {
			t.Errorf("unexpected residue after atomic write: %q", e.Name())
		}
	}
	info, err := os.Stat(HeartbeatPath(rig))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("heartbeat mode = %04o, want 0600 to match the rig's other metadata", perm)
	}
}

func TestReadHeartbeatE_DistinguishesAbsentFromUnreadable(t *testing.T) {
	rig := t.TempDir()

	// Absent is not an error — the rig is simply idle.
	hb, err := ReadHeartbeatE(rig)
	if hb != nil || err != nil {
		t.Errorf("absent: got (%v, %v), want (nil, nil)", hb, err)
	}

	// Malformed IS an error. A supervisor must not read a torn or corrupt file
	// as "no reviewer is working here" and take the harshest available action.
	path := HeartbeatPath(rig)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	hb, err = ReadHeartbeatE(rig)
	if err == nil {
		t.Error("malformed heartbeat must surface an error, not read as absent")
	}
	if hb != nil {
		t.Errorf("malformed: got %v, want nil heartbeat", hb)
	}

	// The lenient reader keeps its best-effort contract for progress-only callers.
	if ReadHeartbeat(rig) != nil {
		t.Error("ReadHeartbeat must stay lenient (nil on malformed)")
	}
}
