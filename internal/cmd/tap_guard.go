package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// prWorkflowCommandPatterns matches the forbidden commands this guard
// covers. Each pattern allows the target command to appear at one of
// three boundaries: (1) line start (possibly indented); (2) after a
// shell command-separator operator in the set `` [|&;(`] ``
// (backtick inclusion covers command-substitution shapes like
// `` `gh pr create` ``, flagged in iter-1 review); (3) immediately
// after a shell `-c` wrapper — `sh -c '...'`, `bash -lc "..."`,
// `dash -ic '...'`, etc. The wrapper-boundary fragment accepts any
// `-<short-opts>c` form (with `c` as the LAST short opt) to cover
// common login-shell (`-lc`) and interactive-shell (`-ic`)
// invocations alongside plain `-c`. Flagged in iter-2 review.
//
// Not covered by design (documented intentional limits rather than
// silent gaps):
//   - Wrappers like `env FOO=1 gh pr create` or `command gh pr
//     create` require shell-aware parsing. The cwd+env-var-based
//     agent context check in isGasTownAgentContext() +
//     refineryAllowedForPR() still applies — the guard WILL block
//     them if it fires. Expanding the regex to catch `env X=Y ...`
//     broadens the false-positive surface (any value with `gh pr
//     create` in it) more than the real-world benefit justifies.
//   - Quoted-string corner cases like `echo "|| gh pr create"` can
//     match because the boundary set `` [|&;(`] `` doesn't know it's
//     inside a quote. Accepted: over-block on an unusual diagnostic
//     command beats silently missing a real PR-creation.
//
// Patterns deliberately do NOT use a plain word boundary (\b) before
// the command — that matches inside all quoted strings and blocks
// too many legitimate diagnostic/logging commands.
//
// The -c-wrapper boundary is only applied to `gh pr create` because
// its regex fragment would accidentally match the `-c` option of
// `git switch -c`. Keeping the wrapper boundary scoped to the gh
// pattern is the simplest safe option.
var prWorkflowCommandPatterns = []*regexp.Regexp{
	regexp.MustCompile("(?m)(^\\s*|[|&;(`]\\s*|-[a-z]*c\\s+['\"]\\s*)gh\\s+pr\\s+create\\b"),
	regexp.MustCompile("(?m)(^\\s*|[|&;(`]\\s*)git\\s+checkout\\s+-b\\b"),
	regexp.MustCompile("(?m)(^\\s*|[|&;(`]\\s*)git\\s+switch\\s+-c\\b"),
}

// prMergeCommandPattern matches direct `gh pr merge` invocations. This is
// split out from prWorkflowCommandPatterns because the refinery-allowed
// exception for PR-mode rigs covers `gh pr create` but NOT `gh pr merge`
// (G21 fix). The refinery must route merges through `gt refinery pr merge`
// — that subcommand enforces PR.6 wait-approval gates via VerifyPRApproval
// before calling the provider's MergePR. Shelling out to `gh pr merge`
// directly is the exact G21 bypass this guard closes.
//
// The boundary set matches prWorkflowCommandPatterns (line-start, shell
// operator, or shell `-c` wrapper) so heredoc/wrapper variants are caught
// consistently with G19b's existing coverage.
var prMergeCommandPattern = regexp.MustCompile("(?m)(^\\s*|[|&;(`]\\s*|-[a-z]*c\\s+['\"]\\s*)gh\\s+pr\\s+merge\\b")

// prMergeViaApiPattern matches the GitHub merge endpoint reached through
// `gh api repos/<owner>/<repo>/pulls/<n>/merge`. Closes the G40 sibling of
// the G21 bypass: a refinery LLM that hits the tap-guard on `gh pr merge`
// can fall back to the same operation via the raw API. The path segment
// alone uniquely identifies the merge endpoint, so any match — GET probe
// or PUT mutation — is treated as a merge attempt; if a refinery wants to
// inspect mergeability it should use `gh pr view --json mergeable`,
// which the guard does not block.
//
// Anchoring details:
//   - `(?s)` mode makes `.` cross newlines so a multi-line invocation
//     (e.g. `gh api ... \` continuation followed by `pulls/<n>/merge`
//     on the next line) still matches — the iter-1 review flagged the
//     original `[^\n]*` form as bypassable via a backslash-newline
//     escape.
//   - The PR-number segment is `[^/\s]+` rather than `[0-9]+` so shell
//     variables (`$PR`), gh placeholders (`:number`), and command
//     substitution (`` `cmd` ``) match too — the digit-only form was
//     bypassable by anything that wasn't a literal integer.
var prMergeViaApiPattern = regexp.MustCompile(
	"(?ms)(^\\s*|[|&;(`]\\s*|-[a-z]*c\\s+['\"]\\s*)gh\\s+api\\b.*?pulls/[^/\\s]+/merge\\b")

