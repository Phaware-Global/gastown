package hooks

import (
	"regexp"
	"strings"
	"testing"
)

// TestReviewerOverrideBlocksWriteSurfaces asserts the Reviewer role's tap-guard
// (P23-2376) blocks every dangerous write surface: posting raw reviews,
// merging, pushing, driving the refinery, and resolving threads. The Reviewer's
// only sanctioned write path is `gt reviewer post`.
func TestReviewerOverrideBlocksWriteSurfaces(t *testing.T) {
	overrides := DefaultOverrides()
	rev, ok := overrides["reviewer"]
	if !ok {
		t.Fatal("DefaultOverrides() has no \"reviewer\" entry")
	}
	if len(rev.PreToolUse) == 0 {
		t.Fatal("reviewer override has no PreToolUse guards")
	}

	// Each dangerous command must be matched by some PreToolUse entry whose
	// hook blocks with a non-zero exit.
	wantBlocked := []string{
		"gh pr review",
		"gh api*pulls*reviews", // raw review-API POST path (defense-in-depth)
		// GraphQL review submission. `gh api graphql` matches neither the
		// `gh pr review` pattern nor the REST one (which needs the pulls/reviews
		// path segments), so these need their own matchers — and their own
		// entries here, or a later cleanup could delete them as apparent
		// duplicates of the REST guard with the suite still green.
		"addPullRequestReview",
		"submitPullRequestReview",
		// Transport-level backstop for the two matchers above: `gh api graphql
		// --input mut.json` / `-F query=@mut.graphql` never puts the mutation
		// name on the command line, so a name-only guard misses it. This role
		// has no legitimate `gh api graphql` use at all (resolveReviewThread
		// below is its only other GraphQL surface, and that's blocked too).
		"gh api*graphql",
		"gh pr merge",
		"git push",
		"gt refinery pr",
		"resolveReviewThread",
		// The MCP transport. Every Bash matcher above models the shell as the
		// only way out; a review submitted through the GitHub MCP tools touches
		// no shell command. Covered by name here and by REGEX EVALUATION in
		// TestReviewerOverride_MCPMatchersMatchRealToolNames, which is the check
		// that matters — a substring scan cannot tell a matcher that fires from
		// one that is merely present.
		"pull_request_review_write",
		"add_comment_to_pending_review",
		"merge_pull_request",
		"push_files",
		"create_or_update_file",
		"delete_file",
		"update_pull_request",
		"create_pull_request",
		"update_pull_request_branch",
		"create_branch",
		"fork_repository",
		"add_reply_to_pull_request_comment",
	}
	for _, needle := range wantBlocked {
		if !matcherCovers(rev.PreToolUse, needle) {
			t.Errorf("reviewer override does not guard %q", needle)
		}
	}

	// Every guard must actually block (exit 2), not just warn.
	for _, entry := range rev.PreToolUse {
		for _, h := range entry.Hooks {
			if !strings.Contains(h.Command, "exit 2") {
				t.Errorf("guard %q does not block (missing 'exit 2'): %q", entry.Matcher, h.Command)
			}
		}
	}
}

// TestReviewerOverrideApplicableViaRigRole confirms the override key resolves
// for a rig-scoped reviewer target (e.g. "gastown/reviewer").
func TestReviewerOverrideApplicableViaRigRole(t *testing.T) {
	got := GetApplicableOverrides("gastown/reviewer")
	found := false
	for _, k := range got {
		if k == "reviewer" {
			found = true
		}
	}
	if !found {
		t.Errorf("GetApplicableOverrides(gastown/reviewer) = %v, missing \"reviewer\"", got)
	}
}

// TestReviewerOverride_BlocksRealGraphQLCommandStrings exercises the guards
// against COMMAND STRINGS rather than against the matcher list.
//
// matcherCovers only asserts that some matcher contains a substring, which
// cannot detect a matcher that is well-formed but positioned wrongly — and that
// is exactly what happened: `Bash(*gh api graphql*)` required the endpoint to
// follow `gh api` contiguously, so every flag-first invocation escaped all three
// GraphQL guards while the suite stayed green.
//
// BOUNDARY, stated because this test is easy to over-read: it validates matcher
// SHAPE against an assumed `*`-glob semantics, not against the engine that
// enforces the block. Nothing in this repository consumes HookEntry.Matcher as a
// pattern — the strings are serialized into settings.json and interpreted by
// Claude Code, out of tree — so globMatches below is this file's model of that
// engine (split on `*`, literals in order, first segment anchored, last as
// suffix), not the engine itself. If the harness's semantics diverge, these
// assertions can pass while the guards do not fire. Closing that would need an
// end-to-end check against a real session, or moving enforcement into a
// `gt tap guard` handler that inspects the hook JSON in Go.
func TestReviewerOverride_BlocksRealGraphQLCommandStrings(t *testing.T) {
	rev, ok := DefaultOverrides()["reviewer"]
	if !ok {
		t.Fatal("DefaultOverrides() has no \"reviewer\" entry")
	}
	for _, cmd := range []string{
		`gh api graphql -f query='mutation { addPullRequestReview(input:{}) { id } }'`,
		`gh api graphql --input mut.json`,
		`gh api --input mut.json graphql`,
		`gh api -H accept:application/json graphql`,
		`gh api --method POST graphql -F query=@mut.graphql`,
	} {
		if !anyMatcherMatches(rev.PreToolUse, "Bash("+cmd+")") {
			t.Errorf("reviewer override does not block %q", cmd)
		}
	}
}

