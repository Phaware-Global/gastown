package cmd

import (
	"encoding/json"
	"testing"
)

func bashHookInput(t *testing.T, command string) []byte {
	t.Helper()
	input, err := json.Marshal(map[string]any{"cwd": "/w", "tool_input": map[string]string{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	return input
}

// Wrapping a command in "gt reviewer exec" must not hide it from the guards.
func TestMemoryWriteGuardSeesThroughReviewerExec(t *testing.T) {
	const mem = "/home/a/.claude/projects/-x/memory/evil.md"
	for _, command := range []string{
		"gt reviewer exec -- tee " + mem,
		"gt reviewer exec cp /tmp/a " + mem,
		"gt reviewer exec -- sh -c 'echo hi > " + mem + "'",
		"gt 'reviewer' exec -- cp /tmp/a " + mem,
		"/usr/local/bin/gt reviewer exec -- touch " + mem,
		"env X=1 gt reviewer exec -- gt reviewer exec -- cp /tmp/a " + mem,
		"sh -c 'gt reviewer exec -- cp /tmp/a " + mem + "'",
	} {
		if _, ok := memoryWriteTarget(bashHookInput(t, command)); !ok {
			t.Errorf("allowed %q", command)
		}
		if _, ok := bashMemoryWrite(command, "/w"); !ok {
			t.Errorf("the shell reading allowed %q", command)
		}
	}
}

func TestGuardsReadTheCommandInsideReviewerExec(t *testing.T) {
	for command, want := range map[string]string{
		"gt reviewer exec -- git push origin x":           "git push origin x",
		"gt reviewer exec git push origin x":              "git push origin x",
		"cd x && /bin/gt reviewer exec -- gh pr create":   "cd x && gh pr create",
		"gt reviewer exec \\\n  -- dolt sql-server":       "dolt sql-server",
		"gt reviewer exec -- gt reviewer exec -- make":    "make",
		"gt reviewer post --pr 1 --findings f.json":       "gt reviewer post --pr 1 --findings f.json",
		"grep -rn 'gt reviewer execute' internal/ | head": "grep -rn 'gt reviewer execute' internal/ | head",
	} {
		if got := extractCommand(bashHookInput(t, command)); got != want {
			t.Errorf("extractCommand(%q) = %q, want %q", command, got, want)
		}
	}
}

// Some guards are registered by command prefix, so the harness never runs them
// on a wrapped command. The reviewer-exec guard applies them itself.
func TestReviewerExecGuardAppliesPrefixMatchedGuardsInside(t *testing.T) {
	for _, command := range []string{
		"gt reviewer exec -- sudo true",
		"gt reviewer exec -- brew install jq",
		"gt reviewer exec -- git push --force origin x",
		"gt reviewer exec -- dolt sql-server",
	} {
		if wrappedReason(command) == "" {
			t.Errorf("allowed %q", command)
		}
	}
	for _, command := range []string{"gt reviewer exec -- go test ./...", "echo sudo"} {
		if reason := wrappedReason(command); reason != "" {
			t.Errorf("blocked %q: %s", command, reason)
		}
	}
}
