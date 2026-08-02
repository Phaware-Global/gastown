package daemon

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/reviewer"
	"github.com/steveyegge/gastown/internal/session"
)

// The reviewer heartbeat is UNTRUSTED INPUT.
//
// It is a plain file inside the rig, writable by any process there, and its
// primary author is an agent whose own input (PR diffs, commit messages) is
// attacker-influenced by construction. The reaper turns that file into
// authority to SIGKILL a process tree — so every value crossing this boundary
// is validated, clamped, or corroborated against something the reviewed process
// does not own before it can influence a kill or reach an agent's prompt.
//
// The rules below exist because each was independently exploitable:
//
//   - a forged started_at killed a healthy reviewer on the next tick
//   - omitting started_at entirely evaded the absolute cap
//   - a future-dated timestamp made a wedged reviewer immortal
//   - hb.Phase was echoed verbatim into the reviewer's TUI under a
//     daemon-authored prefix — a prompt-injection channel
//   - hb.Phase and hb.SHA were written raw into the line-oriented daemon log,
//     letting a newline forge log entries
//   - a rig-authored stuck_threshold set the kill clock in both directions

// shaPattern matches a plausible git object id. Anything else is rejected
// rather than truncated, so a crafted value cannot survive as a prefix.
var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// maxLoggedFieldLen bounds any heartbeat-sourced string reaching a log line.
const maxLoggedFieldLen = 64

// safePhase maps a heartbeat phase onto the known phase constants, returning
// "unknown" for anything else.
//
// Used everywhere a phase reaches a log line, a feed payload, or — critically —
// an agent's prompt. An allowlist rather than an escape: the set of legitimate
// values is small, closed, and known, so there is no reason to pass through
// anything outside it.
func safePhase(phase string) string {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case reviewer.PhaseDispatched:
		return reviewer.PhaseDispatched
	case reviewer.PhaseCheckout:
		return reviewer.PhaseCheckout
	case reviewer.PhasePrompt:
		return reviewer.PhasePrompt
	case reviewer.PhaseConsolidate:
		return reviewer.PhaseConsolidate
	case reviewer.PhasePost:
		return reviewer.PhasePost
	default:
		return "unknown"
	}
}

// safeSHA returns a validated short SHA, or "unknown".
func safeSHA(sha string) string {
	s := strings.TrimSpace(sha)
	if !shaPattern.MatchString(s) {
		return "unknown"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// safeLogField strips anything that could forge a log entry: newlines, carriage
// returns, and other control characters, then truncates. The daemon log is
// line-oriented, so an unsanitized field with an embedded newline can author
// entries that appear to come from the daemon itself.
func safeLogField(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxLoggedFieldLen {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// safePR clamps a heartbeat PR number to a plausible range; 0 means "unknown".
func safePR(pr int) int {
	if pr < 0 || pr > 10_000_000 {
		return 0
	}
	return pr
}

// Threshold bounds. A rig's reviewer.toml is agent-writable, so an unclamped
// stuck_threshold is a kill switch in both directions: a tiny value kills every
// reviewer on sight, and a huge one disables the reaper entirely.
const (
	// minReviewerStuckThreshold must stay above pr_review_timeout (30m) so the
	// refinery's await-review escalation always fires before anything kills the
	// session — that escalation carries diagnostics a kill destroys.
	minReviewerStuckThreshold = 31 * time.Minute
	// maxReviewerStuckThreshold bounds how long a wedged reviewer can hold a
	// capacity slot before the operator's own config stops being believed.
	maxReviewerStuckThreshold = 6 * time.Hour
)

// clampStuckThreshold bounds a configured threshold into the compiled-in range,
// reporting whether it had to be adjusted so the caller can log it. An
// out-of-range value is replaced by the default rather than by the nearest
// bound: a value that far off is more likely a mistake or an attack than an
// intent worth approximating.
func clampStuckThreshold(d time.Duration) (time.Duration, bool) {
	if d < minReviewerStuckThreshold || d > maxReviewerStuckThreshold {
		return reviewer.DefaultStuckThreshold, true
	}
	return d, false
}

// reviewerRuntime is the wall time the absolute cap acts on: the LONGER of the
// heartbeat's self-reported elapsed and the tmux session's own age.
//
// Neither signal is sufficient alone. hb.Elapsed() is forgeable by the reviewer
// — deleting the file, omitting started_at, or future-dating it all yield 0 or
// negative, which under an elapsed-only rule meant "brand new" and evaded the
// cap forever. sessionAge cannot be reset from inside the session (tmux owns
// it), but it under-reports when a dispatcher seeded the heartbeat before the
// session existed. Taking the max makes the bound unforgeable without losing
// the dispatcher's head start.
//
// Negative inputs are treated as 0 = unknown, never as "healthy".
func reviewerRuntime(elapsed, sessionAge time.Duration) time.Duration {
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

// reviewerPhaseAge is the heartbeat's progress signal, with a future-dated
// timestamp treated as unknown rather than as a negative (infinitely fresh)
// age. Without this a single future timestamp makes a wedged reviewer immortal:
// Age() stays negative, so neither the nudge nor the kill threshold is ever
// crossed. Returns ok=false when the value cannot be trusted.
func reviewerPhaseAge(age time.Duration) (time.Duration, bool) {
	if age < 0 {
		return 0, false
	}
	return age, true
}

// resolveReviewerSession returns the tmux session name for a rig's reviewer,
// refusing to guess.
//
// session.PrefixFor falls back to DefaultPrefix ("gt") for any rig missing a
// beads.prefix in rigs.json, so several rigs can collapse onto the single
// session name "gt-reviewer". The reaper would then read rig A's heartbeat and
// kill rig B's healthy reviewer. Refusing an unresolved or shared prefix costs
// only reaper coverage on a misconfigured rig; guessing costs an unrelated
// rig's running review.
func resolveReviewerSession(rigName string) (string, error) {
	prefix := session.PrefixFor(rigName)
	if prefix == "" {
		return "", fmt.Errorf("rig %q has no session prefix", rigName)
	}
	if prefix == session.DefaultPrefix {
		// Accept the default only when this rig genuinely owns it — i.e. it is
		// registered with that prefix rather than merely falling back to it.
		if session.DefaultRegistry().AllRigs()[rigName] != session.DefaultPrefix {
			return "", fmt.Errorf("rig %q has no beads.prefix and would collapse onto the shared %q session namespace",
				rigName, session.DefaultPrefix)
		}
	}
	// Any other rig registered under the same prefix makes the target ambiguous.
	for other, p := range session.DefaultRegistry().AllRigs() {
		if other != rigName && p == prefix {
			return "", fmt.Errorf("rig %q shares session prefix %q with rig %q — target is ambiguous",
				rigName, prefix, other)
		}
	}
	return session.ReviewerSessionName(prefix), nil
}
