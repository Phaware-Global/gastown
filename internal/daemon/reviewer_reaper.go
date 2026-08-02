package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/reviewer"
	"github.com/steveyegge/gastown/internal/session"
)

// Reviewer reaper thresholds. These are the compiled-in floor; a rig's
// reviewer.toml [health] block overrides StuckThreshold per rig.
const (
	// defaultReviewerStuckThreshold matches reviewer.toml's shipped value. It is
	// deliberately longer than merge_queue.pr_review_timeout (30m) so the
	// refinery escalates a non-engaging reviewer BEFORE the reaper kills it —
	// the escalation carries diagnostic value that a silent kill would destroy.
	defaultReviewerStuckThreshold = 45 * time.Minute

	// reviewerKillMultiple: a heartbeat this many times past the stuck threshold
	// is killed. One threshold nudges (a wedged agent often unsticks), the next
	// kills. Two tiers instead of a nudge-tracking state machine.
	reviewerKillMultiple = 2

	// reviewerAbsoluteCapMultiple bounds TOTAL review wall time (heartbeat
	// elapsed), independent of phase age. This is the rail a reviewer that keeps
	// refreshing its heartbeat while making no real progress cannot evade —
	// phase age alone would let it run forever.
	reviewerAbsoluteCapMultiple = 4

	// reviewerNudgeCooldown rate-limits stuck nudges so a wedged reviewer is not
	// nudged on every heartbeat tick.
	reviewerNudgeCooldown = 10 * time.Minute

	// reviewerOrphanGrace is how long a live reviewer session may have no
	// heartbeat before it is reaped. `gt reviewer done` clears the heartbeat and
	// then kills its own session ~3s later, so a brief no-heartbeat window is
	// normal; anything past this grace is a session whose self-termination
	// failed, or one started outside the dispatch path.
	reviewerOrphanGrace = 15 * time.Minute
)

// reapStuckReviewers checks every known rig's Reviewer session and nudges or
// kills the ones that have stopped progressing.
//
// The Reviewer is spawn-on-demand and has no persistent registry entry and no
// agent bead, so — unlike every other role — it cannot be enumerated from
// existing daemon state. It is enumerated here from the rig list plus the
// presence of a heartbeat or a live session. Before this patrol existed nothing
// in the town could reap a hung reviewer: `gt reviewer done` was the only exit,
// and it requires the very agent that is stuck to run it.
func (d *Daemon) reapStuckReviewers() {
	d.rigPool.runPerRig(d.ctx, d.getKnownRigs(), func(ctx context.Context, rigName string) error {
		d.reapRigReviewer(rigName)
		return nil
	})
}

// reviewerStuckThreshold resolves a rig's reviewer stuck threshold from its
// role definition, falling back to the compiled-in default. This is the first
// real consumer of RoleDefinition.Health — until now reviewer.toml's
// stuck_threshold was read only by `gt role def`'s printer and enforced nothing.
func (d *Daemon) reviewerStuckThreshold(rigPath string) time.Duration {
	def, err := config.LoadRoleDefinition(d.config.TownRoot, rigPath, constants.RoleReviewer)
	if err != nil || def == nil || def.Health.StuckThreshold.Duration <= 0 {
		return defaultReviewerStuckThreshold
	}
	return def.Health.StuckThreshold.Duration
}

// reapRigReviewer evaluates one rig's reviewer and takes at most one action.
//
// The heartbeat and the session are independent signals, and their four
// combinations mean different things:
//
//	none / none   → idle rig, nothing to do
//	none / alive  → self-termination failed (or a hand-started session): reap past grace
//	present / none → the reviewer died mid-review: clear the stale heartbeat
//	present/alive → a review in flight: apply the progress and runtime rails
func (d *Daemon) reapRigReviewer(rigName string) {
	rigPath := filepath.Join(d.config.TownRoot, rigName)
	sessionName := session.ReviewerSessionName(session.PrefixFor(rigName))

	alive, err := d.tmux.HasSession(sessionName)
	if err != nil {
		d.logger.Printf("Reviewer reaper: checking session %s: %v", sessionName, err)
		return
	}
	hb := reviewer.ReadHeartbeat(rigPath)

	if hb == nil {
		if alive {
			d.reapOrphanReviewerSession(rigName, sessionName)
		}
		return
	}

	if !alive {
		// A heartbeat with no session means the reviewer died mid-review. There
		// is nothing to kill; clear the record so the rig doesn't look
		// permanently stalled, and surface it — the refinery's await-review
		// timeout is what re-dispatches, but a silent death should still be
		// visible in the feed.
		d.logger.Printf("Reviewer reaper: %s reviewer died mid-review (phase=%s pr=%d after %s) — clearing stale heartbeat",
			rigName, hb.Phase, hb.PR, hb.Elapsed().Round(time.Second))
		_ = events.LogFeed(events.TypeSessionDeath, rigName+"/"+constants.RoleReviewer,
			map[string]interface{}{
				"rig": rigName, "role": constants.RoleReviewer, "reason": "died_mid_review",
				"phase": hb.Phase, "pr": hb.PR, "elapsed": hb.Elapsed().Round(time.Second).String(),
			})
		if cerr := reviewer.ClearHeartbeat(rigPath); cerr != nil {
			d.logger.Printf("Reviewer reaper: clearing %s heartbeat: %v", rigName, cerr)
		}
		return
	}

	d.enforceReviewerProgress(rigName, sessionName, rigPath, hb)
}