// anyMatcherMatches reports whether any entry's glob matches the given tool
// invocation string.
func anyMatcherMatches(entries []HookEntry, invocation string) bool {
	for _, e := range entries {
		if globMatches(e.Matcher, invocation) {
			return true
		}
	}
	return false
}

// globMatches implements the `*`-only glob semantics the matcher strings use:
// every literal segment must appear in order.
func globMatches(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(s[pos:], part)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 {
			return false
		}
		pos += idx + len(part)
	}
	if last := parts[len(parts)-1]; last != "" && !strings.HasSuffix(s, last) {
		return false
	}
	return true
}

func matcherCovers(entries []HookEntry, needle string) bool {
	for _, e := range entries {
		if strings.Contains(e.Matcher, needle) {
			return true
		}
	}
	return false
}

// TestReviewerOverride_MCPMatchersMatchRealToolNames evaluates the MCP matchers
// as REGEX against the tool names they must stop.
//
// This is the control that was missing when `mcp__github__<tool>` was rewritten
// to `mcp__*__<tool>`: `*` was meant as a glob, but the matcher is a regex over
// the tool name (vendored reference: plugin-dev/skills/hook-development/SKILL.md
// § Matchers, which gives `mcp__.*__delete.*` as the MCP form). Read as a regex,
// `mcp__*__x` is `mcp_` followed by any number of `_` then `__x`, which cannot
// span a server segment — so every MCP guard matched nothing while
// matcherCovers, a substring scan over the matcher strings, stayed green.
//
// Same stated boundary as the command-string test: nothing in-repo consumes
// HookEntry.Matcher, so this validates the pattern against the DOCUMENTED
// semantics, not against the enforcing harness. That is strictly more than the
// substring scan could do — it distinguishes a matcher that fires from one that
// is merely present.
func TestReviewerOverride_MCPMatchersMatchRealToolNames(t *testing.T) {
	rev, ok := DefaultOverrides()["reviewer"]
	if !ok {
		t.Fatal("DefaultOverrides() has no \"reviewer\" entry")
	}
	// Real tool names, including a non-default server alias — the alias is a
	// user-chosen key in ~/.claude.json, so pinning one is what the regex form
	// exists to survive.
	for _, tool := range []string{
		"mcp__github__pull_request_review_write",
		"mcp__github__add_comment_to_pending_review",
		"mcp__github__merge_pull_request",
		"mcp__github__push_files",
		"mcp__github__create_or_update_file",
		"mcp__github__delete_file",
		// update_pull_request rewrites the title, body and BASE branch of the PR
		// under review — it edits the artifact the Reviewer exists to judge.
		"mcp__github__update_pull_request",
		"mcp__github__update_pull_request_branch",
		"mcp__github__create_pull_request",
		"mcp__github__create_branch",
		"mcp__github__fork_repository",
		// Posts into review threads under the machine-user token, bypassing the
		// `gt reviewer post` output contract.
		"mcp__github__add_reply_to_pull_request_comment",
		"mcp__github-remote__pull_request_review_write",
		"mcp__gh_work__merge_pull_request",
	} {
		if !anyMatcherRegexMatches(rev.PreToolUse, tool) {
			t.Errorf("no reviewer matcher fires on %q — the MCP guard is inert for it", tool)
		}
	}
	// Tools the reviewer legitimately needs must NOT be caught. codegraph is the
	// review contract's required tooling, so a guard that swallowed it would
	// break every review rather than only the writes.
	for _, tool := range []string{
		"mcp__codegraph__codegraph_explore",
		"mcp__github__get_file_contents",
		"mcp__github__pull_request_read",
	} {
		if anyMatcherRegexMatches(rev.PreToolUse, tool) {
			t.Errorf("a reviewer matcher blocks %q, which the review contract requires", tool)
		}
	}
}

// anyMatcherRegexMatches reports whether any entry's matcher, read as a regex
// anchored over the whole tool name, matches. Bash(...) entries are skipped:
// they target command strings, not tool names, and are covered separately.
func anyMatcherRegexMatches(entries []HookEntry, tool string) bool {
	for _, e := range entries {
		if strings.HasPrefix(e.Matcher, "Bash(") {
			continue
		}
		re, err := regexp.Compile("^(?:" + e.Matcher + ")$")
		if err != nil {
			continue
		}
		if re.MatchString(tool) {
			return true
		}
	}
	return false
}
