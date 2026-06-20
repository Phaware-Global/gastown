package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/style"
)

// G33: imperative review-fix dispatch step. Replaces the bash blob in the
// refinery patrol formula's PR.5 step with a single command that the LLM
// has nowhere to optimize away into "do it myself". The subcommand
// preserves the exact state machine the bash had (polecat-in-flight check,
// thread poll, iter-cap escalation, dispatch, bead update) and shells out
// to the same primitives (`gt polecat status`, `gt sling`,
// `gt mq set-review-state`, `gt escalate`) the bash already used — so this
// is a structural collapse, not a re-implementation.
//
// Actor-boundary principle (see refinery-pr-workflow.md §"Actor-boundary
// principle"): the refinery is purely an orchestrator under pr mode. This
// command never edits source files, never pushes to a polecat branch, and
// never resolves review threads — it dispatches a polecat that does all
// of that and reports back.

var (
	refPrDispatchReviewFixMR string
)

var refineryPrDispatchReviewFixCmd = &cobra.Command{
	Use:   "dispatch-review-fix <pr-number>",
	Short: "Dispatch a polecat to address unresolved review threads (one-shot, patrol-resumable)",
	Long: `Run one cycle of the imperative review-fix dispatch gate on a PR.

Replaces the prose-style "if threads unresolved, sling a polecat" bash blob
in the refinery patrol formula's PR.5 step. Patrol-resumable: each invocation
takes at most one action and returns immediately.

State machine (read from MR bead, write to MR bead via gt mq set-review-state):

  - review_fix_polecat set + polecat 'working'  → exit 1 (still fixing)
  - review_fix_polecat set + polecat terminal   → clear state, exit 1
                                                  (next patrol re-enters PR.4
                                                   for the new HEAD)
  - threads clean (count = 0)                   → exit 0 (advance to PR.6)
  - threads dirty + iter < pr_review_loop_max   → dispatch polecat, set
                                                  review_fix_polecat +
                                                  review_loop_iter on the bead,
                                                  exit 1
  - threads dirty + iter >= pr_review_loop_max  → escalate via gt escalate,
                                                  exit 3

Exit codes:
  0  no review-fix needed — advance to PR.6 (wait-approval)
  1  still in flight or just dispatched — patrol again
  3  iteration cap reached — escalate (gt escalate already invoked)
  4  operational/config error — caller escalates`,
	Args: cobra.ExactArgs(1),
	RunE: runRefineryPrDispatchReviewFix,
}

func init() {
	refineryPrCmd.AddCommand(refineryPrDispatchReviewFixCmd)
	refineryPrDispatchReviewFixCmd.Flags().StringVar(&refPrDispatchReviewFixMR, "mr", "",
		"MR bead ID (required; carries review_pr / branch / source_issue / review_fix_polecat / review_loop_iter)")
}

// dispatchReviewFixState is the parsed MR-bead snapshot the dispatch loop
// consumes. Loaded from the bead's description by parseDispatchMRFields.
// All fields are required for the dispatch path; the caller validates and
// escalates on missing entries rather than muddling through with empties.
type dispatchReviewFixState struct {
	PRNumber       int
	Branch         string
	SourceIssue    string
	ReviewFixName  string // currently-dispatched polecat name, or empty
	ReviewLoopIter int    // already incremented; 0 if never dispatched
}

