package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
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

	// Only block when we can positively confirm merge_strategy=pr for
	// the current rig. Fail-closed semantics are inside the helper:
	// when the caller is inside a gas town workspace but we can't load
	// that rig's settings, the helper returns true (block). That matches
	// the defense-in-depth intent — an ambiguous rig state shouldn't let
	// an unreviewed push to main slip through.
	if !currentRigRequiresPRStrategy() {
		return nil
	}

	printPushMainBlock(command)
	return NewSilentExit(2)
}

// isPushToMain reports whether a shell command is a `git push` that can
// land commits on `refs/heads/main` on the remote.
//
// The check is deliberately tokenized rather than regex — we want to be
// robust across these shapes (all positive matches):
//
//	git push origin main
//	git push origin HEAD:main
//	git push origin HEAD:refs/heads/main
//	git push origin polecat/foo:main
//	git push origin polecat/foo:refs/heads/main
//	git push origin FETCH_HEAD:refs/heads/main     (the one we observed)
//	git push -f origin main
//	git push --force-with-lease origin main
//	git push origin main HEAD:other                (multi-refspec — main is one of them)
//	git push origin --all                          (implicitly includes main)
//	git push origin --mirror                       (mirrors all refs including main)
//	git push                                       (bare; on main branch → push to main)
//	git push origin                                (same)
//
// and reject non-matches (pass through):
//
//	git push origin polecat/foo
//	git push origin polecat/foo:feat/branch
//	git fetch origin main                          (not a push)
//	echo "git push origin main"                    (not the top-level command)
//	git push origin feature -o main                (main is the VALUE of the -o flag, not a refspec)
//
// We don't try to handle every possible shell-quoting edge case; this is
// a hook-fired best effort, not a security boundary. Ambiguous commands
// err on the side of blocking — defense-in-depth.
func isPushToMain(command string) bool {
	trimmed := strings.TrimSpace(command)
	fields := strings.Fields(trimmed)
	if len(fields) < 2 {
		return false
	}
	if fields[0] != "git" || fields[1] != "push" {
		return false
	}
	argv := fields[2:]

	// `--all` / `--mirror` push multiple refs, including main. `--tags`
	// alone doesn't touch branches, but in combination with other refspecs
	// the refspec test below will catch them.
	for _, a := range argv {
		if a == "--all" || a == "--mirror" {
			return true
		}
	}

	// Walk all positional args, skipping flags and the values of flags
	// known to take a separate-argument value (so `git push origin feature
	// -o main` doesn't false-positive on `main` being `-o`'s value).
	//
	// We skip flag values for a small, well-known set of `git push` flags
	// that use space-separated argv form. Flags using `=` form
	// (`--push-option=main`) are already ignored because they start with
	// `-`, so the token is dropped whole.
	flagsTakingArg := map[string]bool{
		"-o":              true,
		"--push-option":   true,
		"--receive-pack":  true,
		"--exec":          true,
		"--repo":          true,
		"--signed":        true, // `--signed=<yes|no|if-asked>` uses =; only its space form takes a separate arg
	}

	var positionals []string
	skipNext := false
	for _, f := range argv {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(f, "-") {
			if flagsTakingArg[f] {
				skipNext = true
			}
			continue
		}
		positionals = append(positionals, f)
	}

	// Conventionally the first positional is the remote and everything
	// after is refspecs. When there are zero refspecs (positionals is empty,
	// or only the remote was provided), git pushes the current branch's
	// upstream — which could be main.
	switch {
	case len(positionals) == 0:
		return currentBranchIsMain()
	case len(positionals) == 1:
		// One positional: either "git push <remote>" (no refspec, push the
		// current branch's upstream) or "git push <refspec>" (no remote).
		only := positionals[0]
		if refspecTargetsMain(only) {
			return true
		}
		// If the single token is a refspec (contains a colon — e.g.
		// "HEAD:feature", ":feature", "main:feature"), we've fully
		// evaluated it above. Don't fall through to currentBranchIsMain —
		// that would false-positive on "git push HEAD:feature" when
		// the user happens to be on main but is clearly pushing to
		// feature.
		if strings.Contains(only, ":") {
			return false
		}
		// No colon: the token could be either a remote name ("origin")
		// or a short branch name ("main"). "main" was already caught
		// by refspecTargetsMain above, so any remaining single no-colon
		// token is treated as a remote name — push targets the current
		// branch's upstream, which is main only when HEAD=main.
		return currentBranchIsMain()
	default:
		// First positional is the remote; check every subsequent positional.
		for _, r := range positionals[1:] {
			if refspecTargetsMain(r) {
				return true
			}
		}
		return false
	}
}

