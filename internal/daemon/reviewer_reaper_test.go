package daemon

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/reviewer"
	"github.com/steveyegge/gastown/internal/tmux"
)

// hb builds a heartbeat whose current phase is phaseAge old and whose review
// started elapsed ago.
func hb(phaseAge, elapsed time.Duration) *reviewer.Heartbeat {
	now := time.Now()
	return &reviewer.Heartbeat{
		Timestamp: now.Add(-phaseAge),
		StartedAt: now.Add(-elapsed),
		Phase:     reviewer.PhasePrompt,
		PR:        175,
	}
}

func TestDecideReviewerAction_HealthyReviewIsLeftAlone(t *testing.T) {
	stuck := 45 * time.Minute
	// A review 10 minutes into its subagent pass — the common case.
	action, _ := decideReviewerAction(hb(10*time.Minute, 12*time.Minute), stuck, 0, false)
	if action != reviewerActionNone {
		t.Errorf("action = %v, want none — a healthy in-flight review must not be touched", action)
	}
}

func TestDecideReviewerAction_LongSubagentPassBelowThresholdSurvives(t *testing.T) {
	stuck := 45 * time.Minute
	// 44 minutes parked at PhasePrompt is legitimate: the perspective subagents
	// run with no command in between to refresh the timestamp. Killing here
	// would reap working reviewers, which is worse than the gap being closed.
	action, _ := decideReviewerAction(hb(44*time.Minute, 44*time.Minute), stuck, 0, false)
	if action != reviewerActionNone {
		t.Errorf("action = %v, want none just below the threshold", action)
	}
}

func TestDecideReviewerAction_NudgesAtThreshold(t *testing.T) {
	stuck := 45 * time.Minute
	action, reason := decideReviewerAction(hb(46*time.Minute, 46*time.Minute), stuck, 0, false)
	if action != reviewerActionNudge {
		t.Fatalf("action = %v, want nudge — the cheap recovery comes before the kill", action)
	}
	if reason == "" {
		t.Error("nudge reason must be populated for the operator log")
	}
}

func TestDecideReviewerAction_KillsAtTwiceTheThreshold(t *testing.T) {
	stuck := 45 * time.Minute
	action, reason := decideReviewerAction(hb(91*time.Minute, 91*time.Minute), stuck, 0, false)
	if action != reviewerActionKill {
		t.Fatalf("action = %v, want kill past %dx the threshold", action, reviewer.StuckMultiple)
	}
	if reason == "" {
		t.Error("kill reason must be populated — a silent kill reproduces the diagnosis gap")
	}
}

func TestDecideReviewerAction_AbsoluteCapCatchesRefreshingLoop(t *testing.T) {
	stuck := 45 * time.Minute
	// The rail that matters most: a reviewer looping through phases refreshes
	// its timestamp forever, so phase age never trips. Only total elapsed can
	// stop it — and the courtesy nudge must not make it immortal.
	freshPhase := 1 * time.Minute
	pastCap := stuck*reviewer.AbsoluteCapMultiple + time.Minute
	// The session clock is what makes this rail work. It is the only input the
	// looping reviewer does not own, and since a live session always has an age,
	// supplying one is the honest shape of this scenario — a zero here would mean
	// tmux could not be queried, which switches the cap off by design.
	sessionAge := pastCap

	action, _ := decideReviewerAction(hb(freshPhase, pastCap), stuck, sessionAge, false)
	if action != reviewerActionNudge {
		t.Errorf("action = %v, want nudge on the first cap breach", action)
	}
	// Nudge spent: the loop cannot buy another one by refreshing its phase.
	if action, _ := decideReviewerAction(hb(freshPhase, pastCap), stuck, sessionAge, true); action != reviewerActionKill {
		t.Errorf("action = %v, want kill: a fresh phase must not exempt a review past the absolute cap "+
			"once its one nudge is spent", action)
	}
	// And with no session clock the cap is OFF, not merely inaccurate: a
	// self-report is never sufficient on its own to authorize a SIGKILL.
	if action, _ := decideReviewerAction(hb(freshPhase, pastCap), stuck, 0, true); action != reviewerActionNone {
		t.Errorf("action = %v, want none — a forged started_at with no corroborating "+
			"session clock must not kill", action)
	}
}

