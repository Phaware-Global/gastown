package cmd

import (
	"strings"
	"testing"
)

func TestExtractToolName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "ask user question payload",
			input: `{"tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Where should I focus?"}]}}`,
			want:  "AskUserQuestion",
		},
		{
			name:  "enter plan mode payload",
			input: `{"tool_name":"EnterPlanMode","tool_input":{}}`,
			want:  "EnterPlanMode",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
		{
			name:  "unparseable JSON",
			input: `{"tool_name": "AskUserQuestion`,
			want:  "",
		},
		{
			name:  "missing tool_name field",
			input: `{"tool_input":{"command":"ls"}}`,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractToolName([]byte(tt.input)); got != tt.want {
				t.Errorf("extractToolName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestInteractiveInputBlockMessage(t *testing.T) {
	msg := interactiveInputBlockMessage("AskUserQuestion")

	// The banner must name the blocked tool and state the mandate:
	// executive decisions on procedural matters, async escalation for
	// product/priority decisions.
	for _, want := range []string{
		"AskUserQuestion",
		"AUTONOMY MANDATE",
		"executive decision",
		"gt escalate",
		"Jira comment",
		"ASYNCHRONOUSLY",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("block message missing %q:\n%s", want, msg)
		}
	}
}

func TestInteractiveInputBlockMessageUnknownTool(t *testing.T) {
	msg := interactiveInputBlockMessage("")
	if !strings.Contains(msg, "(synchronous user-input tool)") {
		t.Errorf("block message should carry a placeholder when tool_name is unknown:\n%s", msg)
	}
}
