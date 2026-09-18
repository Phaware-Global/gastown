package cmd

import (
	"strings"
	"testing"
)

// TestCompletionCommitShaLine covers hq-8ynas required behavior #3: never
// cite a commit_sha the worker did not produce — specifically, never present
// the base branch's own tip as if it were evidence of a fix. PR #234 finding
// D scoped that judgment to the verified-push call site only (VerifyPushedCommit
// already succeeded); the --skip-verify path never claims a SHA is or isn't
// evidence, since nothing was actually checked there.
func TestCompletionCommitShaLine(t *testing.T) {
	const baseSHA = "fa4eda4ec46bc606a66382914dcf2d8a9fc71827"

	tests := []struct {
		name       string
		commitSHA  string
		baseSHA    string
		verified   bool
		wantHasSha bool // whether the literal "commit_sha:" label should appear
	}{
		{
			name:       "verified path, HEAD is exactly the base tip — hga-2put's actual incident shape",
			commitSHA:  baseSHA,
			baseSHA:    baseSHA,
			verified:   true,
			wantHasSha: false,
		},
		{
			name:       "verified path, HEAD predates the base tip but is a real, distinct commit — legit evidence",
			commitSHA:  "b2fc53e5",
			baseSHA:    baseSHA,
			verified:   true,
			wantHasSha: true,
		},
		{
			name:       "verified path, no HEAD sha resolved — nothing to cite",
			commitSHA:  "",
			baseSHA:    baseSHA,
			verified:   true,
			wantHasSha: false,
		},
		{
			name:       "--skip-verify path: SHA is reported as-is, no evidence judgment either way (finding D)",
			commitSHA:  baseSHA,
			baseSHA:    baseSHA,
			verified:   false,
			wantHasSha: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := completionCommitShaLine(tt.commitSHA, tt.baseSHA, "develop", tt.verified)
			hasSha := tt.commitSHA != "" && strings.Contains(got, "commit_sha: "+tt.commitSHA)
			if hasSha != tt.wantHasSha {
				t.Errorf("completionCommitShaLine(%q, %q, verified=%v) = %q, wantHasSha=%v", tt.commitSHA, tt.baseSHA, tt.verified, got, tt.wantHasSha)
			}
			if tt.verified && tt.commitSHA != "" && tt.commitSHA == tt.baseSHA && strings.Contains(got, tt.commitSHA) {
				t.Errorf("completionCommitShaLine(%q, %q, verified=true) = %q must not cite the base-tip sha as evidence", tt.commitSHA, tt.baseSHA, got)
			}
		})
	}
}