// reviewerAction is what the reaper decides to do with a live reviewer.
type reviewerAction int

const (
	reviewerActionNone reviewerAction = iota
	reviewerActionNudge
	reviewerActionKill
)

// decideReviewerAction is the reaper's decision rule, kept pure (no daemon,
// tmux, or filesystem) so the thresholds can be tested directly — the logic
// that decides to kill an agent deserves tests that don't need a live town.
//
// Two independent rails:
//
//	elapsed — total review wall time. Bounds a reviewer that keeps refreshing
//	          its heartbeat while looping, which phase age alone cannot catch.
//	age     — time in the current phase. Note that a heartbeat parked at
//	          PhasePrompt is the NORMAL shape of a review in flight: the
//	          perspective subagents run entirely between the last `gt reviewer
//	          prompt` and `gt reviewer consolidate`, with no command in between
//	          to refresh the timestamp. The threshold must accommodate a full
//	          subagent pass, which is why it defaults to 45m.
func decideReviewerAction(hb *reviewer.Heartbeat, stuck, sessionAge time.Duration) (reviewerAction, string) {
	if hb == nil || stuck <= 0 {
		return reviewerActionNone, ""
	}
	age := hb.Age()

	// The runtime the cap acts on is the LONGER of the session's own age and the
	// heartbeat's self-reported elapsed.
	//
	// hb.Elapsed() alone is forgeable by the process the cap exists to constrain:
	// the heartbeat is a file the reviewer writes, so deleting or corrupting it
	// yields a fresh clock on the next touch, and a looping reviewer evades the
	// cap forever. The tmux session's creation time is owned by tmux, not by the
	// reviewer, so it cannot be reset from inside the session — taking the max
	// makes the cap unforgeable while still honoring a heartbeat that (via a
	// dispatcher seed predating the session) reports MORE elapsed time.
	elapsed := hb.Elapsed()
	if sessionAge > elapsed {
		elapsed = sessionAge
	}

	// Elapsed is zero only when both signals are unknown. Zero must never trip
	// the cap, so this guard is load-bearing rather than defensive: without it,
	// every heartbeat missing a StartedAt would be killed on sight.
	if capDur := stuck * reviewerAbsoluteCapMultiple; elapsed > 0 && elapsed >= capDur {
		return reviewerActionKill, fmt.Sprintf("exceeded absolute runtime cap (%s of %s)",
			elapsed.Round(time.Second), capDur)
	}
	switch {
	case age >= stuck*reviewerKillMultiple:
		return reviewerActionKill, fmt.Sprintf("no progress for %s (phase=%s, %dx the %s threshold)",
			age.Round(time.Second), hb.Phase, reviewerKillMultiple, stuck)
	case age >= stuck:
		return reviewerActionNudge, fmt.Sprintf("no progress for %s (phase=%s)",
			age.Round(time.Second), hb.Phase)
	}
	return reviewerActionNone, ""
}

// enforceReviewerProgress applies decideReviewerAction to a live reviewer.
func (d *Daemon) enforceReviewerProgress(rigName, sessionName, rigPath string, hb *reviewer.Heartbeat) {
	stuck := d.reviewerStuckThreshold(rigPath)
	action, reason := decideReviewerAction(hb, stuck, d.reviewerSessionAge(sessionName))
	switch action {
	case reviewerActionKill:
		d.killStuckReviewer(rigName, sessionName, rigPath, hb, reason)
	case reviewerActionNudge:
		d.nudgeStuckReviewer(rigName, sessionName, hb, hb.Age(), stuck)
	case reviewerActionNone:
	}
}

// reviewerSessionAge returns how long the reviewer's tmux session has been up,
// or 0 when it cannot be determined. Unlike the heartbeat, this clock is owned
// by tmux rather than by the reviewed process, so it cannot be reset from inside
// the session. 0 means "unknown" and never contributes to a kill decision.
func (d *Daemon) reviewerSessionAge(sessionName string) time.Duration {
	created, err := d.tmux.GetSessionCreatedTime(sessionName)
	if err != nil || created.IsZero() {
		return 0
	}
	return time.Since(created)
}