// refspecTargetsMain returns true if a single refspec token (one of the
// `<src>:<dst>` / `<ref>` forms) names main on the destination side.
func refspecTargetsMain(token string) bool {
	if token == "main" || token == "refs/heads/main" {
		return true
	}
	if idx := strings.Index(token, ":"); idx >= 0 {
		dst := token[idx+1:]
		if dst == "main" || dst == "refs/heads/main" {
			return true
		}
	}
	return false
}

// currentBranchIsMain returns true if the current git branch is "main".
// Any error (not in a repo, git unavailable) returns false — we can't
// prove the push targets main, so fall through to the "no refspec → allow"
// default. This is the only place the guard fails open on a push-target
// determination; every other path is "when in doubt, block".
func currentBranchIsMain() bool {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "main"
}

// currentRigRequiresPRStrategy returns true when the calling process is
// inside a gas town rig and that rig's merge_queue config declares
// `merge_strategy = "pr"`.
//
// Fail-closed semantics (critical under the defense-in-depth intent):
//
//   - Not in a gas town workspace at all → return false (fail open).
//     This is not a gas town push; the guard must not disturb normal dev.
//   - Inside a workspace, but the rig can't be identified (no GT_RIG and
//     cwd doesn't name a rig) → return false (fail open). The push isn't
//     scoped to a specific rig, so we have no policy to enforce.
//   - Inside a workspace, rig identified, but settings load fails →
//     return TRUE (fail closed). An ambiguous rig state shouldn't let
//     unreviewed pushes to main slip past; ask a human via the block.
//   - Settings load and strategy is not pr → return false.
//   - Settings load and strategy is pr → return true.
//
// Note this is intentionally NOT the same helper as refineryAllowedForPR:
// that one requires GT_REFINERY and is about granting `gh pr create`
// permission to the refinery only. Here we want the strategy fact
// independently of which agent is calling.
func currentRigRequiresPRStrategy() bool {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return false
	}

	rigName := identifyCurrentRig(townRoot)
	if rigName == "" {
		return false
	}

	rigPath := filepath.Join(townRoot, rigName)
	settings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	if err != nil {
		// In a rig workspace but settings unreadable — fail closed.
		// Defense-in-depth: an unreviewed push to main is a bigger
		// problem than a noisy block the user can investigate.
		return true
	}
	if settings == nil || settings.MergeQueue == nil {
		// No merge_queue block → no policy → let the push through.
		return false
	}
	return settings.MergeQueue.MergeStrategy == config.MergeStrategyPR
}

// identifyCurrentRig resolves the rig name from GT_RIG (preferred) or
// from cwd relative to townRoot. Returns "" if the rig can't be
// identified; callers treat that as "no policy" (fail open).
//
// Mirrors the GT_RIG-preference logic in refineryAllowedForPR, with the
// same defense-in-depth escape guard against path-separator injection.
func identifyCurrentRig(townRoot string) string {
	if rig := strings.TrimSpace(os.Getenv("GT_RIG")); rig != "" {
		if strings.ContainsAny(rig, "/\\") || rig == ".." || rig == "." {
			return ""
		}
		return rig
	}
	cwd, err := filepath.Abs(".")
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(townRoot, cwd)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "..") {
		return ""
	}
	parts := strings.Split(rel, "/")
	if len(parts) == 0 || parts[0] == "" || parts[0] == "." {
		return ""
	}
	rig := parts[0]
	if strings.ContainsAny(rig, "/\\") || rig == ".." || rig == "." {
		return ""
	}
	return rig
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
