package reviewer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrReviewInFlight means a dispatch was skipped because the heartbeat already
// describes a different, unfinished review. Not a failure: the request is still
// mailed and drained from the queue; only the telemetry seed is deferred.
var ErrReviewInFlight = errors.New("a different review is already in flight")

// Review phases, in execution order. These are the observable checkpoints of
// the review protocol — each is a deterministic `gt reviewer` subcommand the
// role template already requires, so recording them costs the agent nothing.
const (
	// PhaseDispatched is written by `gt reviewer request` on the dispatcher's
	// side, before the session exists. A reviewer that never starts still leaves
	// a heartbeat, so "dispatched but never ran" is observable rather than silent.
	PhaseDispatched = "dispatched"
	// PhaseCheckout is written by `gt reviewer checkout`.
	PhaseCheckout = "checkout"
	// PhasePrompt is written by `gt reviewer prompt`, once per perspective. The
	// long, unobserved subagent passes run immediately after the last one — so a
	// heartbeat parked at "prompt" is the expected shape of a review in flight,
	// not by itself evidence of a wedge. See Age vs Elapsed below.
	PhasePrompt = "prompt"
	// PhaseConsolidate is written by `gt reviewer consolidate`.
	PhaseConsolidate = "consolidate"
	// PhasePost is written by `gt reviewer post`.
	PhasePost = "post"
)

// heartbeatFile is the reviewer's liveness file, kept in <rig>/reviewer/ —
// deliberately OUTSIDE the <rig>/reviewer/rig/ worktree so it never appears in
// `git status` and can never be swept by a detached checkout.
const heartbeatFile = "heartbeat.json"

// Heartbeat is the Reviewer's liveness and progress telemetry.
//
// This is NOT lifecycle state: nothing reads it to decide what the Reviewer
// should do next, and deleting it never changes review behavior. The tmux
// session remains the source of truth for "is there a reviewer", and mail
// remains the work queue. This file exists so that supervisors (the daemon
// reaper, `gt reviewer status`, an operator) can answer "is it progressing,
// and for how long" without attaching to the session. See
// docs/design/reviewer-role.md § "State vs telemetry".
type Heartbeat struct {
	// Timestamp is when this phase was entered. Its age is the progress signal.
	Timestamp time.Time `json:"timestamp"`

	// StartedAt is when the review was dispatched. Preserved across every phase
	// touch, so Elapsed() measures total review wall time. TouchDispatch sets it;
	// TouchCheckout also does, but only when starting a review the dispatcher
	// could not seed, and see Runtime for why that reset cannot move a cap.
	//
	// This is a self-reported lower bound, NOT a tamper-proof one: a process that
	// deletes or corrupts this file gets a fresh clock on the next touch, and
	// nothing here can prevent that. A supervisor that needs an unforgeable
	// runtime bound must anchor it on something the reviewed process does not own
	// — the tmux session's start time — and use Elapsed() only as a lower bound.
	StartedAt time.Time `json:"started_at"`

	// Phase is one of the Phase* constants — where the review has reached.
	Phase string `json:"phase"`

	// PR, Round, and SHA identify the review under way, so an operator seeing a
	// stalled reviewer knows what it was reviewing without reading its mailbox.
	PR    int    `json:"pr,omitempty"`
	Round int    `json:"round,omitempty"`
	SHA   string `json:"sha,omitempty"`

	// DispatchedAt is set ONLY by TouchDispatch, which runs on the dispatcher's
	// side, outside the reviewer session. Its presence is what separates a record
	// somebody actually asked for from one the reviewer wrote about itself.
	//
	// That distinction is load-bearing, not descriptive. Two rules key on it:
	// TouchDispatch defers to an existing record only if that record was
	// dispatcher-seeded, and ClearHeartbeatFor will always clear one that was
	// not. Without them, a single in-session `gt reviewer checkout <any-real-pr>`
	// planted an identity that made every later dispatch for the rig return
	// ErrReviewInFlight — reopening the "dispatched into the void" blind spot for
	// the whole rig — while `done --pr` refused to clear it because the PR did
	// not match. One command, permanent, unrecoverable by any documented step.
	//
	// It is not a security boundary: the file is rig-writable, so a determined
	// process can set this field too. It removes the ACCIDENT and the one-command
	// version of the attack, which is what a plain-file telemetry record can
	// honestly offer.
	DispatchedAt time.Time `json:"dispatched_at,omitempty"`

	// Origin is who asked for this review ("refinery" or "crew"), and Requester
	// is their mail address. Recorded so a supervisor that kills the reviewer can
	// tell the waiting party — otherwise a refinery-origin review is only
	// rescued by await-review's 30m timeout, and a crew-origin one is never
	// rescued at all, because no timeout covers that path.
	Origin    string `json:"origin,omitempty"`
	Requester string `json:"requester,omitempty"`
}

