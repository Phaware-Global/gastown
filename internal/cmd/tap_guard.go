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
  push-main          - Block git push to origin/main under merge_strategy=pr
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
	Short: "Block PR creation, feature branches, and direct merges",
	Long: `Block PR-workflow operations in Gas Town.

Invoked as a Claude Code PreToolUse hook on every Bash tool call. The
hook is only installed in Gas Town agent session settings files
(~/gt/<rig>/<role>/.claude/settings.json), so the guard's invocation
itself is the agent-context signal — it does NOT depend on env-var
detection (PR #58 dogfood incident showed env-var detection is
fragile and drifts out of sync with the launch path).

This guard blocks:
  - gh pr merge / gh api .../merge — UNCONDITIONAL block. Refineries
    must route merges through 'gt refinery pr merge' which enforces
    the PR.4 await-review, PR.6 unresolved-threads, and PR.6
    wait-approval gates (G21 / G40 fixes).
  - gh pr create — blocked unless the caller is a refinery on a rig
    configured with merge_strategy=pr (refineryAllowedForPR check).
    Polecats, witnesses, crew, deacons, mayors are always blocked
    from creating PRs directly.
  - git checkout -b / git switch -c — feature branches are not the
    Gas Town flow; agents push to the rig's base branch.

Maintainer-origin specialization: when origin is steveyegge/gastown,
gh pr create gets a maintainer-specific banner ("push to main, don't
PR against your own repo") instead of the generic agent banner.

Exit codes:
  0 - Allowed (command isn't guarded, or refinery-on-PR-mode-rig
      exception applies)
  2 - BLOCKED (forbidden command, OR uninterpretable stdin payload
      while hook is firing — fail-closed against malformed payloads)

Human operators are not affected: they don't have this hook installed
in their personal shells, so the guard never fires for them.`,
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
	// Pipe stdin (the production hook path) MUST yield a parseable
	// command. Three states are distinguished:
	//
	//   1. Stdin is a TTY → manual invocation from a shell, no
	//      command to evaluate, allow.
	//   2. Stdin is a pipe AND read fails → infrastructure glitch,
	//      fail closed (we can't see what was attempted).
	//   3. Stdin is a pipe AND extractCommand returns "" → payload
	//      was unparseable JSON or missing tool_input.command, fail
	//      closed (a malformed hook payload should not silently
	//      become a bypass — same threat model as a read failure).
	//
	// Only state 1 reaches the "manual invocation" allow path. State
	// 2 + 3 collapse to fail-closed.
	var (
		command           string
		stdinUninterpretable bool
	)
	if !isStdinTerminal() {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			style.PrintWarning("tap guard pr-workflow: stdin read failed (%v) — failing closed", err)
			stdinUninterpretable = true
		} else {
			command = extractCommand(input)
			if command == "" {
				// Pipe stdin succeeded but the payload didn't yield a
				// command field — same fail-closed treatment as a read
				// error. A hook firing with an unparseable payload is
				// not a license to allow the underlying tool call.
				style.PrintWarning("tap guard pr-workflow: stdin payload yielded empty command (unparseable JSON or missing tool_input.command) — failing closed")
				stdinUninterpretable = true
			}
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
	// Fail-closed on uninterpretable stdin (read error OR unparseable
	// payload). The hook firing already established we're in an agent
	// context; with no command payload to inspect, the safest
	// behavior is to refuse the tool call entirely. The agent sees
	// the banner and either retries (transient glitch — rare) or
	// escalates.
	if stdinUninterpretable {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "╔════════════════════════════════════════════════════════════════════════╗")
		fmt.Fprintln(os.Stderr, "║  ❌ TAP GUARD FAILED CLOSED                                             ║")
		fmt.Fprintln(os.Stderr, "╠════════════════════════════════════════════════════════════════════════╣")
		fmt.Fprintln(os.Stderr, "║  The tap-guard could not read the tool-call payload from stdin and    ║")
		fmt.Fprintln(os.Stderr, "║  refuses to allow guarded patterns through without inspection.        ║")
		fmt.Fprintln(os.Stderr, "║  This is rare — Claude Code always provides a pipe. If it persists,   ║")
		fmt.Fprintln(os.Stderr, "║  escalate to the mayor via `gt mail send mayor/`.                     ║")
		fmt.Fprintln(os.Stderr, "╚════════════════════════════════════════════════════════════════════════╝")
		fmt.Fprintln(os.Stderr, "")
		return NewSilentExit(2)
	}
	// If stdin was empty/terminal (e.g. guard invoked manually for
	// testing) or the command IS a guarded command, fall through
	// to the block-or-allow logic.

	// **The hook-firing IS the context signal.** This guard runs only
	// because a PreToolUse `Bash` matcher fired, and that matcher is
	// only installed in Gas Town agent session settings files
	// (~/gt/<rig>/<role>/.claude/settings.json). A human operator
	// running `gh pr merge` from their own terminal does not have the
	// hook installed, so the guard never fires for them. Therefore:
	// when this guard runs, we are by construction in a Gas Town
	// agent session, and forbidden patterns (`gh pr merge`,
	// `gh pr create` outside the refinery-on-PR-mode-rig exception)
	// must be blocked unconditionally.
	//
	// The previous design predicated the block on env-var detection
	// (GT_POLECAT, GT_REFINERY, etc.). That detection drifted out of
	// sync with the launch path — production sessions set GT_ROLE
	// and GT_RIG, not GT_REFINERY — and the env-var-based gate
	// returned false, letting a refinery's `gh pr merge` slip past
	// (PR #58 dogfood incident). Removing the gate eliminates the
	// test/prod skew and makes the block a structural property of
	// "the hook ran" rather than a property of which env vars
	// happen to be set.

	// `gh pr merge` (and `gh api .../merge`) are NEVER allowed under
	// this hook. The refinery is the only Gas Town agent with merge
	// authority, and the refinery must route merges through
	// `gt refinery pr merge <n>` which enforces PR.4 await-review,
	// PR.6 unresolved-threads, and PR.6 wait-approval before calling
	// the GitHub merge API. The gt subcommand's internal subprocess
	// to `gh pr merge` does not re-trigger the PreToolUse hook (it's
	// a child process, not a Bash tool call from Claude Code), so
	// that legitimate path is not blocked here.
	if isMerge {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "╔════════════════════════════════════════════════════════════════════════╗")
		fmt.Fprintln(os.Stderr, "║  ❌ DIRECT MERGE BLOCKED (G21 / G40 fix)                                ║")
		fmt.Fprintln(os.Stderr, "╠════════════════════════════════════════════════════════════════════════╣")
		fmt.Fprintln(os.Stderr, "║  PR merges in Gas Town must go through the refinery's imperative       ║")
		fmt.Fprintln(os.Stderr, "║  subcommand, which runs the await-review, threads-resolved, and        ║")
		fmt.Fprintln(os.Stderr, "║  approval gates before calling the GitHub merge API.                   ║")
		fmt.Fprintln(os.Stderr, "║                                                                        ║")
		fmt.Fprintln(os.Stderr, "║  Use:    gt refinery pr merge <pr-number>                             ║")
		fmt.Fprintln(os.Stderr, "║  Block:  gh pr merge <n>  /  gh api repos/.../pulls/<n>/merge          ║")
		fmt.Fprintln(os.Stderr, "║                                                                        ║")
		fmt.Fprintln(os.Stderr, "║  IF gt refinery pr merge errors out:                                   ║")
		fmt.Fprintln(os.Stderr, "║    1. Read the error message — it names the gate that's blocking.     ║")
		fmt.Fprintln(os.Stderr, "║    2. NeedsReviewResolution → run `gt refinery pr dispatch-review-fix` ║")
		fmt.Fprintln(os.Stderr, "║       to send the polecat back to address unresolved review threads.  ║")
		fmt.Fprintln(os.Stderr, "║    3. NeedsApproval / NeedsCI → wait for the gate to clear, retry.    ║")
		fmt.Fprintln(os.Stderr, "║    4. Persistent operational error → escalate to the mayor via         ║")
		fmt.Fprintln(os.Stderr, "║       `gt mail send mayor/`. DO NOT bypass with gh pr merge.          ║")
		fmt.Fprintln(os.Stderr, "║                                                                        ║")
		fmt.Fprintln(os.Stderr, "║  See: docs/design/refinery-pr-workflow.md §G21, §G40                  ║")
		fmt.Fprintln(os.Stderr, "╚════════════════════════════════════════════════════════════════════════╝")
		fmt.Fprintln(os.Stderr, "")
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	}

	// `gh pr create` (and `git checkout -b` / `git switch -c` for
	// feature branches): the refinery on a PR-mode rig is the ONLY
	// session permitted to create PRs. Other agents (polecats, crew,
	// witness, deacon, mayor) are blocked. A refinery on a non-PR-mode
	// rig is blocked because the rig hasn't opted in.
	if isCreateOrBranch {
		if refineryAllowedForPR() {
			return nil
		}
		// Distinguish maintainer-origin from generic agent contexts so
		// the rejection cites the actually-relevant remediation. The
		// maintainer-origin path is the historical case (operator's
		// own clone of steveyegge/gastown — push to main, don't PR
		// against your own repo); other agent contexts get the
		// refinery-routing message.
		if isMaintainerOrigin() {
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
			fmt.Fprintln(os.Stderr, "║  ❌ PR BLOCKED — MAINTAINER ORIGIN                               ║")
			fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
			fmt.Fprintln(os.Stderr, "║  Your origin is steveyegge/gastown — push directly to main.    ║")
			fmt.Fprintln(os.Stderr, "║  PRs are for external contributors, not maintainers.            ║")
			fmt.Fprintln(os.Stderr, "║                                                                  ║")
			fmt.Fprintln(os.Stderr, "║  Use:    git push origin main                                   ║")
			fmt.Fprintln(os.Stderr, "║  Block:  gh pr create / git checkout -b / git switch -c        ║")
			fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
			fmt.Fprintln(os.Stderr, "")
			return NewSilentExit(2)
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "╔════════════════════════════════════════════════════════════════════════╗")
		fmt.Fprintln(os.Stderr, "║  ❌ PR WORKFLOW BLOCKED                                                 ║")
		fmt.Fprintln(os.Stderr, "╠════════════════════════════════════════════════════════════════════════╣")
		fmt.Fprintln(os.Stderr, "║  PR creation in Gas Town is the refinery's responsibility, and only   ║")
		fmt.Fprintln(os.Stderr, "║  on a rig configured with merge_queue.merge_strategy=pr.               ║")
		fmt.Fprintln(os.Stderr, "║                                                                        ║")
		fmt.Fprintln(os.Stderr, "║  Use:    gt refinery pr create --branch <branch> --base <base> \\      ║")
		fmt.Fprintln(os.Stderr, "║                                --title <title> --body-file <file>    ║")
		fmt.Fprintln(os.Stderr, "║  Block:  gh pr create  /  git checkout -b  /  git switch -c           ║")
		fmt.Fprintln(os.Stderr, "║                                                                        ║")
		fmt.Fprintln(os.Stderr, "║  IF gt refinery pr create returns a usage / flag-parse error:          ║")
		fmt.Fprintln(os.Stderr, "║    Run `gt refinery pr create --help` to see the correct flags.        ║")
		fmt.Fprintln(os.Stderr, "║    A flag-parse error is NOT license to fall back to gh pr create.    ║")
		fmt.Fprintln(os.Stderr, "║    The imperative subcommand IS the only allowed path; the bypass is  ║")
		fmt.Fprintln(os.Stderr, "║    structurally blocked here.                                          ║")
		fmt.Fprintln(os.Stderr, "║                                                                        ║")
		fmt.Fprintln(os.Stderr, "║  IF the rig is in merge_strategy=direct mode (no refinery PR pipe):   ║")
		fmt.Fprintln(os.Stderr, "║    Don't open a PR. Push directly to the base branch.                  ║")
		fmt.Fprintln(os.Stderr, "║                                                                        ║")
		fmt.Fprintln(os.Stderr, "║  IF you're a polecat / witness / deacon / mayor (not a refinery):     ║")
		fmt.Fprintln(os.Stderr, "║    PR creation isn't your job. Polecats finish work via `gt done`,    ║")
		fmt.Fprintln(os.Stderr, "║    which hands off to the refinery for the create + merge pipeline.   ║")
		fmt.Fprintln(os.Stderr, "║                                                                        ║")
		fmt.Fprintln(os.Stderr, "║  See: docs/design/refinery-pr-workflow.md (PR.2 step)                 ║")
		fmt.Fprintln(os.Stderr, "╚════════════════════════════════════════════════════════════════════════╝")
		fmt.Fprintln(os.Stderr, "")
		return NewSilentExit(2)
	}

	// Empty stdin (manual invocation from a terminal) AND no command
	// was extracted — nothing to act on. Allow.
	return nil
}

