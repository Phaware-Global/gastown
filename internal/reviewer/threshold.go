package reviewer

import (
	"time"

)

// MinPassDuration is the floor for a perspective pass's wall-clock budget.
//
// A budget below this cannot produce a review worth posting — the pass would
// stop before it had established anything — so a sub-minute value is not a
// tuning choice but a way to rubber-stamp every PR. Callers clamp to it.
const MinPassDuration = 5 * time.Minute

// ClampPassDuration bounds a pass budget to [MinPassDuration, stuck].
//
// The ceiling matters as much as the floor and is the mirror-image injection: a
// budget at or beyond the rig's stuck threshold guarantees the pass outruns the
// reaper's phase rail, so the session is killed mid-pass and every finding it
// had established is discarded — exactly the outcome §5's budget exists to
// prevent. An out-of-range value is corrected rather than honored.
func ClampPassDuration(d, stuck time.Duration) time.Duration {
	if stuck <= 0 {
		stuck = DefaultStuckThreshold
	}
	if d >= stuck {
		d = stuck / 2
	}
	// Floor LAST. Applying it first let the ceiling hand back a sub-floor value
	// whenever stuck/2 < MinPassDuration — the ceiling could undo the floor.
	if d < MinPassDuration {
		return MinPassDuration
	}
	return d
}

// PassDuration returns the soft wall-clock budget for one perspective subagent
// pass on a rig: half that rig's configured stuck threshold, floored at
// MinPassDuration.
//
// Derived from the rig's threshold rather than a compile-time constant, because
// the passes run in parallel between `gt reviewer prompt` and `gt reviewer
// consolidate` with nothing refreshing the heartbeat — so the slowest pass is
// exactly what the reaper's phase-age rail measures. A rig that raises its
// threshold must get proportionally more budget, or its passes trip a rail it
// was configured to avoid. A fixed constant silently broke that relationship,
// and the --max-duration help text already promised this behavior.
func PassDuration(townRoot, rigPath string) time.Duration {
	stuck, _ := ClampStuckThreshold(StuckThreshold(townRoot, rigPath))
	if d := stuck / 2; d > MinPassDuration {
		return d
	}
	return MinPassDuration
}

// IsWedged reports whether a live session's heartbeat shows it has stopped
// draining work — no phase progress for StuckMultiple thresholds.
//
// Used at dispatch time. A session that is alive but wedged is the worst case
// for EnsureRunning's idempotency: CheckSessionHealth calls it "healthy" (tmux
// up, agent process up), so a second request mails into a mailbox nobody will
// read. The refinery then times out at 30m, escalates, and the wedged session
// survives to swallow the next round too.
//
// A nil heartbeat is NOT wedged: it means no review is on record, the normal
// state for a session that just finished one. An untrustworthy phase age (zero
// or future timestamp) is likewise not wedged — dispatch must never be more
// aggressive than the reaper, which treats those as "no progress signal".
func IsWedged(hb *Heartbeat, stuck time.Duration) bool {
	if hb == nil || stuck <= 0 {
		return false
	}
	// Clamp before multiplying. An unclamped rig-configured threshold overflows
	// `stuck * StuckMultiple` into a negative duration, which inverts the
	// comparison below into "always wedged" — turning an agent-writable config
	// value into an unconditional recycle of every reviewer.
	stuck, _ = ClampStuckThreshold(stuck)
	age, ok := PhaseAge(hb)
	if !ok {
		return false
	}
	return age >= stuck*StuckMultiple
}