// dispatcherSeeded reports whether a record was written by TouchDispatch.
func (hb *Heartbeat) dispatcherSeeded() bool {
	return hb != nil && !hb.DispatchedAt.IsZero()
}

// HeartbeatPath returns the reviewer heartbeat path for a rig.
func HeartbeatPath(rigPath string) string {
	return filepath.Join(rigPath, "reviewer", heartbeatFile)
}

// Age returns how long the Reviewer has been in its current phase. A nil
// heartbeat reports a very large age so callers treat "absent" as "not
// progressing" without a special case.
func (hb *Heartbeat) Age() time.Duration {
	if hb == nil {
		return 365 * 24 * time.Hour
	}
	return time.Since(hb.Timestamp)
}

// Elapsed returns total review wall time since dispatch. Zero when unknown
// (nil heartbeat or unset StartedAt) so callers can distinguish "no data" from
// "just started" — a zero Elapsed must never trip an absolute-runtime cap.
func (hb *Heartbeat) Elapsed() time.Duration {
	if hb == nil || hb.StartedAt.IsZero() {
		return 0
	}
	return time.Since(hb.StartedAt)
}

// ReadHeartbeatE loads the reviewer heartbeat, distinguishing "absent" (nil,
// nil) from "unreadable or malformed" (nil, err).
//
// Supervisors need that distinction: a transient I/O error or a torn read is
// not evidence that a reviewer is idle, and conflating them hands the harshest
// available action to a momentary filesystem hiccup. ReadHeartbeat keeps the
// lenient nil-on-anything behavior for callers that only want a best-effort
// progress signal.
func ReadHeartbeatE(rigPath string) (*Heartbeat, error) {
	data, err := os.ReadFile(HeartbeatPath(rigPath)) //nolint:gosec // path derived from trusted rig path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var hb Heartbeat
	if err := json.Unmarshal(data, &hb); err != nil {
		return nil, fmt.Errorf("malformed heartbeat: %w", err)
	}
	return &hb, nil
}

// ReadHeartbeat loads the reviewer heartbeat for a rig. Returns nil when the
// file is absent, unreadable, or malformed — every caller is a supervisor that
// already treats nil as "no progress signal", and a corrupt file must not be a
// hard error on a monitoring path.
func ReadHeartbeat(rigPath string) *Heartbeat {
	data, err := os.ReadFile(HeartbeatPath(rigPath)) //nolint:gosec // path derived from trusted rig path
	if err != nil {
		return nil
	}
	var hb Heartbeat
	if err := json.Unmarshal(data, &hb); err != nil {
		return nil
	}
	return &hb
}