func parseDispatchMRFields(mrID string) (dispatchReviewFixState, error) {
	if mrID == "" {
		return dispatchReviewFixState{}, fmt.Errorf("--mr is required")
	}
	if err := validateBeadIDShape(mrID); err != nil {
		return dispatchReviewFixState{}, fmt.Errorf("invalid --mr %q: %w", mrID, err)
	}
	bd := beads.New(resolveBeadDir(mrID))
	issue, err := bd.Show(mrID)
	if err != nil {
		return dispatchReviewFixState{}, fmt.Errorf("loading MR bead %s: %w", mrID, err)
	}
	if issue == nil {
		return dispatchReviewFixState{}, fmt.Errorf("MR bead %s not found", mrID)
	}
	fields := beads.ParseMRFields(issue)
	if fields == nil {
		return dispatchReviewFixState{}, fmt.Errorf("MR %s has no MR fields in description", mrID)
	}

	prNumberStr := extractDescField(issue.Description, "review_pr")
	if prNumberStr == "" {
		return dispatchReviewFixState{}, fmt.Errorf("MR %s missing review_pr field", mrID)
	}
	prNumber, err := parsePRNumber(prNumberStr)
	if err != nil {
		return dispatchReviewFixState{}, fmt.Errorf("MR %s review_pr=%q: %w", mrID, prNumberStr, err)
	}
	if fields.SourceIssue == "" {
		return dispatchReviewFixState{}, fmt.Errorf("MR %s missing source_issue", mrID)
	}
	if fields.Branch == "" {
		return dispatchReviewFixState{}, fmt.Errorf("MR %s missing branch", mrID)
	}

	return dispatchReviewFixState{
		PRNumber:       prNumber,
		Branch:         fields.Branch,
		SourceIssue:    fields.SourceIssue,
		ReviewFixName:  fields.ReviewFixPolecat,
		ReviewLoopIter: fields.ReviewLoopIter,
	}, nil
}

// extractDescField pulls a `key: value` line out of the MR description.
// review_pr is now a first-class MRField (gt-5le: written by `gt refinery pr
// create`), but we still read it directly rather than via beads.ParseMRFields
// because this stops at the FIRST match — a later prose reference to
// `review_pr:` can't shadow the structured value, whereas ParseMRFields keeps
// the last match. After any SetMRFields rewrite there is exactly one
// review_pr line, so the two agree; the first-match guard only matters for a
// hand-edited bead with stray prose.
func extractDescField(desc, key string) string {
	prefix := key + ":"
	for _, line := range strings.Split(desc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		}
	}
	return ""
}

