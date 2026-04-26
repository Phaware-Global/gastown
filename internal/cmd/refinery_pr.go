package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// refinery_pr.go: thin wrappers that expose PRProvider operations as CLI
// subcommands. The refinery patrol formula drives these from bash — the Go
// command layer keeps the provider-specific quirks (gh api graphql for
// resolved threads, gh pr view --json for rollup) out of the formula text.

var (
	// gt refinery pr create
	refPrCreateBranch   string
	refPrCreateBase     string
	refPrCreateTitle    string
	refPrCreateBody     string
	refPrCreateBodyFile string
	refPrCreateJSON     bool

	// gt refinery pr wait-ci
	refPrWaitCITimeout  time.Duration
	refPrWaitCIInterval time.Duration

	// gt refinery pr request-review
	refPrRequestReviewers []string

	// gt refinery pr threads
	refPrThreadsUnresolved bool
	refPrThreadsJSON       bool

	// gt refinery pr wait-approval
	refPrWaitApproverUser string
	refPrWaitApprovalMin  int
	refPrWaitApprovalTO   time.Duration
	refPrWaitApprovalInt  time.Duration
	refPrWaitApprovalEsc  bool

	// gt refinery pr merge
	refPrMergeMethod string

	// gt refinery pr await-review
	refPrAwaitReviewer       string
	refPrAwaitTriggerComment string
	refPrAwaitWait           time.Duration
	refPrAwaitTimeout        time.Duration
	refPrAwaitNoTrigger      bool
	refPrAwaitMR             string
)

var refineryPrCmd = &cobra.Command{
	Use:   "pr",
	Short: "PR primitives used by the refinery patrol formula",
	Long: `Thin wrappers around the configured VCS provider.

These commands are invoked from the refinery patrol formula when a rig has
merge_strategy=pr. They encapsulate the provider-specific details
(gh CLI flags, graphql queries, review-thread resolution) so the formula
can orchestrate without embedding gh plumbing.

All subcommands run in the current rig (inferred from cwd).`,
	RunE: requireSubcommand,
}

var refineryPrCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a PR (idempotent — returns existing PR if already open)",
	Long: `Create a pull request for the given branch against base.

If an open PR already exists for --branch, returns its number and URL
without creating a new one.

Examples:
  gt refinery pr create --branch polecat/nux --base main \
    --title "Add widget (gt-abc)" --body "Closes gt-abc"

  gt refinery pr create --branch polecat/nux --base main \
    --title "Add widget (gt-abc)" --body-file /tmp/pr-body.md --json`,
	RunE: runRefineryPrCreate,
}

var refineryPrWaitCICmd = &cobra.Command{
	Use:   "wait-ci <pr-number>",
	Short: "Block until CI checks on the PR reach a terminal state",
	Args:  cobra.ExactArgs(1),
	RunE:  runRefineryPrWaitCI,
}

var refineryPrRequestReviewCmd = &cobra.Command{
	Use:   "request-review <pr-number>",
	Short: "Request reviews from the given users/teams",
	Args:  cobra.ExactArgs(1),
	RunE:  runRefineryPrRequestReview,
}

var refineryPrThreadsCmd = &cobra.Command{
	Use:   "threads <pr-number>",
	Short: "List review threads on a PR (default: unresolved only)",
	Args:  cobra.ExactArgs(1),
	RunE:  runRefineryPrThreads,
}

var refineryPrWaitApprovalCmd = &cobra.Command{
	Use:   "wait-approval <pr-number>",
	Short: "Block until the PR is approved by the configured approver",
	Args:  cobra.ExactArgs(1),
	RunE:  runRefineryPrWaitApproval,
}

var refineryPrMergeCmd = &cobra.Command{
	Use:   "merge <pr-number>",
	Short: "Merge the PR via the VCS provider API",
	Args:  cobra.ExactArgs(1),
	RunE:  runRefineryPrMerge,
}

