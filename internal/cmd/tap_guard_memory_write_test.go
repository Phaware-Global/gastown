package cmd

import "testing"

// TestIsClaudeCodeMemoryPath covers the path-classifier used by the
// memory-write guard. The classifier decides, given a file_path
// argument from Write/Edit/NotebookEdit, whether that write targets a
// Claude Code per-project memory directory.
//
// Pinning this shape protects against future path-layout changes in
// Claude Code silently weakening the guard: if the canonical path
// shifts (e.g. a new XDG-style location), the negative cases here
// fail loudly and the maintainer must revisit the classifier rather
// than let the guard become a no-op.
func TestIsClaudeCodeMemoryPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		// Positives — recognized memory paths across likely variations.
		{
			"standard memory file",
			"/Users/agent/.claude/projects/-Users-agent-projects-gastown/memory/feedback.md",
			true,
		},
		{
			"nested subdir under memory",
			"/Users/agent/.claude/projects/some-hash/memory/feedback/nested.md",
			true,
		},
		{
			"MEMORY.md index",
			"/home/user/.claude/projects/abc/memory/MEMORY.md",
			true,
		},
		{
			"different home layout",
			"/root/.claude/projects/x/memory/y.md",
			true,
		},

		// Negatives — paths that superficially look memory-adjacent but
		// are not a memory write.
		{"empty", "", false},
		{
			"non-claude path",
			"/Users/agent/projects/gastown/internal/cmd/tap_guard.go",
			false,
		},
		{
			"claude projects but no memory segment",
			"/Users/agent/.claude/projects/abc/settings.json",
			false,
		},
		{
			"memory segment but not under claude/projects",
			"/Users/agent/my-repo/memory/notes.md",
			false,
		},
		{
			"claude/agents (different Claude Code feature, not memory)",
			"/Users/agent/.claude/agents/my-agent.md",
			false,
		},
		{
			"claude/skills (different Claude Code feature, not memory)",
			"/Users/agent/.claude/skills/my-skill/SKILL.md",
			false,
		},
		{
			"substring collision — 'memory' in filename but not directory",
			"/Users/agent/.claude/projects/x/notes-memory.md",
			false,
		},
		{
			"shell interpolation not expanded — literal ~ retained",
			"~/.claude/projects/x/memory/y.md",
			// The guard operates on whatever file_path the tool received.
			// Claude Code normally resolves ~ before tool_input is sent,
			// but if an unresolved tilde arrives the guard still catches
			// the memory segment — safer to block than to silently allow.
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isClaudeCodeMemoryPath(tc.path)
			if got != tc.want {
				t.Errorf("isClaudeCodeMemoryPath(%q) = %v; want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestExtractFilePath covers the JSON hook-input parser. If the hook
// protocol ever changes the "tool_input.file_path" key name, this
// test fails and the maintainer knows to update both extract and the
// test together.
func TestExtractFilePath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty input", "", ""},
		{"malformed JSON", "{not json", ""},
		{"missing tool_input", `{"other": "fields"}`, ""},
		{"empty tool_input", `{"tool_input": {}}`, ""},
		{
			"standard Write call",
			`{"tool_input": {"file_path": "/tmp/foo.md", "content": "hello"}}`,
			"/tmp/foo.md",
		},
		{
			"Edit call shape",
			`{"tool_input": {"file_path": "/tmp/bar.go", "old_string": "a", "new_string": "b"}}`,
			"/tmp/bar.go",
		},
		{
			"extra top-level fields ignored",
			`{"hook_event_name": "PreToolUse", "tool_name": "Write", "tool_input": {"file_path": "/x.md"}}`,
			"/x.md",
		},
		{
			"NotebookEdit uses notebook_path instead of file_path",
			`{"tool_input": {"notebook_path": "/tmp/notebook.ipynb"}}`,
			"/tmp/notebook.ipynb",
		},
		{
			"file_path takes precedence over notebook_path when both present",
			`{"tool_input": {"file_path": "/tmp/a.md", "notebook_path": "/tmp/b.ipynb"}}`,
			"/tmp/a.md",
		},
		{
			"both path fields empty — returns empty",
			`{"tool_input": {"file_path": "", "notebook_path": ""}}`,
			"",
		},
		{
			"memory path via notebook_path should flow into classifier as-is",
			`{"tool_input": {"notebook_path": "/Users/x/.claude/projects/y/memory/nb.ipynb"}}`,
			"/Users/x/.claude/projects/y/memory/nb.ipynb",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractFilePath([]byte(tc.input))
			if got != tc.want {
				t.Errorf("extractFilePath(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}
