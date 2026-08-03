package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/reviewer"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	reviewerStopRig    string
	reviewerStopForce  bool
	reviewerStopPR     int
	reviewerStatusRig  string
	reviewerStatusJSON bool
	reviewerListJSON   bool
)

var reviewerStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a rig's Reviewer session",
	Long: `Terminate a rig's Reviewer session.

Until now the only way to end a reviewer session was ` + "`gt reviewer done`" + `, run by
the reviewer itself — which is exactly what a stuck reviewer cannot do. The
daemon's reaper handles the unattended case; this is the operator's manual
override for the session in front of them.

The rig is inferred from the current directory unless --rig is given. --force
skips the graceful interrupt and kills the session's process group immediately.

Clearing the session also clears its heartbeat, so the rig does not read as
permanently stalled afterwards.`,
	RunE: runReviewerStop,
}

var reviewerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show a rig's Reviewer session and review progress",
	Long: `Report whether a rig's Reviewer is running and what it is working on.

Combines the tmux session state with the reviewer heartbeat, so the answer
includes which PR is under review, which phase it reached, how long it has been
in that phase, and total review wall time — the numbers the daemon's reaper
acts on.

Note that a heartbeat parked at "prompt" is the NORMAL shape of a review in
flight: the perspective subagents run between 'gt reviewer prompt' and
'gt reviewer consolidate' with no command in between to refresh it. Judge by
total elapsed rather than phase age alone.

The rig is inferred from the current directory unless --rig is given.`,
	RunE: runReviewerStatus,
}

var reviewerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Reviewer sessions across all rigs",
	Long: `List every rig's Reviewer session state in one table.

The Reviewer is spawn-on-demand and has no persistent registry entry, so it is
otherwise invisible to a town-wide sweep. Rigs with no reviewer activity are
omitted unless --json is given.`,
	RunE: runReviewerList,
}

func init() {
	reviewerStopCmd.Flags().StringVar(&reviewerStopRig, "rig", "", "rig name (default: inferred from cwd)")
	reviewerStopCmd.Flags().IntVar(&reviewerStopPR, "pr", 0,
		"only clear the heartbeat if it records this PR (omit to clear unconditionally)")
	reviewerStopCmd.Flags().BoolVar(&reviewerStopForce, "force", false,
		"kill the process group immediately instead of interrupting first")

	reviewerStatusCmd.Flags().StringVar(&reviewerStatusRig, "rig", "", "rig name (default: inferred from cwd)")
	reviewerStatusCmd.Flags().BoolVar(&reviewerStatusJSON, "json", false, "output as JSON")

	reviewerListCmd.Flags().BoolVar(&reviewerListJSON, "json", false, "output as JSON (includes idle rigs)")

	reviewerCmd.AddCommand(reviewerStopCmd)
	reviewerCmd.AddCommand(reviewerStatusCmd)
	reviewerCmd.AddCommand(reviewerListCmd)
}

// resolveReviewerRig resolves the target rig from an explicit flag, else the
// current directory.
func resolveReviewerRig(explicit string) (string, string, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	name := explicit
	if name == "" {
		name, err = inferRigFromCwd(townRoot)
		if err != nil {
			return "", "", fmt.Errorf("could not determine rig (pass --rig): %w", err)
		}
	}
	_, r, err := getRig(name)
	if err != nil {
		return "", "", err
	}
	return r.Name, r.Path, nil
}

// ReviewerState is the reported state of one rig's Reviewer, combining session
// liveness with heartbeat progress.
//
// Every heartbeat-sourced string is sanitized before it lands here, because
// these values are printed to a terminal: a forged heartbeat with an embedded
// ESC sequence could otherwise rewrite the operator's screen — erasing the row
// that would have revealed a stalled review, or drawing a fake one.
type ReviewerState struct {
	Rig       string `json:"rig"`
	Session   string `json:"session"`
	Running   bool   `json:"running"`
	Phase     string `json:"phase,omitempty"`
	PR        int    `json:"pr,omitempty"`
	Round     int    `json:"round,omitempty"`
	SHA       string `json:"sha,omitempty"`
	PhaseAge  string `json:"phase_age,omitempty"`
	Elapsed   string `json:"elapsed,omitempty"`
	Diagnosis string `json:"diagnosis"`
	// Err is set when the rig could not be inspected (unresolvable session
	// target, tmux failure). Reported rather than swallowed: "cannot tell" and
	// "healthy" must not look the same to an operator.
	Err string `json:"error,omitempty"`
}

