package tmux

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// makeBusyPane creates a session whose pane displays the "esc to interrupt"
// busy indicator, so IsIdle reports the agent as busy. Returns the session name
// (already registered for cleanup) or skips when the pane can't be put into a
// detectable busy state (environment-specific shell prompt).
func makeBusyPane(t *testing.T, tm *Tmux) string {
	t.Helper()
	session := fmt.Sprintf("gt-test-requireidle-%d", time.Now().UnixNano()%100000)
	if err := tm.NewSession(session, os.TempDir()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(session) })
	time.Sleep(200 * time.Millisecond)

	// Render the busy indicator into the pane. hasBusyIndicator matches any
	// line containing "esc to interrupt".
	if err := tm.SendKeys(session, "printf 'esc to interrupt\\n'"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	if tm.IsIdle(session) {
		t.Skip("pane not detected as busy in this environment; skipping")
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