func runRefineryPrDispatchReviewFix(cmd *cobra.Command, args []string) error {
	// Operational/config errors return exit 4 per the documented exit-code
	// contract — exit 1 means "wait, retry next patrol", and we don't want
	// the caller silently looping on a config-shape error. Each failure
	// path below wraps its return with NewSilentExit(4) where the failure
	// is operational rather than transient.
	prNumberArg, err := parsePRNumber(args[0])
	if err != nil {
		return wrapOperationalErr(err)
	}
	provider, cfg, rigPtr, err := getRefineryPRContext()
	if err != nil {
		return wrapOperationalErr(err)
	}
	if rigPtr == nil || rigPtr.Name == "" {
		return wrapOperationalErr(fmt.Errorf("dispatch-review-fix: rig name unknown (cwd not in a rig)"))
	}
	rigName := rigPtr.Name

	state, err := parseDispatchMRFields(refPrDispatchReviewFixMR)
	if err != nil {
		return wrapOperationalErr(err)
	}
	if state.PRNumber != prNumberArg {
		return wrapOperationalErr(fmt.Errorf(
			"dispatch-review-fix: PR arg #%d disagrees with MR %s review_pr=%d — refusing to dispatch on a different PR",
			prNumberArg, refPrDispatchReviewFixMR, state.PRNumber))
	}

	// Release-branch bypass: a PR whose head branch matches a configured
	// review-bypass glob skips the review loop, so there is no review-fix to
	// dispatch. Exit 0 to advance the formula to PR.6 (wait-approval) — the
	// same exit code used when there are no unresolved threads.
	if cfg != nil && branchMatchesReviewBypass(cfg.ReviewBypassBranches, state.Branch) {
		fmt.Fprintf(os.Stdout,
			"PR #%d: head branch %q matches a review-bypass pattern — no review-fix needed\n",
			state.PRNumber, state.Branch)
		return nil
	}

	maxIter := 3
	if cfg != nil && cfg.PRReviewLoopMax > 0 {
		maxIter = cfg.PRReviewLoopMax
	}

	// Stage 1: if a polecat is already in flight, classify its state and
	// either keep waiting OR clear the marker so the next patrol cycle can
	// re-enter PR.4 for the new HEAD.
	if state.ReviewFixName != "" {
		alive, lookupErr := isReviewFixPolecatAlive(rigName, state.ReviewFixName)
		if lookupErr != nil {
			// Status-lookup failure: keep state, retry next patrol. NEVER
			// clear review_fix_polecat on a transient lookup error — that
			// would re-dispatch the same work and burn an iteration.
			fmt.Fprintf(os.Stdout, "PR #%d: polecat %s status check failed (%v); retrying next patrol\n",
				state.PRNumber, state.ReviewFixName, lookupErr)
			return NewSilentExit(1)
		}
		if alive {
			fmt.Fprintf(os.Stdout, "PR #%d awaiting review-fix polecat %s (iter=%d)\n",
				state.PRNumber, state.ReviewFixName, state.ReviewLoopIter)
			return NewSilentExit(1)
		}
		// Polecat finished or otherwise terminal: clear state + the await
		// timestamp so the next patrol cycle re-enters PR.4 for the new
		// HEAD. The G35/G36 drift detection in await-review (PR #50) will
		// then re-post the trigger automatically when it sees the new SHA.
		if err := mqClearReviewState(refPrDispatchReviewFixMR); err != nil {
			return wrapOperationalErr(fmt.Errorf("clearing review-fix state on MR %s: %w", refPrDispatchReviewFixMR, err))
		}
		fmt.Fprintf(os.Stdout,
			"PR #%d: review-fix polecat %s done — cleared review_fix_polecat + await_review_started_at; next patrol re-enters PR.4\n",
			state.PRNumber, state.ReviewFixName)
		return NewSilentExit(1)
	}

	// Stage 2: no polecat in flight — poll unresolved threads. Author-
	// agnostic: every unresolved thread counts regardless of who opened it.
	// Filtering by author here would re-introduce the G12b dogfood failure
	// mode (refinery parked "waiting for X" while threads from Y sit open).
	threads, err := provider.UnresolvedThreads(state.PRNumber)
	if err != nil {
		return wrapOperationalErr(fmt.Errorf("polling unresolved threads: %w", err))
	}
	if len(threads) == 0 {
		fmt.Fprintf(os.Stdout, "PR #%d: no unresolved review threads, advancing to wait-approval\n",
			state.PRNumber)
		return nil
	}

	// Stage 3: threads exist. Either dispatch a fresh polecat or escalate
	// if we've hit the iteration cap.
	if state.ReviewLoopIter >= maxIter {
		// Escalate. The mayor closes the escalation when a human merges
		// the PR or kills the loop; the next patrol picks the MR back up.
		threadsJSON, _ := json.Marshal(threads)
		if err := escalateReviewLoopCap(
			fmt.Sprintf("PR #%d review loop exceeded %d iterations; %d thread(s) still unresolved",
				state.PRNumber, maxIter, len(threads)),
			string(threadsJSON)); err != nil {
			return wrapOperationalErr(fmt.Errorf("escalating iteration cap: %w", err))
		}
		fmt.Fprintf(os.Stdout, "PR #%d: review loop iteration cap (%d) reached — escalated to mayor\n",
			state.PRNumber, maxIter)
		return NewSilentExit(3)
	}

	// Stage 4: dispatch a fresh polecat.
	polecatName, err := slingReviewFixPolecat(rigName, state, threads, cfg.PRReviewer)
	if err != nil {
		// Dispatch failure: do NOT advance the iter counter — a failed sling
		// shouldn't burn an iteration. Next patrol retries from the same
		// state. The error path covers auth issues, rig paused, beads lock
		// contention, etc.
		fmt.Fprintf(os.Stdout, "PR #%d: review-fix dispatch FAILED (%v) — iter stays at %d, will retry next patrol\n",
			state.PRNumber, err, state.ReviewLoopIter)
		return NewSilentExit(4)
	}
	if polecatName == "" {
		fmt.Fprintf(os.Stdout, "PR #%d: review-fix dispatch returned empty polecat name — iter stays at %d, will retry next patrol\n",
			state.PRNumber, state.ReviewLoopIter)
		return NewSilentExit(4)
	}

	newIter := state.ReviewLoopIter + 1
	if err := mqRecordDispatch(refPrDispatchReviewFixMR, polecatName, newIter); err != nil {
		// Dangerous corner: gt sling already spawned the polecat (it's
		// "working" right now), but the bead doesn't record
		// review_fix_polecat. The next patrol cycle will see the bead as
		// idle and re-dispatch a SECOND polecat against the same PR —
		// two cooks in the same kitchen. Escalate hard so an operator
		// can clean up; this is a rare beads-lock-or-disk-failure case.
		// Iter counter is not advanced (we never wrote it), so the
		// re-dispatch is at least bounded by max-iter.
		threadsJSON, _ := json.Marshal(threads)
		_ = escalateReviewLoopCap(
			fmt.Sprintf("PR #%d: review-fix polecat %s spawned but MR-bead update FAILED (%v) — manual intervention required to prevent double-dispatch",
				state.PRNumber, polecatName, err),
			string(threadsJSON))
		return wrapOperationalErr(fmt.Errorf("recording dispatch on MR %s after polecat %s spawned: %w (escalation filed)",
			refPrDispatchReviewFixMR, polecatName, err))
	}
	fmt.Fprintf(os.Stdout, "%s PR #%d: dispatched review-fix polecat %s (iter=%d of %d)\n",
		style.Bold.Render("→"), state.PRNumber, polecatName, newIter, maxIter)
	return NewSilentExit(1)
}

