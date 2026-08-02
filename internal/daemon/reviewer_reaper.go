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

	// reviewerSpawnGrace covers the window between `gt reviewer request` seeding
	// the heartbeat and the session actually existing: a Dolt-backed mail write
	// plus, on a first dispatch, a full git worktree provision. Throughout it the
	// rig is legitimately in the "heartbeat, no session" state that otherwise
	// means the reviewer died.
	reviewerSpawnGrace = 10 * time.Minute

	// reviewerKillCooldown is the minimum gap between kills of the same rig's
	// reviewer, honoring reviewer.toml's kill_cooldown intent. Without it a
	// forged heartbeat drives a kill every tick — an unbounded denial of service
	// against a role that respawns on demand.
	reviewerKillCooldown = 5 * time.Minute

	// reviewerMissingGrace is how long a live session may have NO heartbeat
	// before it is reaped, measured from first observation of the absence.
	reviewerMissingGrace = 15 * time.Minute

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
	// getPatrolRigs, not getKnownRigs: this honors the patrol's configured Rigs
	// filter and inherits the same parked/docked/unreadable fail-safe as the
	// witness and refinery patrols. With getKnownRigs a destructive, default-on
	// patrol had no working per-rig opt-out and would keep killing inside a rig
	// an operator had deliberately parked.
	d.rigPool.runPerRig(d.ctx, d.getPatrolRigs(constants.RoleReviewer), func(ctx context.Context, rigName string) error {
		d.reapRigReviewer(rigName)
		return nil
	})
}

