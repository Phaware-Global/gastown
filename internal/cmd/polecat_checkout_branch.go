package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/util"
)

// G42 + G43: the polecat formula needs an imperative branch-creation step
// because:
//
//  1. The tap-guard intentionally blocks `git checkout -b` from agent
//     contexts (G19b / G21 hardening) so polecats can't open feature
//     branches outside the merge-queue workflow. That block is correct.
//  2. But polecats LEGITIMATELY need a polecat-namespaced branch before
//     editing — every other formula step assumes they're on
//     `polecat/<name>-<bead-id>`, not `main`. With raw `git checkout -b`
//     blocked and no `gt`-side affordance, a polecat that finds itself
//     on main (worktree-init residue, post-merge cleanup, gt-pvx
//     auto-save aftermath) has no permitted way to recover.
//
// This subcommand is the formula-side dual of the safety net in G41:
// instead of refusing to commit on main, it lets the polecat *leave*
// main onto a branch the rest of the workflow expects. Tap-guard does
// not intercept `gt` subcommand invocations, so this path is allowed
// even when the equivalent raw `git checkout -b` is not.

var polecatCheckoutBranchCmd = &cobra.Command{
	Use:   "checkout-branch <bead-id>",
	Short: "Create + check out the polecat-namespaced work branch (G42/G43)",
	Long: `Create or check out the polecat's work branch for the given bead.

Branch name: polecat/<polecat-name>-<bead-id>
Base:        origin/main (fetched fresh)

This is the formula-imperative replacement for ` + "`" + `git checkout -b` + "`" + `, which the
tap-guard blocks for polecat sessions. Without this affordance, a polecat
that lands on main (worktree-init residue, post-merge state, recovery
from a gt-pvx safety-net incident) has no permitted way to start work.

Polecat name is read from $GT_POLECAT (set by the session manager) or
--polecat <name> if you're invoking the command outside a session.

Idempotent:
  - already on the target branch     → no-op, exit 0
  - on main (or any non-target)      → fetch origin, create branch from
                                       origin/main, check it out
  - on a different polecat branch    → exit 1, refuse silently switching
                                       between polecats' branches

Recovery flow when the gt-pvx safety net (G41 guard) refused to commit
on main:

    git stash push -m "<recover>"
    gt polecat checkout-branch <bead-id>
    git stash pop
    git add . && git commit -m "<your real message>"`,
	Args: cobra.ExactArgs(1),
	RunE: runPolecatCheckoutBranch,
}

var polecatCheckoutBranchPolecat string

func init() {
	polecatCmd.AddCommand(polecatCheckoutBranchCmd)
	polecatCheckoutBranchCmd.Flags().StringVar(&polecatCheckoutBranchPolecat,
		"polecat", "",
		"polecat name (overrides $GT_POLECAT; required when run outside a polecat session)")
}

