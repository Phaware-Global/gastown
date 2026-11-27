package cmd

import (
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

var agentStateCall = regexp.MustCompile(`\bgt (\w+) state\b`)

func checkAgentStateCalls(t *testing.T, source, text string) {
	t.Helper()
	calls := agentStateCall.FindAllStringSubmatch(text, -1)
	if len(calls) == 0 {
		t.Fatalf("%s has no agent-state command to check", source)
	}
	for _, call := range calls {
		got, _, err := rootCmd.Find([]string{call[1], "state"})
		if err != nil || got != agentStateCmd {
			t.Errorf("%s: %q does not resolve to the agent state command (err: %v)", source, call[0], err)
		}
	}
}

// Every agent-state command a patrol formula tells its agent to run must resolve
// to the real command.
func TestPatrolFormulaAgentStateCommandResolves(t *testing.T) {
	for _, name := range []string{"mol-refinery-patrol", "mol-witness-patrol", "mol-deacon-patrol"} {
		t.Run(name, func(t *testing.T) {
			data, err := formula.GetEmbeddedFormulaContent(name)
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			// Parsed step text, because a TOML string may escape its newlines.
			parsed, err := formula.Parse(data)
			if err != nil {
				t.Fatalf("parsing %s: %v", name, err)
			}
			var text strings.Builder
			for _, step := range parsed.Steps {
				text.WriteString(step.Description + "\n")
			}
			checkAgentStateCalls(t, name, text.String())
		})
	}
}

// The same holds for the examples in help text.
func TestHelpTextAgentStateCommandResolves(t *testing.T) {
	checkAgentStateCalls(t, "agents state help", agentStateCmd.Long)
	checkAgentStateCalls(t, "await-signal help", moleculeAwaitSignalCmd.Long)
}
