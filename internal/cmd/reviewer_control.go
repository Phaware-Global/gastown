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
	"github.com/steveyegge/gastown/internal/reviewer"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	reviewerStopRig    string
	reviewerStopForce  bool
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
}

// collectReviewerState reads one rig's reviewer session + heartbeat.
func collectReviewerState(rigName, rigPath string) ReviewerState {
	sessionName := session.ReviewerSessionName(session.PrefixFor(rigName))
	t := tmux.NewTmux()
	running, _ := t.HasSession(sessionName)

	st := ReviewerState{Rig: rigName, Session: sessionName, Running: running}
	hb := reviewer.ReadHeartbeat(rigPath)
	if hb != nil {
		st.Phase, st.PR, st.Round, st.SHA = hb.Phase, hb.PR, hb.Round, hb.SHA
		st.PhaseAge = hb.Age().Round(time.Second).String()
		if el := hb.Elapsed(); el > 0 {
			st.Elapsed = el.Round(time.Second).String()
		}
	}
	st.Diagnosis = diagnoseReviewer(running, hb)
	return st
}

// diagnoseReviewer names the four session/heartbeat combinations in the same
// terms the daemon's reaper uses, so an operator reading `gt reviewer status`
// and an operator reading the daemon log see the same vocabulary.
func diagnoseReviewer(running bool, hb *reviewer.Heartbeat) string {
	switch {
	case !running && hb == nil:
		return "idle — no reviewer session, no review in flight"
	case !running && hb != nil:
		return "died mid-review — heartbeat present but no session (the reaper will clear it)"
	case running && hb == nil:
		return "orphan — session running with no review on record (self-termination may have failed)"
	default:
		return "working"
	}
}

func runReviewerStop(cmd *cobra.Command, args []string) error {
	rigName, rigPath, err := resolveReviewerRig(reviewerStopRig)
	if err != nil {
		return err
	}
	sessionName := session.ReviewerSessionName(session.PrefixFor(rigName))
	t := tmux.NewTmux()
	running, err := t.HasSession(sessionName)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		// Still clear a stale heartbeat: a dead session with a live heartbeat is
		// the "died mid-review" state, and leaving the record behind makes the
		// rig read as permanently stalled.
		if hb := reviewer.ReadHeartbeat(rigPath); hb != nil {
			if cerr := reviewer.ClearHeartbeat(rigPath); cerr != nil {
				return fmt.Errorf("clearing stale heartbeat: %w", cerr)
			}
			fmt.Printf("No reviewer session for rig %s; cleared a stale heartbeat (PR #%d, phase %s).\n",
				rigName, hb.PR, hb.Phase)
			return nil
		}
		return fmt.Errorf("no reviewer session running for rig %s", rigName)
	}

	if !reviewerStopForce {
		// Graceful first: an interrupt lets the agent finish a write in progress.
		_ = t.SendKeysRaw(sessionName, "C-c")
		session.WaitForSessionExit(t, sessionName, constants.GracefulShutdownTimeout)
	}
	if stillUp, _ := t.HasSession(sessionName); stillUp {
		if err := t.KillSessionWithProcesses(sessionName); err != nil {
			return fmt.Errorf("killing session %s: %w", sessionName, err)
		}
	}
	// Clear after the kill, mirroring the reaper: a heartbeat outliving its
	// session makes an idle rig look stalled.
	if err := reviewer.ClearHeartbeat(rigPath); err != nil {
		style.PrintWarning("could not clear reviewer heartbeat: %v", err)
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
