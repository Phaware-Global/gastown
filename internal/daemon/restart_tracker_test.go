package daemon

import (
	"testing"
	"time"
)

// TestCanRestart_CrashLoopRecoversAfterStabilityPeriod verifies that crash-loop
// state is a circuit breaker, not a permanent lockout. CanRestart holds while an
// agent is freshly crash-looping, then permits a half-open retry once the
// stability period elapses since the last restart attempt. Regression for the
// deacon that sat dead ~1.5 days because CanRestart returned false forever once
// CrashLoopSince was set (recoverable only via manual 'gt daemon clear-backoff').
func TestCanRestart_CrashLoopRecoversAfterStabilityPeriod(t *testing.T) {
	rt := NewRestartTracker(t.TempDir(), RestartTrackerConfig{})
	const agentID = "deacon"

	// Freshly crash-looping (last attempt a minute ago): must hold.
	rt.state.Agents[agentID] = &AgentRestartInfo{
		RestartCount:   rt.config.CrashLoopCount,
		CrashLoopSince: time.Now().Add(-1 * time.Minute),
		LastRestart:    time.Now().Add(-1 * time.Minute),
	}
	if rt.CanRestart(agentID) {
		t.Fatal("CanRestart = true for a freshly crash-looping agent; want false (still holding)")
	}

	// Crash-loop older than the stability period: half-open retry allowed.
	old := time.Now().Add(-(rt.config.StabilityPeriod + time.Minute))
	rt.state.Agents[agentID].CrashLoopSince = old
	rt.state.Agents[agentID].LastRestart = old
	if !rt.CanRestart(agentID) {
		t.Fatal("CanRestart = false after the recovery window elapsed; want true (half-open retry)")
	}
}

// TestRecordRestart_ClearsCrashLoopOnRecoveryRetry verifies that the half-open
// retry (a restart attempted after the stability period) resets the fault count
// and clears the crash loop, returning the agent to normal management.
func TestRecordRestart_ClearsCrashLoopOnRecoveryRetry(t *testing.T) {
	rt := NewRestartTracker(t.TempDir(), RestartTrackerConfig{})
	const agentID = "deacon"

	old := time.Now().Add(-(rt.config.StabilityPeriod + time.Minute))
	rt.state.Agents[agentID] = &AgentRestartInfo{
		RestartCount:   rt.config.CrashLoopCount,
		CrashLoopSince: old,
		LastRestart:    old,
	}

	rt.RecordRestart(agentID)

	info := rt.state.Agents[agentID]
	if !info.CrashLoopSince.IsZero() {
		t.Errorf("CrashLoopSince not cleared after recovery retry: %v", info.CrashLoopSince)
	}
	if info.RestartCount != 1 {
		t.Errorf("RestartCount = %d after recovery retry; want 1 (reset then incremented)", info.RestartCount)
	}
	if rt.IsInCrashLoop(agentID) {
		t.Error("IsInCrashLoop = true after recovery retry; want false")
	}
}

// TestCanRestart_RapidCrashStormStillArmsCrashLoop guards the other direction:
// the recovery path must not defeat crash-loop detection. Rapid restarts within
// the window re-arm the loop and CanRestart holds.
func TestCanRestart_RapidCrashStormStillArmsCrashLoop(t *testing.T) {
	rt := NewRestartTracker(t.TempDir(), RestartTrackerConfig{})
	const agentID = "witness-rig"

	for i := 0; i < rt.config.CrashLoopCount; i++ {
		rt.RecordRestart(agentID) // rapid, back-to-back — within CrashLoopWindow
	}

	if !rt.IsInCrashLoop(agentID) {
		t.Fatal("expected crash-loop armed after a rapid restart storm")
	}
	if rt.CanRestart(agentID) {
		t.Fatal("CanRestart = true immediately after arming crash-loop; want false (hold for recovery window)")
	}
}
