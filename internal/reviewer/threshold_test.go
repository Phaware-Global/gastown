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

func TestPassDuration_DerivesFromTheRigAndHasAFloor(t *testing.T) {
	// An unconfigured rig falls back to the default threshold; the budget must
	// leave room to blow it and still return before the reaper nudges.
	d := PassDuration(t.TempDir(), t.TempDir())
	if d >= DefaultStuckThreshold {
		t.Errorf("pass budget %v must be under the stuck threshold %v, or a pass that "+
			"honors its budget still trips the reaper", d, DefaultStuckThreshold)
	}
	if d < MinPassDuration {
		t.Errorf("pass budget %v is below the floor %v — a budget that small stops a "+
			"pass before it establishes anything, which rubber-stamps the PR", d, MinPassDuration)
	}
}

func TestIsWedged_ClampsBeforeMultiplyingToAvoidOverflow(t *testing.T) {
	// An unclamped rig-configured threshold overflows stuck*StuckMultiple into a
	// negative duration, inverting the comparison into "always wedged" — an
	// agent-writable config value becoming an unconditional recycle.
	hb := &Heartbeat{Timestamp: time.Now(), Phase: PhasePrompt}
	if IsWedged(hb, time.Duration(1<<62)) {
		t.Error("a fresh heartbeat must not be wedged under an absurd threshold (overflow)")
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

func TestBuildPerspectivePrompt_FloorsTheTimeBudget(t *testing.T) {
	// An unset or absurdly small budget must be corrected UP to the floor. A
	// sub-minute budget tells the pass to stop before it has established
	// anything, which posts an APPROVE on an unreviewed diff.
	for _, given := range []time.Duration{0, -time.Hour, time.Second} {
		out, err := BuildPerspectivePrompt(PromptParams{
			Perspective: "security", Lens: "look for holes", RigName: "r", PR: 1, SHA: "abc",
			MaxDuration: given,
		})
		if err != nil {
			t.Fatalf("BuildPerspectivePrompt(%v): %v", given, err)
		}
		if !strings.Contains(out, MinPassDuration.String()) {
			t.Errorf("MaxDuration=%v must render the floor %v", given, MinPassDuration)
		}
		if strings.Contains(out, "budget of **0s**") {
			t.Error("must never render a zero budget")
		}
	}
}

func TestBuildPerspectivePrompt_BudgetExhaustionMustSetADisposition(t *testing.T) {
	// The gap this closes: a pass that stops early with zero findings and no
	// disposition is indistinguishable from a clean pass, so the review posts as
	// APPROVE — an endorsement of a diff the pass did not finish reading.
	out, err := BuildPerspectivePrompt(PromptParams{
		Perspective: "adversarial", Lens: "look", RigName: "r", PR: 1, SHA: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Asserted WITHOUT the `"disposition": "comment"` JSON literal, deliberately.
	// TestSchemaExamplesDoNotSeedAnActionableDisposition scans the whole template
	// for that key/value shape regardless of fencing — correctly, since a pass
	// copying an actionable literal posts a verdict nobody asked for — so this
	// instruction has to name the field and the value in prose rather than model
	// them. Asserting on the rendered prose keeps the requirement enforced while
	// leaving nothing copyable behind.
	// Whitespace-normalized so the assertion survives a re-wrap of the paragraph.
	flat := strings.Join(strings.Fields(out), " ")
	if !strings.Contains(flat, "`disposition` field to `comment`") {
		t.Error("the budget section must tell a truncated pass to set disposition=comment; " +
			"verdict alone is free text no code reads")
	}
	if !strings.Contains(out, "APPROVE") {
		t.Error("the contract should state the consequence (a posted APPROVE), not just the rule")
	}
}

func TestClampPassDuration_BoundsBothDirections(t *testing.T) {
	stuck := DefaultStuckThreshold

	// Too small: the pass stops before it establishes anything — a rubber stamp.
	if got := ClampPassDuration(time.Second, stuck); got != MinPassDuration {
		t.Errorf("ClampPassDuration(1s) = %v, want the floor %v", got, MinPassDuration)
	}
	// Too large: the pass outruns the reaper's phase rail, so the session is
	// killed mid-pass and every established finding is discarded — the exact
	// outcome the budget exists to prevent. The mirror image of the floor.
	//
	// Assert BOTH bounds on the result, not just the ceiling: a ceiling that
	// returned 1ns satisfied "< stuck" while violating the floor, so the old
	// assertion passed under a mutation that broke the function.
	if got := ClampPassDuration(10*time.Hour, stuck); got >= stuck || got < MinPassDuration {
		t.Errorf("ClampPassDuration(10h) = %v, want within [%v, %v)", got, MinPassDuration, stuck)
	}
	// A small threshold makes the ceiling (stuck/2) fall BELOW the floor. The
	// floor must still win — otherwise the ceiling silently undoes it.
	small := 2 * MinPassDuration
	if got := ClampPassDuration(10*time.Hour, small); got < MinPassDuration {
		t.Errorf("ClampPassDuration(10h, %v) = %v, want >= the floor %v — the ceiling must not undo the floor",
			small, got, MinPassDuration)
	}
	if got := ClampPassDuration(stuck, stuck); got >= stuck {
		t.Errorf("a budget equal to the threshold must also be reduced, got %v", got)
	}
	// In range passes through.
	if got := ClampPassDuration(20*time.Minute, stuck); got != 20*time.Minute {
		t.Errorf("ClampPassDuration(20m) = %v, want it unchanged", got)
	}
	// A nonsense threshold falls back rather than producing a nonsense budget.
	if got := ClampPassDuration(20*time.Minute, 0); got <= 0 {
		t.Errorf("ClampPassDuration with a zero threshold = %v, want a sane budget", got)
	}
}