func TestDecideReviewerAction_UnknownStartedAtNeverTripsTheCap(t *testing.T) {
	stuck := 45 * time.Minute
	// Elapsed() is 0 when StartedAt is unset. Zero must read as "no data", not
	// as "infinitely old" — otherwise every heartbeat lacking StartedAt is
	// killed on sight.
	h := &reviewer.Heartbeat{Timestamp: time.Now(), Phase: reviewer.PhaseCheckout}
	if action, reason := decideReviewerAction(h, stuck, 0, false); action != reviewerActionNone {
		t.Errorf("action = %v (%q), want none when StartedAt is unknown", action, reason)
	}
}

func TestDecideReviewerAction_NilHeartbeatIsNoAction(t *testing.T) {
	// The nil case is handled by the caller (orphan-session path); the decision
	// rule itself must not claim a kill on no data.
	if action, _ := decideReviewerAction(nil, 45*time.Minute, 0, false); action != reviewerActionNone {
		t.Errorf("action = %v, want none for a nil heartbeat", action)
	}
}

func TestDecideReviewerAction_NonPositiveThresholdDisablesTheReaper(t *testing.T) {
	// A misconfigured (zero/negative) threshold must fail safe to "do nothing"
	// rather than to "kill everything immediately".
	for _, stuck := range []time.Duration{0, -time.Minute} {
		if action, _ := decideReviewerAction(hb(10*time.Hour, 10*time.Hour), stuck, 0, false); action != reviewerActionNone {
			t.Errorf("stuck=%v: action = %v, want none (fail safe)", stuck, action)
		}
	}
}

func TestDecideReviewerAction_ThresholdScalesWithConfig(t *testing.T) {
	// A rig that raises its stuck_threshold must actually get more headroom —
	// proves the reaper honors reviewer.toml rather than a hardcoded constant.
	age := 50 * time.Minute
	if action, _ := decideReviewerAction(hb(age, age), 45*time.Minute, 0, false); action != reviewerActionNudge {
		t.Errorf("45m threshold: action = %v, want nudge at %v", action, age)
	}
	if action, _ := decideReviewerAction(hb(age, age), 2*time.Hour, 0, false); action != reviewerActionNone {
		t.Errorf("2h threshold: action = %v, want none at %v", action, age)
	}
}

func TestReviewerReaperThresholdOrdering(t *testing.T) {
	// The reaper must never fire before the refinery's await-review timeout
	// (30m): the escalation carries diagnostic value that a silent kill
	// destroys, so the escalation has to come first.
	const prReviewTimeout = 30 * time.Minute
	if reviewer.DefaultStuckThreshold <= prReviewTimeout {
		t.Errorf("stuck threshold %v must exceed pr_review_timeout %v so the refinery escalates first",
			reviewer.DefaultStuckThreshold, prReviewTimeout)
	}
	if reviewer.StuckMultiple < 2 {
		t.Error("kill multiple must leave room for at least one nudge before killing")
	}
	if reviewer.AbsoluteCapMultiple <= reviewer.StuckMultiple {
		t.Errorf("absolute cap multiple (%d) must exceed the kill multiple (%d), or the cap is unreachable",
			reviewer.AbsoluteCapMultiple, reviewer.StuckMultiple)
	}
}

