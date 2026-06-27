package tmux

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// busyPaneSeq guarantees unique session names across rapid/parallel calls.
var busyPaneSeq atomic.Int64

// makeBusyPane creates a session whose pane displays the "esc to interrupt"
// busy indicator, so IsIdle reports the agent as busy. Returns the session name
// (already registered for cleanup) or skips when the pane can't be put into a
// detectable busy state (environment-specific shell prompt).
func makeBusyPane(t *testing.T, tm *Tmux) string {
	t.Helper()
	session := fmt.Sprintf("gt-test-requireidle-%d-%d", time.Now().UnixNano(), busyPaneSeq.Add(1))
	if err := tm.NewSession(session, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(session) })
	time.Sleep(200 * time.Millisecond)

	// Idle baseline: the fresh shell must read as idle first. If it doesn't,
	// this environment's shell prompt isn't detectable (IsIdle needs the ❯
	// prompt or ⏵⏵ status marker) and the busy assertion below would pass
	// trivially for the wrong reason — so skip rather than validate nothing.
	if !tm.IsIdle(session) {
		t.Skip("shell prompt not detected as idle in this environment; skipping")
	}

	// Render the busy indicator into the pane. hasBusyIndicator matches any
	// line containing "esc to interrupt". This must flip IsIdle to busy — if it
	// doesn't, hasBusyIndicator has stopped influencing IsIdle, which is a real
	// regression, not an environment quirk.
	if err := tm.SendKeys(session, "printf 'esc to interrupt\\n'"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	if tm.IsIdle(session) {
		t.Fatalf("busy indicator present but IsIdle still reports idle (hasBusyIndicator not influencing IsIdle?)")
	}
	return session
}

// TestNudgeRequireIdle_BusyAborts verifies that RequireIdle aborts delivery with
// ErrAgentBusy when the agent is busy at paste time, rather than typing the
// nudge into a busy TUI where it would be stranded unsubmitted.
func TestNudgeRequireIdle_BusyAborts(t *testing.T) {
	tm := newTestTmux(t)
	session := makeBusyPane(t, tm)

	err := tm.NudgeSessionWithOpts(session, "background notification", NudgeOpts{RequireIdle: true})
	if !errors.Is(err, ErrAgentBusy) {
		t.Errorf("NudgeSessionWithOpts(RequireIdle) on busy agent = %v, want ErrAgentBusy", err)
	}
}

// TestNudgeRequireIdle_OptIn verifies the busy-abort is opt-in: without
// RequireIdle, delivery to a busy pane is attempted (legacy behavior) and does
// not return ErrAgentBusy.
func TestNudgeRequireIdle_OptIn(t *testing.T) {
	tm := newTestTmux(t)
	session := makeBusyPane(t, tm)

	err := tm.NudgeSessionWithOpts(session, "background notification", NudgeOpts{})
	if errors.Is(err, ErrAgentBusy) {
		t.Errorf("NudgeSessionWithOpts without RequireIdle = ErrAgentBusy, want delivery attempt")
	}
}

// TestSendEnterVerified_StrandedReturnsErrNudgeStranded verifies that when the
// input box still holds typed text after all Enter retries, sendEnterVerified
// returns an error that unwraps to ErrNudgeStranded — the signal callers use to
// recover (clear + re-queue) instead of reporting a hard failure. This is the
// exact condition operators hit ("nudge Enter not processed ... input box still
// holds text") when an agent slips busy mid-paste. (GH#gt-nudge-strand)
func TestSendEnterVerified_StrandedReturnsErrNudgeStranded(t *testing.T) {
	tm := newTestTmux(t)
	session := fmt.Sprintf("gt-test-stranded-%d-%d", time.Now().UnixNano(), busyPaneSeq.Add(1))
	if err := tm.NewSession(session, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(session) })
	time.Sleep(200 * time.Millisecond)

	// Run `cat` so the trailing Enter keystrokes are inert: cat echoes stdin
	// without regenerating a shell prompt, so the rendered prompt-prefix line
	// keeps holding text across all retries (a real agent that has gone busy
	// behaves the same — Enter does not submit). Without this, Enter in a bare
	// shell would spawn fresh prompts and the verification could read as cleared.
	if err := tm.SendKeys(session, "cat"); err != nil {
		t.Fatalf("SendKeys(cat): %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// Type a prompt-prefixed line WITH leftover text, no Enter — this simulates
	// a nudge typed into the input box but not yet submitted.
	if _, err := tm.run("send-keys", "-t", session, "-l", DefaultReadyPromptPrefix+"stranded nudge text"); err != nil {
		t.Fatalf("send-keys literal: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// Precondition: the harness must actually render the stranded prompt line so
	// inputBoxSubmitted can see it. If this terminal/locale can't (e.g. ❯ not
	// displayed), skip rather than assert nothing.
	lines, err := tm.CapturePaneLines(session, 5)
	if err != nil {
		t.Skipf("CapturePaneLines: %v", err)
	}
	if submitted, conclusive := inputBoxSubmitted(lines, DefaultReadyPromptPrefix); !conclusive || submitted {
		t.Skipf("stranded prompt line not rendered in this environment (conclusive=%v submitted=%v); skipping", conclusive, submitted)
	}

	err = tm.sendEnterVerified(session, DefaultReadyPromptPrefix)
	if !errors.Is(err, ErrNudgeStranded) {
		t.Errorf("sendEnterVerified on stranded box = %v, want ErrNudgeStranded", err)
	}
}
