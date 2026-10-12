package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
)

// The Claude Code Stop hook fires at the end of every assistant turn, so the
// session is alive whenever polecat-stop-check runs. A polecat that pushed its
// work and ended a turn (waiting on a build, a review, or a tool) must not have
// gt done run on its behalf: that closes the hooked bead, mails the dispatcher,
// may create an MR, and checks the worktree out to origin/<base> mid-task.
func TestDecidePolecatStopAction_OnlyRemindsLiveSession(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name   string
		hb     *polecat.SessionHeartbeat
		branch string
		ahead  string
	}{
		{"working heartbeat, pushed commits ahead", &polecat.SessionHeartbeat{Timestamp: now, State: polecat.HeartbeatWorking}, "polecat/bullet/gt-abc@x", "3"},
		{"v1 heartbeat without state", &polecat.SessionHeartbeat{Timestamp: now}, "polecat/bullet/gt-abc@x", "1"},
		{"stuck heartbeat", &polecat.SessionHeartbeat{Timestamp: now, State: polecat.HeartbeatStuck}, "polecat/bullet/gt-abc@x", "2"},
		{"no heartbeat file", nil, "polecat/bullet/gt-abc@x", "1"},
		{"another polecat's PR branch", &polecat.SessionHeartbeat{Timestamp: now, State: polecat.HeartbeatWorking}, "polecat/other/gt-pr@y", "5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decidePolecatStopAction(tc.hb, tc.branch, tc.ahead); got != stopActionRemind {
				t.Fatalf("decidePolecatStopAction = %v, want stopActionRemind: a Stop event proves the session is alive, so gt done must not be run for it", got)
			}
		})
	}
}

func TestDecidePolecatStopAction_QuietWhenNothingPending(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name   string
		hb     *polecat.SessionHeartbeat
		branch string
		ahead  string
	}{
		{"gt done already started", &polecat.SessionHeartbeat{Timestamp: now, State: polecat.HeartbeatExiting}, "polecat/bullet/gt-abc@x", "3"},
		{"idle", &polecat.SessionHeartbeat{Timestamp: now, State: polecat.HeartbeatIdle}, "polecat/bullet/gt-abc@x", "3"},
		{"on main", nil, "main", "2"},
		{"detached HEAD", nil, "HEAD", "2"},
		{"no commits ahead", nil, "polecat/bullet/gt-abc@x", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decidePolecatStopAction(tc.hb, tc.branch, tc.ahead); got != stopActionNone {
				t.Fatalf("decidePolecatStopAction = %v, want stopActionNone", got)
			}
		})
	}
}