// isReviewFixPolecatAlive shells out to `gt polecat status <rig>/<name>` and
// classifies the result against the lifecycle states defined in
// internal/polecat/types.go. Only the `working` state means "still doing
// review-fix work"; everything else (idle, done, stuck, stalled, zombie)
// signals the polecat is past the dispatch and the refinery should clear
// state + advance.
//
// Empty stdout from a successful exit is treated as "polecat not found"
// (gt polecat status returns empty JSON for unknown polecats). That maps
// to "terminal" — same handling as `idle`/`done`.
func isReviewFixPolecatAlive(rigName, polecatName string) (bool, error) {
	cmd := exec.Command(gtBinary(), "polecat", "status",
		fmt.Sprintf("%s/%s", rigName, polecatName), "--json")
	out, err := cmd.Output()
	if err != nil {
		// Don't auto-clear on lookup failure. The caller treats this as
		// "transient — try again next patrol" so we never lose track of an
		// in-flight polecat to a flaky status query.
		return false, fmt.Errorf("gt polecat status failed: %w", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		// Status succeeded but stdout empty — polecat unknown, treat as terminal.
		return false, nil
	}
	var resp struct {
		State string `json:"state"`
	}
	if jerr := json.Unmarshal([]byte(trimmed), &resp); jerr != nil {
		return false, fmt.Errorf("parsing polecat status JSON: %w", jerr)
	}
	return resp.State == "working", nil
}

// gtBinary returns the absolute path to the running `gt` binary so the
// shell-out calls invoke the same build the patrol is running. Falls back
// to the bare command when os.Executable fails (rare; the path lookup is a
// belt-and-braces measure).
func gtBinary() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "gt"
}

