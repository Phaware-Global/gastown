package witness

import (
	"testing"
	"time"
)

func finishedBase() finishedNotSubmittedInput {
	return finishedNotSubmittedInput{
		HeartbeatAge:    30 * time.Minute,
		HasHeartbeat:    true,
		HookOpen:        true,
		OnPolecatBranch: true,
		Clean:           true,
		AheadOfBase:     2,
	}
}

func TestIsFinishedNotSubmitted(t *testing.T) {
	const threshold = 15 * time.Minute
	if !isFinishedNotSubmitted(finishedBase(), threshold) {
		t.Fatal("pushed, clean, quiet, bead open: want nudge")
	}
	blockers := map[string]func(*finishedNotSubmittedInput){
		"no heartbeat":           func(i *finishedNotSubmittedInput) { i.HasHeartbeat = false },
		"recent heartbeat":       func(i *finishedNotSubmittedInput) { i.HeartbeatAge = time.Minute },
		"bead closed":            func(i *finishedNotSubmittedInput) { i.HookOpen = false },
		"already submitted":      func(i *finishedNotSubmittedInput) { i.Submitted = true },
		"no_merge or review-fix": func(i *finishedNotSubmittedInput) { i.NoMergeOrFix = true },
		"detached or base":       func(i *finishedNotSubmittedInput) { i.OnPolecatBranch = false },
		"dirty or unpushed":      func(i *finishedNotSubmittedInput) { i.Clean = false },
		"nothing ahead of base":  func(i *finishedNotSubmittedInput) { i.AheadOfBase = 0 },
	}
	for name, mutate := range blockers {
		t.Run(name, func(t *testing.T) {
			in := finishedBase()
			mutate(&in)
			if isFinishedNotSubmitted(in, threshold) {
				t.Fatalf("%s: want no nudge", name)
			}
		})
	}
}