// collectReviewerState reads one rig's reviewer session + heartbeat and
// classifies it with reviewer.Classify — the SAME function the daemon reaper
// uses, so `gt reviewer status` can never report "working" for a session the
// reaper is about to kill.
func collectReviewerState(rigName, rigPath string) ReviewerState {
	st := ReviewerState{Rig: rigName}

	// Read the heartbeat FIRST and unconditionally. An unresolvable session
	// target must block acting on the rig, not reporting on it — the heartbeat is
	// still readable and is exactly what an operator needs in order to understand
	// a misconfigured rig.
	hb, rerr := reviewer.ReadHeartbeatE(rigPath)
	if hb != nil {
		st.Phase = reviewer.SafePhase(hb.Phase)
		st.PR = reviewer.SafePR(hb.PR)
		st.Round = hb.Round
		st.SHA = reviewer.SafeSHA(hb.SHA)
		if age, ok := reviewer.PhaseAge(hb); ok {
			st.PhaseAge = age.Round(time.Second).String()
		}
	}

	obs := reviewer.Observation{
		Heartbeat:      hb,
		ReadErr:        rerr,
		StuckThreshold: clampedStuckThreshold(rigPath),
	}

	sessionName, serr := reviewer.ResolveSessionName(rigName)
	if serr != nil {
		// Refusing an ambiguous target matters more here than in the reaper:
		// `gt reviewer stop` acts on this name, and a rig without beads.prefix
		// collapses onto the shared "gt-reviewer", so guessing would let an
		// operator kill a rig they did not name.
		st.Err = serr.Error()
	} else {
		st.Session = sessionName
		t := tmux.NewTmux()
		running, herr := t.HasSession(sessionName)
		if herr != nil {
			// Surface rather than swallow: "cannot tell" and "healthy" must not
			// look the same to an operator.
			st.Err = herr.Error()
		} else {
			st.Running = running
			obs.SessionAlive = running
			obs.SessionAge = sessionAgeOrZero(t, sessionName, running)
		}
	}

	if hb != nil {
		// Report the CORROBORATED runtime, not the heartbeat's self-report. The
		// reaper deliberately refuses to trust elapsed alone (it is forgeable by
		// the reviewer), so showing the raw value would tell an operator a
		// different story than the one the reaper is acting on.
		if rt := reviewer.Runtime(hb.Elapsed(), obs.SessionAge); rt > 0 {
			st.Elapsed = rt.Round(time.Second).String()
		}
	}

	st.Diagnosis = reviewer.Classify(obs).Describe()
	if st.Err != "" {
		st.Diagnosis += " (session state unknown: " + st.Err + ")"
	}
	return st
}

// sessionAgeOrZero returns the tmux session's age, or 0 when unknown.
func sessionAgeOrZero(t *tmux.Tmux, sessionName string, running bool) time.Duration {
	if !running {
		return 0
	}
	created, err := t.GetSessionCreatedTime(sessionName)
	if err != nil || created.IsZero() {
		return 0
	}
	return time.Since(created)
}

// clampedStuckThreshold resolves the rig's threshold through the same clamp the
// reaper applies, so the CLI's notion of "stalled" matches the reaper's.
func clampedStuckThreshold(rigPath string) time.Duration {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return reviewer.DefaultStuckThreshold
	}
	clamped, _ := reviewer.ClampStuckThreshold(reviewer.StuckThreshold(townRoot, rigPath))
	return clamped
}

