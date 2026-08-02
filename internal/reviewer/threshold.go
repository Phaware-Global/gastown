package reviewer

import (
	"time"

)

// DefaultPassDuration is the soft wall-clock budget given to one perspective
// subagent pass.
//
// Sized against DefaultStuckThreshold, not independently: the passes run in
// parallel between `gt reviewer prompt` and `gt reviewer consolidate` with
// nothing refreshing the heartbeat, so the slowest pass is what the reaper's
// phase-age rail actually measures. A budget at half the stuck threshold leaves
// a pass room to run long and still return a partial result before the reaper
// starts nudging.
const DefaultPassDuration = DefaultStuckThreshold / 2

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
	age, ok := PhaseAge(hb)
	if !ok {
		return false
	}
	return age >= stuck*StuckMultiple
}
