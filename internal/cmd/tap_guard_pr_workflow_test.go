package cmd

import (
	"os"
	"testing"
)

// TestIsRefineryRole_AcceptsBothEnvShapes pins the refinery-role
// detection across the two env-var conventions the launch path has
// used over time. The PR #58 dogfood incident reproduced when this
// check only knew GT_REFINERY (legacy/test shape) but production
// refinery sessions set GT_ROLE=<rig>/refinery instead. Both shapes
// must be detected as "refinery role".
func TestIsRefineryRole_AcceptsBothEnvShapes(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		expected bool
	}{
		{"legacy GT_REFINERY only", map[string]string{"GT_REFINERY": "gastown"}, true},
		{"production GT_ROLE=<rig>/refinery", map[string]string{"GT_ROLE": "gastown/refinery"}, true},
		{"production GT_ROLE=refinery (ungrouped)", map[string]string{"GT_ROLE": "refinery"}, true},
		{"both set (real production overlap)", map[string]string{
			"GT_REFINERY": "gastown",
			"GT_ROLE":     "gastown/refinery",
		}, true},
		{"GT_ROLE for a different role", map[string]string{"GT_ROLE": "gastown/witness"}, false},
		{"GT_ROLE for polecat", map[string]string{"GT_ROLE": "gastown/polecats/furiosa"}, false},
		{"empty environment", map[string]string{}, false},
		// Sanity: a hypothetical "ex-refinery" role must NOT match the
		// HasSuffix check via the slash boundary.
		{"GT_ROLE with refinery as substring (no slash)", map[string]string{"GT_ROLE": "ex-refinery"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear all relevant vars first so leftover env from the
			// shell doesn't pollute the test.
			t.Setenv("GT_REFINERY", "")
			t.Setenv("GT_ROLE", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := isRefineryRole()
			if got != tc.expected {
				t.Errorf("isRefineryRole() with env %v = %v, want %v", tc.env, got, tc.expected)
			}
		})
	}
}

// TestIsGasTownAgentContext_ProductionEnvVars covers the specific bug
// that motivated the broader hook-is-context refactor: production
// sessions set GT_ROLE / GT_RIG / GT_TOWN_ROOT but not the legacy
// GT_REFINERY / GT_POLECAT / etc. The detection must cover both
// generations.
func TestIsGasTownAgentContext_ProductionEnvVars(t *testing.T) {
	productionVars := []string{"GT_ROLE", "GT_RIG", "GT_TOWN_ROOT"}
	for _, v := range productionVars {
		t.Run(v, func(t *testing.T) {
			// Clear the legacy vars so we're testing the production
			// var alone.
			for _, legacy := range []string{"GT_POLECAT", "GT_CREW", "GT_WITNESS",
				"GT_REFINERY", "GT_MAYOR", "GT_DEACON", "GT_ROLE", "GT_RIG", "GT_TOWN_ROOT"} {
				t.Setenv(legacy, "")
			}
			t.Setenv(v, "test-value")
			if !isGasTownAgentContext() {
				t.Errorf("isGasTownAgentContext() with %s set = false, want true (production env shape must be detected)", v)
			}
		})
	}
}

// TestIsGasTownAgentContext_LegacyEnvVars asserts the legacy env
// vars are still recognized — backwards compatibility for older
// session bootstraps and existing test fixtures that set GT_REFINERY
// or GT_POLECAT directly.
func TestIsGasTownAgentContext_LegacyEnvVars(t *testing.T) {
	legacyVars := []string{"GT_POLECAT", "GT_CREW", "GT_WITNESS",
		"GT_REFINERY", "GT_MAYOR", "GT_DEACON"}
	for _, v := range legacyVars {
		t.Run(v, func(t *testing.T) {
			for _, key := range append(legacyVars, "GT_ROLE", "GT_RIG", "GT_TOWN_ROOT") {
				t.Setenv(key, "")
			}
			t.Setenv(v, "test-value")
			if !isGasTownAgentContext() {
				t.Errorf("isGasTownAgentContext() with %s set = false, want true", v)
			}
		})
	}
}

// TestIsGasTownAgentContext_NoEnv asserts the function returns false
// when nothing in the env signals an agent context AND we're not in
// an agent worktree. This is the path a human operator running gh
// from a personal shell takes — the hook isn't installed there
// either, so this test mostly guards against the env-detection
// becoming over-eager and false-positiving on operator shells that
// happen to have an unrelated GT_* var set in the future.
func TestIsGasTownAgentContext_NoEnv(t *testing.T) {
	for _, key := range []string{"GT_POLECAT", "GT_CREW", "GT_WITNESS",
		"GT_REFINERY", "GT_MAYOR", "GT_DEACON", "GT_ROLE", "GT_RIG", "GT_TOWN_ROOT"} {
		t.Setenv(key, "")
	}
	// CWD is the test's tmp dir, which is not under /crew/ or /polecats/
	// — the path-based fallback should also return false.
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir to tmp: %v", err)
	}
	if isGasTownAgentContext() {
		t.Error("isGasTownAgentContext() with no env vars and a clean cwd returned true")
	}
}

