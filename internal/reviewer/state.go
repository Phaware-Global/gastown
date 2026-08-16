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
// The tmux session's age is the ONLY clock here that the reviewed process does
// not own, so it is the only one allowed to authorize a kill. The heartbeat's
// self-reported elapsed is admitted solely as a refinement WITHIN the window the
// session clock already permits — it can raise the number by at most SpawnGrace,
// the legitimate head start a dispatcher-seeded heartbeat has over its session,
// and it can never lower it below the session's own age:
//
//	runtime = max(sessionAge, min(elapsed, sessionAge+SpawnGrace))
//
// Both bounds earn their place, and each closes a bug the other reopens:
//
//   - The min bounds FORGERY. This function used to take a plain max, so a
//     `started_at` of now-1000h in a rig-writable file was on its own sufficient
//     to SIGKILL a healthy thirty-second-old reviewer's whole process tree on
//     the next tick.
//   - The max floor bounds EVASION. Deleting or zeroing the file reports elapsed
//     0, so a min alone would let `rm heartbeat.json` in a loop clear the cap
//     forever. The session clock keeps accruing underneath.
//
// A zero sessionAge means the session clock is UNAVAILABLE, not that the session
// is new, and this returns 0 — "unknown" — rather than falling back to the
// self-report. Falling back was the last path by which a rig-writable file alone
// could authorize a kill, and the fallback bought little: without a working tmux
// query the reaper cannot carry out the kill anyway, while the phase rails,
// which are bounded separately by PhaseAge, still apply.
//
// The session clock deliberately OVER-estimates a later round's own wall time,
// because rounds share a session (Manager.EnsureRunning is a no-op when one is
// up). That inaccuracy is why a cap breach must nudge a still-progressing
// reviewer before it kills, rather than acting on this number alone. It is the
// right trade: the failure mode of an over-estimate is a nudge, and the failure
// mode of a forgeable clock is a kill.
func Runtime(elapsed, sessionAge time.Duration) time.Duration {
	if sessionAge <= 0 {
		return 0
	}
	claimed := elapsed
	if claimed > sessionAge+SpawnGrace {
		claimed = sessionAge + SpawnGrace
	}
	if claimed > sessionAge {
		return claimed
	}
	return sessionAge
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
// Refusing an ambiguous name costs only coverage on a misconfigured rig;
// guessing costs an unrelated rig's running review.
//
// peers is the caller's own list of rig names to check for collisions, and it
// matters that it is a parameter. An earlier version resolved collisions from
// session.DefaultRegistry().AllRigs(), which is populated only from rigs.json —
// so a rig ABSENT from rigs.json could never be detected as a collision partner
// even though PrefixForRig hands it the fallback prefix and it therefore runs
// under literally "gt-reviewer". The refusal fired on the misconfigured rig and
// not on the correctly-configured `beads.prefix = "gt"` rig sharing its session
// name — exactly backwards. Passing the caller's rig list (the daemon already
// enumerates one) covers rigs the registry never learned about. An empty peers
// list means "no other rigs to disambiguate against", not "checks disabled":
// the DefaultPrefix rule below still applies.
func ResolveSessionName(rigName string, peers []string) (string, error) {
	prefix := session.PrefixFor(rigName)
	if prefix == "" {
		return "", fmt.Errorf("rig %q has no session prefix", rigName)
	}
	name := session.ReviewerSessionName(prefix)
	registered := session.DefaultRegistry().AllRigs()
	if prefix == session.DefaultPrefix && registered[rigName] != session.DefaultPrefix {
		return "", fmt.Errorf("rig %q has no beads.prefix and would collapse onto the shared %q session namespace",
			rigName, session.DefaultPrefix)
	}
	// Compare resolved NAMES, not prefixes. Two rigs collide precisely when they
	// produce the same tmux session, and a rig missing from the registry produces
	// one via the same fallback that makes it invisible in the prefix map.
	seen := make(map[string]bool, len(peers)+len(registered))
	for _, p := range peers {
		seen[p] = true
	}
	for r := range registered {
		seen[r] = true
	}
	for other := range seen {
		if other != rigName && session.ReviewerSessionName(session.PrefixFor(other)) == name {
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
	// SessionAge is how long that session has existed; 0 means unknown. A live
	// session is never genuinely zero seconds old at observation time, so this
	// needs no companion known-flag — but see StateUnanchored, which is how
	// "unknown" is surfaced rather than silently absorbed.
	SessionAge time.Duration
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
	// a phase, a clock, or an identity. Keeping them on one timer is also what
	// makes corrupting the file no better an evasion than deleting it.
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
		// An unreadable heartbeat used to be an UNBOUNDED exemption from every
		// rail: it returned here before the session, the phase, and the runtime
		// were even consulted, so corrupting the file with one byte was a strictly
		// better evasion than deleting it — deletion was caught by the abandoned
		// path, corruption by nothing, forever.
		//
		// Time-bound it exactly as the missing case is bounded. A transient I/O
		// error or a torn read still costs nothing: it has to persist, with the
		// observer watching, past the same grace window before anything escalates.
		if !o.SessionAlive || !o.MissingKnown || o.MissingFor < MissingGrace {
			return StateUnreadable
		}
		// Past the window there is no usable heartbeat and there IS a live
		// session, so fall through to the one rail that needs no heartbeat at all.
		// Runtime(0, SessionAge) is the session-anchored bound, which is precisely
		// the clock a process that owns the file cannot touch.
		if o.StuckThreshold > 0 {
			if rt := Runtime(0, o.SessionAge); rt > 0 && rt >= o.StuckThreshold*AbsoluteCapMultiple {
				return StateKillImminent
			}
		}
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
		// Healthy on the phase rail — but say so honestly. Runtime returns 0 only
		// when there is no session clock, and without one the cap is not merely
		// inaccurate, it is off.
		if runtime == 0 {
			return StateUnanchored
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