// nudgeStuckReviewer pokes a reviewer that has stalled but is not yet past the
// kill threshold. A wedged agent frequently resumes on a nudge, so this is the
// cheap recovery attempt before the expensive one.
func (d *Daemon) nudgeStuckReviewer(rigName, sessionName string, hb *reviewer.Heartbeat, age, stuck time.Duration) {
	if !d.shouldNudgeReviewer(rigName) {
		return
	}
	msg := fmt.Sprintf(
		"REVIEWER_STALLED: no progress for %s (phase=%s, PR #%d). Continue the review, "+
			"or if you cannot, post what you have via `gt reviewer post` and run `gt reviewer done`.",
		age.Round(time.Second), hb.Phase, hb.PR)
	if err := d.tmux.NudgeSession(sessionName, msg); err != nil {
		d.logger.Printf("Reviewer reaper: nudging %s: %v", sessionName, err)
		return
	}
	d.logger.Printf("Reviewer reaper: nudged %s reviewer (stalled %s at phase=%s, threshold %s)",
		rigName, age.Round(time.Second), hb.Phase, stuck)
}

// shouldNudgeReviewer rate-limits stuck nudges to one per cooldown per rig, so
// a wedged reviewer isn't nudged on every heartbeat tick. State is in-memory
// only: losing it on daemon restart costs at most one extra nudge, which is far
// cheaper than persisting it.
func (d *Daemon) shouldNudgeReviewer(rigName string) bool {
	// runPerRig fans out concurrently across rigs, so this map needs a lock.
	d.reviewerNudgeMu.Lock()
	defer d.reviewerNudgeMu.Unlock()
	if d.reviewerLastNudge == nil {
		d.reviewerLastNudge = make(map[string]time.Time)
	}
	if last, ok := d.reviewerLastNudge[rigName]; ok && time.Since(last) < reviewerNudgeCooldown {
		return false
	}
	d.reviewerLastNudge[rigName] = time.Now()
	return true
}

// killStuckReviewer terminates a reviewer session and clears its heartbeat.
//
// The kill is loud by design: it is logged, emitted to the feed, and the reason
// is recorded. A reviewer session dying without explanation was the original
// diagnosis problem, and a reaper that reproduced it silently would be no
// better than the gap it closes.
func (d *Daemon) killStuckReviewer(rigName, sessionName, rigPath string, hb *reviewer.Heartbeat, reason string) {
	d.logger.Printf("Reviewer reaper: killing %s reviewer — %s (pr=%d phase=%s sha=%s)",
		rigName, reason, hb.PR, hb.Phase, hb.SHA)

	if err := d.tmux.KillSessionWithProcesses(sessionName); err != nil {
		d.logger.Printf("Reviewer reaper: killing session %s: %v", sessionName, err)
		// Fall through: still clear the heartbeat. Leaving it would make the rig
		// look stalled forever while re-attempting a kill that is failing anyway.
	}
	_ = events.LogFeed(events.TypeKill, rigName+"/"+constants.RoleReviewer,
		map[string]interface{}{
			"rig": rigName, "role": constants.RoleReviewer, "reason": reason,
			"pr": hb.PR, "phase": hb.Phase, "sha": hb.SHA,
			"elapsed": hb.Elapsed().Round(time.Second).String(),
		})
	d.clearReviewerHeartbeat(rigName, rigPath)
}

// reapOrphanReviewerSession kills a live reviewer session that has no
// heartbeat. `gt reviewer done` clears the heartbeat and then kills its own
// session a few seconds later, so a short window with no heartbeat is normal;
// past reviewerOrphanGrace it means that self-termination never completed, and
// the session is burning a capacity slot (pressure.go counts it) for no work.
func (d *Daemon) reapOrphanReviewerSession(rigName, sessionName string) {
	created, err := d.tmux.GetSessionCreatedTime(sessionName)
	// Without a creation time we cannot tell a just-started session from an
	// abandoned one. Killing on no evidence risks reaping a reviewer mid-spawn,
	// so skip: a later tick will catch it once the time is readable.
	if err != nil || created.IsZero() {
		return
	}
	age := time.Since(created)
	if age < reviewerOrphanGrace {
		return
	}
	d.logger.Printf("Reviewer reaper: killing orphan %s reviewer session (no heartbeat, up %s) — "+
		"self-termination did not complete", rigName, age.Round(time.Second))
	if err := d.tmux.KillSessionWithProcesses(sessionName); err != nil {
		d.logger.Printf("Reviewer reaper: killing orphan session %s: %v", sessionName, err)
		return
	}
	_ = events.LogFeed(events.TypeKill, rigName+"/"+constants.RoleReviewer,
		map[string]interface{}{
			"rig": rigName, "role": constants.RoleReviewer, "reason": "orphan_no_heartbeat",
			"session_age": age.Round(time.Second).String(),
		})
}

// clearReviewerHeartbeat removes a rig's reviewer heartbeat, logging failures.
func (d *Daemon) clearReviewerHeartbeat(rigName, rigPath string) {
	if err := reviewer.ClearHeartbeat(rigPath); err != nil {
		d.logger.Printf("Reviewer reaper: clearing %s heartbeat: %v", rigName, err)
	}
}
