package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/reviewer"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Reviewer command flags.
var (
	reviewerPostPR          int
	reviewerPostFindings    string
	reviewerPostSHA         string
	reviewerCheckoutSHA     string
	reviewerPerspectiveShow string
)

var reviewerCmd = &cobra.Command{
	Use:     "reviewer",
	GroupID: GroupAgents,
	Short:   "Rig-level on-demand code reviewer (replaces Augment)",
	Long: `Manage the rig-level Reviewer role.

The Reviewer performs AI code review on pull requests, dispatched by the
Refinery (merge-queue PRs) and by crew members (their own PRs). It checks out
the PR head, reviews from configurable perspectives, and posts its findings as a
single GitHub review with inline comment threads under a dedicated machine-user
identity.

The Reviewer never approves and never merges — human approval is the merge gate.
Posting goes exclusively through ` + "`gt reviewer post`" + `; raw ` + "`gh pr review`" + ` is
tap-guard-blocked.`,
}

var reviewerPostCmd = &cobra.Command{
	Use:   "post",
	Short: "Post a review (one COMMENT review with inline finding threads)",
	Long: `Post a code review to a PR from a findings JSON payload.

This is the ONLY sanctioned review-posting path. It submits a single review
(event=COMMENT) anchored to the reviewed head SHA, with one inline thread per
finding. Each finding body carries a neutral shields.io priority badge and a
[perspective] tag so the refinery's review-fix loop and human reviewers can act
on it.

The findings file (--findings, or "-" for stdin) is a JSON object:

  {
    "summary": "per-perspective verdicts + counts",
    "reviewed_sha": "<optional; --sha overrides>",
    "findings": [
      {"path": "internal/foo.go", "line": 42, "priority": "high",
       "perspective": "adversarial", "title": "nil deref",
       "body": "explanation with codegraph evidence",
       "suggestion": "guard it"}
    ]
  }`,
	RunE: runReviewerPost,
}

var reviewerCheckoutCmd = &cobra.Command{
	Use:   "checkout <pr>",
	Short: "Fetch and detached-checkout a PR head in the reviewer worktree",
	Long: `Fetch the PR's head commit and check it out in a detached HEAD state.

This is the only sanctioned way the Reviewer touches git: it never creates a
branch and never pushes. With --sha the worktree detaches at exactly that commit
(the SHA the Reviewer was asked to review), so the review is anchored even if
upstream HEAD has moved.`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewerCheckout,
}

var reviewerPerspectivesCmd = &cobra.Command{
	Use:   "perspectives",
	Short: "List the rig's enabled review perspectives and where each resolves",
	Long: `List the review perspectives enabled for the current rig.

Enablement and order come from the rig's review.perspectives setting (defaulting
to the built-in adversarial + security when unset). Each perspective resolves in
order: rig file, town-shared file, then the embedded default. Use
--show <name> to print a single perspective's resolved prompt content.`,
	RunE: runReviewerPerspectives,
}

func init() {
	reviewerPostCmd.Flags().IntVar(&reviewerPostPR, "pr", 0, "PR number (required)")
	reviewerPostCmd.Flags().StringVar(&reviewerPostFindings, "findings", "",
		`findings JSON file, or "-" for stdin (required)`)
	reviewerPostCmd.Flags().StringVar(&reviewerPostSHA, "sha", "",
		"head SHA to anchor the review to (default: PR's current head)")
	_ = reviewerPostCmd.MarkFlagRequired("pr")
	_ = reviewerPostCmd.MarkFlagRequired("findings")

	reviewerCheckoutCmd.Flags().StringVar(&reviewerCheckoutSHA, "sha", "",
		"specific commit SHA to detach at (default: fetched PR head)")

	reviewerPerspectivesCmd.Flags().StringVar(&reviewerPerspectiveShow, "show", "",
		"print the resolved prompt content for a single perspective")

	reviewerCmd.AddCommand(reviewerPostCmd)
	reviewerCmd.AddCommand(reviewerCheckoutCmd)
	reviewerCmd.AddCommand(reviewerPerspectivesCmd)

	rootCmd.AddCommand(reviewerCmd)
}

