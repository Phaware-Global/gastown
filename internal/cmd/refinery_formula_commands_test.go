package cmd

import (
	"regexp"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

// Every agent-state command the refinery patrol tells its agent to run must
// resolve to the real command.
func TestRefineryPatrolAgentStateCommandResolves(t *testing.T) {
	data, err := formula.GetEmbeddedFormulaContent("mol-refinery-patrol")
	if err != nil {
		t.Fatalf("reading refinery formula: %v", err)
	}
	calls := regexp.MustCompile(`\bgt (\w+) state\b`).FindAllStringSubmatch(string(data), -1)
	if len(calls) == 0 {
		t.Fatal("refinery formula has no agent-state command to check")
	}
	for _, call := range calls {
		got, _, err := rootCmd.Find([]string{call[1], "state"})
		if err != nil || got != agentStateCmd {
			t.Errorf("%q does not resolve to the agent state command (err: %v)", call[0], err)
		}
	}
}