func TestShouldNudgeReviewer_RateLimitsPerRig(t *testing.T) {
	d := &Daemon{}

	if !d.shouldNudgeReviewer("rig-a") {
		t.Fatal("first nudge must be allowed")
	}
	if d.shouldNudgeReviewer("rig-a") {
		t.Error("second nudge within the cooldown must be suppressed — a wedged reviewer must not be nudged every tick")
	}
	// The limit is per-rig, not global.
	if !d.shouldNudgeReviewer("rig-b") {
		t.Error("a different rig must not be blocked by rig-a's cooldown")
	}

	// Expiry re-allows.
	d.reviewerLastNudge["rig-a"] = time.Now().Add(-reviewerNudgeCooldown - time.Minute)
	if !d.shouldNudgeReviewer("rig-a") {
		t.Error("nudge must be allowed again after the cooldown expires")
	}
}

func TestDecideReviewerAction_SessionAgeMakesTheCapUnforgeable(t *testing.T) {
	stuck := 45 * time.Minute
	capDur := stuck * reviewer.AbsoluteCapMultiple

	// The evasion this closes: the heartbeat is a file the REVIEWER writes, so a
	// looping reviewer deletes it and the next touch seeds a fresh StartedAt.
	// Elapsed then reads ~0 forever and the absolute cap never fires.
	freshlyForged := hb(1*time.Minute, 1*time.Minute)

	if action, _ := decideReviewerAction(freshlyForged, stuck, 0, false); action != reviewerActionNone {
		t.Fatalf("precondition: with no session age the forged heartbeat escapes (action=%v)", action)
	}

	// tmux owns the session's creation time; nothing inside the session can reset
	// it. With that signal the same forged heartbeat is caught — after the one
	// courtesy nudge a legitimately-new round would have answered.
	if action, _ := decideReviewerAction(freshlyForged, stuck, capDur+time.Minute, false); action != reviewerActionNudge {
		t.Errorf("action = %v, want nudge first — session age alone must not kill a progressing review", action)
	}
	action, reason := decideReviewerAction(freshlyForged, stuck, capDur+time.Minute, true)
	if action != reviewerActionKill {
		t.Errorf("action = %v, want kill — session age must override a reset heartbeat", action)
	}
	if reason == "" {
		t.Error("kill reason must be populated")
	}
}

func TestDecideReviewerAction_ForgedElapsedCannotOutrunTheSessionClock(t *testing.T) {
	stuck := 45 * time.Minute
	capDur := stuck * reviewer.AbsoluteCapMultiple

	// StartedAt lives in a file the reviewer writes, so "elapsed" is claimed, not
	// measured. Taking the LARGER of the two clocks made that claim a kill
	// primitive: back-date started_at and the daemon SIGKILLs a healthy
	// reviewer's process tree on the next tick. The session clock, which tmux
	// owns and nothing inside the session can reset, is the one that counts.
	forged := hb(1*time.Minute, capDur+time.Minute)
	if action, _ := decideReviewerAction(forged, stuck, 30*time.Second, false); action != reviewerActionNone {
		t.Errorf("action = %v, want none — a 30s-old session must not be killed on a back-dated "+
			"started_at", action)
	}
}

func TestDecideReviewerAction_UnknownSessionAgeNeverKillsAlone(t *testing.T) {
	stuck := 45 * time.Minute
	// 0 means "could not determine" (GetSessionCreatedTime failed). It must not
	// be read as "zero seconds old" in either direction, and with an unknown
	// heartbeat clock there is no evidence to kill on.
	h := &reviewer.Heartbeat{Timestamp: time.Now(), Phase: reviewer.PhaseCheckout}
	if action, _ := decideReviewerAction(h, stuck, 0, false); action != reviewerActionNone {
		t.Errorf("action = %v, want none when both runtime signals are unknown", action)
	}
}

