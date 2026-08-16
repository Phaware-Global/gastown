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

	// A FIXED temp name, not os.CreateTemp's random one. The reaper kills reviewer
	// sessions by design, so a SIGKILL inside the write window is expected — and
	// with unique names each one would leave an orphan dotfile that nothing ever
	// sweeps, in a directory operators are now told to inspect. A fixed name means
	// the next write simply overwrites the debris.
	tmpName := path + ".tmp"
	// A fixed name is predictable, so it must NOT be opened with os.WriteFile:
	// that is O_CREATE|O_TRUNC, which follows a symlink planted at the temp path
	// (turning the next heartbeat write into an arbitrary-path overwrite) and
	// ignores its mode argument on an existing file (so a pre-created 0666 file
	// permanently defeats the 0600 rule below). Any process in the rig can
	// create that dotfile — the same trust class the state.go header describes.
	//
	// O_EXCL refuses both: it fails on anything already at the path, symlink or
	// not. Sweeping a stale tmp first with os.Remove keeps the anti-accumulation
	// property the fixed name was chosen for — a SIGKILL mid-write leaves one
	// file, and the next write clears it.
	if err := os.Remove(tmpName); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale temp heartbeat: %w", err)
	}
	// 0600 matches internal/deacon/heartbeat.go and the rest of the rig's
	// metadata. The contents (PR, round, head SHA, timings) are low-value but
	// there is no reader that needs more: the reaper, `gt reviewer status`, and
	// the operator all run as this user.
	f, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path derived from trusted rig path
	if err != nil {
		return fmt.Errorf("creating temp heartbeat: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing temp heartbeat: %w", err)
	}
	if err := f.Close(); err != nil {
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
	if round == 0 && prev != nil {
		round = prev.Round
	}
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
// A dispatch for a DIFFERENT review while one is still on record does not
// overwrite it: one file cannot represent the mail queue the design sanctions,
// and the in-flight review's telemetry is what supervisors need. The queued
// request establishes its own record when the reviewer reaches it.
func TouchDispatch(rigPath string, pr, round int, sha string) error {
	prev := ReadHeartbeat(rigPath)
	// A DIFFERENT PR is a queued request; leave the in-flight record alone. A new
	// round or SHA of the SAME PR is a re-review that supersedes the round on
	// record — the reviewer works one PR at a time, so there is nothing to
	// preserve, and this is the path that must reset the clock.
	//
	// Deferring requires a record this function can corroborate. A zero StartedAt
	// means no dispatcher ever seeded it, so it is a phase-only marker rather
	// than a review in flight — deferring to one would let a record that carries
	// no clock block every future seed for the rig, with no command able to clear
	// it. Overwrite instead.
	if prev != nil && !prev.StartedAt.IsZero() && prev.PR != 0 && pr != 0 && prev.PR != pr {
		return ErrReviewInFlight
	}
	hb := &Heartbeat{
		Timestamp: time.Now().UTC(),
		StartedAt: time.Now().UTC(),
		Phase:     PhaseDispatched,
		PR:        pr, Round: round, SHA: sha,
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
	hb := ReadHeartbeat(rigPath)
	if hb != nil && hb.PR != 0 && hb.PR != pr {
		return false, nil
	}
	return true, ClearHeartbeat(rigPath)
}