// isPRWorkflowCommand returns true when cmd looks like any of the PR-
// creation / feature-branch commands this guard blocks.
func isPRWorkflowCommand(cmd string) bool {
	if cmd == "" {
		return false
	}
	for _, p := range prWorkflowCommandPatterns {
		if p.MatchString(cmd) {
			return true
		}
	}
	return false
}

// isPRMergeCommand returns true when cmd looks like a direct `gh pr merge`
// invocation OR an `gh api …pulls/<n>/merge` API-level merge (G40). Both
// shapes hit the same GitHub merge endpoint and bypass the
// `gt refinery pr merge` approval gate. Any match returns true — there is
// no method-aware filtering; a GET probe on the merge path is blocked
// alongside a PUT mutation, which is intentional defense-in-depth.
// Separate from isPRWorkflowCommand because merge and create have
// different refinery-context policies (G21 fix).
func isPRMergeCommand(cmd string) bool {
	if cmd == "" {
		return false
	}
	return prMergeCommandPattern.MatchString(cmd) || prMergeViaApiPattern.MatchString(cmd)
}

var tapGuardCmd = &cobra.Command{
	Use:   "guard",
	Short: "Block forbidden operations (PreToolUse hook)",
	Long: `Block forbidden operations via Claude Code PreToolUse hooks.

Guard commands exit with code 2 to BLOCK tool execution when a policy
is violated. They're called before the tool runs, preventing the
forbidden operation entirely.

Available guards:
  pr-workflow        - Block PR creation and feature branches
  bd-init            - Block bd init in wrong directories
  mol-patrol         - Block mol patrol from agent contexts
  dangerous-command  - Block rm -rf, force push, hard reset, git clean
  memory-write       - Block Write/Edit/NotebookEdit to Claude Code memory paths for worker roles

External guards (standalone scripts, not compiled into gt):
  context-budget   - scripts/guards/context-budget-guard.sh

Example hook configuration:
  {
    "PreToolUse": [{
      "matcher": "Bash(gh pr create*)",
      "hooks": [{"command": "gt tap guard pr-workflow"}]
    }]
  }`,
}

var tapGuardPRWorkflowCmd = &cobra.Command{
	Use:   "pr-workflow",
	Short: "Block PR creation and feature branches",
	Long: `Block PR workflow operations in Gas Town.

Gas Town workers push directly to main. PRs add friction that breaks
the autonomous execution model (GUPP principle).

This guard blocks:
  - gh pr create (allowed for refinery on pr-mode rigs only)
  - gh pr merge (blocked for ALL agents — refinery must use
    'gt refinery pr merge' which enforces PR.6 wait-approval, G21 fix)
  - git checkout -b (feature branches)
  - git switch -c (feature branches)

Exit codes:
  0 - Operation allowed (not in Gas Town agent context, not maintainer origin)
  2 - Operation BLOCKED (in agent context OR maintainer origin)

The guard blocks in two scenarios:
  1. Running as a Gas Town agent (crew, polecat, witness, etc.)
  2. Origin remote is steveyegge/gastown (maintainer should push directly)

Humans running outside Gas Town with a fork origin can still use PRs.`,
	RunE: runTapGuardPRWorkflow,
}

func init() {
	tapCmd.AddCommand(tapGuardCmd)
	tapGuardCmd.AddCommand(tapGuardPRWorkflowCmd)
}

