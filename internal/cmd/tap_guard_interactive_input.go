package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var tapGuardInteractiveInputCmd = &cobra.Command{
	Use:   "interactive-input",
	Short: "Block synchronous user-input tools (AskUserQuestion, plan mode) for autonomous roles",
	Long: `Block synchronous user-input tools in autonomous Gas Town sessions.

The Mayor (and any role this guard is wired to) operates unattended.
Tools like AskUserQuestion and agent-initiated plan mode present a
blocking prompt and then wait for a human answer. When no human is
watching, the session freezes — and every Witness, Refinery, and
Polecat waiting on that agent's decisions stalls with it. That is the
exact failure mode the Propulsion Principle (GUPP) exists to prevent.

The autonomy mandate this guard enforces:
  - Procedural/operational decisions (dispatch order, cleanup,
    recovery, sequencing) are the agent's to make. Decide, act,
    record the rationale in a bead or mail.
  - Product, priority, and scope decisions are escalated to the
    overseer through ASYNCHRONOUS channels — gt escalate, a Jira
    comment @-mentioning the decision owner, a GitHub PR/issue
    comment — while the agent continues with other work and picks
    up the answer on a later cycle.

Like the pr-workflow guard, this guard blocks unconditionally when it
fires: the hook is only installed in the settings of roles that must
not block on user input (currently the Mayor), and the PreToolUse
matcher restricts it to the synchronous-input tools, so the hook
firing IS the policy decision. Stdin is read only to name the blocked
tool in the banner; an unreadable or unparseable payload still blocks.

Plan-mode coverage: EnterPlanMode is the model's tool-call entry into
plan mode (present in Claude Code v2.1.x), and the planning flow it
starts terminates in the synchronous ExitPlanMode plan-approval
prompt. Guarding the entry tool stops the agent from ever reaching
that prompt on its own. ExitPlanMode is deliberately NOT covered by
the default matchers: with EnterPlanMode blocked, the only way into
plan mode is a human operator switching the session mode manually,
and that human-initiated plan must remain presentable/exitable.

Exit codes:
  2 - Always (invocation means a blocked tool was attempted)

Hook configuration (installed via the "mayor" hooks override):
  {
    "PreToolUse": [{
      "matcher": "AskUserQuestion",
      "hooks": [{"type": "command", "command": "gt tap guard interactive-input"}]
    }, {
      "matcher": "EnterPlanMode",
      "hooks": [{"type": "command", "command": "gt tap guard interactive-input"}]
    }]
  }`,
	// A block is the guard doing its job, not a CLI usage error — keep
	// cobra's usage/error chatter out of the stderr the agent sees.
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runTapGuardInteractiveInput,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardInteractiveInputCmd)
}

// runTapGuardInteractiveInput always blocks. The matcher scoping (only
// synchronous-input tools) plus the role-scoped install (only roles
// with the autonomy mandate) make the hook firing itself the decision;
// stdin is parsed only to make the banner name the offending tool.
func runTapGuardInteractiveInput(cmd *cobra.Command, args []string) error {
	toolName := ""
	if !isStdinTerminal() {
		if input, err := io.ReadAll(os.Stdin); err == nil {
			toolName = extractToolName(input)
		}
	}
	fmt.Fprint(os.Stderr, interactiveInputBlockMessage(toolName))
	return NewSilentExit(2)
}

// extractToolName pulls tool_name out of the PreToolUse hook JSON.
// Returns "" on empty or unparseable input — the guard blocks either
// way, this only affects the banner wording.
func extractToolName(input []byte) string {
	if len(input) == 0 {
		return ""
	}
	var hookInput struct {
		ToolName string `json:"tool_name"`
	}
	if err := json.Unmarshal(input, &hookInput); err != nil {
		return ""
	}
	return hookInput.ToolName
}

// interactiveInputBlockMessage renders the block banner. Split from the
// printing so tests can assert on the mandate content.
func interactiveInputBlockMessage(toolName string) string {
	tool := toolName
	if tool == "" {
		tool = "(synchronous user-input tool)"
	}
	return fmt.Sprintf(`
╔════════════════════════════════════════════════════════════════════════╗
║  ❌ SYNCHRONOUS USER INPUT BLOCKED — AUTONOMY MANDATE                   ║
╠════════════════════════════════════════════════════════════════════════╣
║  Tool: %-64s ║
║                                                                        ║
║  You operate autonomously. A blocking prompt freezes this session —    ║
║  and every agent waiting on your decisions — until a human happens     ║
║  to look. Nobody is watching (GUPP).                                   ║
║                                                                        ║
║  Procedural / operational decisions (dispatch, cleanup, recovery,      ║
║  sequencing): make the executive decision yourself. Record the         ║
║  rationale in the bead or in mail.                                     ║
║                                                                        ║
║  Product, priority, or scope decisions: escalate ASYNCHRONOUSLY to     ║
║  the overseer and continue with other work:                            ║
║    gt escalate "<question>" --severity <level> --reason "<context>"   ║
║    Jira comment on the pertinent ticket, @-mention the decision owner  ║
║    GitHub PR/issue comment                                             ║
║                                                                        ║
║  Never wait inline for an answer. Choose a reasonable default, note    ║
║  it, and pick up replies on a later cycle (mail, Jira, escalation).    ║
╚════════════════════════════════════════════════════════════════════════╝

`, truncateStr(tool, 64))
}