// WriteHeartbeat persists a heartbeat atomically (temp + rename) so a
// concurrently-reading daemon never observes a torn write.
func WriteHeartbeat(rigPath string, hb *Heartbeat) error {
	path := HeartbeatPath(rigPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating reviewer dir: %w", err)
	}
	data, err := json.MarshalIndent(hb, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling heartbeat: %w", err)
	}
	data = append(data, '\n')

	// A FIXED temp name, not os.CreateTemp's random one, for the common path. The
	// reaper kills reviewer sessions by design, so a SIGKILL inside the write
	// window is expected — and with unique names each one would leave an orphan
	// dotfile that nothing ever sweeps, in a directory operators are told to
	// inspect. A fixed name means the next write clears the debris.
	//
	// Being predictable, it must NOT be opened with os.WriteFile: that is
	// O_CREATE|O_TRUNC, which follows a symlink planted at the temp path (turning
	// the next heartbeat write into an arbitrary-path overwrite) and ignores its
	// mode argument on an existing file (so a pre-created 0666 file permanently
	// defeats the 0600 rule). Any process in the rig can create that dotfile — the
	// trust class the state.go header describes. O_EXCL refuses both: it fails on
	// anything already at the path, symlink or not.
	//
	// But the sweep must never be able to FAIL the write, and O_EXCL made that a
	// real hazard. os.Remove cannot delete a non-empty directory, so one
	// `mkdir -p heartbeat.json.tmp/x` by any rig process froze the record at
	// whatever it last held. That is worse than lost telemetry in two directions:
	// the frozen Timestamp ages until the phase rails kill a healthy session, and
	// the frozen identity makes TouchDispatch return ErrReviewInFlight for every
	// other PR forever. Writes here are best-effort at the call site — a stderr
	// warning, never a failed command — so nothing would surface it.
	//
	// So: sweep, then fall back to a unique name if the fixed one is still
	// unusable. The fallback gives up the anti-accumulation property for exactly
	// the writes that would otherwise not happen at all, which is the right trade —
	// a stray dotfile is a cosmetic problem, a frozen heartbeat is a kill.
	tmpName := path + ".tmp"
	if err := os.Remove(tmpName); err != nil && !os.IsNotExist(err) {
		_ = os.RemoveAll(tmpName)
	}
	// 0600 matches internal/deacon/heartbeat.go and the rest of the rig's
	// metadata. The contents (PR, round, head SHA, timings) are low-value but
	// there is no reader that needs more: the reaper, `gt reviewer status`, and
	// the operator all run as this user.
	f, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path derived from trusted rig path
	if err != nil {
		var terr error
		if f, terr = os.CreateTemp(filepath.Dir(path), heartbeatFile+".tmp-"); terr != nil {
			return fmt.Errorf("creating temp heartbeat: %w", err)
		}
		tmpName = f.Name()
		if cerr := f.Chmod(0o600); cerr != nil {
			_ = f.Close()
			_ = os.Remove(tmpName)
			return fmt.Errorf("securing temp heartbeat: %w", cerr)
		}
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing temp heartbeat: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing temp heartbeat: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("installing heartbeat: %w", err)
	}
	return nil
}

// TouchHeartbeat records that the Reviewer has entered phase for the review
// already on record. It updates only Timestamp and Phase — every identity field
// (PR, round, SHA) and StartedAt are inherited unchanged.
//
// It takes no identity arguments, deliberately. In-session touches must not be
// able to change the review identity or reseed the clock: the values reaching
// these call sites come from the reviewer agent's own flags (`--pr` on
// prompt/post), and the reviewer is the entity the absolute runtime cap exists
// to constrain. Making the parameters absent rather than ignored means a future
// caller cannot reintroduce the hole by passing them through. Only TouchDispatch
// and TouchCheckout establish identity.
func TouchHeartbeat(rigPath, phase string) error {
	prev := ReadHeartbeat(rigPath)
	if prev == nil {
		// No dispatch on record. Write a PHASE-ONLY marker: zero StartedAt, and
		// zero identity. Elapsed() then reports "unknown", which never trips the
		// absolute cap, and the marker cannot masquerade as a review.
		//
		// Identity is withheld for the same reason the clock is. `gt reviewer
		// prompt --pr 999999` run with no heartbeat present used to plant PR
		// 999999 here; TouchDispatch then saw a different PR "in flight" and
		// refused to seed every subsequent real dispatch, while `done --pr <real>`
		// refused to clear it because the PR did not match. One in-session command
		// permanently reopened the dispatched-into-the-void blind spot.
		return WriteHeartbeat(rigPath, &Heartbeat{
			Timestamp: time.Now().UTC(), Phase: phase,
		})
	}
	// Copying prev wholesale inherits every identity field — including Origin and
	// Requester, which only the dispatcher supplies. Losing those would leave a
	// killed review with no one to escalate to.
	next := *prev
	next.Timestamp = time.Now().UTC()
	next.Phase = phase
	return WriteHeartbeat(rigPath, &next)
}

