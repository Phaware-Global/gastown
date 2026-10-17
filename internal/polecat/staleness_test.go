package polecat

import (
	"strings"
	"testing"
)

// A failed probe means staleness is unknown. Treating it as "clean tree, no agent
// bead" would let the daemon's cap-relief cleanup nuke a polecat it merely failed to
// inspect (git error, Dolt timeout), bypassing the protected agent states.
func TestAssessStaleness_ProbeErrorFailsClosed(t *testing.T) {
	info := &StalenessInfo{
		Name:       "p1",
		ProbeError: "reading agent bead: timeout",
		// Would otherwise be stale: no session, no bead, way behind main.
		CommitsBehind: 100,
	}
	stale, reason := assessStaleness(info, 20)
	if stale {
		t.Fatalf("assessStaleness marked a polecat with a failed probe stale (%q)", reason)
	}
	if !strings.Contains(reason, "probe failed") || !strings.Contains(reason, "timeout") {
		t.Errorf("reason = %q, want it to name the failed probe", reason)
	}
}

func TestAssessStaleness_NoProbeErrorStillStale(t *testing.T) {
	stale, _ := assessStaleness(&StalenessInfo{Name: "p1"}, 20)
	if !stale {
		t.Fatal("session-less polecat with no bead and no probe error should be stale")
	}
}

// Reuse writes agent_state=spawning under the per-polecat lock, but the hook bead
// that moves State/Issue lands afterwards, outside it. In that gap the polecat reads
// idle with no issue (same as at detection), so only the agent state shows the reuse.
func TestApplyRedispatchCheck(t *testing.T) {
	tests := []struct {
		name      string
		agent     string
		p         Polecat
		wantStale bool
	}{
		{"reused, hook not yet landed", "spawning", Polecat{State: StateIdle}, false},
		{"working agent state", "working", Polecat{State: StateIdle}, false},
		{"hooked to a different issue", "idle", Polecat{State: StateIdle, Issue: "gt-new"}, false},
		{"state working", "idle", Polecat{State: StateWorking}, false},
		{"still idle and unchanged", "idle", Polecat{State: StateIdle}, true},
		{"no agent bead", "", Polecat{State: StateIdle}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &StalenessInfo{Name: "p1", AgentState: tt.agent, IsStale: true, Reason: "stale"}
			applyRedispatchCheck(info, &tt.p, &StalenessInfo{Name: "p1"})
			if info.IsStale != tt.wantStale {
				t.Errorf("IsStale = %v, want %v (reason %q)", info.IsStale, tt.wantStale, info.Reason)
			}
		})
	}
}