// slingReviewFixPolecat dispatches the polecat via `gt sling` and returns the
// spawned polecat's name. gt sling has no JSON output mode, so the name is read
// back from the dispatch bead's assignee after the hook write commits (see
// reviewFixPolecatNameFromBead). The mission text is built in buildReviewFixMission.
func slingReviewFixPolecat(rigName string, state dispatchReviewFixState, threads []refinery.ReviewThread, reviewer string) (string, error) {
	threadsJSON, jerr := json.MarshalIndent(threads, "", "  ")
	if jerr != nil {
		return "", fmt.Errorf("marshaling threads: %w", jerr)
	}
	mission := buildReviewFixMission(state.PRNumber, string(threadsJSON), reviewer)

	// Determine which bead to sling. A closed source bead (the normal state after
	// gt done) is refused by gt sling — so we create a fresh open bead in that
	// case. An open source bead takes the existing path.
	dispatchBeadID, err := resolveReviewFixDispatchBead(rigName, state)
	if err != nil {
		return "", err
	}

	// Divergence guard (review thread #2). The sling below passes --force, which
	// is necessary: with a bare-rig target, sling treats "already hooked to a
	// polecat in this rig" as an idempotent no-op and returns success WITHOUT
	// spawning anyone (matchesSlingTarget), so dropping --force would silently
	// fail to dispatch. But --force also triggers sling's reassign path
	// (LIFECYCLE:Shutdown + unhook) on the current holder. Stage 1 guarantees no
	// review-fix polecat is tracked in MR state, yet the source bead can still be
	// hooked to a polecat whose state diverges from MR state. If that holder is
	// actively WORKING, displacing it would disrupt a live session — so defer with
	// a retryable error instead. A terminal/idle/absent holder is safe to displace
	// (it's the normal case: the original polecat went terminal after gt done).
	//
	// The guard is skipped for a fresh dispatch bead: a newly created bead has no
	// holder, so there is nothing to protect and the check would be vacuous.
	if dispatchBeadID == state.SourceIssue {
		if held, holder, herr := reviewFixSourceHeldByWorkingPolecat(rigName, state.SourceIssue); herr != nil {
			return "", herr
		} else if held {
			return "", fmt.Errorf("source issue %s is held by working polecat %s/polecats/%s — deferring review-fix dispatch to avoid disrupting an active session",
				state.SourceIssue, rigName, holder)
		}
	}

	// Dispatch with the documented sling form: `gt sling <bead> <rig> ...`.
	// The bare rig target auto-spawns a fresh polecat; --review-pr/--review-branch
	// route it onto the existing PR branch (consumed by sling's
	// slingPR/slingBranchOverride). These flags are namespaced with the review-
	// prefix because upstream's resume feature owns the bare --pr/--branch (fork-sync).
	//
	// Earlier this passed a `review-fix/<issue-id>` pseudo-target plus a --json
	// flag, neither of which gt sling understands — cobra rejected the unknown
	// flag, printed its usage banner, and exited 1, so the review-fix loop never
	// dispatched anyone (hq-eew / gt-o4z).
	args := reviewFixSlingArgs(rigName, dispatchBeadID, state, mission)
	cmd := exec.Command(gtBinary(), args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Only fold captured output into the message when it's non-empty —
		// otherwise the error reads "gt sling failed: : exit status 1" (review
		// thread #1).
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			return "", fmt.Errorf("gt sling failed: %s: %w", trimmed, err)
		}
		return "", fmt.Errorf("gt sling failed: %w", err)
	}

	return reviewFixPolecatNameFromBead(rigName, dispatchBeadID)
}

// shouldCreateReviewFixBead reports whether dispatch-review-fix should create a
// fresh bead instead of slinging the original source bead. Returns true when the
// source bead is closed, tombstoned, or absent — the normal states for a
// completed PR after gt done (gt sling rejects both "closed" and "tombstone"
// beads with "work already completed"). Returns false for any other (still-live)
// status, meaning the bead can be slung directly.
func shouldCreateReviewFixBead(sourceBead *beads.Issue) bool {
	return sourceBead == nil || sourceBead.Status == "closed" || sourceBead.Status == "tombstone"
}