// beadIDPattern accepts the standard bd issue-id forms: a 2-3 char
// lowercase prefix followed by one or more `-segment` parts, where each
// segment is lowercase-alphanumeric and may include `.` for subtask
// suffixes. Covers gt-mwy, gt-mwy.5, gt-mwy.5.2, gt-i71, hq-1pl,
// hq-wisp-ku6, gt-1qlg. Rejects shell-injection-friendly characters
// (whitespace, `/`, `;`, backticks, `$()`) so a mistyped or hostile
// arg can't leak into the constructed branch name.
//
// Pattern alone is necessary but not sufficient — additional git
// ref-rule checks (no `..`, no leading/trailing `.`, no `.lock`
// suffix) live in validateBeadID, since they don't compose cleanly
// with a single regex.
var beadIDPattern = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9.]+)+$`)

// validateBeadID applies the bead-id pattern AND the git-ref-rule
// fragments that don't fit a single regex (git refuses refs containing
// `..`, components ending in `.`, and `.lock` suffixes). Iter-2 review
// flagged that a strict pattern match still admitted refs git would
// reject — surface those as clear bead-id errors here rather than at
// the `git checkout -b` step where the failure mode is opaque.
func validateBeadID(id string) error {
	if !beadIDPattern.MatchString(id) {
		return fmt.Errorf("bead-id %q is not a valid bd issue id "+
			"(expected lowercase prefix-suffix, e.g. gt-mwy.5)", id)
	}
	if strings.Contains(id, "..") {
		return fmt.Errorf("bead-id %q contains \"..\" which git refs reject", id)
	}
	if strings.HasSuffix(id, ".") {
		return fmt.Errorf("bead-id %q ends with \".\" which git refs reject", id)
	}
	if strings.HasSuffix(id, ".lock") {
		return fmt.Errorf("bead-id %q ends with \".lock\" which git treats as a lock file", id)
	}
	return nil
}

func runPolecatCheckoutBranch(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])

	polecatName := strings.TrimSpace(polecatCheckoutBranchPolecat)
	if polecatName == "" {
		polecatName = strings.TrimSpace(os.Getenv("GT_POLECAT"))
	}
	if polecatName == "" {
		return fmt.Errorf("checkout-branch: polecat name unknown — set $GT_POLECAT (the session " +
			"manager normally does this) or pass --polecat <name>")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("checkout-branch: getwd: %w", err)
	}

	res, err := ensurePolecatWorkBranch(cwd, polecatName, beadID)
	if err != nil {
		return fmt.Errorf("checkout-branch: %w", err)
	}

	switch res.Action {
	case polecatBranchNoop:
		fmt.Fprintf(os.Stdout, "%s Already on %s — no-op\n",
			style.Bold.Render("✓"), res.Target)
	case polecatBranchResumed:
		fmt.Fprintf(os.Stdout, "%s Resumed existing %s (no rebase, no -b)\n",
			style.Bold.Render("✓"), res.Target)
	case polecatBranchCreated:
		fmt.Fprintf(os.Stdout, "%s Created and checked out %s (from origin/main)\n",
			style.Bold.Render("✓"), res.Target)
	}
	return nil
}

// polecatBranchAction describes what ensurePolecatWorkBranch did, so callers can
// render their own message (the CLI command prints a "✓ …" line; the prime-time
// off-main guard prints an "⚠️ OFF-MAIN GUARD" line).
type polecatBranchAction string

const (
	polecatBranchNoop    polecatBranchAction = "noop"    // already on the target branch
	polecatBranchResumed polecatBranchAction = "resumed" // target existed locally, plain checkout
	polecatBranchCreated polecatBranchAction = "created" // created fresh from origin/main
)

// polecatBranchResult reports the target branch and what ensurePolecatWorkBranch
// did to land the worktree on it.
type polecatBranchResult struct {
	Target string
	Action polecatBranchAction
}

// ensurePolecatWorkBranch creates or checks out the polecat's namespaced work
// branch (polecat/<name>-<beadID>) and leaves the worktree on it. It is the
// shared core behind both `gt polecat checkout-branch` (the formula-imperative
// recovery command) and the prime-time off-main guard (gt-tk5).
//
// Behavior by current branch:
//   - already on the target            → no-op (polecatBranchNoop)
//   - on a *different* polecat/… branch → error (refuse to swap silently; the
//     caller's uncommitted work would be lost or contaminated)
//   - anything else (main, master,     → fetch the repo's default branch, then
//     detached, residual)                check out the target: plain checkout if
//     it already exists locally
//     (polecatBranchResumed), else create it
//     from origin/<default> (polecatBranchCreated)
//
// git operations run via exec.Command (a subprocess of `gt`), NOT a Bash tool
// call from the LLM, so the tap-guard PreToolUse hook — which blocks raw
// `git checkout -b` from agent contexts — does not fire here. That bypass is
// the whole point: a polecat stuck on main has no other permitted way to reach
// a work branch.
func ensurePolecatWorkBranch(cwd, polecatName, beadID string) (polecatBranchResult, error) {
	beadID = strings.TrimSpace(beadID)
	if err := validateBeadID(beadID); err != nil {
		return polecatBranchResult{}, err
	}
	polecatName = strings.TrimSpace(polecatName)
	if polecatName == "" {
		return polecatBranchResult{}, fmt.Errorf("polecat name is empty")
	}

	g := git.NewGit(cwd)
	currentBranch, err := g.CurrentBranch()
	if err != nil {
		return polecatBranchResult{}, fmt.Errorf("reading current branch: %w", err)
	}
	currentBranch = strings.TrimSpace(currentBranch)

	// A review-fix dispatch (`gt sling --review-branch`) records the PR
	// branch to resume directly on the dispatch bead (review_branch field,
	// internal/beads/fields.go). When present, that recorded branch is
	// authoritative: resuming it is the entire point of a review-fix
	// polecat. Falling through to the default polecat/<name>-<beadID> name
	// instead abandons the reviewed PR branch for a brand-new one built off
	// mainline — reconstructing state from the environment instead of
	// reading the fact that was recorded (gt-i48h; same root cause as
	// gt-0t6b).
	reviewBranch, rbErr := recordedReviewBranchFn(beadID)
	if rbErr != nil {
		return polecatBranchResult{}, fmt.Errorf("checking bead %s for a recorded review branch: %w", beadID, rbErr)
	}

	targetBranch := fmt.Sprintf("polecat/%s-%s", polecatName, beadID)
	if reviewBranch != "" {
		targetBranch = reviewBranch
	}

	switch {
	case currentBranch == targetBranch:
		return polecatBranchResult{Target: targetBranch, Action: polecatBranchNoop}, nil

	case strings.HasPrefix(currentBranch, "polecat/") && currentBranch != targetBranch:
		// Refuse to silently swap one polecat branch for another. If the
		// polecat has uncommitted work on the existing branch, switching
		// would either lose it (failed checkout) or contaminate the new
		// branch with unrelated changes. Operator chooses how to recover.
		return polecatBranchResult{}, fmt.Errorf("refusing to switch from polecat branch %q to %q — "+
			"commit/stash on the current branch first (this would not be safe to do silently)",
			currentBranch, targetBranch)
	}

	if reviewBranch != "" {
		return resumeReviewBranch(cwd, reviewBranch)
	}

	// Base the work branch on the repo's ACTUAL default branch, not a
	// hardcoded "main". Master-based repos/worktrees have no origin/main, so
	// hardcoding it makes restore fail and wedges `gt prime` — the exact
	// failure this guard exists to prevent. RemoteDefaultBranch resolves
	// origin/HEAD (falling back to origin/master then origin/main) from the
	// remote-tracking refs already present after clone.
	defaultBranch := g.RemoteDefaultBranch()
	remoteRef := "origin/" + defaultBranch

	// Fresh fetch so the new branch is based on the latest default branch.
	// Routing through `git fetch` (rather than g.Fetch which we'd have to
	// add) keeps this command's blast radius narrow — a single helper, no
	// new git package surface.
	fetchCmd := exec.Command("git", "fetch", "origin", defaultBranch)
	fetchCmd.Dir = cwd
	util.SetDetachedProcessGroup(fetchCmd)
	if out, fErr := fetchCmd.CombinedOutput(); fErr != nil {
		return polecatBranchResult{}, fmt.Errorf("git fetch origin %s failed: %s: %w",
			defaultBranch, strings.TrimSpace(string(out)), fErr)
	}

	// Branch already exists locally? Plain `checkout -b` would fail; the
	// polecat may have created the branch on a prior session and ended up
	// back on main without it being deleted (post-merge cleanup, gt-pvx
	// guard re-runs, etc.). Iter-2 review flagged this as a re-entry
	// case. Detect with `rev-parse --verify`; if present, do a plain
	// checkout rather than -b. Either way the polecat ends up on the
	// target branch.
	branchExists := false
	probe := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+targetBranch)
	probe.Dir = cwd
	util.SetDetachedProcessGroup(probe)
	if err := probe.Run(); err == nil {
		branchExists = true
	}

	// exec.Command runs as a subprocess of `gt`, NOT a Bash tool call
	// from the LLM, so the tap-guard PreToolUse hook does not fire here —
	// the imperative step proceeds where the raw form would block. That's
	// the whole point.
	var checkoutCmd *exec.Cmd
	if branchExists {
		checkoutCmd = exec.Command("git", "checkout", targetBranch)
	} else {
		checkoutCmd = exec.Command("git", "checkout", "-b", targetBranch, remoteRef)
	}
	checkoutCmd.Dir = cwd
	util.SetDetachedProcessGroup(checkoutCmd)
	if out, cErr := checkoutCmd.CombinedOutput(); cErr != nil {
		return polecatBranchResult{}, fmt.Errorf("git checkout %s failed: %s: %w",
			targetBranch, strings.TrimSpace(string(out)), cErr)
	}

	action := polecatBranchCreated
	if branchExists {
		action = polecatBranchResumed
	}
	return polecatBranchResult{Target: targetBranch, Action: action}, nil
}

// recordedReviewBranchFn is the injection point for tests: recordedReviewBranch
// shells out to the real beads store, which requires a live bd/Dolt setup this
// package's git-fixture tests don't have. Tests override this var directly
// rather than faking a bead store.
var recordedReviewBranchFn = recordedReviewBranch

// recordedReviewBranch reads the review_branch attachment field off beadID's
// description — the branch a review-fix dispatch (`gt sling --review-branch`)
// recorded for the polecat to resume (internal/beads/fields.go,
// AttachmentFields.ReviewBranch). Returns "" (not an error) when the bead
// carries no such field, which is the normal case for every non-review-fix
// dispatch — those must keep computing the default polecat/<name>-<beadID>
// name.
func recordedReviewBranch(beadID string) (string, error) {
	bd := beads.New(resolveBeadDir(beadID))
	issue, err := bd.Show(beadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("reading bead %s: %w", beadID, err)
	}
	if issue == nil {
		return "", nil
	}
	fields := beads.ParseAttachmentFields(issue)
	if fields == nil {
		return "", nil
	}
	return strings.TrimSpace(fields.ReviewBranch), nil
}

// resumeReviewBranch fetches and checks out an existing PR branch recorded on
// the dispatch bead (review_branch). Unlike the default path, this never
// falls back to creating a fresh branch from mainline on failure — a
// review-fix polecat that cannot reach the recorded branch must fail loudly
// (gt-i48h FIX guidance) rather than silently starting a new branch that
// abandons the PR under review; that silent-fallback shape is exactly what
// hmetet-z6zz already burned a rig on.
//
// When the branch already exists locally (a prior session in this same
// worktree), it is hard-reset to origin's head rather than trusted as-is: a
// stale local copy sitting behind (or diverged from) the branch's true head
// is the exact state gt-i48h reproduced from. The acceptance bar is
// resuming AT the branch's head, not wherever this worktree last left it.
func resumeReviewBranch(cwd, branch string) (polecatBranchResult, error) {
	fetchCmd := exec.Command("git", "fetch", "origin", branch)
	fetchCmd.Dir = cwd
	util.SetDetachedProcessGroup(fetchCmd)
	if out, fErr := fetchCmd.CombinedOutput(); fErr != nil {
		return polecatBranchResult{}, fmt.Errorf(
			"git fetch origin %s failed: %s: %w — the recorded review branch could not be reached; "+
				"refusing to fall back to a fresh branch from mainline (that would abandon the PR under review)",
			branch, strings.TrimSpace(string(out)), fErr)
	}

	branchExists := false
	probe := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	probe.Dir = cwd
	util.SetDetachedProcessGroup(probe)
	if err := probe.Run(); err == nil {
		branchExists = true
	}

	var checkoutCmd *exec.Cmd
	if branchExists {
		checkoutCmd = exec.Command("git", "checkout", branch)
	} else {
		checkoutCmd = exec.Command("git", "checkout", "-b", branch, "origin/"+branch)
	}
	checkoutCmd.Dir = cwd
	util.SetDetachedProcessGroup(checkoutCmd)
	if out, cErr := checkoutCmd.CombinedOutput(); cErr != nil {
		return polecatBranchResult{}, fmt.Errorf("git checkout %s failed: %s: %w",
			branch, strings.TrimSpace(string(out)), cErr)
	}

	if branchExists {
		resetCmd := exec.Command("git", "reset", "--hard", "origin/"+branch)
		resetCmd.Dir = cwd
		util.SetDetachedProcessGroup(resetCmd)
		if out, rErr := resetCmd.CombinedOutput(); rErr != nil {
			return polecatBranchResult{}, fmt.Errorf("git reset --hard origin/%s failed: %s: %w",
				branch, strings.TrimSpace(string(out)), rErr)
		}
	}

	return polecatBranchResult{Target: branch, Action: polecatBranchResumed}, nil
}
