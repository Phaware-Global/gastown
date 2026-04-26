package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/style"
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
var beadIDPattern = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9.]+)+$`)

func runPolecatCheckoutBranch(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])
	if !beadIDPattern.MatchString(beadID) {
		return fmt.Errorf("checkout-branch: bead-id %q is not a valid bd issue id "+
			"(expected lowercase prefix-suffix, e.g. gt-mwy.5)", beadID)
	}

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
	g := git.NewGit(cwd)

	currentBranch, err := g.CurrentBranch()
	if err != nil {
		return fmt.Errorf("checkout-branch: reading current branch: %w", err)
	}
	currentBranch = strings.TrimSpace(currentBranch)

	targetBranch := fmt.Sprintf("polecat/%s-%s", polecatName, beadID)

	switch {
	case currentBranch == targetBranch:
		fmt.Fprintf(os.Stdout, "%s Already on %s — no-op\n",
			style.Bold.Render("✓"), targetBranch)
		return nil

	case strings.HasPrefix(currentBranch, "polecat/") && currentBranch != targetBranch:
		// Refuse to silently swap one polecat branch for another. If the
		// polecat has uncommitted work on the existing branch, switching
		// would either lose it (failed checkout) or contaminate the new
		// branch with unrelated changes. Operator chooses how to recover.
		return fmt.Errorf("checkout-branch: refusing to switch from polecat branch %q to %q — "+
			"commit/stash on the current branch first (this would not be safe to do silently)",
			currentBranch, targetBranch)
	}

	// Fresh fetch so the new branch is based on the latest origin/main.
	// Routing through `git fetch` (rather than g.Fetch which we'd have to
	// add) keeps this command's blast radius narrow — a single helper, no
	// new git package surface.
	fetchCmd := exec.Command("git", "fetch", "origin", "main")
	fetchCmd.Dir = cwd
	if out, fErr := fetchCmd.CombinedOutput(); fErr != nil {
		return fmt.Errorf("checkout-branch: git fetch origin main failed: %s: %w",
			strings.TrimSpace(string(out)), fErr)
	}

	// `git checkout -b <new> origin/main` creates the branch from the
	// freshly-fetched main tip and checks it out. exec.Command runs as a
	// subprocess of `gt`, NOT a Bash tool call from the LLM, so the
	// tap-guard PreToolUse hook does not fire here — the imperative step
	// proceeds where the raw form would block. That's the whole point.
	checkoutCmd := exec.Command("git", "checkout", "-b", targetBranch, "origin/main")
	checkoutCmd.Dir = cwd
	if out, cErr := checkoutCmd.CombinedOutput(); cErr != nil {
		return fmt.Errorf("checkout-branch: git checkout -b %s origin/main failed: %s: %w",
			targetBranch, strings.TrimSpace(string(out)), cErr)
	}

	fmt.Fprintf(os.Stdout, "%s Created and checked out %s (from origin/main)\n",
		style.Bold.Render("✓"), targetBranch)
	return nil
}