var refineryPrAwaitReviewCmd = &cobra.Command{
	Use:   "await-review <pr-number>",
	Short: "Post the reviewer trigger, then enforce wait + thread gate (one-shot, patrol-resumable)",
	Long: `Run one cycle of the imperative review-wait gate on a PR.

Single-shot, NOT a blocking poll: each invocation takes at most one
action and returns immediately. State (when the trigger was posted)
lives on the MR bead's description so the patrol formula re-enters
on each cycle without busy-waiting. This keeps the merge queue
unblocked while one PR awaits its reviewer.

State machine:
  - first call (no await_review_started_at on the MR bead) → post the
    trigger comment, record the timestamp, exit 1 (still waiting).
  - subsequent call inside --wait window → exit 1 (still waiting).
  - subsequent call past --wait, threads clean, reviewer engaged →
    exit 0 (advance).
  - threads unresolved at any point past --wait → exit 2 (review-fix
    loop required).
  - --timeout elapsed with reviewer still absent → exit 3 (escalate).

Exit codes:
  0  ready to advance
  1  still waiting (post-trigger or inside the wait window)
  2  unresolved threads — caller dispatches review-fix
  3  reviewer never engaged — caller escalates
  4  operational/config error — caller escalates (does NOT silently retry)`,
	Args: cobra.ExactArgs(1),
	RunE: runRefineryPrAwaitReview,
}

func init() {
	refineryCmd.AddCommand(refineryPrCmd)
	refineryPrCmd.AddCommand(refineryPrCreateCmd)
	refineryPrCmd.AddCommand(refineryPrWaitCICmd)
	refineryPrCmd.AddCommand(refineryPrRequestReviewCmd)
	refineryPrCmd.AddCommand(refineryPrThreadsCmd)
	refineryPrCmd.AddCommand(refineryPrWaitApprovalCmd)
	refineryPrCmd.AddCommand(refineryPrMergeCmd)
	refineryPrCmd.AddCommand(refineryPrAwaitReviewCmd)

	refineryPrCreateCmd.Flags().StringVar(&refPrCreateBranch, "branch", "", "head branch (required)")
	refineryPrCreateCmd.Flags().StringVar(&refPrCreateBase, "base", "main", "target branch")
	refineryPrCreateCmd.Flags().StringVar(&refPrCreateTitle, "title", "", "PR title (required)")
	refineryPrCreateCmd.Flags().StringVar(&refPrCreateBody, "body", "", "PR body")
	refineryPrCreateCmd.Flags().StringVar(&refPrCreateBodyFile, "body-file", "", "read PR body from file")
	refineryPrCreateCmd.Flags().BoolVar(&refPrCreateJSON, "json", false, "output {number,url} as JSON")

	refineryPrWaitCICmd.Flags().DurationVar(&refPrWaitCITimeout, "timeout", 15*time.Minute, "give up after this duration")
	refineryPrWaitCICmd.Flags().DurationVar(&refPrWaitCIInterval, "interval", 30*time.Second, "poll interval")

	refineryPrRequestReviewCmd.Flags().StringSliceVar(&refPrRequestReviewers, "user", nil, "reviewer username (repeatable)")

	refineryPrThreadsCmd.Flags().BoolVar(&refPrThreadsUnresolved, "unresolved", true, "show only unresolved threads")
	refineryPrThreadsCmd.Flags().BoolVar(&refPrThreadsJSON, "json", true, "output JSON (default)")

	refineryPrWaitApprovalCmd.Flags().StringVar(&refPrWaitApproverUser, "approver", "", "GitHub user whose approval gates the merge")
	refineryPrWaitApprovalCmd.Flags().IntVar(&refPrWaitApprovalMin, "min-approvals", 1, "minimum approving reviews required")
	refineryPrWaitApprovalCmd.Flags().DurationVar(&refPrWaitApprovalTO, "timeout", 24*time.Hour, "give up after this duration")
	refineryPrWaitApprovalCmd.Flags().DurationVar(&refPrWaitApprovalInt, "interval", 60*time.Second, "poll interval")
	refineryPrWaitApprovalCmd.Flags().BoolVar(&refPrWaitApprovalEsc, "escalate", false, "open an escalation on first call (reserved; Phase 3)")

	refineryPrMergeCmd.Flags().StringVar(&refPrMergeMethod, "method", "squash", "merge method: squash, merge, or rebase")

	refineryPrAwaitReviewCmd.Flags().StringVar(&refPrAwaitMR, "mr", "",
		"MR bead ID for state persistence (required for patrol-resumable use; "+
			"omit only for one-off CLI debugging — without --mr each call posts a fresh trigger)")
	refineryPrAwaitReviewCmd.Flags().StringVar(&refPrAwaitReviewer, "reviewer", "",
		"GitHub user whose review must land (defaults to rig merge_queue.pr_reviewer)")
	refineryPrAwaitReviewCmd.Flags().StringVar(&refPrAwaitTriggerComment, "trigger-comment", "",
		"body to post as the review trigger (defaults to rig merge_queue.pr_trigger_comment or \"augment review\")")
	refineryPrAwaitReviewCmd.Flags().DurationVar(&refPrAwaitWait, "wait", 0,
		"min wait between trigger and first check (defaults to rig merge_queue.pr_review_wait or 5m)")
	refineryPrAwaitReviewCmd.Flags().DurationVar(&refPrAwaitTimeout, "timeout", 0,
		"max total wait before escalating (defaults to rig merge_queue.pr_review_timeout or 30m)")
	refineryPrAwaitReviewCmd.Flags().BoolVar(&refPrAwaitNoTrigger, "no-trigger", false,
		"skip posting the trigger comment (use when request-review already triggered the bot)")
}