func runTapGuardPRWorkflow(cmd *cobra.Command, args []string) error {
	// Read tool_input from stdin. With the catch-all `Bash` matcher
	// in the hook config, every Bash call reaches this guard — the
	// guard itself decides whether the specific command is relevant.
	// Before G19b, the matcher was `Bash(gh pr create*)` + siblings;
	// Claude Code's glob matching doesn't cross newlines, so a multi-
	// line `gh pr create` (with a heredoc body, etc.) slipped past.
	// Moving the pattern check inside the guard removes that
	// fragility.
	//
	// Skip the stdin read entirely when stdin is a terminal — that
	// happens when a human invokes `gt tap guard pr-workflow`
	// directly from a shell (the G19b iter-1 review flagged this as
	// a hang risk). Under Claude Code hooks, stdin is always a pipe
	// carrying the hook JSON, never a terminal.
	var command string
	if !isStdinTerminal() {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			// Read failure is unusual (Claude Code always provides a
			// pipe). Fail-closed into the block-or-allow logic below;
			// letting a real pr-workflow command slip past is worse
			// than over-blocking a manual-invocation edge case.
			style.PrintWarning("tap guard pr-workflow: stdin read failed (%v) — falling back to agent-context block", err)
		} else {
			command = extractCommand(input)
		}
	}
	// Fast path: if we successfully extracted a Bash command AND that
	// command isn't one we guard (neither create/branch nor direct
	// merge), exit clean immediately.
	isMerge := isPRMergeCommand(command)
	isCreateOrBranch := isPRWorkflowCommand(command)
	if command != "" && !isCreateOrBranch && !isMerge {
		return nil
	}
	// If stdin was empty/terminal (e.g. guard invoked manually for
	// testing) or the command IS a guarded command, fall through
	// to the block-or-allow logic.

	// G21 fix: direct `gh pr merge` is blocked for ALL Gas Town agents,
	// including the refinery — the refinery exception that covers
	// `gh pr create` does NOT extend to merge. The refinery must route
	// merges through `gt refinery pr merge <n>` which enforces PR.6
	// wait-approval via refinery.VerifyPRApproval. Shelling out to
	// `gh pr merge` directly is the G21 bypass shape: create-then-
	// merge back-to-back without running PR.4/PR.5/PR.6.
	//
	// The gt subcommand internally subprocesses `gh pr merge` — that
	// subprocess is not a Bash tool call from Claude Code, so the
	// PreToolUse hook doesn't fire on it. Only direct Bash invocations
	// from the LLM agent are guarded here.
	if isMerge && isGasTownAgentContext() {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
		fmt.Fprintln(os.Stderr, "║  ❌ DIRECT MERGE BLOCKED (G21 / G40 fix)                         ║")
		fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
		fmt.Fprintln(os.Stderr, "║  Refinery must route PR merges through `gt refinery pr merge`   ║")
		fmt.Fprintln(os.Stderr, "║  which enforces PR.6 wait-approval gates before calling the     ║")
		fmt.Fprintln(os.Stderr, "║  GitHub merge API.                                              ║")
		fmt.Fprintln(os.Stderr, "║                                                                  ║")
		fmt.Fprintln(os.Stderr, "║  Instead of:  gh pr merge <n> --squash                          ║")
		fmt.Fprintln(os.Stderr, "║  Or:          gh api repos/.../pulls/<n>/merge -X PUT (G40)     ║")
		fmt.Fprintln(os.Stderr, "║  Do this:     gt refinery pr merge <n>                          ║")
		fmt.Fprintln(os.Stderr, "║                                                                  ║")
		fmt.Fprintln(os.Stderr, "║  See: docs/design/refinery-pr-workflow.md §G21, §G40            ║")
		fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
		fmt.Fprintln(os.Stderr, "")
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	}

	// Check if we're in a Gas Town agent context
	if isGasTownAgentContext() {
		// Exception: the refinery running for a rig configured with
		// merge_strategy=pr legitimately needs to call `gh pr create`
		// as part of its normal workflow (PR.2 pr-create step). Polecats
		// and other agents are still blocked. Direct `gh pr merge` was
		// handled above — that's blocked for the refinery too.
		if refineryAllowedForPR() {
			return nil
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
		fmt.Fprintln(os.Stderr, "║  ❌ PR WORKFLOW BLOCKED                                          ║")
		fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
		fmt.Fprintln(os.Stderr, "║  Gas Town workers push directly to main. PRs are forbidden.     ║")
		fmt.Fprintln(os.Stderr, "║                                                                  ║")
		fmt.Fprintln(os.Stderr, "║  Instead of:  gh pr create / git checkout -b / git switch -c    ║")
		fmt.Fprintln(os.Stderr, "║  Do this:     git add . && git commit && git push origin main   ║")
		fmt.Fprintln(os.Stderr, "║                                                                  ║")
		fmt.Fprintln(os.Stderr, "║  Why? PRs add friction that breaks autonomous execution.        ║")
		fmt.Fprintln(os.Stderr, "║  See: ~/gt/docs/PRIMING.md (GUPP principle)                     ║")
		fmt.Fprintln(os.Stderr, "║                                                                  ║")
		fmt.Fprintln(os.Stderr, "║  Refineries: set merge_queue.merge_strategy=pr on the rig to    ║")
		fmt.Fprintln(os.Stderr, "║  allow PR creation through the refinery PR workflow.            ║")
		fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
		fmt.Fprintln(os.Stderr, "")
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	}

	// Check if origin is the maintainer's repo (steveyegge/gastown).
	// The maintainer-origin block targets create/feature-branch commands
	// (the philosophy: maintainers push directly to main rather than open
	// PRs against their own repo). It does NOT cover `gh pr merge` — a
	// maintainer who happens to land an incoming contributor PR with
	// `gh pr merge` from a maintainer clone is exercising normal review
	// workflow, not the `create-your-own-PR-to-your-own-repo` antipattern
	// this block exists to stop. The earlier agent-context branch already
	// blocked `gh pr merge` for Gas Town agents regardless of origin.
	if isMerge {
		return nil
	}
	if isMaintainerOrigin() {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
		fmt.Fprintln(os.Stderr, "║  ❌ PR BLOCKED - MAINTAINER ORIGIN                               ║")
		fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
		fmt.Fprintln(os.Stderr, "║  Your origin is steveyegge/gastown - push directly to main.     ║")
		fmt.Fprintln(os.Stderr, "║  PRs are for external contributors, not maintainers.            ║")
		fmt.Fprintln(os.Stderr, "║                                                                  ║")
		fmt.Fprintln(os.Stderr, "║  Instead of:  gh pr create                                      ║")
		fmt.Fprintln(os.Stderr, "║  Do this:     git push origin main                              ║")
		fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
		fmt.Fprintln(os.Stderr, "")
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	}

	// Not in Gas Town context and not maintainer origin - allow PRs
	return nil
}

