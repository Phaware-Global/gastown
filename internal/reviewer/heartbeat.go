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
	// touch, so Elapsed() measures total review wall time. Only TouchDispatch
	// sets it, so an in-session touch cannot reseed the clock.
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
	// 0600 matches internal/deacon/heartbeat.go and the rest of the rig's
	// metadata. The contents (PR, round, head SHA, timings) are low-value but
	// there is no reader that needs more: the reaper, `gt reviewer status`, and
	// the operator all run as this user.
	if err := os.WriteFile(tmpName, data, 0o600); err != nil {
		return fmt.Errorf("writing temp heartbeat: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("installing heartbeat: %w", err)
	}
	return nil
}

// TouchHeartbeat records that the Reviewer has entered phase for the review
// already on record. It updates only Timestamp and Phase — every identity field
// (PR, round, SHA, origin, requester) and StartedAt are inherited unchanged.
//
// In-session touches deliberately CANNOT change the review identity or reseed
// the clock. The PR number reaching these call sites comes from the reviewer
// agent's own flags (`--pr` on prompt/post, the positional arg on checkout), and
// the reviewer is the entity the absolute runtime cap exists to constrain. If a
// mismatched PR reseeded StartedAt, a wedged or prompt-injected session could
// zero its own kill clock with one `gt reviewer prompt --pr 999`. Only the
// dispatcher, via TouchDispatch, establishes identity and starts the clock.
func TouchHeartbeat(rigPath, phase string, pr, round int, sha string) error {
	prev := ReadHeartbeat(rigPath)
	if prev == nil {
		// No dispatch on record. Write a phase-only marker with a ZERO StartedAt:
		// Elapsed() then reports "unknown", which never trips the absolute cap.
		// Seeding a clock here would let an in-session touch start one.
		return WriteHeartbeat(rigPath, &Heartbeat{
			Timestamp: time.Now().UTC(), Phase: phase, PR: pr, Round: round, SHA: sha,
		})
	}
	next := *prev
	next.Timestamp = time.Now().UTC()
	next.Phase = phase
	return WriteHeartbeat(rigPath, &next)
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
	if prev != nil && prev.PR != 0 && pr != 0 && prev.PR != pr {
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