// getRefineryPRContext builds the PRProvider for the rig inferred from cwd.
// Returns the provider + the rig-level MergeQueueConfig (needed for approval
// gate enforcement in the CLI merge path, G21 fix) + the *rig.Rig handle
// (used by callers that need rig metadata beyond MergeQueue).
func getRefineryPRContext() (refinery.PRProvider, *refinery.MergeQueueConfig, *rig.Rig, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigName, err := inferRigFromCwd(townRoot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not determine rig: %w", err)
	}
	_, r, err := getRig(rigName)
	if err != nil {
		return nil, nil, nil, err
	}

	eng := refinery.NewEngineer(r)
	if lerr := eng.LoadConfig(); lerr != nil {
		return nil, nil, nil, fmt.Errorf("loading rig merge_queue config: %w", lerr)
	}
	provider, err := eng.PRProvider()
	if err != nil {
		return nil, nil, nil, err
	}
	if provider == nil {
		return nil, nil, nil, fmt.Errorf("no PR provider for rig %s (merge_strategy=%q)", rigName, eng.Config().MergeStrategy)
	}
	return provider, eng.Config(), r, nil
}

func parsePRNumber(arg string) (int, error) {
	raw := arg
	arg = strings.TrimPrefix(arg, "#")
	// strconv.Atoi rejects trailing garbage (unlike fmt.Sscanf("%d", ...)),
	// so "123abc" fails fast instead of being silently treated as PR 123.
	n, err := strconv.Atoi(arg)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid PR number %q", raw)
	}
	return n, nil
}

func runRefineryPrCreate(cmd *cobra.Command, args []string) error {
	if refPrCreateBranch == "" {
		return fmt.Errorf("--branch is required")
	}
	if refPrCreateTitle == "" {
		return fmt.Errorf("--title is required")
	}
	body := refPrCreateBody
	if refPrCreateBodyFile != "" {
		data, err := os.ReadFile(refPrCreateBodyFile) //nolint:gosec // path comes from caller
		if err != nil {
			return fmt.Errorf("reading --body-file: %w", err)
		}
		body = string(data)
	}

	provider, _, _, err := getRefineryPRContext()
	if err != nil {
		return err
	}

	number, url, err := provider.CreatePR(refinery.CreatePROptions{
		Branch: refPrCreateBranch,
		Base:   refPrCreateBase,
		Title:  refPrCreateTitle,
		Body:   body,
	})
	if err != nil {
		return err
	}

	if refPrCreateJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"number": number, "url": url})
	}
	fmt.Fprintf(os.Stdout, "%d\t%s\n", number, url)
	return nil
}

