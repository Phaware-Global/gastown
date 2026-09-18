package cmd

import (
	"strings"
	"testing"
)

// TestLacksCompletionEvidence covers hq-8ynas REQUIRED BEHAVIOUR #1: a
// zero-commit "no code changes" close must not happen on silence alone.
func TestLacksCompletionEvidence(t *testing.T) {
	tests := []struct {
		name          string
		isPolecat     bool
		hasEvidence   bool
		cleanupStatus string // EXPLICIT --cleanup-status only
		skipVerify    bool
		isNoMergeTask bool
		want          bool
	}{
		{
			name:      "hga-2put shape: fresh dispatch, zero commits, no notes — refuse",
			isPolecat: true, hasEvidence: false,
			want: true,
		},
		{
			name:      "notes recorded — evidence satisfies the gate",
			isPolecat: true, hasEvidence: true,
			want: false,
		},
		{
			name:          "explicit --cleanup-status=clean is an opt-in signal, not silence",
			isPolecat:     true,
			hasEvidence:   false,
			cleanupStatus: "clean",
			want:          false,
		},
		{
			name:      "--skip-verify is an explicit operator override",
			isPolecat: true, hasEvidence: false, skipVerify: true,
			want: false,
		},
		{
			name:      "no_merge/review_only task legitimately has no commits or notes",
			isPolecat: true, hasEvidence: false, isNoMergeTask: true,
			want: false,
		},
		{
			name:      "non-polecat (crew/mayor) exempt",
			isPolecat: false, hasEvidence: false,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lacksCompletionEvidence(tt.isPolecat, tt.hasEvidence, tt.cleanupStatus, tt.skipVerify, tt.isNoMergeTask)
			if got != tt.want {
				t.Errorf("lacksCompletionEvidence() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCompletionCommitShaLine covers hq-8ynas REQUIRED BEHAVIOUR #3: never
// cite a commit_sha the worker did not produce — specifically, never present
// the base branch's own tip as if it were evidence of a fix.
func TestCompletionCommitShaLine(t *testing.T) {
	const baseSHA = "fa4eda4ec46bc606a66382914dcf2d8a9fc71827"

	tests := []struct {
		name       string
		commitSHA  string
		baseSHA    string
		wantHasSha bool // whether the literal "commit_sha:" label should appear
	}{
		{
			name:       "HEAD is exactly the base tip — hga-2put's actual incident shape",
			commitSHA:  baseSHA,
			baseSHA:    baseSHA,
			wantHasSha: false,
		},
		{
			name:       "HEAD predates the base tip but is a real, distinct commit — legit evidence",
			commitSHA:  "b2fc53e5",
			baseSHA:    baseSHA,
			wantHasSha: true,
		},
		{
			name:       "no HEAD sha resolved — nothing to cite",
			commitSHA:  "",
			baseSHA:    baseSHA,
			wantHasSha: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := completionCommitShaLine(tt.commitSHA, tt.baseSHA, "develop")
			hasSha := tt.commitSHA != "" && strings.Contains(got, "commit_sha: "+tt.commitSHA)
			if hasSha != tt.wantHasSha {
				t.Errorf("completionCommitShaLine(%q, %q) = %q, wantHasSha=%v", tt.commitSHA, tt.baseSHA, got, tt.wantHasSha)
			}
			if tt.commitSHA != "" && tt.commitSHA == tt.baseSHA && strings.Contains(got, tt.commitSHA) {
				t.Errorf("completionCommitShaLine(%q, %q) = %q must not cite the base-tip sha as evidence", tt.commitSHA, tt.baseSHA, got)
			}
		})
	}
}
