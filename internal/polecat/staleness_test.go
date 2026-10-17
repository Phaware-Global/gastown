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