// TouchCheckout records that the Reviewer has begun working a review, starting
// a FRESH clock when the PR differs from what is on record.
//
// This is the one in-session touch permitted to establish identity, and it
// exists because skipping the dispatcher seed (which the wedged-session path
// does, to preserve evidence the reaper needs) otherwise hands the next review
// the previous one's frozen StartedAt — and the phase rail then reports a
// seconds-old review as stalled for its predecessor's lifetime.
//
// What this reset can and cannot buy is worth stating precisely, because an
// earlier version of this comment overstated it. StartedAt is a SELF-REPORT.
// Supervisors anchor the absolute runtime cap on the tmux session's age, which
// is owned by tmux and cannot be reset from inside the session (see Runtime), so
// resetting this field does not move the cap in either direction — it moves the
// phase clock and the operator-facing identity, which is all it is for. A
// reviewer that alternates PR numbers therefore gains nothing on the rail that
// constrains it.
//
// Round comes from the request payload rather than being dropped: this is the
// only writer of a queued review's identity, and round is the field that
// distinguishes a first review from a fix round — precisely what an operator
// investigating a stalled reviewer needs. Round 0 (not supplied) inherits the
// previous record's round rather than writing a zero, so TouchDispatch's
// identical-re-request check is not silently falsified into handing out a fresh
// budget.
func TouchCheckout(rigPath string, pr, round int, sha string) error {
	prev := ReadHeartbeat(rigPath)
	if prev != nil && (pr == 0 || prev.PR == pr) {
		// Same review (or no PR to compare): ordinary phase advance.
		return TouchHeartbeat(rigPath, PhaseCheckout)
	}
	// Round is NOT inherited from prev. Reaching this line guarantees
	// prev.PR != pr (the early return above covers every other case), so prev
	// describes a DIFFERENT review and its round belongs to that one. Copying it
	// stamped a stranger's round onto this record — worse than leaving it absent,
	// because a wrong round reads as authoritative to the operator it exists to
	// inform, and it falsified sameReview into handing an in-flight review a
	// fresh budget on an identical re-request.
	//
	// So an omitted --round records 0, meaning "unknown". The role template
	// passes it; a manual invocation that omits it loses only the round.
	now := time.Now().UTC()
	hb := &Heartbeat{
		Timestamp: now, StartedAt: now, Phase: PhaseCheckout, PR: pr, Round: round, SHA: sha,
	}
	return WriteHeartbeat(rigPath, hb)
}

// sameReview reports whether a heartbeat describes the given review. A review is
// identified by (PR, round, SHA) together: round 2 of the same PR is a NEW
// review with its own budget, not a continuation.
func (hb *Heartbeat) sameReview(pr, round int, sha string) bool {
	if hb == nil {
		return false
	}
	return hb.PR == pr && hb.Round == round && strings.EqualFold(hb.SHA, sha)
}