func runRefineryPrWaitCI(cmd *cobra.Command, args []string) error {
	prNumber, err := parsePRNumber(args[0])
	if err != nil {
		return err
	}
	provider, _, _, err := getRefineryPRContext()
	if err != nil {
		return err
	}

	deadline := time.Now().Add(refPrWaitCITimeout)
	for {
		state, done, err := provider.ChecksRollup(prNumber)
		if err != nil {
			return fmt.Errorf("polling checks: %w", err)
		}
		if done {
			fmt.Fprintf(os.Stdout, "%s\n", state)
			if state == "SUCCESS" {
				return nil
			}
			return fmt.Errorf("CI not green: %s", state)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for CI (last state: %s)", state)
		}
		time.Sleep(refPrWaitCIInterval)
	}
}

func runRefineryPrRequestReview(cmd *cobra.Command, args []string) error {
	prNumber, err := parsePRNumber(args[0])
	if err != nil {
		return err
	}
	if len(refPrRequestReviewers) == 0 {
		return fmt.Errorf("at least one --user is required")
	}
	provider, _, _, err := getRefineryPRContext()
	if err != nil {
		return err
	}
	if err := provider.RequestReview(prNumber, refPrRequestReviewers); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "%s Review requested on PR #%d from %s\n",
		style.Bold.Render("✓"), prNumber, strings.Join(refPrRequestReviewers, ", "))
	return nil
}

func runRefineryPrThreads(cmd *cobra.Command, args []string) error {
	prNumber, err := parsePRNumber(args[0])
	if err != nil {
		return err
	}
	provider, _, _, err := getRefineryPRContext()
	if err != nil {
		return err
	}

	var threads []refinery.ReviewThread
	if refPrThreadsUnresolved {
		threads, err = provider.UnresolvedThreads(prNumber)
	} else {
		threads, err = provider.AllThreads(prNumber)
	}
	if err != nil {
		return err
	}

	if refPrThreadsJSON {
		return json.NewEncoder(os.Stdout).Encode(threads)
	}
	if len(threads) == 0 {
		if refPrThreadsUnresolved {
			fmt.Fprintln(os.Stdout, "no unresolved threads")
		} else {
			fmt.Fprintln(os.Stdout, "no review threads")
		}
		return nil
	}
	for _, t := range threads {
		fmt.Fprintf(os.Stdout, "%s\t%s:%d\t%s\n", t.URL, t.Path, t.Line, firstLine(t.Body))
	}
	return nil
}

func runRefineryPrWaitApproval(cmd *cobra.Command, args []string) error {
	prNumber, err := parsePRNumber(args[0])
	if err != nil {
		return err
	}
	approver := refPrWaitApproverUser
	minApprovals := refPrWaitApprovalMin
	if minApprovals < 0 {
		return fmt.Errorf("--min-approvals must be non-negative, got %d", minApprovals)
	}
	// Require at least one active gate so callers can't accidentally bypass
	// the approval wait by omitting --approver and setting --min-approvals=0
	// (or letting it default to 0). An explicit "no gates" configuration is
	// almost always a scripting error.
	if approver == "" && minApprovals == 0 {
		return fmt.Errorf("wait-approval requires at least one gate: " +
			"set --approver=<user> and/or --min-approvals=<n≥1>")
	}
	provider, _, _, err := getRefineryPRContext()
	if err != nil {
		return err
	}

	// Phase 1: --escalate is reserved; escalation wiring lands in Phase 3
	// when gt escalate integration is done. Silently ignore for now so the
	// formula can pass the flag and the command works.
	_ = refPrWaitApprovalEsc

	// Two gates, both must pass to return success:
	//   1. --approver set  → that specific user must have an active APPROVED
	//   2. --min-approvals → distinct APPROVED reviewers count >= min
	deadline := time.Now().Add(refPrWaitApprovalTO)
	for {
		approverOK := true
		if approver != "" {
			approverOK, err = provider.IsPRApprovedBy(prNumber, approver)
			if err != nil {
				return fmt.Errorf("polling approval by %s: %w", approver, err)
			}
		}

		countOK := true
		var count int
		if minApprovals > 0 {
			count, err = provider.CountApprovals(prNumber)
			if err != nil {
				return fmt.Errorf("counting approvals: %w", err)
			}
			countOK = count >= minApprovals
		}

		if approverOK && countOK {
			// The no-gate case is unreachable — we reject it above.
			switch {
			case approver != "" && minApprovals > 0:
				fmt.Fprintf(os.Stdout, "%s PR #%d approved by %s (%d/%d total approvals)\n",
					style.Bold.Render("✓"), prNumber, approver, count, minApprovals)
			case approver != "":
				fmt.Fprintf(os.Stdout, "%s PR #%d approved by %s\n",
					style.Bold.Render("✓"), prNumber, approver)
			case minApprovals > 0:
				fmt.Fprintf(os.Stdout, "%s PR #%d has %d/%d required approvals\n",
					style.Bold.Render("✓"), prNumber, count, minApprovals)
			}
			return nil
		}

		if time.Now().After(deadline) {
			var parts []string
			if approver != "" && !approverOK {
				parts = append(parts, fmt.Sprintf("approval from %s pending", approver))
			}
			if minApprovals > 0 && !countOK {
				parts = append(parts, fmt.Sprintf("have %d/%d approvals", count, minApprovals))
			}
			return fmt.Errorf("timeout waiting for approval: %s", strings.Join(parts, ", "))
		}
		time.Sleep(refPrWaitApprovalInt)
	}
}