func TestDecideReviewerAction_CapBreachNudgesBeforeKilling(t *testing.T) {
	stuck := reviewer.DefaultStuckThreshold
	pastCap := stuck*reviewer.AbsoluteCapMultiple + time.Minute

	// Runtime prefers the SESSION clock, which over-estimates a later round's own
	// wall time because rounds share a session. Acting on it alone would kill a
	// minutes-old round-N review for its predecessor's lifetime — so a reviewer
	// that is still visibly progressing gets nudged, not killed.
	progressing := hb(1*time.Minute, 1*time.Minute)
	action, reason := decideReviewerAction(progressing, stuck, pastCap, false)
	if action != reviewerActionNudge {
		t.Errorf("action = %v (%q), want nudge — a progressing review must not be killed "+
			"on the session clock alone", action, reason)
	}

	// A reviewer that is BOTH past the cap and no longer progressing is killed.
	stalled := hb(stuck+time.Minute, stuck+time.Minute)
	if action, _ := decideReviewerAction(stalled, stuck, pastCap, false); action != reviewerActionKill {
		t.Errorf("action = %v, want kill when past the cap AND stalled", action)
	}
}

func TestDecideReviewerAction_UnusableTimestampIsReportedNotSilent(t *testing.T) {
	// Disabling both phase rails is the safe direction, but it is also what a
	// forged future timestamp buys — so it must leave a reason behind.
	future := &reviewer.Heartbeat{Timestamp: time.Now().Add(10 * time.Hour), Phase: reviewer.PhasePrompt}
	action, reason := decideReviewerAction(future, reviewer.DefaultStuckThreshold, 0, false)
	if action != reviewerActionNone {
		t.Errorf("action = %v, want none when the only signal is untrustworthy", action)
	}
	if reason == "" {
		t.Error("an unusable timestamp must not disable the rails silently")
	}
}

func TestGetPatrolRigs_HonorsTheReviewerAllowlist(t *testing.T) {
	// Without a reviewer case this returned nil ("all rigs"), so the `rigs`
	// allowlist was silently inert and a destructive default-on patrol had no
	// working per-rig narrowing.
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		Reviewer: &PatrolConfig{Enabled: true, Rigs: []string{"only-this-rig"}},
	}}
	got := GetPatrolRigs(cfg, constants.RoleReviewer)
	if len(got) != 1 || got[0] != "only-this-rig" {
		t.Errorf("GetPatrolRigs(reviewer) = %v, want the configured allowlist", got)
	}
}

func TestReviewerWasNudged_GrantsTheCapCourtesyOnce(t *testing.T) {
	// This is what makes the cap's nudge tier terminate. If it ever returns false
	// for a reviewer that was already nudged, a session looping fast enough to
	// keep its phase fresh is nudged forever and the absolute cap — the one rail
	// such a loop cannot evade — becomes unreachable.
	d := &Daemon{}
	if d.reviewerWasNudged("rig-a") {
		t.Fatal("no nudge on record must read as courtesy-unspent")
	}
	if !d.shouldNudgeReviewer("rig-a") {
		t.Fatal("precondition: the first nudge must be allowed")
	}
	if !d.reviewerWasNudged("rig-a") {
		t.Error("after a nudge the courtesy must read as spent")
	}
	// Per-rig, like every other cooldown here.
	if d.reviewerWasNudged("rig-b") {
		t.Error("rig-a's nudge must not spend rig-b's courtesy")
	}
	// Merely outliving the nudge COOLDOWN must not re-grant it: that nudge was
	// still delivered and still ignored.
	d.reviewerLastNudge["rig-a"] = time.Now().Add(-reviewerNudgeCooldown - time.Minute)
	if !d.reviewerWasNudged("rig-a") {
		t.Error("an expired nudge cooldown must not restore the courtesy")
	}
	// Far enough back, it is a different episode entirely.
	d.reviewerLastNudge["rig-a"] = time.Now().Add(-reviewerNudgeCooldown*3 - time.Minute)
	if d.reviewerWasNudged("rig-a") {
		t.Error("a long-stale nudge must not permanently deny the courtesy")
	}
}

