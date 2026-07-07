package cmd

import "testing"

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