// reviewerStuckThreshold resolves a rig's reviewer stuck threshold from its
// role definition, falling back to the compiled-in default. This is the first
// real consumer of RoleDefinition.Health — until now reviewer.toml's
// stuck_threshold was read only by `gt role def`'s printer and enforced nothing.
func (d *Daemon) reviewerStuckThreshold(rigName, rigPath string) time.Duration {
	raw := defaultReviewerStuckThreshold
	if def, err := config.LoadRoleDefinition(d.config.TownRoot, rigPath, constants.RoleReviewer); err == nil &&
		def != nil && def.Health.StuckThreshold.Duration > 0 {
		raw = def.Health.StuckThreshold.Duration
	}
	// reviewer.toml lives INSIDE the rig and is agent-writable, so an unclamped
	// threshold is a kill switch in both directions: a one-second value kills
	// every reviewer on sight, a one-year value disables the reaper.
	clamped, adjusted := clampStuckThreshold(raw)
	if adjusted {
		d.logger.Printf("Reviewer reaper: %s stuck_threshold %v is outside [%v, %v] — using %v",
			rigName, raw, minReviewerStuckThreshold, maxReviewerStuckThreshold, clamped)
	}
	return clamped
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

	// Refuse to guess the target. session.PrefixFor falls back to "gt" for any
	// rig missing beads.prefix, collapsing several rigs onto one session name —
	// so the reaper could read rig A's heartbeat and kill rig B's healthy
	// reviewer. Skipping a misconfigured rig costs coverage; guessing costs an
	// unrelated rig's running review.
	sessionName, serr := resolveReviewerSession(rigName)
	if serr != nil {
		d.logger.Printf("Reviewer reaper: skipping %s — %v", rigName, serr)
		return
	}

	alive, err := d.tmux.HasSession(sessionName)
	if err != nil {
		d.logger.Printf("Reviewer reaper: checking session %s: %v", sessionName, err)
		return
	}

	// A transient or corrupt read is NOT "no heartbeat". Conflating them gave
	// the harshest available action to a momentary I/O error.
	hb, rerr := reviewer.ReadHeartbeatE(rigPath)
	if rerr != nil {
		d.logger.Printf("Reviewer reaper: %s heartbeat unreadable (%v) — taking no action", rigName, rerr)
		return
	}

	if hb == nil {
		d.noteReviewerHeartbeatPresent(rigName, false)
		if alive {
			d.reapReviewerWithoutHeartbeat(rigName, sessionName)
		}
		return
	}
	d.noteReviewerHeartbeatPresent(rigName, true)

	if !alive {
		// AMBIGUOUS: this is both how a dead reviewer looks and how a dispatch in
		// progress looks. `gt reviewer request` seeds the heartbeat before the
		// session exists, and the work in between — a Dolt-backed mail write, plus
		// a full git worktree provision on a first dispatch — can outlast a daemon
		// tick. Acting immediately destroyed the dispatch record PhaseDispatched
		// exists to provide, and the session then came up with NO heartbeat,
		// routing a healthy just-started reviewer onto the missing-heartbeat kill
		// path. Every other branch here has a grace or a ramp; this one had none.
		age, ok := reviewerPhaseAge(hb.Age())
		if !ok {
			d.logger.Printf("Reviewer reaper: %s heartbeat timestamp is in the future — taking no action", rigName)
			return
		}
		if age < reviewerSpawnGrace {
			return
		}
		d.logger.Printf("Reviewer reaper: %s reviewer has no session (phase=%s pr=%d, no progress for %s) — clearing stale heartbeat",
			rigName, safePhase(hb.Phase), safePR(hb.PR), age.Round(time.Second))
		_ = events.LogFeed(events.TypeSessionDeath, rigName+"/"+constants.RoleReviewer,
			map[string]interface{}{
				"rig": rigName, "role": constants.RoleReviewer, "reason": "died_mid_review",
				"phase": safePhase(hb.Phase), "pr": safePR(hb.PR),
				"elapsed": hb.Elapsed().Round(time.Second).String(),
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
	// The runtime the cap acts on is corroborated against tmux, which the
	// reviewed process does not own — see reviewerRuntime. Negative inputs (a
	// future-dated file) collapse to 0 = unknown rather than reading as healthy.
	runtime := reviewerRuntime(hb.Elapsed(), sessionAge)

	// Zero runtime means BOTH signals are unknown. Unknown must never kill.
	if capDur := stuck * reviewerAbsoluteCapMultiple; runtime > 0 && runtime >= capDur {
		return reviewerActionKill, fmt.Sprintf("exceeded absolute runtime cap (%s of %s)",
			runtime.Round(time.Second), capDur)
	}

	// A future-dated timestamp yields a negative age, which under a naive
	// comparison reads as infinitely fresh and makes a wedged reviewer immortal.
	// Treat it as no progress signal; the runtime rail above still applies.
	age, ok := reviewerPhaseAge(hb.Age())
	if !ok {
		return reviewerActionNone, ""
	}
	switch {
	case age >= stuck*reviewerKillMultiple:
		return reviewerActionKill, fmt.Sprintf("no progress for %s (phase=%s, %dx the %s threshold)",
			age.Round(time.Second), safePhase(hb.Phase), reviewerKillMultiple, stuck)
	case age >= stuck:
		return reviewerActionNudge, fmt.Sprintf("no progress for %s (phase=%s)",
			age.Round(time.Second), safePhase(hb.Phase))
	}
	return reviewerActionNone, ""
}

// enforceReviewerProgress applies decideReviewerAction to a live reviewer.
func (d *Daemon) enforceReviewerProgress(rigName, sessionName, rigPath string, hb *reviewer.Heartbeat) {
	stuck := d.reviewerStuckThreshold(rigName, rigPath)
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
	// This string is delivered into the reviewer AGENT's input under a
	// daemon-authored prefix, which makes it a prompt-injection channel: the
	// phase comes from a rig-writable file whose author consumes
	// attacker-influenced PR diffs. Only an allowlisted phase and a clamped
	// integer reach it — never a raw heartbeat string.
	msg := fmt.Sprintf(
		"REVIEWER_STALLED: no progress for %s (phase=%s, PR #%d). Continue the review, "+
			"or if you cannot, post what you have via `gt reviewer post` and run `gt reviewer done`.",
		age.Round(time.Second), safePhase(hb.Phase), safePR(hb.PR))
	if err := d.tmux.NudgeSession(sessionName, msg); err != nil {
		d.logger.Printf("Reviewer reaper: nudging %s: %v", sessionName, err)
		return
	}
	d.logger.Printf("Reviewer reaper: nudged %s reviewer (stalled %s at phase=%s, threshold %s)",
		rigName, age.Round(time.Second), safePhase(hb.Phase), stuck)
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
	// Re-read under the decision. Between deciding and acting the reviewer may
	// have finished and a new review been dispatched into the same session, in
	// which case this kill would land on an innocent successor.
	if current, cerr := reviewer.ReadHeartbeatE(rigPath); cerr != nil || current == nil ||
		!current.Timestamp.Equal(hb.Timestamp) || current.PR != hb.PR {
		d.logger.Printf("Reviewer reaper: %s heartbeat changed under the kill decision — aborting", rigName)
		return
	}
	if !d.shouldKillReviewer(rigName) {
		d.logger.Printf("Reviewer reaper: %s kill suppressed by cooldown", rigName)
		return
	}

	// Every heartbeat-sourced field is sanitized: the daemon log is
	// line-oriented, so a raw value with an embedded newline can forge entries
	// that appear to come from the daemon itself.
	d.logger.Printf("Reviewer reaper: killing %s reviewer — %s (pr=%d phase=%s sha=%s)",
		rigName, safeLogField(reason), safePR(hb.PR), safePhase(hb.Phase), safeSHA(hb.SHA))

	if err := d.tmux.KillSessionWithProcesses(sessionName); err != nil {
		d.logger.Printf("Reviewer reaper: killing session %s: %v", sessionName, err)
		// Fall through: still clear the heartbeat. Leaving it would make the rig
		// look stalled forever while re-attempting a kill that is failing anyway.
	}
	_ = events.LogFeed(events.TypeKill, rigName+"/"+constants.RoleReviewer,
		map[string]interface{}{
			"rig": rigName, "role": constants.RoleReviewer, "reason": safeLogField(reason),
			"pr": safePR(hb.PR), "phase": safePhase(hb.Phase), "sha": safeSHA(hb.SHA),
			"elapsed": hb.Elapsed().Round(time.Second).String(),
		})
	d.clearReviewerHeartbeat(rigName, rigPath)
}

// noteReviewerHeartbeatPresent records whether a rig currently has a heartbeat,
// so the missing-heartbeat grace runs from the daemon's FIRST observation of the
// absence rather than from session creation.
func (d *Daemon) noteReviewerHeartbeatPresent(rigName string, present bool) {
	d.reviewerNudgeMu.Lock()
	defer d.reviewerNudgeMu.Unlock()
	if d.reviewerMissingSince == nil {
		d.reviewerMissingSince = make(map[string]time.Time)
	}
	if present {
		delete(d.reviewerMissingSince, rigName)
		return
	}
	if _, seen := d.reviewerMissingSince[rigName]; !seen {
		d.reviewerMissingSince[rigName] = time.Now()
	}
}

// reviewerMissingFor returns how long a rig has been observed without a
// heartbeat, and whether such an observation exists at all.
func (d *Daemon) reviewerMissingFor(rigName string) (time.Duration, bool) {
	d.reviewerNudgeMu.Lock()
	defer d.reviewerNudgeMu.Unlock()
	since, ok := d.reviewerMissingSince[rigName]
	if !ok {
		return 0, false
	}
	return time.Since(since), true
}

// reapReviewerWithoutHeartbeat handles a live session with no heartbeat.
//
// `gt reviewer done` clears the heartbeat and then kills its own session a few
// seconds later, so a brief window with no heartbeat is normal. Past the grace
// it means self-termination never completed — or that someone deleted the file.
//
// The grace runs from the daemon's first observation of the absence, NOT from
// session creation. Measuring from creation made `rm heartbeat.json` an instant
// kill switch for any session older than the window; measuring from first
// observation means the window must also be waited out with the daemon watching.
func (d *Daemon) reapReviewerWithoutHeartbeat(rigName, sessionName string) {
	missingFor, seen := d.reviewerMissingFor(rigName)
	if !seen || missingFor < reviewerMissingGrace {
		return
	}
	// Corroborate with something the file's author does not control. A session
	// still producing output is doing work, and killing it on the strength of a
	// deleted file alone would let any rig process terminate a working reviewer.
	if !d.tmux.IsIdle(sessionName) {
		return
	}
	if !d.shouldKillReviewer(rigName) {
		return
	}

	d.logger.Printf("Reviewer reaper: killing %s reviewer — no heartbeat for %s and session idle",
		rigName, missingFor.Round(time.Second))
	if err := d.tmux.KillSessionWithProcesses(sessionName); err != nil {
		d.logger.Printf("Reviewer reaper: killing orphan session %s: %v", sessionName, err)
		return
	}
	_ = events.LogFeed(events.TypeKill, rigName+"/"+constants.RoleReviewer,
		map[string]interface{}{
			"rig": rigName, "role": constants.RoleReviewer,
			// Named for what was OBSERVED, not for an inferred cause: the
			// heartbeat is missing, which may or may not be a failed self-exit.
			"reason":      "heartbeat_missing",
			"missing_for": missingFor.Round(time.Second).String(),
		})
}

// clearReviewerHeartbeat removes a rig's reviewer heartbeat, logging failures.
func (d *Daemon) clearReviewerHeartbeat(rigName, rigPath string) {
	if err := reviewer.ClearHeartbeat(rigPath); err != nil {
		d.logger.Printf("Reviewer reaper: clearing %s heartbeat: %v", rigName, err)
	}
}

// shouldKillReviewer enforces a per-rig kill cooldown, honoring reviewer.toml's
// kill_cooldown intent. Without it a forged heartbeat drives a kill on every
// tick — an unbounded denial of service against a role that respawns on demand.
func (d *Daemon) shouldKillReviewer(rigName string) bool {
	d.reviewerNudgeMu.Lock()
	defer d.reviewerNudgeMu.Unlock()
	if d.reviewerLastKill == nil {
		d.reviewerLastKill = make(map[string]time.Time)
	}
	if last, ok := d.reviewerLastKill[rigName]; ok && time.Since(last) < reviewerKillCooldown {
		return false
	}
	d.reviewerLastKill[rigName] = time.Now()
	return true
}