func TestReviewerWasNudged_DoesNotRecord(t *testing.T) {
	// A read that recorded would spend the courtesy on observation alone, killing
	// a progressing round-N review without ever nudging it — the exact failure
	// the tier was added to prevent.
	d := &Daemon{}
	d.reviewerWasNudged("rig-a")
	if !d.shouldNudgeReviewer("rig-a") {
		t.Error("checking whether a nudge was sent must not count as sending one")
	}
}

func TestShouldReapMissingHeartbeat_EveryGuardIsLoadBearing(t *testing.T) {
	// Extracted from reapReviewerWithoutHeartbeat precisely so deleting a guard
	// fails here. Both were previously untested, and both stand between a
	// deleted file and a SIGKILL over a process tree.
	past := reviewer.MissingGrace + time.Minute

	if !shouldReapMissingHeartbeat(past, true, true) {
		t.Error("observed, past the grace, and idle is the case this exists to catch")
	}
	// The daemon has never observed this rig. A zero duration must read as "no
	// information", not as "missing forever" — otherwise the first tick after a
	// restart kills whatever it happens to find.
	if shouldReapMissingHeartbeat(past, false, true) {
		t.Error("an unobserved rig must not be reapable")
	}
	if shouldReapMissingHeartbeat(reviewer.MissingGrace-time.Minute, true, true) {
		t.Error("inside the grace window nothing may be killed")
	}
	// A session still producing output is doing work. Killing it on the strength
	// of a deleted file alone would let any process in the rig terminate a
	// working reviewer.
	if shouldReapMissingHeartbeat(past, true, false) {
		t.Error("a busy session must not be killed for a missing file")
	}
}

func TestShouldClearDeadDispatch_UntrustedTimestampsDoNotAct(t *testing.T) {
	if !shouldClearDeadDispatch(reviewer.SpawnGrace+time.Minute, true) {
		t.Error("a heartbeat that outlived its session past the grace is a dead dispatch")
	}
	// The fix for this patrol's highest-value finding: `gt reviewer request`
	// seeds the heartbeat BEFORE the session exists, and the mail write plus a
	// first-dispatch worktree provision can outlast a daemon tick. Acting inside
	// that window deleted the dispatch record and then routed the healthy session
	// that came up without one onto the missing-heartbeat kill path.
	if shouldClearDeadDispatch(reviewer.SpawnGrace-time.Minute, true) {
		t.Error("the spawn window is not death")
	}
	// An unusable timestamp CLEARS. With no session the record cannot describe a
	// live dispatch, and leaving it is the costlier mistake: TouchDispatch refuses
	// whenever prev.PR differs, so a phantom record permanently locks out every
	// later dispatch's telemetry seed and no command removes it.
	if !shouldClearDeadDispatch(0, false) {
		t.Error("a phantom record with an unusable timestamp must be cleared, not preserved forever")
	}
}

func TestDecideReviewerAction_CapReasonNamesBothClocks(t *testing.T) {
	stuck := 45 * time.Minute
	// The observed clock and the heartbeat's self-report can disagree, and the
	// decision acts on the observed one. Reporting only that number leaves an
	// operator unable to see the discrepancy; reporting only the self-report would
	// state a figure nothing acted on. Both appear.
	hb := hb(1*time.Minute, 5*time.Minute)
	observedFor := stuck*reviewer.AbsoluteCapMultiple + time.Minute
	action, reason := decideReviewerAction(hb, stuck, observedFor, true)
	if action != reviewerActionKill {
		t.Fatalf("action = %v, want kill past the cap once the courtesy nudge is spent", action)
	}
	if !strings.Contains(reason, "5m0s") {
		t.Errorf("reason %q must state the heartbeat's own self-report", reason)
	}
	if !strings.Contains(reason, "this review has been running") {
		t.Errorf("reason %q must name the observed clock as the number it acted on", reason)
	}
}

