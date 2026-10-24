package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/git"
	rigpkg "github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/util"
	"github.com/steveyegge/gastown/internal/workspace"
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
Base:        resolved in this order, fetched fresh from origin:
               1. --base <branch>             (e.g. a stacked PR's head)
               2. the rig's default_branch    (<rig>/config.json — the same
                                               source gt sling uses for base_branch)
               3. origin's default branch     (origin/HEAD, then master, then main)

This is the formula-imperative replacement for ` + "`" + `git checkout -b` + "`" + `, which the
tap-guard blocks for polecat sessions. Without this affordance, a polecat
that lands on main (worktree-init residue, post-merge state, recovery
from a gt-pvx safety-net incident) has no permitted way to start work.

Polecat name is read from $GT_POLECAT (set by the session manager) or
--polecat <name> if you're invoking the command outside a session.

Idempotent:
  - already on the target branch     → no-op, exit 0
  - on main (or any non-target)      → fetch origin, create branch from the
                                       resolved base, check it out
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

var polecatCheckoutBranchPolecat, polecatCheckoutBranchBase string

func init() {
	polecatCmd.AddCommand(polecatCheckoutBranchCmd)
	polecatCheckoutBranchCmd.Flags().StringVar(&polecatCheckoutBranchPolecat,
		"polecat", "",
		"polecat name (overrides $GT_POLECAT; required when run outside a polecat session)")
	polecatCheckoutBranchCmd.Flags().StringVar(&polecatCheckoutBranchBase,
		"base", "",
		"branch to base the new work branch on (e.g. a stacked PR's head); overrides the rig's base branch")
}

// beadIDPattern accepts the standard bd issue-id forms: a lowercase
// rig prefix (a letter followed by 0-19 lowercase alphanumerics, matching
// beadsPrefixRegexp's 1-20 char bound) and one or more `-segment` parts, where each segment is lowercase-alphanumeric
// and may include `.` for subtask suffixes. Rig prefixes are configured
// per rig and vary in length. Covers gt-mwy, gt-mwy.5, gt-mwy.5.2,
// gt-i71, hq-1pl, hq-wisp-ku6, gt-1qlg, hmetet-za9w. Rejects shell-injection-friendly characters
// (whitespace, `/`, `;`, backticks, `$()`) so a mistyped or hostile
// arg can't leak into the constructed branch name.
//
// Pattern alone is necessary but not sufficient — additional git
// ref-rule checks (no `..`, no leading/trailing `.`, no `.lock`
// suffix) live in validateBeadID, since they don't compose cleanly
// with a single regex.
var beadIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,19}(-[a-z0-9.]+)+$`)

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

	res, err := ensurePolecatWorkBranch(cwd, polecatName, beadID, polecatCheckoutBranchBase)
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
		fmt.Fprintf(os.Stdout, "%s Created and checked out %s (from origin/%s)\n",
			style.Bold.Render("✓"), res.Target, res.Base)
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
	polecatBranchCreated polecatBranchAction = "created" // created fresh from origin/<base>
)

// polecatBranchResult reports the target branch and what ensurePolecatWorkBranch
// did to land the worktree on it.
type polecatBranchResult struct {
	Target string
	Action polecatBranchAction
	Base   string // branch the work branch was created from; set when Action is created
}

// resolvePolecatBaseBranch picks the branch a new work branch is based on:
// the explicit override, else the rig's default_branch, else the remote's
// default. The returned name has no "origin/" prefix.
func resolvePolecatBaseBranch(g *git.Git, cwd, override string) (string, error) {
	if override = strings.TrimSpace(override); override != "" {
		base := strings.TrimPrefix(override, "origin/")
		// Reject anything git would parse as an option.
		if base == "" || strings.HasPrefix(base, "-") {
			return "", fmt.Errorf("invalid --base %q", override)
		}
		return base, nil
	}
	if base := rigDefaultBranch(cwd); base != "" {
		return base, nil
	}
	return g.RemoteDefaultBranch(), nil
}

// rigDefaultBranch returns default_branch from the config.json of the rig that
// contains cwd, or "" when cwd is outside a rig or the rig sets none.
func rigDefaultBranch(cwd string) string {
	townRoot, err := workspace.Find(cwd)
	if err != nil || townRoot == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(townRoot, abs)
	if err != nil {
		return ""
	}
	rigName, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	if rigName == "" || rigName == "." || rigName == ".." {
		return ""
	}
	cfg, err := rigpkg.LoadRigConfig(filepath.Join(townRoot, rigName))
	if err != nil || cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.DefaultBranch)
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
func ensurePolecatWorkBranch(cwd, polecatName, beadID, baseOverride string) (polecatBranchResult, error) {
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

	targetBranch := fmt.Sprintf("polecat/%s-%s", polecatName, beadID)

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

	// Never hardcode "main": master- and develop-based rigs have no origin/main,
	// so restore would fail and wedge `gt prime` — the exact failure this guard
	// exists to prevent. The base comes from --base, then the rig's
	// default_branch, then origin/HEAD (falling back to origin/master then
	// origin/main) from the remote-tracking refs already present after clone.
	defaultBranch, err := resolvePolecatBaseBranch(g, cwd, baseOverride)
	if err != nil {
		return polecatBranchResult{}, err
	}
	if err := validatePolecatBaseBranch(defaultBranch); err != nil {
		return polecatBranchResult{}, err
	}
	remoteRef := "origin/" + defaultBranch

	// Fresh fetch so the new branch is based on the latest default branch.
	// Routing through `git fetch` (rather than g.Fetch which we'd have to
	// add) keeps this command's blast radius narrow — a single helper, no
	// new git package surface.
	// The refspec is built here so user input is never the refspec itself.
	fetchCmd := exec.Command("git", "fetch", "origin",
		"+refs/heads/"+defaultBranch+":refs/remotes/origin/"+defaultBranch)
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
	return polecatBranchResult{Target: targetBranch, Action: action, Base: defaultBranch}, nil
}

// validatePolecatBaseBranch accepts only a plain branch name. The base is used
// to build a fetch refspec, and a name carrying ':' would let the caller write
// arbitrary local refs in the repo the worktree shares. '+' and '@' are valid in
// branch names (polecat branches carry '@') and are inert inside the explicit
// refspec the fetch builds, so they are allowed.
func validatePolecatBaseBranch(base string) error {
	if base == "" || strings.HasPrefix(base, "-") || strings.HasPrefix(base, "refs/") ||
		strings.ContainsAny(base, ":*?[\\^~") || strings.IndexFunc(base, unicode.IsSpace) >= 0 {
		return fmt.Errorf("invalid base branch %q: want a plain branch name", base)
	}
	// Check the full refname, not --branch: --branch expands "@{-1}" against
	// the reflog of gt's cwd and accepts it.
	if err := exec.Command("git", "check-ref-format", "refs/heads/"+base).Run(); err != nil {
		return fmt.Errorf("invalid base branch %q: not a valid branch name", base)
	}
	return nil
}