// TestIsPRWorkflowCommand pins the command classifier used by the
// pr-workflow guard. The guard's matcher in the hook template is now
// a catch-all `Bash` (G19b fix) so every Bash call reaches the guard;
// this classifier is the fast path that decides whether the specific
// command is one we actually block. A regression in any of these
// patterns would let a polecat slip a `gh pr create` past the guard,
// which is the exact failure mode that reproduced on slit's PR #16.
//
// The positive cases cover shapes Claude Code's original glob-matcher
// `Bash(gh pr create*)` was observed or suspected to miss: multi-line
// commands (heredoc bodies), leading whitespace, shell wrappers,
// backtick substitution, and commands with complex flag substitution.
// All MUST match.
//
// The negative cases cover commands that superficially contain
// "gh pr" or "git checkout" tokens but are not the forbidden shape
// (e.g. `gh pr list`, `git checkout main` without `-b`, commands
// that only mention the strings in string literals).
func TestIsPRWorkflowCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		// Positives — every one of these MUST be caught. Includes the
		// shapes that slipped past the matcher-only guard in the
		// 2026-04-20 dogfood, plus backtick-substitution which the
		// iter-1 review flagged as an additional escape hatch.
		{"bare gh pr create", "gh pr create", true},
		{"gh pr create with flags", "gh pr create --title foo", true},
		{"gh pr create indented", "    gh pr create --title foo", true},
		{"gh pr create tab-indented", "\tgh pr create", true},
		{
			"gh pr create multi-line with heredoc body (slit's PR #16 shape)",
			"gh pr create --title \"feat(telegraph): ...\" --body \"$(cat <<'EOF'\n## Summary\n- foo\nEOF\n)\"",
			true,
		},
		{
			"gh pr create wrapped in sh -c single-line",
			"sh -c 'gh pr create --title foo'",
			true,
		},
		{
			"gh pr create wrapped in bash -lc (login shell)",
			"bash -lc 'gh pr create --title foo'",
			true,
		},
		{
			"gh pr create wrapped in dash -ic (interactive shell)",
			`dash -ic "gh pr create --title foo"`,
			true,
		},
		{
			"gh pr create later in a compound command",
			"git push origin HEAD && \\\n  gh pr create --title foo",
			true,
		},
		{
			"gh pr create in backtick command substitution",
			"URL=`gh pr create --title foo`",
			true,
		},
		{"gh pr create with extra whitespace between words", "gh  pr  create --title x", true},
		{"git checkout -b bare", "git checkout -b feature/foo", true},
		{"git checkout -b indented", "  git checkout -b feature/foo", true},
		{"git switch -c bare", "git switch -c feature/foo", true},
		{"git switch -c indented", "    git switch -c feature/foo", true},
		{"git switch -c in multi-line", "cd /tmp && \\\n  git switch -c feature/foo", true},
		{"git checkout -b in backticks", "REF=`git checkout -b feat/x`", true},

		// Negatives — these must NOT trigger the guard.
		{"empty string", "", false},
		{"gh pr list (not create)", "gh pr list --state open", false},
		{"gh pr view (not create)", "gh pr view 42", false},
		{"gh pr merge (refinery path, different guard if needed)", "gh pr merge 42 --squash", false},
		{"git checkout without -b", "git checkout main", false},
		{"git checkout -- file", "git checkout -- file.txt", false},
		{"git switch without -c", "git switch main", false},
		{
			"command that only mentions gh pr create in a double-quoted string",
			`echo "Don't run: gh pr create"`,
			false,
		},
		{
			"command that only mentions gh pr create in a single-quoted string",
			`echo 'Don\u0027t run: gh pr create'`,
			false,
		},
		{
			"command with # comment mentioning forbidden",
			"ls # would use gh pr create here but we push instead",
			false,
		},
		{
			"unrelated command",
			"go test ./internal/refinery/...",
			false,
		},
		{
			// Case-sensitive: `GH` is not `gh`. The gh CLI is case-
			// sensitive, so `GH PR CREATE` wouldn't invoke it anyway.
			// If we ever need case-insensitive matching, update the
			// regex flag here AND document why.
			"uppercase GH PR CREATE is not a match (gh is case-sensitive)",
			"GH PR CREATE --title foo",
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPRWorkflowCommand(tc.cmd)
			if got != tc.want {
				t.Errorf("isPRWorkflowCommand(%q) = %v; want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestIsPRMergeCommand pins the `gh pr merge` classifier added in the
// G21 fix. Unlike TestIsPRWorkflowCommand, these cases must be
// blocked EVEN for the refinery running on a pr-mode rig — the
// refinery-allow exception covers `gh pr create` but not `gh pr merge`
// (see tap_guard.go:runTapGuardPRWorkflow).
//
// Positive-case coverage mirrors the G19b create pattern: bare
// invocations, shell-operator prefixes (|, &, ;, backtick), and
// `sh -c` / `bash -lc` / `dash -ic` wrapper forms — so a refinery LLM
// can't dodge the block by wrapping the command.
//
// Negative-case coverage protects diagnostic commands that mention
// "gh pr merge" in string-literal / comment positions and unrelated
// `gh pr` subcommands (view, list, checks, etc.).
func TestIsPRMergeCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		// Positives — the G21 bypass shape and its wrapper variants.
		{"bare gh pr merge", "gh pr merge 23", true},
		{"gh pr merge with squash flag", "gh pr merge 23 --squash", true},
		{"gh pr merge with repo flag", "gh pr merge 23 --repo Phaware-Global/gastown --squash", true},
		{"gh pr merge indented", "    gh pr merge 23", true},
		{"gh pr merge tab-indented", "\tgh pr merge 23", true},
		{"gh pr merge after semicolon", "echo starting; gh pr merge 23 --squash", true},
		{"gh pr merge after &&", "go test && gh pr merge 23", true},
		{"gh pr merge after pipe", "echo 23 | gh pr merge", true},
		{"gh pr merge after backtick", "FOO=`gh pr merge 23` echo $FOO", true},
		{"gh pr merge via sh -c", "sh -c 'gh pr merge 23 --squash'", true},
		{"gh pr merge via bash -lc login shell", "bash -lc 'gh pr merge 23 --squash'", true},
		{"gh pr merge via dash -ic interactive shell", "dash -ic 'gh pr merge 23 --squash'", true},

		// Negatives — superficially similar commands that are NOT
		// the bypass shape. Must NOT match or we over-block.
		{"gh pr view (diagnostic)", "gh pr view 23 --json state,mergedAt", false},
		{"gh pr list (diagnostic)", "gh pr list --state open", false},
		{"gh pr checks (diagnostic)", "gh pr checks 23", false},
		{"gh pr edit (non-merge mutation)", "gh pr edit 23 --title foo", false},
		{"gh pr merge in a literal string argument", "echo 'gh pr merge is forbidden'", false},
		{"gh pr merge in a shell comment", "ls # we used to gh pr merge here", false},
		{"command with pr merge but not gh", "git pr merge", false},
		{"empty string", "", false},
		{
			// Case-sensitive: gh is case-sensitive at the command layer.
			"uppercase GH PR MERGE is not a match",
			"GH PR MERGE 23",
			false,
		},

		// G40: the API-level sibling of `gh pr merge`. Same endpoint, different
		// CLI surface. Must be blocked too or the LLM can fall back to the API
		// form when the gh-pr-merge guard fires.
		{"gh api PUT pulls/N/merge", "gh api repos/Phaware-Global/gastown/pulls/41/merge -X PUT", true},
		{"gh api with field flags", "gh api repos/owner/repo/pulls/123/merge -X PUT -f merge_method=squash", true},
		{"gh api merge after pipe", "echo go | gh api repos/o/r/pulls/9/merge -X PUT", true},
		{"gh api merge via sh -c", "sh -c 'gh api repos/o/r/pulls/9/merge -X PUT'", true},
		{"gh api merge via bash -lc", "bash -lc 'gh api repos/o/r/pulls/9/merge -X PUT'", true},
		// We accept the false-positive of catching a GET probe on the same path —
		// the LLM should use `gh pr view --json mergeable` for that information.
		{"gh api default-method (GET) on merge path", "gh api repos/o/r/pulls/9/merge", true},

		// Iter-1 review: the digit-only PR-number form was bypassable by
		// shell variables and gh placeholders. The broader `[^/\s]+`
		// segment catches both shapes. Command substitution with embedded
		// spaces (e.g. `$(gh pr view --json number)`) intentionally does
		// NOT match — the segment refuses whitespace, and the LLM-bypass
		// shapes that have actually been observed are single-token
		// variable references rather than spaces-bearing $(...) bodies.
		{"gh api with shell variable PR number", "gh api repos/o/r/pulls/$PR_NUMBER/merge -X PUT", true},
		{"gh api with gh placeholder", "gh api repos/:owner/:repo/pulls/:number/merge -X PUT", true},
		{"gh api with single-token backtick substitution", "gh api repos/o/r/pulls/`echo9`/merge -X PUT", true},

		// Iter-1 review: the `[^\n]*` form didn't cross a backslash-newline.
		// (?s) + `.*?` makes the path-segment match work across line
		// continuations — augment iter-1 flagged this as a high-severity
		// bypass shape.
		{
			"gh api merge with backslash-newline continuation",
			"gh api repos/o/r \\\n  pulls/9/merge -X PUT",
			true,
		},
		{
			"gh api merge spread over multiline",
			"gh api \\\n  repos/o/r/pulls/9/merge \\\n  -X PUT",
			true,
		},

		// Negatives for the API form: the path must include /pulls/<n>/merge.
		{"gh api pulls list (no merge segment)", "gh api repos/o/r/pulls", false},
		{"gh api PR comments (different endpoint)", "gh api repos/o/r/pulls/9/comments", false},
		{"gh api unrelated", "gh api repos/o/r", false},
		{"gh api pulls/N/files (read endpoint)", "gh api repos/o/r/pulls/9/files", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPRMergeCommand(tc.cmd)
			if got != tc.want {
				t.Errorf("isPRMergeCommand(%q) = %v; want %v", tc.cmd, got, tc.want)
			}
		})
	}
}