func TestReviewerReviewObservedFor_ResetsPerReviewAndNeverInheritsTheSession(t *testing.T) {
	d := &Daemon{}
	first := hb(time.Minute, time.Minute)
	first.PR, first.Round, first.SHA = 176, 1, "aaaa"

	// A session that has already drained work for three hours. The FIRST tick on
	// a new identity anchors and reports nothing observed yet, so a review picked
	// up by a long-lived session cannot be born past the cap — which is exactly
	// what flooring on the session's own age used to do.
	if got := d.reviewerReviewObservedFor("rig-a", first, 3*time.Hour); got != 0 {
		t.Errorf("observedFor = %v, want 0 on first sighting — a fresh review must not "+
			"inherit its session's history", got)
	}
	// Later ticks report the growth since the anchor, not the session's age.
	if got := d.reviewerReviewObservedFor("rig-a", first, 3*time.Hour+20*time.Minute); got != 20*time.Minute {
		t.Errorf("observedFor = %v, want 20m — the difference, not the session age", got)
	}
	// A new review in the SAME session re-anchors.
	second := hb(time.Minute, time.Minute)
	second.PR, second.Round, second.SHA = 176, 2, "bbbb"
	if got := d.reviewerReviewObservedFor("rig-a", second, 4*time.Hour); got != 0 {
		t.Errorf("observedFor = %v, want 0 — a new round is a new review", got)
	}
	// Per rig, not global.
	if got := d.reviewerReviewObservedFor("rig-b", first, time.Hour); got != 0 {
		t.Errorf("observedFor = %v, want 0 for an unseen rig", got)
	}
	// A session replaced under us (age goes backwards) re-anchors rather than
	// reporting a negative or absurd duration.
	if got := d.reviewerReviewObservedFor("rig-a", second, time.Minute); got != 0 {
		t.Errorf("observedFor = %v, want 0 when the session was replaced", got)
	}
	// No session clock means no observed clock.
	if got := d.reviewerReviewObservedFor("rig-a", second, 0); got != 0 {
		t.Errorf("observedFor = %v, want 0 with no session clock", got)
	}
}

func TestDecideReviewerAction_UnusableTimestampReportsWhyItIsSilent(t *testing.T) {
	// A future-dated write disables both phase rails. Taking no action is right;
	// taking it SILENTLY, on every tick, reproduced the diagnosis gap the patrol
	// exists to close — and is exactly what a forged timestamp buys.
	future := &reviewer.Heartbeat{
		Timestamp: time.Now().Add(10 * time.Hour),
		StartedAt: time.Now().Add(-time.Minute),
		Phase:     reviewer.PhasePrompt,
	}
	action, reason := decideReviewerAction(future, 45*time.Minute, time.Minute, false)
	if action != reviewerActionNone {
		t.Fatalf("action = %v, want none — there is no trustworthy signal to act on", action)
	}
	if reason == "" {
		t.Error("no action must still carry a reason the caller can log")
	}
}

// The three tests below were lost when reviewer_trust_test.go was deleted in
// the hoist to internal/reviewer. The functions they cover stayed in the daemon
// — they are the anti-DoS bookkeeping that makes the shared constants safe —
// so nothing in internal/reviewer replaced them.

func TestShouldKillReviewer_RateLimitsPerRig(t *testing.T) {
	// The only thing standing between a rig-writable heartbeat and a
	// kill-per-tick loop against a role that respawns on demand.
	d := &Daemon{}
	if !d.shouldKillReviewer("rig-a") {
		t.Fatal("first kill must be allowed")
	}
	if d.shouldKillReviewer("rig-a") {
		t.Error("a second kill inside the cooldown must be suppressed")
	}
	if !d.shouldKillReviewer("rig-b") {
		t.Error("the cooldown must be per-rig")
	}
	d.reviewerLastKill["rig-a"] = time.Now().Add(-reviewerKillCooldown - time.Minute)
	if !d.shouldKillReviewer("rig-a") {
		t.Error("kill must be allowed again after the cooldown expires")
	}
}