// TouchDispatch records a newly dispatched review at PhaseDispatched, including
// who asked for it. Called by `gt reviewer request` on the dispatcher's side.
//
// StartedAt is reseeded whenever the (PR, round, SHA) identity differs from what
// is on record. Keying on the PR alone was wrong in the dominant case: the
// refinery's await-review gives up at pr_review_timeout (30m) and re-dispatches
// the SAME PR at round 2 well before the reaper's stuck_threshold (45m) has
// cleared round 1's heartbeat — so the stale record is almost always present,
// and a fresh round-2 reviewer would be born already 40 minutes into its budget
// and killed on the reaper's first cycle.
//
// A dispatch for a DIFFERENT PR while one is still on record does not overwrite
// it: one file cannot represent the mail queue the design sanctions, and the
// in-flight review's telemetry is what supervisors need. The queued request
// establishes its own record when the reviewer reaches it.
func TouchDispatch(rigPath string, pr, round int, sha, origin, requester string) error {
	prev := ReadHeartbeat(rigPath)
	// A DIFFERENT PR is a queued request; leave the in-flight record alone. A new
	// round or SHA of the SAME PR is a re-review that supersedes the round on
	// record — the reviewer works one PR at a time, so there is nothing to
	// preserve, and this is the path that must reset the clock.
	//
	// Deferring requires a record this function can corroborate, and only
	// DispatchedAt corroborates it. A record without it was written from inside
	// the reviewer session — a phase-only marker, or an in-session `gt reviewer
	// checkout <pr>` — and deferring to one let a single command block every
	// future seed for the rig with nothing able to clear it. Overwrite instead.
	//
	// Keying on StartedAt was the earlier attempt and was not enough:
	// TouchCheckout sets a non-zero StartedAt too, so a planted checkout record
	// was indistinguishable from a real dispatch.
	if prev.dispatcherSeeded() && prev.PR != 0 && pr != 0 && prev.PR != pr {
		return ErrReviewInFlight
	}
	now := time.Now().UTC()
	hb := &Heartbeat{
		Timestamp:    now,
		StartedAt:    now,
		DispatchedAt: now,
		Phase:        PhaseDispatched,
		PR:           pr, Round: round, SHA: sha,
		Origin: origin, Requester: requester,
	}
	// Only an IDENTICAL re-dispatch — same PR, round, and SHA — keeps the existing
	// clock. That is the idempotent-retry case, where handing out a fresh budget
	// would make the absolute cap unreachable by simply retrying. Any change in
	// (pr, round, sha) is a new review and starts a new clock.
	if prev.sameReview(pr, round, sha) && !prev.StartedAt.IsZero() {
		hb.StartedAt = prev.StartedAt
	}
	return WriteHeartbeat(rigPath, hb)
}

// ClearHeartbeat removes the heartbeat, marking the Reviewer idle. A missing
// file is success — clearing is idempotent so a double `done`, or a clear racing
// the reaper's own kill, is not an error.
func ClearHeartbeat(rigPath string) error {
	if err := os.Remove(HeartbeatPath(rigPath)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ClearHeartbeatFor removes the heartbeat only if it describes review pr,
// reporting whether it did. Used by `gt reviewer done`.
//
// An unconditional clear destroys a queued review's dispatch record: the
// reviewer finishes PR 100, removes the file, and PR 200 — dispatched while 100
// was running — becomes invisible, which is exactly the "dispatched into the
// void" blind spot the dispatcher seed exists to close. pr <= 0 means the caller
// could not determine which review it finished and falls back to an
// unconditional clear.
func ClearHeartbeatFor(rigPath string, pr int) (bool, error) {
	if pr <= 0 {
		return true, ClearHeartbeat(rigPath)
	}
	// ReadHeartbeatE, not the lenient reader. ReadHeartbeat returns nil for a
	// file that is present but malformed — the very distinction this package added
	// ReadHeartbeatE to preserve — and a nil here skipped the mismatch check and
	// deleted the file. So a torn read (no attacker required, just a concurrent
	// rename) let `done --pr 176` destroy PR 200's dispatch record and report a
	// clean finish, which is exactly the outcome this function exists to prevent.
	hb, err := ReadHeartbeatE(rigPath)
	if err != nil {
		return false, err
	}
	// A record no dispatcher seeded is always clearable, whatever PR it names.
	// Otherwise an in-session touch could name a PR nobody asked for and no
	// documented command could remove it — `done --pr <real>` refuses on the
	// mismatch, and `done` with no --pr is no longer the form the role template
	// prescribes.
	if hb != nil && hb.dispatcherSeeded() && hb.PR != 0 && hb.PR != pr {
		return false, nil
	}
	return true, ClearHeartbeat(rigPath)
}
