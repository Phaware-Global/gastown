package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

// prWorkflowCommandPatterns matches the forbidden commands this guard
// covers. Each pattern allows the target command to appear at one of
// three boundaries: (1) line start (possibly indented); (2) after a
// shell command-separator operator (|, &&, ||, ;, ( — the characters
// that end the previous command in a compound); (3) immediately after
// a `-c '` or `-c "` wrapper (sh -c / bash -c / dash -c). Together
// those cover every real polecat invocation shape the 2026-04-20
// dogfood surfaced (G19b) without false-positives on mentions inside
// double- or single-quoted string literals like `echo "gh pr create"`.
//
// Patterns deliberately do NOT use a plain word boundary (\b) before
// the command. Word boundaries match inside quoted strings, which
// would block `echo "Don't run: gh pr create"` — a legitimate
// diagnostic/logging command. The "at-boundary-or-wrapper" rule is
// the tightest pattern that catches real invocations without that
// false positive.
//
// The sh -c wrapper boundary is only applied to `gh pr create`
// because the wrapper boundary regex fragment `(-c\s+['"])` would
// accidentally match the `-c` option of `git switch -c`. Keeping the
// wrapper boundary scoped to the gh pattern is the simplest safe
// option.
var prWorkflowCommandPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?m)(^\s*|[|&;(]\s*|-c\s+['"]\s*)gh\s+pr\s+create\b`),
	regexp.MustCompile(`(?m)(^\s*|[|&;(]\s*)git\s+checkout\s+-b\b`),
	regexp.MustCompile(`(?m)(^\s*|[|&;(]\s*)git\s+switch\s+-c\b`),
}

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

// extractBashToolCommand pulls tool_input.command out of the Claude
// Code hook input. Parallels extractCommand in tap_guard_dangerous.go;
// keeping a per-guard extractor means each guard can fail-open
// independently on parse errors.
func extractBashToolCommand(input []byte) string {
	if len(input) == 0 {
		return ""
	}
	var hookInput struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(input, &hookInput); err != nil {
		return ""
	}
	return hookInput.ToolInput.Command
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
  - gh pr create
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
	// Fast path: if stdin carries a Bash command AND that command
	// isn't one we guard, exit clean immediately. Per-invocation
	// overhead is one fork+exec + a regex match for the common case.
	input, _ := io.ReadAll(os.Stdin)
	if command := extractBashToolCommand(input); command != "" && !isPRWorkflowCommand(command) {
		return nil
	}
	// If stdin was empty (e.g. guard invoked manually for testing)
	// or the command IS a PR-workflow command, fall through to the
	// existing block-or-allow logic.

	// Check if we're in a Gas Town agent context
	if isGasTownAgentContext() {
		// Exception: the refinery running for a rig configured with
		// merge_strategy=pr legitimately needs to call `gh pr create` and
		// `gh pr merge` as part of its normal workflow. Polecats and other
		// agents are still blocked.
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

	// Check if origin is the maintainer's repo (steveyegge/gastown)
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