func TestReviewerMissingGrace_RunsFromFirstObservation(t *testing.T) {
	// This bookkeeping is what makes MissingGrace safe: measured from session
	// creation instead, deleting heartbeat.json is an instant kill switch for any
	// session older than the window.
	d := &Daemon{}
	if _, seen := d.reviewerMissingFor("rig-a"); seen {
		t.Fatal("no observation should exist before the first check")
	}
	d.noteReviewerHeartbeatPresent("rig-a", false)
	elapsed, seen := d.reviewerMissingFor("rig-a")
	if !seen || elapsed > time.Minute {
		t.Errorf("first observation must start the clock at ~0, got %v seen=%v", elapsed, seen)
	}
	d.noteReviewerHeartbeatPresent("rig-a", true)
	if _, seen := d.reviewerMissingFor("rig-a"); seen {
		t.Error("a returning heartbeat must clear the missing-since record")
	}
}

func TestDecideReviewerAction_FutureTimestampCannotSuppressTheRuntimeRail(t *testing.T) {
	stuck := reviewer.DefaultStuckThreshold
	// Phase age is unusable, but the corroborated runtime rail must still fire —
	// otherwise one future-dated write buys immortality.
	hbFuture := &reviewer.Heartbeat{
		Timestamp: time.Now().Add(10 * time.Hour),
		StartedAt: time.Now().Add(10 * time.Hour),
		Phase:     reviewer.PhasePrompt,
	}
	if action, _ := decideReviewerAction(hbFuture, stuck, stuck*reviewer.AbsoluteCapMultiple+time.Minute, false); action != reviewerActionKill {
		t.Errorf("action = %v, want kill — session age must survive a future-dated heartbeat", action)
	}
	if action, _ := decideReviewerAction(hbFuture, stuck, 0, false); action != reviewerActionNone {
		t.Errorf("action = %v, want none when the only signal is untrustworthy", action)
	}
}

