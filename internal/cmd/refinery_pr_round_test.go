package cmd

import (
	"testing"
	"time"
)

// TestDispatchRound pins the round number to the durable review_loop_iter
// counter. The regression it guards is hga-y1b: the previous derivation read
// await_review_started_at, which PR.5 clears before every re-dispatch, so the
// round was pinned at 1 forever and the execution contract never engaged its
// anti-relitigation rules.
func TestDispatchRound(t *testing.T) {
	// A non-zero StartedAt is set on every case that has one to prove the
	// timestamp no longer participates in the derivation at all.
	started := time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		state awaitReviewBeadState
		want  int
	}{
		{
			name:  "first review: no fix dispatches yet",
			state: awaitReviewBeadState{ReviewLoopIter: 0},
			want:  1,
		},
		{
			name:  "after one fix round",
			state: awaitReviewBeadState{ReviewLoopIter: 1},
			want:  2,
		},
		{
			// The exact shape of hga-y1b: PR.5 cleared StartedAt, so the old
			// code returned 1 here. Three fix dispatches have landed, so this
			// is round 4.
			name:  "started-at cleared by PR.5 does not reset the round",
			state: awaitReviewBeadState{ReviewLoopIter: 3, StartedAt: time.Time{}},
			want:  4,
		},
		{
			// The old code's only path to 2. It must not cap here.
			name:  "started-at set does not cap the round at 2",
			state: awaitReviewBeadState{ReviewLoopIter: 7, StartedAt: started},
			want:  8,
		},
		{
			// PR #132 reached an actual round 26 while every dispatch said 1.
			name:  "deep fix rounds keep counting",
			state: awaitReviewBeadState{ReviewLoopIter: 25, StartedAt: started},
			want:  26,
		},
		{
			// A corrupt/hand-edited bead field must not yield round 0, which
			// BuildPerspectivePrompt would silently floor back to 1.
			name:  "negative iter clamps to the first round",
			state: awaitReviewBeadState{ReviewLoopIter: -4},
			want:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dispatchRound(tt.state); got != tt.want {
				t.Errorf("dispatchRound(%+v) = %d, want %d", tt.state, got, tt.want)
			}
		})
	}
}

// TestDispatchRoundIgnoresStartedAt states the invariant directly: for a fixed
// iter, the round must not depend on the await timestamp. This is the property
// whose violation was the bug.
func TestDispatchRoundIgnoresStartedAt(t *testing.T) {
	for iter := 0; iter < 5; iter++ {
		cleared := dispatchRound(awaitReviewBeadState{ReviewLoopIter: iter})
		set := dispatchRound(awaitReviewBeadState{
			ReviewLoopIter: iter,
			StartedAt:      time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC),
		})
		if cleared != set {
			t.Errorf("iter=%d: round differs by StartedAt (cleared=%d, set=%d); "+
				"the round must derive from review_loop_iter alone", iter, cleared, set)
		}
	}
}
