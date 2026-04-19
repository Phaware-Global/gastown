package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// tapGuardPushMainCmd blocks `git push ... main` and its refspec variants
// when the current rig is configured with merge_strategy = "pr".
//
// This is defense-in-depth against a real incident we observed: a polecat's
// MR bead creation failed (an unrelated bug), the refinery patrol found a
// polecat branch on origin with no MR in the queue, and its Claude session —
// without explicit formula instructions for that state — improvised by doing
// a fast-forward `git push origin FETCH_HEAD:refs/heads/main`. That single
// push bypassed the entire PR workflow (no PR, no review, no approval).
//
// Under merge_strategy=pr, NO agent should push directly to main — even the
// refinery. The refinery's formal merge step uses `gh pr merge`, which
// GitHub performs server-side; it never needs a `git push :main`. This guard
// enforces that invariant.
//
// Under any other strategy (direct, empty, merge-queue), the guard is a
// no-op and `git push origin main` is allowed — that's the expected path.
var tapGuardPushMainCmd = &cobra.Command{
	Use:   "push-main",
	Short: "Block git push to main under merge_strategy=pr",
	Long: `Block git push commands that target main under merge_strategy=pr.

When a rig has merge_queue.merge_strategy=pr, all merges must flow through
the GitHub PR + refinery review path. Any agent (refinery included) that
pushes directly to main bypasses the review gate — we caught this in the
wild during the first dogfood of the PR workflow, when the refinery's
Claude session improvised a fast-forward push after an MR bead creation
failure.

The guard reads the tool input from stdin (Claude Code hook protocol),
parses the command, and blocks it with exit 2 when:
  - the command is a "git push" that lands on refs/heads/main, AND
  - the current rig's merge_queue config has merge_strategy=pr.

Under any other merge_strategy (direct, empty, merge-queue), the guard is
a no-op and pushes pass through unchanged.

Hook matcher: Bash(git push*)

Exit codes:
  0 - Operation allowed (not push-to-main, or strategy is not pr)
  2 - Operation BLOCKED (push-to-main under merge_strategy=pr)`,
	RunE: runTapGuardPushMain,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardPushMainCmd)
}

func runTapGuardPushMain(cmd *cobra.Command, args []string) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil // fail open on hook-protocol weirdness
	}

	command := extractCommand(input)
	if command == "" {
		return nil
	}

	if !isPushToMain(command) {
		return nil
	}

	// Only block under merge_strategy=pr. For any other strategy,
	// push-to-main is the normal path (Gas Town's default direct-merge).
	if !currentRigRequiresPRStrategy() {
		return nil
	}

	printPushMainBlock(command)
	return NewSilentExit(2)
}

// isPushToMain reports whether a shell command is a `git push` whose
// refspec lands on `main` (or `refs/heads/main`).
//
// The check is deliberately tokenized rather than regex — we want to be
// robust across:
//
//	git push origin main
//	git push origin HEAD:main
//	git push origin HEAD:refs/heads/main
//	git push origin polecat/foo:main
//	git push origin polecat/foo:refs/heads/main
//	git push origin FETCH_HEAD:refs/heads/main     (the one we observed)
//	git push -f origin main
//	git push --force-with-lease origin main
//
// and reject non-matches like:
//
//	git push origin polecat/foo
//	git push origin polecat/foo:feat/branch
//	git fetch origin main                          (not a push)
//	echo "git push origin main"                    (not the top-level command)
//
// We don't try to handle every possible shell-quoting edge case; this is a
// hook-fired best effort, not a security boundary.
func isPushToMain(command string) bool {
	// Strip leading whitespace; the tool-input can include a wrapping shell
	// invocation in some cases but Claude Code's Bash tool passes the raw
	// command, so this is usually a no-op.
	trimmed := strings.TrimSpace(command)

	// Reject non-push commands fast. `git push` must appear as an
	// adjacent pair at the top of the command.
	fields := strings.Fields(trimmed)
	if len(fields) < 2 {
		return false
	}
	if fields[0] != "git" || fields[1] != "push" {
		return false
	}

	// Walk the rest of the args. The last non-flag token is the refspec
	// (or the branch name, which is equivalent to <ref>:<ref>).
	var lastPositional string
	for _, f := range fields[2:] {
		if strings.HasPrefix(f, "-") {
			continue
		}
		lastPositional = f
	}
	if lastPositional == "" {
		return false
	}

	// The refspec targets main if either:
	//   - no colon and token is literally "main", or
	//   - colon and the RHS names main / refs/heads/main
	if lastPositional == "main" || lastPositional == "refs/heads/main" {
		return true
	}
	if idx := strings.Index(lastPositional, ":"); idx >= 0 {
		dst := lastPositional[idx+1:]
		if dst == "main" || dst == "refs/heads/main" {
			return true
		}
	}
	return false
}

// currentRigRequiresPRStrategy returns true when the rig we're currently
// running in has merge_strategy=pr. Reuses the refineryAllowedForPR
// settings-lookup path but without the GT_REFINERY gate — here we want
// the strategy independent of which agent is calling.
func currentRigRequiresPRStrategy() bool {
	// We deliberately reuse refineryAllowedForPR's settings-lookup body
	// rather than factoring both out, because:
	//   - The two callers want slightly different semantics (one requires
	//     GT_REFINERY, this one doesn't).
	//   - Both fail-closed on any lookup error, which is the correct
	//     behavior in both places.
	//
	// The temporary GT_REFINERY override lets us reuse refineryAllowedForPR
	// as-is: set GT_REFINERY if unset, call the helper, restore. This
	// keeps the settings-resolution logic (GT_RIG preference, CWD
	// fallback, rigName escape guard) in exactly one place.
	prevRefinery, hadRefinery := os.LookupEnv("GT_REFINERY")
	if !hadRefinery {
		os.Setenv("GT_REFINERY", "1")
		defer os.Unsetenv("GT_REFINERY")
	} else {
		_ = prevRefinery // no change needed
	}
	return refineryAllowedForPR()
}

func printPushMainBlock(command string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ DIRECT PUSH TO main BLOCKED                                  ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(os.Stderr, "║  This rig has merge_strategy=pr. All merges must land via a PR.  ║")
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintf(os.Stderr, "║  Command: %-53s ║\n", truncateStr(command, 53))
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  Expected flow:                                                  ║")
	fmt.Fprintln(os.Stderr, "║    polecat:  git push origin <feature-branch>  +  gt done        ║")
	fmt.Fprintln(os.Stderr, "║    refinery: gh pr create / gh pr merge --squash                 ║")
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  If you need to bypass this (disaster recovery), ask a human.    ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