// resolveReviewFixDispatchBead returns the bead ID to sling for review-fix
// dispatch. Returns state.SourceIssue when that bead is still open (the original
// polecat may be idle but the bead is live). When the source bead is closed,
// tombstoned, or absent — the normal completed-PR case after gt done — it creates
// a fresh open bead in the rig and returns its ID, so gt sling has an open bead
// to hook.
func resolveReviewFixDispatchBead(rigName string, state dispatchReviewFixState) (string, error) {
	bd := beads.New(resolveBeadDir(state.SourceIssue))
	sourceBead, err := bd.Show(state.SourceIssue)
	// An absent bead (ErrNotFound) is the expected post-gt-done state, not a
	// failure: bd.Show returns (nil, ErrNotFound), which shouldCreateReviewFixBead
	// treats as "create a fresh bead". Only surface other (real) read errors.
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return "", fmt.Errorf("reading source bead %s: %w", state.SourceIssue, err)
	}
	if !shouldCreateReviewFixBead(sourceBead) {
		return state.SourceIssue, nil
	}
	// Source bead is closed, tombstoned, or absent (the normal post-gt-done
	// state). Create a fresh open bead so gt sling has something to hook. Embed
	// the structured
	// fields the review-fix polecat needs to orient itself on the PR branch.
	desc := fmt.Sprintf("review_pr: %d\nbranch: %s\nsource_issue: %s",
		state.PRNumber, state.Branch, state.SourceIssue)
	bdCreate := beads.New(resolveBeadDir(refPrDispatchReviewFixMR))
	fresh, cerr := bdCreate.Create(beads.CreateOptions{
		Title:       fmt.Sprintf("Review-fix: PR #%d", state.PRNumber),
		Labels:      []string{"gt:task"},
		Priority:    2,
		Description: desc,
		Rig:         rigName,
	})
	if cerr != nil {
		return "", fmt.Errorf("creating review-fix bead (source %s closed/tombstoned/absent): %w", state.SourceIssue, cerr)
	}
	return fresh.ID, nil
}

// reviewFixSourceHeldByWorkingPolecat reports whether the source issue is
// currently hooked to a same-rig polecat that is actively working. The dispatch
// uses this to avoid --force displacing a live session (review thread #2).
//
// Conservative on uncertainty: an unparseable/foreign/empty assignee means
// "nothing of ours to protect" (proceed), but a polecat whose liveness can't be
// determined is treated as held (defer) so we never shut down a session we
// simply failed to query.
func reviewFixSourceHeldByWorkingPolecat(rigName, sourceIssue string) (bool, string, error) {
	bd := beads.New(resolveBeadDir(sourceIssue))
	issue, err := bd.Show(sourceIssue)
	if err != nil {
		return false, "", fmt.Errorf("reading source issue %s before dispatch: %w", sourceIssue, err)
	}
	if issue == nil {
		return false, "", nil
	}
	name, perr := polecatNameFromAssignee(rigName, issue.Assignee)
	if perr != nil {
		// Unassigned, or held by a non-polecat / different-rig owner — nothing
		// for the review-fix sling to displace.
		return false, "", nil
	}
	alive, aerr := isReviewFixPolecatAlive(rigName, name)
	if aerr != nil {
		// Liveness query failed — assume the holder is live so we defer rather
		// than risk shutting down an active session on a flaky status lookup.
		return true, name, nil
	}
	return alive, name, nil
}

