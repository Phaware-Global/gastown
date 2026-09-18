package cmd

import (
	"errors"
	"testing"
)

func TestSuspectedWorktreeReset(t *testing.T) {
	tests := []struct {
		name          string
		isPolecat     bool
		cleanupStatus string // EXPLICIT --cleanup-status only ("" = not passed / auto-detected)
		isNoMergeTask bool
		branch        string
		want          bool
	}{
		{
			name:      "reset: detached HEAD on a code polecat",
			isPolecat: true, branch: "HEAD",
			want: true, // the hga-y3jm false-completion shape (checkout to origin/develop)
		},
		{
			name:      "unresolved branch is treated as suspect (fail closed)",
			isPolecat: true, branch: "",
			want: true,
		},
		{
			name:      "genuine work: on the polecat feature branch",
			isPolecat: true, branch: "polecat/coma/hga-y3jm@mr8obcsf",
			want: false,
		},
		{
			name:      "legit direct push-to-default: on the default branch, not detached",
			isPolecat: true, branch: "develop",
			want: false, // T2 — must not be flagged as a reset
		},
		{
			name:          "explicit --cleanup-status=clean report-only task exempt",
			isPolecat:     true,
			cleanupStatus: "clean",
			branch:        "HEAD",
			want:          false,
		},
		{
			name:          "auto-detected clean (empty explicit) still triggers on detached reset",
			isPolecat:     true,
			cleanupStatus: "", // reset worktree auto-detects clean, but the flag was NOT explicitly passed
			branch:        "HEAD",
			want:          true,
		},
		{
			name:          "no_merge task exempt even when detached",
			isPolecat:     true,
			isNoMergeTask: true,
			branch:        "HEAD",
			want:          false,
		},
		{
			name:      "non-polecat (crew/mayor) exempt",
			isPolecat: false, branch: "HEAD",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := suspectedWorktreeReset(tt.isPolecat, tt.cleanupStatus, tt.isNoMergeTask, tt.branch)
			if got != tt.want {
				t.Errorf("suspectedWorktreeReset() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRequiresCommitsBeforeClose covers PR #234, discussion_r4050325617: an
// auto-detected "clean" status (doneCleanupStatus, passed as "" here since
// it was never explicitly flagged) must NOT exempt a zero-commit polecat
// from the must-have-commits refusal — only an EXPLICIT --cleanup-status=clean
// does.
func TestRequiresCommitsBeforeClose(t *testing.T) {
	tests := []struct {
		name          string
		isPolecat     bool
		cleanupStatus string // EXPLICIT --cleanup-status only ("" = not passed / auto-detected)
		isNoMergeTask bool
		want          bool
	}{
		{
			name:      "auto-detected clean (empty explicit) still requires commits",
			isPolecat: true,
			want:      true, // the exact hazard this PR's explicitCleanupFlag doc names
		},
		{
			name:          "explicit --cleanup-status=clean report-only task exempt",
			isPolecat:     true,
			cleanupStatus: "clean",
			want:          false,
		},
		{
			name:          "no_merge task exempt",
			isPolecat:     true,
			isNoMergeTask: true,
			want:          false,
		},
		{
			name:      "non-polecat (crew/mayor) exempt",
			isPolecat: false,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requiresCommitsBeforeClose(tt.isPolecat, tt.cleanupStatus, tt.isNoMergeTask)
			if got != tt.want {
				t.Errorf("requiresCommitsBeforeClose() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRemoteBranchAlreadySubmitted covers PR #234, discussion_r4050410848:
// the GH#wd7 fallback must not treat a branch that never diverged from the
// dispatch base as already-submitted work just because it's pushed+clean —
// a 0-ahead polecat branch with no remote branch (or a remote branch that
// trivially matches the base) must still fall through to the refusal.
func TestRemoteBranchAlreadySubmitted(t *testing.T) {
	tests := []struct {
		name              string
		pushed            bool
		unpushed          int
		pushErr           error
		remoteAheadOfBase int
		remoteAheadErr    error
		want              bool
	}{
		{
			name:   "genuine wd7 case: pushed, clean, remote branch has commits ahead of base",
			pushed: true, unpushed: 0, remoteAheadOfBase: 3,
			want: true,
		},
		{
			name:   "reset/never-diverged branch: pushed+clean but remote branch equals base",
			pushed: true, unpushed: 0, remoteAheadOfBase: 0,
			want: false, // the exact hazard this fix closes
		},
		{
			name:    "no remote branch at all",
			pushed:  false,
			pushErr: errors.New("no such remote branch"),
			want:    false,
		},
		{
			name:   "pushed but with unpushed commits remaining",
			pushed: true, unpushed: 2, remoteAheadOfBase: 3,
			want: false,
		},
		{
			name:   "remote-ahead lookup failed",
			pushed: true, unpushed: 0, remoteAheadErr: errors.New("rev-list failed"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := remoteBranchAlreadySubmitted(tt.pushed, tt.unpushed, tt.pushErr, tt.remoteAheadOfBase, tt.remoteAheadErr)
			if got != tt.want {
				t.Errorf("remoteBranchAlreadySubmitted() = %v, want %v", got, tt.want)
			}
		})
	}
}
