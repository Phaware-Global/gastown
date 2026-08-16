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
	if err := TouchHeartbeat(rig, PhasePrompt); err != nil {
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
	// constrain. TouchHeartbeat now takes no identity arguments at all — the
	// hole is closed by the signature — so what remains to verify is that the
	// existing identity and clock survive a phase advance intact.
	if err := TouchHeartbeat(rig, PhasePrompt); err != nil {
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
	if err := TouchHeartbeat(rig, PhaseConsolidate); err != nil {
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
	// Identity is withheld for the same reason the clock is. A marker that
	// carried a PR would be treated as a review in flight by TouchDispatch and
	// would then block every future seed for the rig — reachable from a single
	// `gt reviewer prompt --pr 999999` run before any dispatch.
	if hb.PR != 0 || hb.Round != 0 || hb.SHA != "" {
		t.Errorf("a phase-only marker must carry no review identity: %+v", hb)
	}
}

func TestTouchDispatch_OverwritesAPhaseOnlyMarker(t *testing.T) {
	rig := t.TempDir()
	// A marker written with no dispatch on record carries no clock. Deferring to
	// it as "in flight" would wedge the dispatcher permanently: nothing clears a
	// record whose PR matches no real review.
	if err := TouchHeartbeat(rig, PhasePrompt); err != nil {
		t.Fatal(err)
	}
	if err := TouchDispatch(rig, 176, 2, "sha176"); err != nil {
		t.Fatalf("TouchDispatch over a phase-only marker = %v, want nil", err)
	}
	hb := ReadHeartbeat(rig)
	if hb.PR != 176 || hb.Round != 2 {
		t.Errorf("the real dispatch did not land: %+v", hb)
	}
	if hb.StartedAt.IsZero() {
		t.Error("the real dispatch must start a clock")
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

func TestWriteHeartbeat_DoesNotInheritAPlantedTempFilesMode(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 1, 1, "s"); err != nil {
		t.Fatal(err)
	}
	tmpName := HeartbeatPath(rig) + ".tmp"
	// Any process in the rig can create this dotfile. os.WriteFile is
	// O_CREATE|O_TRUNC, whose mode argument is IGNORED on an existing file — so a
	// pre-created 0666 temp permanently downgraded the 0600 rule, with no code
	// path that ever restored it.
	if err := os.WriteFile(tmpName, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmpName, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := TouchDispatch(rig, 1, 2, "s"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(HeartbeatPath(rig))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("heartbeat mode = %04o after a planted 0666 temp, want 0600", perm)
	}
}

func TestWriteHeartbeat_RefusesToWriteThroughAPlantedSymlink(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 1, 1, "s"); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	const sentinel = "do not overwrite me"
	if err := os.WriteFile(victim, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	// A FIXED temp name is predictable, which is the price of the
	// anti-accumulation property. Opened with O_CREATE|O_TRUNC it followed a
	// symlink planted here, turning every subsequent heartbeat write into an
	// arbitrary-path overwrite — and the rename then installed the symlink as the
	// heartbeat, so later reads came from the attacker's file too.
	if err := os.Symlink(victim, HeartbeatPath(rig)+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := TouchDispatch(rig, 1, 2, "s"); err != nil {
		t.Fatalf("TouchDispatch = %v, want nil — a planted symlink must be swept, not fatal", err)
	}
	got, err := os.ReadFile(victim) //nolint:gosec // test-local path
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinel {
		t.Errorf("wrote through a planted symlink: victim now %q", got)
	}
	if hb := ReadHeartbeat(rig); hb == nil || hb.Round != 2 {
		t.Errorf("the legitimate write did not land: %+v", hb)
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

func TestTouchCheckout_NewPRStartsAFreshClock(t *testing.T) {
	rig := t.TempDir()
	// Round 1 wedged three hours ago and its record was deliberately preserved
	// (the wedged-session path skips the dispatcher seed so the reaper can act).
	stale := time.Now().UTC().Add(-3 * time.Hour)
	if err := WriteHeartbeat(rig, &Heartbeat{
		Timestamp: stale, StartedAt: stale, Phase: PhasePrompt, PR: 100, SHA: "aaaa",
	}); err != nil {
		t.Fatal(err)
	}

	// The reviewer drains the queued request and checks out a DIFFERENT PR.
	// Inheriting the frozen clock would put the new review instantly past the
	// absolute-runtime cap and get it killed seconds after it started.
	if err := TouchCheckout(rig, 200, 3, "bbbb"); err != nil {
		t.Fatal(err)
	}
	hb := ReadHeartbeat(rig)
	if hb == nil {
		t.Fatal("nil heartbeat")
	}
	if hb.PR != 200 || hb.SHA != "bbbb" {
		t.Errorf("identity not established: %+v", hb)
	}
	// Assert the clock was STARTED, not merely that it is not stale. Elapsed()
	// returns 0 for a zero StartedAt, so a freshness bound alone passes just as
	// happily when no clock exists — and "started_at omitted entirely" is one of
	// the evasions this module was written to prevent, so the mutant that drops
	// the field must fail here.
	if hb.StartedAt.IsZero() {
		t.Fatal("a new review must START a clock, not leave it unknown")
	}
	if el := hb.Elapsed(); el > time.Minute {
		t.Errorf("Elapsed = %v, want ~0 — a new review must not inherit the previous one's clock", el)
	}
	// Round comes from the request. Dropping it loses the first-review/fix-round
	// distinction in exactly the queued-behind-a-wedge case an operator is most
	// likely to be investigating — and falsifies TouchDispatch's identical-
	// re-request check, which compares Round exactly.
	if hb.Round != 3 {
		t.Errorf("Round = %d, want 3 — checkout is the only writer of a queued review's identity", hb.Round)
	}
}

func TestTouchCheckout_InheritsRoundRatherThanWritingZero(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 100, 4, "aaaa"); err != nil {
		t.Fatal(err)
	}
	// No --round supplied. Writing a zero would make a later identical
	// re-dispatch compare unequal and hand the in-flight review a fresh budget,
	// which is the retry-loop-defeats-the-cap hole sameReview exists to close.
	if err := TouchCheckout(rig, 200, 0, "bbbb"); err != nil {
		t.Fatal(err)
	}
	if r := ReadHeartbeat(rig).Round; r != 4 {
		t.Errorf("Round = %d, want the inherited 4 rather than a silent zero", r)
	}
}

func TestTouchDispatch_IdenticalRerequestKeepsTheClock(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 100, 1, "aaaa"); err != nil {
		t.Fatal(err)
	}
	origin := ReadHeartbeat(rig).StartedAt
	if origin.IsZero() {
		t.Fatal("precondition: a dispatch must start a clock")
	}
	if err := TouchDispatch(rig, 100, 1, "aaaa"); err != nil {
		t.Fatal(err)
	}
	if got := ReadHeartbeat(rig).StartedAt; !got.Equal(origin) {
		t.Errorf("StartedAt = %v, want preserved %v — an identical retry must not hand out a fresh budget", got, origin)
	}
}

func TestTouchDispatch_DoesNotInheritAZeroClock(t *testing.T) {
	rig := t.TempDir()
	// A phase-only marker has a zero StartedAt. An identical re-dispatch that
	// inherited it would report "unknown" runtime for the whole review, so the
	// absolute cap would never see a number at all.
	if err := WriteHeartbeat(rig, &Heartbeat{
		Timestamp: time.Now().UTC(), Phase: PhasePrompt, PR: 100, Round: 1, SHA: "aaaa",
	}); err != nil {
		t.Fatal(err)
	}
	if err := TouchDispatch(rig, 100, 1, "aaaa"); err != nil {
		t.Fatal(err)
	}
	if ReadHeartbeat(rig).StartedAt.IsZero() {
		t.Error("a dispatch must seed a real clock rather than inherit a marker's zero")
	}
}

func TestTouchCheckout_SamePRAdvancesWithoutResetting(t *testing.T) {
	rig := t.TempDir()
	if err := TouchDispatch(rig, 100, 1, "aaaa"); err != nil {
		t.Fatal(err)
	}
	origin := ReadHeartbeat(rig).StartedAt

	// Re-checking out the SAME review is an ordinary phase advance; resetting
	// here would let a reviewer refresh its own runtime clock by re-running
	// checkout in a loop.
	if err := TouchCheckout(rig, 100, 1, "aaaa"); err != nil {
		t.Fatal(err)
	}
	hb := ReadHeartbeat(rig)
	if !hb.StartedAt.Equal(origin) {
		t.Errorf("StartedAt = %v, want preserved %v for the same review", hb.StartedAt, origin)
	}
	if hb.Phase != PhaseCheckout {
		t.Errorf("Phase = %q, want %q", hb.Phase, PhaseCheckout)
	}
}
