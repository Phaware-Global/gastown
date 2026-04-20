package cmd

import "testing"

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
