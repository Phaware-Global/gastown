package memwritebaseline

import "strings"

// isClaudeCodeMemoryPath returns true iff the path targets a Claude
// Code per-project memory directory. The canonical layout on any
// platform Claude Code supports is
// ~/.claude/projects/<project-hash>/memory/<filename>. Filename
// extension is not constrained: markdown files (.md) are the common
// case for Write/Edit, but NotebookEdit targets (.ipynb) living under
// a memory/ subdir are also blocked — the guard pins the directory,
// not the file extension.
//
// Matching strategy: look for ".claude/projects/" anywhere in the
// path (accepting both "/.claude/projects/..." and
// ".claude/projects/..." — the latter covers relative paths a tool
// might pass unresolved), followed later by "/memory/". We reject
// cases where a filename coincidentally contains ".claude" (e.g.
// "foo.claude/..." ) by requiring the match to be at a path segment
// boundary: either the start of the string, or immediately after a
// path separator.
//
// Exported-lowercase for testability from the cmd package's tests.
func isClaudeCodeMemoryPath(filePath string) bool {
	if filePath == "" {
		return false
	}
	const needle = ".claude/projects/"
	// Walk through every occurrence of needle; accept the first one that
	// sits at a path-segment boundary.
	start := 0
	for {
		idx := strings.Index(filePath[start:], needle)
		if idx < 0 {
			return false
		}
		absIdx := start + idx
		// Segment-boundary check: absIdx == 0 (relative path starting
		// with ".claude/") OR the preceding byte is a path separator.
		boundaryOK := absIdx == 0 || filePath[absIdx-1] == '/'
		if boundaryOK {
			rest := filePath[absIdx+len(needle):]
			if strings.Contains(rest, "/memory/") {
				return true
			}
			return false
		}
		start = absIdx + 1
	}
}
