package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/text/cases"
)

var tapGuardMemoryWriteCmd = &cobra.Command{
	Use:   "memory-write",
	Short: "Block writes to Claude Code memory paths for worker roles",
	Long: `Block persistent-memory writes from Gas Town worker agents.

Gas Town worker roles (refinery, mayor, deacon, witness, polecat, crew)
get their durable state from three sources: the formula, the beads DB,
and the mail subsystem. All three are visible, versioned, and
reviewable. Claude Code's per-session auto-memory system is none of
those — a memory file persists across sessions for that role/agent
but exists outside the formula's view, so a policy the LLM writes
into memory can silently override the formula on future cycles.

That is exactly the drift pattern observed in the 2026-04-19 Telegraph
v1 dogfood (G12b, G13 in docs/design/refinery-pr-workflow.md): the
refinery LLM inserted its own policy ("wait for augment before
dispatching") that the formula did not authorize, and then — after a
user nudge to correct the policy — tried to save the correction as a
feedback memory, calcifying the correction without any review of
whether the formula text itself should change.

This guard blocks Write/Edit/NotebookEdit tool calls whose file_path
targets a Claude Code memory directory, and Bash commands that write
into one, but only when the caller is a Gas Town agent. Non-agent Claude Code sessions (a human using Claude
Code directly for other work) are unaffected.

Path pattern: any tool_input.file_path / tool_input.notebook_path
containing "/.claude/projects/" followed later by "/memory/", ignoring
case, as the default macOS filesystem does. That
covers all the current Claude Code memory layouts on macOS/Linux
regardless of the specific project hash. Windows (backslash) paths
are out of scope — gastown is macOS/Linux only; if that changes, add
a Windows-path case to the classifier test.

In a Bash command the guard follows redirects, tee, sed -i, perl -i,
cp, mv, ln, install, rsync, touch, truncate and dd, through cd, simple
variables, command substitutions, sh -c and heredocs fed to a shell.
Reading or removing memory files is allowed. This is a check on ordinary
shell writes, not a sandbox. It does not see a path computed at run time
or reached through a symlink, a write made by a script in another
language or piped into a shell, or other writers such as curl -o,
wget -O, tar -C, unzip -d, gawk -i inplace and find -exec.

A command that names a memory path is also blocked when the shell syntax
cannot be read for certain: bash and zsh would read it differently,
something is left unclosed, or a shell runs a pipe or a script that the
same command produces.

Exit codes:
  0 - Operation allowed (not an agent, or path is not a memory file)
  2 - Operation BLOCKED (agent trying to write a memory file)

Hook configuration (applied to Bash, Write, Edit, NotebookEdit):
  {
    "PreToolUse": [{
      "matcher": "Bash|Write|Edit|NotebookEdit",
      "hooks": [{
        "type": "command",
        "command": "gt tap guard memory-write"
      }]
    }]
  }`,
	RunE: runTapGuardMemoryWrite,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardMemoryWriteCmd)
}

// runTapGuardMemoryWrite is the entry point invoked by the PreToolUse
// hook. Fails open on parse errors to avoid bricking the agent over a
// hook-protocol hiccup.
func runTapGuardMemoryWrite(cmd *cobra.Command, args []string) error {
	if !isGasTownAgentContext() {
		return nil
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil
	}
	if target, ok := memoryWriteTarget(input); ok {
		printMemoryWriteBlock(target)
		return NewSilentExit(2)
	}
	return nil
}

// memoryWriteTarget returns the memory path the hooked tool call writes.
func memoryWriteTarget(input []byte) (string, bool) {
	if filePath := extractFilePath(input); filePath != "" {
		return filePath, isClaudeCodeMemoryPath(filePath)
	}
	command := extractCommand(input)
	if command == "" {
		return "", false
	}
	var hookInput struct {
		Cwd string `json:"cwd"`
	}
	_ = json.Unmarshal(input, &hookInput)
	return bashMemoryWrite(command, hookInput.Cwd)
}

// extractFilePath pulls the target path out of the hook JSON.
//
// Different tools use different field names in tool_input:
//   - Write and Edit:       "file_path"
//   - NotebookEdit:         "notebook_path"
//
// We probe both and return the first non-empty value. If Claude Code
// adds a new file-producing tool with a third field name later, this
// function needs a new probe — the guard's hook matcher is
// "Write|Edit|NotebookEdit", so any file tool added to the matcher must
// have its path field represented here too. A Bash call has neither field.
func extractFilePath(input []byte) string {
	if len(input) == 0 {
		return ""
	}
	var hookInput struct {
		ToolInput struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(input, &hookInput); err != nil {
		return ""
	}
	if hookInput.ToolInput.FilePath != "" {
		return hookInput.ToolInput.FilePath
	}
	return hookInput.ToolInput.NotebookPath
}

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
// Case is ignored because on a case-insensitive filesystem MEMORY/ and
// memory/ are the same directory.
//
// Exported-lowercase for testability from the cmd package's tests.
func isClaudeCodeMemoryPath(filePath string) bool {
	if filePath == "" {
		return false
	}
	filePath = cases.Fold().String(filePath)
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

func printMemoryWriteBlock(filePath string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ WORKER MEMORY-WRITE BLOCKED                                  ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(os.Stderr, "║  Path: %-56s ║\n", truncateStr(filePath, 56))
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  Gas Town worker roles must not persist private memory.          ║")
	fmt.Fprintln(os.Stderr, "║  Durable state flows through formula + beads + mail, where it    ║")
	fmt.Fprintln(os.Stderr, "║  is visible, versioned, and reviewable. Memory files silently    ║")
	fmt.Fprintln(os.Stderr, "║  drift from the formula and calcify per-agent policy.            ║")
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  If you have feedback worth keeping, put it in the formula,      ║")
	fmt.Fprintln(os.Stderr, "║  a bead, or mail to the dispatcher.                              ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
