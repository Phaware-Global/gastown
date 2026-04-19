package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
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

func init() {
	refineryCmd.AddCommand(refineryPrCmd)
	refineryPrCmd.AddCommand(refineryPrCreateCmd)
	refineryPrCmd.AddCommand(refineryPrWaitCICmd)
	refineryPrCmd.AddCommand(refineryPrRequestReviewCmd)
	refineryPrCmd.AddCommand(refineryPrThreadsCmd)
	refineryPrCmd.AddCommand(refineryPrWaitApprovalCmd)
	refineryPrCmd.AddCommand(refineryPrMergeCmd)

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
}

// getRefineryPRContext builds the PRProvider for the rig inferred from cwd.
// Returns the provider + the rig-relative git working directory for diagnostics.
func getRefineryPRContext() (refinery.PRProvider, *rig.Rig, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigName, err := inferRigFromCwd(townRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("could not determine rig: %w", err)
	}
	_, r, err := getRig(rigName)
	if err != nil {
		return nil, nil, err
	}

	eng := refinery.NewEngineer(r)
	if lerr := eng.LoadConfig(); lerr != nil {
		return nil, nil, fmt.Errorf("loading rig merge_queue config: %w", lerr)
	}
	provider, err := eng.PRProvider()
	if err != nil {
		return nil, nil, err
	}
	if provider == nil {
		return nil, nil, fmt.Errorf("no PR provider for rig %s (merge_strategy=%q)", rigName, eng.Config().MergeStrategy)
	}
	return provider, r, nil
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

	provider, _, err := getRefineryPRContext()
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
	provider, _, err := getRefineryPRContext()
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
	provider, _, err := getRefineryPRContext()
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
	provider, _, err := getRefineryPRContext()
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
	provider, _, err := getRefineryPRContext()
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
	provider, _, err := getRefineryPRContext()
	if err != nil {
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

// Keep the git dependency referenced so `go vet` doesn't complain about unused
// imports when the file is edited during development.
var _ = git.NewGit

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
