package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var tapGuardMemoryWriteCmd = &cobra.Command{
	Use:   "memory-write",
	Short: "Block Write/Edit/NotebookEdit to Claude Code memory paths for worker roles",
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
targets a Claude Code memory directory, but only when the caller is a
Gas Town agent. Non-agent Claude Code sessions (a human using Claude
Code directly for other work) are unaffected.

Path pattern: any tool_input.file_path / tool_input.notebook_path
containing "/.claude/projects/" followed later by "/memory/". That
covers all the current Claude Code memory layouts on macOS/Linux
regardless of the specific project hash. Windows (backslash) paths
are out of scope — gastown is macOS/Linux only; if that changes, add
a Windows-path case to the classifier test.

Exit codes:
  0 - Operation allowed (not an agent, or path is not a memory file)
  2 - Operation BLOCKED (agent trying to write a memory file)

Hook configuration (applied to Write, Edit, NotebookEdit):
  {
    "PreToolUse": [{
      "matcher": "Write|Edit|NotebookEdit",
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
	filePath := extractFilePath(input)
	if filePath == "" {
		return nil
	}

	if isClaudeCodeMemoryPath(filePath) {
		printMemoryWriteBlock(filePath)
		return NewSilentExit(2)
	}
	return nil
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
// "Write|Edit|NotebookEdit", so any tool added to the matcher must
// have its path field represented here too.
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
// ~/.claude/projects/<project-hash>/memory/<file>.md; we match the
// "/.claude/projects/" prefix followed (anywhere after) by "/memory/"
// to cover variations without binding to a specific hash scheme.
//
// Exported-lowercase for testability from the cmd package's tests.
func isClaudeCodeMemoryPath(filePath string) bool {
	if filePath == "" {
		return false
	}
	idx := strings.Index(filePath, "/.claude/projects/")
	if idx < 0 {
		return false
	}
	rest := filePath[idx+len("/.claude/projects/"):]
	return strings.Contains(rest, "/memory/")
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
