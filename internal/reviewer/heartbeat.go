package reviewer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

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
	// touch, so Elapsed() measures total review wall time — the basis for an
	// absolute cap that a still-touching-but-looping reviewer cannot evade.
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

	tmp, err := os.CreateTemp(filepath.Dir(path), ".heartbeat-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp heartbeat: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp heartbeat: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp heartbeat: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil { //nolint:gosec // operational telemetry, non-sensitive
		return fmt.Errorf("chmod temp heartbeat: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("installing heartbeat: %w", err)
	}
	return nil
}

// TouchHeartbeat records that the Reviewer has entered phase for a review.
//
// StartedAt is carried forward from any existing heartbeat so total review wall
// time survives every phase transition; it is seeded from now only on the first
// touch of a review. A PR change resets StartedAt — a session draining a second
// request from its mailbox is a new review, and inheriting the previous one's
// clock would make the reaper kill it early.
func TouchHeartbeat(rigPath, phase string, pr, round int, sha string) error {
	now := time.Now().UTC()
	hb := &Heartbeat{
		Timestamp: now,
		StartedAt: now,
		Phase:     phase,
		PR:        pr,
		Round:     round,
		SHA:       sha,
	}
	if prev := ReadHeartbeat(rigPath); prev != nil && !prev.StartedAt.IsZero() {
		// Same review (or a phase touch that didn't carry a PR) continues the
		// existing clock; a different PR starts a fresh one.
		if pr == 0 || prev.PR == 0 || prev.PR == pr {
			hb.StartedAt = prev.StartedAt
		}
		// Phase touches inside the session don't re-state PR/round/SHA — inherit
		// them so the record stays complete for whoever reads it.
		if hb.PR == 0 {
			hb.PR = prev.PR
		}
		if hb.Round == 0 {
			hb.Round = prev.Round
		}
		if hb.SHA == "" {
			hb.SHA = prev.SHA
		}
	}
	return WriteHeartbeat(rigPath, hb)
}

// ClearHeartbeat removes the heartbeat, marking the Reviewer idle. Called by
// `gt reviewer done`. A missing file is success — clearing is idempotent so a
// double `done` (or a clear racing the reaper's own kill) is not an error.
func ClearHeartbeat(rigPath string) error {
	if err := os.Remove(HeartbeatPath(rigPath)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
