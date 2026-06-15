package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/reviewer"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Reviewer command flags.
var (
	reviewerPostPR          int
	reviewerPostFindings    string
	reviewerPostSHA         string
	reviewerCheckoutSHA     string
	reviewerPerspectiveShow string

	reviewerRequestMR     string
	reviewerRequestBranch string
	reviewerRequestSHA    string
	reviewerRequestRound  int
	reviewerRequestOrigin string

	reviewerPromptPR           int
	reviewerPromptSHA          string
	reviewerPromptRound        int
	reviewerPromptPriorThreads string
	reviewerPromptInstructions string
	reviewerPromptMaxFindings  int

	reviewerConsolidateSHA string
	reviewerConsolidateOut string
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

var reviewerRequestCmd = &cobra.Command{
	Use:   "request <pr>",
	Short: "Dispatch a review of a PR to the rig's Reviewer (bead + session)",
	Long: `Dispatch a code review of a PR to the rig's on-demand Reviewer.

Resolves the machine-user token from merge_queue.reviewer_token_env (fail-fast
if unset), assembles a review-request mail carrying the PR number, head SHA,
branch, round, and origin (refinery when --mr is given, else crew), sends it to
<rig>/reviewer, and starts the reviewer session if one isn't already running
(idempotent — a second request queues in the running session's mailbox).

On round >= 2 the prior round's unresolved review threads are fetched and
embedded so the Reviewer has the fix-loop context without gathering it itself.`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewerRequest,
}

var reviewerDoneCmd = &cobra.Command{
	Use:   "done",
	Short: "Clear reviewer state and self-terminate the session",
	Long: `Signal the review is complete and self-terminate the reviewer session.

Run from inside the reviewer session after posting the review. The session is
killed after a short delay so the command can report success first.`,
	RunE: runReviewerDone,
}

var reviewerPromptCmd = &cobra.Command{
	Use:   "prompt <perspective>",
	Short: "Generate the fully-resolved prompt for one perspective review pass",
	Long: `Generate the deterministic prompt a single perspective review pass executes.

The prompt is assembled from the resolved perspective content (rig → town →
built-in) plus a shared, templated execution contract that centralizes how to
review: which SHA/diff to target, how to handle fix rounds, the required
codegraph tooling, the evidence standard, and the exact output JSON schema. The
perspective markdown supplies only the lens; this command supplies the rest, so
no part of the procedure is left to per-agent interpretation.

The reviewer role generates one prompt per enabled perspective and hands each to
a subagent, which reviews from that prompt alone and returns a PerspectiveResult
JSON object. The prompt is written to stdout.`,
	Args: cobra.ExactArgs(1),
	RunE: runReviewerPrompt,
}

var reviewerConsolidateCmd = &cobra.Command{
	Use:   "consolidate [result.json ...]",
	Short: "Merge per-perspective results into one findings payload for post",
	Long: `Consolidate per-perspective subagent results into a single findings payload.

Each input is a PerspectiveResult JSON object (perspective + verdict + findings),
one per enabled perspective. Pass them as file arguments, or pipe a JSON array of
them on stdin. The output is the findings JSON that 'gt reviewer post' consumes:

  - the summary lists every perspective's verdict (perspectives with no findings
    are still accounted for — never silent),
  - findings are deduplicated by (path, line, title) with the highest priority
    winning and perspective tags unioned.

Deterministic dedup lives here, in Go, rather than in per-run reviewer judgment.
Writes to --out, or stdout when --out is omitted.`,
	RunE: runReviewerConsolidate,
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

	reviewerRequestCmd.Flags().StringVar(&reviewerRequestMR, "mr", "",
		"MR bead ID (refinery origin); omit for a standalone/crew request")
	reviewerRequestCmd.Flags().StringVar(&reviewerRequestBranch, "branch", "", "PR head branch (optional)")
	reviewerRequestCmd.Flags().StringVar(&reviewerRequestSHA, "sha", "",
		"PR head SHA to review (default: the PR's current head)")
	reviewerRequestCmd.Flags().IntVar(&reviewerRequestRound, "round", 1, "review round number (>=2 embeds prior threads)")
	reviewerRequestCmd.Flags().StringVar(&reviewerRequestOrigin, "origin", "",
		"request origin: refinery|crew (default: refinery when --mr is set, else crew)")

	reviewerPromptCmd.Flags().IntVar(&reviewerPromptPR, "pr", 0, "PR number under review (required)")
	reviewerPromptCmd.Flags().StringVar(&reviewerPromptSHA, "sha", "",
		"exact head SHA the pass must review and anchor to (required)")
	reviewerPromptCmd.Flags().IntVar(&reviewerPromptRound, "round", 1, "review round (>=2 is a fix round)")
	reviewerPromptCmd.Flags().StringVar(&reviewerPromptPriorThreads, "prior-threads", "",
		`file of prior-round thread context (or "-" for stdin); only used when round >= 2`)
	reviewerPromptCmd.Flags().StringVar(&reviewerPromptInstructions, "instructions", "",
		`file of extra execution instructions to inject (or "-" for stdin)`)
	reviewerPromptCmd.Flags().IntVar(&reviewerPromptMaxFindings, "max-findings", 0,
		"per-pass finding cap (default: the rig's review.max_findings_per_perspective)")
	_ = reviewerPromptCmd.MarkFlagRequired("pr")
	_ = reviewerPromptCmd.MarkFlagRequired("sha")

	reviewerConsolidateCmd.Flags().StringVar(&reviewerConsolidateSHA, "sha", "",
		"reviewed head SHA to record in the consolidated payload")
	reviewerConsolidateCmd.Flags().StringVar(&reviewerConsolidateOut, "out", "",
		"write the consolidated findings JSON here (default: stdout)")

	reviewerCmd.AddCommand(reviewerPostCmd)
	reviewerCmd.AddCommand(reviewerCheckoutCmd)
	reviewerCmd.AddCommand(reviewerPerspectivesCmd)
	reviewerCmd.AddCommand(reviewerPromptCmd)
	reviewerCmd.AddCommand(reviewerConsolidateCmd)
	reviewerCmd.AddCommand(reviewerRequestCmd)
	reviewerCmd.AddCommand(reviewerDoneCmd)

	rootCmd.AddCommand(reviewerCmd)
}

// requireReviewerWorktree fails unless the current directory resolves to the
// reviewer role (any directory under a rig's reviewer/ tree, per detectRole),
// returning the cwd on success. Both `post` and `checkout` use it: `post`
// resolves rig/PR context from the current directory, and `checkout` does a
// destructive detached checkout — so running either outside the reviewer
// context risks targeting an unrelated PR or resetting an unintended working
// tree. (git operations in checkout additionally fail fast if the directory
// isn't a real worktree.)
func requireReviewerWorktree() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolving working directory: %w", err)
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if info := detectRole(cwd, townRoot); info.Role != RoleReviewer {
		return "", fmt.Errorf("must run inside the reviewer worktree (a <rig>/reviewer/ directory); "+
			"current directory resolves to role %q", info.Role)
	}
	return cwd, nil
}

