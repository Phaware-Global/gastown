package reviewer

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
)

// The reviewer heartbeat is UNTRUSTED INPUT.
//
// It is a plain file inside the rig, writable by any process there, and its
// primary author is an agent whose own input — PR diffs, commit messages — is
// attacker-influenced by construction. Two very different consumers act on it:
// the daemon reaper, which turns it into authority to SIGKILL a process tree,
// and `gt reviewer status`/`list`, which render it onto an operator's terminal.
//
// Everything both consumers need lives HERE, in one place, for two reasons:
//
//  1. They must agree. The CLI's help text promises it reports "the same four
//     states the reaper acts on". When the sanitizers and the state machine
//     lived in internal/daemon, the CLI silently kept a naive copy and the two
//     diverged — status said "working" for a reviewer the reaper was about to
//     kill, and reported a phase the reaper would have rejected.
//  2. Each rule below was independently exploitable, and a second
//     implementation is a second chance to miss one.

// shaPattern matches a plausible git object id. Anything else is rejected
// rather than truncated, so a crafted value cannot survive as a prefix.
var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// maxFieldLen bounds any heartbeat-sourced string reaching a log line or a
// terminal.
const maxFieldLen = 64

// SafePhase maps a heartbeat phase onto the known phase constants, returning
// "unknown" for anything else.
//
// An allowlist rather than an escape: the set of legitimate values is small,
// closed, and known, so there is no reason to pass through anything outside it.
// This value reaches the daemon log, the event feed, an operator's terminal,
// and — via the reaper's stall nudge — the reviewer agent's own prompt.
func SafePhase(phase string) string {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case PhaseDispatched:
		return PhaseDispatched
	case PhaseCheckout:
		return PhaseCheckout
	case PhasePrompt:
		return PhasePrompt
	case PhaseConsolidate:
		return PhaseConsolidate
	case PhasePost:
		return PhasePost
	default:
		return "unknown"
	}
}

