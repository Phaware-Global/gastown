package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reviewer"
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
	action, _ := decideReviewerAction(hb(10*time.Minute, 12*time.Minute), stuck, 0)
	if action != reviewerActionNone {
		t.Errorf("action = %v, want none — a healthy in-flight review must not be touched", action)
	}
}

func TestDecideReviewerAction_LongSubagentPassBelowThresholdSurvives(t *testing.T) {
	stuck := 45 * time.Minute
	// 44 minutes parked at PhasePrompt is legitimate: the perspective subagents
	// run with no command in between to refresh the timestamp. Killing here
	// would reap working reviewers, which is worse than the gap being closed.
	action, _ := decideReviewerAction(hb(44*time.Minute, 44*time.Minute), stuck, 0)
	if action != reviewerActionNone {
		t.Errorf("action = %v, want none just below the threshold", action)
	}
}

func TestDecideReviewerAction_NudgesAtThreshold(t *testing.T) {
	stuck := 45 * time.Minute
	action, reason := decideReviewerAction(hb(46*time.Minute, 46*time.Minute), stuck, 0)
	if action != reviewerActionNudge {
		t.Fatalf("action = %v, want nudge — the cheap recovery comes before the kill", action)
	}
	if reason == "" {
		t.Error("nudge reason must be populated for the operator log")
	}
}

func TestDecideReviewerAction_KillsAtTwiceTheThreshold(t *testing.T) {
	stuck := 45 * time.Minute
	action, reason := decideReviewerAction(hb(91*time.Minute, 91*time.Minute), stuck, 0)
	if action != reviewerActionKill {
		t.Fatalf("action = %v, want kill past %dx the threshold", action, reviewerKillMultiple)
	}
	if reason == "" {
		t.Error("kill reason must be populated — a silent kill reproduces the diagnosis gap")
	}
}

func TestDecideReviewerAction_AbsoluteCapCatchesRefreshingLoop(t *testing.T) {
	stuck := 45 * time.Minute
	// The rail that matters most: a reviewer looping through phases refreshes
	// its timestamp forever, so phase age never trips. Only total elapsed can
	// stop it.
	freshPhase := 1 * time.Minute
	pastCap := stuck*reviewerAbsoluteCapMultiple + time.Minute

	if action, _ := decideReviewerAction(hb(freshPhase, pastCap), stuck, 0); action != reviewerActionKill {
		t.Errorf("action = %v, want kill: a fresh phase must not exempt a review past the absolute cap", action)
	}
}

func TestDecideReviewerAction_UnknownStartedAtNeverTripsTheCap(t *testing.T) {
	stuck := 45 * time.Minute
	// Elapsed() is 0 when StartedAt is unset. Zero must read as "no data", not
	// as "infinitely old" — otherwise every heartbeat lacking StartedAt is
	// killed on sight.
	h := &reviewer.Heartbeat{Timestamp: time.Now(), Phase: reviewer.PhaseCheckout}
	if action, reason := decideReviewerAction(h, stuck, 0); action != reviewerActionNone {
		t.Errorf("action = %v (%q), want none when StartedAt is unknown", action, reason)
	}
}

func TestDecideReviewerAction_NilHeartbeatIsNoAction(t *testing.T) {
	// The nil case is handled by the caller (orphan-session path); the decision
	// rule itself must not claim a kill on no data.
	if action, _ := decideReviewerAction(nil, 45*time.Minute, 0); action != reviewerActionNone {
		t.Errorf("action = %v, want none for a nil heartbeat", action)
	}
}

func TestDecideReviewerAction_NonPositiveThresholdDisablesTheReaper(t *testing.T) {
	// A misconfigured (zero/negative) threshold must fail safe to "do nothing"
	// rather than to "kill everything immediately".
	for _, stuck := range []time.Duration{0, -time.Minute} {
		if action, _ := decideReviewerAction(hb(10*time.Hour, 10*time.Hour), stuck, 0); action != reviewerActionNone {
			t.Errorf("stuck=%v: action = %v, want none (fail safe)", stuck, action)
		}
	}
}

