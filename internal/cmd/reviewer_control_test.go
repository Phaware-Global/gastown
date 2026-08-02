package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reviewer"
)

func TestDiagnoseReviewer_CoversAllFourStates(t *testing.T) {
	live := &reviewer.Heartbeat{Timestamp: time.Now(), StartedAt: time.Now(), Phase: reviewer.PhasePrompt, PR: 1}

	tests := []struct {
		name    string
		running bool
		hb      *reviewer.Heartbeat
		want    string // substring
	}{
		{"idle rig", false, nil, "idle"},
		{"died mid-review", false, live, "died mid-review"},
		{"orphan session", true, nil, "orphan"},
		{"working", true, live, "working"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := diagnoseReviewer(tc.running, tc.hb)
			if !strings.Contains(got, tc.want) {
				t.Errorf("diagnoseReviewer(%v, hb=%v) = %q, want it to mention %q",
					tc.running, tc.hb != nil, got, tc.want)
			}
		})
	}
}

func TestDiagnoseReviewer_DistinguishesDeadFromIdle(t *testing.T) {
	// The distinction that matters operationally: a rig with no reviewer at all
	// is healthy; a rig whose reviewer died mid-review left work unfinished and
	// needs the refinery to re-dispatch. Both have no session.
	idle := diagnoseReviewer(false, nil)
	dead := diagnoseReviewer(false, &reviewer.Heartbeat{Phase: reviewer.PhaseConsolidate, PR: 9})
	if idle == dead {
		t.Error("an idle rig and a rig whose reviewer died mid-review must not report the same state")
	}
}

func TestCollectReviewerState_ReportsHeartbeatFields(t *testing.T) {
	rigPath := t.TempDir()
	started := time.Now().Add(-30 * time.Minute)
	if err := reviewer.WriteHeartbeat(rigPath, &reviewer.Heartbeat{
		Timestamp: time.Now().Add(-5 * time.Minute),
		StartedAt: started,
		Phase:     reviewer.PhasePrompt,
		PR:        175,
		Round:     2,
		SHA:       "abcdef1234567890",
	}); err != nil {
		t.Fatal(err)
	}

	st := collectReviewerState("testrig", rigPath)
	if st.PR != 175 || st.Round != 2 || st.Phase != reviewer.PhasePrompt {
		t.Errorf("heartbeat fields not surfaced: %+v", st)
	}
	if st.Elapsed == "" {
		t.Error("Elapsed must be reported — it is the number the reaper's absolute cap acts on")
	}
	if st.PhaseAge == "" {
		t.Error("PhaseAge must be reported")
	}
	if st.Rig != "testrig" {
		t.Errorf("Rig = %q, want testrig", st.Rig)
	}
}

func TestCollectReviewerState_NoHeartbeatLeavesProgressEmpty(t *testing.T) {
	st := collectReviewerState("testrig", t.TempDir())
	if st.Phase != "" || st.PR != 0 || st.Elapsed != "" {
		t.Errorf("absent heartbeat must leave progress fields empty, got %+v", st)
	}
	if st.Diagnosis == "" {
		t.Error("Diagnosis must always be populated")
	}
}

func TestCollectReviewerState_UnknownStartedAtOmitsElapsed(t *testing.T) {
	// Elapsed() is 0 when StartedAt is unset. Reporting "0s" would read as "just
	// started" rather than "unknown", which is the opposite of the truth.
	rigPath := t.TempDir()
	if err := reviewer.WriteHeartbeat(rigPath, &reviewer.Heartbeat{
		Timestamp: time.Now(), Phase: reviewer.PhaseCheckout, PR: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if st := collectReviewerState("testrig", rigPath); st.Elapsed != "" {
		t.Errorf("Elapsed = %q, want empty when StartedAt is unknown", st.Elapsed)
	}
}

func TestDashIfEmpty(t *testing.T) {
	if got := dashIfEmpty(""); got != "-" {
		t.Errorf("dashIfEmpty(\"\") = %q, want \"-\"", got)
	}
	if got := dashIfEmpty("x"); got != "x" {
		t.Errorf("dashIfEmpty(\"x\") = %q, want \"x\"", got)
	}
}