// SafeSHA returns a validated short SHA, or "unknown".
func SafeSHA(sha string) string {
	s := strings.TrimSpace(sha)
	if !shaPattern.MatchString(s) {
		return "unknown"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// SafeText strips anything that could forge a log entry or drive a terminal,
// then truncates.
//
// Two sinks, one rule. The daemon log is line-oriented, so an embedded newline
// authors entries attributable to the daemon. A terminal interprets ESC
// sequences, so an embedded CSI rewrites the operator's screen — a forged
// heartbeat could redraw `gt reviewer status` to show a healthy reviewer, or
// erase the row that would have revealed it. Stripping the whole C0 range plus
// DEL covers both without needing to model either.
func SafeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
		// Bound BOTH branches. Checking only after a printable rune let a field
		// made entirely of control characters grow without limit — the sanitizer
		// would neutralize the content and then flood the log with the result.
		if b.Len() >= maxFieldLen {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// SafePR clamps a heartbeat PR number to a plausible range; 0 means "unknown".
func SafePR(pr int) int {
	if pr < 0 || pr > 10_000_000 {
		return 0
	}
	return pr
}

// Runtime is the wall time an absolute cap should act on.
//
// observedAge is how long THIS REVIEW has been running, measured by an observer
// on a clock the reviewed process does not own. It is not the tmux session's
// age, and the difference is the whole point of this function's second
// iteration: the session is shared by design — Manager.EnsureRunning is a no-op
// when one is up, and the design doc sanctions "one session, sequential drain" —
// so a session that has drained queued work accrues age every LATER review would
// inherit. Flooring on it made a ten-second-old review in a three-hour-old
// session instantly kill-imminent, which is the round-1 "born pre-aged" failure
// relocated from the round boundary to the session boundary. A consumer derives
// observedAge by differencing the session clock across the review it is watching
// (see internal/daemon's reviewer reaper); it resets per review, and it is still
// unforgeable because both terms come from tmux.
//
// The heartbeat's self-reported elapsed is admitted solely as a refinement
// WITHIN the window observedAge already permits — it can raise the number by at
// most SpawnGrace, the legitimate head start a dispatcher-seeded heartbeat has
// over its session, and it can never lower it below what was observed:
//
//	runtime = max(observedAge, min(elapsed, observedAge+SpawnGrace))
//
// Both bounds earn their place, and each closes a bug the other reopens:
//
//   - The min bounds FORGERY. This function once took a plain max, so a
//     `started_at` of now-1000h in a rig-writable file was on its own sufficient
//     to SIGKILL a healthy thirty-second-old reviewer's whole process tree on
//     the next tick.
//   - The max floor bounds EVASION. Deleting or zeroing the file reports elapsed
//     0, so a min alone would let a reviewer clear the cap by rewriting one
//     field. The observed clock keeps accruing underneath.
//
// A zero observedAge means the observer has no clock for this review, NOT that
// the review is new, and this returns 0 — "unknown" — rather than falling back
// to the self-report. Falling back was the last path by which a rig-writable
// file alone could authorize a kill, and it bought little: an observer with no
// clock generally cannot carry out the kill either, while the phase rails,
// bounded separately by PhaseAge, still apply.
//
// What this deliberately does NOT defend against is a reviewer that rewrites its
// own (pr, round, sha) to make the observer start a fresh review clock. That
// costs the attacker the identity an operator sees and the dispatcher's ability
// to match it, and it is bounded by the phase rails, which no identity rewrite
// refreshes. Trying to cover it here is what produced the shared-session floor.
func Runtime(elapsed, observedAge time.Duration) time.Duration {
	if observedAge <= 0 {
		return 0
	}
	claimed := elapsed
	if claimed > observedAge+SpawnGrace {
		claimed = observedAge + SpawnGrace
	}
	if claimed > observedAge {
		return claimed
	}
	return observedAge
}

// PhaseAge validates a heartbeat's progress signal, reporting ok=false when it
// cannot be trusted.
//
// Rejects both directions of nonsense. A future-dated timestamp yields a
// negative age which reads as infinitely fresh, making a wedged reviewer
// immortal. A zero timestamp yields ~2562047h, which renders as a garbage
// duration on an operator's screen and would trip every threshold at once.
func PhaseAge(hb *Heartbeat) (time.Duration, bool) {
	if hb == nil || hb.Timestamp.IsZero() {
		return 0, false
	}
	age := time.Since(hb.Timestamp)
	if age < 0 {
		return 0, false
	}
	return age, true
}

// ResolveSessionName returns the tmux session name for a rig's reviewer,
// refusing to guess.
//
// session.PrefixFor falls back to DefaultPrefix ("gt") for any rig missing a
// beads.prefix in rigs.json, so several rigs can collapse onto the single
// session name "gt-reviewer". A consumer would then read rig A's heartbeat and
// act on rig B's session — for the reaper that is a cross-rig SIGKILL, and for
// `gt reviewer stop` it is an operator killing a rig they did not name.
//
// The refusal is deliberately asymmetric, and getting that wrong has now failed
// in both directions:
//
//   - Resolving collisions from the registry alone missed rigs ABSENT from
//     rigs.json. They are invisible in the prefix map yet still resolve to
//     "gt-reviewer", so the refusal fired on the misconfigured rig and not on
//     the correctly-configured `beads.prefix = "gt"` rig sharing its name.
//   - Treating every peer as a collision partner then over-corrected: because an
//     unregistered peer resolves to the fallback, ONE rig added but not yet in
//     rigs.json — the ordinary state right after `gt rig add` — permanently
//     switched off reviewer supervision for gastown, the rig this feature exists
//     for.
//
// So a peer only vetoes when it has an EXPLICIT registry entry naming the same
// prefix. An unconfigured peer is a configuration defect on the peer: it cannot
// be safely targeted itself (the check below refuses it when it is the subject),
// but it must not disqualify a rig that did configure itself correctly.
func ResolveSessionName(rigName string, peers []string) (string, error) {
	prefix := session.PrefixFor(rigName)
	if prefix == "" {
		return "", fmt.Errorf("rig %q has no session prefix", rigName)
	}
	registered := session.DefaultRegistry().AllRigs()
	// The subject itself must be explicitly configured. A rig relying on the
	// fallback shares a namespace with every other such rig, so there is no
	// name that unambiguously belongs to it.
	if prefix == session.DefaultPrefix && registered[rigName] != session.DefaultPrefix {
		return "", fmt.Errorf("rig %q has no beads.prefix and would collapse onto the shared %q session namespace",
			rigName, session.DefaultPrefix)
	}
	name := session.ReviewerSessionName(prefix)
	seen := make(map[string]bool, len(peers)+len(registered))
	for _, p := range peers {
		seen[p] = true
	}
	for r := range registered {
		seen[r] = true
	}
	for other := range seen {
		if other == rigName {
			continue
		}
		// Explicitly configured peers only. An unregistered peer resolves to the
		// fallback prefix, which would collide with every DefaultPrefix rig and
		// veto them all.
		op, ok := registered[other]
		if !ok || op == "" {
			continue
		}
		if session.ReviewerSessionName(op) == name {
			return "", fmt.Errorf("rig %q shares reviewer session %q with rig %q — target is ambiguous",
				rigName, name, other)
		}
	}
	return name, nil
}

// Grace windows. These are shared so the CLI never reports a state the reaper
// would not act on, and vice versa.
const (
	// SpawnGrace covers the window between `gt reviewer request` seeding the
	// heartbeat and the session existing: a Dolt-backed mail write plus, on a
	// first dispatch, a full git worktree provision. Throughout it the rig is
	// legitimately in the "heartbeat, no session" state that otherwise means the
	// reviewer died.
	SpawnGrace = 10 * time.Minute

	// MissingGrace is how long a live session may have NO heartbeat before it
	// counts as abandoned. `gt reviewer done` clears the heartbeat and kills its
	// own session a few seconds later, so a brief gap is normal.
	MissingGrace = 15 * time.Minute
)

// State is a reviewer's observed condition, derived from the heartbeat, the
// session, and the clock. Both the reaper and the CLI classify with this, so
// the daemon log and `gt reviewer status` describe reality identically.
type State int

const (
	// StateIdle: no heartbeat, no session. A healthy rig with no review.
	StateIdle State = iota
	// StateUnreadable: the heartbeat exists but could not be parsed. NOT idle —
	// a transient I/O error must never earn the harshest available action.
	StateUnreadable
	// StateSpawning: dispatched, session not up yet, inside SpawnGrace.
	StateSpawning
	// StateDied: heartbeat outlived its session past SpawnGrace.
	StateDied
	// StateStarting: session up, no heartbeat, inside MissingGrace.
	StateStarting
	// StateAbandoned: session up, no heartbeat, past MissingGrace.
	StateAbandoned
	// StateWorking: session up, heartbeat progressing.
	StateWorking
	// StateUnanchored: session up and the phase clock looks healthy, but there is
	// no session clock, so the absolute runtime cap cannot be enforced at all.
	// Distinguished from StateWorking because it is the shape of a consumer that
	// forgot to populate SessionAge as much as it is a failing tmux query, and in
	// either case the rail a looping reviewer cannot evade is switched off.
	StateUnanchored
	// StateStalled: past the stuck threshold — the reaper nudges here.
	StateStalled
	// StateKillImminent: past the kill threshold or the runtime cap.
	StateKillImminent
)

// Describe renders a State as the one-line explanation shown to operators and
// written to the daemon log.
func (s State) Describe() string {
	switch s {
	case StateIdle:
		return "idle — no reviewer session, no review in flight"
	case StateUnreadable:
		return "unreadable — the heartbeat exists but could not be parsed; no action will be taken on it"
	case StateSpawning:
		return "spawning — dispatched, session still coming up"
	case StateDied:
		return "died mid-review — heartbeat outlived its session"
	case StateStarting:
		return "starting — session up, no review on record yet"
	case StateAbandoned:
		return "abandoned — session up with no review on record; self-termination may have failed"
	case StateWorking:
		return "working"
	case StateUnanchored:
		return "working — but no session clock is available, so the absolute runtime cap is not being enforced"
	case StateStalled:
		return "stalled — past the stuck threshold; the reaper will nudge"
	case StateKillImminent:
		return "stalled past the kill threshold — the reaper will terminate this session"
	}
	return "unknown"
}

// Observation is everything a consumer needs to classify a reviewer.
type Observation struct {
	// Heartbeat is the parsed heartbeat, or nil when absent.
	Heartbeat *Heartbeat
	// ReadErr is non-nil when the heartbeat existed but could not be read.
	ReadErr error
	// SessionAlive reports whether the rig's reviewer tmux session exists.
	SessionAlive bool
	// SessionAge is how long that session has existed; 0 means unknown. Used
	// only where the question really is about the SESSION — an unreadable or
	// absent heartbeat leaves no review to measure, so session age is then the
	// only clock there is.
	SessionAge time.Duration
	// ReviewObservedFor is how long the observer has been watching THIS review —
	// this (pr, round, sha) — on a clock the reviewed process does not own; 0
	// means unknown. See Runtime.
	//
	// Distinct from SessionAge because the session is shared: it survives `gt
	// reviewer done` and drains queued requests sequentially, so its age belongs
	// to no particular review. Feeding it to the absolute cap made every review
	// in a long-lived session kill-imminent on arrival.
	ReviewObservedFor time.Duration
	// MissingFor is how long the observer has seen NO USABLE heartbeat — absent
	// OR unreadable; 0 with MissingKnown false means the observer has not been
	// watching.
	//
	// Measured from first observation rather than from session creation on
	// purpose: the latter makes deleting heartbeat.json an instant kill switch
	// for any session older than MissingGrace.
	//
	// Unreadable shares this clock rather than getting its own because "cannot be
	// parsed" and "is not there" are the same thing to every rail: neither yields
	// a phase, a clock, or an identity. Sharing the timer is necessary for
	// corrupting the file to be no better an evasion than deleting it, but it is
	// not sufficient — the two must also reach the same STATES, which is why the
	// ReadErr branch runs the same session-anchored ladder as the absent case.
	MissingFor   time.Duration
	MissingKnown bool
	// StuckThreshold is the rig's configured (and clamped) stall threshold. Zero
	// is treated as DefaultStuckThreshold rather than as "no rails" — see
	// Classify. StuckThreshold() cannot return zero (it clamps to a 31m floor),
	// so a zero here can only mean a consumer forgot the field, and that must
	// fail toward enforcement rather than toward silence.
	StuckThreshold time.Duration
}

// Classify derives the State from an Observation.
//
// Deliberately total and pure: no filesystem, no tmux, no daemon. The rule that
// decides whether to kill an agent — and the one an operator reads before
// intervening — should be testable without a live town.
func Classify(o Observation) State {
	// A zero threshold means the CONSUMER forgot the field, not that the rig
	// wants no rails: StuckThreshold() clamps to a 31m floor and cannot return
	// zero. Branching on it — as this used to — skipped the runtime cap, the
	// stall rail and the kill rail together, so a reviewer wedged for thirty days
	// reported plain "working". Defaulting fails toward enforcement instead, and
	// it is the same asymmetry StateUnanchored was introduced to fix for
	// SessionAge, applied to the field whose omission costs strictly more.
	stuck := o.StuckThreshold
	if stuck <= 0 {
		stuck = DefaultStuckThreshold
	}

	if o.ReadErr != nil {
		// An unreadable heartbeat used to be an UNBOUNDED exemption from every
		// rail: it returned here before the session, the phase, and the runtime
		// were even consulted, so corrupting the file with one byte was a strictly
		// better evasion than deleting it.
		//
		// Sharing the missing case's timer was necessary but not sufficient — the
		// two must also reach the same STATES. Escalating only at the 4x absolute
		// cap left one garbage byte buying 180 minutes of "no action will be taken
		// on it" where `rm heartbeat.json` bought 15, and skipping the nudge rung
		// entirely. So this runs the same session-anchored ladder the absent case
		// runs, on the only clock an unusable heartbeat leaves: the session's.
		if !o.SessionAlive || !o.MissingKnown || o.MissingFor < MissingGrace {
			return StateUnreadable
		}
		// No session clock means no rail at all — say so, rather than returning
		// "no action will be taken on it", which is how the unbounded exemption
		// silently came back whenever a tmux query failed.
		if o.SessionAge <= 0 {
			return StateUnanchored
		}
		switch rt := Runtime(0, o.SessionAge); {
		case rt >= stuck*AbsoluteCapMultiple:
			return StateKillImminent
		case rt >= stuck*StuckMultiple:
			return StateStalled
		}
		return StateAbandoned
	}
	hb := o.Heartbeat

	if hb == nil {
		if !o.SessionAlive {
			return StateIdle
		}
		// The abandoned window must clear the documented no-touch gap, or a third
		// party gets a supervisory signal for free. The perspective subagents run
		// entirely between `gt reviewer prompt` and `gt reviewer consolidate` with
		// nothing refreshing the file — which is why the stuck threshold is 45m —
		// so with a bare 15m grace, one `rm heartbeat.json` during that window had
		// a perfectly healthy reviewer reported as abandoned. Waiting out the same
		// span the phase rail waits out costs only a slower cleanup of a genuinely
		// dead session.
		if !o.MissingKnown || o.MissingFor < abandonGrace(stuck) {
			return StateStarting
		}
		return StateAbandoned
	}

	age, ageOK := PhaseAge(hb)

	if !o.SessionAlive {
		// Ambiguous by nature: this is how a dead reviewer looks AND how a
		// dispatch still spawning looks, because the heartbeat is seeded before
		// the session exists. An untrustworthy timestamp resolves to the
		// conservative side.
		if !ageOK || age < SpawnGrace {
			return StateSpawning
		}
		return StateDied
	}

	// Live session with a heartbeat. The runtime rail is checked first and is
	// independent of phase age, so a reviewer looping through phases — which
	// refreshes its timestamp forever — is still caught.
	runtime := Runtime(hb.Elapsed(), o.ReviewObservedFor)
	if capDur := stuck * AbsoluteCapMultiple; runtime > 0 && runtime >= capDur {
		return StateKillImminent
	}
	if ageOK {
		switch {
		case age >= stuck*StuckMultiple:
			return StateKillImminent
		case age >= stuck:
			return StateStalled
		}
	}
	// Healthy on the phase rail — but say so honestly. Runtime returns 0 only
	// when the observer has no clock for this review, and without one the cap is
	// not merely inaccurate, it is off.
	if runtime == 0 {
		return StateUnanchored
	}
	return StateWorking
}

// abandonGrace is how long a live session may show no usable heartbeat before it
// counts as abandoned. It is the larger of MissingGrace and the rig's stuck
// threshold, because the stuck threshold is by construction the longest window
// in which a healthy review legitimately writes nothing.
func abandonGrace(stuck time.Duration) time.Duration {
	if stuck > MissingGrace {
		return stuck
	}
	return MissingGrace
}

// Thresholds. These live beside Classify because they are part of "what the
// heartbeat means" — every consumer (reaper, CLI, dispatcher) must agree on
// them or they will disagree about whether a reviewer is healthy.
const (
	// DefaultStuckThreshold is the compiled-in floor for "this reviewer has
	// stopped progressing", matching reviewer.toml's shipped value.
	//
	// It deliberately exceeds merge_queue.pr_review_timeout (30m) so the
	// refinery's await-review escalation — which carries diagnostics a silent
	// kill would destroy — always fires first. It must also accommodate a full
	// perspective subagent pass, because those run between `gt reviewer prompt`
	// and `gt reviewer consolidate` with nothing refreshing the heartbeat.
	DefaultStuckThreshold = 45 * time.Minute

	// StuckMultiple is how many stuck-thresholds of no progress mean the session
	// is unrecoverable rather than merely slow. One threshold earns a nudge; this
	// many earns a kill.
	StuckMultiple = 2

	// AbsoluteCapMultiple bounds TOTAL review wall time as a multiple of the
	// stuck threshold, independent of phase age. This is the rail a reviewer that
	// keeps refreshing its heartbeat while looping cannot evade.
	AbsoluteCapMultiple = 4

	// MinStuckThreshold / MaxStuckThreshold bound a rig-configured threshold.
	// reviewer.toml lives INSIDE the rig and is agent-writable, so an unclamped
	// value is a kill switch in both directions: a one-second threshold kills
	// every reviewer on sight, a one-year threshold disables the reaper.
	MinStuckThreshold = 31 * time.Minute
	MaxStuckThreshold = 6 * time.Hour
)

// StuckThreshold resolves a rig's configured stall threshold from its role
// definition, ALREADY CLAMPED, falling back to DefaultStuckThreshold.
//
// The single source for the configured value. The daemon reaper, the
// dispatch-time wedge check, and `gt reviewer status` all call it, so "the
// reaper would kill this session", "dispatch should recycle this session", and
// "the CLI says it is stalled" can never drift into three different numbers.
//
// The clamp is applied HERE rather than left to each caller. reviewer.toml lives
// inside the rig and is agent-writable, so the value is a kill switch in both
// directions — and a defense every caller must remember to apply is one a caller
// will eventually forget. There is now no way to obtain the raw value except by
// asking for it explicitly via StuckThresholdRaw, which exists only so the
// operator warning can name what was rejected.
func StuckThreshold(townRoot, rigPath string) time.Duration {
	clamped, _ := ClampStuckThreshold(StuckThresholdRaw(townRoot, rigPath))
	return clamped
}

// StuckThresholdRaw returns a rig's configured stall threshold UNCLAMPED. Only
// for reporting: pair it with ClampStuckThreshold to tell an operator which
// value was rejected and what replaced it. Never act on this directly.
func StuckThresholdRaw(townRoot, rigPath string) time.Duration {
	def, err := config.LoadRoleDefinition(townRoot, rigPath, constants.RoleReviewer)
	if err != nil || def == nil || def.Health.StuckThreshold.Duration <= 0 {
		return DefaultStuckThreshold
	}
	return def.Health.StuckThreshold.Duration
}

// ClampStuckThreshold bounds a configured threshold into the supported range,
// reporting whether it had to be adjusted. An out-of-range value is replaced by
// the default rather than by the nearest bound: a value that far off is more
// likely a mistake or an attack than an intent worth approximating.
func ClampStuckThreshold(d time.Duration) (time.Duration, bool) {
	if d < MinStuckThreshold || d > MaxStuckThreshold {
		return DefaultStuckThreshold, true
	}
	return d, false
}