// isGasTownAgentContext returns true if we're running as a Gas Town managed agent.
func isGasTownAgentContext() bool {
	// Check environment variables set by Gas Town session management
	envVars := []string{
		"GT_POLECAT",
		"GT_CREW",
		"GT_WITNESS",
		"GT_REFINERY",
		"GT_MAYOR",
		"GT_DEACON",
	}
	for _, env := range envVars {
		if os.Getenv(env) != "" {
			return true
		}
	}

	// Also check if we're in a crew or polecat worktree by path
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}

	agentPaths := []string{"/crew/", "/polecats/"}
	for _, path := range agentPaths {
		if strings.Contains(cwd, path) {
			return true
		}
	}

	return false
}

// refineryAllowedForPR returns true when the guard should permit PR-workflow
// commands (gh pr create, etc.) because:
//   - the caller is the refinery (GT_REFINERY is set), AND
//   - the current rig's merge_queue config has merge_strategy=pr
//
// All other agents (polecats, crew, witness, deacon, mayor) remain blocked
// even when the rig uses merge_strategy=pr — PR creation is a refinery-only
// responsibility under the PR workflow. Fails closed on any lookup error,
// which keeps polecats blocked when the env is degraded.
func refineryAllowedForPR() bool {
	if os.Getenv("GT_REFINERY") == "" {
		return false
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return false
	}

	// Prefer GT_RIG when set — Gas Town sets it for refinery sessions and it
	// is a more reliable identifier than CWD. The CWD path can be the town
	// root, the mayor/rig worktree, or any ad-hoc location when a hook fires
	// (e.g., pre-tool hooks invoked from a prompt outside the rig directory).
	// Fall back to CWD-relative inference for older session bootstraps that
	// don't set GT_RIG yet.
	var rigName string
	if rigName = strings.TrimSpace(os.Getenv("GT_RIG")); rigName == "" {
		cwd, err := filepath.Abs(".")
		if err != nil {
			return false
		}
		rel, err := filepath.Rel(townRoot, cwd)
		if err != nil {
			return false
		}
		rel = filepath.ToSlash(rel)
		// A relative path starting with ".." means cwd is outside townRoot
		// (common with symlink/realpath mismatches). Fail closed rather than
		// let `filepath.Join(townRoot, "..")` escape.
		if strings.HasPrefix(rel, "..") {
			return false
		}
		parts := strings.Split(rel, "/")
		if len(parts) == 0 || parts[0] == "" || parts[0] == "." {
			return false
		}
		rigName = parts[0]
	}

	// Belt-and-suspenders: if rigName somehow resolves outside townRoot
	// (e.g., an env-provided GT_RIG value containing path separators or `..`),
	// refuse to trust it rather than reading settings from an unintended
	// location.
	if strings.ContainsAny(rigName, "/\\") || rigName == ".." || rigName == "." {
		return false
	}

	rigPath := filepath.Join(townRoot, rigName)
	settings, err := config.LoadRigSettings(config.RigSettingsPath(rigPath))
	if err != nil || settings == nil || settings.MergeQueue == nil {
		return false
	}
	return settings.MergeQueue.MergeStrategy == config.MergeStrategyPR
}

// isMaintainerOrigin returns true if the origin remote points to the maintainer's repo.
// This prevents the maintainer from accidentally creating PRs in their own repo.
func isMaintainerOrigin() bool {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	url := strings.TrimSpace(string(output))
	// Match both HTTPS and SSH URL formats:
	// - https://github.com/steveyegge/gastown.git
	// - git@github.com:steveyegge/gastown.git
	return strings.Contains(url, "steveyegge/gastown")
}
