package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/reviewer"
)

// Daemon-side reaper knobs. Everything about what the heartbeat MEANS —
// thresholds, grace windows, sanitizers, the state machine — lives in
// internal/reviewer so the daemon and `gt reviewer status` cannot drift.
const (
	// reviewerNudgeCooldown rate-limits stuck nudges so a wedged reviewer is not
	// nudged on every heartbeat tick.
	reviewerNudgeCooldown = 10 * time.Minute

	// reviewerKillCooldown is the minimum gap between kills of the same rig's
	// reviewer, honoring reviewer.toml's kill_cooldown intent. Without it a
	// forged heartbeat drives a kill every tick — an unbounded denial of service
	// against a role that respawns on demand.
	reviewerKillCooldown = 5 * time.Minute
	// reviewerEscalateCooldown rate-limits failure escalations per rig. The
	// died-mid-review path fires on every tick for as long as the record
	// persists, so without this a rig-writable heartbeat is a daemon-authored
	// mail + nudge amplifier aimed at the refinery.
	reviewerEscalateCooldown = 30 * time.Minute

	// reviewerEscalateTimeout bounds the mail send, which shells out to bd and
	// can block on Dolt. This runs inline on the heartbeat path, so an unbounded
	// call stalls every later recovery phase behind it.
	reviewerEscalateTimeout = 20 * time.Second
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

// reviewerStuckThreshold resolves a rig's reviewer stuck threshold, clamped.
//
// Delegates to reviewer.StuckThreshold so the reaper's kill decision and the
// dispatcher's wedge-recycle decision read the same number from the same place;
// two copies of this rule would eventually disagree, and the failure mode is a
// session the dispatcher considers fine and the reaper kills. The clamp matters
// because reviewer.toml lives INSIDE the rig and is agent-writable: unclamped,
// a one-second threshold kills every reviewer on sight and a one-year threshold
// disables the reaper.
func (d *Daemon) reviewerStuckThreshold(rigName, rigPath string) time.Duration {
	raw := reviewer.StuckThreshold(d.config.TownRoot, rigPath)
	clamped, adjusted := reviewer.ClampStuckThreshold(raw)
	if adjusted {
		d.logger.Printf("Reviewer reaper: %s stuck_threshold %v is outside [%v, %v] — using %v",
			rigName, raw, reviewer.MinStuckThreshold, reviewer.MaxStuckThreshold, clamped)
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
	sessionName, serr := reviewer.ResolveSessionName(rigName)
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
		// Only accrue the missing-heartbeat grace while a session actually
		// exists. Letting the clock run across idle periods lets an attacker
		// pre-age it, so `rm heartbeat.json` becomes a zero-grace kill on the
		// NEXT freshly spawned reviewer — the grace is supposed to mean "missing
		// for 15m with a session up", not "15m have passed at some point".
		d.noteReviewerHeartbeatPresent(rigName, !alive)
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
		// An unusable timestamp (zero or future) on a record with NO session is
		// not a reason to hold back — it cannot be a live dispatch, and leaving it
		// is worse than clearing it: TouchDispatch refuses whenever prev.PR
		// differs, so a phantom record permanently locks out every later
		// dispatch's telemetry seed. Only a trustworthy, in-grace age defers.
		if age, ok := reviewer.PhaseAge(hb); ok && age < reviewer.SpawnGrace {
			return
		}
		age, ageOK := reviewer.PhaseAge(hb)
		reason, detail := "died_mid_review", fmt.Sprintf("no progress for %s", age.Round(time.Second))
		if !ageOK {
			// Distinguish the case this branch was widened to handle. Reporting
			// "no progress for 0s" for a record that has no usable timestamp at
			// all describes the opposite of what happened, and elapsed=0s in the
			// feed reads as "just started".
			reason, detail = "unusable_heartbeat", "heartbeat has no usable timestamp"
		}
		d.logger.Printf("Reviewer reaper: %s reviewer has no session (phase=%s pr=%d, %s) — clearing stale heartbeat",
			rigName, reviewer.SafePhase(hb.Phase), reviewer.SafePR(hb.PR), detail)
		_ = events.LogFeed(events.TypeSessionDeath, rigName+"/"+constants.RoleReviewer,
			map[string]interface{}{
				"rig": rigName, "role": constants.RoleReviewer, "reason": reason,
				"phase": reviewer.SafePhase(hb.Phase), "pr": reviewer.SafePR(hb.PR),
				"elapsed": reviewer.SafeText(hb.Elapsed().Round(time.Second).String()),
				"detail":  reviewer.SafeText(detail),
			})
		d.escalateReviewerFailure(rigName, hb, "reviewer session died mid-review")
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
//
// nudged reports whether this reviewer has already been nudged for a cap breach.
// It is what makes the courtesy nudge below terminate: without it, a reviewer
// looping fast enough to keep its phase fresh would be nudged forever and the
// absolute cap — the one rail such a loop cannot evade — would be unreachable.
func decideReviewerAction(hb *reviewer.Heartbeat, stuck, sessionAge time.Duration, nudged bool) (reviewerAction, string) {
	if hb == nil || stuck <= 0 {
		return reviewerActionNone, ""
	}
	// The runtime the cap acts on is corroborated against tmux, which the
	// reviewed process does not own — see reviewerRuntime. Negative inputs (a
	// future-dated file) collapse to 0 = unknown rather than reading as healthy.
	runtime := reviewer.Runtime(hb.Elapsed(), sessionAge)

	// Zero runtime means BOTH signals are unknown. Unknown must never kill.
	if capDur := stuck * reviewer.AbsoluteCapMultiple; runtime > 0 && runtime >= capDur {
		// One courtesy nudge before killing. Runtime prefers the SESSION clock,
		// which is unforgeable but deliberately over-estimates a later round's own
		// wall time, because rounds share a session. Acting on it alone would kill
		// a minutes-old round-N review for its predecessor's lifetime, so a review
		// still visibly advancing its phase gets one chance to wrap up.
		//
		// Exactly one: `nudged` makes the tier terminate. A reviewer looping fast
		// enough to keep its phase fresh is the case the cap exists for, and an
		// unconditional nudge tier would make it immortal.
		if age, ok := reviewer.PhaseAge(hb); ok && age < stuck && !nudged {
			return reviewerActionNudge, fmt.Sprintf(
				"past the absolute runtime cap (%s of %s) but still progressing — nudging once before killing",
				runtime.Round(time.Second), capDur)
		}
		return reviewerActionKill, fmt.Sprintf("exceeded absolute runtime cap (%s of %s)",
			runtime.Round(time.Second), capDur)
	}

	// A future-dated timestamp yields a negative age, which under a naive
	// comparison reads as infinitely fresh and makes a wedged reviewer immortal.
	// Treat it as no progress signal; the runtime rail above still applies.
	age, ok := reviewer.PhaseAge(hb)
	if !ok {
		// The safe direction, but not a silent one: this is also what a forged
		// future timestamp buys, and the runtime rail is then all that remains.
		return reviewerActionNone, "phase timestamp is unusable — progress rails disabled"
	}
	switch {
	case age >= stuck*reviewer.StuckMultiple:
		return reviewerActionKill, fmt.Sprintf("no progress for %s (phase=%s, %dx the %s threshold)",
			age.Round(time.Second), reviewer.SafePhase(hb.Phase), reviewer.StuckMultiple, stuck)
	case age >= stuck:
		return reviewerActionNudge, fmt.Sprintf("no progress for %s (phase=%s)",
			age.Round(time.Second), reviewer.SafePhase(hb.Phase))
	}
	return reviewerActionNone, ""
}

// enforceReviewerProgress applies decideReviewerAction to a live reviewer.
func (d *Daemon) enforceReviewerProgress(rigName, sessionName, rigPath string, hb *reviewer.Heartbeat) {
	stuck := d.reviewerStuckThreshold(rigName, rigPath)
	action, reason := decideReviewerAction(hb, stuck, d.reviewerSessionAge(sessionName), d.reviewerWasNudged(rigName))
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
		age.Round(time.Second), reviewer.SafePhase(hb.Phase), reviewer.SafePR(hb.PR))
	if err := d.tmux.NudgeSession(sessionName, msg); err != nil {
		d.logger.Printf("Reviewer reaper: nudging %s: %v", sessionName, err)
		return
	}
	d.logger.Printf("Reviewer reaper: nudged %s reviewer (stalled %s at phase=%s, threshold %s)",
		rigName, age.Round(time.Second), reviewer.SafePhase(hb.Phase), stuck)
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

// reviewerWasNudged reports whether this rig's reviewer has been nudged recently
// enough that the nudge should count as already spent. Unlike shouldNudgeReviewer
// it records nothing — a read of the same bookkeeping, so the cap's courtesy
// nudge is granted once and then withdrawn.
//
// The window is deliberately wider than the nudge cooldown: a nudge whose
// cooldown has merely expired was still delivered and still ignored, and
// re-granting the courtesy on that basis would restore the immortality the
// single-nudge rule exists to prevent.
func (d *Daemon) reviewerWasNudged(rigName string) bool {
	d.reviewerNudgeMu.Lock()
	defer d.reviewerNudgeMu.Unlock()
	last, ok := d.reviewerLastNudge[rigName]
	return ok && time.Since(last) < reviewerNudgeCooldown*3
}

// shouldEscalateReviewer rate-limits failure escalations to one per rig per
// cooldown, mirroring the nudge and kill cooldowns.
func (d *Daemon) shouldEscalateReviewer(rigName string) bool {
	d.reviewerNudgeMu.Lock()
	defer d.reviewerNudgeMu.Unlock()
	if d.reviewerLastEscalate == nil {
		d.reviewerLastEscalate = make(map[string]time.Time)
	}
	if last, ok := d.reviewerLastEscalate[rigName]; ok && time.Since(last) < reviewerEscalateCooldown {
		return false
	}
	d.reviewerLastEscalate[rigName] = time.Now()
	return true
}

// safeRound clamps a heartbeat round number. It was the one heartbeat-sourced
// field reaching the mail body, the subject, and the recipient's nudge without
// a sanitizer.
func safeRound(round int) int {
	if round < 0 || round > 1000 {
		return 0
	}
	return round
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
	// Compare IDENTITY, not Timestamp. The decision is about which review is
	// running, and the timestamp changes on every phase touch — so comparing it
	// handed any process writing heartbeat.json in a loop a permanent veto over
	// its own kill, which is exactly the runaway this rail exists to stop.
	if current, cerr := reviewer.ReadHeartbeatE(rigPath); cerr != nil || current == nil ||
		current.PR != hb.PR || !strings.EqualFold(current.SHA, hb.SHA) {
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
		rigName, reviewer.SafeText(reason), reviewer.SafePR(hb.PR), reviewer.SafePhase(hb.Phase), reviewer.SafeSHA(hb.SHA))

	if err := d.tmux.KillSessionWithProcesses(sessionName); err != nil {
		d.logger.Printf("Reviewer reaper: killing session %s: %v", sessionName, err)
		// Fall through: still clear the heartbeat. Leaving it would make the rig
		// look stalled forever while re-attempting a kill that is failing anyway.
	}
	d.escalateReviewerFailure(rigName, hb, reason)
	_ = events.LogFeed(events.TypeKill, rigName+"/"+constants.RoleReviewer,
		map[string]interface{}{
			"rig": rigName, "role": constants.RoleReviewer, "reason": reviewer.SafeText(reason),
			"pr": reviewer.SafePR(hb.PR), "phase": reviewer.SafePhase(hb.Phase), "sha": reviewer.SafeSHA(hb.SHA),
			"elapsed": reviewer.SafeText(hb.Elapsed().Round(time.Second).String()),
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
	if !seen || missingFor < reviewer.MissingGrace {
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

// escalateReviewerFailure tells whoever asked for the review that it will never
// arrive.
//
// Without this the two origins fail very differently, and both badly. A
// refinery-origin review is only rescued by await-review's 30m timeout — the
// refinery sits waiting on a session the daemon already killed, learning
// nothing about why. A crew-origin review is never rescued at all: that timeout
// lives INSIDE the refinery's await-review step, so nothing covers the crew
// path and the requesting crew member waits forever.
//
// Best-effort: a mail failure never prevents the kill from completing.
func (d *Daemon) escalateReviewerFailure(rigName string, hb *reviewer.Heartbeat, reason string) {
	if hb == nil {
		return
	}
	// Rate-limit. This is the one heartbeat-driven action here that used to have
	// no cooldown, which made a rig-writable file a per-tick daemon-authored mail
	// AND nudge amplifier aimed at the refinery — the died-mid-review path fires
	// on every tick for as long as the record persists.
	if !d.shouldEscalateReviewer(rigName) {
		return
	}
	// The requester is read from the rig-writable heartbeat, so it is untrusted
	// input being used as a MAIL ADDRESS. Unvalidated, a forged value redirects
	// the failure notice to an arbitrary mailbox — the escalation becomes a
	// delivery primitive for whoever can write the file. Only the two addresses
	// `gt reviewer request` actually writes are accepted.
	to, ok := validReviewerRequester(rigName, hb.Requester)
	if !ok {
		// The feed event is still emitted by the caller, so the kill is never
		// invisible — it just isn't routed.
		d.logger.Printf("Reviewer reaper: %s has no valid requester (%q) — kill logged to the feed but not escalated",
			rigName, reviewer.SafeText(hb.Requester))
		return
	}

	// Round is heartbeat-sourced like every other field and was the one reaching
	// the body, the subject, and the recipient's nudge without a sanitizer.
	round := safeRound(hb.Round)
	pr, sha := reviewer.SafePR(hb.PR), reviewer.SafeSHA(hb.SHA)

	// The remedy line is only actionable when we have a usable PR and SHA; a
	// bogus `gt reviewer request 0 --sha unknown` is worse than no suggestion.
	remedy := "Re-dispatch the review if it is still wanted."
	if pr > 0 && sha != "unknown" {
		remedy = fmt.Sprintf("Re-dispatch with `gt reviewer request %d --sha %s` if the review is still wanted.", pr, sha)
	}
	body := fmt.Sprintf(
		"REVIEW_FAILED\nrig: %s\npr: %d\nround: %d\nsha: %s\nphase: %s\nelapsed: %s\nreason: %s\n\n"+
			"The reviewer session was terminated by the daemon.\n"+
			// Deliberately not "no review will arrive": the reviewer may have posted
			// before it died, and asserting otherwise sends the reader to re-request
			// a review that already exists.
			"Any review for this request that has not already been posted will not arrive.\n%s\n",
		rigName, pr, round, sha,
		reviewer.SafePhase(hb.Phase), hb.Elapsed().Round(time.Second),
		reviewer.SafeText(reason), remedy)

	router := mail.NewRouterWithTownRoot(d.config.TownRoot, d.config.TownRoot)
	if stores := d.BeadsStores(); len(stores) > 0 {
		router.SetStores(stores)
	}
	defer router.WaitPendingNotifications()

	msg := mail.NewMessage("daemon", to,
		fmt.Sprintf("Review failed: PR #%d (round %d)", pr, round), body)
	msg.Type = mail.TypeTask
	msg.Timestamp = time.Now()

	// Bound the send. router.Send shells out to bd and can block on Dolt, and
	// this runs inline on the daemon's heartbeat path — an unbounded call there
	// stalls every other recovery phase behind it. Escalation is best-effort;
	// a slow mailer must never hold the heartbeat hostage.
	done := make(chan error, 1)
	go func() { done <- router.Send(msg) }()
	select {
	case err := <-done:
		if err != nil {
			d.logger.Printf("Reviewer reaper: escalating %s reviewer failure to %s: %v", rigName, to, err)
			return
		}
	case <-time.After(reviewerEscalateTimeout):
		d.logger.Printf("Reviewer reaper: escalation to %s timed out after %v — kill is still recorded in the feed",
			to, reviewerEscalateTimeout)
		return
	case <-d.ctx.Done():
		return
	}
	d.logger.Printf("Reviewer reaper: escalated %s reviewer failure (PR #%d) to %s", rigName, pr, to)
}

// validReviewerRequester returns the escalation address for a heartbeat's
// recorded requester, accepting only the two values `gt reviewer request`
// writes for THIS rig: "<rig>/refinery" and "<rig>/crew".
//
// Anything else — another rig's mailbox, an arbitrary agent, a crafted string —
// is rejected rather than sanitized, because there is no partially-valid
// address worth delivering to.
func validReviewerRequester(rigName, requester string) (string, bool) {
	req := strings.TrimSpace(requester)
	// The rig's refinery — a real, polled identity.
	if req == rigName+"/"+constants.RoleRefinery {
		return req, true
	}
	// The rig's crew group. Router.sendToGroup handles this form; "<rig>/crew"
	// does NOT — that is the container directory, and delivering to it is a
	// silent dead letter that Send reports as success.
	if req == "@crew/"+rigName {
		return req, true
	}
	// A concrete crew member: "<rig>/crew/<name>", which normalizes to the
	// "<rig>/<name>" inbox identity a crew agent actually polls.
	if name, ok := strings.CutPrefix(req, rigName+"/crew/"); ok &&
		name != "" && !strings.ContainsAny(name, "/ \t") {
		return req, true
	}
	return "", false
}
