package git

import (
	"reflect"
	"testing"
)

// TestBuildRequestReviewArgv pins the reviewer classification behavior
// added for G12a: "augment" (case-insensitive) must be routed through
// `gh pr comment --body "augment review"` because the Augment Code bot
// listens for that exact comment body and is NOT reachable via
// `gh pr edit --add-reviewer` (augment is a GitHub App, not a user).
// All other reviewer names go through --add-reviewer.
//
// This test covers classification only — it does not invoke `gh`.
func TestBuildRequestReviewArgv(t *testing.T) {
	tests := []struct {
		name            string
		reviewers       []string
		wantCommentArgv []string
		wantEditArgv    []string
	}{
		{
			name:            "empty reviewers",
			reviewers:       nil,
			wantCommentArgv: nil,
			wantEditArgv:    nil,
		},
		{
			name:            "augment only — comment path",
			reviewers:       []string{"augment"},
			wantCommentArgv: []string{"pr", "comment", "42", "--body", "augment review"},
			wantEditArgv:    nil,
		},
		{
			name:            "augment case-insensitive",
			reviewers:       []string{"Augment"},
			wantCommentArgv: []string{"pr", "comment", "42", "--body", "augment review"},
			wantEditArgv:    nil,
		},
		{
			name:            "augment uppercase",
			reviewers:       []string{"AUGMENT"},
			wantCommentArgv: []string{"pr", "comment", "42", "--body", "augment review"},
			wantEditArgv:    nil,
		},
		{
			name:            "single user — edit path",
			reviewers:       []string{"kevinpjones"},
			wantCommentArgv: nil,
			wantEditArgv:    []string{"pr", "edit", "42", "--add-reviewer", "kevinpjones"},
		},
		{
			name:            "multiple users — CSV edit path",
			reviewers:       []string{"alice", "bob"},
			wantCommentArgv: nil,
			wantEditArgv:    []string{"pr", "edit", "42", "--add-reviewer", "alice,bob"},
		},
		{
			name:            "team — edit path",
			reviewers:       []string{"@myorg/reviewers"},
			wantCommentArgv: nil,
			wantEditArgv:    []string{"pr", "edit", "42", "--add-reviewer", "@myorg/reviewers"},
		},
		{
			name:            "mixed augment + user — both paths",
			reviewers:       []string{"augment", "alice"},
			wantCommentArgv: []string{"pr", "comment", "42", "--body", "augment review"},
			wantEditArgv:    []string{"pr", "edit", "42", "--add-reviewer", "alice"},
		},
		{
			name:            "mixed order — augment classification is name-based, not position-based",
			reviewers:       []string{"alice", "augment", "bob"},
			wantCommentArgv: []string{"pr", "comment", "42", "--body", "augment review"},
			wantEditArgv:    []string{"pr", "edit", "42", "--add-reviewer", "alice,bob"},
		},
		// Whitespace normalization — augment classification must tolerate
		// whitespace padding (config fields often carry trailing newlines or
		// stray spaces; silently routing "augment " to --add-reviewer would
		// reproduce G12a).
		{
			name:            "augment with trailing whitespace — matches comment path",
			reviewers:       []string{"augment "},
			wantCommentArgv: []string{"pr", "comment", "42", "--body", "augment review"},
			wantEditArgv:    nil,
		},
		{
			name:            "augment with leading whitespace — matches comment path",
			reviewers:       []string{" augment"},
			wantCommentArgv: []string{"pr", "comment", "42", "--body", "augment review"},
			wantEditArgv:    nil,
		},
		{
			name:            "augment surrounded by tabs/newlines — matches comment path",
			reviewers:       []string{"\taugment\n"},
			wantCommentArgv: []string{"pr", "comment", "42", "--body", "augment review"},
			wantEditArgv:    nil,
		},
		// Empty-after-trim dropping — never emit "a,,b" to gh, which would fail.
		{
			name:            "empty string in list — dropped, no blank reviewer emitted",
			reviewers:       []string{"alice", "", "bob"},
			wantCommentArgv: nil,
			wantEditArgv:    []string{"pr", "edit", "42", "--add-reviewer", "alice,bob"},
		},
		{
			name:            "whitespace-only string in list — dropped",
			reviewers:       []string{"alice", "   ", "bob"},
			wantCommentArgv: nil,
			wantEditArgv:    []string{"pr", "edit", "42", "--add-reviewer", "alice,bob"},
		},
		{
			name:            "all entries empty/whitespace — both argvs nil",
			reviewers:       []string{"", "  ", "\t"},
			wantCommentArgv: nil,
			wantEditArgv:    nil,
		},
		{
			name:            "augment alongside empty entries — still routes cleanly",
			reviewers:       []string{"", "  augment  ", "alice", "\t", "bob"},
			wantCommentArgv: []string{"pr", "comment", "42", "--body", "augment review"},
			wantEditArgv:    []string{"pr", "edit", "42", "--add-reviewer", "alice,bob"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotComment, gotEdit := buildRequestReviewArgv(42, tc.reviewers)
			if !reflect.DeepEqual(gotComment, tc.wantCommentArgv) {
				t.Errorf("commentArgv = %v; want %v", gotComment, tc.wantCommentArgv)
			}
			if !reflect.DeepEqual(gotEdit, tc.wantEditArgv) {
				t.Errorf("editArgv = %v; want %v", gotEdit, tc.wantEditArgv)
			}
		})
	}
}