func runReviewerPost(cmd *cobra.Command, args []string) error {
	if reviewerPostPR <= 0 {
		return fmt.Errorf("--pr must be a positive PR number")
	}
	if _, err := requireReviewerWorktree(); err != nil {
		return err
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

	// Anchor the review to the SHA actually reviewed so it is SHA-scoped for
	// the refinery's engagement gate. Resolution order: --sha flag, then the
	// findings payload's reviewed_sha (the SHA the Reviewer checked out and
	// reviewed), then the PR's current head as a best-effort fallback. Honoring
	// reviewed_sha preserves the "review exactly the requested SHA" contract
	// even if the PR head moved between checkout and post.
	sha := reviewerPostSHA
	if sha == "" {
		sha = findings.ReviewedSHA
	}
	if sha == "" {
		if head, herr := provider.CurrentHeadSHA(reviewerPostPR); herr == nil {
			sha = head
		}
	}
	// A review with inline findings MUST be anchored to a commit: GitHub rejects
	// the review API call without commit_id when comments are present, and the
	// refinery's SHA-scoped engagement gate needs it to observe the review.
	// Fail loudly rather than post a review the refinery will never see.
	if sha == "" && len(findings.Findings) > 0 {
		return fmt.Errorf("could not resolve a commit SHA to anchor PR #%d's review "+
			"(pass --sha, or set reviewed_sha in the findings payload): a review with "+
			"inline findings requires a commit_id", reviewerPostPR)
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

	cwd, err := requireReviewerWorktree()
	if err != nil {
		return err
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

	// Load the rig's review config. A genuinely-absent settings file falls
	// back to the built-in defaults, but a malformed or unreadable file must
	// surface — silently running the reviewer with an unintended config (e.g.
	// the wrong perspectives) is worse than failing loudly.
	var reviewCfg *config.ReviewConfig
	settings, lerr := config.LoadRigSettings(config.RigSettingsPath(r.Path))
	if lerr != nil && !errors.Is(lerr, config.ErrNotFound) {
		return fmt.Errorf("loading rig review settings: %w", lerr)
	}
	if settings != nil {
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

// loadRigReviewConfig resolves the current rig's review config for the
// prompt/perspectives commands. A genuinely-absent settings file falls back to
// defaults (nil ReviewConfig, which the getters treat as defaults), but a
// malformed/unreadable file surfaces — silently reviewing with an unintended
// config is worse than failing loudly.
func loadRigReviewConfig() (townRoot, rigName, rigPath string, reviewCfg *config.ReviewConfig, err error) {
	townRoot, err = workspace.FindFromCwdOrError()
	if err != nil {
		return "", "", "", nil, fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	rigName, err = inferRigFromCwd(townRoot)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("could not determine rig: %w", err)
	}
	_, r, err := getRig(rigName)
	if err != nil {
		return "", "", "", nil, err
	}
	settings, lerr := config.LoadRigSettings(config.RigSettingsPath(r.Path))
	if lerr != nil && !errors.Is(lerr, config.ErrNotFound) {
		return "", "", "", nil, fmt.Errorf("loading rig review settings: %w", lerr)
	}
	if settings != nil {
		reviewCfg = settings.Review
	}
	return townRoot, rigName, r.Path, reviewCfg, nil
}

func runReviewerPrompt(cmd *cobra.Command, args []string) error {
	perspective := args[0]
	if reviewerPromptPR <= 0 {
		return fmt.Errorf("--pr must be a positive PR number")
	}
	if strings.TrimSpace(reviewerPromptSHA) == "" {
		return fmt.Errorf("--sha is required: a pass reviews and anchors to an exact commit")
	}

	townRoot, rigName, rigPath, reviewCfg, err := loadRigReviewConfig()
	if err != nil {
		return err
	}

	rp, err := reviewer.ResolvePerspective(townRoot, rigPath, perspective)
	if err != nil {
		return err
	}

	// stdin can only be consumed once, so both inputs cannot read from it.
	if reviewerPromptPriorThreads == "-" && reviewerPromptInstructions == "-" {
		return fmt.Errorf("--prior-threads and --instructions cannot both read from stdin (\"-\")")
	}
	priorThreads, err := readOptionalInput(reviewerPromptPriorThreads)
	if err != nil {
		return fmt.Errorf("reading --prior-threads: %w", err)
	}
	extra, err := readOptionalInput(reviewerPromptInstructions)
	if err != nil {
		return fmt.Errorf("reading --instructions: %w", err)
	}

	maxFindings := reviewerPromptMaxFindings
	if maxFindings <= 0 {
		maxFindings = reviewCfg.GetMaxFindingsPerPerspective()
	}

	out, err := reviewer.BuildPerspectivePrompt(reviewer.PromptParams{
		Perspective:       rp.Name,
		Lens:              rp.Content,
		RigName:           rigName,
		PR:                reviewerPromptPR,
		SHA:               reviewerPromptSHA,
		Round:             reviewerPromptRound,
		PriorThreads:      priorThreads,
		MaxFindings:       maxFindings,
		ExtraInstructions: extra,
	})
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func runReviewerConsolidate(cmd *cobra.Command, args []string) error {
	var results []reviewer.PerspectiveResult

	if len(args) == 0 {
		// No file args: read a JSON array of PerspectiveResult objects from
		// stdin. Re-marshal each element and run it through ParsePerspectiveResult
		// so stdin and file inputs get identical validation/normalization.
		// Guard against a hang when stdin is an interactive terminal (no pipe).
		if stat, statErr := os.Stdin.Stat(); statErr == nil && (stat.Mode()&os.ModeCharDevice) != 0 {
			return fmt.Errorf("no result files given and stdin is a terminal; " +
				"pass result.json file arguments or pipe a JSON array of results")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading results from stdin: %w", err)
		}
		var raw []json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parsing results array from stdin: %w", err)
		}
		if len(raw) == 0 {
			return fmt.Errorf("no perspective results on stdin")
		}
		for i, elem := range raw {
			r, perr := reviewer.ParsePerspectiveResult(elem)
			if perr != nil {
				return fmt.Errorf("stdin result[%d]: %w", i, perr)
			}
			results = append(results, *r)
		}
	} else {
		for _, path := range args {
			data, err := os.ReadFile(path) //nolint:gosec // operator-supplied result file
			if err != nil {
				return fmt.Errorf("reading result file %s: %w", path, err)
			}
			r, perr := reviewer.ParsePerspectiveResult(data)
			if perr != nil {
				return fmt.Errorf("%s: %w", path, perr)
			}
			results = append(results, *r)
		}
	}

	fs := reviewer.Consolidate(results, reviewerConsolidateSHA)
	encoded, err := json.MarshalIndent(fs, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding consolidated findings: %w", err)
	}
	encoded = append(encoded, '\n')

	if reviewerConsolidateOut != "" {
		if err := os.WriteFile(reviewerConsolidateOut, encoded, 0o644); err != nil { //nolint:gosec // operator-facing output
			return fmt.Errorf("writing %s: %w", reviewerConsolidateOut, err)
		}
		fmt.Printf("Wrote consolidated findings (%d) to %s\n", len(fs.Findings), reviewerConsolidateOut)
		return nil
	}
	_, err = os.Stdout.Write(encoded)
	return err
}

func runReviewerRequest(cmd *cobra.Command, args []string) error {
	prNumber, err := parsePRNumber(args[0])
	if err != nil {
		return err
	}

	provider, cfg, r, err := getRefineryPRContext()
	if err != nil {
		return err
	}

	// Fail fast if the machine-user token isn't present in the dispatcher's
	// environment — the value never touches disk; config holds only the name.
	tokenEnv := cfg.GetReviewerTokenEnv()
	tokenVal := os.Getenv(tokenEnv)
	if tokenVal == "" {
		return fmt.Errorf("reviewer token env %q is not set — add `export %s=…` to "+
			"settings/daemon.env and restart the daemon", tokenEnv, tokenEnv)
	}

	sha := reviewerRequestSHA
	if sha == "" {
		if head, herr := provider.CurrentHeadSHA(prNumber); herr == nil {
			sha = head
		}
	}
	// The Reviewer must review a specific commit: it anchors the posted review
	// (GitHub requires commit_id with inline comments) and the refinery's
	// SHA-scoped engagement gate keys on it. Refuse to dispatch a request with
	// no SHA rather than mail one the Reviewer can't act on reliably.
	if sha == "" {
		return fmt.Errorf("could not resolve a head SHA for PR #%d to anchor the review "+
			"(pass --sha)", prNumber)
	}

	origin := reviewer.DefaultOrigin(reviewerRequestOrigin, reviewerRequestMR)
	spec := reviewer.RequestSpec{
		PR:      prNumber,
		HeadSHA: sha,
		Branch:  reviewerRequestBranch,
		Round:   reviewerRequestRound,
		Origin:  origin,
		MRID:    reviewerRequestMR,
	}

	// On a re-review, embed the prior round's unresolved threads so the
	// Reviewer gets fix-loop context deterministically (best-effort).
	priorThreads := ""
	if spec.Round >= 2 {
		if threads, terr := provider.UnresolvedThreads(prNumber); terr == nil {
			var pb strings.Builder
			for _, th := range threads {
				fmt.Fprintf(&pb, "- %s:%d [%s] %s\n", th.Path, th.Line, th.Author, firstLineOf(th.Body))
			}
			priorThreads = pb.String()
		}
	}

	townRoot := filepath.Dir(r.Path)
	from := fmt.Sprintf("%s/refinery", r.Name)
	if origin == reviewer.OriginCrew {
		from = fmt.Sprintf("%s/crew", r.Name)
	}
	to := fmt.Sprintf("%s/reviewer", r.Name)

	router := mail.NewRouterWithTownRoot(townRoot, townRoot)
	defer router.WaitPendingNotifications()
	msg := mail.NewMessage(from, to, spec.Subject(), spec.Body(priorThreads))
	msg.Type = mail.TypeTask
	if err := router.Send(msg); err != nil {
		return fmt.Errorf("sending review request to %s: %w", to, err)
	}

	// Start the reviewer session if not already running, injecting the token as
	// GH_TOKEN/GITHUB_TOKEN. Idempotent: a running session drains the new mail.
	mgr := reviewer.NewManager(r)
	if serr := mgr.EnsureRunning("", map[string]string{"GH_TOKEN": tokenVal, "GITHUB_TOKEN": tokenVal}); serr != nil {
		// The request mail persists; await-review's timeout is the safety net.
		fmt.Fprintf(os.Stderr, "warning: review request mailed but reviewer session did not start: %v\n", serr)
	}

	fmt.Printf("Dispatched review of PR #%d (round %d, origin %s) → %s\n", prNumber, spec.Round, origin, to)
	return nil
}

func runReviewerDone(cmd *cobra.Command, args []string) error {
	cwd, err := requireReviewerWorktree()
	if err != nil {
		return err
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	rigName := detectRole(cwd, townRoot).Rig
	if rigName == "" {
		return fmt.Errorf("could not determine rig from %s", cwd)
	}
	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	// Self-terminate the session after a short delay so this command reports
	// success before the agent is killed (mirrors `gt dog done`).
	sessionID := reviewer.NewManager(r).SessionName()
	t := tmux.NewTmux()
	_ = t.SetRemainOnExit(sessionID, false)
	go func() {
		time.Sleep(3 * time.Second)
		_ = t.KillSession(sessionID)
	}()
	fmt.Printf("Reviewer done — session %s will terminate shortly.\n", sessionID)
	time.Sleep(4 * time.Second)
	return nil
}

// firstLineOf returns the first non-empty line of s, trimmed, for compact
// prior-thread summaries.
func firstLineOf(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			// Truncate on runes, not bytes, so a multi-byte character is never
			// split into invalid UTF-8.
			if r := []rune(t); len(r) > 120 {
				return string(r[:120])
			}
			return t
		}
	}
	return ""
}

// readOptionalInput returns the content of an optional file argument: "" for an
// unset flag, stdin for "-", else the file's contents. Used for the prompt
// command's --prior-threads and --instructions slots, both of which are
// optional.
func readOptionalInput(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied prompt input file
	if err != nil {
		return "", err
	}
	return string(data), nil
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