func runReviewerPost(cmd *cobra.Command, args []string) error {
	if reviewerPostPR <= 0 {
		return fmt.Errorf("--pr must be a positive PR number")
	}

	data, err := readFindingsInput(reviewerPostFindings)
	if err != nil {
		return err
	}
	findings, err := reviewer.ParseFindings(data)
	if err != nil {
		return err
	}

	provider, _, _, err := getRefineryPRContext()
	if err != nil {
		return err
	}

	// Anchor the review to the requested SHA, or the PR's current head when
	// not supplied. A best-effort lookup keeps the review SHA-scoped for the
	// refinery's engagement gate; if it fails we still post (unanchored).
	sha := reviewerPostSHA
	if sha == "" {
		if head, herr := provider.CurrentHeadSHA(reviewerPostPR); herr == nil {
			sha = head
		}
	}

	in := findings.BuildReviewInput(sha)
	if err := provider.SubmitReview(reviewerPostPR, in); err != nil {
		return fmt.Errorf("submitting review for PR #%d: %w", reviewerPostPR, err)
	}

	fmt.Printf("Posted review on PR #%d: %d inline finding(s)", reviewerPostPR, len(in.Comments))
	if sha != "" {
		fmt.Printf(" at %s", shortSHA(sha))
	}
	fmt.Println()
	return nil
}

func runReviewerCheckout(cmd *cobra.Command, args []string) error {
	prNumber, err := parsePRNumber(args[0])
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolving working directory: %w", err)
	}
	// The checkout happens in the reviewer worktree (the current directory),
	// not the refinery worktree, so gh/git resolve the right tree. Refuse to
	// run anywhere else: a detached checkout is destructive to the working
	// tree, so an operator (or a confused agent) running this in an arbitrary
	// repo must fail fast rather than silently reset HEAD there.
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if info := detectRole(cwd, townRoot); info.Role != RoleReviewer {
		return fmt.Errorf("gt reviewer checkout must run in a reviewer worktree "+
			"(<rig>/reviewer/rig); current directory resolves to role %q", info.Role)
	}

	g := git.NewGit(cwd)
	if err := g.CheckoutPRHeadDetached(prNumber, reviewerCheckoutSHA); err != nil {
		return err
	}

	target := reviewerCheckoutSHA
	if target == "" {
		target = "head"
	} else {
		target = shortSHA(target)
	}
	fmt.Printf("Checked out PR #%d (%s) in %s\n", prNumber, target, cwd)
	return nil
}

func runReviewerPerspectives(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigName, err := inferRigFromCwd(townRoot)
	if err != nil {
		return fmt.Errorf("could not determine rig: %w", err)
	}
	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	// Load the rig's review config (best-effort: a missing/empty settings file
	// falls back to the built-in defaults rather than erroring).
	var reviewCfg *config.ReviewConfig
	if settings, lerr := config.LoadRigSettings(config.RigSettingsPath(r.Path)); lerr == nil && settings != nil {
		reviewCfg = settings.Review
	}
	names := reviewCfg.GetPerspectives()
	failSilent := reviewCfg != nil && reviewCfg.FailSilentPerspectives

	// --show: print a single perspective's resolved content.
	if reviewerPerspectiveShow != "" {
		rp, rerr := reviewer.ResolvePerspective(townRoot, r.Path, reviewerPerspectiveShow)
		if rerr != nil {
			return rerr
		}
		fmt.Print(rp.Content)
		if len(rp.Content) > 0 && rp.Content[len(rp.Content)-1] != '\n' {
			fmt.Println()
		}
		return nil
	}

	resolved, skipped, rerr := reviewer.ResolvePerspectives(townRoot, r.Path, names, failSilent)
	if rerr != nil {
		return rerr
	}
	fmt.Printf("Review perspectives for rig %s (%d enabled):\n", rigName, len(resolved))
	for i, rp := range resolved {
		fmt.Printf("  %d. %-14s [%s] %s\n", i+1, rp.Name, rp.Source, rp.Path)
	}
	for _, s := range skipped {
		fmt.Printf("  - %-14s [skipped: not found]\n", s)
	}
	return nil
}

// readFindingsInput reads the findings payload from a file path or stdin ("-").
func readFindingsInput(path string) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("reading findings from stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied findings file
	if err != nil {
		return nil, fmt.Errorf("reading findings file %s: %w", path, err)
	}
	return data, nil
}

// shortSHA truncates a commit SHA to its first 12 characters for display.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