// reviewFixSlingArgs builds the argv (sans the gt binary) for the review-fix
// dispatch sling. Factored out so the command shape is unit-testable without
// shelling out: the regression that broke the whole loop lived entirely in
// these arguments.
//
// dispatchBeadID is the bead to sling — either the original source issue (when
// still open) or a fresh review-fix bead (when the source is closed). Callers
// must pass the resolved ID from resolveReviewFixDispatchBead; this function does
// not perform the open/closed check.
//
// --force avoids sling's bare-rig idempotency no-op (see the divergence guard
// in slingReviewFixPolecat) so the dispatch reliably spawns a fresh polecat
// even when the bead is still hooked to a terminal/idle prior owner. The caller
// has already confirmed the holder is not actively working before we get here,
// so the force never tears down a live session.
func reviewFixSlingArgs(rigName string, dispatchBeadID string, state dispatchReviewFixState, mission string) []string {
	return []string{
		"sling",
		dispatchBeadID,
		rigName,
		"--review-pr", fmt.Sprintf("%d", state.PRNumber),
		"--review-branch", state.Branch,
		"--args", mission,
		"--force",
	}
}

// reviewFixPolecatNameFromBead reads the just-slung bead and extracts the
// spawned polecat's short name from its assignee. gt sling commits the hook
// write (which sets the assignee) before it returns, so this read is race-free
// against a successful sling. beadID is whichever bead was passed to gt sling —
// the original source issue (open path) or a fresh review-fix bead (closed path).
func reviewFixPolecatNameFromBead(rigName, beadID string) (string, error) {
	bd := beads.New(resolveBeadDir(beadID))
	issue, err := bd.Show(beadID)
	if err != nil {
		return "", fmt.Errorf("reading bead %s after sling: %w", beadID, err)
	}
	if issue == nil {
		return "", fmt.Errorf("bead %s not found after sling", beadID)
	}
	return polecatNameFromAssignee(rigName, issue.Assignee)
}

// polecatNameFromAssignee pulls the short polecat name out of a
// `<rig>/polecats/<name>` assignee, validating it belongs to the expected rig.
func polecatNameFromAssignee(rigName, assignee string) (string, error) {
	assignee = strings.TrimSpace(assignee)
	prefix := rigName + "/polecats/"
	if !strings.HasPrefix(assignee, prefix) {
		return "", fmt.Errorf("post-sling assignee %q is not a %s polecat", assignee, rigName)
	}
	name := strings.TrimPrefix(assignee, prefix)
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("post-sling assignee %q has no usable polecat name", assignee)
	}
	return name, nil
}

// buildReviewFixMission renders the polecat's mission prompt. Imperative
// numbered steps — no narrative — so the LLM polecat reads a clear sequence
// rather than absorbing a wall of context. The threadsJSON is included
// verbatim (G14 invariant: do not paraphrase, do not drop threads).
func buildReviewFixMission(prNumber int, threadsJSON, reviewer string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a review-fix polecat for PR #%d. Address every unresolved thread below.\n\n", prNumber)
	b.WriteString("Mission steps (run in order; the formula's polecat-side handler wires `gt prime` and `bd prime` for you):\n\n")
	b.WriteString("1. `gt polecat checkout-branch <bead-id>` — enter the existing PR branch. The tap-guard blocks raw `git checkout -b`; this subcommand is the permitted path. Idempotent if you're already on the target branch; refuses to silently swap from a different polecat branch.\n\n")
	b.WriteString("2. For EACH thread in the JSON below: apply the smallest fix that resolves the comment. No scope creep. No \"while I'm here\" refactors.\n\n")
	b.WriteString("3. Stage explicitly: `git add <each-edited-path>` for each file you touched. Do NOT use `git add -A` — it would drag any incidental untracked files (test scratch, editor cruft, runtime artifacts) into the commit and silently violate the no-scope-creep contract. Then `git commit -m \"<descriptive message naming the threads addressed>\"`.\n\n")
	b.WriteString("4. `git push --force-with-lease origin <branch-name-from-step-1>`. The polecat namespace is yours; the actor-boundary principle permits force-push here.\n\n")
	b.WriteString("5. **Resolve every thread you addressed via the GraphQL `resolveReviewThread` mutation** — `gh api graphql -f query='mutation { resolveReviewThread(input: {threadId: \"...\"}) { thread { id isResolved } } }'`. Thread resolution is GraphQL-only on GitHub — there is no REST endpoint. Reply-only is not enough; gastown has no auto-resolve and the refinery's review loop cannot converge while any thread stays unresolved (G13 failure mode).\n\n")
	b.WriteString("6. Verify with `gt refinery pr threads <PR-number> --unresolved --json` (substitute the actual PR number, not a shell variable). Confirm the result is an empty array.\n\n")
	b.WriteString("7. `gt done` — the witness/refinery handshake takes over. `gt done` updates the MR bead's `commit_sha` to the new HEAD; the refinery's next `await-review` cycle detects the SHA drift, clears the wait timer, and re-posts the trigger to wake ")
	if reviewer != "" {
		fmt.Fprintf(&b, "%s (the configured pr_reviewer)", reviewer)
	} else {
		b.WriteString("the configured pr_reviewer")
	}
	b.WriteString(" against the new HEAD.\n\n")
	b.WriteString("Author-agnostic: address every thread regardless of opener (gemini, augment, human — all count). Do not filter, do not paraphrase the JSON below into your own list.\n\n")
	b.WriteString("THREADS (unresolved, fresh poll at dispatch time):\n")
	b.WriteString(threadsJSON)
	return b.String()
}

