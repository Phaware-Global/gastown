package transform

import (
	"reflect"
	"testing"
)

// TestNudgeCommandArgs verifies telegraph wakeups shell out to `gt nudge` in
// queue mode, so high-volume webhook fan-out is cooperatively drained at the
// mayor's turn boundary instead of send-keys-injected into a busy input box.
func TestNudgeCommandArgs(t *testing.T) {
	got := nudgeCommandArgs("mayor/", "Telegraph: new events pending")
	want := []string{"nudge", "mayor/", "--mode=queue", "-m", "Telegraph: new events pending"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("nudgeCommandArgs() = %#v, want %#v", got, want)
	}
}
