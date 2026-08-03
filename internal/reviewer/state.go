package reviewer

import (
	"fmt"
	"regexp"
	"strings"
	"time"

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
			continue
		}
		b.WriteRune(r)
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

// Runtime is the wall time an absolute cap should act on: the LONGER of the
// heartbeat's self-reported elapsed and the tmux session's own age.
//
// Neither signal is sufficient alone. Elapsed is forgeable by the reviewer —
// deleting the file, omitting started_at, or future-dating it all yield 0 or
// negative, which under an elapsed-only rule reads as "brand new" and evades
// the cap forever. Session age cannot be reset from inside the session (tmux
// owns it) but under-reports when a dispatcher seeded the heartbeat before the
// session existed. Negative inputs are 0 = unknown, never "healthy".
func Runtime(elapsed, sessionAge time.Duration) time.Duration {
	if elapsed < 0 {
		elapsed = 0
	}
	if sessionAge < 0 {
		sessionAge = 0
	}
	if sessionAge > elapsed {
		return sessionAge
	}
	return elapsed
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
// Refusing an unresolved or shared prefix costs only coverage on a
// misconfigured rig; guessing costs an unrelated rig's running review.
func ResolveSessionName(rigName string) (string, error) {
	prefix := session.PrefixFor(rigName)
	if prefix == "" {
		return "", fmt.Errorf("rig %q has no session prefix", rigName)
	}
	all := session.DefaultRegistry().AllRigs()
	if prefix == session.DefaultPrefix && all[rigName] != session.DefaultPrefix {
		return "", fmt.Errorf("rig %q has no beads.prefix and would collapse onto the shared %q session namespace",
			rigName, session.DefaultPrefix)
	}
	for other, p := range all {
		if other != rigName && p == prefix {
			return "", fmt.Errorf("rig %q shares session prefix %q with rig %q — target is ambiguous",
				rigName, prefix, other)
		}
	}
	return session.ReviewerSessionName(prefix), nil
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
	// SessionAge is how long that session has existed; 0 means unknown.
	SessionAge time.Duration
	// MissingFor is how long the observer has seen NO heartbeat; 0 with
	// MissingKnown false means the observer has not been watching.
	//
	// Measured from first observation rather than from session creation on
	// purpose: the latter makes deleting heartbeat.json an instant kill switch
	// for any session older than MissingGrace.
	MissingFor   time.Duration
	MissingKnown bool
	// StuckThreshold is the rig's configured (and clamped) stall threshold.
	StuckThreshold time.Duration
}

// Classify derives the State from an Observation.
//
// Deliberately total and pure: no filesystem, no tmux, no daemon. The rule that
// decides whether to kill an agent — and the one an operator reads before
// intervening — should be testable without a live town.
func Classify(o Observation) State {
	if o.ReadErr != nil {
		return StateUnreadable
	}
	hb := o.Heartbeat

	if hb == nil {
		if !o.SessionAlive {
			return StateIdle
		}
		if !o.MissingKnown || o.MissingFor < MissingGrace {
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
	if o.StuckThreshold > 0 {
		runtime := Runtime(hb.Elapsed(), o.SessionAge)
		if capDur := o.StuckThreshold * AbsoluteCapMultiple; runtime > 0 && runtime >= capDur {
			return StateKillImminent
		}
		if ageOK {
			switch {
			case age >= o.StuckThreshold*StuckMultiple:
				return StateKillImminent
			case age >= o.StuckThreshold:
				return StateStalled
			}
		}
	}
	return StateWorking
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