func runRefineryPrMerge(cmd *cobra.Command, args []string) error {
	prNumber, err := parsePRNumber(args[0])
	if err != nil {
		return err
	}
	provider, cfg, _, err := getRefineryPRContext()
	if err != nil {
		return err
	}

	// Unresolved threads block merge, checked BEFORE the approval gate
	// so the refinery LLM sees the thread list first — if threads are
	// outstanding, fixing them often produces the missing approval as a
	// side effect, whereas approving without thread resolution lets
	// reviewer guidance slip through to main.
	if err := refinery.VerifyReviewThreadsResolved(provider, prNumber, nil); err != nil {
		var needsResolution *refinery.NeedsReviewResolutionError
		if errors.As(err, &needsResolution) {
			return fmt.Errorf("%w\nResolve each thread (fix the issue + mark resolved on GitHub) "+
				"before retrying `gt refinery pr merge`. The refinery patrol formula's PR.5 "+
				"review-fix loop exists for this — do not merge around unresolved threads", err)
		}
		return err
	}

	// G21 fix: enforce the same approval gate the patrol formula asserts at
	// PR.6 (wait-approval). Before this check, the CLI subcommand called
	// provider.MergePR() directly — making `gt refinery pr merge <n>` a
	// one-step review bypass if the refinery LLM reached it without running
	// request-review / wait-approval first. Sharing VerifyPRApproval with
	// doMergePR ensures both paths reject on the same gates.
	//
	// Rigs that leave both PRApprover and PRRequiredApprovals unconfigured
	// get no gate (opt-in) — existing behavior for rigs that haven't
	// adopted the approval policy is preserved.
	if err := refinery.VerifyPRApproval(provider, cfg, prNumber, nil); err != nil {
		var needsApproval *refinery.NeedsApprovalError
		if errors.As(err, &needsApproval) {
			return fmt.Errorf("%w\n\n"+
				"The refinery patrol formula requires `gt refinery pr await-review` (PR.4) "+
				"and `gt refinery pr wait-approval` (PR.6) to run between pr-create and pr-merge. "+
				"If PR #%d is genuinely ready to land, re-run those steps; if this is an operator "+
				"adoption path for an orphan branch, merge via `gh pr merge` on the CLI outside "+
				"the refinery session", err, prNumber)
		}
		return err
	}

	sha, err := provider.MergePR(prNumber, refPrMergeMethod)
	if err != nil {
		return err
	}
	if sha == "" {
		sha = "<unknown>"
	}
	fmt.Fprintf(os.Stdout, "%s PR #%d merged (%s): %s\n",
		style.Bold.Render("✓"), prNumber, refPrMergeMethod, sha)
	return nil
}