func runReviewerStop(cmd *cobra.Command, args []string) error {
	rigName, rigPath, err := resolveReviewerRig(reviewerStopRig)
	if err != nil {
		return err
	}
	// Refuse an ambiguous target. A rig without beads.prefix collapses onto the
	// shared "gt-reviewer" session name, so guessing here would let an operator
	// kill a rig they did not name.
	sessionName, serr := reviewer.ResolveSessionName(rigName)
	if serr != nil {
		return fmt.Errorf("cannot determine which session belongs to rig %s: %w", rigName, serr)
	}

	t := tmux.NewTmux()
	running, err := t.HasSession(sessionName)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}

	hb, rerr := reviewer.ReadHeartbeatE(rigPath)
	if rerr != nil {
		// An unreadable heartbeat is the one case where an unconditional clear is
		// right: it cannot be matched against --pr, and leaving it makes the rig
		// permanently unusable to both the reaper and status.
		style.PrintWarning("reviewer heartbeat is unreadable (%v) — clearing it", rerr)
		if cerr := reviewer.ClearHeartbeat(rigPath); cerr != nil {
			return fmt.Errorf("clearing unreadable heartbeat: %w", cerr)
		}
	}

	if !running {
		if hb == nil {
			return fmt.Errorf("no reviewer session running for rig %s", rigName)
		}
		// A heartbeat with no session is AMBIGUOUS: it is both how a dead reviewer
		// looks and how a still-spawning dispatch looks, because `gt reviewer
		// request` seeds the record before the session exists. Deleting it inside
		// the spawn window destroys the only evidence that a request was made —
		// while that request's mail is still sitting in the mailbox, so the review
		// would later start with no record at all.
		if reviewer.Classify(reviewer.Observation{Heartbeat: hb}) == reviewer.StateSpawning {
			return fmt.Errorf("rig %s has a review dispatched for PR #%d but no session yet "+
				"(still inside the %s spawn window) — its request mail is queued and the session "+
				"may still come up; wait, or re-run once it has",
				rigName, reviewer.SafePR(hb.PR), reviewer.SpawnGrace)
		}
		if _, cerr := reviewer.ClearHeartbeatFor(rigPath, reviewerStopPR); cerr != nil {
			return fmt.Errorf("clearing stale heartbeat: %w", cerr)
		}
		fmt.Printf("No reviewer session for rig %s; cleared a stale heartbeat (PR #%d, phase %s).\n",
			rigName, reviewer.SafePR(hb.PR), reviewer.SafePhase(hb.Phase))
		return nil
	}

	if !reviewerStopForce {
		// Graceful first: an interrupt lets the agent finish a write in progress.
		_ = t.SendKeysRaw(sessionName, "C-c")
		session.WaitForSessionExit(t, sessionName, constants.GracefulShutdownTimeout)
	}
	stillUp, err := t.HasSession(sessionName)
	if err != nil {
		// Previously this error was swallowed and the command printed "Stopped
		// reviewer" without having killed anything.
		return fmt.Errorf("re-checking session before kill: %w", err)
	}
	if stillUp {
		if err := t.KillSessionWithProcesses(sessionName); err != nil {
			return fmt.Errorf("killing session %s: %w", sessionName, err)
		}
	}

	// An operator-initiated kill is an audit event. Without it the only record
	// that a review was terminated is the heartbeat this command then deletes,
	// so a stopped review would leave no trace at all.
	pr, phase := 0, ""
	if hb != nil {
		pr, phase = reviewer.SafePR(hb.PR), reviewer.SafePhase(hb.Phase)
	}
	if ferr := events.LogFeed(events.TypeKill, rigName+"/"+constants.RoleReviewer,
		map[string]interface{}{
			"rig": rigName, "role": constants.RoleReviewer,
			"reason": "operator_stop", "pr": pr, "phase": phase,
			"forced": reviewerStopForce,
		}); ferr != nil {
		style.PrintWarning("could not record the stop in the feed: %v", ferr)
	}

	cleared, cerr := reviewer.ClearHeartbeatFor(rigPath, reviewerStopPR)
	switch {
	case cerr != nil:
		style.PrintWarning("could not clear reviewer heartbeat: %v", cerr)
	case !cleared && hb != nil:
		fmt.Printf("Left the heartbeat in place — it records PR #%d, not #%d.\n",
			reviewer.SafePR(hb.PR), reviewerStopPR)
	}
	fmt.Printf("Stopped reviewer for rig %s (session %s).\n", rigName, sessionName)
	return nil
}

func runReviewerStatus(cmd *cobra.Command, args []string) error {
	rigName, rigPath, err := resolveReviewerRig(reviewerStatusRig)
	if err != nil {
		return err
	}
	st := collectReviewerState(rigName, rigPath)

	if reviewerStatusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}

	fmt.Printf("%s %s\n", style.Bold.Render("Reviewer:"), rigName)
	fmt.Printf("  Session:   %s (%s)\n", st.Session, runningLabel(st.Running))
	if st.PR > 0 {
		fmt.Printf("  Reviewing: PR #%d (round %d)\n", st.PR, st.Round)
	}
	if st.SHA != "" {
		fmt.Printf("  SHA:       %s\n", shortSHA(st.SHA))
	}
	if st.Phase != "" {
		fmt.Printf("  Phase:     %s (for %s)\n", st.Phase, st.PhaseAge)
	}
	if st.Elapsed != "" {
		fmt.Printf("  Elapsed:   %s (total review time)\n", st.Elapsed)
	}
	fmt.Printf("  State:     %s\n", st.Diagnosis)
	if st.Err != "" {
		// "could not tell" must never render as "healthy".
		style.PrintWarning("inspection incomplete: %s", st.Err)
	}
	return nil
}

func runningLabel(running bool) string {
	if running {
		return "running"
	}
	return "not running"
}

func runReviewerList(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigsConfig, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return fmt.Errorf("loading rigs: %w", err)
	}

	names := make([]string, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		names = append(names, name)
	}
	sort.Strings(names)

	states := make([]ReviewerState, 0, len(names))
	for _, name := range names {
		_, r, gerr := getRig(name)
		if gerr != nil || r == nil {
			continue
		}
		states = append(states, collectReviewerState(name, r.Path))
	}

	if reviewerListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(states)
	}

	active := make([]ReviewerState, 0, len(states))
	for _, s := range states {
		if s.Running || s.Phase != "" {
			active = append(active, s)
		}
	}
	if len(active) == 0 {
		fmt.Println("No reviewer activity in any rig.")
		return nil
	}
	fmt.Printf("%-24s %-9s %-14s %-8s %-10s %s\n", "RIG", "SESSION", "PHASE", "PR", "ELAPSED", "STATE")
	for _, s := range active {
		pr := ""
		if s.PR > 0 {
			pr = fmt.Sprintf("#%d", s.PR)
		}
		fmt.Printf("%-24s %-9s %-14s %-8s %-10s %s\n",
			s.Rig, runningShort(s.Running), dashIfEmpty(s.Phase), dashIfEmpty(pr),
			dashIfEmpty(s.Elapsed), s.Diagnosis)
	}
	return nil
}

func runningShort(running bool) string {
	if running {
		return "up"
	}
	return "down"
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