// isGasTownAgentContext returns true if we're running as a Gas Town
// managed agent. Used by the bd-init, mol-patrol, and memory-write
// guards (the pr-workflow guard does NOT call this — see runTapGuard
// PRWorkflow for why "the hook firing IS the context").
//
// The check looks for env vars set by Gas Town session management.
// Two generations of env-var conventions are accepted because the
// launch path has evolved:
//
//   - Legacy role-specific vars (GT_POLECAT, GT_REFINERY, GT_WITNESS,
//     GT_DEACON, GT_MAYOR, GT_CREW) — set by older session bootstraps
//     and by tests. Not all of these are set in current production
//     sessions, but they're checked first for backwards compatibility.
//   - Generic role/rig vars (GT_ROLE, GT_RIG, GT_TOWN_ROOT) — set by
//     the current session-launch path on every Gas Town agent
//     session (refinery, witness, polecat, mayor, deacon, crew). At
//     least one of these is always set in production.
//
// Drift between this check and the launch path was the root cause of
// the PR #58 dogfood incident: GT_REFINERY was the only refinery
// signal here, but production refinery sessions only have GT_ROLE
// and GT_RIG set. The check returned false for a real refinery
// session and the pr-workflow guard let `gh pr merge` through.
//
// CWD-based fallback: even with no env vars, a cwd under a /crew/
// or /polecats/ subtree OF THE TOWN ROOT is sufficient evidence of
// agent context. The town-root scoping is load-bearing — a bare
// `strings.Contains(cwd, "/crew/")` would false-positive on any
// operator path that happens to include those segments (e.g.,
// /Users/anyone/crew/foo). Resolves the town root via
// workspace.FindFromCwd; if that fails, the path-based check is
// skipped (the env-var check above is the primary signal anyway).
func isGasTownAgentContext() bool {
	envVars := []string{
		// Legacy role-specific (kept for backwards compat / tests).
		"GT_POLECAT",
		"GT_CREW",
		"GT_WITNESS",
		"GT_REFINERY",
		"GT_MAYOR",
		"GT_DEACON",
		// Production role/rig vars (always set on launch).
		"GT_ROLE",
		"GT_RIG",
		"GT_TOWN_ROOT",
	}
	for _, env := range envVars {
		if os.Getenv(env) != "" {
			return true
		}
	}

	// Path-based fallback, scoped to the town root. We only accept
	// a CWD as agent-context if it lives under <town-root>/<rig>/
	// (crew|polecats)/. Without the town-root scoping the check
	// would false-positive on any operator path containing /crew/
	// or /polecats/ as a substring.
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		// No town root resolvable from cwd — the path-based fallback
		// can't run safely (we can't tell whether cwd is "under a
		// town root" without knowing where one is). Returning false
		// here means "I can't confirm agent context from the path";
		// the env-var checks above were the primary signal and have
		// already returned true if an agent indicator was present.
		// Callers that interpret a false return as "allow" (e.g., the
		// bd-init / mol-patrol / memory-write guards) are doing so
		// because those guards have other layers (tool matchers,
		// argument parsing) that catch the dangerous shapes. The
		// pr-workflow guard does NOT use this function on its block
		// path — see runTapGuardPRWorkflow header comment for why
		// "the hook firing IS the context".
		return false
	}
	rel, err := filepath.Rel(townRoot, cwd)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	// rel starting with ".." means cwd is outside townRoot (symlink
	// edge cases) — definitively not a managed agent.
	if strings.HasPrefix(rel, "..") {
		return false
	}
	// Match <rig>/crew/... or <rig>/polecats/... structure. Accept
	// the segment anywhere from path[1:] onwards to allow nested
	// agent worktrees (e.g., <rig>/polecats/<polecat>/<rig> for
	// polecat clones).
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if parts[i] == "crew" || parts[i] == "polecats" {
			return true
		}
	}
	return false
}