func runRefineryPrAwaitReview(cmd *cobra.Command, args []string) error {
	// Two-tier exit code scheme so the formula can distinguish "still
	// waiting" (retry next patrol) from "operational/config error"
	// (escalate). awaitReviewInner returns regular errors and
	// SilentExitErrors for the known status codes (1/2/3); this wrapper
	// catches non-silent errors and converts them to exit 4 with the
	// underlying message printed to stderr. Without this split, exit 1
	// would conflate AwaitStatusWaiting with a config-load failure and
	// the patrol would silently retry an unrecoverable failure forever.
	err := awaitReviewInner(args)
	if err == nil {
		return nil
	}
	if _, isSilent := IsSilentExit(err); isSilent {
		return err
	}
	fmt.Fprintf(os.Stderr, "Error: await-review (operational): %v\n", err)
	return NewSilentExit(4)
}

func awaitReviewInner(args []string) error {
	prNumber, err := parsePRNumber(args[0])
	if err != nil {
		return err
	}
	provider, cfg, _, err := getRefineryPRContext()
	if err != nil {
		return err
	}

	reviewer := firstNonEmpty(refPrAwaitReviewer, cfg.PRReviewer)
	if reviewer == "" {
		return fmt.Errorf("await-review: --reviewer required (or set merge_queue.pr_reviewer in rig config)")
	}
	triggerComment := firstNonEmpty(refPrAwaitTriggerComment, cfg.PRTriggerComment)
	if refPrAwaitNoTrigger {
		triggerComment = ""
	}
	// Reject negative --wait / --timeout explicitly. firstNonZero would
	// happily pass a negative value through, which would either make
	// elapsed >= MinWait trivially true (defeating the min-wait gate
	// the formula relies on, reintroducing the original race) or push
	// the timeout into the past.
	if refPrAwaitWait < 0 {
		return fmt.Errorf("await-review: --wait must be non-negative, got %v", refPrAwaitWait)
	}
	if refPrAwaitTimeout < 0 {
		return fmt.Errorf("await-review: --timeout must be non-negative, got %v", refPrAwaitTimeout)
	}
	wait := firstNonZero(refPrAwaitWait, cfg.PRReviewWait)
	timeout := firstNonZero(refPrAwaitTimeout, cfg.PRReviewTimeout)
	if timeout > 0 && wait > 0 && timeout <= wait {
		return fmt.Errorf(
			"await-review: timeout (%v) must be greater than wait (%v); "+
				"otherwise the timeout fires inside the min-wait window",
			timeout, wait)
	}

	startedAt, mrBd, err := readAwaitReviewState(refPrAwaitMR)
	if err != nil {
		return err
	}

	// G37: pass the PR's CURRENT head SHA into AwaitReviewStep so the
	// reviewer-engagement check is SHA-scoped. Querying the provider
	// (rather than reading the MR bead's commit_sha field) sidesteps G36
	// — bead state can drift after a force-push, while the upstream PR
	// always has the live HEAD. Tolerate ErrUnsupported (Bitbucket) by
	// passing an empty SHA, which retains the legacy "any review counts"
	// semantics; refinery-pr-workflow.md §G37 marks SHA-scoping as
	// load-bearing only for the GitHub path that augment runs on.
	headSHA, sErr := provider.CurrentHeadSHA(prNumber)
	if sErr != nil && !errors.Is(sErr, refinery.ErrUnsupported) {
		return fmt.Errorf("await-review: fetching current head SHA: %w", sErr)
	}

	res, err := refinery.AwaitReviewStep(provider, prNumber, refinery.AwaitReviewStepInput{
		Reviewer:       reviewer,
		TriggerComment: triggerComment,
		MinWait:        wait,
		Timeout:        timeout,
		StartedAt:      startedAt,
		HeadSHA:        headSHA,
	})
	if err != nil {
		return err
	}

	if !res.NewStartedAt.IsZero() {
		if err := writeAwaitReviewStartedAt(mrBd, refPrAwaitMR, res.NewStartedAt); err != nil {
			return fmt.Errorf("persisting await_review_started_at to MR %s: %w", refPrAwaitMR, err)
		}
	}

	refinery.EmitAwaitReviewProgress(os.Stdout, res)

	switch res.Status {
	case refinery.AwaitStatusReady:
		fmt.Fprintf(os.Stdout, "%s PR #%d review gate satisfied (reviewer=%s)\n",
			style.Bold.Render("✓"), prNumber, reviewer)
		return nil
	case refinery.AwaitStatusTriggerPosted, refinery.AwaitStatusWaiting:
		return NewSilentExit(1)
	case refinery.AwaitStatusNeedsResolution:
		fmt.Fprintf(os.Stdout, "%d unresolved review thread(s) — run the review-fix loop\n",
			len(res.UnresolvedThreads))
		for i, t := range res.UnresolvedThreads {
			loc := t.URL
			if t.Path != "" {
				loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
			}
			prio := ""
			if t.Priority != "" {
				prio = fmt.Sprintf("[%s] ", strings.ToUpper(t.Priority))
			}
			fmt.Fprintf(os.Stdout, "  %d. %s%s@%s — %s\n", i+1, prio, t.Author, loc, t.Preview)
		}
		return NewSilentExit(2)
	case refinery.AwaitStatusTimedOut:
		fmt.Fprintf(os.Stdout,
			"reviewer %q never engaged — escalate; do NOT proceed to merge\n", reviewer)
		return NewSilentExit(3)
	default:
		return fmt.Errorf("await-review: unrecognized status %d", res.Status)
	}
}