func TestDecideReviewerAction_ThresholdScalesWithConfig(t *testing.T) {
	// A rig that raises its stuck_threshold must actually get more headroom —
	// proves the reaper honors reviewer.toml rather than a hardcoded constant.
	age := 50 * time.Minute
	if action, _ := decideReviewerAction(hb(age, age), 45*time.Minute, 0); action != reviewerActionNudge {
		t.Errorf("45m threshold: action = %v, want nudge at %v", action, age)
	}
	if action, _ := decideReviewerAction(hb(age, age), 2*time.Hour, 0); action != reviewerActionNone {
		t.Errorf("2h threshold: action = %v, want none at %v", action, age)
	}
}

func TestReviewerReaperThresholdOrdering(t *testing.T) {
	// The reaper must never fire before the refinery's await-review timeout
	// (30m): the escalation carries diagnostic value that a silent kill
	// destroys, so the escalation has to come first.
	const prReviewTimeout = 30 * time.Minute
	if defaultReviewerStuckThreshold <= prReviewTimeout {
		t.Errorf("stuck threshold %v must exceed pr_review_timeout %v so the refinery escalates first",
			defaultReviewerStuckThreshold, prReviewTimeout)
	}
	if reviewerKillMultiple < 2 {
		t.Error("kill multiple must leave room for at least one nudge before killing")
	}
	if reviewerAbsoluteCapMultiple <= reviewerKillMultiple {
		t.Errorf("absolute cap multiple (%d) must exceed the kill multiple (%d), or the cap is unreachable",
			reviewerAbsoluteCapMultiple, reviewerKillMultiple)
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
	capDur := stuck * reviewerAbsoluteCapMultiple

	// The evasion this closes: the heartbeat is a file the REVIEWER writes, so a
	// looping reviewer deletes it and the next touch seeds a fresh StartedAt.
	// Elapsed then reads ~0 forever and the absolute cap never fires.
	freshlyForged := hb(1*time.Minute, 1*time.Minute)

	if action, _ := decideReviewerAction(freshlyForged, stuck, 0); action != reviewerActionNone {
		t.Fatalf("precondition: with no session age the forged heartbeat escapes (action=%v)", action)
	}

	// tmux owns the session's creation time; nothing inside the session can reset
	// it. With that signal the same forged heartbeat is caught.
	action, reason := decideReviewerAction(freshlyForged, stuck, capDur+time.Minute)
	if action != reviewerActionKill {
		t.Errorf("action = %v, want kill — session age must override a reset heartbeat", action)
	}
	if reason == "" {
		t.Error("kill reason must be populated")
	}
}

func TestDecideReviewerAction_TakesTheLongerOfSessionAgeAndElapsed(t *testing.T) {
	stuck := 45 * time.Minute
	capDur := stuck * reviewerAbsoluteCapMultiple

	// A heartbeat seeded by the DISPATCHER predates the session, so it can
	// legitimately report more elapsed time than the session has existed. The
	// larger signal must win in that direction too.
	longHeartbeat := hb(1*time.Minute, capDur+time.Minute)
	if action, _ := decideReviewerAction(longHeartbeat, stuck, 30*time.Second); action != reviewerActionKill {
		t.Errorf("action = %v, want kill — a heartbeat older than the session must still count", action)
	}
}

func TestDecideReviewerAction_UnknownSessionAgeNeverKillsAlone(t *testing.T) {
	stuck := 45 * time.Minute
	// 0 means "could not determine" (GetSessionCreatedTime failed). It must not
	// be read as "zero seconds old" in either direction, and with an unknown
	// heartbeat clock there is no evidence to kill on.
	h := &reviewer.Heartbeat{Timestamp: time.Now(), Phase: reviewer.PhaseCheckout}
	if action, _ := decideReviewerAction(h, stuck, 0); action != reviewerActionNone {
		t.Errorf("action = %v, want none when both runtime signals are unknown", action)
	}
}
