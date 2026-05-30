package transform

import (
	"strings"
	"testing"
)

// TestNudgeCommandArgs verifies telegraph wakeups shell out to `gt nudge` in
// queue mode, so high-volume webhook fan-out is cooperatively drained at the
// mayor's turn boundary instead of send-keys-injected into a busy input box.
func TestNudgeCommandArgs(t *testing.T) {
	args := nudgeCommandArgs("mayor/", "Telegraph: new events pending")

	want := []string{"nudge", "mayor/", "--mode=queue", "-m", "Telegraph: new events pending"}
	if len(args) != len(want) {
		t.Fatalf("nudgeCommandArgs len = %d (%v), want %d (%v)", len(args), args, len(want), want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, args[i], want[i])
		}
	}

	if !contains(args, "--mode=queue") {
		t.Errorf("telegraph nudge must use queue mode; args = %v", args)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