// refineryAllowedForPR returns true when the guard should permit PR-workflow
// commands (gh pr create, etc.) because:
//   - the caller is the refinery, AND
//   - the current rig's merge_queue config has merge_strategy=pr
//
// All other agents (polecats, crew, witness, deacon, mayor) remain blocked
// even when the rig uses merge_strategy=pr — PR creation is a refinery-only
// responsibility under the PR workflow. Fails closed on any lookup error,
// which keeps polecats blocked when the env is degraded.
//
// The "is refinery?" check accepts two env-var shapes for backwards
// compatibility with older session bootstraps:
//   - GT_REFINERY=<rig>: legacy refinery indicator, set by older bootstraps
//     and tests.
//   - GT_ROLE=<rig>/refinery (or GT_ROLE=refinery): the current production
//     shape. Used everywhere by `gt up` today — GT_REFINERY is no longer
//     set on production refinery sessions, which was the PR #58 dogfood
//     bug: this function returned false for real refinery sessions because
//     it only knew the legacy shape.
func refineryAllowedForPR() bool {
	if !isRefineryRole() {
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

// isRefineryRole returns true when env vars indicate this session is a
// refinery. Accepts both the legacy shape (GT_REFINERY non-empty) and
// the production shape (GT_ROLE ending in "refinery", e.g.
// "gastown/refinery", or GT_ROLE == "refinery"). Production refinery
// sessions set GT_ROLE but not GT_REFINERY; tests typically set
// GT_REFINERY. Both must be honored.
func isRefineryRole() bool {
	if os.Getenv("GT_REFINERY") != "" {
		return true
	}
	role := strings.TrimSpace(os.Getenv("GT_ROLE"))
	if role == "" {
		return false
	}
	// Accept ONLY two exact shapes:
	//   - "refinery" (ungrouped, single segment)
	//   - "<rig>/refinery" (current gt-up shape, exactly two segments)
	//
	// A naive HasSuffix check would also match deeper paths like
	// "gastown/polecats/refinery" — i.e., a polecat that happened to
	// be named "refinery" would falsely gain refinery-only allowances.
	// Use strings.Cut to require exactly one slash in the role, and
	// verify the segment before the slash is non-empty (rejects "/refinery").
	if role == "refinery" {
		return true
	}
	rig, suffix, hasSlash := strings.Cut(role, "/")
	if !hasSlash {
		// No slash and not "refinery" — not a refinery role.
		return false
	}
	// strings.Cut returns the part AFTER the first slash in `suffix`.
	// For "<rig>/refinery", suffix == "refinery"; for the polecat
	// shape "gastown/polecats/refinery", suffix == "polecats/refinery".
	// Requiring suffix to equal exactly "refinery" rejects deeper
	// paths.
	return rig != "" && suffix == "refinery"
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