// mqClearReviewState clears review_fix_polecat AND await_review_started_at
// on the MR bead — the same combination the formula's bash blob uses on the
// polecat-done branch. Done in one shell-out so we get the lock-once shape
// gt mq set-review-state already provides.
func mqClearReviewState(mrID string) error {
	cmd := exec.Command(gtBinary(), "mq", "set-review-state", mrID,
		"--clear-polecat",
		"--clear-await-started-at",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gt mq set-review-state --clear-polecat failed: %s: %w",
			strings.TrimSpace(string(out)), err)
	}
	return nil
}

// mqRecordDispatch stamps review_fix_polecat + review_loop_iter on the MR
// bead atomically (gt mq set-review-state holds the bead lock for the
// whole read-modify-write so it never clobbers concurrent edits to other
// MR fields). Called after `gt sling` reports a successful dispatch.
func mqRecordDispatch(mrID, polecatName string, iter int) error {
	cmd := exec.Command(gtBinary(), "mq", "set-review-state", mrID,
		"--polecat", polecatName,
		"--iter", fmt.Sprintf("%d", iter),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gt mq set-review-state failed: %s: %w",
			strings.TrimSpace(string(out)), err)
	}
	return nil
}

// wrapOperationalErr returns a SilentExit(4) carrying the error's message
// for stderr, distinguishing operational/config failures from the wait/
// advance/escalate exit codes (1/0/3) the patrol formula's `case` block
// dispatches on. Without this, a normal returned error maps to exit 1
// (cobra's default), which the formula reads as "still waiting" — the
// caller would silently spin on a config-shape problem instead of
// surfacing it. The original error message is preserved on stderr via the
// SilentExitError wrapping in cli.go.
func wrapOperationalErr(err error) error {
	if err == nil {
		return nil
	}
	fmt.Fprintln(os.Stderr, err.Error())
	return NewSilentExit(4)
}

// escalateReviewLoopCap shells out to `gt escalate` to file an escalation
// bead and route mail when the review-fix loop exceeds its per-PR cap.
// Mirrors the formula's existing `gt escalate -s HIGH ... --mail mayor/`
// call so the output path is identical.
func escalateReviewLoopCap(description, contextStr string) error {
	cmd := exec.Command(gtBinary(), "escalate",
		"-s", "HIGH",
		"--mail", "mayor/",
		"--context", contextStr,
		description,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Compile-time silencer for time.Time so test harnesses that import this
// file don't fail under -unused. The helper is a no-op.
var _ = time.Time{}