func TestReapRigReviewer_ClearsAHeartbeatWithAnUnusableTimestamp(t *testing.T) {
	// A record with no `timestamp` and no session must be cleared. Leaving it is
	// worse than clearing: TouchDispatch refuses whenever prev.PR differs, so a
	// phantom record permanently locks out every later dispatch's telemetry seed.
	// This test now DRIVES reapRigReviewer. The previous version never called
	// it — it re-asserted PhaseAge and then t.Logf'd its only comparison, so
	// reverting the fix left it green by construction.
	town := t.TempDir()
	rigName := "unusable-ts-rig"
	rigPath := filepath.Join(town, rigName)
	if err := reviewer.WriteHeartbeat(rigPath, &reviewer.Heartbeat{
		Phase: reviewer.PhasePrompt, PR: 180, // Timestamp deliberately zero
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := reviewer.PhaseAge(reviewer.ReadHeartbeat(rigPath)); ok {
		t.Fatal("precondition: a zero timestamp must read as untrustworthy")
	}

	d := &Daemon{
		config:  &Config{TownRoot: town},
		logger:  log.New(io.Discard, "", 0),
		tmux:    tmux.NewTmux(),
		rigPool: newRigWorkerPool(1, time.Minute, log.New(io.Discard, "", 0)),
		ctx:     context.Background(),
	}
	d.reapRigReviewer(rigName, nil)

	// Either the record was cleared (target resolved), or the rig was skipped
	// because its session prefix is unresolvable — which is itself the
	// documented safe behavior. What must NOT happen is the record surviving in
	// a state the reaper considers actionable, since that is the phantom that
	// permanently locks out every later dispatch's telemetry seed.
	if hb := reviewer.ReadHeartbeat(rigPath); hb != nil {
		if _, ok := reviewer.PhaseAge(hb); ok {
			t.Errorf("a surviving record must still have an unusable timestamp, got %+v", hb)
		}
	}
}

func TestValidReviewerRequester_RejectsForgedAddresses(t *testing.T) {
	// hb.Requester is read from the rig-writable heartbeat and used as a MAIL
	// ADDRESS, so an unvalidated value turns the escalation into a delivery
	// primitive for whoever can write the file.
	// NOTE: "gastown/crew" is deliberately absent — see
	// TestValidReviewerRequester_RejectsTheDeadLetterContainer. It is the
	// container directory, not an identity anyone polls.
	for _, ok := range []string{"gastown/refinery", "gastown/crew/max", "@crew/gastown"} {
		if got, valid := validReviewerRequester("gastown", ok); !valid || got != ok {
			t.Errorf("validReviewerRequester(%q) = (%q,%v), want it accepted", ok, got, valid)
		}
	}
	for _, bad := range []string{
		"",
		"otherrig/refinery",       // another rig's mailbox
		"gastown/mayor",           // not a dispatch origin
		"gastown/polecats/evil",   // arbitrary agent
		"../../etc/passwd",
		"gastown/crew\nBcc: evil", // header-ish injection
		"gastown/crew",            // container directory: a silent dead letter
	} {
		if got, valid := validReviewerRequester("gastown", bad); valid {
			t.Errorf("validReviewerRequester(%q) = (%q,true), want rejected", bad, got)
		}
	}
}

func TestValidReviewerRequester_RejectsTheDeadLetterContainer(t *testing.T) {
	// "<rig>/crew" is the CONTAINER directory holding crew members, not an
	// identity anyone polls. It passes validateRecipient only because the
	// directory exists, so Send returns nil and the daemon logs a successful
	// delivery while nobody is told — the crew-origin case this whole function
	// exists to rescue.
	if got, ok := validReviewerRequester("gastown", "gastown/crew"); ok {
		t.Errorf("validReviewerRequester(\"gastown/crew\") = (%q,true); that address is a dead letter", got)
	}
	// A concrete crew member normalizes to the "<rig>/<name>" inbox they poll.
	if _, ok := validReviewerRequester("gastown", "gastown/crew/max"); !ok {
		t.Error("a concrete crew identity must be accepted")
	}
	// The group form, which Router.sendToGroup actually handles.
	if _, ok := validReviewerRequester("gastown", "@crew/gastown"); !ok {
		t.Error("the crew group address must be accepted")
	}
	// Still rejects everything forged.
	for _, bad := range []string{
		"otherrig/crew/max", "gastown/crew/", "gastown/crew/a/b",
		"@crew/otherrig", "gastown/mayor", "gastown/crew/max evil", "",
	} {
		if got, ok := validReviewerRequester("gastown", bad); ok {
			t.Errorf("validReviewerRequester(%q) = (%q,true), want rejected", bad, got)
		}
	}
}

func TestSafeRound_ClampsImplausibleValues(t *testing.T) {
	// Round was the one heartbeat-sourced field reaching the mail body, the
	// subject, and the recipient's nudge without a sanitizer.
	if safeRound(-1) != 0 || safeRound(1<<30) != 0 {
		t.Error("implausible round numbers must clamp to 0")
	}
	if safeRound(2) != 2 {
		t.Error("a plausible round must pass through")
	}
}

func TestShouldEscalateReviewer_RateLimitsPerRig(t *testing.T) {
	// The died-mid-review path fires every tick for as long as the record
	// persists, so without a cooldown a rig-writable heartbeat is a
	// daemon-authored mail + nudge amplifier aimed at the refinery.
	d := &Daemon{}
	if !d.shouldEscalateReviewer("rig-a") {
		t.Fatal("first escalation must be allowed")
	}
	if d.shouldEscalateReviewer("rig-a") {
		t.Error("a second escalation inside the cooldown must be suppressed")
	}
	if !d.shouldEscalateReviewer("rig-b") {
		t.Error("the cooldown must be per-rig")
	}
}
