package polecat

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

type fakeWorkstateMRFinder struct {
	issue *beads.Issue
	err   error
}

func (f fakeWorkstateMRFinder) FindMRForBranchAny(branch string) (*beads.Issue, error) {
	return f.issue, f.err
}

func TestPopulateGitFallbackVerdict(t *testing.T) {
	tests := []struct {
		name        string
		input       SlotReuseInput
		wantCleanup CleanupStatus
		wantVerdict string
		wantNeeds   bool
	}{
		{
			name:        "git check failed is recovery",
			input:       SlotReuseInput{GitCheckFailed: true},
			wantCleanup: CleanupUnknown,
			wantVerdict: WorkVerdictNeedsRecovery,
			wantNeeds:   true,
		},
		{
			name:        "clean git is safe",
			input:       SlotReuseInput{},
			wantCleanup: CleanupClean,
			wantVerdict: WorkVerdictSafeToNuke,
		},
		{
			name:        "unpushed wins precedence",
			input:       SlotReuseInput{UnpushedCommits: 2, StashCount: 1, GitDirty: true},
			wantCleanup: CleanupUnpushed,
			wantVerdict: WorkVerdictNeedsRecovery,
			wantNeeds:   true,
		},
		{
			name:        "stash beats dirty",
			input:       SlotReuseInput{StashCount: 1, GitDirty: true},
			wantCleanup: CleanupStash,
			wantVerdict: WorkVerdictNeedsRecovery,
			wantNeeds:   true,
		},
		{
			name:        "dirty is recovery",
			input:       SlotReuseInput{GitDirty: true},
			wantCleanup: CleanupUncommitted,
			wantVerdict: WorkVerdictNeedsRecovery,
			wantNeeds:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &PolecatWorkState{}
			mgr := &Manager{}
			mgr.populateGitFallbackVerdict(state, tt.input)

			if state.CleanupStatus != tt.wantCleanup {
				t.Fatalf("CleanupStatus = %q, want %q", state.CleanupStatus, tt.wantCleanup)
			}
			if state.Verdict != tt.wantVerdict {
				t.Fatalf("Verdict = %q, want %q", state.Verdict, tt.wantVerdict)
			}
			if state.NeedsRecovery != tt.wantNeeds {
				t.Fatalf("NeedsRecovery = %v, want %v", state.NeedsRecovery, tt.wantNeeds)
			}
		})
	}
}

func TestApplyMQWorkStateTerminalAndNoWork(t *testing.T) {
	tests := []struct {
		name               string
		beadTerminal       bool
		hasSubmittableWork bool
		wantMQ             string
		wantVerdict        string
	}{
		{
			name:               "terminal bead skips mq submit",
			beadTerminal:       true,
			hasSubmittableWork: true,
			wantMQ:             MQStatusSubmitted,
			wantVerdict:        WorkVerdictSafeToNuke,
		},
		{
			name:               "no submittable work does not require mq",
			beadTerminal:       false,
			hasSubmittableWork: false,
			wantMQ:             MQStatusNotRequired,
			wantVerdict:        WorkVerdictSafeToNuke,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &PolecatWorkState{Verdict: WorkVerdictSafeToNuke}
			mgr := &Manager{}
			mgr.applyMQWorkState(state, nil, tt.beadTerminal, tt.hasSubmittableWork)

			if state.MQStatus != tt.wantMQ {
				t.Fatalf("MQStatus = %q, want %q", state.MQStatus, tt.wantMQ)
			}
			if state.Verdict != tt.wantVerdict {
				t.Fatalf("Verdict = %q, want %q", state.Verdict, tt.wantVerdict)
			}
		})
	}
}

func TestApplyMQWorkStateMatrix(t *testing.T) {
	tests := []struct {
		name      string
		finder    workstateMRFinder
		wantMQ    string
		wantVer   string
		wantNeeds bool
	}{
		{
			name:    "mr found stays safe",
			finder:  fakeWorkstateMRFinder{issue: &beads.Issue{ID: "gt-wisp-mr", Status: "open"}},
			wantMQ:  MQStatusSubmitted,
			wantVer: WorkVerdictSafeToNuke,
		},
		{
			name:      "mr missing needs mq submit",
			finder:    fakeWorkstateMRFinder{},
			wantMQ:    MQStatusNotSubmitted,
			wantVer:   WorkVerdictNeedsMQSubmit,
			wantNeeds: true,
		},
		{
			name:      "mr lookup error fails closed",
			finder:    fakeWorkstateMRFinder{err: errors.New("bd unavailable")},
			wantMQ:    MQStatusUnknown,
			wantVer:   WorkVerdictNeedsRecovery,
			wantNeeds: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &PolecatWorkState{Branch: "polecat/test", Verdict: WorkVerdictSafeToNuke}
			mgr := &Manager{}
			mgr.applyMQWorkState(state, tt.finder, false, true)

			if state.MQStatus != tt.wantMQ {
				t.Fatalf("MQStatus = %q, want %q", state.MQStatus, tt.wantMQ)
			}
			if state.Verdict != tt.wantVer {
				t.Fatalf("Verdict = %q, want %q", state.Verdict, tt.wantVer)
			}
			if state.NeedsRecovery != tt.wantNeeds {
				t.Fatalf("NeedsRecovery = %v, want %v", state.NeedsRecovery, tt.wantNeeds)
			}
		})
	}
}