// readAwaitReviewState reads the persisted await_review_started_at
// timestamp from the MR bead, returning a zero time when no MR is
// passed or the field is unset/empty. The bd handle is returned so
// the caller can pass it back to writeAwaitReviewStartedAt without
// re-resolving the bead directory.
func readAwaitReviewState(mrID string) (time.Time, *beads.Beads, error) {
	if mrID == "" {
		return time.Time{}, nil, nil
	}
	if err := validateBeadIDShape(mrID); err != nil {
		return time.Time{}, nil, fmt.Errorf("invalid --mr %q: %w", mrID, err)
	}
	bd := beads.New(resolveBeadDir(mrID))
	issue, err := bd.Show(mrID)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("loading MR bead %s: %w", mrID, err)
	}
	if issue == nil {
		return time.Time{}, nil, fmt.Errorf("MR bead %s not found", mrID)
	}
	fields := beads.ParseMRFields(issue)
	if fields == nil || fields.AwaitReviewStartedAt == "" {
		return time.Time{}, bd, nil
	}
	t, perr := time.Parse(time.RFC3339, fields.AwaitReviewStartedAt)
	if perr != nil {
		return time.Time{}, nil, fmt.Errorf(
			"MR %s has invalid await_review_started_at %q: %w",
			mrID, fields.AwaitReviewStartedAt, perr)
	}
	return t, bd, nil
}

func writeAwaitReviewStartedAt(bd *beads.Beads, mrID string, t time.Time) error {
	if bd == nil || mrID == "" {
		return nil
	}
	unlock, err := bd.LockBead(mrID)
	if err != nil {
		return fmt.Errorf("acquiring bead lock: %w", err)
	}
	defer unlock()

	// Re-read under the lock so we don't clobber a concurrent writer's
	// changes to other MR fields (review_loop_iter, review_fix_polecat,
	// etc.) — read-modify-write needs to see the latest snapshot.
	latest, err := bd.Show(mrID)
	if err != nil {
		return fmt.Errorf("re-reading MR under lock: %w", err)
	}
	if latest == nil {
		return fmt.Errorf("MR %s vanished under lock", mrID)
	}
	latestFields := beads.ParseMRFields(latest)
	if latestFields == nil {
		latestFields = &beads.MRFields{}
	}
	latestFields.AwaitReviewStartedAt = t.UTC().Format(time.RFC3339)

	newDesc := beads.SetMRFields(latest, latestFields)
	return bd.Update(mrID, beads.UpdateOptions{Description: &newDesc})
}

// Keep the git dependency referenced so `go vet` doesn't complain about unused
// imports when the file is edited during development.
var _ = git.NewGit

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(vals ...time.Duration) time.Duration {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
