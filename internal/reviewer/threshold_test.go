package reviewer

import (
	"strings"
	"testing"
	"time"
)

func TestIsWedged_NilHeartbeatIsNotWedged(t *testing.T) {
	// No heartbeat means no review on record — the normal state for a session
	// that just finished one. Treating it as wedged would recycle healthy idle
	// sessions on every dispatch.
	if IsWedged(nil, DefaultStuckThreshold) {
		t.Error("a nil heartbeat must not be wedged")
	}
}

func TestIsWedged_NonPositiveThresholdFailsSafe(t *testing.T) {
	old := &Heartbeat{Timestamp: time.Now().Add(-10 * time.Hour)}
	for _, d := range []time.Duration{0, -time.Minute} {
		if IsWedged(old, d) {
			t.Errorf("threshold %v: must fail safe to not-wedged rather than recycling everything", d)
		}
	}
}

func TestIsWedged_WorkingReviewIsNotWedged(t *testing.T) {
	// A pass mid-subagent: parked at PhasePrompt but well inside the threshold.
	hb := &Heartbeat{Timestamp: time.Now().Add(-20 * time.Minute), Phase: PhasePrompt}
	if IsWedged(hb, DefaultStuckThreshold) {
		t.Error("a review inside its threshold must not be recycled — that would kill working reviewers")
	}
}

func TestIsWedged_OnlyPastTheKillMultiple(t *testing.T) {
	stuck := DefaultStuckThreshold
	// Between 1x and 2x the reaper only NUDGES. Dispatch must not be more
	// aggressive than the reaper, or a session gets recycled out from under a
	// nudge that would have revived it.
	justOver := &Heartbeat{Timestamp: time.Now().Add(-(stuck + time.Minute)), Phase: PhasePrompt}
	if IsWedged(justOver, stuck) {
		t.Error("dispatch must not recycle in the nudge band (1x–2x) — the reaper gets to try a nudge first")
	}

	wayOver := &Heartbeat{Timestamp: time.Now().Add(-(stuck*StuckMultiple + time.Minute)), Phase: PhasePrompt}
	if !IsWedged(wayOver, stuck) {
		t.Errorf("past %dx the threshold the session must be recycled rather than mailed into", StuckMultiple)
	}
}

func TestDefaultPassDuration_LeavesRoomBeforeTheReaperActs(t *testing.T) {
	// A pass must be able to blow its budget and still return a partial result
	// before the reaper starts nudging; otherwise the budget buys nothing.
	if DefaultPassDuration >= DefaultStuckThreshold {
		t.Errorf("pass budget %v must be under the stuck threshold %v, or a pass that "+
			"honors its budget still trips the reaper", DefaultPassDuration, DefaultStuckThreshold)
	}
}

func TestDefaultStuckThreshold_ExceedsRefineryEscalation(t *testing.T) {
	// Pinned here as well as in the daemon: the refinery's await-review timeout
	// (30m) must escalate before anything kills the session, because the
	// escalation carries diagnostics a silent kill destroys.
	const prReviewTimeout = 30 * time.Minute
	if DefaultStuckThreshold <= prReviewTimeout {
		t.Errorf("stuck threshold %v must exceed pr_review_timeout %v", DefaultStuckThreshold, prReviewTimeout)
	}
}

func TestBuildPerspectivePrompt_RendersTimeBudget(t *testing.T) {
	out, err := BuildPerspectivePrompt(PromptParams{
		Perspective: "adversarial",
		Lens:        "look for bugs",
		RigName:     "gastown",
		PR:          175,
		SHA:         "deadbeef",
		MaxDuration: 12 * time.Minute,
	})
	if err != nil {
		t.Fatalf("BuildPerspectivePrompt: %v", err)
	}
	if !strings.Contains(out, "12m0s") {
		t.Error("the pass budget must appear in the rendered contract — nothing else conveys it to the subagent")
	}
	// The instruction that matters is the fallback behavior, not the number.
	if !strings.Contains(strings.ToLower(out), "partial") {
		t.Error("the contract must tell the pass to return a PARTIAL result rather than hang")
	}
}

func TestBuildPerspectivePrompt_DefaultsTimeBudget(t *testing.T) {
	out, err := BuildPerspectivePrompt(PromptParams{
		Perspective: "security", Lens: "look for holes", RigName: "r", PR: 1, SHA: "abc",
	})
	if err != nil {
		t.Fatalf("BuildPerspectivePrompt: %v", err)
	}
	if !strings.Contains(out, DefaultPassDuration.String()) {
		t.Errorf("an unset MaxDuration must render DefaultPassDuration (%v), not a zero budget",
			DefaultPassDuration)
	}
	// A zero budget would read as "you have no time", which is worse than absent.
	if strings.Contains(out, "budget of **0s**") {
		t.Error("must never render a zero budget")
	}
}
